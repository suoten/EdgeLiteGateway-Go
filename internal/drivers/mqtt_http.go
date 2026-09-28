package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// MQTTClientDriver receives data from MQTT topics and exposes it as device points.
// It subscribes to configured topics and parses JSON payloads to extract point values.
// The MQTT packets are encoded directly over the broker socket (plain TCP or TLS),
// so no external MQTT client library is needed.
type MQTTClientDriver struct {
	BaseDriver
	broker       string
	port         int
	username     string
	password     string
	topicPrefix  string
	subTopic     string
	subQoS       byte
	timeout      time.Duration
	mu           sync.Mutex
	connected    bool
	subscribed   bool
	conn         net.Conn          // TCP or TLS connection to the MQTT broker
	pointMap     map[string]string // point name -> topic
	lastValues   map[string]interface{}
	lastQuality  map[string]string
	lastStamp    map[string]time.Time
	lastStale    time.Time // last message that could not be mapped to a point
	clientID     string
	keepAlive    int // seconds
	cleanSession bool

	tlsCfg *tls.Config // nil when the broker is reached in plaintext
}

// mqttTLSConfig holds the TLS options of a broker connection.
type mqttTLSConfig struct {
	enabled    bool
	caCert     string
	clientCert string
	clientKey  string
	certReqs   string // required | optional | none
}

// build returns a tls.Config, or nil when TLS is not enabled.
func (t mqttTLSConfig) build(serverName string) (*tls.Config, error) {
	if !t.enabled {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
	switch t.certReqs {
	case "none":
		// Explicitly configured "no verification"; a self-signed broker is the use case.
		cfg.InsecureSkipVerify = true
	case "optional":
		if t.caCert == "" {
			cfg.InsecureSkipVerify = true
		}
	case "required", "":
	default:
		return nil, fmt.Errorf("invalid cert_reqs %q: expected required, optional or none", t.certReqs)
	}

	if t.caCert != "" {
		pool := x509.NewCertPool()
		pem, err := readPEMOrFile(t.caCert)
		if err != nil {
			return nil, fmt.Errorf("ca_cert: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_cert contains no parseable PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if (t.clientCert == "") != (t.clientKey == "") {
		return nil, fmt.Errorf("client_cert and client_key must be configured together")
	}
	if t.clientCert != "" {
		certPEM, err := readPEMOrFile(t.clientCert)
		if err != nil {
			return nil, fmt.Errorf("client_cert: %w", err)
		}
		keyPEM, err := readPEMOrFile(t.clientKey)
		if err != nil {
			return nil, fmt.Errorf("client_key: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("client certificate/key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// readPEMOrFile accepts either inline PEM content (what the certificate fields
// ask the operator to paste) or a path to a PEM file.
func readPEMOrFile(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "-----BEGIN") {
		return []byte(trimmed), nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("not PEM and not a readable file: %w", err)
	}
	return data, nil
}

// NewMQTTClientDriver creates a new MQTTClientDriver.
func NewMQTTClientDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	qos := GetConfigInt(config, "qos", 1)
	if qos < 0 || qos > 2 {
		return nil, fmt.Errorf("invalid qos %d: MQTT supports 0, 1 or 2", qos)
	}
	tlsOpts := mqttTLSConfig{
		enabled:    GetConfigBool(config, "tls_enabled", false),
		caCert:     strings.TrimSpace(GetConfigString(config, "ca_cert", "")),
		clientCert: strings.TrimSpace(GetConfigString(config, "client_cert", "")),
		clientKey:  strings.TrimSpace(GetConfigString(config, "client_key", "")),
		certReqs:   strings.ToLower(strings.TrimSpace(GetConfigString(config, "cert_reqs", "required"))),
	}
	d := &MQTTClientDriver{
		broker:      GetConfigString(config, "broker", "localhost"),
		port:        GetConfigInt(config, "port", 1883),
		username:    GetConfigString(config, "username", ""),
		password:    GetConfigString(config, "password", ""),
		topicPrefix: GetConfigString(config, "topic_prefix", "edgelite"),
		subTopic:    GetConfigString(config, "subscribe_topic", ""),
		subQoS:      byte(qos),
		timeout:     time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
		pointMap:    make(map[string]string),
		lastValues:  make(map[string]interface{}),
		lastQuality: make(map[string]string),
		lastStamp:   make(map[string]time.Time),
		clientID:    fmt.Sprintf("edgelite_%s", deviceID),
		keepAlive:   GetConfigInt(config, "keepalive", 60),
		cleanSession: GetConfigBool(config, "clean_session", true),
	}
	if d.keepAlive <= 0 {
		d.keepAlive = 60
	}
	if d.timeout <= 0 {
		d.timeout = 5 * time.Second
	}
	if cid := GetConfigString(config, "client_id", ""); cid != "" {
		d.clientID = cid
	}
	tlsCfg, err := tlsOpts.build(d.broker)
	if err != nil {
		return nil, err
	}
	d.tlsCfg = tlsCfg
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *MQTTClientDriver) Name() string { return "mqtt_client" }

// dialBroker opens the transport the device configures: plaintext TCP, or TLS
// with the broker certificate validated according to cert_reqs.
func (d *MQTTClientDriver) dialBroker(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: d.timeout}
	if d.tlsCfg == nil {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("mqtt dial %s: %w", addr, err)
		}
		return conn, nil
	}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: d.tlsCfg}
	conn, err := tlsDialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mqtt tls dial %s: %w", addr, err)
	}
	return conn, nil
}

func (d *MQTTClientDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Reconnect guard: connecting while already connected would overwrite d.conn
	// and leak the previous broker connection. Treat as idempotent success.
	if d.connected {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "mqtt connecting")

	// Connect to the MQTT broker
	addr := net.JoinHostPort(d.broker, fmt.Sprintf("%d", d.port))
	conn, err := d.dialBroker(ctx, addr)
	if err != nil {
		// "Simulated mode" kept the device online with an empty value set forever,
		// so a wrong broker address was indistinguishable from a quiet topic.
		// The connect attempt is also the only place this failure is seen: the
		// collect loop never reaches ReadPoints, so this is what puts an
		// unreachable broker into the health counters.
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return err
	}
	d.conn = conn

	// Send MQTT CONNECT packet
	if err := d.sendMQTTConnect(); err != nil {
		conn.Close()
		d.conn = nil
		d.RecordReadFailure()
		return fmt.Errorf("mqtt connect to %s: %w", addr, err)
	}

	// Read CONNACK
	if err := d.readMQTTConnAck(); err != nil {
		conn.Close()
		d.conn = nil
		d.RecordReadFailure()
		return fmt.Errorf("mqtt connack from %s: %w", addr, err)
	}

	d.connected = true
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "mqtt connected")
	// Subscribe the configured point topics and start the inbound reader.
	if err := d.subscribeAllLocked(nil); err != nil {
		logrus.WithField("device_id", d.DeviceID()).
			WithError(err).Warn("MQTT subscribe failed")
	}
	go d.readLoop()
	logrus.WithField("device_id", d.DeviceID()).
		WithField("broker", d.broker).
		WithField("tls", d.tlsCfg != nil).
		Info("MQTT client connected to broker")
	return nil
}

// subscribeAllLocked sends a SUBSCRIBE packet for every topic the driver must
// receive (call with d.mu held) and starts the keep-alive pinger once.
// Passing a nil/empty point list still subscribes the topic prefix wildcard so
// that messages arriving before the first collection are not dropped.
func (d *MQTTClientDriver) subscribeAllLocked(points []models.PointDef) error {
	if d.conn == nil {
		return fmt.Errorf("not connected to broker")
	}
	topics := d.subscriptionTopics(points)
	if len(topics) == 0 {
		return nil
	}
	if err := d.sendMQTTSubscribe(topics, d.subQoS); err != nil {
		return err
	}
	d.subscribed = true
	return nil
}

// subscriptionTopics returns the de-duplicated MQTT topic filters to subscribe:
// every point's own topic plus a wildcard under the configured prefix.
func (d *MQTTClientDriver) subscriptionTopics(points []models.PointDef) []string {
	seen := make(map[string]bool)
	topics := make([]string, 0, len(points)+2)
	add := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		topics = append(topics, t)
	}
	for _, pt := range points {
		add(d.TopicForPoint(pt))
	}
	if d.subTopic != "" {
		add(d.subTopic)
	}
	if d.topicPrefix != "" {
		add(d.topicPrefix + "/#")
	}
	return topics
}

// TopicForPoint resolves the MQTT topic carrying a point's value, mirroring the
// ProtoForge/MQTT device convention {topic_prefix}/{device_id}/{point_name}.
// An address containing "/" is an explicit topic; "{device_id}" is substituted.
func (d *MQTTClientDriver) TopicForPoint(pt models.PointDef) string {
	address := strings.TrimSpace(pt.Address)
	if address == "" {
		address = strings.TrimSpace(pt.Name)
	}
	if strings.Contains(address, "{device_id}") {
		return strings.ReplaceAll(address, "{device_id}", d.DeviceID())
	}
	if strings.Contains(address, "/") {
		return address
	}
	prefix := d.topicPrefix
	if prefix == "" {
		prefix = "edgelite"
	}
	return prefix + "/" + d.DeviceID() + "/" + address
}

// matchTopicFilter reports whether an MQTT topic matches a filter with +/# wildcards.
func matchTopicFilter(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if seg == "+" {
			continue
		}
		if seg != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// sendMQTTSubscribe sends an MQTT SUBSCRIBE packet for the given topic filters.
func (d *MQTTClientDriver) sendMQTTSubscribe(topics []string, qos byte) error {
	var vh bytes.Buffer
	vh.WriteByte(0x00) // Packet ID high
	vh.WriteByte(0x01) // Packet ID low
	var payload bytes.Buffer
	for _, topic := range topics {
		writeMQTTString(&payload, topic)
		payload.WriteByte(qos)
	}

	var packet bytes.Buffer
	packet.WriteByte(0x82) // SUBSCRIBE with reserved bits
	writeMQTTRemainingLength(&packet, vh.Len()+payload.Len())
	packet.Write(vh.Bytes())
	packet.Write(payload.Bytes())

	d.conn.SetWriteDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(packet.Bytes()); err != nil {
		return fmt.Errorf("mqtt subscribe write: %w", err)
	}
	// SUBACK is not read here: the driver's inbound readLoop owns the socket and
	// simply skips non-PUBLISH control packets. Blocking on a synchronous SUBACK
	// would stall Connect for the full timeout against brokers that answer
	// asynchronously (the MQTT 3.1.1 contract).
	return nil
}

// readLoop consumes inbound MQTT packets until the connection closes and feeds
// PUBLISH payloads into HandleMessage. Without this loop the driver can connect
// to a broker but never receives device data.
func (d *MQTTClientDriver) readLoop() {
	for {
		d.mu.Lock()
		conn := d.conn
		d.mu.Unlock()
		if conn == nil {
			return
		}
		// A long read deadline keeps the loop parked between publishes without
		// busy-looping; the keep-alive pinger refreshes the connection.
		_ = conn.SetReadDeadline(time.Now().Add(time.Duration(d.keepAlive) * time.Second))
		packet, err := readMQTTPacket(conn)
		if err != nil {
			// The socket is dead (peer closed, or the keep-alive read deadline
			// elapsed without traffic). A dropped inbound stream means the broker
			// is gone, so mark the driver disconnected and close the socket; the
			// reconnect manager will re-establish it. Leaving the connection open
			// would report "connected" forever with no data flowing.
			d.mu.Lock()
			if d.conn == conn {
				d.subscribed = false
				d.conn = nil
				d.connected = false
				d.SetConnected(false)
				conn.Close()
			}
			d.mu.Unlock()
			return
		}
		if packet.typ != 3 { // PUBLISH
			continue
		}
		d.HandleMessage(packet.topic, packet.payload)
	}
}

// mqttIncomingPacket is a decoded inbound MQTT control packet.
type mqttIncomingPacket struct {
	typ     byte
 qos     byte
 retain  bool
 topic   string
 payload []byte
}

// readMQTTPacket decodes one MQTT control packet from a stream reader.
func readMQTTPacket(r io.Reader) (mqttIncomingPacket, error) {
	var out mqttIncomingPacket
	hdr := make([]byte, 1)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return out, err
	}
	out.typ = hdr[0] >> 4
	out.qos = (hdr[0] >> 1) & 0x03
	out.retain = hdr[0]&0x01 != 0

	// Remaining length is a variable-length integer (up to 4 bytes).
	remaining := 0
	multiplier := 1
	for i := 0; i < 4; i++ {
		b := make([]byte, 1)
		if _, err := io.ReadFull(r, b); err != nil {
			return out, err
		}
		remaining += int(b[0]&0x7F) * multiplier
		multiplier *= 128
		if b[0]&0x80 == 0 {
			break
		}
	}
	if remaining < 0 || remaining > 8*1024*1024 {
		return out, fmt.Errorf("mqtt packet remaining length out of range: %d", remaining)
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(r, body); err != nil {
		return out, err
	}
	if out.typ != 3 {
		return out, nil
	}
	// PUBLISH variable header: topic name (2-byte prefixed), packet id when QoS > 0.
	if len(body) < 2 {
		return out, fmt.Errorf("malformed mqtt publish packet")
	}
	topicLen := int(body[0])<<8 | int(body[1])
	if len(body) < 2+topicLen {
		return out, fmt.Errorf("malformed mqtt publish topic")
	}
	out.topic = string(body[2 : 2+topicLen])
	out.payload = body[2+topicLen:]
	return out, nil
}

// sendMQTTConnect sends an MQTT CONNECT packet.
func (d *MQTTClientDriver) sendMQTTConnect() error {
	// MQTT 3.1.1 CONNECT packet
	clientID := d.clientID
	if len(clientID) > 100 {
		clientID = clientID[:100]
	}

	// Variable header
	var vh bytes.Buffer
	vh.Write([]byte{0x00, 0x04}) // Protocol name length
	vh.WriteString("MQTT")       // Protocol name
	vh.WriteByte(0x04)           // Protocol level: MQTT 3.1.1
	// Connect flags: Clean Session(1) + (username/password if set)
	flags := byte(0x00)
	if d.cleanSession {
		flags |= 0x02 // Clean Session
	}
	if d.username != "" {
		flags |= 0x80 // Username flag
	}
	if d.password != "" {
		flags |= 0x40 // Password flag
	}
	vh.WriteByte(flags)
	vh.Write([]byte{byte(d.keepAlive >> 8), byte(d.keepAlive & 0xFF)}) // Keep alive

	// Payload: ClientID, username, password
	var payload bytes.Buffer
	writeMQTTString(&payload, clientID)
	if d.username != "" {
		writeMQTTString(&payload, d.username)
	}
	if d.password != "" {
		writeMQTTString(&payload, d.password)
	}

	// Remaining length
	remLen := vh.Len() + payload.Len()

	// Fixed header
	var packet bytes.Buffer
	packet.WriteByte(0x10) // CONNECT packet type
	writeMQTTRemainingLength(&packet, remLen)
	packet.Write(vh.Bytes())
	packet.Write(payload.Bytes())

	d.conn.SetWriteDeadline(time.Now().Add(d.timeout))
	_, err := d.conn.Write(packet.Bytes())
	return err
}

// readMQTTConnAck reads and validates the MQTT CONNACK packet.
func (d *MQTTClientDriver) readMQTTConnAck() error {
	d.conn.SetReadDeadline(time.Now().Add(d.timeout))
	header := make([]byte, 4)
	if _, err := readFull(d.conn, header); err != nil {
		return fmt.Errorf("read connack: %w", err)
	}
	if header[0] != 0x20 {
		return fmt.Errorf("expected CONNACK (0x20), got 0x%02x", header[0])
	}
	if header[3] != 0x00 {
		return fmt.Errorf("connection rejected with code: 0x%02x", header[3])
	}
	return nil
}

// sendMQTTPublish sends an MQTT PUBLISH packet.
func (d *MQTTClientDriver) sendMQTTPublish(topic string, payload []byte, qos byte) error {
	if d.conn == nil {
		return fmt.Errorf("not connected to broker")
	}

	// Variable header: topic name
	var vh bytes.Buffer
	writeMQTTString(&vh, topic)
	if qos > 0 {
		vh.WriteByte(0x00) // Packet ID high
		vh.WriteByte(0x01) // Packet ID low
	}

	remLen := vh.Len() + len(payload)

	var packet bytes.Buffer
	packet.WriteByte(0x30 | (qos << 1)) // PUBLISH packet type with QoS
	writeMQTTRemainingLength(&packet, remLen)
	packet.Write(vh.Bytes())
	packet.Write(payload)

	d.conn.SetWriteDeadline(time.Now().Add(d.timeout))
	_, err := d.conn.Write(packet.Bytes())
	return err
}

// writeMQTTString writes a UTF-8 string with 2-byte length prefix.
func writeMQTTString(buf *bytes.Buffer, s string) {
	buf.WriteByte(byte(len(s) >> 8))
	buf.WriteByte(byte(len(s) & 0xFF))
	buf.WriteString(s)
}

// writeMQTTRemainingLength encodes the MQTT remaining length field.
func writeMQTTRemainingLength(buf *bytes.Buffer, length int) {
	for {
		digit := byte(length % 128)
		length /= 128
		if length > 0 {
			digit |= 0x80
		}
		buf.WriteByte(digit)
		if length == 0 {
			break
		}
	}
}

func (d *MQTTClientDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Send MQTT DISCONNECT packet if connected to broker
	if d.conn != nil {
		// MQTT DISCONNECT: fixed header 0xE0, remaining length 0x00
		d.conn.SetDeadline(time.Now().Add(d.timeout))
		d.conn.Write([]byte{0xE0, 0x00})
		d.conn.Close()
		d.conn = nil
	}
	d.connected = false
	d.SetConnected(false)
	return nil
}

func (d *MQTTClientDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	// The drivers are the only writers of the health counters the device panels
	// read, so a driver that records nothing left an MQTT device that had been
	// streaming for days reporting "no samples" while the scheduler called it
	// online. What is measured here is the broker link, which the readLoop keeps
	// current from real socket events.
	if !d.IsConnected() {
		d.RecordReadFailure()
		return nil, fmt.Errorf("mqtt not connected")
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Learn point -> topic mappings so the reader can attribute payloads.
	d.learnPointTopics(points)

	// Refresh topic subscriptions when the point list changes (collection drives
	// this call with the device's configured points).
	if d.conn != nil && !d.subscribed {
		if err := d.subscribeAllLocked(points); err != nil {
			logrus.WithField("device_id", d.DeviceID()).
				WithError(err).Warn("MQTT subscribe refresh failed")
		}
	}

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	for _, pt := range points {
		val, ok := d.lastValues[pt.Name]
		quality := "good"
		if q, exists := d.lastQuality[pt.Name]; exists && q != "" {
			quality = q
		}
		if !ok {
			val = nil
			quality = "unknown"
		}
		ts := now
		if stamp, exists := d.lastStamp[pt.Name]; exists && !stamp.IsZero() {
			ts = stamp
		}
		result = append(result, storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Value:     val,
			Quality:   quality,
			Timestamp: ts,
		})
	}
	// A publish-driven read answers out of the cache the broker pushes into, so
	// there is no device round trip to time; the latency stays unmeasured rather
	// than being stored as 0 ms.
	d.RecordReadSuccess(LatencyNotMeasured)
	return result, nil
}

// HandleMessage processes an incoming MQTT message and updates point values.
func (d *MQTTClientDriver) HandleMessage(topic string, payload []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applyMessageLocked(topic, payload)
}

// applyMessageLocked decodes an MQTT payload into point values (call with d.mu held).
//
// Supported payload shapes:
//   - ProtoForge/MQTT device format: {"device_id":..,"point":<name>,"value":<v>,
//     "timestamp":<unix seconds>,"unit":..} — a single point per message.
//   - Flat object of point name -> value (also used for JSON-array pushes where
//     each element carries {"point"/"name"/"key":..,"value":..}).
//   - Any other JSON value or plain text, keyed by topic.
//
// Point names are resolved against the point definitions learned from previous
// ReadPoints calls so that the stored key matches the collection's point name.
func (d *MQTTClientDriver) applyMessageLocked(topic string, payload []byte) {
	valueAt := func(i int) (name string, value interface{}, ts interface{}, quality string, ok bool) {
		switch obj := payloadAt(payload, i).(type) {
		case map[string]interface{}:
			for _, key := range []string{"point", "name", "key", "tag", "metric"} {
				if s, isStr := obj[key].(string); isStr && s != "" {
					name = s
					break
				}
			}
			value, hasValue := obj["value"]
			if !hasValue {
				if v, exists := obj["v"]; exists {
					value, hasValue = v, true
				}
			}
			if q, isStr := obj["quality"].(string); isStr {
				quality = q
			}
			return name, value, obj["timestamp"], quality, hasValue
		default:
			return "", nil, nil, "", false
		}
	}

	// Single-point device payload: {point, value} (possibly with a timestamp).
	if name, value, ts, quality, ok := valueAt(0); ok && name != "" {
		d.storePointValue(d.resolvePointName(topic, name), value, ts, quality)
		return
	}

	// Flat object of point name -> value.
	if flat, err := decodeJSONObject(payload); err == nil {
		for name, value := range flat {
			d.storePointValue(d.resolvePointName(topic, name), value, nil, "")
		}
		return
	}

	// Array of {point, value} objects.
	if n := payloadObjectCount(payload); n > 1 {
		stored := false
		for i := 0; i < n; i++ {
			name, value, ts, quality, ok := valueAt(i)
			if !ok || name == "" {
				continue
			}
			d.storePointValue(d.resolvePointName(topic, name), value, ts, quality)
			stored = true
		}
		if stored {
			return
		}
	}

	// Non-JSON payload or unmappable JSON: keep the raw text keyed by the
	// matching point (topic) so the value is still observable.
	if name := d.pointForTopic(topic); name != "" {
		d.storePointValue(name, string(payload), nil, "")
		return
	}
	d.lastValues[topic] = string(payload)
	d.lastQuality[topic] = "good"
	d.lastStamp[topic] = time.Now()
}

// storePointValue records one decoded point sample.
func (d *MQTTClientDriver) storePointValue(name string, value interface{}, rawTS interface{}, quality string) {
	if name == "" {
		return
	}
	d.lastValues[name] = value
	if quality == "" {
		// A null/absent value is not a trustworthy reading. Defaulting every
		// payload that merely carried a "value" key to "good" would fabricate
		// quality for {"point":"x","value":null}.
		if value == nil {
			quality = "bad"
		} else {
			quality = "good"
		}
	}
	d.lastQuality[name] = quality
	d.lastStamp[name] = parseMQTTTimestamp(rawTS)
}

// parseMQTTTimestamp converts a payload timestamp (unix seconds as float64 or
// an RFC3339 string) into local time; zero when absent or unparseable.
func parseMQTTTimestamp(raw interface{}) time.Time {
	switch v := raw.(type) {
	case float64:
		if v <= 0 {
			return time.Time{}
		}
		sec := int64(v)
		return time.Unix(sec, int64((v-float64(sec))*1e9))
	case int64:
		return time.Unix(v, 0)
	case string:
		if v == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.Local()
		}
	}
	return time.Time{}
}

// resolvePointName maps a name carried by a payload (a point name, or a topic
// leaf) back to the configured point name.
func (d *MQTTClientDriver) resolvePointName(topic, name string) string {
	if name == "" {
		return d.pointForTopic(topic)
	}
	if _, known := d.pointMap[name]; known {
		return name
	}
	for point, topicOfPoint := range d.pointMap {
		if topicOfPoint == topic {
			return point
		}
	}
	if leaf := lastTopicSegment(topic); leaf != "" {
		if _, known := d.pointMap[leaf]; known {
			return leaf
		}
	}
	return name
}

// pointForTopic returns the point name whose configured topic matches (exactly
// or through a wildcard filter) the given topic.
func (d *MQTTClientDriver) pointForTopic(topic string) string {
	for name, t := range d.pointMap {
		if t == topic {
			return name
		}
	}
	for name, t := range d.pointMap {
		if strings.Contains(t, "#") || strings.Contains(t, "+") {
			if matchTopicFilter(t, topic) {
				return name
			}
		}
	}
	if leaf := lastTopicSegment(topic); leaf != "" {
		if _, known := d.pointMap[leaf]; known {
			return leaf
		}
	}
	return ""
}

// lastTopicSegment returns the trailing '/'-separated segment of an MQTT topic.
func lastTopicSegment(topic string) string {
	if i := strings.LastIndex(topic, "/"); i >= 0 {
		return topic[i+1:]
	}
	return topic
}

// learnPointTopics records the topic of every collected point so later
// ReadPoints calls can subscribe to them.
func (d *MQTTClientDriver) learnPointTopics(points []models.PointDef) {
	for _, pt := range points {
		if pt.Name == "" {
			continue
		}
		d.pointMap[pt.Name] = d.TopicForPoint(pt)
	}
}

// rememberSeenPoint records a point name observed outside the payload path.
func (d *MQTTClientDriver) rememberSeenPoint(name string) {
	if _, exists := d.pointMap[name]; !exists {
		d.pointMap[name] = d.TopicForPoint(models.PointDef{Name: name})
	}
}

func (d *MQTTClientDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if !d.IsConnected() {
		return fmt.Errorf("mqtt not connected")
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	// Encode the command as a well-formed JSON object. Using json.Marshal (rather
	// than fmt's %v) guarantees string/bool values are quoted, so the published
	// payload is always valid JSON that a subscriber can decode.
	payload, err := json.Marshal(map[string]interface{}{
		"point": point,
		"value": value,
	})
	if err != nil {
		return fmt.Errorf("mqtt encode payload: %w", err)
	}
	if len(payload) > 256*1024 {
		return fmt.Errorf("mqtt publish payload too large: %d bytes", len(payload))
	}

	d.mu.Lock()
	// The default command topic is {prefix}/{device}/command, which is the
	// contract simulators and PLC gateways subscribe to. A point's mapped topic
	// is its DATA topic (where the device publishes readings); publishing a
	// command there would echo into the device's own data stream and be
	// silently dropped by command consumers, so only an address that is
	// explicitly a command topic may override the default.
	publishTopic := fmt.Sprintf("%s/%s/command", d.topicPrefix, d.DeviceID())
	if pt, ok := d.pointMap[point]; ok && strings.HasSuffix(pt, "/command") {
		publishTopic = pt
	}
	// If we have a real broker connection, send PUBLISH packet.
	var pubErr error
	if d.conn != nil {
		if err := d.sendMQTTPublish(publishTopic, payload, 0); err != nil {
			pubErr = err
		}
	}
	d.mu.Unlock()

	if pubErr != nil {
		d.RecordWriteFailure()
		logrus.WithError(pubErr).WithField("device_id", d.DeviceID()).
			WithField("topic", publishTopic).Warn("MQTT publish failed")
		return fmt.Errorf("mqtt publish %s: %w", publishTopic, pubErr)
	}
	d.RecordWriteSuccess()

	logrus.WithFields(logrus.Fields{
		"device_id": d.DeviceID(),
		"topic":     publishTopic,
		"point":     point,
	}).Debug("MQTT publish")
	return nil
}

func (d *MQTTClientDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *MQTTClientDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	// If we have a real connection, send a ping. The inbound readLoop owns the
	// socket and consumes the PINGRESP, so we must not read it here (that would
	// race the reader); a successful write proves the socket is alive, and a dead
	// broker surfaces as a write error or via readLoop marking us disconnected.
	if d.conn != nil {
		d.conn.SetWriteDeadline(time.Now().Add(d.timeout))
		// MQTT PINGREQ: fixed header 0xC0, remaining length 0x00
		if _, err := d.conn.Write([]byte{0xC0, 0x00}); err != nil {
			d.RecordReadFailure()
			return err
		}
		// The PINGRESP arrives on the readLoop, so this call cannot time a round
		// trip. Recording it as 0 ms used to seed the latency history with a
		// sample that every panel then drew as a perfect link.
		d.RecordReadSuccess(LatencyNotMeasured)
		return nil
	}
	d.RecordReadSuccess(LatencyNotMeasured)
	return nil
}

// --- HTTP Webhook Driver ---

// HTTPWebhookDriver moves data over HTTP in both directions. It receives pushed
// payloads (HandleWebhook, called from the device push API) and, when the device
// configures a poll URL, fetches a JSON document on every collection cycle.
type HTTPWebhookDriver struct {
	BaseDriver
	apiKey string

	// Outbound poll.
	urlStr       string
	method       string
	headers      map[string]string
	bodyType     string
	bodyTemplate string
	authType     string
	authToken    string
	dialTimeout  time.Duration
	readTimeout  time.Duration

	// Outbound write target.
	pushURL      string
	writeTimeout time.Duration
	maxRetries   int
	retryBackoff time.Duration

	mu          sync.Mutex
	lastValues  map[string]interface{}
	lastQuality map[string]string
	lastStamp   map[string]time.Time
	lastUpdate  time.Time
}

// maxHTTPPollBody caps how much of a polled response is parsed.
const maxHTTPPollBody = 4 << 20

// NewHTTPWebhookDriver creates a new HTTPWebhookDriver.
func NewHTTPWebhookDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	method := strings.ToUpper(strings.TrimSpace(GetConfigString(config, "method", "GET")))
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	case "":
		method = "GET"
	default:
		return nil, fmt.Errorf("invalid http method %q: expected GET, POST, PUT, PATCH or DELETE", method)
	}

	authType := strings.TrimSpace(GetConfigString(config, "auth_type", "None"))
	switch authType {
	case "", "None", "none":
		authType = "None"
	case "Basic", "Bearer", "OAuth2":
	default:
		return nil, fmt.Errorf("invalid auth_type %q: expected None, Basic, Bearer or OAuth2", authType)
	}

	headers, err := parseHTTPConfigHeaders(config)
	if err != nil {
		return nil, err
	}

	bodyType := strings.ToLower(strings.TrimSpace(GetConfigString(config, "body_type", "json")))
	switch bodyType {
	case "":
		bodyType = "json"
	case "json", "xml", "form", "raw":
	default:
		return nil, fmt.Errorf("invalid body_type %q: expected json, xml, form or raw", bodyType)
	}

	urlStr := strings.TrimSpace(GetConfigString(config, "url", ""))
	if err := validateHTTPDriverURL("url", urlStr); err != nil {
		return nil, err
	}
	pushURL := strings.TrimSpace(GetConfigString(config, "push_url", ""))
	if err := validateHTTPDriverURL("push_url", pushURL); err != nil {
		return nil, err
	}
	// Polling must be opted into explicitly (poll_url, poll_interval, or
	// mode:"poll"). The Python edition's http_webhook driver never polls: `url`
	// there is the webhook's identity/fallback URL, and simulators push data to
	// the gateway instead. Treating a bare `url` as a poll target made every
	// pushed webhook device fail collection and trip its circuit breaker.
	pollURL := strings.TrimSpace(GetConfigString(config, "poll_url", ""))
	if pollURL == "" && (GetConfigFloat(config, "poll_interval", 0) > 0 ||
		strings.EqualFold(strings.TrimSpace(GetConfigString(config, "mode", "")), "poll")) {
		pollURL = urlStr
	}
	if err := validateHTTPDriverURL("poll_url", pollURL); err != nil {
		return nil, err
	}
	urlStr = pollURL

	maxRetries := GetConfigInt(config, "max_retries", 3)
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 10 {
		maxRetries = 10
	}
	retryBackoff := GetConfigFloat(config, "retry_backoff", 1)
	if retryBackoff <= 0 {
		retryBackoff = 1
	}

	d := &HTTPWebhookDriver{
		apiKey:       GetConfigString(config, "api_key", ""),
		urlStr:       urlStr,
		method:       method,
		headers:      headers,
		bodyType:     bodyType,
		bodyTemplate: GetConfigString(config, "body_template", ""),
		authType:     authType,
		authToken:    strings.TrimSpace(GetConfigString(config, "auth_token", "")),
		dialTimeout:  time.Duration(GetConfigFloat(config, "connect_timeout", float64(constants.DeviceConnectTimeout))) * time.Second,
		readTimeout:  time.Duration(GetConfigFloat(config, "read_timeout", 30)) * time.Second,
		pushURL:      pushURL,
		writeTimeout: time.Duration(GetConfigFloat(config, "write_timeout", 10)) * time.Second,
		maxRetries:   maxRetries,
		retryBackoff: time.Duration(retryBackoff * float64(time.Second)),
		lastValues:   make(map[string]interface{}),
		lastQuality:  make(map[string]string),
		lastStamp:    make(map[string]time.Time),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	// The receive side is served by the gateway's own push API, so an inbound-only
	// device is listening from the moment it exists.
	d.SetConnected(true)
	return d, nil
}

// parseHTTPConfigHeaders reads the form's "请求头(JSON)" value. A malformed map is
// a configuration error, not something to silently ignore on every request.
func parseHTTPConfigHeaders(config map[string]interface{}) (map[string]string, error) {
	out := map[string]string{}
	raw, ok := config["headers"]
	if !ok || raw == nil {
		return out, nil
	}
	switch v := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return out, nil
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, fmt.Errorf("invalid headers JSON %q: expected an object of string values", trimmed)
		}
		for k, val := range m {
			out[k] = val
		}
		return out, nil
	case map[string]interface{}:
		for k, val := range v {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("invalid header %q: value must be a string", k)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid headers value of type %T", raw)
	}
}

func validateHTTPDriverURL(key, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %q: scheme must be http or https", key, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s %q: missing host", key, raw)
	}
	return nil
}

// applyAuth sets the Authorization header from the configured auth type.
func (d *HTTPWebhookDriver) applyAuth(req *http.Request) {
	if d.authType == "None" || d.authToken == "" {
		return
	}
	switch d.authType {
	case "Basic":
		// The form carries a single credential field, so a base64 "user:pass" is
		// what an operator can supply here.
		if strings.Contains(d.authToken, " ") {
			req.Header.Set("Authorization", "Basic "+d.authToken)
			return
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.authToken)))
	case "Bearer", "OAuth2":
		req.Header.Set("Authorization", "Bearer "+d.authToken)
	}
}

func (d *HTTPWebhookDriver) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: d.dialTimeout}).DialContext,
		},
	}
}

// request builds the outbound HTTP request for a poll, or for a write to the
// given URL. bodyVars is substituted into the configured body template.
func (d *HTTPWebhookDriver) request(ctx context.Context, method, target string, bodyVars map[string]string) (*http.Request, error) {
	var body io.Reader
	if method != "GET" && method != "DELETE" {
		body = strings.NewReader(renderHTTPBodyTemplate(d.bodyTemplate, bodyVars))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("http request to %s: %w", target, err)
	}
	for k, v := range d.headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" && body != nil {
		req.Header.Set("Content-Type", httpBodyContentType(d.bodyType))
	}
	d.applyAuth(req)
	return req, nil
}

func httpBodyContentType(bodyType string) string {
	switch bodyType {
	case "xml":
		return "application/xml"
	case "form":
		return "application/x-www-form-urlencoded"
	case "raw":
		return "text/plain"
	default:
		return "application/json"
	}
}

// renderHTTPBodyTemplate expands ${key} placeholders. An empty template yields
// the default write payload shape used by the push API.
func renderHTTPBodyTemplate(template string, vars map[string]string) string {
	if strings.TrimSpace(template) == "" && len(vars) > 0 {
		template = `{"point":"${point}","value":${value}}`
	}
	out := template
	for k, v := range vars {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	return out
}

func (d *HTTPWebhookDriver) Name() string { return "http_webhook" }

// APIKey is the shared secret configured for this device's push endpoint.
// Nothing enforces it yet: the gateway's data-push route authenticates with a
// operator JWT plus the device:write permission, so this value is reported to
// the UI as configured-but-not-enforced rather than presented as active auth.
func (d *HTTPWebhookDriver) APIKey() string { return d.apiKey }

func (d *HTTPWebhookDriver) Connect(ctx context.Context) error {
	d.SetConnected(true)
	return nil
}

func (d *HTTPWebhookDriver) Disconnect() error {
	d.SetConnected(false)
	return nil
}

func (d *HTTPWebhookDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	// Checked before the outbound call: while the breaker is open the device must
	// not keep hammering the endpoint.
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	if d.urlStr != "" {
		// A polled endpoint has a genuine request to time; the push-only mode
		// below has none, and storing 0 ms for it would read as an instant server.
		started := time.Now()
		if _, err := d.pollOnce(ctx); err != nil {
			d.RecordReadFailure()
			return nil, err
		}
		d.RecordReadSuccess(ElapsedMs(started))
	} else {
		d.RecordReadSuccess(LatencyNotMeasured)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	for _, pt := range points {
		val, ok := d.lastValues[pt.Name]
		quality := "good"
		if q, exists := d.lastQuality[pt.Name]; exists && q != "" {
			quality = q
		}
		if !ok {
			val = nil
			quality = "unknown"
		}
		ts := now
		if stamp, exists := d.lastStamp[pt.Name]; exists && !stamp.IsZero() {
			ts = stamp
		}
		result = append(result, storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Value:     val,
			Quality:   quality,
			Timestamp: ts,
		})
	}
	return result, nil
}

// HandleWebhook processes an incoming webhook POST request.
//
// Accepted payload shapes:
//   - flat JSON object of point name -> value: {"temperature":25.5,"humidity":60}
//   - JSON array of point objects (the shape used by the device data push API):
//     [{"point":"temperature","value":25.5,"quality":"good","timestamp":"..."}]
//   - JSON object wrapping such an array: {"data":[...]} / {"points":[...]}
func (d *HTTPWebhookDriver) HandleWebhook(payload []byte) error {
	_, err := d.ingest(payload)
	return err
}

// ingest decodes a payload into point samples and stores them as the device's
// current values. It serves both the pushed data and a polled response.
func (d *HTTPWebhookDriver) ingest(payload []byte) (int, error) {
	points, err := flattenWebhookPayload(payload)
	if err != nil {
		return 0, err
	}

	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range points {
		if p.name == "" {
			continue
		}
		quality := p.quality
		if quality == "" {
			// A null value is not a trustworthy reading; defaulting it to "good"
			// would fabricate quality for {"point":"x","value":null}.
			if p.value == nil {
				quality = "bad"
			} else {
				quality = "good"
			}
		}
		d.lastValues[p.name] = p.value
		d.lastQuality[p.name] = quality
		if !p.timestamp.IsZero() {
			d.lastStamp[p.name] = p.timestamp
		}
		d.lastUpdate = now
	}
	if d.lastUpdate.IsZero() {
		d.lastUpdate = now
	}
	return len(points), nil
}

// pollOnce performs one outbound request against the configured interface URL and
// stores the points its response carries.
func (d *HTTPWebhookDriver) pollOnce(ctx context.Context) (int, error) {
	req, err := d.request(ctx, d.method, d.urlStr, nil)
	if err != nil {
		return 0, err
	}
	resp, err := d.httpClient(d.readTimeout).Do(req)
	if err != nil {
		return 0, fmt.Errorf("http poll %s: %w", d.urlStr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPPollBody))
	if err != nil {
		return 0, fmt.Errorf("http poll %s: read response: %w", d.urlStr, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("http poll %s: returned status %d", d.urlStr, resp.StatusCode)
	}
	n, err := d.ingest(body)
	if err != nil {
		return 0, fmt.Errorf("http poll %s: %w", d.urlStr, err)
	}
	return n, nil
}

// webhookPoint is one decoded point sample from a pushed payload.
type webhookPoint struct {
	name      string
	value     interface{}
	quality   string
	timestamp time.Time
}

// pointNameKeys are the JSON keys used to carry a point name in pushed payloads.
var pointNameKeys = []string{"point", "name", "key", "tag", "metric"}

// flattenWebhookPayload decodes a webhook payload into point samples.
func flattenWebhookPayload(payload []byte) ([]webhookPoint, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("invalid JSON: empty payload")
	}

	out := make([]webhookPoint, 0, 8)
	switch trimmed[0] {
	case '[':
		var items []interface{}
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		for _, item := range items {
			if p, ok := webhookPointFromItem(item); ok {
				out = append(out, p)
			}
		}
	case '{':
		var obj map[string]interface{}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		// Nested point array wins over treating the object as a flat map.
		for _, key := range []string{"data", "points", "values", "samples", "metrics"} {
			if arr, ok := obj[key].([]interface{}); ok && len(arr) > 0 {
				for _, item := range arr {
					if p, ok := webhookPointFromItem(item); ok {
						out = append(out, p)
					}
				}
				if len(out) > 0 {
					return out, nil
				}
			}
		}
		// Single-point object: {"point":"x","value":1}
		if p, ok := webhookPointFromItem(obj); ok && p.name != "" {
			usesValueField := false
			for _, key := range pointNameKeys {
				if _, exists := obj[key]; exists {
					usesValueField = true
					break
				}
			}
			if usesValueField {
				return []webhookPoint{p}, nil
			}
		}
		// Flat map of point name -> value.
		for name, value := range obj {
			out = append(out, webhookPoint{name: name, value: value})
		}
	default:
		return nil, fmt.Errorf("invalid JSON: unexpected payload %q", truncateForLog(string(trimmed), 32))
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no point data in payload")
	}
	return out, nil
}

// webhookPointFromItem converts one {"point":..,"value":..} object.
func webhookPointFromItem(item interface{}) (webhookPoint, bool) {
	obj, ok := item.(map[string]interface{})
	if !ok {
		return webhookPoint{}, false
	}
	var name string
	for _, key := range pointNameKeys {
		if s, isStr := obj[key].(string); isStr && s != "" {
			name = s
			break
		}
	}
	value, hasValue := obj["value"]
	if !hasValue {
		if v, exists := obj["v"]; exists {
			value, hasValue = v, true
		}
	}
	if name == "" || !hasValue {
		return webhookPoint{}, false
	}
	p := webhookPoint{name: name, value: value}
	if q, isStr := obj["quality"].(string); isStr {
		p.quality = q
	}
	p.timestamp = parseMQTTTimestamp(obj["timestamp"])
	return p, true
}

// truncateForLog shortens a string for error messages.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// decodeJSONObject unmarshals a payload into a flat object when it is one.
func decodeJSONObject(payload []byte) (map[string]interface{}, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// payloadObjectCount reports how many top-level JSON objects a payload carries.
func payloadObjectCount(payload []byte) int {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return 0
	}
	if trimmed[0] == '[' {
		var items []interface{}
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return 0
		}
		return len(items)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		return 1
	}
	return 0
}

// payloadAt returns the i-th value object of a payload (array element, or the
// object itself when the payload is a single object).
func payloadAt(payload []byte, i int) interface{} {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		var items []interface{}
		if err := json.Unmarshal(trimmed, &items); err != nil || i >= len(items) {
			return nil
		}
		return items[i]
	}
	if i != 0 {
		return nil
	}
	var obj interface{}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil
	}
	return obj
}

// WritePoint pushes the value to the configured 推送URL. A webhook device owns no
// register to write into, so storing the value locally and reporting success
// (which is what this did) showed a confirmed write for a command that never
// left the gateway.
func (d *HTTPWebhookDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if d.pushURL == "" {
		return fmt.Errorf("http_webhook device %s has no push_url: it can only receive data", d.DeviceID())
	}
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	valueJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("http_webhook encode value for point %s: %w", point, err)
	}
	vars := map[string]string{
		"point":     point,
		"value":     string(valueJSON),
		"device_id": d.DeviceID(),
	}

	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := d.retryBackoff * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		req, err := d.writeRequest(ctx, vars)
		if err != nil {
			return err
		}
		resp, err := d.httpClient(d.writeTimeout).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			d.RecordWriteSuccess()
			return nil
		}
		lastErr = fmt.Errorf("push to %s returned status %d: %s", d.pushURL, resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode < 500 && resp.StatusCode != 429 && resp.StatusCode != 408 {
			break // A rejection the server won't accept on retry either.
		}
	}
	d.RecordWriteFailure()
	return fmt.Errorf("http_webhook write: %w", lastErr)
}

// writeRequest builds the POST carrying a point value. A configured body
// template is reused here with ${point}, ${value} and ${device_id} placeholders;
// without one the payload matches the shape the device push API accepts back.
func (d *HTTPWebhookDriver) writeRequest(ctx context.Context, vars map[string]string) (*http.Request, error) {
	return d.request(ctx, "POST", d.pushURL, vars)
}

func (d *HTTPWebhookDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *HTTPWebhookDriver) HealthCheck(ctx context.Context) error {
	// Webhook is healthy if we've received data recently.
	// lastUpdate is written under d.mu in HandleWebhook, so read it under the
	// same lock to avoid a data race.
	d.mu.Lock()
	last := d.lastUpdate
	d.mu.Unlock()
	if time.Since(last) > 5*time.Minute {
		return fmt.Errorf("no webhook data received in 5 minutes")
	}
	return nil
}

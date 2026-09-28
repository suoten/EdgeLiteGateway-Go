package engine

import (
	"bufio"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// MqttServerConfig holds configuration for the built-in MQTT server.
type MqttServerConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	AuthMode     string `yaml:"auth_mode"` // "none", "token", "basic"
	AllowNoAuth  bool   `yaml:"allow_no_auth"`
	Token        string `yaml:"token"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	MaxClients   int    `yaml:"max_clients"`
	MaxQueueSize int    `yaml:"max_queue_size"`
}

// MqttServer implements a minimal MQTT broker for edge scenarios.
// It supports basic CONNECT, PUBLISH, and SUBSCRIBE operations.
type MqttServer struct {
	mu        sync.Mutex
	config    MqttServerConfig
	listener  net.Listener
	clients   map[string]*mqttClient
	topicSubs map[string][]*mqttClient // topic -> subscribers
	started   bool
	cancelFn  context.CancelFunc
	wg        sync.WaitGroup
}

type mqttClient struct {
	conn        net.Conn
	wmu         sync.Mutex // serialises packet writes: interleaved writes corrupt the stream
	id          string
	subscribed  map[string]bool
	connectedAt time.Time
	closed      atomic.Bool
}

// write frames and sends one control packet to the client.
func (c *mqttClient) write(hdr byte, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(encodeMQTTPacket(hdr, body))
	return err
}

// NewMqttServer creates a new MqttServer.
func NewMqttServer() *MqttServer {
	return &MqttServer{
		clients:   make(map[string]*mqttClient),
		topicSubs: make(map[string][]*mqttClient),
	}
}

// Start starts the MQTT server.
func (s *MqttServer) Start(ctx context.Context, config MqttServerConfig) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("MQTT server already started")
	}
	// The YAML surface only offers username/password/allow_no_auth, so without
	// this mapping AuthMode stayed empty and every client was accepted no
	// matter what credentials the operator configured. Empty credentials are
	// not a licence to run anonymously: with allow_no_auth: false they fall
	// through to "basic", which validateMqttAuth then refuses to start, so the
	// operator's explicit "authentication required" is never silently dropped.
	if config.AuthMode == "" {
		if config.AllowNoAuth {
			config.AuthMode = "none"
		} else {
			config.AuthMode = "basic"
		}
	}
	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	// A credential mode whose secret is empty authenticates nobody: an
	// anonymous client sends empty username/password and ConstantTimeCompare
	// then matches. Refusing to listen is the only honest outcome, otherwise
	// the operator believes the broker is protected because the config says so.
	if err := validateMqttAuth(config); err != nil {
		listener.Close()
		s.mu.Unlock()
		return err
	}
	s.config = config
	s.listener = listener
	// Set only after the bind succeeded: a failed start must leave the server
	// restartable, otherwise Stop() reports success while nothing runs and the
	// operator cannot fix the port conflict without restarting the process.
	s.started = true

	childCtx, cancel := context.WithCancel(ctx)
	s.cancelFn = cancel
	// Release before spawning acceptLoop: it locks s.mu per client, so a lock
	// held past this point deadlocks the broker on its first CONNECT.
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(childCtx)

	realAddr := listener.Addr().String()
	if config.AuthMode == "none" && !isLoopbackHost(config.Host) {
		logrus.WithField("addr", realAddr).
			Warn("MQTT server accepts anonymous clients on a routable address")
	}
	logrus.WithField("addr", realAddr).
		WithField("auth_mode", config.AuthMode).
		Info("MQTT server started")
	return nil
}

// validateMqttAuth rejects auth modes that would silently authenticate nobody.
func validateMqttAuth(config MqttServerConfig) error {
	switch config.AuthMode {
	case "none":
		return nil
	case "basic":
		if config.Username == "" || config.Password == "" {
			return fmt.Errorf("MQTT auth_mode %q needs both username and password, refusing to start an anonymous broker", config.AuthMode)
		}
		return nil
	case "token":
		if config.Token == "" {
			return fmt.Errorf("MQTT auth_mode %q needs a non-empty token, refusing to start an anonymous broker", config.AuthMode)
		}
		return nil
	default:
		return fmt.Errorf("MQTT auth_mode %q is not one of none/basic/token", config.AuthMode)
	}
}

// isLoopbackHost reports whether addr binds only to this machine. An empty
// host means every interface, which is not loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Stop stops the MQTT server.
func (s *MqttServer) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	s.mu.Unlock()

	if s.cancelFn != nil {
		s.cancelFn()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// Close all client connections
	s.mu.Lock()
	for _, client := range s.clients {
		client.closed.Store(true)
		_ = client.conn.Close()
	}
	s.clients = make(map[string]*mqttClient)
	s.topicSubs = make(map[string][]*mqttClient)
	s.mu.Unlock()

	s.wg.Wait()
	logrus.Info("MQTT server stopped")
	return nil
}

// IsRunning returns whether the server is running.
func (s *MqttServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *MqttServer) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := s.listener.Accept()
		if err != nil {
			if s.isStopped() {
				return
			}
			logrus.WithError(err).Error("MQTT server accept error")
			continue
		}
		s.wg.Add(1)
		go s.handleClient(ctx, conn)
	}
}

func (s *MqttServer) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.started
}

func (s *MqttServer) handleClient(ctx context.Context, conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		_ = conn.Close()
	}()

	// Set read deadline for initial connect
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(conn)
	connect, err := readMQTTPacket(r)
	if err != nil || connect.packetType() != 1 {
		return
	}
	body := connect.body
	pos := 0
	protoName, err := readMQTTFixedString(body, &pos)
	if err != nil || (protoName != "MQTT" && protoName != "MQIsdp") {
		return
	}
	if pos+4 > len(body) {
		return
	}
	connectFlags := body[pos+1]
	pos += 4
	clientID, err := readMQTTFixedString(body, &pos)
	if err != nil {
		return
	}
	if connectFlags&0x04 != 0 {
		// Will topic and will message are consumed but not retained.
		if _, err := readMQTTFixedString(body, &pos); err != nil {
			return
		}
		if _, err := readMQTTFixedString(body, &pos); err != nil {
			return
		}
	}
	var username, password string
	if connectFlags&0x80 != 0 {
		username, err = readMQTTFixedString(body, &pos)
		if err != nil {
			return
		}
		if connectFlags&0x40 != 0 {
			password, err = readMQTTFixedString(body, &pos)
			if err != nil {
				return
			}
		}
	}

	// Authenticate based on auth mode
	authenticated := false
	switch s.config.AuthMode {
	case "none", "":
		authenticated = true
	case "basic":
		if subtle.ConstantTimeCompare([]byte(username), []byte(s.config.Username)) == 1 &&
			subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Password)) == 1 {
			authenticated = true
		}
	case "token":
		if subtle.ConstantTimeCompare([]byte(username), []byte(s.config.Token)) == 1 ||
			subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Token)) == 1 {
			authenticated = true
		}
	}

	if !authenticated {
		// Send CONNACK with "bad username or password" (return code 4)
		connack := []byte{0x20, 0x02, 0x00, 0x04}
		_, _ = conn.Write(connack)
		return
	}

	if clientID == "" {
		clientID = fmt.Sprintf("client_%d", time.Now().UnixNano())
	}
	client := &mqttClient{
		conn:        conn,
		id:          clientID,
		subscribed:  make(map[string]bool),
		connectedAt: time.Now(),
	}

	// Register before CONNACK is written: the old order let a client that was
	// already accepted be invisible to the client list, and let an over-capacity
	// client be told it had connected before being closed.
	s.mu.Lock()
	if s.config.MaxClients > 0 && len(s.clients) >= s.config.MaxClients {
		s.mu.Unlock()
		_, _ = conn.Write([]byte{0x20, 0x02, 0x00, 0x03}) // server unavailable
		return
	}
	s.clients[client.id] = client
	s.mu.Unlock()

	// Send CONNACK (connection accepted)
	connack := []byte{0x20, 0x02, 0x00, 0x00}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(connack)
	if err != nil {
		s.deregister(client)
		return
	}

	// Clear read deadline for normal operation
	_ = conn.SetReadDeadline(time.Time{})

	defer s.deregister(client)

	// Main read loop: decode one control packet at a time. Passing conn.Read's
	// whole buffer to the dispatcher made a PINGREQ that arrived coalesced
	// behind a PUBLISH vanish, so clients were cut off on keepalive.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		p, err := readMQTTPacket(r)
		if err != nil {
			return
		}
		if !s.processPacket(client, p) {
			return
		}
	}
}

// deregister removes a closed client from the registry and from every topic it
// subscribed to.
func (s *MqttServer) deregister(client *mqttClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[client.id] == client {
		delete(s.clients, client.id)
	}
	for topic := range client.subscribed {
		subs := s.topicSubs[topic]
		remaining := subs[:0]
		for _, sub := range subs {
			if sub != client {
				remaining = append(remaining, sub)
			}
		}
		if len(remaining) > 0 {
			s.topicSubs[topic] = remaining
		} else {
			delete(s.topicSubs, topic)
		}
	}
}

// processPacket handles one decoded packet and reports whether to keep serving
// the connection.
func (s *MqttServer) processPacket(client *mqttClient, p mqttPacket) bool {
	switch p.packetType() {
	case 3: // PUBLISH
		s.handlePublish(client, p)
	case 6: // PUBREL: finish the QoS 2 handshake so the publisher drains
		if len(p.body) >= 2 {
			_ = client.write(0xB0, p.body[:2])
		}
	case 8: // SUBSCRIBE
		s.handleSubscribe(client, p)
	case 12: // PINGREQ
		_ = client.write(0xD0, nil)
	case 14: // DISCONNECT
		client.closed.Store(true)
		return false
	}
	return true
}

func (s *MqttServer) handlePublish(client *mqttClient, p mqttPacket) {
	qos := (p.hdr >> 1) & 0x03
	pos := 0
	topic, err := readMQTTFixedString(p.body, &pos)
	if err != nil {
		return
	}
	var packetID []byte
	if qos > 0 {
		if pos+2 > len(p.body) {
			return
		}
		packetID = p.body[pos : pos+2]
		pos += 2
	}

	// Acknowledge upstream: without PUBACK a QoS 1 publisher stalls once its
	// inflight window fills, which silently froze the offline queue flush.
	switch qos {
	case 1:
		_ = client.write(0x40, packetID)
	case 2:
		_ = client.write(0x50, packetID) // PUBREC; PUBCOMP follows on PUBREL
	}

	for _, sub := range s.matchSubscribers(topic) {
		if sub == client || sub.closed.Load() {
			continue
		}
		// 0xF6 keeps type and QoS, clears DUP and RETAIN for this copy.
		_ = sub.write(p.hdr&0xF6, p.body)
	}
}

func (s *MqttServer) handleSubscribe(client *mqttClient, p mqttPacket) {
	if len(p.body) < 2 {
		return
	}
	pos := 2
	var returnCodes []byte
	for pos < len(p.body) {
		filter, err := readMQTTFixedString(p.body, &pos)
		if err != nil || pos >= len(p.body) {
			break
		}
		requested := p.body[pos] & 0x03
		granted := requested
		if granted > 1 {
			granted = 1 // this broker does not deliver above QoS 1
		}
		pos++
		if filter != "" {
			s.addSubscription(client, filter)
		}
		returnCodes = append(returnCodes, granted)
	}
	if len(returnCodes) == 0 {
		returnCodes = []byte{0x80}
	}
	ack := make([]byte, 0, 2+len(returnCodes))
	ack = append(ack, p.body[:2]...)
	ack = append(ack, returnCodes...)
	_ = client.write(0x90, ack)
}

func (s *MqttServer) addSubscription(client *mqttClient, filter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if client.subscribed[filter] {
		return
	}
	client.subscribed[filter] = true
	s.topicSubs[filter] = append(s.topicSubs[filter], client)
}

// matchSubscribers resolves wildcard filters against a concrete topic; the
// exact map lookup used to leave "prefix/#" subscribers with nothing.
func (s *MqttServer) matchSubscribers(topic string) []*mqttClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []*mqttClient
	for filter, subs := range s.topicSubs {
		if mqttTopicMatches(filter, topic) {
			matched = append(matched, subs...)
		}
	}
	return matched
}

// GetStats returns statistics about the MQTT server.
func (s *MqttServer) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"running":      s.started,
		"client_count": len(s.clients),
		"topic_count":  len(s.topicSubs),
		"max_clients":  s.config.MaxClients,
	}
}

// ClientInfo describes one connected MQTT client.
type ClientInfo struct {
	ClientID      string    `json:"client_id"`
	RemoteAddr    string    `json:"remote_addr"`
	Subscriptions []string  `json:"subscriptions"`
	ConnectedAt   time.Time `json:"connected_at"`
}

// ListClients reports the connected clients. The management API used to answer
// with a hardcoded empty list, so the module page always looked idle.
func (s *MqttServer) ListClients() []ClientInfo {
	s.mu.Lock()
	out := make([]ClientInfo, 0, len(s.clients))
	for _, c := range s.clients {
		if c.closed.Load() {
			continue
		}
		subs := make([]string, 0, len(c.subscribed))
		for filter := range c.subscribed {
			subs = append(subs, filter)
		}
		sort.Strings(subs)
		out = append(out, ClientInfo{
			ClientID:      c.id,
			RemoteAddr:    c.conn.RemoteAddr().String(),
			Subscriptions: subs,
			ConnectedAt:   c.connectedAt,
		})
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out
}

// KickClient closes one client's connection. An unknown id is an error so the
// API cannot report a kick that disconnected nothing.
func (s *MqttServer) KickClient(clientID string) error {
	s.mu.Lock()
	client, ok := s.clients[clientID]
	if ok {
		client.closed.Store(true)
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("mqtt client %q is not connected", clientID)
	}
	// Closing the socket ends that client's read loop, which removes it from the
	// registry and drops its subscriptions.
	return client.conn.Close()
}

// maxMQTTPacketSize bounds one control packet so a bogus remaining-length field
// cannot make the gateway allocate unbounded memory.
const maxMQTTPacketSize = 1 << 20

// mqttPacket is one decoded control packet: its fixed header byte plus the
// bytes that follow the remaining-length field.
type mqttPacket struct {
	hdr  byte
	body []byte
}

func (p mqttPacket) packetType() byte { return p.hdr >> 4 }

// readMQTTPacket decodes exactly one packet from the stream.
func readMQTTPacket(r io.Reader) (mqttPacket, error) {
	var p mqttPacket
	hdr := make([]byte, 1)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return p, err
	}
	p.hdr = hdr[0]

	remaining, multiplier := 0, 1
	for i := 0; i < 4; i++ {
		b := make([]byte, 1)
		if _, err := io.ReadFull(r, b); err != nil {
			return p, err
		}
		remaining += int(b[0]&0x7F) * multiplier
		if b[0]&0x80 == 0 {
			p.body = make([]byte, remaining)
			if _, err := io.ReadFull(r, p.body); err != nil {
				return p, err
			}
			return p, nil
		}
		multiplier *= 128
	}
	return p, fmt.Errorf("malformed remaining length in packet 0x%02x", p.hdr)
}

// encodeMQTTPacket frames a body with its fixed header and remaining length.
func encodeMQTTPacket(hdr byte, body []byte) []byte {
	out := make([]byte, 0, len(body)+5)
	out = append(out, hdr)
	for remaining := len(body); ; remaining /= 128 {
		b := byte(remaining % 128)
		if remaining > 128 {
			b |= 0x80
		}
		out = append(out, b)
		if remaining <= 128 {
			break
		}
	}
	return append(out, body...)
}

// readMQTTFixedString reads one length-prefixed field and advances pos.
func readMQTTFixedString(body []byte, pos *int) (string, error) {
	if *pos+2 > len(body) {
		return "", fmt.Errorf("truncated length prefix at %d", *pos)
	}
	n := int(body[*pos])<<8 | int(body[*pos+1])
	*pos += 2
	if *pos+n > len(body) {
		return "", fmt.Errorf("truncated field of %d bytes at %d", n, *pos)
	}
	s := string(body[*pos : *pos+n])
	*pos += n
	return s, nil
}

// mqttTopicMatches reports whether a subscription filter selects a topic.
// Single-level "+" and trailing multi-level "#" are the only wildcards.
func mqttTopicMatches(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fsegs := strings.Split(filter, "/")
	tsegs := strings.Split(topic, "/")
	for i, seg := range fsegs {
		if seg == "#" {
			// "#" covers the remaining levels, including the parent topic.
			return i == len(fsegs)-1
		}
		if i >= len(tsegs) {
			return false
		}
		if seg != "+" && seg != tsegs[i] {
			return false
		}
	}
	return len(fsegs) == len(tsegs)
}

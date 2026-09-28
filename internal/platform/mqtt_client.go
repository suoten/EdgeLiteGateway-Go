package platform

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// LightweightMQTTClient is a minimal MQTT 3.1.1 client that supports
// CONNECT, PUBLISH, SUBSCRIBE, PINGREQ and DISCONNECT.
// It does not require any third-party dependencies.
type LightweightMQTTClient struct {
	mu        sync.Mutex
	conn      net.Conn
	reader    *bufio.Reader
	broker    string
	port      int
	clientID  string
	username  string
	password  string
	keepAlive int // seconds
	connected bool
	packetID  uint16
	stopCh    chan struct{}
	pingDone  chan struct{}
	wg        sync.WaitGroup
	onMessage func(topic string, payload []byte)
	onDrop    func() // invoked when the connection is lost unexpectedly
	reading   bool   // readLoop running guard
}

// SetDisconnectHandler registers a callback invoked when the connection is
// lost unexpectedly (TCP error / keep-alive timeout), so the owning platform
// handler can flip its IsConnected state and trigger a reconnect.
func (c *LightweightMQTTClient) SetDisconnectHandler(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDrop = fn
}

// NewLightweightMQTTClient creates a new MQTT client.
func NewLightweightMQTTClient(broker string, port int, clientID string) *LightweightMQTTClient {
	return &LightweightMQTTClient{
		broker:    broker,
		port:      port,
		clientID:  clientID,
		keepAlive: 60,
		stopCh:    make(chan struct{}),
		pingDone:  make(chan struct{}),
	}
}

// SetCredentials sets the username and password for MQTT authentication.
func (c *LightweightMQTTClient) SetCredentials(username, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.username = username
	c.password = password
}

// SetKeepAlive sets the keep-alive interval in seconds.
func (c *LightweightMQTTClient) SetKeepAlive(seconds int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keepAlive = seconds
}

// SetMessageHandler sets the callback for received messages.
func (c *LightweightMQTTClient) SetMessageHandler(handler func(topic string, payload []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage = handler
}

// Connect establishes a connection to the MQTT broker.
func (c *LightweightMQTTClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	addr := net.JoinHostPort(c.broker, fmt.Sprintf("%d", c.port))
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("mqtt dial %s: %w", addr, err)
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 65536)

	// Send CONNECT packet
	if err := c.sendConnect(); err != nil {
		conn.Close()
		c.conn = nil
		return fmt.Errorf("mqtt connect: %w", err)
	}

	// Read CONNACK
	if err := c.readConnack(); err != nil {
		conn.Close()
		c.conn = nil
		return fmt.Errorf("mqtt connack: %w", err)
	}

	c.connected = true
	c.packetID = 1

	// Start ping loop
	c.wg.Add(1)
	go c.pingLoop()

	// Start the read loop so broker responses (PINGRESP/PUBACK) are consumed
	// and half-open connections are detected via the read deadline.
	if !c.reading {
		c.reading = true
		c.wg.Add(1)
		go c.readLoop()
	}

	logrus.WithField("broker", addr).
		WithField("client_id", c.clientID).
		Info("MQTT connected")
	return nil
}

// Disconnect sends a DISCONNECT packet and closes the connection.
func (c *LightweightMQTTClient) Disconnect() {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return
	}
	c.connected = false
	c.reading = false
	close(c.stopCh)
	// Send DISCONNECT
	if c.conn != nil {
		_, _ = c.conn.Write([]byte{0xE0, 0x00}) // DISCONNECT packet
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
	c.wg.Wait()

	// Recreate stop channel for future reconnects
	c.stopCh = make(chan struct{})
	logrus.Info("MQTT disconnected")
}

// markDisconnected flags the connection as lost and fires the drop callback.
// Called from the read/ping loops when an I/O error is detected.
func (c *LightweightMQTTClient) markDisconnected(reason string, err error) {
	c.mu.Lock()
	wasConnected := c.connected
	c.connected = false
	fn := c.onDrop
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	if wasConnected {
		logrus.WithError(err).WithField("reason", reason).Warn("MQTT connection lost")
		if fn != nil {
			go fn()
		}
	}
}

// Publish publishes a message to a topic with the default 10s write timeout.
func (c *LightweightMQTTClient) Publish(topic string, qos int, payload []byte) error {
	return c.publish(topic, qos, payload, time.Now().Add(10*time.Second))
}

// PublishCtx publishes a message honouring the context deadline: the write
// timeout is the earlier of the context deadline and 10 seconds. Platform
// handlers must prefer this variant so a stalled broker cannot block the
// EventBus dispatch goroutine longer than the forwarding context allows.
func (c *LightweightMQTTClient) PublishCtx(ctx context.Context, topic string, qos int, payload []byte) error {
	deadline := time.Now().Add(10 * time.Second)
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
	}
	return c.publish(topic, qos, payload, deadline)
}

func (c *LightweightMQTTClient) publish(topic string, qos int, payload []byte, deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected || c.conn == nil {
		return fmt.Errorf("mqtt not connected")
	}

	// Build PUBLISH packet
	// Fixed header: 0x30 (QoS 0) | remaining length
	// Variable header: topic name length (2 bytes) + topic name
	topicBytes := []byte(topic)
	topicLen := len(topicBytes)

	var variableHeader []byte
	variableHeader = append(variableHeader, byte(topicLen>>8), byte(topicLen&0xFF))
	variableHeader = append(variableHeader, topicBytes...)

	// Add packet ID for QoS > 0
	if qos > 0 {
		c.packetID++
		variableHeader = append(variableHeader, byte(c.packetID>>8), byte(c.packetID&0xFF))
	}

	// Combine variable header + payload
	remainingPayload := append(variableHeader, payload...)

	// Build fixed header
	fixedHeader := byte(0x30) // PUBLISH, QoS 0, no retain
	if qos == 1 {
		fixedHeader = 0x32
	} else if qos == 2 {
		fixedHeader = 0x33
	}

	packet := append([]byte{fixedHeader}, encodeRemainingLength(len(remainingPayload))...)
	packet = append(packet, remainingPayload...)

	_ = c.conn.SetWriteDeadline(deadline)
	_, err := c.conn.Write(packet)
	if err != nil {
		// The TCP write failed — treat the connection as lost so the handler
		// flips to disconnected and the manager watchdog can reconnect.
		c.mu.Unlock()
		c.markDisconnected("publish", err)
		c.mu.Lock()
	}
	return err
}

// Subscribe subscribes to a topic.
func (c *LightweightMQTTClient) Subscribe(topic string, qos int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected || c.conn == nil {
		return fmt.Errorf("mqtt not connected")
	}

	c.packetID++
	packetID := c.packetID

	// Build SUBSCRIBE packet
	// Fixed header: 0x82 | remaining length
	topicBytes := []byte(topic)
	var payload []byte
	payload = append(payload, byte(packetID>>8), byte(packetID&0xFF)) // Packet ID
	payload = append(payload, byte(len(topicBytes)>>8), byte(len(topicBytes)&0xFF))
	payload = append(payload, topicBytes...)
	payload = append(payload, byte(qos))

	fixedHeader := byte(0x82) // SUBSCRIBE
	packet := append([]byte{fixedHeader}, encodeRemainingLength(len(payload))...)
	packet = append(packet, payload...)

	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(packet); err != nil {
		return err
	}

	// Start reading loop if not already started (Connect normally starts it)
	if !c.reading {
		c.reading = true
		c.wg.Add(1)
		go c.readLoop()
	}

	return nil
}

// IsConnected returns whether the client is connected.
func (c *LightweightMQTTClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// --- Internal methods ---

// sendConnect sends an MQTT CONNECT packet.
func (c *LightweightMQTTClient) sendConnect() error {
	// Variable header: Protocol Name + Protocol Level + Connect Flags + Keep Alive
	protocolName := []byte{0x00, 0x04, 'M', 'Q', 'T', 'T'}
	protocolLevel := byte(0x04) // MQTT 3.1.1

	// Connect flags
	var flags byte = 0x02 // Clean session
	if c.username != "" {
		flags |= 0x80 // Username flag
	}
	if c.password != "" {
		flags |= 0x40 // Password flag
	}

	keepAlive := make([]byte, 2)
	binary.BigEndian.PutUint16(keepAlive, uint16(c.keepAlive))

	variableHeader := append(protocolName, protocolLevel, flags)
	variableHeader = append(variableHeader, keepAlive...)

	// Payload: Client ID, then optional username and password
	clientIDBytes := []byte(c.clientID)
	payload := encodeMQTTString(clientIDBytes)

	if c.username != "" {
		payload = append(payload, encodeMQTTString([]byte(c.username))...)
	}
	if c.password != "" {
		payload = append(payload, encodeMQTTString([]byte(c.password))...)
	}

	// Combine
	remaining := append(variableHeader, payload...)
	packet := append([]byte{0x10}, encodeRemainingLength(len(remaining))...) // CONNECT
	packet = append(packet, remaining...)

	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(packet)
	return err
}

// readConnack reads the CONNACK packet.
func (c *LightweightMQTTClient) readConnack() error {
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// Read fixed header
	header := make([]byte, 1)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		return err
	}
	if header[0] != 0x20 {
		return fmt.Errorf("expected CONNACK (0x20), got 0x%02X", header[0])
	}
	// Read remaining length (should be 2)
	rl, err := decodeRemainingLength(c.reader)
	if err != nil {
		return err
	}
	if rl != 2 {
		return fmt.Errorf("invalid CONNACK length: %d", rl)
	}
	// Read CONNACK payload
	ack := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, ack); err != nil {
		return err
	}
	// ack[1] is the return code
	if ack[1] != 0 {
		return fmt.Errorf("mqtt connection rejected, code: %d", ack[1])
	}
	return nil
}

// pingLoop sends PINGREQ packets at the keep-alive interval.
func (c *LightweightMQTTClient) pingLoop() {
	defer c.wg.Done()
	if c.keepAlive <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(c.keepAlive) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			if !c.connected || c.conn == nil {
				c.mu.Unlock()
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, err := c.conn.Write([]byte{0xC0, 0x00}) // PINGREQ
			c.mu.Unlock()
			if err != nil {
				logrus.WithError(err).Warn("MQTT PINGREQ failed")
				return
			}
		}
	}
}

// readLoop reads incoming packets from the MQTT broker. A read deadline of
// keepAlive*1.5 is refreshed before each packet header read: if the broker
// (or the TCP path) stays silent past the keep-alive window, the connection
// is considered lost and the drop callback fires.
func (c *LightweightMQTTClient) readLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		c.mu.Lock()
		connected := c.connected
		reader := c.reader
		conn := c.conn
		ka := c.keepAlive
		c.mu.Unlock()
		if !connected || reader == nil {
			return
		}

		if conn != nil && ka > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(ka*3/2) * time.Second))
		}

		// Read packet type
		b, err := reader.ReadByte()
		if err != nil {
			if c.isStopped() {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				c.markDisconnected("keepalive timeout", err)
			} else {
				c.markDisconnected("read", err)
			}
			return
		}
		packetType := b >> 4

		// Read remaining length
		rl, err := decodeRemainingLength(reader)
		if err != nil {
			if c.isStopped() {
				return
			}
			c.markDisconnected("read", err)
			return
		}

		// Read payload
		payload := make([]byte, rl)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if c.isStopped() {
				return
			}
			c.markDisconnected("read", err)
			return
		}

		switch packetType {
		case 3: // PUBLISH
			c.handlePublish(payload)
		case 4: // PUBACK
			// QoS 1 acknowledged
		case 11: // UNSUBACK
			// Subscription removed
		case 13: // PINGRESP
			// PINGREQ acknowledged
		}
	}
}

func (c *LightweightMQTTClient) isStopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// handlePublish processes a PUBLISH packet received from the broker.
func (c *LightweightMQTTClient) handlePublish(payload []byte) {
	if len(payload) < 2 {
		return
	}
	topicLen := int(binary.BigEndian.Uint16(payload[0:2]))
	if len(payload) < 2+topicLen {
		return
	}
	topic := string(payload[2 : 2+topicLen])
	msgPayload := payload[2+topicLen:]

	c.mu.Lock()
	handler := c.onMessage
	c.mu.Unlock()

	if handler != nil {
		handler(topic, msgPayload)
	}
}

// encodeMQTTString encodes a string in MQTT format (length prefix + data).
func encodeMQTTString(s []byte) []byte {
	result := make([]byte, 2+len(s))
	result[0] = byte(len(s) >> 8)
	result[1] = byte(len(s) & 0xFF)
	copy(result[2:], s)
	return result
}

// encodeRemainingLength encodes an MQTT remaining length value.
func encodeRemainingLength(length int) []byte {
	var encoded []byte
	for {
		byteVal := length % 128
		length /= 128
		if length > 0 {
			byteVal |= 128
		}
		encoded = append(encoded, byte(byteVal))
		if length == 0 {
			break
		}
	}
	return encoded
}

// decodeRemainingLength decodes an MQTT remaining length from a reader.
func decodeRemainingLength(r *bufio.Reader) (int, error) {
	multiplier := 1
	value := 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value += int(b&127) * multiplier
		if b&128 == 0 {
			break
		}
		multiplier *= 128
		if multiplier > 128*128*128 {
			return 0, fmt.Errorf("malformed remaining length")
		}
	}
	return value, nil
}

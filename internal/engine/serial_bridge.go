package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"go.bug.st/serial"
)

// relayWriteTimeout bounds a single write towards a TCP client. A client that
// cannot drain the serial stream is dropped instead of stalling the relay for
// every other client.
const relayWriteTimeout = 2 * time.Second

// serialReadTimeout only controls how often the read loop re-checks the
// context; the port itself delivers bytes as they arrive.
const serialReadTimeout = 200 * time.Millisecond

// SerialBridgeStats tracks statistics for a serial-TCP bridge.
// 计数器使用 atomic.Int64/Int32 类型：自带 8 字节对齐保证，避免在 32 位平台
// （linux/arm）上对未对齐字段的 64 位原子操作直接 panic。
type SerialBridgeStats struct {
	mu               sync.Mutex // guards StartedAt / LastClientAt / LastError
	BytesFromSerial  atomic.Int64
	BytesToSerial    atomic.Int64
	ClientCount      atomic.Int32
	TotalConnections atomic.Int64
	Rejected         atomic.Int64
	StartedAt        *time.Time
	LastClientAt     *time.Time
	LastError        string
}

// SerialTcpBridge bridges serial data over TCP in both directions.
// Remote clients connect to the TCP listener and are served the bytes arriving
// on the serial device; everything they write goes to the device.
type SerialTcpBridge struct {
	opener   func(SerialBridgeConfig) (io.ReadWriteCloser, error)
	mu       sync.Mutex
	config   SerialBridgeConfig
	listener net.Listener
	serial   io.ReadWriteCloser
	clients  map[*bridgeClient]struct{}
	allowed  []*net.IPNet
	stats    *SerialBridgeStats
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	started  bool
}

type bridgeClient struct {
	conn net.Conn
	mu   sync.Mutex // guards writes to conn, so concurrent relays never interleave frames
}

// SerialBridgeConfig holds configuration for a serial-TCP bridge.
type SerialBridgeConfig struct {
	SerialPort  string   `json:"serial_port"`
	BaudRate    int      `json:"baud_rate"`
	DataBits    int      `json:"data_bits"`
	Parity      string   `json:"parity"` // "N", "O", "E"
	StopBits    int      `json:"stop_bits"`
	ListenAddr  string   `json:"listen_addr"` // TCP listen address
	MaxClients  int      `json:"max_clients"`
	IPWhitelist []string `json:"ip_whitelist"`
}

// NewSerialTcpBridge creates a new SerialTcpBridge served by the OS serial driver.
func NewSerialTcpBridge() *SerialTcpBridge {
	return NewSerialTcpBridgeWithOpener(openSerialPort)
}

// NewSerialTcpBridgeWithOpener creates a bridge whose device is produced by
// opener. Production passes the OS driver; a caller with no serial hardware to
// serve (the HTTP layer's tests) injects an in-memory device so the
// start/stop/apply-config paths remain testable on any host.
func NewSerialTcpBridgeWithOpener(opener func(SerialBridgeConfig) (io.ReadWriteCloser, error)) *SerialTcpBridge {
	if opener == nil {
		opener = openSerialPort
	}
	return &SerialTcpBridge{
		opener:  opener,
		stats:   &SerialBridgeStats{},
		clients: map[*bridgeClient]struct{}{},
	}
}

func openSerialPort(cfg SerialBridgeConfig) (io.ReadWriteCloser, error) {
	mode, err := serialMode(cfg)
	if err != nil {
		return nil, err
	}
	port, err := serial.Open(cfg.SerialPort, mode)
	if err != nil {
		return nil, err
	}
	if err := port.SetReadTimeout(serialReadTimeout); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("set read timeout on %s: %w", cfg.SerialPort, err)
	}
	return port, nil
}

// serialMode turns the bridge configuration into a line setting. The defaults
// (8N1) fill in only the fields the config leaves blank; a value that is set
// but invalid is an error, because silently reinterpreting it would make the
// gateway talk to the device at a framing the device does not use.
func serialMode(cfg SerialBridgeConfig) (*serial.Mode, error) {
	if cfg.BaudRate <= 0 {
		return nil, fmt.Errorf("serial_bridge.baud_rate is %d: a device is opened at a rate, not without one", cfg.BaudRate)
	}
	dataBits := cfg.DataBits
	if dataBits == 0 {
		dataBits = 8
	}
	switch dataBits {
	case 5, 6, 7, 8:
	default:
		return nil, fmt.Errorf("serial_bridge.data_bits is %d: expected 5, 6, 7 or 8", dataBits)
	}
	var parity serial.Parity
	switch strings.ToLower(strings.TrimSpace(cfg.Parity)) {
	case "", "none", "n":
		parity = serial.NoParity
	case "even", "e":
		parity = serial.EvenParity
	case "odd", "o":
		parity = serial.OddParity
	case "mark", "m":
		parity = serial.MarkParity
	case "space", "s":
		parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("serial_bridge.parity is %q: expected none, even, odd, mark or space", cfg.Parity)
	}
	stopBits := serial.OneStopBit
	if cfg.StopBits != 0 {
		switch cfg.StopBits {
		case 1:
			stopBits = serial.OneStopBit
		case 2:
			stopBits = serial.TwoStopBits
		default:
			return nil, fmt.Errorf("serial_bridge.stop_bits is %d: expected 1 or 2", cfg.StopBits)
		}
	}
	return &serial.Mode{
		BaudRate: cfg.BaudRate,
		DataBits: dataBits,
		Parity:   parity,
		StopBits: stopBits,
	}, nil
}

// ValidateSerialBridgeConfig reports whether a section describes a bridge that
// could actually run, applying the same rules Start uses. The HTTP layer checks
// before writing the config file: persisting a section Start rejects would make
// the file claim settings the service never applies.
func ValidateSerialBridgeConfig(cfg SerialBridgeConfig) error {
	if strings.TrimSpace(cfg.SerialPort) == "" {
		return errors.New("ERR_SVC_SERIAL_PORT_UNAVAILABLE: serial_bridge.serial_port is empty, so there is nothing to bridge (set it to a device such as COM3 or /dev/ttyUSB0)")
	}
	if cfg.ListenAddr == "" || cfg.ListenAddr == ":" {
		return errors.New("ERR_SVC_CONFIG_INVALID: serial_bridge.tcp_port must name a port to serve clients on")
	}
	if cfg.MaxClients < 0 {
		return fmt.Errorf("ERR_SVC_CONFIG_INVALID: serial_bridge.max_clients is %d; use 0 for no limit", cfg.MaxClients)
	}
	if _, err := serialMode(cfg); err != nil {
		return fmt.Errorf("ERR_SVC_CONFIG_INVALID: %w", err)
	}
	if _, err := parseIPWhitelist(cfg.IPWhitelist); err != nil {
		return err
	}
	return nil
}

// Start opens the serial device and serves it on the configured TCP address.
//
// It used to fall back to an io.Pipe loopback whenever the device could not be
// opened, and then report the bridge as running: a client sending bytes got
// those same bytes back and every counter moved, while no serial hardware was
// involved. A port that cannot be opened is now a start failure.
func (b *SerialTcpBridge) Start(ctx context.Context, config SerialBridgeConfig) error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return fmt.Errorf("serial bridge already started")
	}
	b.mu.Unlock()

	if err := ValidateSerialBridgeConfig(config); err != nil {
		return err
	}
	config.SerialPort = strings.TrimSpace(config.SerialPort)
	allowed, err := parseIPWhitelist(config.IPWhitelist)
	if err != nil {
		return err
	}

	serialConn, err := b.opener(config)
	if err != nil {
		return fmt.Errorf("ERR_SVC_SERIAL_PORT_UNAVAILABLE: cannot open serial port %s: %w", config.SerialPort, err)
	}

	listener, err := net.Listen("tcp", config.ListenAddr)
	if err != nil {
		_ = serialConn.Close()
		return fmt.Errorf("failed to listen on %s: %w", config.ListenAddr, err)
	}

	childCtx, cancel := context.WithCancel(ctx)
	now := time.Now()

	b.mu.Lock()
	b.config = config
	b.allowed = allowed
	b.listener = listener
	b.serial = serialConn
	b.cancelFn = cancel
	b.started = true
	b.mu.Unlock()

	b.stats.mu.Lock()
	b.stats.StartedAt = &now
	b.stats.LastError = ""
	b.stats.mu.Unlock()

	b.wg.Add(2)
	go b.acceptLoop(childCtx)
	go b.serialReadLoop(childCtx)

	logrus.WithField("listen_addr", config.ListenAddr).
		WithField("serial_port", config.SerialPort).
		WithField("baud_rate", config.BaudRate).
		Info("Serial TCP bridge started")
	return nil
}

// Stop stops the serial bridge.
func (b *SerialTcpBridge) Stop() error {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = false
	listener := b.listener
	serialConn := b.serial
	clients := make([]*bridgeClient, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	cancel := b.cancelFn
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	for _, c := range clients {
		_ = c.conn.Close()
	}
	if serialConn != nil {
		_ = serialConn.Close()
	}
	b.wg.Wait()
	logrus.Info("Serial TCP bridge stopped")
	return nil
}

// GetStatus returns bridge status.
func (b *SerialTcpBridge) GetStatus() map[string]interface{} {
	b.mu.Lock()
	started := b.started
	cfg := b.config
	b.mu.Unlock()

	b.stats.mu.Lock()
	defer b.stats.mu.Unlock()
	var startedAt interface{}
	if b.stats.StartedAt != nil {
		startedAt = b.stats.StartedAt.Format(time.RFC3339)
	}
	var lastClientAt interface{}
	if b.stats.LastClientAt != nil {
		lastClientAt = b.stats.LastClientAt.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"started":           started,
		"bytes_from_serial": b.stats.BytesFromSerial.Load(),
		"bytes_to_serial":   b.stats.BytesToSerial.Load(),
		"client_count":      b.stats.ClientCount.Load(),
		"total_connections": b.stats.TotalConnections.Load(),
		"rejected":          b.stats.Rejected.Load(),
		"started_at":        startedAt,
		"last_client_at":    lastClientAt,
		"last_error":        b.stats.LastError,
		"serial_port":       cfg.SerialPort,
		"baud_rate":         cfg.BaudRate,
		"data_bits":         cfg.DataBits,
		"parity":            cfg.Parity,
		"stop_bits":         cfg.StopBits,
		"listen_addr":       cfg.ListenAddr,
		"max_clients":       cfg.MaxClients,
		"ip_whitelist":      cfg.IPWhitelist,
	}
}

// noteError records the latest relay fault so the status page can show why a
// running bridge has stopped moving bytes.
func (b *SerialTcpBridge) noteError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	b.stats.mu.Lock()
	b.stats.LastError = msg
	b.stats.mu.Unlock()
	logrus.WithError(err).Warn("Serial bridge fault")
}

// admit applies the client limit and the IP whitelist to an accepted
// connection, and registers it when the bridge may serve it.
func (b *SerialTcpBridge) admit(conn net.Conn) (*bridgeClient, bool) {
	ip, err := clientIP(conn)
	if err != nil {
		b.stats.Rejected.Add(1)
		_ = conn.Close()
		b.noteError(fmt.Errorf("rejected connection with unparseable address %q: %w", conn.RemoteAddr().String(), err))
		return nil, false
	}
	b.mu.Lock()
	allowed := b.allowed
	maxClients := b.config.MaxClients
	started := b.started
	b.mu.Unlock()
	if !started {
		_ = conn.Close()
		return nil, false
	}
	if len(allowed) > 0 && !ipAllowed(allowed, ip) {
		b.stats.Rejected.Add(1)
		_ = conn.Close()
		logrus.WithField("client", ip.String()).Warn("Serial bridge rejected a client outside the IP whitelist")
		return nil, false
	}

	client := &bridgeClient{conn: conn}
	b.mu.Lock()
	// The limit is re-checked under the lock so two connections arriving at the
	// same time cannot both take the last slot.
	if maxClients > 0 && len(b.clients) >= maxClients {
		b.mu.Unlock()
		b.stats.Rejected.Add(1)
		_ = conn.Close()
		logrus.WithField("client", ip.String()).WithField("max_clients", maxClients).
			Warn("Serial bridge rejected a client: connection limit reached")
		return nil, false
	}
	b.clients[client] = struct{}{}
	b.mu.Unlock()

	b.stats.ClientCount.Add(1)
	b.stats.TotalConnections.Add(1)
	now := time.Now()
	b.stats.mu.Lock()
	b.stats.LastClientAt = &now
	b.stats.mu.Unlock()
	return client, true
}

func (b *SerialTcpBridge) removeClient(client *bridgeClient) {
	b.mu.Lock()
	_, ok := b.clients[client]
	if ok {
		delete(b.clients, client)
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	b.stats.ClientCount.Add(-1)
	_ = client.conn.Close()
}

// clientSnapshots returns the registered clients without holding the bridge
// lock across a network write.
func (b *SerialTcpBridge) clientSnapshots() []*bridgeClient {
	b.mu.Lock()
	out := make([]*bridgeClient, 0, len(b.clients))
	for c := range b.clients {
		out = append(out, c)
	}
	b.mu.Unlock()
	return out
}

func (b *SerialTcpBridge) acceptLoop(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := b.listener.Accept()
		if err != nil {
			b.mu.Lock()
			started := b.started
			b.mu.Unlock()
			if !started || ctx.Err() != nil {
				return
			}
			logrus.WithError(err).Error("Serial bridge accept error")
			continue
		}
		client, ok := b.admit(conn)
		if !ok {
			continue
		}
		b.wg.Add(1)
		go b.handleClient(ctx, client)
	}
}

// handleClient relays TCP -> serial for one client. Serial -> TCP is fanned
// out by serialReadLoop, which owns every registered client's write side.
func (b *SerialTcpBridge) handleClient(ctx context.Context, client *bridgeClient) {
	defer b.wg.Done()
	defer b.removeClient(client)

	conn := client.conn
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// A read deadline keeps this loop responsive to Stop() on a connection
		// that goes quiet, which a blocking Read would not.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			b.mu.Lock()
			serialConn := b.serial
			b.mu.Unlock()
			if serialConn != nil {
				if _, werr := serialConn.Write(buf[:n]); werr != nil {
					b.noteError(werr)
					return
				}
				b.stats.BytesToSerial.Add(int64(n))
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if !errors.Is(err, io.EOF) {
				b.noteError(err)
			}
			return
		}
	}
}

// serialReadLoop fans the device's bytes out to every connected client. Before
// this loop existed the bytes were read, counted and thrown away, so a client
// could write to the device but never received its answer.
func (b *SerialTcpBridge) serialReadLoop(ctx context.Context) {
	defer b.wg.Done()
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		b.mu.Lock()
		serialConn := b.serial
		b.mu.Unlock()
		if serialConn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		n, err := serialConn.Read(buf)
		if n > 0 {
			b.stats.BytesFromSerial.Add(int64(n))
			payload := make([]byte, n)
			copy(payload, buf[:n])
			for _, client := range b.clientSnapshots() {
				if _, werr := b.writeToClient(client, payload); werr != nil {
					logrus.WithError(werr).WithField("client", client.conn.RemoteAddr().String()).
						Warn("Serial bridge dropped a client that could not be served")
					b.removeClient(client)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				b.noteError(err)
				return
			}
			b.noteError(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// writeToClient serialises the relay writes for one client and gives up on it
// after relayWriteTimeout instead of blocking the other clients.
func (b *SerialTcpBridge) writeToClient(client *bridgeClient, payload []byte) (int, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	_ = client.conn.SetWriteDeadline(time.Now().Add(relayWriteTimeout))
	return client.conn.Write(payload)
}

// parseIPWhitelist turns the configured entries into matchers. A malformed
// entry is an error: accepting it silently would narrow (or open) the gate the
// operator believes they set.
func parseIPWhitelist(entries []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("ERR_SVC_CONFIG_INVALID: serial_bridge.ip_whitelist entry %q is neither an IP address nor a CIDR range", e)
		}
		out = append(out, network)
	}
	return out, nil
}

func ipAllowed(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func clientIP(conn net.Conn) (net.IP, error) {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("%q is not an IP address", host)
	}
	return ip, nil
}

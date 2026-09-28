package northbound

import (
	"net"
	"sync"
	"testing"
	"time"

	"edgelite/internal/platform"
)

// fakeBroker is a minimal MQTT broker used to exercise the platform
// connection lifecycle: it completes CONNACK handshakes, answers PINGREQ and
// can drop established connections to simulate a broker outage.
type fakeBroker struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake broker listen: %v", err)
	}
	b := &fakeBroker{ln: ln}
	go b.serve()
	return b
}

func (b *fakeBroker) addr() (string, int) {
	return "127.0.0.1", b.ln.Addr().(*net.TCPAddr).Port
}

func (b *fakeBroker) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, conn)
		b.mu.Unlock()
		go b.handle(conn)
	}
}

func (b *fakeBroker) handle(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case 0x10: // CONNECT -> CONNACK (success)
			if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
				return
			}
		case 0xC0: // PINGREQ -> PINGRESP
			if _, err := conn.Write([]byte{0xD0, 0x00}); err != nil {
				return
			}
		case 0xE0: // DISCONNECT
			return
		}
	}
}

// dropConns closes all established client connections (server side), which
// the client must detect via its read loop and report as a lost connection.
func (b *fakeBroker) dropConns() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// TestManagerDropDetectionAndWatchdogReconnect covers the two previously
// missing platform-integration behaviours:
//  1. an unexpectedly closed broker connection must flip the handler to
//     disconnected (read-loop drop detection), and
//  2. the manager watchdog must reconnect the platform automatically while
//     the broker is reachable again.
// It also asserts that an intentional Disconnect is NOT resurrected by the
// watchdog.
func TestManagerDropDetectionAndWatchdogReconnect(t *testing.T) {
	platform.RegisterAll()
	broker := startFakeBroker(t)
	defer broker.ln.Close()

	host, port := broker.addr()
	mgr := NewManager(nil)
	mgr.watchdogInterval = 300 * time.Millisecond
	mgr.Start()
	defer mgr.Stop()

	cfg := map[string]interface{}{
		"type":    "custom_mqtt",
		"broker":  host,
		"port":    float64(port),
		"enabled": true,
	}
	if err := mgr.Connect("testplat", cfg); err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	if !mgr.IsConnected("testplat") {
		t.Fatal("platform should be connected after Connect")
	}

	// 1) Simulate a broker-side drop; drop detection must fire.
	broker.dropConns()
	waitFor(t, 5*time.Second, func() bool { return !mgr.IsConnected("testplat") },
		"drop detection: platform should become disconnected after broker closes the connection")

	// 2) Broker still accepts; the watchdog must reconnect automatically.
	waitFor(t, 5*time.Second, func() bool { return mgr.IsConnected("testplat") },
		"watchdog reconnect: platform should be reconnected automatically")

	// 3) An explicit Disconnect must stay down (no watchdog resurrection).
	if err := mgr.Disconnect("testplat"); err != nil {
		t.Fatalf("explicit disconnect: %v", err)
	}
	time.Sleep(time.Duration(3*mgr.watchdogInterval) + 500*time.Millisecond)
	if mgr.IsConnected("testplat") {
		t.Fatal("watchdog must not resurrect an intentionally disconnected platform")
	}
}

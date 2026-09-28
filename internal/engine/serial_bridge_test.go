package engine

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakeSerial swaps the device for an in-memory pair so the relay itself is
// what the tests measure: one end is handed to the bridge, the test plays the
// device on the other.
type fakeSerial struct {
	bridge  net.Conn
	device  net.Conn
	openErr error
	modes   atomic.Pointer[serial.Mode]
}

func newFakeSerial() *fakeSerial {
	bridge, device := net.Pipe()
	return &fakeSerial{bridge: bridge, device: device}
}

func (f *fakeSerial) open(cfg SerialBridgeConfig) (io.ReadWriteCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	// Recording the mode lets the test assert the configured line settings reach
	// the port instead of stopping at the config file.
	mode, err := serialMode(cfg)
	if err != nil {
		return nil, err
	}
	f.modes.Store(mode)
	return f.bridge, nil
}

// withFakeSerial closes the test device when the test ends.
func withFakeSerial(t *testing.T, f *fakeSerial) {
	t.Helper()
	t.Cleanup(func() { _ = f.device.Close() })
}

// startBridge starts a bridge on a free loopback port and returns it with the
// address clients should dial.
func startBridge(t *testing.T, f *fakeSerial, cfg SerialBridgeConfig) (*SerialTCPBridge, string) {
	t.Helper()
	withFakeSerial(t, f)
	cfg.ListenAddr = "127.0.0.1:0"
	bridge := NewSerialTCPBridgeWithOpener(f.open)
	if err := bridge.Start(t.Context(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Stop() })
	return bridge, bridge.listener.Addr().String()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readWithin(t *testing.T, r io.Reader, n int, within time.Duration) []byte {
	t.Helper()
	if d, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(within))
	}
	buf := make([]byte, n)
	got := 0
	for got < n {
		read, err := r.Read(buf[got:])
		got += read
		if err != nil {
			t.Fatalf("read %d bytes within %s: got %d, %v", n, within, got, err)
		}
	}
	return buf[:got]
}

func expectClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("%s: expected the connection to be refused, but it stayed readable", what)
	}
}

// waitUntil polls a condition the bridge's own goroutines have to reach. A TCP
// dial returns as soon as the kernel queues the connection, so the relay has no
// client registered yet at that moment; writing device bytes before then would
// test the fan-out against an empty client list.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSerialBridgeCarriesBytesBothWaysToEveryClient is the point of the whole
// module: bytes from the device reach every TCP client, and what a client sends
// reaches the device. The previous relay read the device, counted the bytes and
// discarded them, so a client could write but never read an answer.
func TestSerialBridgeCarriesBytesBothWaysToEveryClient(t *testing.T) {
	f := newFakeSerial()
	bridge, addr := startBridge(t, f, SerialBridgeConfig{SerialPort: "COM9", BaudRate: 9600})

	c1, c2 := dial(t, addr), dial(t, addr)
	waitUntil(t, "both clients to be registered", func() bool {
		cc, _ := bridge.GetStatus()["client_count"].(int32)
		return cc == 2
	})

	if _, err := f.device.Write([]byte("485hi")); err != nil {
		t.Fatalf("device write: %v", err)
	}
	for i, c := range []net.Conn{c1, c2} {
		if got := string(readWithin(t, c, 5, 2*time.Second)); got != "485hi" {
			t.Errorf("client %d received %q, want the device bytes", i+1, got)
		}
	}

	if _, err := c1.Write([]byte("request")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if got := string(readWithin(t, f.device, 7, 2*time.Second)); got != "request" {
		t.Errorf("device received %q, want the client bytes", got)
	}

	// The relay counts each chunk as it forwards it, which happens just after the
	// peer's Read has already returned. Reading the counters immediately therefore
	// raced the goroutine and failed about once in a full-package run.
	st := bridge.GetStatus()
	deadline := time.Now().Add(2 * time.Second)
	for {
		from, _ := st["bytes_from_serial"].(int64)
		to, _ := st["bytes_to_serial"].(int64)
		if (from == 5 && to == 7) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
		st = bridge.GetStatus()
	}
	if from, _ := st["bytes_from_serial"].(int64); from != 5 {
		t.Errorf("bytes_from_serial = %v, want 5", st["bytes_from_serial"])
	}
	if to, _ := st["bytes_to_serial"].(int64); to != 7 {
		t.Errorf("bytes_to_serial = %v, want 7", st["bytes_to_serial"])
	}
	if cc, _ := st["client_count"].(int32); cc != 2 {
		t.Errorf("client_count = %v, want 2", st["client_count"])
	}
	if tc, _ := st["total_connections"].(int64); tc != 2 {
		t.Errorf("total_connections = %v, want 2", st["total_connections"])
	}
}

// TestSerialBridgeStartFailsWhenTheDeviceCannotBeOpened: the bridge used to
// fall back to an io.Pipe loopback and report itself running, which made an
// absent COM port look like a working transparent link.
func TestSerialBridgeStartFailsWhenTheDeviceCannotBeOpened(t *testing.T) {
	f := newFakeSerial()
	f.openErr = errors.New("open COM7: The system cannot find the file specified.")
	withFakeSerial(t, f)

	bridge := NewSerialTCPBridgeWithOpener(f.open)
	err := bridge.Start(t.Context(), SerialBridgeConfig{SerialPort: "COM7", BaudRate: 9600, ListenAddr: "127.0.0.1:0"})
	if err == nil {
		_ = bridge.Stop()
		t.Fatal("Start succeeded on a port that cannot be opened; the bridge has to fail instead of relaying itself")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("ERR_SVC_SERIAL_PORT_UNAVAILABLE")) {
		t.Errorf("error = %v, want it to carry the serial port code the page maps", err)
	}
	if st, _ := bridge.GetStatus()["started"].(bool); st {
		t.Error("status reports started after a failed Start")
	}

	empty := NewSerialTCPBridgeWithOpener(f.open)
	if err := empty.Start(t.Context(), SerialBridgeConfig{BaudRate: 9600, ListenAddr: "127.0.0.1:0"}); err == nil {
		_ = empty.Stop()
		t.Error("Start succeeded with no serial port configured")
	} else if !bytes.Contains([]byte(err.Error()), []byte("serial_bridge.serial_port")) {
		t.Errorf("error = %v, want it to name the setting to fix", err)
	}
}

// TestSerialBridgeEnforcesClientLimitAndWhitelist: max_clients and
// ip_whitelist were editable in the config file and read by nothing.
func TestSerialBridgeEnforcesClientLimitAndWhitelist(t *testing.T) {
	f := newFakeSerial()
	_, addr := startBridge(t, f, SerialBridgeConfig{SerialPort: "COM9", BaudRate: 9600, MaxClients: 1})

	first := dial(t, addr)
	second := dial(t, addr)
	expectClosed(t, second, "the second client over max_clients=1")
	_ = first
}

func TestSerialBridgeRejectsClientsOutsideTheWhitelist(t *testing.T) {
	f := newFakeSerial()
	bridge, addr := startBridge(t, f, SerialBridgeConfig{
		SerialPort: "COM9", BaudRate: 9600, IPWhitelist: []string{"10.0.0.0/8"},
	})
	conn := dial(t, addr)
	expectClosed(t, conn, "a loopback client while the whitelist only allows 10/8")
	if rejected, _ := bridge.GetStatus()["rejected"].(int64); rejected != 1 {
		t.Errorf("rejected = %v, want 1 so the operator can see the refusal", bridge.GetStatus()["rejected"])
	}

	f2 := newFakeSerial()
	withFakeSerial(t, f2)
	bad := NewSerialTCPBridge()
	if err := bad.Start(t.Context(), SerialBridgeConfig{SerialPort: "COM9", BaudRate: 9600, ListenAddr: "127.0.0.1:0", IPWhitelist: []string{"not-an-ip"}}); err == nil {
		_ = bad.Stop()
		t.Error("Start accepted a whitelist entry that is neither an IP nor a CIDR")
	}
}

// TestSerialModeAppliesTheConfiguredLineSettings: baud_rate used to be accepted
// by the config and dropped on the floor, because the port was opened with
// os.OpenFile and never configured.
func TestSerialModeAppliesTheConfiguredLineSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     SerialBridgeConfig
		want    *serial.Mode
		wantErr bool
	}{
		{
			name: "8N1 filled in when unset",
			cfg:  SerialBridgeConfig{SerialPort: "COM1", BaudRate: 19200},
			want: &serial.Mode{BaudRate: 19200, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit},
		},
		{
			name: "even parity and two stop bits",
			cfg:  SerialBridgeConfig{SerialPort: "COM1", BaudRate: 9600, DataBits: 7, Parity: "e", StopBits: 2},
			want: &serial.Mode{BaudRate: 9600, DataBits: 7, Parity: serial.EvenParity, StopBits: serial.TwoStopBits},
		},
		{name: "zero baud", cfg: SerialBridgeConfig{SerialPort: "COM1"}, wantErr: true},
		{name: "nine data bits", cfg: SerialBridgeConfig{BaudRate: 9600, DataBits: 9}, wantErr: true},
		{name: "three stop bits", cfg: SerialBridgeConfig{BaudRate: 9600, StopBits: 3}, wantErr: true},
		{name: "unknown parity", cfg: SerialBridgeConfig{BaudRate: 9600, Parity: "z"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serialMode(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("serialMode(%+v) = %+v, want an error", tc.cfg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("serialMode(%+v): %v", tc.cfg, err)
			}
			if *got != *tc.want {
				t.Errorf("serialMode(%+v) = %+v, want %+v", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestSerialBridgeStopReleasesEveryGoroutine keeps Stop() from leaving clients
// or the read loop behind, which a restart would then double-serve.
func TestSerialBridgeStopReleasesEveryGoroutine(t *testing.T) {
	f := newFakeSerial()
	bridge, addr := startBridge(t, f, SerialBridgeConfig{SerialPort: "COM9", BaudRate: 9600})
	conn := dial(t, addr)
	waitUntil(t, "the client to be registered", func() bool {
		cc, _ := bridge.GetStatus()["client_count"].(int32)
		return cc == 1
	})
	if _, err := f.device.Write([]byte("x")); err != nil {
		t.Fatalf("device write: %v", err)
	}
	readWithin(t, conn, 1, 2*time.Second)

	if err := bridge.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := bridge.GetStatus()["started"].(bool); st {
		t.Error("status still reports started after Stop")
	}
	if cc, _ := bridge.GetStatus()["client_count"].(int32); cc != 0 {
		t.Errorf("client_count = %v after Stop, want 0", bridge.GetStatus()["client_count"])
	}
	// The listener has to be gone too: a stopped bridge that keeps accepting
	// would count clients it can never serve.
	late, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = late.Close()
		t.Errorf("dial %s after Stop succeeded, want the port closed", addr)
	}
}

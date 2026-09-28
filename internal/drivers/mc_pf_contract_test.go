package drivers

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// mockMCServer speaks Mitsubishi SLMP 3E binary framing (9-byte header +
// length-prefixed request block; response = 9-byte header + 2-byte little-endian
// completion code + word data) so the driver's corrected read/write framing can
// be verified hermetically, without a live simulator.
type mockMCServer struct {
	ln   net.Listener
	mu   sync.Mutex
	word map[int]uint16 // word device number -> value (D area)
}

func newMockMC(t *testing.T) (*mockMCServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockMCServer{ln: ln, word: map[int]uint16{}}
	s.word[100] = 0x1234
	go s.serve()
	return s, ln.Addr().String()
}

func (s *mockMCServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *mockMCServer) handle(conn net.Conn) {
	defer conn.Close()
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 9)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if header[0] != 0x50 { // expect 3E ASCII/STX subheader 0x50 0x00
			return
		}
		dataLen := int(binary.LittleEndian.Uint16(header[7:9]))
		block := make([]byte, dataLen)
		if _, err := io.ReadFull(conn, block); err != nil {
			return
		}
		if len(block) < 12 {
			return
		}
		cmd := binary.LittleEndian.Uint16(block[2:4])
		startAddr := int(block[6]) | int(block[7])<<8 | int(block[8])<<16
		count := int(binary.LittleEndian.Uint16(block[10:12]))
		if cmd != 0x0401 { // memory-area read only
			return
		}
		body := make([]byte, 2+count*2) // 2-byte completion code (0x0000) + data
		s.mu.Lock()
		for i := 0; i < count; i++ {
			binary.LittleEndian.PutUint16(body[2+i*2:], s.word[startAddr+i])
		}
		s.mu.Unlock()
		resp := make([]byte, 9+len(body))
		resp[0] = 0xD0 // response subheader D0 00
		resp[1] = 0x00
		copy(resp[2:7], header[2:7]) // echo routing fields (driver ignores them)
		binary.LittleEndian.PutUint16(resp[7:9], uint16(len(body)))
		copy(resp[9:], body)
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// TestMCReadAgainstMock is a hermetic contract test: it proves the driver's
// corrected SLMP 3E request/response framing (CPU monitor timer placement, head
// device encoding, and completion code at response bytes [9:11]) round-trips a
// word read to the correct value.
func TestMCReadAgainstMock(t *testing.T) {
	_, addr := newMockMC(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)

	d, err := NewMCDriver("mc-mock", map[string]interface{}{
		"host": host, "port": port, "plc_type": "Q", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{{Name: "D100", Address: "D100", DataType: "uint16"}}
	got, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("bad read: %+v", got)
	}
	if v, ok := got[0].Value.(uint16); !ok || v != 0x1234 {
		t.Fatalf("MC word decode mismatch at D100: %v (%T), want 0x1234", got[0].Value, got[0].Value)
	}
}

// TestMCAddressProtoForgeContract locks in the address convention shared with
// the ProtoForge Mitsubishi simulator: leading letters are the device code and
// the trailing digits are the absolute device number.
func TestMCAddressProtoForgeContract(t *testing.T) {
	cases := []struct {
		addr   string
		device string
		number int
	}{
		{"D100", "D", 100},
		{"d0", "D", 0},
		{"M2048", "M", 2048},
		{"R100", "R", 100},
	}
	for _, c := range cases {
		dev, num, err := parseMCAddress(c.addr)
		if err != nil {
			t.Fatalf("parseMCAddress(%q): %v", c.addr, err)
		}
		if dev != c.device || num != c.number {
			t.Errorf("parseMCAddress(%q)=(%s,%d), want (%s,%d)", c.addr, dev, num, c.device, c.number)
		}
	}
}

// TestMCDriverAgainstProtoForge is a live joint-debugging smoke test: it points
// the real MCDriver at a running ProtoForge Mitsubishi simulator
// (127.0.0.1:5000) and verifies the SLMP 3E read exchange works end-to-end. It
// skips when no simulator is reachable so CI stays green, and it does not assert
// a specific value because the simulator's device contents are volatile.
func TestMCDriverAgainstProtoForge(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:5000", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Mitsubishi simulator not reachable: %v", err)
	}
	conn.Close()

	d, err := NewMCDriver("pf-mc", map[string]interface{}{
		"host": "127.0.0.1", "port": 5000, "plc_type": "Q", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge MC: %v", err)
	}
	defer d.Disconnect()

	// D100 is a stable word slot in the simulator; contents are volatile so we
	// assert only protocol success (no error + good quality).
	pts := []models.PointDef{{Name: "D100", Address: "D100", DataType: "int16"}}
	got, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("bad read: %+v", got)
	}
}

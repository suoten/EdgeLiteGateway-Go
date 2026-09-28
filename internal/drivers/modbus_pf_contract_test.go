package drivers

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// TestParseModbusAddressProtoForgeContract locks in the address convention that
// EdgeLite shares with the ProtoForge simulator (and real PLCs): prefixed and
// bare addresses are absolute 0-based offsets; only the 5/6-digit PLC notation
// carries an implicit area + 1-based origin.
func TestParseModbusAddressProtoForgeContract(t *testing.T) {
	cases := []struct {
		addr    string
		wantReg string
		wantOff int
	}{
		{"HR200", "holding", 200},
		{"hr200", "holding", 200},
		{"4x200", "holding", 200},
		{"IR150", "input", 150},
		{"3x150", "input", 150},
		{"C50", "coil", 50},
		{"0x50", "coil", 50},
		{"DI12", "discrete", 12},
		{"1x12", "discrete", 12},
		{"100", "holding", 100}, // bare = absolute offset
		{"400100", "holding", 99}, // 6-digit PLC: 400001 -> offset 0
		{"300100", "input", 99},
		{"000100", "coil", 99},
		{"100100", "discrete", 99},
		{"40001", "holding", 0}, // 5-digit PLC
		{"30001", "input", 0},
		{"10001", "discrete", 0},
		{"00001", "coil", 0},
	}
	for _, c := range cases {
		reg, off, _, err := parseModbusAddress(c.addr)
		if err != nil {
			t.Fatalf("parseModbusAddress(%q): unexpected error %v", c.addr, err)
		}
		if reg != c.wantReg || off != c.wantOff {
			t.Errorf("parseModbusAddress(%q) = (%s,%d), want (%s,%d)", c.addr, reg, off, c.wantReg, c.wantOff)
		}
	}
}

func TestModbusRegCount(t *testing.T) {
	cases := map[string]int{
		"uint16": 1, "int16": 1, "bool": 1, "": 1,
		"int32": 2, "uint32": 2, "float32": 2, "float": 2,
		"float64": 4, "double": 4,
	}
	for dt, want := range cases {
		if got := modbusRegCount(dt); got != want {
			t.Errorf("modbusRegCount(%q) = %d, want %d", dt, got, want)
		}
	}
}

// mockModbusServer serves holding registers from a 0-based offset map so a
// driver's requested register index can be verified precisely.
type mockModbusServer struct {
	ln      net.Listener
	mu      sync.Mutex
	holding map[int]uint16
	wg      sync.WaitGroup
}

func newMockModbus(t *testing.T) (*mockModbusServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockModbusServer{ln: ln, holding: map[int]uint16{}}
	s.wg.Add(1)
	go s.serve()
	return s, ln.Addr().String()
}

func (s *mockModbusServer) set(offset int, v uint16) {
	s.mu.Lock()
	s.holding[offset] = v
	s.mu.Unlock()
}

func (s *mockModbusServer) setFloat32(offset int, f float32) {
	bits := math.Float32bits(f)
	s.set(offset, uint16(bits>>16))
	s.set(offset+1, uint16(bits&0xFFFF))
}

func (s *mockModbusServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *mockModbusServer) handle(conn net.Conn) {
	header := make([]byte, 7)
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := binary.BigEndian.Uint16(header[4:6])
		pdu := make([]byte, int(length)-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			return
		}
		txid := binary.BigEndian.Uint16(header[0:2])
		unit := header[6]
		if pdu[0] != 0x03 { // only read-holding supported
			continue
		}
		start := int(binary.BigEndian.Uint16(pdu[1:3]))
		qty := int(binary.BigEndian.Uint16(pdu[3:5]))
		s.mu.Lock()
		body := make([]byte, 2*qty)
		for i := 0; i < qty; i++ {
			binary.BigEndian.PutUint16(body[i*2:], s.holding[start+i])
		}
		s.mu.Unlock()
		// PDU: fc + byteCount + data
		resp := append([]byte{0x03, byte(len(body))}, body...)
		// MBAP
		mbap := make([]byte, 7)
		binary.BigEndian.PutUint16(mbap[0:2], txid)
		binary.BigEndian.PutUint16(mbap[2:4], 0)
		binary.BigEndian.PutUint16(mbap[4:6], uint16(len(resp)+1))
		mbap[6] = unit
		if _, err := conn.Write(append(mbap, resp...)); err != nil {
			return
		}
	}
}

// TestModbusTCPReadFloat32AtBareAddress proves that a float32 point declared
// with a bare address reads the correct value from the correct register index,
// exercising both the ProtoForge offset convention and auto register-count.
func TestModbusTCPReadFloat32AtBareAddress(t *testing.T) {
	srv, addr := newMockModbus(t)
	defer srv.ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)
	// Float32 42.5 lives at offset 200 exactly as the ProtoForge simulator serves "HR200".
	srv.setFloat32(200, 42.5)

	d, err := NewModbusTCPDriver("it", map[string]interface{}{
		"host": host, "port": port, "slave_id": 1, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create driver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{{Name: "T", Address: "HR200", DataType: "float32"}}
	got, err := d.(*ModbusTCPDriver).ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 point, got %d", len(got))
	}
	if got[0].Quality != "good" {
		t.Fatalf("quality = %s (value=%v)", got[0].Quality, got[0].Value)
	}
	if v, ok := got[0].Value.(float32); !ok || math.Abs(float64(v)-42.5) > 0.001 {
		t.Fatalf("float32 read mismatch: %v (%T), want 42.5", got[0].Value, got[0].Value)
	}
}

// TestModbusTCPAgainstProtoForge is a live joint-debugging smoke test: it points
// the real driver at a running ProtoForge Modbus TCP simulator (127.0.0.1:5020)
// and verifies the wire protocol works end-to-end (connect, well-formed PDU,
// no Modbus exception, clean float32 decode). It skips when no simulator is
// reachable so it stays out of CI. It intentionally does not assert a specific
// value because the simulator's register contents are volatile.
func TestModbusTCPAgainstProtoForge(t *testing.T) {
	pfLive(t, "127.0.0.1:5020")

	d, err := NewModbusTCPDriver("pf-it", map[string]interface{}{
		"host": "127.0.0.1", "port": 5020, "slave_id": 1, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{{Name: "T", Address: "HR200", DataType: "float32"}}
	got, err := d.(*ModbusTCPDriver).ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("bad read: %+v", got)
	}
	if _, ok := got[0].Value.(float32); !ok {
		t.Fatalf("float32 decode failed against live ProtoForge: %v (%T)", got[0].Value, got[0].Value)
	}
}

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

// These tests pin down the multi-register byte-order handling: a device that
// stores its float32 in DCBA must read back as the value it holds, and must be
// written in DCBA, not in the driver's native ABCD.

func TestNormalizeModbusByteOrder(t *testing.T) {
	ok := map[string]string{
		"":              modbusOrderABCD,
		"ABCD":          modbusOrderABCD,
		" big ":         modbusOrderABCD,
		"BIG_ENDIAN":    modbusOrderABCD,
		"msb":           modbusOrderABCD,
		"DCBA":          modbusOrderDCBA,
		"Little_Endian": modbusOrderDCBA,
		"CDAB":          modbusOrderCDAB,
		"word_swap":     modbusOrderCDAB,
		"BADC":          modbusOrderBADC,
		"ByteSwap":      modbusOrderBADC,
	}
	for raw, want := range ok {
		got, err := normalizeModbusByteOrder(raw)
		if err != nil {
			t.Errorf("normalizeModbusByteOrder(%q): unexpected error %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeModbusByteOrder(%q) = %s, want %s", raw, got, want)
		}
	}
	for _, raw := range []string{"XYZW", "AB", "0", "big endian"} {
		if _, err := normalizeModbusByteOrder(raw); err == nil {
			t.Errorf("normalizeModbusByteOrder(%q) accepted an unknown layout", raw)
		}
	}
	if _, err := NewModbusTCPDriver("bad-order", map[string]interface{}{"host": "127.0.0.1", "port": 1, "byte_order": "DCBA-1"}); err == nil {
		t.Error("NewModbusTCPDriver accepted an invalid byte_order")
	}
}

// TestReorderRegistersLayouts checks the four layouts against a hand-computed
// reference. 42.5f is 0x422A0000, so ABCD is [0x422A, 0x0000] and each other
// layout is a distinct permutation of its four bytes.
func TestReorderRegistersLayouts(t *testing.T) {
	// Guard the hand-written hex below against Go's own encoding of 42.5f.
	if bits := math.Float32bits(42.5); bits != 0x422A0000 {
		t.Fatalf("reference bits for 42.5 are wrong: %#08x", bits)
	}
	abcd := []uint16{0x422A, 0x0000}
	cases := []struct {
		order string
		wire  []uint16
	}{
		{modbusOrderABCD, []uint16{0x422A, 0x0000}},
		{modbusOrderCDAB, []uint16{0x0000, 0x422A}}, // words swapped
		{modbusOrderBADC, []uint16{0x2A42, 0x0000}}, // bytes swapped inside each word
		{modbusOrderDCBA, []uint16{0x0000, 0x2A42}}, // fully reversed
	}
	for _, c := range cases {
		if got := reorderRegisters(c.wire, c.order); !equalU16(got, abcd) {
			t.Errorf("reorderRegisters(%v, %s) = %v, want %v", c.wire, c.order, got, abcd)
		}
		// Every layout is its own inverse, so encoding and decoding share a call.
		if back := reorderRegisters(abcd, c.order); !equalU16(back, c.wire) {
			t.Errorf("reorderRegisters(%v, %s) inverse = %v, want %v", abcd, c.order, back, c.wire)
		}
	}
	// A single register must never be touched: Modbus fixes the byte order inside
	// a word, so swapping there would corrupt 16-bit points.
	if got := reorderRegisters([]uint16{0x1234}, modbusOrderDCBA); !equalU16(got, []uint16{0x1234}) {
		t.Errorf("reorderRegisters single register = %v, want unchanged", got)
	}
	// 64-bit values apply the same rule to each 32-bit group, and a group is
	// never reversed across groups.
	long := []uint16{0x0001, 0x0002, 0x0003, 0x0004}
	if got := reorderRegisters(long, modbusOrderCDAB); !equalU16(got, []uint16{0x0002, 0x0001, 0x0004, 0x0003}) {
		t.Errorf("reorderRegisters(CDAB) 4 words = %04x, want 0002 0001 0004 0003", got)
	}
	if got := reorderRegisters(long, modbusOrderBADC); !equalU16(got, []uint16{0x0100, 0x0200, 0x0300, 0x0400}) {
		t.Errorf("reorderRegisters(BADC) 4 words = %04x, want 0100 0200 0300 0400", got)
	}
	if got := reorderRegisters(long, modbusOrderDCBA); !equalU16(got, []uint16{0x0400, 0x0300, 0x0200, 0x0100}) {
		t.Errorf("reorderRegisters(DCBA) 4 words = %04x, want 0400 0300 0200 0100", got)
	}
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestModbusEncodeRegisters(t *testing.T) {
	cases := []struct {
		name     string
		value    interface{}
		dataType string
		count    int
		want     []uint16
		wantErr  bool
	}{
		{"float32", 42.5, "float32", 2, []uint16{0x422A, 0x0000}, false},
		{"float32 short count", 42.5, "float32", 1, nil, true},
		{"float32 from string", "42.5", "float32", 2, nil, true},
		{"float64", 1.5, "double", 4, []uint16{0x3FF8, 0x0000, 0x0000, 0x0000}, false},
		{"int32 negative", -70000, "int32", 2, []uint16{0xFFFE, 0xEE90}, false},
		{"int32 out of range", 3000000000, "int32", 2, nil, true},
		{"int32 fractional", 1.5, "int32", 2, nil, true},
		{"uint32", 70000, "uint32", 2, []uint16{0x0001, 0x1170}, false},
		{"uint32 negative", -1, "dword", 2, nil, true},
		{"bool true", true, "bool", 1, []uint16{1}, false},
		{"bool false", false, "bool", 1, []uint16{0}, false},
		{"uint16", 1234, "uint16", 1, []uint16{1234}, false},
		{"undeclared type", 1234, "", 1, []uint16{1234}, false},
	}
	for _, c := range cases {
		got, err := modbusEncodeRegisters(c.value, c.dataType, c.count)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: modbusEncodeRegisters(%v, %q, %d) = %v, want error", c.name, c.value, c.dataType, c.count, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: modbusEncodeRegisters: %v", c.name, err)
			continue
		}
		if !equalU16(got, c.want) {
			t.Errorf("%s: modbusEncodeRegisters(%v, %q, %d) = %04x, want %04x", c.name, c.value, c.dataType, c.count, got, c.want)
		}
	}
}

// TestModbusWriteQty covers the register count a write uses. A bare address has
// to expand to the data type's width or a float32 write lands in one register
// while the read takes two.
func TestModbusWriteQty(t *testing.T) {
	cases := []struct {
		qty      int
		dataType string
		want     int
	}{
		{0, "float32", 2},
		{1, "float32", 2},
		{0, "float64", 4},
		{0, "uint16", 1},
		{0, "", 1},
		{4, "uint16", 4}, // an explicit .N suffix still wins
	}
	for _, c := range cases {
		if got := modbusWriteQty(c.qty, c.dataType); got != c.want {
			t.Errorf("modbusWriteQty(%d, %q) = %d, want %d", c.qty, c.dataType, got, c.want)
		}
	}
}

// mockModbusRWSrv stores holding registers verbatim, so a test can assert both
// the bytes the driver put on the wire and the value it reads back.
type mockModbusRWSrv struct {
	ln      net.Listener
	mu      sync.Mutex
	holding map[int]uint16
	wg      sync.WaitGroup
}

func newMockModbusRW(t *testing.T) (*mockModbusRWSrv, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockModbusRWSrv{ln: ln, holding: map[int]uint16{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		ln.Close()
		s.wg.Wait()
	})
	return s, ln.Addr().String()
}

func (s *mockModbusRWSrv) set(offset int, vals ...uint16) {
	s.mu.Lock()
	for i, v := range vals {
		s.holding[offset+i] = v
	}
	s.mu.Unlock()
}

func (s *mockModbusRWSrv) snapshot(offset, qty int) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint16, qty)
	for i := range out {
		out[i] = s.holding[offset+i]
	}
	return out
}

func (s *mockModbusRWSrv) serve() {
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

func (s *mockModbusRWSrv) handle(conn net.Conn) {
	header := make([]byte, 7)
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := binary.BigEndian.Uint16(header[4:6])
		if length < 2 || length-1 > 256 {
			return
		}
		pdu := make([]byte, int(length)-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			return
		}
		txid := binary.BigEndian.Uint16(header[0:2])
		unit := header[6]

		var body []byte
		switch pdu[0] {
		case 0x03: // read holding registers
			start := int(binary.BigEndian.Uint16(pdu[1:3]))
			qty := int(binary.BigEndian.Uint16(pdu[3:5]))
			regs := s.snapshot(start, qty)
			body = make([]byte, 2*qty)
			for i, r := range regs {
				binary.BigEndian.PutUint16(body[i*2:], r)
			}
			body = append([]byte{0x03, byte(len(body))}, body...)
		case 0x06: // write single register
			addr := int(binary.BigEndian.Uint16(pdu[1:3]))
			s.set(addr, binary.BigEndian.Uint16(pdu[3:5]))
			body = append([]byte{}, pdu[:5]...)
		case 0x10: // write multiple registers
			addr := int(binary.BigEndian.Uint16(pdu[1:3]))
			qty := int(binary.BigEndian.Uint16(pdu[3:5]))
			byteCount := int(pdu[5])
			if byteCount != qty*2 || len(pdu) < 6+byteCount {
				return
			}
			vals := make([]uint16, qty)
			for i := range vals {
				vals[i] = binary.BigEndian.Uint16(pdu[6+i*2:])
			}
			s.set(addr, vals...)
			body = []byte{0x10, pdu[1], pdu[2], pdu[3], pdu[4]}
		default:
			continue
		}

		mbap := make([]byte, 7)
		binary.BigEndian.PutUint16(mbap[0:2], txid)
		binary.BigEndian.PutUint16(mbap[2:4], 0)
		binary.BigEndian.PutUint16(mbap[4:6], uint16(len(body)+1))
		mbap[6] = unit
		if _, err := conn.Write(append(mbap, body...)); err != nil {
			return
		}
	}
}

// TestModbusByteOrderWriteReadBackOverTCP is the end-to-end proof of the
// byte_order feature: for every supported layout it writes a float32 setpoint,
// checks the register words the device actually received, and reads the value
// back through the same driver.
func TestModbusByteOrderWriteReadBackOverTCP(t *testing.T) {
	const target = 42.5
	wantWire := map[string][]uint16{
		"ABCD": {0x422A, 0x0000},
		"CDAB": {0x0000, 0x422A},
		"BADC": {0x2A42, 0x0000},
		"DCBA": {0x0000, 0x2A42},
	}

	for _, order := range []string{"ABCD", "CDAB", "BADC", "DCBA"} {
		t.Run(order, func(t *testing.T) {
			srv, addr := newMockModbusRW(t)
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("split host port: %v", err)
			}
			port, err := net.LookupPort("tcp", portStr)
			if err != nil {
				t.Fatalf("lookup port: %v", err)
			}

			d, err := NewModbusTCPDriver("bo-"+order, map[string]interface{}{
				"host": host, "port": port, "slave_id": 1, "timeout": 3, "byte_order": order,
			})
			if err != nil {
				t.Fatalf("create driver with byte_order %s: %v", order, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.Connect(ctx); err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer d.Disconnect()

			if err := d.(*ModbusTCPDriver).WritePointTyped(ctx, "HR200", target, "float32"); err != nil {
				t.Fatalf("WritePointTyped: %v", err)
			}
			if got := srv.snapshot(200, 2); !equalU16(got, wantWire[order]) {
				t.Fatalf("device received %04x, want %04x for order %s", got, wantWire[order], order)
			}

			got, err := d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
				{Name: "T", Address: "HR200", DataType: "float32"},
			})
			if err != nil {
				t.Fatalf("ReadPoints: %v", err)
			}
			if len(got) != 1 || got[0].Quality != "good" {
				t.Fatalf("bad read: %+v", got)
			}
			v, ok := got[0].Value.(float32)
			if !ok || float64(v) != target {
				t.Fatalf("read back %v (%T), want %f", got[0].Value, got[0].Value, target)
			}
		})
	}
}

// TestModbusByteOrderFloat64WriteReadBack repeats the round trip for a value
// that spans four registers, where a word swap and a byte swap differ.
func TestModbusByteOrderFloat64WriteReadBack(t *testing.T) {
	srv, addr := newMockModbusRW(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)

	d, err := NewModbusTCPDriver("bo-f64", map[string]interface{}{
		"host": host, "port": port, "timeout": 3, "byte_order": "CDAB",
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

	const target = 123456.789
	if bits := math.Float64bits(target); bits != 0x40FE240C9FBE76C9 {
		t.Fatalf("reference bits for %v are wrong: %#016x", target, bits)
	}
	if err := d.(*ModbusTCPDriver).WritePointTyped(ctx, "HR300", target, "float64"); err != nil {
		t.Fatalf("WritePointTyped: %v", err)
	}
	// 123456.789 is 0x40FE240C9FBE76C9, so its ABCD words are 40FE 240C 9FBE 76C9.
	// A CDAB device swaps the two words of each 32-bit pair.
	want := []uint16{0x240C, 0x40FE, 0x76C9, 0x9FBE}
	if got := srv.snapshot(300, 4); !equalU16(got, want) {
		t.Fatalf("device received %04x, want %04x", got, want)
	}

	pts, err := d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
		{Name: "D", Address: "HR300", DataType: "float64"},
	})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if v, ok := pts[0].Value.(float64); !ok || v != target {
		t.Fatalf("read back %v (%T), want %v", pts[0].Value, pts[0].Value, target)
	}
}

// TestModbusRTUConstructorAcceptsByteOrder guards the RTU path: it shares the
// normalization with TCP but has no socket to exercise here.
func TestModbusRTUConstructorRejectsBadByteOrder(t *testing.T) {
	if _, err := NewModbusRTUDriver("rtu-bad", map[string]interface{}{
		"serial_port": "COM1", "baud_rate": 9600, "byte_order": "QQQQ",
	}); err == nil {
		t.Fatal("NewModbusRTUDriver accepted an invalid byte_order")
	}
	d, err := NewModbusRTUDriver("rtu-ok", map[string]interface{}{
		"serial_port": "COM1", "baud_rate": 9600, "byte_order": "word_swap",
	})
	if err != nil {
		t.Fatalf("NewModbusRTUDriver: %v", err)
	}
	if got := d.(*ModbusRTUDriver).byteOrder; got != modbusOrderCDAB {
		t.Fatalf("byteOrder = %s, want %s", got, modbusOrderCDAB)
	}
}

package drivers

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// mcPFMockServer speaks the Mitsubishi SLMP Qna-3E *binary* frame exactly as the
// MELSEC protocol specification (and pymcprotocol, the library ProtoForge's own
// MC docs point at) defines it:
//
//	request  = 50 00 | net | pc | io(2) | station | dataLen(2 LE)
//	           + timer(2) | cmd(2) | sub(2) | head device number(3 LE) |
//	             device code(1) | device count(2) | write payload
//	response = D0 00 | net | pc | io(2) | station | dataLen(2 LE)
//	           + completion(2 LE) + data
//
// For iQ-R (wide=true) the head device field is 4 bytes and the code 2 bytes.
// Bit-unit reads pack two points per byte with the first point in the HIGH
// nibble; bits of M/X/Y/S/B are addressed as word n/16, bit n%16.
type mcPFMockServer struct {
	ln    net.Listener
	mu    sync.Mutex
	words map[byte]map[uint32]uint16
	wide  bool
	wg    sync.WaitGroup
}

func newMcPFMock(t *testing.T, wide bool) (*mcPFMockServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mcPFMockServer{ln: ln, words: map[byte]map[uint32]uint16{}, wide: wide}
	for _, code := range []byte{0xA8, 0x90, 0xAF} { // D, M, R
		s.words[code] = map[uint32]uint16{}
	}
	s.wg.Add(1)
	go s.serve()
	return s, ln.Addr().String()
}

func (s *mcPFMockServer) setWord(code byte, word uint32, v uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.words[code]; !ok {
		s.words[code] = map[uint32]uint16{}
	}
	s.words[code][word] = v
}

func (s *mcPFMockServer) word(code byte, word uint32) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.words[code][word]
}

// setFloat32 stores a REAL the way iQ works it: low word first.
func (s *mcPFMockServer) setFloat32(code byte, word uint32, f float32) {
	bits := math.Float32bits(f)
	s.setWord(code, word, uint16(bits&0xFFFF))
	s.setWord(code, word+1, uint16(bits>>16))
}

func (s *mcPFMockServer) setInt32(code byte, word uint32, i int32) {
	s.setWord(code, word, uint16(uint32(i)&0xFFFF))
	s.setWord(code, word+1, uint16(uint32(i)>>16))
}

func (s *mcPFMockServer) close() {
	s.ln.Close()
	s.wg.Wait()
}

func (s *mcPFMockServer) serve() {
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

func (s *mcPFMockServer) handle(conn net.Conn) {
	header := make([]byte, 9)
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if header[0] != 0x50 || header[1] != 0x00 {
			return // only the 3E binary frame is served
		}
		dataLen := int(binary.LittleEndian.Uint16(header[7:9]))
		block := make([]byte, dataLen)
		if _, err := io.ReadFull(conn, block); err != nil {
			return
		}
		cmd := binary.LittleEndian.Uint16(block[2:4])
		sub := binary.LittleEndian.Uint16(block[4:6])
		var addr uint32
		var code byte
		var payloadAt int
		if s.wide {
			if len(block) < 14 {
				return
			}
			addr = binary.LittleEndian.Uint32(block[6:10])
			code = block[10]
			payloadAt = 14
		} else {
			if len(block) < 12 {
				return
			}
			addr = uint32(block[6]) | uint32(block[7])<<8 | uint32(block[8])<<16
			code = block[9]
			payloadAt = 12
		}
		count := int(binary.LittleEndian.Uint16(block[payloadAt-2 : payloadAt]))

		var data []byte
		var completion uint16
		switch {
		case cmd == 0x0401 && sub == 0x0000: // word batch read
			data = make([]byte, count*2)
			for i := 0; i < count; i++ {
				binary.LittleEndian.PutUint16(data[i*2:], s.word(code, addr+uint32(i)))
			}
		case cmd == 0x0401 && sub == 0x0001: // bit batch read
			data = make([]byte, (count+1)/2)
			for i := 0; i < count; i++ {
				if s.bitAt(code, addr+uint32(i)) {
					if i%2 == 0 {
						data[i/2] |= 0x10 // first point of the pair: high nibble
					} else {
						data[i/2] |= 0x01
					}
				}
			}
		case cmd == 0x1401 && sub == 0x0000: // word batch write
			payload := block[payloadAt:]
			for i := 0; i+2 <= len(payload) && i/2 < count; i += 2 {
				s.setWord(code, addr+uint32(i/2), binary.LittleEndian.Uint16(payload[i:]))
			}
		case cmd == 0x1401 && sub == 0x0001: // bit batch write
			payload := block[payloadAt:]
			for i := 0; i < count && i/2 < len(payload); i++ {
				on := false
				if i%2 == 0 {
					on = payload[i/2]&0x10 != 0
				} else {
					on = payload[i/2]&0x01 != 0
				}
				s.setBitAt(code, addr+uint32(i), on)
			}
		default:
			completion = 0x8401 // command not supported
		}
		if _, err := conn.Write(s.frame(header, completion, data)); err != nil {
			return
		}
	}
}

func (s *mcPFMockServer) frame(header []byte, completion uint16, data []byte) []byte {
	body := make([]byte, 2+len(data))
	binary.LittleEndian.PutUint16(body, completion)
	copy(body[2:], data)
	resp := make([]byte, 9+len(body))
	resp[0], resp[1] = 0xD0, 0x00
	copy(resp[2:7], header[2:7])
	binary.LittleEndian.PutUint16(resp[7:9], uint16(len(body)))
	copy(resp[9:], body)
	return resp
}

// bitAt / setBitAt model the real mapping of a bit device number onto the word
// grid, so a bit write and a later word read of the same device agree.
func (s *mcPFMockServer) bitAt(code byte, n uint32) bool {
	w := s.word(code, n/16)
	return w&(1<<(n%16)) != 0
}

func (s *mcPFMockServer) setBitAt(code byte, n uint32, on bool) {
	w := s.word(code, n/16)
	if on {
		w |= 1 << (n % 16)
	} else {
		w &^= 1 << (n % 16)
	}
	s.setWord(code, n/16, w)
}

// TestMcPFFrameLayout locks the request frame the driver must put on the wire.
func TestMcPFFrameLayout(t *testing.T) {
	// Read 1 word from D100 (device code 0xA8).
	got := mcBuildRequest(0x0401, 0x0000, 0xA8, 100, 1, nil)
	want := []byte{
		0x50, 0x00, 0x00, 0xFF, 0xFF, 0x03, 0x00, 0x0C, 0x00, // header, dataLen 12
		0x10, 0x00, // CPU monitor timer
		0x01, 0x04, // command 0x0401 (LE)
		0x00, 0x00, // subcommand word access
		0x64, 0x00, 0x00, // head device number 100 (3 bytes LE)
		0xA8,       // device code D
		0x01, 0x00, // point count
	}
	if len(got) != len(want) || !equalBytes(got, want) {
		t.Fatalf("mcBuildRequest(D100,1) =\n % X\nwant\n % X", got, want)
	}

	// iQ-R widens the device field to 4 + 2 bytes.
	// iQ-R: 4-byte number + 2-byte code, so the data block grows to 14 bytes.
	wide := mcBuildRequestSeries(0x0401, 0x0000, 0xA8, 100, 1, nil, true)
	wantWide := []byte{
		0x50, 0x00, 0x00, 0xFF, 0xFF, 0x03, 0x00, 0x0E, 0x00,
		0x10, 0x00, 0x01, 0x04, 0x00, 0x00,
		0x64, 0x00, 0x00, 0x00, // number 100 (4 bytes LE)
		0xA8, 0x00, // code D (2 bytes LE)
		0x01, 0x00,
	}
	if len(wide) != len(wantWide) || !equalBytes(wide, wantWide) {
		t.Fatalf("iQ-R frame = % X, want % X", wide, wantWide)
	}
}

func equalBytes(a, b []byte) bool {
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

// TestMcPFWordReadsAgainstMock proves the width-per-data-type read: the legacy
// driver always fetched one word, so REAL/DINT points fell through to a raw
// uint16 (wrong type and wrong value).
func TestMcPFWordReadsAgainstMock(t *testing.T) {
	srv, addr := newMcPFMock(t, false)
	defer srv.close()
	srv.setWord(0xA8, 0, 0xBEEF) // D0  uint16
	var neg int16 = -1234
	srv.setWord(0xA8, 2, uint16(neg)) // D2 int16 (two's complement)
	srv.setFloat32(0xA8, 10, 42.5)    // D10 REAL
	srv.setInt32(0xA8, 20, -70000)    // D20 DINT
	srv.setWord(0xA8, 22, 0xD1)
	srv.setWord(0xA8, 23, 0) // D22 UDINT = 209
	bits := math.Float64bits(3.75)
	for i := 0; i < 4; i++ { // D24 LREAL
		srv.setWord(0xA8, uint32(24+i), uint16((bits>>(16*i))&0xFFFF))
	}
	srv.setBitAt(0x90, 5, true) // M5 -> M word 0, bit 5

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)
	d, err := GetRegistry().CreateDriver("mitsubishi_mc", "test-mc", map[string]interface{}{
		"host": host, "port": port, "plc_type": "Q", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{
		{Name: "u", Address: "D0", DataType: "uint16"},
		{Name: "i", Address: "D2", DataType: "int16"},
		{Name: "r", Address: "D10", DataType: "float32"},
		{Name: "di", Address: "D20", DataType: "int32"},
		{Name: "ud", Address: "D22", DataType: "uint32"},
		{Name: "lr", Address: "D24", DataType: "float64"},
		{Name: "b", Address: "M5", DataType: "bool"},
		{Name: "boff", Address: "M6", DataType: "bool"},
		{Name: "mw", Address: "M0", DataType: "uint16"},
	}
	rows, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	got := pfIndexRows(rows)
	for _, pt := range pts {
		row, ok := got[pt.Name]
		if !ok {
			t.Fatalf("point %q missing", pt.Name)
		}
		if row.Quality != "good" {
			t.Errorf("%s (%s %s): quality %q, want good (err path?)", pt.Name, pt.Address, pt.DataType, row.Quality)
		}
	}
	checks := []struct {
		name string
		want interface{}
	}{
		{"u", uint16(0xBEEF)},
		{"i", int16(-1234)},
		{"r", float32(42.5)},
		{"di", int32(-70000)},
		{"ud", uint32(209)},
		{"lr", float64(3.75)},
		{"b", true},
		{"boff", false},
		{"mw", uint16(0x0020)}, // M5 set -> word 0 = 0b100000
	}
	for _, c := range checks {
		row := got[c.name]
		if !valuesEqualMC(row.Value, c.want) {
			t.Errorf("%s = %v (%T), want %v (%T)", c.name, row.Value, row.Value, c.want, c.want)
		}
	}
}

func valuesEqualMC(got, want interface{}) bool {
	switch w := want.(type) {
	case bool:
		v, ok := got.(bool)
		return ok && v == w
	case uint16:
		v, ok := got.(uint16)
		return ok && v == w
	case int16:
		v, ok := got.(int16)
		return ok && v == w
	case int32:
		v, ok := got.(int32)
		return ok && v == w
	case uint32:
		v, ok := got.(uint32)
		return ok && v == w
	case float32:
		v, ok := got.(float32)
		return ok && math.Abs(float64(v-w)) < 1e-6
	case float64:
		v, ok := got.(float64)
		return ok && math.Abs(v-w) < 1e-9
	}
	return false
}

// TestMcPFWriteRoundTripAgainstMock covers the write path width rules: a float
// value must reach the PLC as REAL bits instead of being truncated, an
// out-of-range integer must not lose its high word, and a bit device must be
// written with the bit-unit command without disturbing its word neighbours.
func TestMcPFWriteRoundTripAgainstMock(t *testing.T) {
	srv, addr := newMcPFMock(t, false)
	defer srv.close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)
	d, err := GetRegistry().CreateDriver("mitsubishi_mc", "test-mc-w", map[string]interface{}{
		"host": host, "port": port, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	writes := []struct {
		address  string
		value    interface{}
		dataType string
		want     interface{}
	}{
		{"D100", int16(4242), "int16", int16(4242)},
		{"D102", float32(-12.5), "float32", float32(-12.5)},
		{"D104", float64(7.25), "float32", float32(7.25)}, // fractional REST value -> REAL
		{"D106", int32(-70000), "int32", int32(-70000)},
		{"D110", uint16(60000), "uint16", uint16(60000)},
	}
	for _, w := range writes {
		if err := d.WritePoint(ctx, w.address, w.value); err != nil {
			t.Fatalf("WritePoint(%s, %v): %v", w.address, w.value, err)
		}
		rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: w.address, Address: w.address, DataType: w.dataType}})
		if err != nil {
			t.Fatalf("ReadPoints(%s): %v", w.address, err)
		}
		if rows[0].Quality != "good" {
			t.Fatalf("%s: quality %q", w.address, rows[0].Quality)
		}
		if !valuesEqualMC(rows[0].Value, w.want) {
			t.Fatalf("%s: read back %v (%T), want %v (%T)", w.address, rows[0].Value, rows[0].Value, w.want, w.value)
		}
	}

	// Bit-unit writes on M must toggle exactly one bit of the owning word.
	if err := d.WritePoint(ctx, "M20", true); err != nil {
		t.Fatalf("WritePoint(M20, true): %v", err)
	}
	if err := d.WritePoint(ctx, "M21", false); err != nil {
		t.Fatalf("WritePoint(M21, false): %v", err)
	}
	rows, err := d.ReadPoints(ctx, []models.PointDef{
		{Name: "m19", Address: "M19", DataType: "bool"},
		{Name: "m20", Address: "M20", DataType: "bool"},
		{Name: "m21", Address: "M21", DataType: "bool"},
	})
	if err != nil {
		t.Fatalf("ReadPoints bits: %v", err)
	}
	got := pfIndexRows(rows)
	for name, want := range map[string]bool{"m19": false, "m20": true, "m21": false} {
		if v, ok := got[name].Value.(bool); !ok || v != want {
			t.Errorf("%s = %v (%T), want %v", name, got[name].Value, got[name].Value, want)
		}
	}
	// M20 is bit 4 of M word 1: the write must land there and nowhere else.
	if w := srv.word(0x90, 1); w != 0x0010 {
		t.Errorf("M word 1 = 0x%04X, want 0x0010 (bit write clobbered its neighbours)", w)
	}
}

// TestMcPFWideDeviceAgainstMock checks that plc_type iQ-R (the value the
// ProtoForge integration pushes) selects the 4-byte number + 2-byte code device
// field, and that the driver still round-trips against an iQ-R PLC.
func TestMcPFWideDeviceAgainstMock(t *testing.T) {
	srv, addr := newMcPFMock(t, true)
	defer srv.close()
	srv.setWord(0xA8, 100, 0x1234)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)
	d, err := GetRegistry().CreateDriver("mitsubishi_mc", "test-mc-r", map[string]interface{}{
		"host": host, "port": port, "plc_type": "iQ-R", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: "d100", Address: "D100", DataType: "uint16"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if rows[0].Quality != "good" {
		t.Fatalf("quality = %q, want good (value=%v)", rows[0].Quality, rows[0].Value)
	}
	if v, ok := rows[0].Value.(uint16); !ok || v != 0x1234 {
		t.Fatalf("iQ-R read = %v (%T), want 0x1234", rows[0].Value, rows[0].Value)
	}
	if err := d.WritePoint(ctx, "D100", int16(777)); err != nil {
		t.Fatalf("WritePoint: %v", err)
	}
	if got := srv.word(0xA8, 100); got != 777 {
		t.Fatalf("iQ-R write landed as 0x%04X, want 0x0309", got)
	}
}

// TestMcPFEncoderContract locks the pure encoder used by WritePoint.
func TestMcPFEncoderContract(t *testing.T) {
	cases := []struct {
		value interface{}
		want  []uint16
	}{
		{int16(5), []uint16{5}},
		{float64(5), []uint16{5}},
		{float64(-2.5), []uint16{0x0000, 0xC020}}, // REAL -2.5, low word first
		{float32(1.0), []uint16{0x0000, 0x3F80}},
		{int32(70000), []uint16{0x1170, 0x0001}}, // DINT, low word first
		{true, []uint16{1}},
		{"12.5", []uint16{0x0000, 0x4148}},
		{"abc", []uint16{0}},
	}
	for _, c := range cases {
		got, err := mcEncodeValue(c.value)
		if err != nil {
			t.Fatalf("mcEncodeValue(%v): %v", c.value, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("mcEncodeValue(%v (%T)) = %v, want %v", c.value, c.value, got, c.want)
		}
	}
	counts := map[string]int{
		"int16": 1, "uint16": 1, "bool": 1, "": 1, "string": 1,
		"int32": 2, "uint32": 2, "float32": 2, "float64": 4,
	}
	for dt, want := range counts {
		if got := mcWordCount(dt); got != want {
			t.Errorf("mcWordCount(%q) = %d, want %d", dt, got, want)
		}
	}
}

// TestMcPFDriverAgainstProtoForge drives the real driver through the registry at
// a running ProtoForge Mitsubishi simulator on 127.0.0.1:5000 (device pf-mc
// serves d0=D0/uint16, f10=D10/float32, m0=M0/bool, m12=M12/bool). It skips when
// the simulator is not reachable.
//
// Only quality and decode TYPE are asserted - both because the simulator's
// contents are volatile and because ProtoForge's MC server parses the head
// device field in the non-standard code-then-number order
// (protoforge/protocols/mc/server.py:305-310, 340-342). EdgeLite sends the SLMP
// standard order, so ProtoForge resolves a different (empty) device slot and
// answers with zeros; the frames still complete without an error. The exact
// value and framing are locked hermetically by the mock tests above.
func TestMcPFDriverAgainstProtoForge(t *testing.T) {
	probe, err := net.DialTimeout("tcp", "127.0.0.1:5000", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Mitsubishi MC simulator not reachable: %v", err)
	}
	probe.Close()

	RegisterAll()
	d, err := GetRegistry().CreateDriver("mitsubishi_mc", "test-mc-pf", map[string]interface{}{
		"host": "127.0.0.1", "port": 5000, "plc_type": "Q", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge MC: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{
		{Name: "d0", Address: "D0", DataType: "uint16"},
		{Name: "f10", Address: "D10", DataType: "float32"},
		{Name: "m0", Address: "M0", DataType: "bool"},
	}
	rows, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	got := pfIndexRows(rows)
	wantType := map[string]interface{}{"d0": uint16(0), "f10": float32(0), "m0": false}
	for name, sample := range wantType {
		row, ok := got[name]
		if !ok {
			t.Fatalf("point %q missing", name)
		}
		if row.Quality != "good" {
			t.Errorf("point %q quality = %q, want good", name, row.Quality)
			continue
		}
		if _, ok := asSameType(row.Value, sample); !ok {
			t.Errorf("point %q decoded %T, want %T", name, row.Value, sample)
		}
	}
	if err := d.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

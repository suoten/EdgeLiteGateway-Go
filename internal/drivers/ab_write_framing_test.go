package drivers

// Hermetic coverage for the Allen-Bradley CIP write path.
//
// Three defects are pinned here, all of which a live controller reports as a
// successful write:
//
//   - the write reply was parsed by the read parser, which demanded a 4-byte
//     data-type + element-count section. Write Tag replies legitimately carry no
//     data, so every write failed with "ab cip data too short".
//   - the request put the payload's byte length where CIP keeps the element
//     count, asking a real CPU for two INT elements.
//   - the read reply's value slice was taken four bytes in, which is right for a
//     ControlLogix (type + count + value) but silently drops the low two bytes
//     of every value from a simulator that answers type + value.
//
// A fake controller speaks both framings so the wire layout is asserted rather
// than inferred, and the driver's behaviour no longer depends on a live
// ProtoForge instance.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// fakeController is an in-process EtherNet/IP endpoint that answers
// RegisterSession, Read Tag (0x4C) and Write Tag (0x4D).
type fakeController struct {
	ln      net.Listener
	tagType uint16
	// withCount selects the ControlLogix reply layout; false selects the
	// type-then-value layout ProtoForge's simulator uses.
	withCount bool

	mu     sync.Mutex
	value  []byte   // current tag contents, replaced by each Write Tag
	writes [][]byte // CIP request bytes of every Write Tag received
	done   chan struct{}
}

// current returns the bytes a read of the tag would answer with.
func (f *fakeController) current() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.value...)
}

// received lists the write requests seen so far.
func (f *fakeController) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.writes...)
}

func startFakeController(t *testing.T, tagType uint16, value []byte, withCount bool) *fakeController {
	t.Helper()
	f := &fakeController{tagType: tagType, value: value, withCount: withCount, done: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.ln = ln
	go f.serve(t)
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeController) serve(t *testing.T) {
	defer close(f.done)
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		if err := f.handle(conn); err != nil {
			conn.Close()
			return
		}
		conn.Close()
	}
}

func (f *fakeController) handle(conn net.Conn) error {
	for {
		header := make([]byte, 24)
		if _, err := readFull(conn, header); err != nil {
			return nil // client hung up
		}
		cmd := binary.LittleEndian.Uint16(header[0:2])
		length := int(binary.LittleEndian.Uint16(header[2:4]))
		if length > 1<<20 {
			return fmt.Errorf("frame too large")
		}
		body := make([]byte, length)
		if _, err := readFull(conn, body); err != nil {
			return nil
		}
		switch cmd {
		case 0x0065: // RegisterSession
			payload := make([]byte, 4)
			if err := f.reply(conn, cmd, 0x0000ABCD, payload); err != nil {
				return err
			}
		case 0x006F: // SendRRData carrying a CIP service
			if len(body) < 16+2 {
				return fmt.Errorf("short unconnected request")
			}
			cip := body[16:]
			switch cip[0] {
			case 0x4C: // Read Tag
				resp := []byte{0xCC, 0x00, 0x00, 0x00}
				two := make([]byte, 2)
				binary.LittleEndian.PutUint16(two, f.tagType)
				resp = append(resp, two...) // Symbol type
				if f.withCount {
					cnt := make([]byte, 2)
					binary.LittleEndian.PutUint16(cnt, 1)
					resp = append(resp, cnt...) // Element count
				}
				resp = append(resp, f.current()...)
				if err := f.replyRRData(conn, resp); err != nil {
					return err
				}
			case 0x4D: // Write Tag
				// A controller keeps what it was told, so the next read answers
				// with the value that was written rather than the seed bytes.
				pathEnd := 2 + int(cip[1])*2
				if len(cip) >= pathEnd+4 {
					f.mu.Lock()
					f.writes = append(f.writes, append([]byte(nil), cip...))
					f.value = append([]byte(nil), cip[pathEnd+4:]...)
					f.mu.Unlock()
				} else {
					return fmt.Errorf("malformed write request: %d bytes", len(cip))
				}
				resp := []byte{0xCD, 0x00, 0x00, 0x00} // no data section, like any real CPU
				if err := f.replyRRData(conn, resp); err != nil {
					return err
				}
			default:
				resp := []byte{cip[0] | 0x80, 0x00, 0x08, 0x00} // service unsupported
				if err := f.replyRRData(conn, resp); err != nil {
					return err
				}
			}
		case 0x0066: // UnregisterSession
			return nil
		default:
			return fmt.Errorf("unexpected command 0x%04x", cmd)
		}
	}
}

func (f *fakeController) reply(conn net.Conn, cmd uint16, session uint32, payload []byte) error {
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint16(hdr[0:2], cmd)
	binary.LittleEndian.PutUint16(hdr[2:4], uint16(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], session)
	binary.LittleEndian.PutUint32(hdr[8:12], 0) // status
	_, err := conn.Write(append(hdr, payload...))
	return err
}

func (f *fakeController) replyRRData(conn net.Conn, cip []byte) error {
	prefix := make([]byte, 16)
	binary.LittleEndian.PutUint16(prefix[6:8], 2)      // item count
	binary.LittleEndian.PutUint16(prefix[12:14], 0xB2) // unconnected data item
	binary.LittleEndian.PutUint16(prefix[14:16], uint16(len(cip)))
	return f.reply(conn, 0x006F, 0x0000ABCD, append(prefix, cip...))
}

func connectFake(t *testing.T, f *fakeController) *ABDriver {
	t.Helper()
	host, port, err := net.SplitHostPort(f.ln.Addr().String())
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	var portNum int
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		t.Fatalf("port: %v", err)
	}
	d, err := NewABDriver("fake-ab", map[string]interface{}{
		"host": host, "port": portNum, "plc_type": "ControlLogix", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = d.Disconnect() })
	return d.(*ABDriver)
}

// TestABWriteAcceptsDatalessReply and sizes the payload for the tag's type.
func TestABWriteAcceptsDatalessReply(t *testing.T) {
	real31337 := make([]byte, 4)
	binary.LittleEndian.PutUint32(real31337, math.Float32bits(31337))
	f := startFakeController(t, CipDataTypeReal, real31337, true)
	d := connectFake(t, f)
	ctx := context.Background()

	// The tag is REAL, so the driver must write four bytes even though the Go
	// value would have fitted an int16.
	if err := d.WritePoint(ctx, "Setpoint", 31337); err != nil {
		t.Fatalf("WritePoint: %v (a data-less write reply must be accepted)", err)
	}
	if got := f.received(); len(got) != 1 {
		t.Fatalf("controller received %d write requests, want exactly 1", len(f.writes))
	}
	cip := f.received()[0]
	if cip[0] != 0x4D {
		t.Fatalf("service byte 0x%02x, want 0x4D", cip[0])
	}
	pathEnd := 2 + int(cip[1])*2
	if len(cip) < pathEnd+6 {
		t.Fatalf("write request too short: % x", cip)
	}
	if got := binary.LittleEndian.Uint16(cip[pathEnd : pathEnd+2]); got != CipDataTypeReal {
		t.Fatalf("request carries type 0x%04x, want 0x%04x (REAL, from the tag not the Go value)", got, CipDataTypeReal)
	}
	if got := binary.LittleEndian.Uint16(cip[pathEnd+2 : pathEnd+4]); got != 1 {
		t.Fatalf("request carries element count %d, want 1 -- the byte length of the payload is not an element count", got)
	}
	payload := cip[pathEnd+4:]
	if !bytes.Equal(payload, real31337) {
		t.Fatalf("payload % x, want the 4 float32 bytes % x", payload, real31337)
	}
}

// TestABReadDecodesBothReplyFramings covers the ControlLogix layout
// (type+count+value) and the simulator layout (type+value).
func TestABReadDecodesBothReplyFramings(t *testing.T) {
	bits := make([]byte, 4)
	binary.LittleEndian.PutUint32(bits, math.Float32bits(21.5))
	for _, withCount := range []bool{true, false} {
		name := "logix(type+count+value)"
		if !withCount {
			name = "simulator(type+value)"
		}
		t.Run(name, func(t *testing.T) {
			f := startFakeController(t, CipDataTypeReal, bits, withCount)
			d := connectFake(t, f)
			rows, err := d.ReadPoints(context.Background(),
				[]models.PointDef{{Name: "T", Address: "Setpoint", DataType: "auto"}})
			if err != nil {
				t.Fatalf("ReadPoints: %v", err)
			}
			if rows[0].Quality != "good" {
				t.Fatalf("quality %s, want good", rows[0].Quality)
			}
			if v, ok := rows[0].Value.(float32); !ok || v != 21.5 {
				t.Fatalf("decoded %v (%T), want float32 21.5", rows[0].Value, rows[0].Value)
			}
		})
	}
}

// TestABReadPrefersTheTagTypeOverTheDeclaredPoint: the configured data type used
// to win, so a REAL tag read through an int16 point returned the low two bytes of
// the float's bit pattern -- 40000.0 came back as 16384 with quality "good", and
// the write→read-back round trip disagreed with the controller.
func TestABReadPrefersTheTagTypeOverTheDeclaredPoint(t *testing.T) {
	bits := make([]byte, 4)
	binary.LittleEndian.PutUint32(bits, math.Float32bits(40000))
	f := startFakeController(t, CipDataTypeReal, bits, false)
	d := connectFake(t, f)

	rows, err := d.ReadPoints(context.Background(),
		[]models.PointDef{{Name: "T", Address: "Temperature", DataType: "int16"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if rows[0].Quality != "good" {
		t.Fatalf("quality %s, want good", rows[0].Quality)
	}
	v, ok := rows[0].Value.(float32)
	if !ok || v != 40000 {
		t.Fatalf("decoded %v (%T), want float32 40000 -- an int16 decode of a REAL gives 16384", rows[0].Value, rows[0].Value)
	}

	// The declared type still decides when the controller names no type at all.
	nf := startFakeController(t, 0, bits, false)
	nd := connectFake(t, nf)
	rows, err = nd.ReadPoints(context.Background(),
		[]models.PointDef{{Name: "T", Address: "Temperature", DataType: "uint32"}})
	if err != nil {
		t.Fatalf("ReadPoints with an untyped tag: %v", err)
	}
	if got, ok := rows[0].Value.(uint32); !ok || got != math.Float32bits(40000) {
		t.Fatalf("untyped tag decoded %v (%T), want the declared uint32 layout", rows[0].Value, rows[0].Value)
	}
}

// TestABWriteRejectsValuesThatDoNotFitTheTag: wrapping is what put a wrong
// setpoint on a machine while the UI said the write succeeded.
func TestABWriteRejectsValuesThatDoNotFitTheTag(t *testing.T) {
	cases := []struct {
		name    string
		tagType uint16
		value   interface{}
	}{
		{"int overflows INT", CipDataTypeInt, 70000},
		{"negative into UINT", CipDataTypeUInt, -1},
		{"fraction into DINT", CipDataTypeDInt, 2.5},
		{"string into REAL", CipDataTypeReal, "hot"},
		{"bool into DINT", CipDataTypeDInt, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startFakeController(t, c.tagType, []byte{0x00, 0x00, 0x00, 0x00}, true)
			d := connectFake(t, f)
			err := d.WritePoint(context.Background(), "Setpoint", c.value)
			if err == nil {
				t.Fatalf("write of %#v to a 0x%04x tag reported success", c.value, c.tagType)
			}
			if got := f.received(); len(got) != 0 {
				t.Fatalf("driver sent %d write frames after refusing: %v", len(got), got)
			}
		})
	}
}

func TestEncodeCipValueWidths(t *testing.T) {
	real31337 := make([]byte, 4)
	binary.LittleEndian.PutUint32(real31337, math.Float32bits(31337))
	cases := []struct {
		cipType uint16
		value   interface{}
		want    []byte
	}{
		{CipDataTypeInt, 31337, []byte{0x69, 0x7A}},
		{CipDataTypeInt, -2, []byte{0xFE, 0xFF}},
		{CipDataTypeDInt, 70000, []byte{0x70, 0x11, 0x01, 0x00}},
		{CipDataTypeReal, 31337, real31337},
		{CipDataTypeBool, true, []byte{0x01, 0x00}},
		{CipDataTypeSInt, 200, nil}, // out of range for a signed 8-bit tag
		{CipDataTypeUSInt, 200, []byte{0xC8, 0x00}},
		{CipDataTypeString, "abc", []byte("abc")},
	}
	for _, c := range cases {
		got, err := encodeCipValue(c.cipType, c.value)
		if c.want == nil {
			if err == nil {
				t.Errorf("type 0x%04x value %#v: expected a refusal, got % x", c.cipType, c.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("type 0x%04x value %#v: %v", c.cipType, c.value, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("type 0x%04x value %#v encoded % x, want % x", c.cipType, c.value, got, c.want)
		}
	}
}

func TestSplitCipReadValueLengths(t *testing.T) {
	value := []byte{0x11, 0x22, 0x33, 0x44}
	// The type word is consumed by the caller; splitCipReadValue sees what follows it.
	logix := append([]byte{0x01, 0x00}, value...) // count(2) + value
	sim := append([]byte{}, value...)             // value only
	if got := splitCipReadValue(CipDataTypeReal, logix); !bytes.Equal(got, value) {
		t.Errorf("logix REAL split % x, want % x", got, value)
	}
	if got := splitCipReadValue(CipDataTypeReal, sim); !bytes.Equal(got, value) {
		t.Errorf("simulator REAL split % x, want % x", got, value)
	}
	// Strings are matched by their own length field, not by an offset guess.
	s := []byte("hi")
	pfString := append([]byte{0x02, 0x00}, s...)                // length(2) + chars
	logixString := append([]byte{0x01, 0x00, 0x02, 0x00}, s...) // count(2) + length(2) + chars
	if got := splitCipReadValue(CipDataTypeString, pfString); !bytes.Equal(got, s) {
		t.Errorf("simulator STRING split % q, want % q", got, s)
	}
	if got := splitCipReadValue(CipDataTypeString, logixString); !bytes.Equal(got, s) {
		t.Errorf("logix STRING split % q, want % q", got, s)
	}
}

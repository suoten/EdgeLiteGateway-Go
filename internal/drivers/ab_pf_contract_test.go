package drivers

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"edgelite/internal/models"
)

// TestAnsiTagPath is a hermetic unit test for the CIP ANSI Extended Symbol path
// encoder used by every Allen-Bradley Read/Write Tag request. It locks in the
// wire layout the ProtoForge Rockwell simulator (and real ControlLogix CPUs)
// expect: 0x91, 8-bit name length, then the name padded to a word-aligned byte
// count. A path carrying a bad array index (e.g. "Tag[x]") is treated verbatim
// as the symbol name rather than producing a malformed path.
func TestAnsiTagPath(t *testing.T) {
	cases := []struct {
		tag  string
		want []byte
	}{
		// Even-length name: header + name, no padding needed.
		{"Tag1", []byte{0x91, 0x04, 'T', 'a', 'g', '1'}},
		// Odd-length name (the live smoke tag) is padded to keep the segment
		// word aligned: 11 chars + 0x91 + length + 1 pad byte = 14 bytes.
		{"Temperature", []byte{0x91, 0x0B, 'T', 'e', 'm', 'p', 'e', 'r', 'a', 't', 'u', 'r', 'e', 0x00}},
		// A bracket with a non-numeric contents is not a valid element index,
		// so the whole string is encoded as the symbol name (padded to even).
		{"Tag[x]", []byte{0x91, 0x06, 'T', 'a', 'g', '[', 'x', ']'}},
	}
	for _, c := range cases {
		got := ansiTagPath(c.tag)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ansiTagPath(%q) = % x, want % x", c.tag, got, c.want)
		}
		// A symbolic path must be an even number of bytes so the "path size in
		// words" byte carried in the CIP service header is exact.
		if len(got)%2 != 0 {
			t.Errorf("ansiTagPath(%q) length %d is not word aligned", c.tag, len(got))
		}
	}
}

// TestABDriverReadPointsAgainstProtoForge is a live joint-debugging smoke test:
// it points the real Allen-Bradley EtherNet/IP (CIP) driver at a running
// ProtoForge Rockwell simulator (127.0.0.1:44818) and verifies the full wire
// path end-to-end: TCP dial, RegisterSession handshake, SendRRData + CIP Read
// Tag round trip, and a clean decode. It skips when no simulator is reachable
// so it stays out of CI. It does not assert a specific value because the
// simulator's tag contents are volatile; it only requires the read to succeed
// (err == nil and quality "good").
func TestABDriverReadPointsAgainstProtoForge(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:44818", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Allen-Bradley simulator not reachable: %v", err)
	}
	conn.Close()

	d, err := NewABDriver("pf-ab", map[string]interface{}{
		"host":     "127.0.0.1",
		"port":     44818,
		"plc_type": "ControlLogix",
		"timeout":  5,
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

	// "Temperature" is a symbolic tag the ProtoForge Rockwell simulator serves.
	// It is decoded as int16: the simulator reports CIP type REAL but returns a
	// short payload, and the first two bytes always decode cleanly, so the
	// assertion measures protocol health rather than a specific value.
	pts := []models.PointDef{{Name: "T", Address: "Temperature", DataType: "int16"}}
	got, err := d.(*ABDriver).ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 point, got %d", len(got))
	}
	if got[0].Quality != "good" {
		t.Fatalf("live read failed: quality=%s value=%v", got[0].Quality, got[0].Value)
	}
}

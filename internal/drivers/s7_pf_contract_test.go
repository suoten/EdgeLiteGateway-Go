package drivers

import (
	"context"
	"net"
	"testing"
	"time"

	"edgelite/internal/models"
)

// TestS7AddressProtoForgeContract is a hermetic contract test for the address
// convention EdgeLite shares with the ProtoForge Siemens S7 simulator. It locks
// in the (area, db, type, offset) mapping without touching the wire.
func TestS7AddressProtoForgeContract(t *testing.T) {
	cases := []struct {
		addr     string
		wantArea int
		wantDB   int
		wantType byte
		wantOff  int
	}{
		{"DB1.DBD0", 0x84, 1, 'D', 0},
		{"DB1.DBW10", 0x84, 1, 'W', 10},
		{"DB2.DBB4", 0x84, 2, 'B', 4},
		{"MW2", 0x83, 0, 'W', 2},
		{"MD4", 0x83, 0, 'D', 4},
		{"QW0", 0x82, 0, 'W', 0},
		{"IW0", 0x81, 0, 'W', 0},
	}
	for _, c := range cases {
		area, db, dt, off, _, err := parseS7Address(c.addr)
		if err != nil {
			t.Fatalf("parseS7Address(%q): %v", c.addr, err)
		}
		if area != c.wantArea || db != c.wantDB || dt != c.wantType || off != c.wantOff {
			t.Errorf("parseS7Address(%q)=(area %x, db %d, type %c, off %d), want (area %x, db %d, type %c, off %d)",
				c.addr, area, db, dt, off, c.wantArea, c.wantDB, c.wantType, c.wantOff)
		}
	}
}

// TestS7DriverAgainstProtoForge is a live joint-debugging smoke test: it points
// the real S7Driver at a running ProtoForge Siemens S7 simulator
// (127.0.0.1:102) and verifies the full ISO-on-TCP (COTP CR/CC) + S7 setup +
// Read Var wire exchange works end-to-end. It skips when no simulator is
// reachable so CI stays green, and it does not assert a specific value because
// the simulator's data block contents are volatile.
func TestS7DriverAgainstProtoForge(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:102", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Siemens S7 simulator not reachable: %v", err)
	}
	conn.Close()

	d, err := NewS7Driver("pf-s7", map[string]interface{}{
		"host": "127.0.0.1", "port": 102, "rack": 0, "slot": 2, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge S7: %v", err)
	}
	defer d.Disconnect()

	// DB1.DBD0 is a stable, always-served dword slot in the S7 simulator; the
	// read exercises the negotiated PDU framing. Contents are volatile, so we
	// assert only protocol success (no error + good quality + decoded float32).
	pts := []models.PointDef{{Name: "DBD0", Address: "DB1.DBD0", DataType: "float32"}}
	got, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 point, got %d", len(got))
	}
	if got[0].Quality != "good" {
		t.Fatalf("quality = %s (value=%v)", got[0].Quality, got[0].Value)
	}
	if _, ok := got[0].Value.(float32); !ok {
		t.Fatalf("float32 decode failed against live ProtoForge S7: %v (%T)", got[0].Value, got[0].Value)
	}
}

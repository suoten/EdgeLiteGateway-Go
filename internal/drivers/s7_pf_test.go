package drivers

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// pfS7Addr pairs a ProtoForge point declaration with the address the EdgeLite
// integration pushes for it: “DB1.DBD0“ -> “DB1.D0“ and
// “DB1.DBX6.0“ -> “DB1.X6.0“ (the DB<t> token loses its leading B), while an
// address already in EdgeLite form (“M10.0“) is passed through untouched.
// See ProtoForge/protoforge/integrations/edgelite.py _translate_point_address.
func TestS7PFAddressTranslationContract(t *testing.T) {
	cases := []struct {
		addr     string
		wantArea int
		wantDB   int
		wantType byte
		wantOff  int
		wantBit  int
	}{
		{"DB1.D0", 0x84, 1, 'D', 0, 0},     // DB1.DBD0  (REAL)
		{"DB1.W4", 0x84, 1, 'W', 4, 0},     // DB1.DBW4  (INT)
		{"DB1.X6.0", 0x84, 1, 'X', 6, 0},   // DB1.DBX6.0 (BOOL)
		{"DB1.B5", 0x84, 1, 'B', 5, 0},     // DB1.DBB5  (BYTE)
		{"DB2.X16.3", 0x84, 2, 'X', 16, 3}, // DB2.DBX16.3
		{"M10.0", 0x83, 0, 'X', 10, 0},     // passthrough
		{"MW20", 0x83, 0, 'W', 20, 0},
		{"Q0.1", 0x82, 0, 'X', 0, 1},
		{"IW0", 0x81, 0, 'W', 0, 0},
		// The untranslated Siemens form must keep working too: the driver is also
		// configured by hand with DB1.DBD0-style addresses.
		{"DB1.DBD0", 0x84, 1, 'D', 0, 0},
		{"DB1.DBX6.1", 0x84, 1, 'X', 6, 1},
	}
	for _, c := range cases {
		area, db, dt, off, bit, err := parseS7Address(c.addr)
		if err != nil {
			t.Fatalf("parseS7Address(%q): unexpected error %v", c.addr, err)
		}
		if area != c.wantArea || db != c.wantDB || dt != c.wantType || off != c.wantOff || bit != c.wantBit {
			t.Errorf("parseS7Address(%q) = (area 0x%02X, db %d, type %c, off %d, bit %d), want (0x%02X, %d, %c, %d, %d)",
				c.addr, area, db, dt, off, bit, c.wantArea, c.wantDB, c.wantType, c.wantOff, c.wantBit)
		}
	}
}

// TestS7PFWriteEncoders locks the write encoders that the live round trip below
// depends on: DB1.D<offset> is 4 bytes wide, so a float32 (and any fractional
// value, which is what the REST write path delivers) must go on the wire as
// IEEE-754 bits instead of being truncated to an integer.
func TestS7PFWriteEncoders(t *testing.T) {
	if got := s7DWordBits(float32(12.5)); got != math.Float32bits(12.5) {
		t.Errorf("s7DWordBits(float32(12.5)) = 0x%08X, want 0x%08X", got, math.Float32bits(12.5))
	}
	if got := s7DWordBits(float64(-3.5)); got != math.Float32bits(float32(-3.5)) {
		t.Errorf("s7DWordBits(float64(-3.5)) = 0x%08X, want 0x%08X", got, math.Float32bits(float32(-3.5)))
	}
	if got := s7DWordBits(float64(1000)); got != 1000 {
		t.Errorf("s7DWordBits(integral float64(1000)) = %d, want 1000 (DINT must stay integer)", got)
	}
	if got := s7DWordBits(int32(-2)); got != 0xFFFFFFFE {
		t.Errorf("s7DWordBits(int32(-2)) = 0x%08X, want 0xFFFFFFFE", got)
	}
}

// pfIndexRows indexes ReadPoints results by point name.
func pfIndexRows(rows []storage.PointData) map[string]storage.PointData {
	m := make(map[string]storage.PointData, len(rows))
	for _, r := range rows {
		m[r.PointName] = r
	}
	return m
}

// TestS7PFDriverLive drives the real S7Driver through the registry against a
// running ProtoForge Siemens S7 simulator on 127.0.0.1:102 (the device pf-s7
// serves temp=DB1.DBD0/float32, word1=DB1.DBW4/int16, bit1=DB1.DBX6.0/bool and
// bit2=M10.0/bool for rack 0 / slot 1). It skips when the simulator is not
// reachable.
//
// The four declared points are generator-backed, so only the decode TYPE and the
// quality are asserted - never the value. The write round trip below uses
// undeclared offsets, which no simulator task touches, so there the exact value
// is asserted: that is what proves the REAL (float32) write encoding.
func TestS7PFDriverLive(t *testing.T) {
	probe, err := net.DialTimeout("tcp", "127.0.0.1:102", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Siemens S7 simulator not reachable: %v", err)
	}
	probe.Close()

	RegisterAll()
	// "ip" is the key the ProtoForge integration pushes for S7 devices.
	d, err := GetRegistry().CreateDriver("siemens_s7", "test-s7", map[string]interface{}{
		"ip": "127.0.0.1", "port": 102, "rack": 0, "slot": 1, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge S7: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{
		{Name: "temp", Address: "DB1.D0", DataType: "float32"},
		{Name: "word1", Address: "DB1.W4", DataType: "int16"},
		{Name: "word1u", Address: "DB1.W4", DataType: "uint16"},
		{Name: "byte5", Address: "DB1.B5", DataType: "uint16"},
		{Name: "bit1", Address: "DB1.X6.0", DataType: "bool"},
		{Name: "bit2", Address: "M10.0", DataType: "bool"},
		{Name: "dword8", Address: "DB1.D8", DataType: "int32"},
	}
	rows, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	got := pfIndexRows(rows)
	wantType := map[string]interface{}{
		"temp":   float32(0),
		"word1":  int16(0),
		"word1u": uint16(0),
		"byte5":  byte(0),
		"bit1":   false,
		"bit2":   false,
		"dword8": int32(0),
	}
	for name, sample := range wantType {
		row, ok := got[name]
		if !ok {
			t.Fatalf("point %q missing from ReadPoints result", name)
		}
		if row.Quality != "good" {
			t.Errorf("point %q quality = %q, want good (value=%v)", name, row.Quality, row.Value)
			continue
		}
		if row.Value == nil {
			t.Errorf("point %q decoded nil", name)
			continue
		}
		if _, ok := asSameType(row.Value, sample); !ok {
			t.Errorf("point %q decoded %T, want %T (value=%v)", name, row.Value, sample, row.Value)
		}
	}

	// --- write -> read round trip on offsets no ProtoForge point owns ---
	roundTrip := []struct {
		name     string
		address  string
		dataType string
		write    interface{}
	}{
		{"real", "DB1.D100", "float32", float32(42.5)},
		{"int", "DB1.W104", "int16", int16(-1234)},
		{"dint", "DB1.D106", "int32", int32(-70000)},
		{"bool", "DB1.X110.3", "bool", true},
		{"merker", "MW112", "int16", int16(4242)},
	}
	for _, rt := range roundTrip {
		if err := d.WritePoint(ctx, rt.address, rt.write); err != nil {
			t.Fatalf("WritePoint(%s, %v): %v", rt.address, rt.write, err)
		}
		rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: rt.name, Address: rt.address, DataType: rt.dataType}})
		if err != nil {
			t.Fatalf("ReadPoints(%s): %v", rt.address, err)
		}
		row := rows[0]
		if row.Quality != "good" {
			t.Fatalf("%s: quality = %q, want good", rt.address, row.Quality)
		}
		if !valuesEqual(row.Value, rt.write) {
			t.Fatalf("%s: read back %v (%T), want %v (%T)", rt.address, row.Value, row.Value, rt.write, rt.write)
		}
	}

	// A BOOL write must only touch its own bit.
	if err := d.WritePoint(ctx, "DB1.X110.4", true); err != nil {
		t.Fatalf("WritePoint(DB1.X110.4, true): %v", err)
	}
	rows, err = d.ReadPoints(ctx, []models.PointDef{
		{Name: "b3", Address: "DB1.X110.3", DataType: "bool"},
		{Name: "b4", Address: "DB1.X110.4", DataType: "bool"},
		{Name: "b5", Address: "DB1.X110.5", DataType: "bool"},
	})
	if err != nil {
		t.Fatalf("ReadPoints bits: %v", err)
	}
	bits := pfIndexRows(rows)
	for name, want := range map[string]bool{"b3": true, "b4": true, "b5": false} {
		v, ok := bits[name].Value.(bool)
		if !ok || v != want {
			t.Errorf("%s = %v (%T), want %v (bit write clobbered its neighbours)", name, bits[name].Value, bits[name].Value, want)
		}
	}

	if err := d.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

// asSameType reports whether v has the same dynamic type as sample.
func asSameType(v, sample interface{}) (interface{}, bool) {
	switch sample.(type) {
	case float32:
		x, ok := v.(float32)
		return x, ok
	case float64:
		x, ok := v.(float64)
		return x, ok
	case int16:
		x, ok := v.(int16)
		return x, ok
	case uint16:
		x, ok := v.(uint16)
		return x, ok
	case int32:
		x, ok := v.(int32)
		return x, ok
	case byte:
		x, ok := v.(byte)
		return x, ok
	case bool:
		x, ok := v.(bool)
		return x, ok
	}
	return nil, false
}

// valuesEqual compares a decoded point value against the value that was written,
// honouring the int16/int32/float32 widening the drivers apply on decode.
func valuesEqual(got, want interface{}) bool {
	switch w := want.(type) {
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case float32:
		g, ok := got.(float32)
		return ok && math.Abs(float64(g-w)) < 1e-6
	case int16:
		g, ok := got.(int16)
		return ok && g == w
	case int32:
		g, ok := got.(int32)
		return ok && g == w
	case uint16:
		g, ok := got.(uint16)
		return ok && g == uint16(w)
	}
	return false
}

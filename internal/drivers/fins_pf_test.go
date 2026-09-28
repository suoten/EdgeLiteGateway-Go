package drivers

import (
	"context"
	"encoding/hex"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"edgelite/internal/models"
)

// TestFinsPFAddressSuffixContract locks in the address convention the ProtoForge
// integration pushes for FINS points: the point address keeps its symbolic area
// and optional ".bit" part, and a data-type suffix is appended
// (r=float, i=int16, w=uint16, b=bool, dw=int32/uint32, str=string).
// See ProtoForge/protoforge/integrations/edgelite.py _translate_point_address.
func TestFinsPFAddressSuffixContract(t *testing.T) {
	cases := []struct {
		addr     string
		wantArea int
		wantOff  int
		wantBit  int
		wantBitA bool
		wantHint string
		wantType string
	}{
		{"D0,w", finsAreaDM, 0, 0, false, "w", "uint16"},
		{"D2,r", finsAreaDM, 2, 0, false, "r", "float32"},
		{"D10,i", finsAreaDM, 10, 0, false, "i", "int16"},
		{"D20.0,b", finsAreaDM, 20, 0, true, "b", "bool"},
		{"D30,dw", finsAreaDM, 30, 0, false, "dw", "int32"},
		{"CIO100.5,b", finsAreaCIO, 100, 5, true, "b", "bool"},
		{"W12,r", finsAreaWR, 12, 0, false, "r", "float32"},
		{"H3,w", finsAreaHR, 3, 0, false, "w", "uint16"},
		{"A7,i", finsAreaAR, 7, 0, false, "i", "int16"},
		// No suffix: the address still parses and the declared data type wins.
		{"D100", finsAreaDM, 100, 0, false, "", "int16"},
	}
	for _, c := range cases {
		area, off, bit, isBit, err := parseFINSAddress(c.addr)
		if err != nil {
			t.Fatalf("parseFINSAddress(%q): unexpected error %v", c.addr, err)
		}
		if area != c.wantArea || off != c.wantOff || bit != c.wantBit || isBit != c.wantBitA {
			t.Errorf("parseFINSAddress(%q) = (area 0x%02X, off %d, bit %d, isBit %v), want (0x%02X, %d, %d, %v)",
				c.addr, area, off, bit, isBit, c.wantArea, c.wantOff, c.wantBit, c.wantBitA)
		}
		if got := finsTypeHint(c.addr); got != c.wantHint {
			t.Errorf("finsTypeHint(%q) = %q, want %q", c.addr, got, c.wantHint)
		}
		declared := ""
		if c.wantHint == "" {
			declared = c.wantType
		}
		if got := finsResolveDataType(c.addr, declared); got != c.wantType {
			t.Errorf("finsResolveDataType(%q) = %q, want %q", c.addr, got, c.wantType)
		}
	}

	// float64 shares the "r" suffix with float32, so the declared type decides
	// the width; ProtoForge stores a float64 point as 8 bytes.
	if got := finsResolveDataType("D40,r", "float64"); got != "float64" {
		t.Errorf("float64 suffix resolution = %q, want float64", got)
	}
	if got := finsResolveDataType("D40,dw", "uint32"); got != "uint32" {
		t.Errorf("uint32 suffix resolution = %q, want uint32", got)
	}
	counts := map[string]int{
		"int16": 1, "uint16": 1, "bool": 1, "": 1,
		"int32": 2, "uint32": 2, "float32": 2, "float64": 4,
	}
	for dt, want := range counts {
		if got := finsWordCount(dt); got != want {
			t.Errorf("finsWordCount(%q) = %d, want %d", dt, got, want)
		}
	}
}

// TestFinsPFWriteEncodingContract checks the big-endian wire encoding used for
// writes, including the REAL case the legacy driver truncated to an integer.
func TestFinsPFWriteEncodingContract(t *testing.T) {
	cases := []struct {
		dt      string
		value   interface{}
		wantHex string
	}{
		{"uint16", int16(30000), "7530"},
		{"int32", int32(-70000), "FFFEEE90"},
		{"float32", float32(-12.25), "C1440000"},
		{"float32", float64(12.5), "41480000"},
		{"bool", true, "0001"},
	}
	for _, c := range cases {
		got, err := finsEncodeValue(c.dt, c.value)
		if err != nil {
			t.Fatalf("finsEncodeValue(%q, %v): %v", c.dt, c.value, err)
		}
		encoded := strings.ToUpper(hex.EncodeToString(got))
		if encoded != c.wantHex {
			t.Errorf("finsEncodeValue(%q, %v) = %s, want %s", c.dt, c.value, encoded, c.wantHex)
		}
	}
	if got := finsDefaultWriteType(float64(12.5)); got != "float32" {
		t.Errorf("finsDefaultWriteType(fractional float64) = %q, want float32", got)
	}
	if got := finsDefaultWriteType(float64(12)); got != "int16" {
		t.Errorf("finsDefaultWriteType(integral float64) = %q, want int16", got)
	}
	if got := finsDefaultWriteType(true); got != "bool" {
		t.Errorf("finsDefaultWriteType(bool) = %q, want bool", got)
	}
}

// TestFinsPFDriverLive drives the real FINSDriver through the registry against a
// running ProtoForge Omron FINS simulator on 127.0.0.1:9600 (device pf-fins
// serves D0/uint16, D2/float32, D10/uint16 and D20.0/bool). It skips when the
// simulator is not reachable and asserts decode TYPE + quality only, because the
// simulator's memory contents are volatile and the served device depends on which
// FINS device ProtoForge created last.
func TestFinsPFDriverLive(t *testing.T) {
	probe, err := net.DialTimeout("tcp", "127.0.0.1:9600", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Omron FINS simulator not reachable: %v", err)
	}
	probe.Close()

	RegisterAll()
	d, err := GetRegistry().CreateDriver("omron_fins", "test-fins", map[string]interface{}{
		"host": "127.0.0.1", "port": 9600, "node": 1, "unit": 0, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge FINS: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{
		{Name: "w0", Address: "D0,w", DataType: "uint16"},
		{Name: "f2", Address: "D2,r", DataType: "float32"},
		{Name: "w10", Address: "D10,i", DataType: "int16"},
		{Name: "b20", Address: "D20.0,b", DataType: "bool"},
		{Name: "d30", Address: "D30,dw", DataType: "int32"},
	}
	rows, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(rows) != len(pts) {
		t.Fatalf("ReadPoints returned %d rows, want %d", len(rows), len(pts))
	}
	got := pfIndexRows(rows)
	wantType := map[string]interface{}{
		"w0":  uint16(0),
		"f2":  float32(0),
		"w10": int16(0),
		"b20": false,
		"d30": int32(0),
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
		if _, ok := asSameType(row.Value, sample); !ok {
			t.Errorf("point %q decoded %T, want %T (value=%v)", name, row.Value, sample, row.Value)
		}
	}

	// --- write -> read round trip on words no ProtoForge point owns ---
	roundTrip := []struct {
		name    string
		address string
		write   interface{}
	}{
		{"u", "D200,w", int16(30000)},
		{"r", "D202,r", float32(-12.25)},
		{"i", "D204,dw", int32(-70000)},
		{"b", "D206.5,b", true},
	}
	for _, rt := range roundTrip {
		if err := d.WritePoint(ctx, rt.address, rt.write); err != nil {
			t.Fatalf("WritePoint(%s, %v): %v", rt.address, rt.write, err)
		}
		dt := finsResolveDataType(rt.address, "")
		rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: rt.name, Address: rt.address, DataType: dt}})
		if err != nil {
			t.Fatalf("ReadPoints(%s): %v", rt.address, err)
		}
		row := rows[0]
		if row.Quality != "good" {
			t.Fatalf("%s: quality = %q, want good", rt.address, row.Quality)
		}
		switch want := rt.write.(type) {
		case float32:
			v, ok := row.Value.(float32)
			if !ok || math.Abs(float64(v-want)) > 1e-6 {
				t.Fatalf("%s: read back %v (%T), want %v", rt.address, row.Value, row.Value, want)
			}
		case int16:
			switch v := row.Value.(type) {
			case int16:
				if v != want {
					t.Fatalf("%s: read back %d, want %d", rt.address, v, want)
				}
			case uint16:
				if int16(v) != want {
					t.Fatalf("%s: read back %d, want %d", rt.address, v, want)
				}
			default:
				t.Fatalf("%s: read back %v (%T), want %d", rt.address, row.Value, row.Value, want)
			}
		case int32:
			v, ok := row.Value.(int32)
			if !ok || v != want {
				t.Fatalf("%s: read back %v (%T), want %d", rt.address, row.Value, row.Value, want)
			}
		case bool:
			v, ok := row.Value.(bool)
			if !ok || v != want {
				t.Fatalf("%s: read back %v (%T), want %v", rt.address, row.Value, row.Value, want)
			}
		}
	}

	// The bit write must leave the rest of the word alone.
	if err := d.WritePoint(ctx, "D206.7,b", false); err != nil {
		t.Fatalf("WritePoint(D206.7,b, false): %v", err)
	}
	rows, err = d.ReadPoints(ctx, []models.PointDef{
		{Name: "b5", Address: "D206.5,b", DataType: "bool"},
		{Name: "b7", Address: "D206.7,b", DataType: "bool"},
	})
	if err != nil {
		t.Fatalf("ReadPoints bits: %v", err)
	}
	bits := pfIndexRows(rows)
	if v, _ := bits["b5"].Value.(bool); !v {
		t.Errorf("D206.5 was cleared by the neighbouring bit write")
	}
	if v, ok := bits["b7"].Value.(bool); !ok || v {
		t.Errorf("D206.7 = %v, want false", bits["b7"].Value)
	}

	if err := d.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

package drivers

// The CIP atomic data type codes are wire constants a controller answers with,
// so they are not ours to invent. An earlier revision of this table had eight
// of sixteen codes rotated (SINT filed under 0xF5, LWORD 0xD4 claimed by an
// 8-bit BYTE, ...), which on a real Logix CPU meant either "cannot decode" or -
// worse - a value cut at the wrong width out of a reply that was read as good
// quality. These tests pin the numbers and the widths they imply.
//
// Reference: ODVA GS-CIP atomic symbol data types, as implemented by pycomm3
// (pycomm3/cip/data_types.py, code + size per type) and libplctag.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"edgelite/internal/models"
)

// TestCipDataCodesAreTheSpecTable locks the numeric codes themselves. Renaming
// a constant is harmless; renumbering one changes what the gateway sends to and
// expects from a PLC, and no other test would notice.
func TestCipDataCodesAreTheSpecTable(t *testing.T) {
	want := map[string]uint16{
		"Bool":   0x00C1,
		"SInt":   0x00C2,
		"Int":    0x00C3,
		"DInt":   0x00C4,
		"LInt":   0x00C5,
		"USInt":  0x00C6,
		"UInt":   0x00C7,
		"UDInt":  0x00C8,
		"ULInt":  0x00C9,
		"Real":   0x00CA,
		"LReal":  0x00CB,
		"String": 0x00D0,
		"Byte":   0x00D1,
		"Word":   0x00D2,
		"DWord":  0x00D3,
		"LWord":  0x00D4,
	}
	got := map[string]uint16{
		"Bool": CipDataTypeBool, "SInt": CipDataTypeSInt, "Int": CipDataTypeInt,
		"DInt": CipDataTypeDInt, "LInt": CipDataTypeLInt, "USInt": CipDataTypeUSInt,
		"UInt": CipDataTypeUInt, "UDInt": CipDataTypeUDInt, "ULInt": CipDataTypeULInt,
		"Real": CipDataTypeReal, "LReal": CipDataTypeLReal, "String": CipDataTypeString,
		"Byte": CipDataTypeByte, "Word": CipDataTypeWord, "DWord": CipDataTypeDWord,
		"LWord": CipDataTypeLWord,
	}
	if len(got) != len(want) {
		t.Fatalf("table drift: %d constants vs %d expected", len(got), len(want))
	}
	for name, code := range want {
		if got[name] != code {
			t.Errorf("CipDataType%s = 0x%04X, spec value is 0x%04X", name, got[name], code)
		}
	}
	// A code owned by two types would make one of them undecodable, and Go
	// would not even compile the switches.
	seen := map[uint16]string{}
	for name, code := range got {
		if other, dup := seen[code]; dup {
			t.Errorf("CipDataType%s and CipDataType%s share 0x%04X", name, other, code)
		}
		seen[code] = name
	}
}

// TestCipElementWidths pins how many bytes one element occupies, which is what
// decides where a reply's value starts.
func TestCipElementWidths(t *testing.T) {
	cases := map[uint16]int{
		CipDataTypeBool: 1, CipDataTypeSInt: 1, CipDataTypeUSInt: 1, CipDataTypeByte: 1,
		CipDataTypeInt: 2, CipDataTypeUInt: 2, CipDataTypeWord: 2,
		CipDataTypeDInt: 4, CipDataTypeUDInt: 4, CipDataTypeDWord: 4, CipDataTypeReal: 4,
		CipDataTypeLInt: 8, CipDataTypeULInt: 8, CipDataTypeLWord: 8, CipDataTypeLReal: 8,
		CipDataTypeString: 0, // variable length, decided by its own length field
	}
	for code, width := range cases {
		if got := cipElementWidth(code); got != width {
			t.Errorf("cipElementWidth(0x%04X) = %d, want %d", code, got, width)
		}
	}
}

// TestCipTypeRoundTripPerCode writes one value per CIP type through the fake
// controller and reads it back, so encode and decode have to agree on the same
// code the way a real CPU would. A rotated code shows up here as either a
// refused write or a value that comes back different.
func TestCipTypeRoundTripPerCode(t *testing.T) {
	cases := []struct {
		code  uint16
		name  string
		write interface{}
		want  interface{}
	}{
		{CipDataTypeBool, "bool", true, true},
		{CipDataTypeSInt, "sint", -3, int8(-3)},
		{CipDataTypeInt, "int", -300, int16(-300)},
		{CipDataTypeDInt, "dint", -70000, int32(-70000)},
		{CipDataTypeLInt, "lint", int64(-5000000000), int64(-5000000000)},
		{CipDataTypeUSInt, "usint", 200, uint8(200)},
		{CipDataTypeUInt, "uint", 65535, uint16(65535)},
		{CipDataTypeUDInt, "udint", 4294967295, uint32(4294967295)},
		{CipDataTypeULInt, "ulint", 1234567890123, uint64(1234567890123)},
		{CipDataTypeReal, "real", 21.5, float32(21.5)},
		{CipDataTypeLReal, "lreal", 1.000000000001, 1.000000000001},
		{CipDataTypeByte, "byte", 250, uint8(250)},
		{CipDataTypeWord, "word", 65535, uint16(65535)},
		{CipDataTypeDWord, "dword", 4294967295, uint32(4294967295)},
		{CipDataTypeLWord, "lword", 1234567890123, uint64(1234567890123)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Start from bytes that are not the answer, so a decode that reads
			// past the value or ignores the write cannot pass by accident.
			f := startFakeController(t, c.code, bytes.Repeat([]byte{0xAA}, cipElementWidth(c.code)), true)
			d := connectFake(t, f)
			ctx := context.Background()

			if err := d.WritePoint(ctx, "Value", c.write); err != nil {
				t.Fatalf("WritePoint(%v): %v", c.write, err)
			}
			rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: "V", Address: "Value", DataType: "auto"}})
			if err != nil {
				t.Fatalf("ReadPoints: %v", err)
			}
			if rows[0].Quality != "good" {
				t.Fatalf("quality %s for CIP 0x%04X, value %v", rows[0].Quality, c.code, rows[0].Value)
			}
			if fmt.Sprint(rows[0].Value) != fmt.Sprint(c.want) {
				t.Fatalf("wrote %v, read %v (%T), want %v (%T)",
					c.write, rows[0].Value, rows[0].Value, c.want, c.want)
			}
		})
	}
}

// TestCipDecodeUsesTheWidthOfTheCodeReported: a negative INT answering 0xFFFF
// and a LWORD needing all eight bytes are the two ways a rotated table showed
// up - unsigned decoding of a signed type, and a value built from the element
// count. Both must be impossible now.
func TestCipDecodeUsesTheWidthOfTheCodeReported(t *testing.T) {
	t.Run("int_minus_one_stays_negative", func(t *testing.T) {
		f := startFakeController(t, CipDataTypeInt, []byte{0xFF, 0xFF}, true)
		d := connectFake(t, f)
		rows, err := d.ReadPoints(context.Background(),
			[]models.PointDef{{Name: "V", Address: "Value", DataType: "auto"}})
		if err != nil {
			t.Fatalf("ReadPoints: %v", err)
		}
		if v, ok := rows[0].Value.(int16); !ok || v != -1 {
			t.Fatalf("INT 0xFFFF decoded as %v (%T), want int16 -1 (a uint16 answer means the code table is rotated)",
				rows[0].Value, rows[0].Value)
		}
	})
	t.Run("lword_decodes_all_eight_bytes", func(t *testing.T) {
		val := make([]byte, 8)
		binary.LittleEndian.PutUint64(val, 0x1122334455667788)
		f := startFakeController(t, CipDataTypeLWord, val, true)
		d := connectFake(t, f)
		rows, err := d.ReadPoints(context.Background(),
			[]models.PointDef{{Name: "V", Address: "Value", DataType: "auto"}})
		if err != nil {
			t.Fatalf("ReadPoints: %v", err)
		}
		if v, ok := rows[0].Value.(uint64); !ok || v != 0x1122334455667788 {
			t.Fatalf("LWORD decoded as %v (%T), want uint64 0x1122334455667788", rows[0].Value, rows[0].Value)
		}
	})
	t.Run("unknown_code_is_not_guessed", func(t *testing.T) {
		// 0x00CC is a CIP type we do not decode; inventing a value for it is
		// worse than reporting that the tag cannot be read.
		f := startFakeController(t, 0x00CC, []byte{1, 2, 3, 4, 5, 6, 7, 8}, true)
		d := connectFake(t, f)
		rows, err := d.ReadPoints(context.Background(),
			[]models.PointDef{{Name: "V", Address: "Value", DataType: "auto"}})
		if err == nil && len(rows) > 0 && rows[0].Quality == "good" {
			t.Fatalf("unknown CIP code 0x00CC produced %v (%T) with good quality", rows[0].Value, rows[0].Value)
		}
	})
}

// TestEncodeCipValueCoversEveryCode keeps the encoder aligned with the table:
// each fixed-width type must emit exactly its width, and a write aimed at a
// type the driver does not know must be refused rather than padded or truncated.
func TestEncodeCipValueCoversEveryCode(t *testing.T) {
	fixed := []struct {
		code  uint16
		value interface{}
		width int
	}{
		{CipDataTypeBool, true, 2},
		{CipDataTypeSInt, -1, 2},
		{CipDataTypeInt, -1, 2},
		{CipDataTypeDInt, -1, 4},
		{CipDataTypeLInt, -1, 8},
		{CipDataTypeUSInt, 1, 2},
		{CipDataTypeUInt, 1, 2},
		{CipDataTypeUDInt, 1, 4},
		{CipDataTypeULInt, 1, 8},
		{CipDataTypeReal, 1.5, 4},
		{CipDataTypeLReal, 1.5, 8},
		{CipDataTypeByte, 1, 2},
		{CipDataTypeWord, 1, 2},
		{CipDataTypeDWord, 1, 4},
		{CipDataTypeLWord, 1, 8},
	}
	for _, c := range fixed {
		got, err := encodeCipValue(c.code, c.value)
		if err != nil {
			t.Errorf("encodeCipValue(0x%04X, %v): %v", c.code, c.value, err)
			continue
		}
		if len(got) != c.width {
			t.Errorf("encodeCipValue(0x%04X, %v) = % x, want %d bytes", c.code, c.value, got, c.width)
		}
	}
	if got, err := encodeCipValue(CipDataTypeString, "abc"); err != nil || !bytes.Equal(got, []byte("abc")) {
		t.Errorf("string encode = % q (%v), want abc", got, err)
	}
	// A code that names no layout must not be written with a guessed one.
	if _, err := encodeCipValue(0x00CC, 1); err == nil {
		t.Errorf("encodeCipValue accepted unknown code 0x00CC")
	}
	// 64-bit float precision is the one place a whole-number check is not
	// enough: LREAL has to carry the fraction.
	if got, err := encodeCipValue(CipDataTypeLReal, math.Pi); err != nil {
		t.Errorf("LREAL pi: %v", err)
	} else if math.Float64frombits(binary.LittleEndian.Uint64(got)) != math.Pi {
		t.Errorf("LREAL pi encoded as % x", got)
	}
}

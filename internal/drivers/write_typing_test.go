package drivers

// A write carries the operator's number and the point's declared data type; the
// wire format only fits some of them. Both gaps covered here let a value be
// stored as something else while the API answered success: a Modbus holding
// register wrapped 70000 into 4464, and an OPC UA Float rejected every write
// because a JSON number can only arrive as a Double.

import (
	"context"
	"math"
	"strings"
	"testing"

	"edgelite/internal/models"
)

func TestHoldingRegisterEncodeRefusesOutOfRangeValues(t *testing.T) {
	cases := []struct {
		name      string
		dataType  string
		value     interface{}
		want      uint16
		wantError string
	}{
		{name: "uint16 upper bound", dataType: "uint16", value: float64(math.MaxUint16), want: math.MaxUint16},
		{name: "uint16 overflow", dataType: "uint16", value: float64(70000), wantError: "out of range for uint16"},
		{name: "uint16 negative", dataType: "uint16", value: float64(-1), wantError: "out of range for uint16"},
		{name: "word alias overflow", dataType: "word", value: float64(70000), wantError: "out of range for word"},
		{name: "int16 lower bound", dataType: "int16", value: float64(math.MinInt16), want: 0x8000},
		{name: "int16 overflow", dataType: "int16", value: float64(32768), wantError: "out of range for int16"},
		{name: "int8 overflow", dataType: "int8", value: float64(300), wantError: "out of range for int8"},
		{name: "uint8 overflow", dataType: "uint8", value: float64(256), wantError: "out of range for uint8"},
		// An undeclared type has no range to enforce, so the conversion is the
		// same one that ran before the guard existed.
		{name: "undeclared type", dataType: "", value: float64(70000), want: 4464},
		{name: "wider type", dataType: "float32", value: float64(3.5), want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toUint16ForType(tc.value, tc.dataType)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("toUint16ForType(%v, %q) returned %v", tc.value, tc.dataType, err)
				}
				if got != tc.want {
					t.Fatalf("toUint16ForType(%v, %q) = %d, want %d", tc.value, tc.dataType, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("toUint16ForType(%v, %q) = %d, want an error mentioning %q", tc.value, tc.dataType, got, tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantError)
			}
		})
	}
}

// The guard has to sit on the path the device service actually takes, not only on
// the helper, so this drives a typed write through the driver and reads it back.
func TestModbusSlaveTypedWriteRejectsUnrepresentableValue(t *testing.T) {
	created, err := NewModbusSlaveDriver("typed-range", map[string]interface{}{"holding_size": 10})
	if err != nil {
		t.Fatalf("NewModbusSlaveDriver: %v", err)
	}
	drv := created.(*ModbusSlaveDriver)
	ctx := context.Background()

	if err := drv.WritePointTyped(ctx, "HR5", float64(40000), "uint16"); err != nil {
		t.Fatalf("in-range write returned %v", err)
	}
	points := []models.PointDef{{Name: "hr5", Address: "HR5", DataType: "uint16"}}
	read, err := drv.ReadPoints(ctx, points)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(read) != 1 || read[0].Value == nil || read[0].Value.(uint16) != 40000 {
		t.Fatalf("read back %v, want 40000", read)
	}

	err = drv.WritePointTyped(ctx, "HR5", float64(70000), "uint16")
	if err == nil {
		t.Fatal("70000 into a uint16 register reported success")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("error %q does not report a range failure", err.Error())
	}
	// The rejected write must leave the previous value standing, not a partial one.
	read, err = drv.ReadPoints(ctx, points)
	if err != nil {
		t.Fatalf("ReadPoints after the rejected write: %v", err)
	}
	if read[0].Value == nil || read[0].Value.(uint16) != 40000 {
		t.Fatalf("rejected write changed the register to %v, want it unchanged at 40000", read[0].Value)
	}
}

// The S7 address carries a width (DBB/DBW/DBD) but no signedness, so the guard
// accepts the union of both ranges and refuses anything wider than the target.
func TestS7WriteRangeRejectsValuesTheAddressCannotHold(t *testing.T) {
	cases := []struct {
		name      string
		value     interface{}
		bits      uint
		wantError string
	}{
		{name: "byte", value: float64(255), bits: 8},
		{name: "byte negative fits as two's complement", value: float64(-128), bits: 8},
		{name: "byte overflow", value: float64(300), bits: 8, wantError: "does not fit a 8-bit S7 target"},
		{name: "word", value: float64(40000), bits: 16},
		{name: "word overflow", value: float64(70000), bits: 16, wantError: "does not fit a 16-bit S7 target"},
		{name: "dword integer overflow", value: float64(5e9), bits: 32, wantError: "does not fit a 32-bit S7 target"},
		// A DBD holding a REAL is encoded as IEEE-754 bits, not as an integer width.
		{name: "dword real", value: float64(12.5), bits: 32},
		{name: "dword float32", value: float32(1e30), bits: 32},
		{name: "non numeric is the driver's own problem", value: "abc", bits: 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkS7WriteRange(tc.value, tc.bits)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("checkS7WriteRange(%v, %d) returned %v", tc.value, tc.bits, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkS7WriteRange(%v, %d) accepted a value, want %q", tc.value, tc.bits, tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantError)
			}
		})
	}
}

func TestFINSEncodeRefusesValuesOutsideTheDeclaredType(t *testing.T) {
	cases := []struct {
		name      string
		dataType  string
		value     interface{}
		wantError string
	}{
		{name: "word in range", dataType: "uint16", value: float64(65535)},
		{name: "word overflow", dataType: "uint16", value: float64(70000), wantError: "out of range for uint16"},
		{name: "signed negative", dataType: "int16", value: float64(-32768)},
		{name: "signed overflow", dataType: "int16", value: float64(32768), wantError: "out of range for int16"},
		{name: "dint overflow", dataType: "int32", value: float64(3e9), wantError: "out of range for int32"},
		{name: "real is not width checked", dataType: "float32", value: float64(1e30)},
		{name: "bool", dataType: "bool", value: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := finsEncodeValue(tc.dataType, tc.value)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("finsEncodeValue(%q, %v) = %v, want %q", tc.dataType, tc.value, got, tc.wantError)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("finsEncodeValue(%q, %v) returned %v", tc.dataType, tc.value, err)
			}
		})
	}
}

// MC widens an integer that does not fit one word instead of wrapping it; a plain
// Go int reaching that path used to fall through to the truncating conversion.
func TestMCEncodeWidensGoIntsTooBigForOneWord(t *testing.T) {
	words, err := mcEncodeValue(int(3000000))
	if err != nil {
		t.Fatalf("mcEncodeValue returned %v", err)
	}
	if len(words) != 2 {
		t.Fatalf("mcEncodeValue(int) = %v, want two words", words)
	}
	if got := int32(uint32(words[0]) | uint32(words[1])<<16); got != 3000000 {
		t.Fatalf("round-trip gave %d, want 3000000", got)
	}
}

func TestCoerceOPCUAWriteValueTypesTheVariant(t *testing.T) {
	cases := []struct {
		name      string
		value     interface{}
		dataType  string
		want      interface{}
		wantError string
	}{
		// A JSON number arrives as float64, which encodes as Double; the ProtoForge
		// Float node answered StatusBadTypeMismatch until it gets a real float32.
		{name: "float32", value: 66.5, dataType: "float32", want: float32(66.5)},
		{name: "float alias", value: 66.5, dataType: "float", want: float32(66.5)},
		{name: "real alias", value: 66.5, dataType: "REAL", want: float32(66.5)},
		{name: "double", value: 66.5, dataType: "double", want: 66.5},
		{name: "int32", value: float64(-7), dataType: "int32", want: int32(-7)},
		{name: "uint16", value: float64(600), dataType: "uint16", want: uint16(600)},
		{name: "bool from number", value: float64(1), dataType: "bool", want: true},
		{name: "bool passthrough", value: false, dataType: "boolean", want: false},
		{name: "string from number", value: float64(12), dataType: "string", want: "12"},
		{name: "empty type is the server's call", value: 66.5, dataType: "", want: 66.5},
		{name: "unknown type is the server's call", value: 66.5, dataType: "structure", want: 66.5},
		{name: "int32 overflow", value: float64(3e9), dataType: "int32", wantError: "does not fit the declared int32"},
		{name: "uint16 overflow", value: float64(70000), dataType: "uint16", wantError: "does not fit the declared uint16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerceOPCUAWriteValue(tc.value, tc.dataType)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("coerce(%v, %q) = %v, want an error mentioning %q", tc.value, tc.dataType, got, tc.wantError)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("coerce(%v, %q) returned %v", tc.value, tc.dataType, err)
			}
			if got != tc.want {
				t.Fatalf("coerce(%v, %q) = %#v (%T), want %#v (%T)", tc.value, tc.dataType, got, got, tc.want, tc.want)
			}
		})
	}
}

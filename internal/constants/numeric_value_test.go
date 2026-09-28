package constants

import "testing"

// The register drivers hand back uint16 / int16 / uint32 for a read, so a
// numeric conversion that only lists float64/int/int32/int64 answers "not a
// number" for every Modbus, S7 and FINS sample — which silently stopped
// threshold alarms, linkage rules and bridge conversions from ever firing.

func TestNumericAsFloatAcceptsWhatDriversReturn(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  float64
	}{
		{"uint16", uint16(7), 7},
		{"int16", int16(-3), -3},
		{"uint32", uint32(70000), 70000},
		{"int32", int32(-70000), -70000},
		{"uint64", uint64(4), 4},
		{"int64", int64(-4), -4},
		{"uint", uint(5), 5},
		{"int", int(6), 6},
		{"uint8", uint8(255), 255},
		{"int8", int8(-128), -128},
		{"float32", float32(1.5), 1.5},
		{"float64", 2.25, 2.25},
		{"true", true, 1},
		{"false", false, 0},
		{"numeric text", " 75.5 ", 75.5},
	}
	for _, tc := range cases {
		got, ok := NumericAsFloat(tc.value)
		if !ok {
			t.Fatalf("%s: reported as not a number, want %v", tc.name, tc.want)
		}
		if got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNumericAsFloatRefusesWhatIsNotAMeasurement(t *testing.T) {
	for _, v := range []interface{}{nil, "setpoint-a", "", []interface{}{1}, map[string]int{"a": 1}} {
		if got, ok := NumericAsFloat(v); ok {
			t.Fatalf("NumericAsFloat(%#v) = %v, want it refused", v, got)
		}
	}
}

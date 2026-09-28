package services

import (
	"math"
	"testing"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

func f64(v float64) *float64 { return &v }

// TestApplyPointScaling covers the read-path unit transform that every protocol
// shares: raw register counts become engineering units before storage.
func TestApplyPointScaling(t *testing.T) {
	defs := []models.PointDef{
		{Name: "temp", DataType: "float32", Scale: f64(0.1), Offset: f64(-40)},
		{Name: "count", DataType: "int16", Scale: f64(2)},
		{Name: "raw", DataType: "uint16"},
		{Name: "status", DataType: "string"},
		{Name: "alarm", DataType: "bool", Scale: f64(1)},
	}
	points := []storage.PointData{
		{PointName: "temp", Value: float64(250), Quality: "good"},
		{PointName: "count", Value: uint16(3), Quality: "good"},
		{PointName: "raw", Value: uint16(7), Quality: "good"},
		{PointName: "status", Value: "RUN", Quality: "good"},
		{PointName: "alarm", Value: true, Quality: "good"},
		{PointName: "missing", Value: float64(1), Quality: "good"},
		{PointName: "temp", Value: nil, Quality: "bad"},
	}
	applyPointScaling(defs, points)

	if v, ok := points[0].Value.(float64); !ok || math.Abs(v-(-15)) > 1e-9 {
		t.Errorf("250 counts at scale 0.1 offset -40 = %v (%T), want -15", points[0].Value, points[0].Value)
	}
	// An integral data type is rounded so the UI never shows "6" for a count of 3.
	if v, ok := points[1].Value.(float64); !ok || v != 6 {
		t.Errorf("int16 count = %v (%T), want 6", points[1].Value, points[1].Value)
	}
	// No scale/offset declared: the value is left exactly as the driver produced
	// it, including its original type.
	if v, ok := points[2].Value.(uint16); !ok || v != 7 {
		t.Errorf("unscaled point = %v (%T), want uint16(7)", points[2].Value, points[2].Value)
	}
	if v, ok := points[3].Value.(string); !ok || v != "RUN" {
		t.Errorf("string point = %v (%T), want untouched RUN", points[3].Value, points[3].Value)
	}
	if v, ok := points[4].Value.(bool); !ok || !v {
		t.Errorf("bool point = %v (%T), want untouched true", points[4].Value, points[4].Value)
	}
	if v, ok := points[5].Value.(float64); !ok || v != 1 {
		t.Errorf("undeclared point = %v (%T), want untouched 1", points[5].Value, points[5].Value)
	}
	if points[6].Value != nil {
		t.Errorf("nil value became %v, want nil", points[6].Value)
	}

	// Offsets apply on their own too (a bare zero-shift must not be treated as
	// "no scaling configured", which would silently drop the transform).
	defs2 := []models.PointDef{{Name: "k", DataType: "float64", Offset: f64(273.15)}}
	pts2 := []storage.PointData{{PointName: "k", Value: 1.0}}
	applyPointScaling(defs2, pts2)
	if v, ok := pts2[0].Value.(float64); !ok || math.Abs(v-274.15) > 1e-9 {
		t.Errorf("offset-only point = %v (%T), want 274.15", pts2[0].Value, pts2[0].Value)
	}

	// The narrow integer kinds a driver can hand back are scaled too: skipping a
	// kind here leaves that tag's engineering units silently unconverted.
	defs3 := []models.PointDef{
		{Name: "b", DataType: "uint8", Scale: f64(2)},
		{Name: "n", DataType: "int8", Scale: f64(2)},
		{Name: "w", DataType: "uint", Scale: f64(2)},
	}
	pts3 := []storage.PointData{
		{PointName: "b", Value: uint8(200)},
		{PointName: "n", Value: int8(-5)},
		{PointName: "w", Value: uint(3)},
	}
	applyPointScaling(defs3, pts3)
	if v, ok := pts3[0].Value.(float64); !ok || v != 400 {
		t.Errorf("uint8 = %v (%T), want 400", pts3[0].Value, pts3[0].Value)
	}
	if v, ok := pts3[1].Value.(float64); !ok || v != -10 {
		t.Errorf("int8 = %v (%T), want -10", pts3[1].Value, pts3[1].Value)
	}
	if v, ok := pts3[2].Value.(float64); !ok || v != 6 {
		t.Errorf("uint = %v (%T), want 6", pts3[2].Value, pts3[2].Value)
	}

	// An empty definition list must not touch anything, and must not panic.
	keep := []storage.PointData{{PointName: "temp", Value: 5.0}}
	applyPointScaling(nil, keep)
	applyPointScaling(defs, nil)
	if keep[0].Value != 5.0 {
		t.Errorf("value changed with no defs: %v", keep[0].Value)
	}
}

// TestInversePointScalingWriteRoundTrip is the property that matters for writes:
// the value an operator sends is what they read back afterwards.
func TestInversePointScalingWriteRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		def         models.PointDef
		engineering float64
	}{
		{"scale and offset", models.PointDef{Name: "p", DataType: "float32", Scale: f64(0.1), Offset: f64(-40)}, 21.5},
		{"offset only", models.PointDef{Name: "p", DataType: "float32", Offset: f64(-40)}, 21.5},
		{"negative scale", models.PointDef{Name: "p", DataType: "float32", Scale: f64(-2)}, 21.5},
		{"integral", models.PointDef{Name: "p", DataType: "int32", Scale: f64(0.5)}, 21.0},
		{"no scaling", models.PointDef{Name: "p", DataType: "float32"}, 21.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			def := c.def
			raw, err := inversePointScaling(&def, c.engineering)
			if err != nil {
				t.Fatalf("inversePointScaling(%v): %v", c.engineering, err)
			}
			points := []storage.PointData{{PointName: "p", Value: raw}}
			applyPointScaling([]models.PointDef{def}, points)
			got, ok := points[0].Value.(float64)
			if !ok {
				t.Fatalf("scaled read-back is %T, want float64", points[0].Value)
			}
			if math.Abs(got-c.engineering) > 1e-9 {
				t.Fatalf("round trip %v -> %v -> %v", c.engineering, raw, got)
			}
		})
	}
}

func TestInversePointScaling(t *testing.T) {
	if got, err := inversePointScaling(nil, 42.0); err != nil || got != 42.0 {
		t.Errorf("nil def = (%v,%v), want passthrough", got, err)
	}
	// A string/bool setpoint has no unit transform: it must reach the driver as-is.
	def := models.PointDef{Name: "s", DataType: "string", Scale: f64(2)}
	if got, err := inversePointScaling(&def, "on"); err != nil || got != "on" {
		t.Errorf("non-numeric write = (%v,%v), want on passthrough", got, err)
	}
	if got, err := inversePointScaling(&def, true); err != nil || got != true {
		t.Errorf("bool write = (%v,%v), want true passthrough", got, err)
	}
	// scale 0 would divide by zero and silently send +Inf to the device.
	zero := models.PointDef{Name: "z", DataType: "float32", Scale: f64(0)}
	if _, err := inversePointScaling(&zero, 5.0); err == nil {
		t.Error("scale 0 accepted: the write would send an unusable value to the device")
	}
	// Integral data types round to a whole raw count, never a fractional register.
	i := models.PointDef{Name: "i", DataType: "uint16", Scale: f64(3)}
	got, err := inversePointScaling(&i, 5.0)
	if err != nil {
		t.Fatalf("inversePointScaling: %v", err)
	}
	if v, ok := got.(float64); !ok || v != 2 {
		t.Errorf("uint16 raw = %v (%T), want 2", got, got)
	}
	// Non-float Go types arriving from other code paths must still convert.
	for _, v := range []interface{}{int(3), int64(3), float32(3), uint16(3)} {
		got, err := inversePointScaling(&i, v)
		if err != nil {
			t.Errorf("inversePointScaling(%T 3): %v", v, err)
			continue
		}
		if f, ok := got.(float64); !ok || f != 1 {
			t.Errorf("inversePointScaling(%T 3) = %v (%T), want 1", v, got, got)
		}
	}
}

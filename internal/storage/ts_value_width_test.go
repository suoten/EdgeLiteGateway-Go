package storage

import (
	"testing"
	"time"
)

// WritePoints used to carry a hand-written type switch that knew float64,
// float32, int, int64, int32, bool and string. The register drivers return
// uint16 / int16 / uint32, so every Modbus, S7 and FINS sample was stored as
// value=NULL, value_str=NULL — with quality still saying "good". The live value
// endpoint kept showing the real number, so the loss was only visible once
// history was queried. These tests pin the width of what gets persisted.

func queryOne(t *testing.T, ts *TimeSeriesStorage, device, point string) PointData {
	t.Helper()
	now := time.Now()
	recs, err := ts.QueryPoints(device, point, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryPoints(%s/%s): %v", device, point, err)
	}
	if len(recs) != 1 {
		t.Fatalf("QueryPoints(%s/%s) = %d rows, want 1", device, point, len(recs))
	}
	return recs[0]
}

func TestWritePointsStoresEveryDriverNumericKind(t *testing.T) {
	ts := newTestTSStorage(t)
	now := time.Now()
	cases := []struct {
		point string
		value interface{}
		want  float64
	}{
		{"u16", uint16(7), 7},
		{"i16", int16(-3), -3},
		{"u32", uint32(70000), 70000},
		{"i32", int32(-70000), -70000},
		{"u8", uint8(200), 200},
		{"i8", int8(-5), -5},
		{"uint", uint(9), 9},
		{"int", int(11), 11},
		{"f32", float32(1.5), 1.5},
		{"f64", float64(2.25), 2.25},
		{"true", true, 1},
		{"false", false, 0},
	}
	points := make([]PointData, 0, len(cases))
	for i, tc := range cases {
		points = append(points, PointData{
			DeviceID:  "dev-width",
			PointName: tc.point,
			Value:     tc.value,
			Quality:   "good",
			Timestamp: now.Add(time.Duration(i) * time.Second),
		})
	}
	if err := ts.WritePoints(points); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}
	for _, tc := range cases {
		rec := queryOne(t, ts, "dev-width", tc.point)
		got, ok := rec.Value.(float64)
		if !ok {
			t.Fatalf("point %s stored %#v, want the float64 %v", tc.point, rec.Value, tc.want)
		}
		if got != tc.want {
			t.Fatalf("point %s = %v, want %v", tc.point, got, tc.want)
		}
		if rec.Quality != "good" {
			t.Fatalf("point %s quality = %q, want good: a value the driver produced is a good sample", tc.point, rec.Quality)
		}
	}
}

func TestWritePointsKeepsTextAndStructuredValues(t *testing.T) {
	ts := newTestTSStorage(t)
	now := time.Now()
	type colour struct {
		R int `json:"r"`
		G int `json:"g"`
	}
	if err := ts.WritePoints([]PointData{
		{DeviceID: "dev-txt", PointName: "label", Value: "setpoint-a", Quality: "good", Timestamp: now},
		{DeviceID: "dev-txt", PointName: "blob", Value: []byte{0x01, 0x02}, Quality: "good", Timestamp: now.Add(time.Second)},
		{DeviceID: "dev-txt", PointName: "struct", Value: colour{R: 1, G: 2}, Quality: "good", Timestamp: now.Add(2 * time.Second)},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}
	if got := queryOne(t, ts, "dev-txt", "label").Value; got != "setpoint-a" {
		t.Fatalf("label = %#v, want the text as written", got)
	}
	// A value that is neither a number nor a text tag still has to survive the
	// round trip: dropping it would leave a NULL nobody can read back.
	if got := queryOne(t, ts, "dev-txt", "blob").Value; got != `"AQI="` {
		t.Fatalf("blob = %#v, want base64 JSON text", got)
	}
	if got := queryOne(t, ts, "dev-txt", "struct").Value; got != `{"r":1,"g":2}` {
		t.Fatalf("struct = %#v, want JSON text", got)
	}
}

// A row that holds no value at all is not a good measurement; calling it good
// is what let the dropped-value bug above hide for so long.
func TestWritePointsMarksValuelessSampleBad(t *testing.T) {
	ts := newTestTSStorage(t)
	now := time.Now()
	if err := ts.WritePoints([]PointData{
		{DeviceID: "dev-nil", PointName: "temp", Value: nil, Quality: "good", Timestamp: now},
		{DeviceID: "dev-nil", PointName: "press", Value: nil, Quality: "uncertain", Timestamp: now},
		{DeviceID: "dev-nil", PointName: "flow", Value: 3, Quality: "good", Timestamp: now},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}
	if rec := queryOne(t, ts, "dev-nil", "temp"); rec.Quality != "bad" {
		t.Fatalf("nil value quality = %q, want bad", rec.Quality)
	} else if rec.Value != nil {
		t.Fatalf("nil value stored as %#v", rec.Value)
	}
	if rec := queryOne(t, ts, "dev-nil", "press"); rec.Quality != "uncertain" {
		t.Fatalf("declared quality = %q, want it kept as the driver wrote it", rec.Quality)
	}
	if rec := queryOne(t, ts, "dev-nil", "flow"); rec.Quality != "good" {
		t.Fatalf("good sample with a value = %q, want good", rec.Quality)
	}

	rows, err := ts.QualityByPoint("dev-nil", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("QualityByPoint: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per point: %#v", len(rows), rows)
	}
	// Points come back ordered by name: flow, press, temp.
	if rows[2].ValidRows != 0 || rows[2].InvalidRows != 1 {
		t.Fatalf("temp = %+v, want the valueless row counted as invalid", rows[2])
	}
	if rows[0].ValidRows != 1 || rows[0].InvalidRows != 0 {
		t.Fatalf("flow = %+v, want the measured row counted as valid", rows[0])
	}
}

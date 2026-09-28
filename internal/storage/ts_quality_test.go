package storage

import (
	"testing"
	"time"
)

// The /data-quality per-device and trend endpoints are backed by these two
// aggregates; before them the handlers answered a hardcoded 100% and an empty
// array, so "never measured" was reported as "flawless".

func TestQualityByPoint(t *testing.T) {
	ts := newTestTSStorage(t)
	base := time.Now().Add(-30 * time.Minute)
	points := []PointData{
		{DeviceID: "dev-q", PointName: "temp", Value: 1, Quality: "good", Timestamp: base},
		{DeviceID: "dev-q", PointName: "temp", Value: 2, Quality: "downsampled", Timestamp: base.Add(time.Minute)},
		{DeviceID: "dev-q", PointName: "temp", Value: 3, Quality: "bad", Timestamp: base.Add(2 * time.Minute)},
		{DeviceID: "dev-q", PointName: "press", Value: 4, Quality: "uncertain", Timestamp: base.Add(3 * time.Minute)},
		// A second device must not leak into this aggregate.
		{DeviceID: "dev-other", PointName: "temp", Value: 5, Quality: "good", Timestamp: base},
	}
	if err := ts.WritePoints(points); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	rows, err := ts.QualityByPoint("dev-q", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("QualityByPoint: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the device's 2 points: %#v", len(rows), rows)
	}
	press, temp := rows[0], rows[1]
	if press.PointName != "press" || temp.PointName != "temp" {
		t.Fatalf("ORDER BY point_name lost: %q then %q", press.PointName, temp.PointName)
	}
	if temp.TotalRows != 3 || temp.ValidRows != 2 || temp.InvalidRows != 1 {
		t.Fatalf("temp = %+v, want 3 rows / 2 valid (downsampled counts) / 1 invalid", temp)
	}
	if press.TotalRows != 1 || press.ValidRows != 0 || press.InvalidRows != 1 {
		t.Fatalf("press = %+v, want an all-invalid point", press)
	}
	if press.LastTimestamp == "" {
		t.Fatalf("%+v has no last timestamp: the UI shows freshness from it", press)
	}

	future, err := ts.QualityByPoint("dev-q", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("QualityByPoint(future): %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("window not applied: %#v", future)
	}
}

func TestQualityHourlyBuckets(t *testing.T) {
	ts := newTestTSStorage(t)
	// Truncate to the hour so a bucket can never be split by the test starting
	// a few seconds before the boundary.
	latest := time.Now().Truncate(time.Hour)
	earlier := latest.Add(-time.Hour)
	if err := ts.WritePoints([]PointData{
		{DeviceID: "dev-b", PointName: "temp", Value: 1, Quality: "good", Timestamp: earlier},
		{DeviceID: "dev-b", PointName: "temp", Value: 2, Quality: "bad", Timestamp: earlier.Add(time.Minute)},
		{DeviceID: "dev-b", PointName: "temp", Value: 3, Quality: "good", Timestamp: latest},
		{DeviceID: "dev-c", PointName: "temp", Value: 4, Quality: "good", Timestamp: latest},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	rows, err := ts.QualityHourlyBuckets("dev-b", time.Now().Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("QualityHourlyBuckets: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per hour: %#v", len(rows), rows)
	}
	if rows[0].Bucket >= rows[1].Bucket {
		t.Fatalf("buckets not oldest first: %q then %q", rows[0].Bucket, rows[1].Bucket)
	}
	if rows[0].TotalRows != 2 || rows[0].ValidRows != 1 {
		t.Fatalf("earlier bucket = %+v, want 2 rows / 1 valid", rows[0])
	}
	if rows[1].TotalRows != 1 || rows[1].ValidRows != 1 {
		t.Fatalf("latest bucket = %+v, want the device's own row only", rows[1])
	}

	all, err := ts.QualityHourlyBuckets("", time.Now().Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("QualityHourlyBuckets(all): %v", err)
	}
	if len(all) != 2 || all[1].TotalRows != 2 {
		t.Fatalf("gateway-wide buckets = %#v, want the latest hour to sum both devices", all)
	}
}

package storage

import (
	"path/filepath"
	"testing"
	"time"

	"edgelite/internal/config"
)

func newTestTSStorage(t *testing.T) *TimeSeriesStorage {
	t.Helper()
	cfg := &config.AppConfig{
		InfluxDB: config.InfluxDBConfig{
			SQLiteTSPath: filepath.Join(t.TempDir(), "ts.db"),
		},
	}
	ts, err := NewTimeSeriesStorage(cfg)
	if err != nil {
		t.Fatalf("NewTimeSeriesStorage failed: %v", err)
	}
	t.Cleanup(func() { ts.Close() })
	return ts
}

func TestRunDownsample(t *testing.T) {
	ts := newTestTSStorage(t)

	// 10 days ago: 5 samples at 1-minute intervals — all far beyond every
	// tier cutoff (7/30/90 days would not touch; use 1/2/3), so tier3 must
	// aggregate all 5 into a single 1-day bucket.
	base := time.Now().AddDate(0, 0, -10).Truncate(time.Hour)
	var points []PointData
	for i := 0; i < 5; i++ {
		points = append(points, PointData{
			DeviceID:  "dev-ds",
			PointName: "temp",
			Value:     float64(i),
			Quality:   "good",
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		})
	}
	// Recent samples inside the tier1 window must not be touched.
	for i := 0; i < 5; i++ {
		points = append(points, PointData{
			DeviceID:  "dev-ds",
			PointName: "temp",
			Value:     1.0,
			Quality:   "good",
			Timestamp: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	if err := ts.WritePoints(points); err != nil {
		t.Fatalf("WritePoints failed: %v", err)
	}

	results, err := ts.RunDownsample(1, 2, 3)
	if err != nil {
		t.Fatalf("RunDownsample failed: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Expected 3 tier results, got %d", len(results))
	}
	if results[0].Tier != 3 || results[0].RowsProcessed != 5 || results[0].RowsArchived != 1 {
		t.Errorf("tier3: expected processed=5 archived=1, got %+v", results[0])
	}
	if results[1].RowsProcessed != 0 || results[2].RowsProcessed != 0 {
		t.Errorf("tier2/tier1 should process nothing, got %+v / %+v", results[1], results[2])
	}

	total, downsampled, err := ts.DownsampleCounts()
	if err != nil {
		t.Fatalf("DownsampleCounts failed: %v", err)
	}
	if downsampled != 1 {
		t.Errorf("Expected 1 downsampled row, got %d", downsampled)
	}
	if total != 6 {
		t.Errorf("Expected 6 total rows after downsample, got %d", total)
	}

	// Downsampled bucket must carry the marker quality; raw rows untouched.
	rows, err := ts.QueryPoints("dev-ds", "temp", base.AddDate(0, 0, -1), time.Now())
	if err != nil {
		t.Fatalf("QueryPoints failed: %v", err)
	}
	markerCount, rawCount := 0, 0
	for _, r := range rows {
		if r.Quality == "downsampled" {
			markerCount++
		} else if r.Quality == "good" {
			rawCount++
		}
	}
	if markerCount != 1 || rawCount != 5 {
		t.Errorf("Expected 1 downsampled + 5 raw rows, got %d + %d", markerCount, rawCount)
	}

	// Running again must be idempotent: no further processing.
	results2, err := ts.RunDownsample(1, 2, 3)
	if err != nil {
		t.Fatalf("Second RunDownsample failed: %v", err)
	}
	var processedAgain int64
	for _, r := range results2 {
		processedAgain += r.RowsProcessed
	}
	if processedAgain != 0 {
		t.Errorf("Expected idempotent second run, processed %d rows", processedAgain)
	}
}

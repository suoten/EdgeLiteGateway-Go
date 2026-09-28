package storage

// GetLatestPoints used to resolve the newest row per point with a correlated
// `id = (SELECT MAX(id) ... WHERE device_id = t1.device_id ...)` tested against
// every row of time_series. On the scratch gateway's 118k-sample store that took
// 61s while holding the storage read lock - long enough to block WritePoints and
// to get the caller killed by the HTTP timeout middleware. The device-scoped
// rewrite answers in 0.02s. This test pins both halves: the rows it returns and
// the time it is allowed to take while returning them.

import (
	"fmt"
	"testing"
	"time"
)

const (
	// 50k rows is the smallest store on which the old shape reliably breaks the
	// budget below: measured here it needed ~10s while the rewrite stayed under
	// 50ms. At 20k rows the old query finished in 1.5s and the assertion passed,
	// which made the test a guard in name only.
	latestScaleDevices = 5
	latestScaleRows    = 10000
	latestQueryBudget  = 2 * time.Second
)

func seedLatestScaleStore(t *testing.T, ts *TimeSeriesStorage) {
	t.Helper()
	base := time.Now().Add(-latestScaleRows * time.Second)
	for d := 0; d < latestScaleDevices; d++ {
		deviceID := fmt.Sprintf("dev-%02d", d)
		batch := make([]PointData, 0, latestScaleRows)
		for i := 0; i < latestScaleRows; i++ {
			point := "temp"
			if i%2 == 1 {
				point = "pressure"
			}
			batch = append(batch, PointData{
				DeviceID:  deviceID,
				PointName: point,
				Value:     float64(i),
				Quality:   "good",
				Timestamp: base.Add(time.Duration(i) * time.Second),
			})
		}
		if err := ts.WritePoints(batch); err != nil {
			t.Fatalf("seed %s: %v", deviceID, err)
		}
	}
}

func TestGetLatestPointsReturnsTheNewestRowPerPoint(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()
	seedLatestScaleStore(t, ts)

	latest, err := ts.GetLatestPoints("dev-03")
	if err != nil {
		t.Fatalf("GetLatestPoints: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest = %#v, want one row per point", latest)
	}
	// Highest id wins, and the ids interleave between the two points.
	if latest["temp"].Value != float64(latestScaleRows-2) {
		t.Errorf("temp = %v, want the last temp sample (%d)", latest["temp"].Value, latestScaleRows-2)
	}
	if latest["pressure"].Value != float64(latestScaleRows-1) {
		t.Errorf("pressure = %v, want the last pressure sample (%d)", latest["pressure"].Value, latestScaleRows-1)
	}
	for name, p := range latest {
		if p.DeviceID != "dev-03" {
			t.Errorf("point %s came from %q, want only the requested device", name, p.DeviceID)
		}
	}
	if empty, err := ts.GetLatestPoints("dev-absent"); err != nil || len(empty) != 0 {
		t.Errorf("an unknown device answered %d rows/%v, want none", len(empty), err)
	}
}

func TestGetLatestPointsStaysWithinQueryBudget(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()
	seedLatestScaleStore(t, ts)

	// Warm the page cache so the budget measures the query, not the first read.
	if _, err := ts.GetLatestPoints("dev-00"); err != nil {
		t.Fatalf("warm-up read: %v", err)
	}
	started := time.Now()
	if _, err := ts.GetLatestPoints(fmt.Sprintf("dev-%02d", latestScaleDevices-1)); err != nil {
		t.Fatalf("GetLatestPoints: %v", err)
	}
	if elapsed := time.Since(started); elapsed > latestQueryBudget {
		t.Errorf("GetLatestPoints took %v on a %d-row table: the query is correlating against the whole store again",
			elapsed, latestScaleDevices*latestScaleRows)
	}
}

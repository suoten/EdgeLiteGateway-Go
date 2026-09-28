package drivers

import (
	"context"
	"testing"
	"time"

	"edgelite/internal/models"
)

// A driver is the only writer of the health counters the device panels read, so
// a driver that records nothing leaves /devices/:id/ops answering "no samples"
// for a device that has been collecting for hours. The simulator was also
// reporting its latency in the wrong unit while doing so: Milliseconds()
// truncated every in-memory read to a flat 0, and a Microseconds() value stored
// without dividing the unit out reads as a 500 ms link.
func TestSimulatorReadRecordsMeasuredLatency(t *testing.T) {
	const deviceID = "sim-latency"
	RegisterAll()
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	d, err := GetRegistry().CreateDriver("simulator", deviceID, map[string]interface{}{})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	ctx := context.Background()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("Connect returned an error: %v", err)
	}
	defer d.Disconnect()

	points := []models.PointDef{{Name: "p1", DataType: "float32", Address: "0"}}
	for i := 0; i < 3; i++ {
		values, err := d.ReadPoints(ctx, points)
		if err != nil {
			t.Fatalf("ReadPoints %d returned an error: %v", i, err)
		}
		if len(values) != 1 {
			t.Fatalf("ReadPoints %d returned %d values, want 1", i, len(values))
		}
	}

	st := hsm.GetHealthStats(deviceID)
	if st == nil {
		t.Fatal("the simulator recorded no health stats for its reads")
	}
	ctr := st.Counters()
	if ctr.TotalReads != 3 {
		t.Fatalf("total_reads = %d, want 3", ctr.TotalReads)
	}
	if !ctr.HasLatencySample {
		t.Fatal("has_latency_sample = false, want the reads to be sampled")
	}
	// A simulated read is faster than the platform clock can always resolve, so
	// the measured value may legitimately be 0. What must never happen is a
	// sample in the wrong unit -- microseconds reported as milliseconds put a
	// sub-microsecond read at 0.5 "ms" and a slow one at 500.
	if ctr.AvgLatencyMs < 0 || ctr.AvgLatencyMs > 50 {
		t.Errorf("avg_latency_ms = %v, want an in-memory read inside 50 ms", ctr.AvgLatencyMs)
	}
	if len(st.LatencySamples()) != 3 {
		t.Errorf("latency_history = %v, want one sample per read", st.LatencySamples())
	}
}

// TestElapsedMsIsInMilliseconds pins the unit every driver reports latency in.
func TestElapsedMsIsInMilliseconds(t *testing.T) {
	started := time.Now()
	time.Sleep(5 * time.Millisecond)
	elapsed := ElapsedMs(started)
	if elapsed < 5 || elapsed > 500 {
		t.Fatalf("ElapsedMs after a 5ms sleep = %v, want ~5 (a value in ms, not ns or µs)", elapsed)
	}
}

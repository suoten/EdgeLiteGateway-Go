package drivers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// Concurrency guards written as plain stress tests because the -race detector is
// unavailable in this environment (no C compiler / CGO disabled). The Go runtime
// still carries a built-in "concurrent map read and map write" / "concurrent map
// writes" detector that ABORTS the process unconditionally, independent of -race.
// So hammering each shared map from many goroutines catches exactly the class of
// data race a mutex-discipline bug introduces — which is what these tests lock in.
//
// The first test is a direct regression for the HealthStatsManager bug: the
// process-wide manager (globalHealthStats) is shared by every device, and
// RecordReadSuccess used to read m.offlineSince while holding only stats.mu,
// racing RecordReadFailure's locked write. That is a fatal concurrent-map access
// under real multi-device collection load.

func TestConcurrentStress_HealthStatsManager(t *testing.T) {
	mgr := NewHealthStatsManager()
	const devices = 8
	const perDevice = 8
	const iters = 1500
	var wg sync.WaitGroup
	for d := 0; d < devices; d++ {
		id := fmt.Sprintf("dev-%d", d)
		for g := 0; g < perDevice; g++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					// Alternate success/failure so the offlineSince window is
					// repeatedly written and read/deleted across goroutines.
					mgr.RecordReadSuccess(id, float64(i))
					mgr.RecordReadFailure(id)
					mgr.RecordWriteSuccess(id)
					mgr.RecordWriteFailure(id)
					if s := mgr.GetHealthStats(id); s != nil {
						_ = s.HealthScore()
						_ = s.ReadErrorRate()
						_ = s.P95LatencyMs()
					}
					_ = mgr.GetObservabilityMetrics(id)
					mgr.SetConnectionState(id, StateConnected, "up")
					mgr.SetConnectionState(id, StateDisconnected, "down")
					_ = mgr.GetConnectionStatus(id)
				}
			}(id)
		}
	}
	wg.Wait()
}

// TestConcurrentStress_HTTPWebhook drives the webhook driver's own maps
// (lastValues/lastQuality/lastStamp) from concurrent pushes and reads.
func TestConcurrentStress_HTTPWebhook(t *testing.T) {
	d, err := NewHTTPWebhookDriver("cs-wh", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wd := d.(*HTTPWebhookDriver)
	const writers = 12
	const readers = 12
	const iters = 1500
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				payload := fmt.Sprintf(`[{"point":"p%d","value":%d,"quality":"good"}]`, w, i)
				if err := wd.HandleWebhook([]byte(payload)); err != nil {
					t.Errorf("HandleWebhook: %v", err)
					return
				}
			}
		}(w)
	}
	pts := make([]models.PointDef, writers)
	for i := range pts {
		pts[i] = models.PointDef{Name: fmt.Sprintf("p%d", i)}
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, err := wd.ReadPoints(context.Background(), pts); err != nil {
					t.Errorf("ReadPoints: %v", err)
					return
				}
				_ = wd.HealthCheck(context.Background())
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentStress_ModbusSharedConn (live ProtoForge) fires concurrent
// ReadPoints/WritePoint on a single driver to prove the socket round-trip is
// serialized under d.mu (no torn MBAP frames) and that the shared circuit
// breaker / health counters survive the churn. Must not panic or deadlock.
func TestConcurrentStress_ModbusSharedConn(t *testing.T) {
	pfLive(t, "127.0.0.1:5020")
	d, err := NewModbusTCPDriver("cs-modbus", map[string]interface{}{
		"host": "127.0.0.1", "port": 5020, "slave_id": 1, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	const goroutines = 10
	const iters = 60
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine reads+writes its own unowned register band so the
			// written value is deterministic even with concurrent drivers.
			addr := fmt.Sprintf("HR%03d", 700+g)
			for i := 0; i < iters; i++ {
				if err := d.WritePoint(ctx, addr, uint16(1000+i)); err != nil {
					t.Errorf("WritePoint(%s): %v", addr, err)
					return
				}
				rows, err := d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
					{Name: "r", Address: addr, DataType: "uint16"},
				})
				if err != nil {
					t.Errorf("ReadPoints(%s): %v", addr, err)
					return
				}
				if len(rows) != 1 {
					t.Errorf("want 1 row, got %d", len(rows))
					return
				}
				// The value we just wrote must be readable on our own exclusive
				// band; a torn/interleaved frame under the socket mutex would
				// surface here as a mismatch or a bad row.
				if rows[0].Quality == "good" {
					if v, ok := rows[0].Value.(uint16); ok && v != uint16(1000+i) {
						t.Errorf("band %s read %d, want %d (torn/interleaved response?)", addr, v, 1000+i)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

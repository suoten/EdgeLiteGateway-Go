package api

import (
	"bytes"
	"fmt"
	"math"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/labstack/echo/v4"
)

// ============================================================================
// On-demand profiling: start/profile/history backed by runtime/pprof.
// ============================================================================

type profileRecord struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	Duration       int       `json:"duration"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
	GoroutineCount int       `json:"goroutine_count"`
	MemoryMB       float64   `json:"memory_mb"`
	Error          string    `json:"error,omitempty"`

	CPU        []map[string]interface{} `json:"cpu"`
	Memory     []map[string]interface{} `json:"memory"`
	Goroutines []map[string]interface{} `json:"goroutines"`
}

const maxProfileRecords = 50

var (
	profileMu      sync.Mutex
	profileRecords = map[string]*profileRecord{}
	profileOrder   []string
	profileRunning bool
)

func handleStartProfiler(c echo.Context) error {
	var req struct {
		Type     string `json:"type"`
		Duration int    `json:"duration"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "invalid request body")
	}
	if req.Type == "" {
		req.Type = "cpu"
	}
	switch req.Type {
	case "cpu", "memory", "goroutines", "all":
	default:
		return BadRequest(c, "invalid profile type: "+req.Type)
	}
	if req.Duration <= 0 {
		req.Duration = 30
	}
	if req.Duration < 5 || req.Duration > 300 {
		return BadRequest(c, "duration must be between 5 and 300 seconds")
	}

	profileMu.Lock()
	if profileRunning {
		profileMu.Unlock()
		return BadRequest(c, "a profile is already running")
	}
	profileRunning = true
	rec := &profileRecord{
		ID:         fmt.Sprintf("prof-%d", time.Now().UnixMilli()),
		Type:       req.Type,
		Duration:   req.Duration,
		Status:     "running",
		StartedAt:  time.Now(),
		CPU:        []map[string]interface{}{},
		Memory:     []map[string]interface{}{},
		Goroutines: []map[string]interface{}{},
	}
	profileRecords[rec.ID] = rec
	profileOrder = append([]string{rec.ID}, profileOrder...)
	if len(profileOrder) > maxProfileRecords {
		for _, old := range profileOrder[maxProfileRecords:] {
			delete(profileRecords, old)
		}
		profileOrder = profileOrder[:maxProfileRecords]
	}
	profileMu.Unlock()

	go runProfile(rec)
	return OK(c, map[string]interface{}{"id": rec.ID, "status": "running"})
}

func handleGetProfilerProfile(c echo.Context) error {
	id := c.Param("id")
	profileMu.Lock()
	defer profileMu.Unlock()
	rec := profileRecords[id]
	if rec == nil {
		return NotFound(c, "profile not found")
	}
	return OK(c, rec)
}

func handleGetProfilerHistory(c echo.Context) error {
	profileMu.Lock()
	defer profileMu.Unlock()
	items := make([]map[string]interface{}, 0, len(profileOrder))
	for _, id := range profileOrder {
		rec := profileRecords[id]
		items = append(items, map[string]interface{}{
			"id":              rec.ID,
			"type":            rec.Type,
			"duration":        rec.Duration,
			"status":          rec.Status,
			"started_at":      rec.StartedAt,
			"completed_at":    rec.CompletedAt,
			"goroutine_count": rec.GoroutineCount,
			"memory_mb":       round2(rec.MemoryMB),
		})
	}
	return OK(c, map[string]interface{}{"items": items, "total": len(items)})
}

func runProfile(rec *profileRecord) {
	defer func() {
		profileMu.Lock()
		profileRunning = false
		profileMu.Unlock()
	}()

	includes := func(t string) bool { return rec.Type == "all" || rec.Type == t }
	wait := time.Duration(rec.Duration) * time.Second

	var cpuBuf bytes.Buffer
	cpuOK := false
	if includes("cpu") {
		if err := pprof.StartCPUProfile(&cpuBuf); err != nil {
			profileMu.Lock()
			rec.Error = err.Error()
			profileMu.Unlock()
		} else {
			cpuOK = true
		}
	}
	time.Sleep(wait)
	if cpuOK {
		pprof.StopCPUProfile()
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	profileMu.Lock()
	rec.GoroutineCount = runtime.NumGoroutine()
	rec.MemoryMB = float64(mem.Alloc) / (1024 * 1024)
	profileMu.Unlock()

	if includes("memory") {
		var buf bytes.Buffer
		if err := pprof.Lookup("heap").WriteTo(&buf, 0); err == nil {
			if rows := aggregateHeapProfile(buf.Bytes()); rows != nil {
				profileMu.Lock()
				rec.Memory = rows
				profileMu.Unlock()
			}
		}
	}
	if includes("goroutines") {
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, 0); err == nil {
			if rows := aggregateGoroutineProfile(buf.Bytes()); rows != nil {
				profileMu.Lock()
				rec.Goroutines = rows
				profileMu.Unlock()
			}
		}
	}
	if includes("cpu") && cpuBuf.Len() > 0 {
		if rows := aggregateCPUProfile(cpuBuf.Bytes(), wait); rows != nil {
			profileMu.Lock()
			rec.CPU = rows
			profileMu.Unlock()
		}
	}

	profileMu.Lock()
	rec.CompletedAt = time.Now()
	rec.Status = "completed"
	profileMu.Unlock()
}

func leafFunction(locs []*profile.Location) string {
	for i := len(locs) - 1; i >= 0; i-- {
		for _, line := range locs[i].Line {
			if line.Function != nil && line.Function.Name != "" {
				return line.Function.Name
			}
		}
	}
	return "unknown"
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func aggregateCPUProfile(data []byte, total time.Duration) []map[string]interface{} {
	p, err := profile.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	type agg struct {
		samples int64
		cpuNs   int64
	}
	byFn := map[string]*agg{}
	var totalCounts int64
	period := p.Period
	if period <= 0 {
		period = 1
	}
	for _, s := range p.Sample {
		fn := leafFunction(s.Location)
		a := byFn[fn]
		if a == nil {
			a = &agg{}
			byFn[fn] = a
		}
		a.samples++
		if len(s.Value) > 0 {
			a.cpuNs += s.Value[0] * period
			totalCounts += s.Value[0]
		}
	}
	if totalCounts == 0 {
		totalCounts = 1
	}
	rows := make([]map[string]interface{}, 0, len(byFn))
	for fn, a := range byFn {
		pct := float64(a.cpuNs) / (float64(totalCounts) * float64(period)) * 100
		rows = append(rows, map[string]interface{}{
			"function":    fn,
			"samples":     a.samples,
			"cpu_pct":     round2(pct),
			"duration_ms": round2(float64(a.cpuNs) / 1e6),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["cpu_pct"].(float64) > rows[j]["cpu_pct"].(float64)
	})
	if len(rows) > 100 {
		rows = rows[:100]
	}
	return rows
}

func aggregateHeapProfile(data []byte) []map[string]interface{} {
	p, err := profile.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	type agg struct {
		allocCount int64
		allocBytes int64
		inuseBytes int64
	}
	byFn := map[string]*agg{}
	for _, s := range p.Sample {
		fn := leafFunction(s.Location)
		a := byFn[fn]
		if a == nil {
			a = &agg{}
			byFn[fn] = a
		}
		if len(s.Value) > 0 {
			a.allocCount += s.Value[0]
		}
		if len(s.Value) > 1 {
			a.allocBytes += s.Value[1]
		}
		if len(s.Value) > 3 {
			a.inuseBytes += s.Value[3]
		}
	}
	rows := make([]map[string]interface{}, 0, len(byFn))
	for fn, a := range byFn {
		rows = append(rows, map[string]interface{}{
			"function":    fn,
			"alloc_count": a.allocCount,
			"alloc_mb":    round2(float64(a.allocBytes) / (1024 * 1024)),
			"live_mb":     round2(float64(a.inuseBytes) / (1024 * 1024)),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["alloc_mb"].(float64) > rows[j]["alloc_mb"].(float64)
	})
	if len(rows) > 100 {
		rows = rows[:100]
	}
	return rows
}

func aggregateGoroutineProfile(data []byte) []map[string]interface{} {
	p, err := profile.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	type agg struct {
		count int64
		leaf  string
	}
	bySig := map[string]*agg{}
	for _, s := range p.Sample {
		names := make([]string, 0, len(s.Location))
		for i := len(s.Location) - 1; i >= 0; i-- {
			for _, line := range s.Location[i].Line {
				if line.Function != nil && line.Function.Name != "" {
					names = append(names, line.Function.Name)
				}
			}
		}
		sig := "unknown"
		if len(names) > 0 {
			if len(names) > 5 {
				sig = strings.Join(names[:5], "|")
			} else {
				sig = strings.Join(names, "|")
			}
		}
		a := bySig[sig]
		if a == nil {
			a = &agg{leaf: leafFunction(s.Location)}
			bySig[sig] = a
		}
		if len(s.Value) > 0 {
			a.count += s.Value[0]
		}
	}
	rows := make([]map[string]interface{}, 0, len(bySig))
	for sig, a := range bySig {
		_ = sig
		rows = append(rows, map[string]interface{}{
			"state":    goroutineState(sig),
			"function": a.leaf,
			"count":    a.count,
			"duration": "-",
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["count"].(int64) > rows[j]["count"].(int64)
	})
	return rows
}

func goroutineState(sig string) string {
	switch {
	case strings.Contains(sig, "time.Sleep"):
		return "sleeping"
	case strings.Contains(sig, "runtime.gopark"), strings.Contains(sig, "selectgo"),
		strings.Contains(sig, "runtime_pollWait"), strings.Contains(sig, "accept"):
		return "waiting"
	default:
		return "running"
	}
}

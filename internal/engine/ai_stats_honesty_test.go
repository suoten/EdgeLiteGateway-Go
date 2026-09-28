package engine

import (
	"testing"

	"edgelite/internal/config"
)

// GetStats used to answer avg_latency_ms 0 whenever nothing had run, and the
// dashboard rendered that as "instant" beside a green engine badge. A failed
// inference incremented the error counter without ever producing a sample, so
// the average has to stay null until a real measurement exists.

func newDeadSidecarEngine() *AIInferenceEngine {
	return NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              "http://127.0.0.1:1",
		MaxConcurrentInferences: 1,
	}, nil)
}

func statsFloat(t *testing.T, s map[string]interface{}, key string) (float64, bool) {
	t.Helper()
	v, ok := s[key]
	if !ok || v == nil {
		return 0, false
	}
	f, isNum := v.(float64)
	if !isNum {
		t.Fatalf("%s = %#v, want a number or nil", key, v)
	}
	return f, true
}

func TestAIStatsLatencyNullUntilFirstSample(t *testing.T) {
	e := newDeadSidecarEngine()

	stats := e.GetStats()
	if _, ok := statsFloat(t, stats, "avg_latency_ms"); ok {
		t.Fatal("avg_latency_ms must be null before any inference has run")
	}
	if _, ok := statsFloat(t, stats, "avg_latency_s"); ok {
		t.Fatal("avg_latency_s must be null before any inference has run")
	}
	if got, _ := stats["total_calls"].(int64); got != 0 {
		t.Fatalf("total_calls = %v, want 0", stats["total_calls"])
	}
	// The card reads model_count; omitting it in the sidecar-down branch left it
	// pinned at 0 even though the engine lists its presets.
	if got, _ := stats["model_count"].(int); got != len(builtinPresets) {
		t.Fatalf("model_count = %v, want the %d presets the engine lists", stats["model_count"], len(builtinPresets))
	}
	if got, _ := stats["sidecar"].(string); got != "unavailable" {
		t.Fatalf("sidecar = %v, want unavailable", stats["sidecar"])
	}

	if _, err := e.RunInference("preset-anomaly-v1", map[string]interface{}{
		"input_data": []float64{1, 2, 3},
	}); err == nil {
		t.Fatal("expected inference against a closed port to fail")
	}

	stats = e.GetStats()
	if got, _ := stats["total_errors"].(int64); got != 1 {
		t.Fatalf("total_errors = %v, want 1", stats["total_errors"])
	}
	if _, ok := statsFloat(t, stats, "avg_latency_ms"); ok {
		t.Fatalf("avg_latency_ms = %#v after a failed run, want null: failures are not latency samples", stats["avg_latency_ms"])
	}
	if got, _ := stats["total_calls"].(int64); got != 0 {
		t.Fatalf("total_calls = %v, want 0 (the run failed)", stats["total_calls"])
	}
}

// The local fallback accumulates seconds (RunInference divides LatencyMs by
// 1000), so a sample has to come back out as milliseconds, not microseconds.
func TestAIStatsLocalLatencyIsMilliseconds(t *testing.T) {
	e := newDeadSidecarEngine()
	e.muStats.Lock()
	e.totalInferences = 2
	e.totalLatencySum = 1.5 // 2 runs averaging 750 ms
	e.muStats.Unlock()

	stats := e.GetStats()
	avg, ok := statsFloat(t, stats, "avg_latency_ms")
	if !ok {
		t.Fatal("avg_latency_ms must be a number once samples exist")
	}
	if avg < 749 || avg > 751 {
		t.Fatalf("avg_latency_ms = %v, want ~750", avg)
	}
	secs, ok := statsFloat(t, stats, "avg_latency_s")
	if !ok || secs < 0.749 || secs > 0.751 {
		t.Fatalf("avg_latency_s = %#v, want ~0.75", stats["avg_latency_s"])
	}
}

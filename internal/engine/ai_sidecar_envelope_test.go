package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"edgelite/internal/config"
)

// The Python sidecar answers business failures with HTTP 200 and
// {success:false, error_code, error_message}. Until now the Go client only
// looked at the status line, so a rejected schedule (or an unloaded model)
// arrived here as "no error" and the gateway recorded a loop that never ran —
// turning the frontend's lie into a backend one.

type sidecarCall struct {
	Path string
	Body map[string]interface{}
}

// newFakeSidecar returns a sidecar stub that answers every POST with resp and
// records each call. /stats and /health get their own bodies so the stub looks
// like the real service shape.
func newFakeSidecar(t *testing.T, resp map[string]interface{}, stats map[string]interface{}) (*httptest.Server, *[]sidecarCall) {
	t.Helper()
	var calls []sidecarCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		var body map[string]interface{}
		_ = dec.Decode(&body)
		if body == nil {
			body = map[string]interface{}{}
		}
		calls = append(calls, sidecarCall{Path: r.URL.Path, Body: body})
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/stats":
			_ = json.NewEncoder(w).Encode(stats)
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newSidecarEngine(url string) *AIInferenceEngine {
	return NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              url,
		MaxConcurrentInferences: 1,
	}, nil)
}

func TestScheduledStartRecordsOnlyAcceptedLoops(t *testing.T) {
	srv, calls := newFakeSidecar(t, map[string]interface{}{"success": true}, map[string]interface{}{})
	eng := newSidecarEngine(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := eng.StartScheduledInference(ctx, "m-accept", "dev-1", "temp", 30, 10); err != nil {
		t.Fatalf("accepted schedule returned an error: %v", err)
	}
	got := eng.ListScheduledInferences()
	if len(got) != 1 {
		t.Fatalf("list = %#v, want exactly the accepted loop", got)
	}
	if got[0].ModelID != "m-accept" || got[0].DeviceID != "dev-1" || got[0].PointName != "temp" || got[0].Interval != 30 || got[0].WindowSize != 10 {
		t.Fatalf("record does not mirror the request: %#v", got[0])
	}
	if len(*calls) == 0 || (*calls)[len(*calls)-1].Path != "/scheduled/start" {
		t.Fatalf("the sidecar was never asked to start the loop: %#v", *calls)
	}
	if (*calls)[len(*calls)-1].Body["model_id"] != "m-accept" {
		t.Fatalf("sidecar got the wrong model_id: %#v", (*calls)[len(*calls)-1].Body)
	}

	if err := eng.StopScheduledInference(ctx, "m-accept"); err != nil {
		t.Fatalf("accepted stop returned an error: %v", err)
	}
	if left := eng.ListScheduledInferences(); len(left) != 0 {
		t.Fatalf("list still holds %d entries after a confirmed stop: %#v", len(left), left)
	}
}

func TestScheduledStartSidecarRejectionIsNotRecorded(t *testing.T) {
	// HTTP 200 + success:false is how the real sidecar says "no".
	srv, _ := newFakeSidecar(t, map[string]interface{}{
		"success":       false,
		"error_code":    "ERR_MODEL_NOT_LOADED",
		"error_message": "model m-reject is not loaded",
	}, map[string]interface{}{})
	eng := newSidecarEngine(srv.URL)

	err := eng.StartScheduledInference(context.Background(), "m-reject", "", "", 5, 5)
	if err == nil {
		t.Fatal("a sidecar rejection at HTTP 200 was treated as success")
	}
	if got := eng.ListScheduledInferences(); len(got) != 0 {
		t.Fatalf("phantom loop recorded after a rejection: %#v (err=%v)", got, err)
	}

	if err := eng.StopScheduledInference(context.Background(), "m-reject"); err == nil {
		t.Fatal("a rejected stop was treated as success")
	}
}

func TestSidecarErrorIgnoresNonEnvelopes(t *testing.T) {
	// /health and /stats carry no success field; treating their absence as a
	// failure would break the stats and health endpoints the UI polls.
	for name, body := range map[string]map[string]interface{}{
		"health":   {"status": "ok", "version": "1.17.0"},
		"stats":    {"stats": map[string]interface{}{"total_calls": 3}},
		"accepted": {"success": true},
	} {
		if err := sidecarError(body); err != nil {
			t.Fatalf("%s: sidecarError() = %v, want nil", name, err)
		}
	}
	err := sidecarError(map[string]interface{}{"success": false})
	if err == nil {
		t.Fatal("success:false with no error code produced no error")
	}
}

func TestGetStatsWithoutStatsBlockFallsBack(t *testing.T) {
	// The stub answers /stats with {} — the old client returned (nil, nil) and
	// the engine dereferenced it.
	srv, _ := newFakeSidecar(t, map[string]interface{}{"success": true}, map[string]interface{}{})
	eng := newSidecarEngine(srv.URL)
	stats := eng.GetStats()
	if stats == nil {
		t.Fatal("GetStats() returned nil instead of the local fallback")
	}
	if v, ok := stats["avg_latency_ms"]; ok && v != nil {
		t.Fatalf("avg_latency_ms = %#v with no samples, want null", v)
	}
}

func TestModelMutationCallsSurfaceSidecarRejection(t *testing.T) {
	srv, _ := newFakeSidecar(t, map[string]interface{}{
		"success":       false,
		"error_code":    "ERR_MODEL_BUSY",
		"error_message": "model in use",
	}, map[string]interface{}{})
	eng := newSidecarEngine(srv.URL)

	for name, call := range map[string]func() error{
		"unload":  func() error { return eng.UnloadModel("m-1") },
		"remove":  func() error { return eng.RemoveModel("m-1") },
		"enable":  func() error { return eng.EnableModel("m-1") },
		"disable": func() error { return eng.DisableModel("m-1") },
		"reload":  func() error { return eng.ReloadModelFrom("m-1", "models/m-1.onnx") },
		"load":    func() error { return eng.LoadModelFile("m-1", "models/m-1.onnx", "anomaly") },
	} {
		if err := call(); err == nil {
			t.Fatalf("%s: reported success for a sidecar rejection", name)
		}
	}
}

func TestModelMutationCallsAcceptConfirmedSidecar(t *testing.T) {
	srv, calls := newFakeSidecar(t, map[string]interface{}{
		"success":     true,
		"new_version": "v1.0.1",
		"model_info":  map[string]interface{}{"model_id": "m-1", "status": "active"},
	}, map[string]interface{}{})
	eng := newSidecarEngine(srv.URL)

	if err := eng.UnloadModel("m-1"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if err := eng.RemoveModel("m-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := eng.EnableModel("m-1"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := eng.DisableModel("m-1"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := eng.ReloadModelFrom("m-1", "models/m-1.onnx"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := eng.LoadModelFile("m-1", "models/m-1.onnx", "anomaly"); err != nil {
		t.Fatalf("load: %v", err)
	}
	// ReloadModelFrom must forward the path the UI supplied; the handler used to
	// bind it and discard it.
	var sawPath interface{}
	for _, c := range *calls {
		if c.Path == "/models/reload" {
			sawPath = c.Body["model_path"]
		}
	}
	if sawPath != "models/m-1.onnx" {
		t.Fatalf("reload sent model_path %#v, want the path the caller passed", sawPath)
	}
}

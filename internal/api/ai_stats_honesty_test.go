package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// /ai/stats, /ai/summary and /ai/inference/logs answered 200 with fabricated
// shapes: zeros that an idle-but-healthy engine also produces, and an empty
// log page for a build that has no inference store at all. Each handler now
// distinguishes "measured nothing" from "cannot measure".

func callAI(t *testing.T, h func(echo.Context) error, method, path, body string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

func useAIContainer(t *testing.T, eng *engine.AIInferenceEngine) {
	t.Helper()
	cont := GetContainer()
	prev := cont.AIInference
	cont.AIInference = eng
	t.Cleanup(func() { cont.AIInference = prev })
}

func newNoSidecarEngine() *engine.AIInferenceEngine {
	return engine.NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              "http://127.0.0.1:1",
		MaxConcurrentInferences: 1,
	}, nil)
}

func TestAIStatsWithoutEngineIs503(t *testing.T) {
	useAIContainer(t, nil)
	for name, h := range map[string]func(echo.Context) error{
		"stats":   handleGetAIStats,
		"summary": handleGetAISummary,
	} {
		code, ec, data := callAI(t, h, http.MethodGet, "/api/v1/ai/"+name, "")
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, code)
		}
		if ec != "ERR_AI_ENGINE_NOT_INITIALIZED" {
			t.Fatalf("%s: error_code = %q, want ERR_AI_ENGINE_NOT_INITIALIZED", name, ec)
		}
		if data != nil {
			t.Fatalf("%s: 503 must not carry a stats object, got %#v", name, data)
		}
	}
}

func TestAIStatsNullLatencyAndRealModelCount(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())

	code, _, data := callAI(t, handleGetAIStats, http.MethodGet, "/api/v1/ai/stats", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the engine up and the sidecar down", code)
	}
	if v, ok := data["avg_latency_ms"]; ok && v != nil {
		t.Fatalf("avg_latency_ms = %#v with zero samples, want null", v)
	}
	if _, ok := data["avg_latency_ms"]; !ok {
		t.Fatal("avg_latency_ms key missing: the card cannot tell null from absent")
	}
	// The engine lists its 3 built-in presets while the sidecar is down; that is
	// exactly what /ai/models shows in the same state.
	if got, _ := data["model_count"].(float64); got != 3 {
		t.Fatalf("model_count = %#v, want 3 presets", data["model_count"])
	}
	if _, ok := data["model_count"]; !ok {
		t.Fatal("model_count missing: the dashboard card read it as 0 forever")
	}
}

func TestInferenceLogsUnsupportedIs501(t *testing.T) {
	code, ec, _ := callAI(t, handleGetInferenceLogs, http.MethodGet, "/api/v1/ai/inference/logs", "")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (no inference store in this build)", code)
	}
	if ec != "ERR_AI_INFERENCE_LOGS_UNSUPPORTED" {
		t.Fatalf("error_code = %q, want ERR_AI_INFERENCE_LOGS_UNSUPPORTED", ec)
	}
}

func TestExecutionProvidersAskTheSidecar(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())

	// ["CPU"] used to be returned unconditionally, so the UI showed a provider
	// list even with the sidecar unreachable.
	code, ec, _ := callAI(t, handleGetExecutionProviders, http.MethodGet, "/api/v1/ai/execution-providers", "")
	if code != http.StatusServiceUnavailable || ec != "ERR_AI_SIDECAR_UNAVAILABLE" {
		t.Fatalf("providers: status = %d error_code = %q, want 503/ERR_AI_SIDECAR_UNAVAILABLE", code, ec)
	}

	code, ec, _ = callAI(t, handleSetExecutionProvider, http.MethodPost, "/api/v1/ai/execution-provider", `{"provider":"CUDA"}`)
	if code != http.StatusServiceUnavailable || ec != "ERR_AI_SIDECAR_UNAVAILABLE" {
		t.Fatalf("set: status = %d error_code = %q, want 503/ERR_AI_SIDECAR_UNAVAILABLE (no fake success)", code, ec)
	}

	code, ec, _ = callAI(t, handleSetExecutionProvider, http.MethodPost, "/api/v1/ai/execution-provider", `{}`)
	if code != http.StatusBadRequest || ec != "ERR_AI_PROVIDER_REQUIRED" {
		t.Fatalf("empty provider: status = %d error_code = %q, want 400/ERR_AI_PROVIDER_REQUIRED", code, ec)
	}
}

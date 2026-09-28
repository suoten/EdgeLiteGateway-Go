package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// The AI version routes reported work that no component did: rollback answered
// status:"rolled_back" after binding the body and touching nothing, and the
// version list answered an empty array without asking the sidecar, so the
// versions dialog showed "no earlier versions" for a sidecar that was never
// contacted. These tests pin both to the sidecar's own verdict.

// aiStub answers the sidecar's routes from a fixture table keyed by
// "METHOD path". The reply it sent for the last request is handed back over a
// buffered channel, so reading it is synchronised with the server goroutine.
func aiStub(t *testing.T, replies map[string]interface{}) (*engine.AIInferenceEngine, chan string) {
	t.Helper()
	recorded := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		recorded <- key + "|" + string(body)
		w.Header().Set("Content-Type", "application/json")
		reply, ok := replies[key]
		if !ok {
			reply = replies[r.URL.Path]
		}
		if reply == nil {
			reply = map[string]interface{}{"success": true}
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	eng := engine.NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              srv.URL,
		MaxConcurrentInferences: 1,
	}, nil)
	return eng, recorded
}

func callAIVersionHandler(t *testing.T, h func(echo.Context) error, method, path, body, modelID string) (int, string, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if modelID != "" {
		c.SetParamNames("model_id")
		c.SetParamValues(modelID)
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Message   string                 `json:"message"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body.String(), err)
	}
	return rec.Code, env.ErrorCode, env.Message, env.Data
}

func waitForSidecarRequest(t *testing.T, recorded chan string) string {
	t.Helper()
	select {
	case got := <-recorded:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("the sidecar was never asked")
		return ""
	}
}

func TestRollbackByIDAsksTheSidecarForTheRequestedVersion(t *testing.T) {
	eng, recorded := aiStub(t, map[string]interface{}{
		"POST /models/rollback": map[string]interface{}{"success": true},
	})
	useAIContainer(t, eng)

	code, errCode, _, data := callAIVersionHandler(t, handleRollbackAIModelByID,
		http.MethodPost, "/api/v1/ai/models/m-1/rollback", `{"target_version":"1.2.0"}`, "m-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d code = %q, want 200 (%v)", code, errCode, data)
	}
	if data["status"] != "rolled_back" {
		t.Fatalf("status field = %v, want rolled_back", data["status"])
	}

	asked := waitForSidecarRequest(t, recorded)
	if asked != `POST /models/rollback|{"model_id":"m-1","target_version":"1.2.0"}` {
		t.Fatalf("sidecar request = %s, want a rollback of m-1 to 1.2.0", asked)
	}
}

// A sidecar that refuses (unknown model, unknown version) answers HTTP 200 with
// success:false. Reporting that as a rolled-back model is the same fake success
// the handlers used to produce on their own.
func TestRollbackReportsTheSidecarsRefusalInsteadOfSuccess(t *testing.T) {
	eng, recorded := aiStub(t, map[string]interface{}{
		"POST /models/rollback": map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_AI_VERSION_NOT_FOUND",
			"error_message": "Version not found",
		},
	})
	useAIContainer(t, eng)

	code, errCode, msg, _ := callAIVersionHandler(t, handleRollbackAIModelByID,
		http.MethodPost, "/api/v1/ai/models/m-1/rollback", `{"target_version":"9.9.9"}`, "m-1")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a refusal is not an outage and not a success)", code)
	}
	if errCode != "ERR_AI_ROLLBACK_REJECTED" {
		t.Fatalf("error_code = %q, want ERR_AI_ROLLBACK_REJECTED", errCode)
	}
	if msg == "" {
		t.Fatal("the sidecar's reason must reach the caller")
	}
	waitForSidecarRequest(t, recorded)
}

func TestRollbackWithoutASidecarSaysSo(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())

	code, errCode, _, _ := callAIVersionHandler(t, handleRollbackAIModelByID,
		http.MethodPost, "/api/v1/ai/models/m-1/rollback", `{"target_version":"1.2.0"}`, "m-1")
	if code != http.StatusServiceUnavailable || errCode != "ERR_AI_SIDECAR_UNAVAILABLE" {
		t.Fatalf("status = %d code = %q, want 503 ERR_AI_SIDECAR_UNAVAILABLE", code, errCode)
	}
}

func TestRollbackRejectsAnIncompleteRequest(t *testing.T) {
	eng, _ := aiStub(t, nil)
	useAIContainer(t, eng)

	code, errCode, _, _ := callAIVersionHandler(t, handleRollbackAIModelByID,
		http.MethodPost, "/api/v1/ai/models/m-1/rollback", `{}`, "m-1")
	if code != http.StatusBadRequest || errCode != "ERR_AI_ROLLBACK_TARGET_REQUIRED" {
		t.Fatalf("missing target_version: status = %d code = %q, want 400 ERR_AI_ROLLBACK_TARGET_REQUIRED", code, errCode)
	}

	// The legacy route takes the model from the body, so an empty one must not
	// roll back whichever model happens to answer.
	code, errCode, _, _ = callAIVersionHandler(t, handleRollbackAIModel,
		http.MethodPost, "/api/v1/ai/rollback", `{"target_version":"1.2.0"}`, "")
	if code != http.StatusBadRequest || errCode != "ERR_AI_MODEL_ID_REQUIRED" {
		t.Fatalf("missing model_id: status = %d code = %q, want 400 ERR_AI_MODEL_ID_REQUIRED", code, errCode)
	}
}

func TestModelVersionHistoryComesFromTheSidecar(t *testing.T) {
	eng, recorded := aiStub(t, map[string]interface{}{
		"/models/m-1/history": map[string]interface{}{
			"history": []interface{}{
				map[string]interface{}{"version": "1.0.0"},
				map[string]interface{}{"version": "1.1.0"},
			},
		},
	})
	useAIContainer(t, eng)

	c, rec := setupEcho(http.MethodGet, "/api/v1/ai/models/m-1/versions", "")
	c.SetParamNames("model_id")
	c.SetParamValues("m-1")
	if err := handleGetAIModelHistory(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body.String(), err)
	}
	if len(env.Data) != 2 || env.Data[0]["version"] != "1.0.0" {
		t.Fatalf("history = %v, want the two versions the sidecar listed", env.Data)
	}
	if asked := waitForSidecarRequest(t, recorded); asked != "GET /models/m-1/history|" {
		t.Fatalf("sidecar request = %q, want the history of m-1", asked)
	}
}

// An empty history is a real answer; not asking anybody is not. With no sidecar
// reachable the handler used to return [] and the dialog looked settled.
func TestModelVersionHistoryWithoutASidecarIsNotAnEmptyList(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())

	code, errCode, _, _ := callAIVersionHandler(t, handleGetAIModelHistory,
		http.MethodGet, "/api/v1/ai/models/m-1/versions", "", "m-1")
	if code != http.StatusServiceUnavailable || errCode != "ERR_AI_SIDECAR_UNAVAILABLE" {
		t.Fatalf("status = %d code = %q, want 503 ERR_AI_SIDECAR_UNAVAILABLE", code, errCode)
	}

	useAIContainer(t, nil)
	code, errCode, _, _ = callAIVersionHandler(t, handleGetAIModelHistory,
		http.MethodGet, "/api/v1/ai/models/m-1/versions", "", "m-1")
	if code != http.StatusServiceUnavailable || errCode != "ERR_AI_ENGINE_NOT_INITIALIZED" {
		t.Fatalf("without an engine: status = %d code = %q, want 503 ERR_AI_ENGINE_NOT_INITIALIZED", code, errCode)
	}
}

// A sidecar that answers without a history list at all said something else; the
// client used to translate that into an empty one, which the dialog then reported
// as "this model has no earlier versions".
func TestModelVersionHistoryTreatsAMissingListAsNoAnswer(t *testing.T) {
	eng, recorded := aiStub(t, map[string]interface{}{
		"/models/m-1/history": map[string]interface{}{"success": true},
	})
	useAIContainer(t, eng)

	code, errCode, _, _ := callAIVersionHandler(t, handleGetAIModelHistory,
		http.MethodGet, "/api/v1/ai/models/m-1/versions", "", "m-1")
	if code != http.StatusServiceUnavailable || errCode != "ERR_AI_SIDECAR_UNAVAILABLE" {
		t.Fatalf("status = %d code = %q, want 503 ERR_AI_SIDECAR_UNAVAILABLE for an answer with no list", code, errCode)
	}
	waitForSidecarRequest(t, recorded)
}

func TestAIResourcesReportsGPUOnlyWhatTheSidecarSays(t *testing.T) {
	cases := map[string]struct {
		provider interface{}
		want     interface{}
	}{
		"cpu":          {"CPU", false},
		"cuda":         {"CUDA", true},
		"unknown":      {"OpenVINO", nil},
		"not reported": {nil, nil},
	}
	for name, tc := range cases {
		reply := map[string]interface{}{"healthy": true}
		if tc.provider != nil {
			reply["execution_provider"] = tc.provider
		}
		eng, _ := aiStub(t, map[string]interface{}{"/health": reply})
		useAIContainer(t, eng)

		code, errCode, _, data := callAIVersionHandler(t, handleGetAIResources,
			http.MethodGet, "/api/v1/ai/resources", "", "")
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d code = %q, want 200", name, code, errCode)
		}
		if data["gpu_available"] != tc.want {
			t.Errorf("%s: gpu_available = %v, want %v", name, data["gpu_available"], tc.want)
		}
		if _, ok := data["cpu_usage"].(float64); !ok {
			t.Errorf("%s: cpu_usage = %v, want a measured percentage", name, data["cpu_usage"])
		}
		if _, ok := data["memory_usage"].(float64); !ok {
			t.Errorf("%s: memory_usage = %v, want a measured percentage", name, data["memory_usage"])
		}
	}

	// With no engine at all the host percentages are still real, but the GPU
	// question has nobody to answer it.
	useAIContainer(t, nil)
	_, _, _, data := callAIVersionHandler(t, handleGetAIResources,
		http.MethodGet, "/api/v1/ai/resources", "", "")
	if data["gpu_available"] != nil {
		t.Fatalf("gpu_available = %v without a sidecar, want null", data["gpu_available"])
	}
}

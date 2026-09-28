package api

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// The model lifecycle endpoints answered 200 with status=unloaded / enabled /
// disabled / removed / updated without the sidecar being asked anything, and
// upload saved a file no engine ever loaded. These tests pin each handler to
// the sidecar's actual verdict.

func newSidecarStub(t *testing.T, resp map[string]interface{}) (*engine.AIInferenceEngine, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/models/load" {
			body := map[string]interface{}{"success": true, "model_info": map[string]interface{}{"model_id": "m-1", "status": "active"}}
			for k, v := range resp {
				body[k] = v
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	eng := engine.NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              srv.URL,
		MaxConcurrentInferences: 1,
	}, nil)
	return eng, &paths
}

func callWithParam(t *testing.T, h func(echo.Context) error, method, path, body, name, value string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	c.SetParamNames(name)
	c.SetParamValues(value)
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

func TestModelLifecycleWithoutEngineIs503(t *testing.T) {
	useAIContainer(t, nil)
	type call struct {
		name   string
		h      func(echo.Context) error
		method string
		path   string
		body   string
		param  bool
	}
	calls := []call{
		{"unload", handleUnloadAIModel, http.MethodPost, "/api/v1/ai/unload", `{"model_id":"m-1"}`, false},
		{"enable", handleEnableAIModel, http.MethodPost, "/api/v1/ai/enable", `{"model_id":"m-1"}`, false},
		{"disable", handleDisableAIModel, http.MethodPost, "/api/v1/ai/disable", `{"model_id":"m-1"}`, false},
		{"remove", handleRemoveAIModel, http.MethodPost, "/api/v1/ai/models/m-1", "", true},
		{"reload", handleReloadAIModelByID, http.MethodPost, "/api/v1/ai/models/m-1/reload", `{}`, true},
		{"enableById", handleEnableAIModelByID, http.MethodPost, "/api/v1/ai/models/m-1/enable", "", true},
		{"disableById", handleDisableAIModelByID, http.MethodPost, "/api/v1/ai/models/m-1/disable", "", true},
		{"startSchedule", handleStartScheduledInferenceByID, http.MethodPost, "/api/v1/ai/models/m-1/schedule", `{"interval":30}`, true},
		{"stopSchedule", handleStopScheduledInferenceByID, http.MethodPost, "/api/v1/ai/models/m-1/schedule", "", true},
		{"listSchedules", handleListScheduledInferences, http.MethodGet, "/api/v1/ai/schedules", "", false},
	}
	for _, cl := range calls {
		var code int
		var ec string
		if cl.param {
			code, ec, _ = callWithParam(t, cl.h, cl.method, cl.path, cl.body, "model_id", "m-1")
		} else {
			code, ec, _ = callAI(t, cl.h, cl.method, cl.path, cl.body)
		}
		if code != http.StatusServiceUnavailable || ec != "ERR_AI_ENGINE_NOT_INITIALIZED" {
			t.Fatalf("%s: status = %d error_code = %q, want 503/ERR_AI_ENGINE_NOT_INITIALIZED", cl.name, code, ec)
		}
	}
}

func TestModelLifecycleReportsSidecarRejection(t *testing.T) {
	eng, _ := newSidecarStub(t, map[string]interface{}{
		"success":       false,
		"error_code":    "ERR_AI_MODEL_NOT_FOUND",
		"error_message": "Model not found",
	})
	useAIContainer(t, eng)

	// A sidecar that has never heard of the model must not let the gateway
	// claim the model was unloaded / disabled / removed.
	cases := []struct {
		name   string
		h      func(echo.Context) error
		body   string
		want   string
		param  bool
		method string
	}{
		{"unload", handleUnloadAIModel, `{"model_id":"m-1"}`, "ERR_AI_MODEL_UNLOAD_FAILED", false, http.MethodPost},
		{"disable", handleDisableAIModel, `{"model_id":"m-1"}`, "ERR_AI_MODEL_DISABLE_FAILED", false, http.MethodPost},
		{"enable", handleEnableAIModel, `{"model_id":"m-1"}`, "ERR_AI_MODEL_ENABLE_FAILED", false, http.MethodPost},
		{"remove", handleRemoveAIModel, "", "ERR_AI_MODEL_REMOVE_FAILED", true, http.MethodPost},
		{"reload", handleReloadAIModelByID, `{}`, "ERR_AI_MODEL_RELOAD_FAILED", true, http.MethodPost},
	}
	for _, cs := range cases {
		var code int
		var ec string
		if cs.param {
			code, ec, _ = callWithParam(t, cs.h, cs.method, "/api/v1/ai/models/m-1", cs.body, "model_id", "m-1")
		} else {
			code, ec, _ = callAI(t, cs.h, cs.method, "/api/v1/ai/"+cs.name, cs.body)
		}
		if code != http.StatusBadGateway || ec != cs.want {
			t.Fatalf("%s: status = %d error_code = %q, want 502/%s", cs.name, code, ec, cs.want)
		}
	}
}

func TestModelLifecyclePassesSidecarSuccess(t *testing.T) {
	eng, paths := newSidecarStub(t, map[string]interface{}{"success": true})
	useAIContainer(t, eng)

	code, _, data := callAI(t, handleUnloadAIModel, http.MethodPost, "/api/v1/ai/unload", `{"model_id":"m-1"}`)
	if code != http.StatusOK || data["status"] != "unloaded" {
		t.Fatalf("unload: status = %d data = %#v, want 200/unloaded", code, data)
	}
	// The toast is only honest if the sidecar was actually called.
	found := false
	for _, p := range *paths {
		if p == "/models/unload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the sidecar never saw a unload request: %#v", *paths)
	}
}

func TestPresetModelCannotBeRemovedThroughTheAPI(t *testing.T) {
	eng, _ := newSidecarStub(t, map[string]interface{}{"success": true})
	useAIContainer(t, eng)

	code, ec, _ := callWithParam(t, handleRemoveAIModel, http.MethodPost, "/api/v1/ai/models/preset-anomaly-v1", "", "model_id", "preset-anomaly-v1")
	if code != http.StatusBadRequest || ec != "ERR_AI_MODEL_PRESET_PROTECTED" {
		t.Fatalf("preset removal: status = %d error_code = %q, want 400/ERR_AI_MODEL_PRESET_PROTECTED", code, ec)
	}
}

func TestUpdateAIModelIsUnsupported(t *testing.T) {
	code, ec, _ := callWithParam(t, handleUpdateAIModel, http.MethodPut, "/api/v1/ai/models/m-1", `{"description":"x"}`, "model_id", "m-1")
	if code != http.StatusNotImplemented || ec != "ERR_AI_MODEL_UPDATE_UNSUPPORTED" {
		t.Fatalf("status = %d error_code = %q, want 501/ERR_AI_MODEL_UPDATE_UNSUPPORTED (it used to answer 200/updated)", code, ec)
	}
}

func TestScheduleRoundTripThroughTheAPI(t *testing.T) {
	eng, _ := newSidecarStub(t, map[string]interface{}{"success": true})
	useAIContainer(t, eng)

	code, _, _ := callAI(t, handleStartScheduledInference, http.MethodPost, "/api/v1/ai/scheduled/start",
		`{"model_id":"m-1","device_id":"d-1","point_name":"temp","interval_seconds":30,"input_window_size":10}`)
	if code != http.StatusOK {
		t.Fatalf("start: status = %d, want 200", code)
	}
	items := scheduleItems(t)
	if len(items) != 1 {
		t.Fatalf("schedules = %#v, want exactly the one accepted loop", items)
	}
	if got := items[0]["model_id"]; got != "m-1" {
		t.Fatalf("listed model_id = %#v, want m-1", got)
	}
	if got := items[0]["interval_seconds"]; got != float64(30) {
		t.Fatalf("listed interval_seconds = %#v, want 30", got)
	}

	code, _, _ = callAI(t, handleStopScheduledInference, http.MethodPost, "/api/v1/ai/scheduled/stop", `{"model_id":"m-1"}`)
	if code != http.StatusOK {
		t.Fatalf("stop: status = %d, want 200", code)
	}
	if left := scheduleItems(t); len(left) != 0 {
		t.Fatalf("schedules still list %#v after a confirmed stop", left)
	}
}

// scheduleItems reads GET /ai/schedules; the payload is the array the frontend
// iterates, not an {items:[...]} envelope.
func scheduleItems(t *testing.T) []map[string]interface{} {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, "/api/v1/ai/schedules", "")
	if err := handleListScheduledInferences(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable schedules payload %s: %v", rec.Body, err)
	}
	return env.Data
}

func TestScheduleStartRejectionLeavesNoPhantom(t *testing.T) {
	eng, _ := newSidecarStub(t, map[string]interface{}{
		"success":       false,
		"error_code":    "ERR_MODEL_NOT_LOADED",
		"error_message": "model m-1 is not loaded",
	})
	useAIContainer(t, eng)

	code, ec, _ := callAI(t, handleStartScheduledInference, http.MethodPost, "/api/v1/ai/scheduled/start", `{"model_id":"m-1","interval_seconds":30}`)
	if code != http.StatusBadGateway || ec != "ERR_AI_SCHEDULED_START_FAILED" {
		t.Fatalf("status = %d error_code = %q, want 502/ERR_AI_SCHEDULED_START_FAILED", code, ec)
	}
	if left := scheduleItems(t); len(left) != 0 {
		t.Fatalf("a rejected schedule is listed as running: %#v", left)
	}
}

func TestUploadRegistersTheModelWithTheSidecar(t *testing.T) {
	// The handler writes to the relative dir data/models, so run it inside a
	// throwaway working directory instead of the package tree.
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	eng, paths := newSidecarStub(t, map[string]interface{}{"success": true})
	useAIContainer(t, eng)

	// The frontend has always sent `name`; the handler only read `model_name`,
	// so no upload could ever pass validation.
	code, body := postModel(t, "legacy-name", true)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("upload with the `name` field: status = %d body = %s, want 201", code, body)
	}
	var env struct {
		Data struct {
			Loaded    bool   `json:"loaded"`
			LoadError string `json:"load_error"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unparseable upload response %s: %v", body, err)
	}
	if !env.Data.Loaded {
		t.Fatalf("upload reported loaded=false (%s): the file was saved but no engine took it", env.Data.LoadError)
	}
	sawLoad := false
	for _, p := range *paths {
		if p == "/models/load" {
			sawLoad = true
		}
	}
	if !sawLoad {
		t.Fatalf("the sidecar was never asked to load the uploaded model: %#v", *paths)
	}
	if _, err := os.Stat(filepath.Join("data", "models", "legacy-name.onnx")); err != nil {
		t.Fatalf("the uploaded file is missing: %v", err)
	}
}

// TestHandlersDoNotClaimSidecarFailuresAsEngineAbsence guards the code split:
// ERR_AI_ENGINE_NOT_INITIALIZED means this gateway has no AI engine wired up,
// ERR_AI_SIDECAR_UNAVAILABLE means the engine is up but Python is unreachable.
// Both used to be reported as the latter, so a missing engine looked like an
// operator who had not started the sidecar.
func TestHandlersDoNotClaimSidecarFailuresAsEngineAbsence(t *testing.T) {
	useAIContainer(t, nil)
	for name, h := range map[string]func(echo.Context) error{
		"startSchedule":   handleStartScheduledInference,
		"stopSchedule":    handleStopScheduledInference,
		"startScheduleId": handleStartScheduledInferenceByID,
		"stopScheduleId":  handleStopScheduledInferenceByID,
	} {
		var code int
		var ec string
		if strings.HasSuffix(name, "Id") {
			code, ec, _ = callWithParam(t, h, http.MethodPost, "/api/v1/ai/models/m-1/schedule", `{"interval":30}`, "model_id", "m-1")
		} else if strings.HasPrefix(name, "stop") {
			code, ec, _ = callAI(t, h, http.MethodPost, "/api/v1/ai/scheduled/stop", `{"model_id":"m-1"}`)
		} else {
			code, ec, _ = callAI(t, h, http.MethodPost, "/api/v1/ai/scheduled/start", `{"model_id":"m-1","interval_seconds":30}`)
		}
		if code != http.StatusServiceUnavailable || ec != "ERR_AI_ENGINE_NOT_INITIALIZED" {
			t.Fatalf("%s: status = %d error_code = %q, want 503/ERR_AI_ENGINE_NOT_INITIALIZED", name, code, ec)
		}
	}
}

// modelListRows calls /ai/models and returns each row as raw JSON so a test can
// tell "the key is null" from "the key is missing".
func modelListRows(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, "/api/v1/ai/models", "")
	if err := handleListAIModels(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable models payload %s: %v", rec.Body, err)
	}
	if len(env.Data) == 0 {
		t.Fatalf("no model rows in %s", rec.Body)
	}
	return env.Data
}

func assertRaw(t *testing.T, row map[string]json.RawMessage, key, want string) {
	t.Helper()
	got, ok := row[key]
	if !ok {
		t.Fatalf("row %s has no %q key: %v", row["model_id"], key, row)
	}
	if string(got) != want {
		t.Fatalf("%s.%s = %s, want %s", row["model_id"], key, got, want)
	}
}

// The "模型性能" card rendered 推理次数 0 / 错误率 0% for every model because
// /ai/models carried no counters at all: an unreachable sidecar was
// indistinguishable from a model with a spotless record.
func TestModelListCountersAreNullWithoutSidecar(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())
	for _, row := range modelListRows(t) {
		assertRaw(t, row, "inference_count", "null")
		assertRaw(t, row, "error_count", "null")
		assertRaw(t, row, "avg_latency_ms", "null")
		// The embedded model info still has to serialize inline, not nested.
		if len(row["model_id"]) < 4 || string(row["status"]) == "null" {
			t.Fatalf("row lost its inlined model fields: %v", row)
		}
	}
}

func TestModelListCarriesSidecarCounters(t *testing.T) {
	cont := GetContainer()
	prev := cont.AIInference
	t.Cleanup(func() { cont.AIInference = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			_, _ = io.WriteString(w, `{"healthy":true,"version":"1.0"}`)
		case r.URL.Path == "/models":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = io.WriteString(w, `{"call_count":12,"error_count":3,"avg_latency_ms":40.5}`)
		default:
			_, _ = io.WriteString(w, `{"success":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	cont.AIInference = engine.NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              srv.URL,
		MaxConcurrentInferences: 1,
	}, nil)

	for _, row := range modelListRows(t) {
		assertRaw(t, row, "inference_count", "12")
		assertRaw(t, row, "error_count", "3")
		assertRaw(t, row, "avg_latency_ms", "40.5")
	}
}

// A model the sidecar has never been asked to run has a measured 0 calls, which
// is not the same as "unknown" — and no latency sample to report.
func TestModelListZeroCallsStillReportLatencyAsNull(t *testing.T) {
	cont := GetContainer()
	prev := cont.AIInference
	t.Cleanup(func() { cont.AIInference = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			_, _ = io.WriteString(w, `{"healthy":true}`)
		case r.URL.Path == "/models":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = io.WriteString(w, `{"call_count":0,"error_count":0,"avg_latency_ms":0}`)
		default:
			_, _ = io.WriteString(w, `{"success":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	cont.AIInference = engine.NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              srv.URL,
		MaxConcurrentInferences: 1,
	}, nil)

	for _, row := range modelListRows(t) {
		assertRaw(t, row, "inference_count", "0")
		assertRaw(t, row, "error_count", "0")
		assertRaw(t, row, "avg_latency_ms", "null")
	}
}

func postModel(t *testing.T, modelName string, useLegacyNameField bool) (int, string) {
	t.Helper()
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	field := "model_name"
	if useLegacyNameField {
		field = "name"
	}
	if err := mw.WriteField(field, modelName); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := mw.CreateFormFile("file", modelName+".onnx")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.Copy(part, strings.NewReader("ONNXPLACEHOLDER")); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/models/upload", strings.NewReader(buf.String()))
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := handleUploadAIModel(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	return rec.Code, rec.Body.String()
}

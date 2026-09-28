package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// /ai/self-learning/* fabricated a result for every way a call could fail:
// handleGetSelfLearningStats answered data:null both when the AI engine was
// missing and when the required device_id/point_name pair was absent, predict
// answered {predicted_value:0, confidence:0} with no engine and no model, and
// reset/threshold dereferenced the engine only inside an `if != nil` guard and
// then answered success:true unconditionally. A missing "value" also decoded as
// the number 0 and was recorded as a real observation.

// callAISL runs a self-learning handler and hands back the envelope's error code
// plus the raw data node, because these routes return an object, a list or
// nothing depending on which branch they take.
func callAISL(t *testing.T, h func(echo.Context) error, method, path, body string) (int, string, json.RawMessage) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

func selfLearningHandlers() map[string]func(echo.Context) error {
	return map[string]func(echo.Context) error{
		"stats":     handleGetSelfLearningStats,
		"all":       handleGetAllSelfLearningStats,
		"sample":    handleAddSelfLearningSample,
		"predict":   handlePredictSelfLearning,
		"reset":     handleResetSelfLearning,
		"threshold": handleSetSelfLearningThreshold,
	}
}

func TestSelfLearningWithoutEngineNeverSucceeds(t *testing.T) {
	useAIContainer(t, nil)
	// Every one of these used to answer 200: null stats, a 0/0 prediction, an
	// empty registry, and success:true for a reset that touched nothing.
	for name, h := range selfLearningHandlers() {
		path := "/api/v1/ai/self-learning/" + name
		body := `{"device_id":"dev-1","point_name":"temp","value":1.5,"threshold":2}`
		code, ec, data := callAISL(t, h, http.MethodPost, path, body)
		if name == "stats" || name == "all" {
			code, ec, data = callAISL(t, h, http.MethodGet, path+"?device_id=dev-1&point_name=temp", "")
		}
		if code != http.StatusServiceUnavailable || ec != "ERR_AI_ENGINE_NOT_INITIALIZED" {
			t.Errorf("%s: status = %d error_code = %q, want 503/ERR_AI_ENGINE_NOT_INITIALIZED", name, code, ec)
		}
		if len(data) != 0 && string(data) != "null" {
			t.Errorf("%s: 503 must not carry a fabricated result, got %s", name, data)
		}
	}
}

func TestSelfLearningRequiresTheFullKey(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())
	for name, h := range selfLearningHandlers() {
		path := "/api/v1/ai/self-learning/" + name
		var code int
		var ec string
		if name == "stats" || name == "all" {
			code, ec, _ = callAISL(t, h, http.MethodGet, path+"?device_id=dev-1", "")
		} else {
			code, ec, _ = callAISL(t, h, http.MethodPost, path, `{"device_id":"dev-1","point_name":"  ","value":1.5,"threshold":2}`)
		}
		if name == "all" {
			// The registry listing is the one call that has no key to omit.
			continue
		}
		if code != http.StatusBadRequest || ec != "ERR_AI_SELF_LEARNING_KEY_REQUIRED" {
			t.Errorf("%s: status = %d error_code = %q, want 400/ERR_AI_SELF_LEARNING_KEY_REQUIRED", name, code, ec)
		}
	}
}

func TestSelfLearningSampleRejectsBadInput(t *testing.T) {
	// A value that is absent or not a number used to reach the learner as 0.0, so
	// one malformed client request moved the model's mean, standard deviation and
	// anomaly verdict with it - state nothing can take back afterwards.
	useAIContainer(t, newNoSidecarEngine())
	for name, body := range map[string]string{
		"missing value":    `{"device_id":"dev-qa","point_name":"temp"}`,
		"string value":     `{"device_id":"dev-qa","point_name":"temp","value":"20"}`,
		"null value":       `{"device_id":"dev-qa","point_name":"temp","value":null}`,
		"window 0":         `{"device_id":"dev-qa","point_name":"temp","value":1,"window_size":0}`,
		"window fraction":  `{"device_id":"dev-qa","point_name":"temp","value":1,"window_size":1.5}`,
		"window too large": `{"device_id":"dev-qa","point_name":"temp","value":1,"window_size":99999999}`,
	} {
		code, ec, _ := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample", body)
		want := "ERR_AI_SAMPLE_VALUE_INVALID"
		if name == "missing value" {
			want = "ERR_AI_SAMPLE_VALUE_REQUIRED"
		}
		if strings.HasPrefix(name, "window") {
			want = "ERR_AI_WINDOW_SIZE_INVALID"
		}
		if code != http.StatusBadRequest || ec != want {
			t.Errorf("%s: status = %d error_code = %q, want 400/%s", name, code, ec, want)
		}
	}

	// None of the rejected requests may have created a model.
	code, ec, data := callAISL(t, handleGetSelfLearningStats, http.MethodGet,
		"/api/v1/ai/self-learning/stats?device_id=dev-qa&point_name=temp", "")
	if code != http.StatusNotFound || ec != "ERR_AI_SELF_LEARNING_NOT_FOUND" {
		t.Fatalf("stats after rejected samples = %d %q %s, want 404/ERR_AI_SELF_LEARNING_NOT_FOUND", code, ec, data)
	}
}

func TestSelfLearningThresholdRejectsZero(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())
	for name, body := range map[string]string{
		"missing":     `{"device_id":"dev-th","point_name":"temp"}`,
		"zero":        `{"device_id":"dev-th","point_name":"temp","threshold":0}`,
		"negative":    `{"device_id":"dev-th","point_name":"temp","threshold":-2}`,
		"not a value": `{"device_id":"dev-th","point_name":"temp","threshold":"high"}`,
	} {
		code, ec, _ := callAISL(t, handleSetSelfLearningThreshold, http.MethodPost, "/api/v1/ai/self-learning/threshold", body)
		want := "ERR_AI_THRESHOLD_INVALID"
		if name == "missing" {
			want = "ERR_AI_THRESHOLD_REQUIRED"
		}
		if code != http.StatusBadRequest || ec != want {
			t.Errorf("%s: status = %d error_code = %q, want 400/%s", name, code, ec, want)
		}
	}
	// A threshold of 0 was not applied, so the learner is still at its default
	// and does not flag every sample from here on.
	code, _, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		`{"device_id":"dev-th","point_name":"temp","value":7}`)
	if code != http.StatusOK {
		t.Fatalf("sample = %d %s, want 200", code, data)
	}
	var verdict struct {
		IsAnomaly bool   `json:"is_anomaly"`
		Source    string `json:"source"`
	}
	if err := json.Unmarshal(data, &verdict); err != nil {
		t.Fatalf("verdict %s: %v", data, err)
	}
	if verdict.IsAnomaly || verdict.Source != "local" {
		t.Fatalf("first sample verdict = %#v, want no anomaly from the local learner", verdict)
	}
}

func TestSelfLearningLocalLifecycle(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())
	const key = `{"device_id":"dev-lc","point_name":"temp"`

	for i := 0; i < 11; i++ {
		code, ec, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
			key+`,"value":20,"window_size":50}`)
		if code != http.StatusOK {
			t.Fatalf("sample %d = %d %s %s, want 200", i, code, ec, data)
		}
	}
	code, _, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		key+`,"value":900,"window_size":50}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"is_anomaly":true`) {
		t.Fatalf("spike verdict = %d %s, want is_anomaly:true", code, data)
	}
	if !strings.Contains(string(data), `"source":"local"`) {
		t.Fatalf("verdict %s does not say which learner produced it", data)
	}

	code, _, data = callAISL(t, handleGetSelfLearningStats, http.MethodGet,
		"/api/v1/ai/self-learning/stats?device_id=dev-lc&point_name=temp", "")
	if code != http.StatusOK {
		t.Fatalf("stats = %d %s, want 200", code, data)
	}
	var stats struct {
		TotalSamples int     `json:"total_samples"`
		MaxWindow    int     `json:"max_window"`
		Mean         float64 `json:"mean"`
		AnomalyCount int     `json:"anomaly_count"`
		LastAnomaly  string  `json:"last_anomaly"`
		Source       string  `json:"source"`
	}
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatalf("stats %s: %v", data, err)
	}
	if stats.TotalSamples != 12 || stats.MaxWindow != 50 || stats.AnomalyCount != 1 {
		t.Fatalf("stats = %+v, want the 12 samples and 1 anomaly that were recorded", stats)
	}
	if stats.LastAnomaly == "" || stats.Source != "local" {
		t.Fatalf("stats = %+v, want the anomaly timestamp and the local source", stats)
	}

	code, _, data = callAISL(t, handlePredictSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/predict",
		key+`}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"source":"local"`) {
		t.Fatalf("predict = %d %s, want 200 from the local learner", code, data)
	}

	code, _, data = callAISL(t, handleGetAllSelfLearningStats, http.MethodGet, "/api/v1/ai/self-learning/stats/all", "")
	if code != http.StatusOK {
		t.Fatalf("stats/all = %d %s, want 200", code, data)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("stats/all %s: %v", data, err)
	}
	if len(rows) != 1 || rows[0]["source"] != "local" {
		t.Fatalf("stats/all = %s, want the one local model, source-stamped", data)
	}

	code, _, data = callAISL(t, handleResetSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/reset", key+`}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"success":true`) {
		t.Fatalf("reset = %d %s, want 200 success", code, data)
	}

	// After the wipe the key is gone from both learners, which is a 404 and not
	// the previous success:true / predicted 0.
	code, _, data = callAISL(t, handleGetSelfLearningStats, http.MethodGet,
		"/api/v1/ai/self-learning/stats?device_id=dev-lc&point_name=temp", "")
	if code != http.StatusOK {
		t.Fatalf("stats after reset = %d %s, want 200 with zeroed counters", code, data)
	}
	code, ec, data := callAISL(t, handlePredictSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/predict",
		`{"device_id":"dev-lc","point_name":"never-sampled"}`)
	if code != http.StatusNotFound || ec != "ERR_AI_SELF_LEARNING_NOT_FOUND" {
		t.Fatalf("predict of an unknown model = %d %q %s, want 404/ERR_AI_SELF_LEARNING_NOT_FOUND", code, ec, data)
	}
	code, ec, data = callAISL(t, handleResetSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/reset",
		`{"device_id":"dev-lc","point_name":"never-sampled"}`)
	if code != http.StatusNotFound || ec != "ERR_AI_SELF_LEARNING_NOT_FOUND" {
		t.Fatalf("reset of an unknown model = %d %q %s, want 404, not a fake success", code, ec, data)
	}
}

func TestSelfLearningReportsTheSidecarVerdict(t *testing.T) {
	// The sidecar is answering, so its numbers are the answer and the local
	// learner must stay out of the response.
	eng, calls := aiStub(t, map[string]interface{}{
		"POST /self-learning/sample":  map[string]interface{}{"is_anomaly": true, "confidence": 0.9},
		"POST /self-learning/predict": map[string]interface{}{"predicted_value": 44.5, "confidence": 0.5},
		"GET /self-learning/stats/all": map[string]interface{}{"stats": []interface{}{
			map[string]interface{}{"device_id": "dev-s", "point_name": "temp", "total_samples": 3, "confidence": 0.7},
		}},
		"POST /self-learning/stats": map[string]interface{}{"stats": map[string]interface{}{
			"device_id": "dev-s", "point_name": "temp", "total_samples": 3, "max_window": 100, "confidence": 0.7,
		}},
	})
	useAIContainer(t, eng)

	code, _, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		`{"device_id":"dev-s","point_name":"temp","value":21,"window_size":60}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"source":"sidecar"`) ||
		!strings.Contains(string(data), `"is_anomaly":true`) {
		t.Fatalf("sample = %d %s, want the sidecar's verdict", code, data)
	}
	seen := <-calls
	if !strings.Contains(seen, `"window_size":60`) || !strings.Contains(seen, `"value":21`) {
		t.Fatalf("the sidecar was sent %s, want the caller's value and window", seen)
	}

	code, _, data = callAISL(t, handlePredictSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/predict",
		`{"device_id":"dev-s","point_name":"temp"}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"predicted_value":44.5`) {
		t.Fatalf("predict = %d %s, want the sidecar's prediction", code, data)
	}

	code, _, data = callAISL(t, handleGetSelfLearningStats, http.MethodGet,
		"/api/v1/ai/self-learning/stats?device_id=dev-s&point_name=temp", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"source":"sidecar"`) {
		t.Fatalf("stats = %d %s, want the sidecar's stats, stamped", code, data)
	}

	code, _, data = callAISL(t, handleGetAllSelfLearningStats, http.MethodGet, "/api/v1/ai/self-learning/stats/all", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"source":"sidecar"`) {
		t.Fatalf("stats/all = %d %s, want the sidecar's registry", code, data)
	}
}

func TestSelfLearningSidecarRefusalIsNotAGatewayVerdict(t *testing.T) {
	eng, _ := aiStub(t, map[string]interface{}{
		"/self-learning/sample": map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_INVALID_REQUEST",
			"error_message": "value must be a number",
		},
		"/self-learning/reset": map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_AI_SELF_LEARNING_NOT_FOUND",
			"error_message": "Self-learning model not found",
		},
	})
	useAIContainer(t, eng)

	code, ec, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		`{"device_id":"dev-r","point_name":"temp","value":"x"}`)
	// The gateway rejects the body before the sidecar sees it, so this exercises
	// the same envelope on a call that does reach it.
	if code != http.StatusBadRequest || ec != "ERR_AI_SAMPLE_VALUE_INVALID" {
		t.Fatalf("sample = %d %q %s, want 400 from validation", code, ec, data)
	}
	code, ec, data = callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		`{"device_id":"dev-r","point_name":"temp","value":5}`)
	if code != http.StatusBadRequest || ec != "ERR_AI_SELF_LEARNING_REJECTED" {
		t.Fatalf("sidecar refusal = %d %q %s, want 400/ERR_AI_SELF_LEARNING_REJECTED", code, ec, data)
	}
	code, ec, data = callAISL(t, handleResetSelfLearning, http.MethodPost, "/api/v1/ai/self-learning/reset",
		`{"device_id":"dev-r","point_name":"temp"}`)
	if code != http.StatusNotFound || ec != "ERR_AI_SELF_LEARNING_NOT_FOUND" {
		t.Fatalf("sidecar not-found = %d %q %s, want 404 rather than success:true", code, ec, data)
	}
}

func TestAIMonitorNamesTheSelfLearningLearner(t *testing.T) {
	useAIContainer(t, newNoSidecarEngine())
	code, _, data := callAISL(t, handleAddSelfLearningSample, http.MethodPost, "/api/v1/ai/self-learning/sample",
		`{"device_id":"dev-mon","point_name":"temp","value":10}`)
	if code != http.StatusOK {
		t.Fatalf("sample = %d %s, want 200", code, data)
	}
	// /system/ai-monitor is a real page (router entry AiMonitor) and it renders
	// this field, so it has to say which learner the rows came from instead of
	// showing numbers out of context.
	c, rec := setupEcho(http.MethodGet, "/api/v1/system/ai-monitor", "")
	if err := handleGetAIMonitor(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Data struct {
			SelfLearning    []map[string]interface{} `json:"self_learning"`
			SelfLearningSrc string                   `json:"self_learning_source"`
			SelfLearningErr string                   `json:"self_learning_error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("monitor body %s: %v", rec.Body, err)
	}
	if env.Data.SelfLearningErr != "" {
		t.Fatalf("monitor reported %q for a learner that answered", env.Data.SelfLearningErr)
	}
	if env.Data.SelfLearningSrc != "local" || len(env.Data.SelfLearning) != 1 {
		t.Fatalf("monitor self_learning = %d rows from %q, want the 1 local row",
			len(env.Data.SelfLearning), env.Data.SelfLearningSrc)
	}
	if env.Data.SelfLearning[0]["source"] != "local" {
		t.Fatalf("row is not source-stamped: %#v", env.Data.SelfLearning[0])
	}
}

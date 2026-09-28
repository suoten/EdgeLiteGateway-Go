package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// AIClient is an HTTP JSON client that talks to the Python AI sidecar.
// It wraps all ONNX inference and self-learning operations.
//
// Architecture:
//
//	Go Gateway ─── HTTP JSON (50052) ───> Python AI Sidecar
//	                                        ├── ONNX Runtime
//	                                        ├── Self-Learning (EWMA)
//	                                        └── Preset Model Generation
type AIClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewAIClient creates a new AI sidecar HTTP client.
func NewAIClient(baseURL string) *AIClient {
	return &AIClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ─── Types ───

type AIModelInfo struct {
	ModelID        string            `json:"model_id"`
	ModelName      string            `json:"model_name"`
	ModelVersion   string            `json:"model_version"`
	ModelType      string            `json:"model_type"`
	IsPreset       bool              `json:"is_preset"`
	ModelPath      string            `json:"model_path"`
	Status         string            `json:"status"`
	LoadedAt       string            `json:"loaded_at"`
	InputSchema    map[string]string `json:"input_schema"`
	OutputSchema   map[string]string `json:"output_schema"`
	InferenceCount int64             `json:"inference_count"`
	ErrorCount     int64             `json:"error_count"`
	AvgLatencyMs   float64           `json:"avg_latency_ms"`
}

type AIInferenceResult struct {
	ModelID      string                 `json:"model_id"`
	OutputData   map[string]interface{} `json:"output_data"`
	LatencyMs    int                    `json:"latency_ms"`
	Status       string                 `json:"status"`
	ErrorMessage string                 `json:"error_message"`
}

type AIEngineStats struct {
	TotalCalls        int64            `json:"total_calls"`
	TotalErrors       int64            `json:"total_errors"`
	AvgLatencyMs      int              `json:"avg_latency_ms"`
	ModelDistribution map[string]int64 `json:"model_distribution"`
	RecentLatencies   []int            `json:"recent_latencies"`
	LoadedModels      int              `json:"loaded_models"`
}

type AIHealthStatus struct {
	Healthy           bool   `json:"healthy"`
	Version           string `json:"version"`
	ExecutionProvider string `json:"execution_provider"`
	OnnxAvailable     bool   `json:"onnx_available"`
	NumpyAvailable    bool   `json:"numpy_available"`
	LoadedModelCount  int    `json:"loaded_model_count"`
}

type AISelfLearningStats struct {
	DeviceID     string  `json:"device_id"`
	PointName    string  `json:"point_name"`
	TotalSamples int64   `json:"total_samples"`
	WindowSize   int     `json:"window_size"`
	MaxWindow    int     `json:"max_window"`
	Mean         float64 `json:"mean"`
	StdDev       float64 `json:"std_dev"`
	Ewma         float64 `json:"ewma"`
	AnomalyCount int64   `json:"anomaly_count"`
	LastAnomaly  string  `json:"last_anomaly"`
	Confidence   float64  `json:"confidence"`
	// A pointer because a sidecar built before this field existed answers without
	// it; a zeroed float64 would then render as "threshold is 0", a value no
	// operator ever set. nil keeps it visibly unknown.
	Threshold *float64 `json:"threshold"`
}

// ─── Internal helpers ───

// ThresholdValue reports the threshold as a plain JSON value: nil, which the
// ledger renders as unknown, when the answering sidecar predates the field. A
// zeroed float64 would claim a threshold of 0 that no operator ever set.
func (s *AISelfLearningStats) ThresholdValue() interface{} {
	if s == nil || s.Threshold == nil {
		return nil
	}
	return *s.Threshold
}

func (c *AIClient) doPost(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	} else {
		bodyReader = nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSidecarUnreachable, err)
	}
	defer resp.Body.Close()
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrSidecarUnreachable, err)
	}
	return decodeSidecarResponse(resp.StatusCode, respData)
}

// decodeSidecarResponse classifies a sidecar reply the same way for GET and POST,
// because callers branch on it: only ErrSidecarUnreachable means "nothing is
// there", which is what licenses the gateway to answer from its own state.
func decodeSidecarResponse(status int, respData []byte) (map[string]interface{}, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(respData, &result); err != nil {
		if status >= http.StatusBadRequest {
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrSidecarRejected, status, snippetForError(respData))
		}
		return nil, fmt.Errorf("unmarshal response: %w (body: %s)", err, snippetForError(respData))
	}
	if status >= http.StatusBadRequest {
		// The sidecar is up and refused. Treating this as a transport failure
		// would let the gateway quietly keep learning in its own copy of the
		// state while the sidecar holds the real one.
		if envErr := sidecarError(result); envErr != nil {
			return nil, fmt.Errorf("HTTP %d: %w", status, envErr)
		}
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrSidecarRejected, status, snippetForError(respData))
	}
	return result, nil
}

// snippetForError keeps a rejected response readable in a log and in the error
// message the API echoes back, without pasting a whole HTML error page.
func snippetForError(body []byte) string {
	const max = 200
	runes := []rune(strings.TrimSpace(string(body)))
	if len(runes) <= max {
		return string(runes)
	}
	// Cut on a rune boundary: this text reaches logs and API error messages,
	// and a mid-rune slice turns the tail of a Chinese body into replacement
	// characters.
	return string(runes[:max]) + "…"
}

func (c *AIClient) doGet(ctx context.Context, path string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSidecarUnreachable, err)
	}
	defer resp.Body.Close()
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrSidecarUnreachable, err)
	}
	return decodeSidecarResponse(resp.StatusCode, respData)
}

// ─── Model Management ───

func (c *AIClient) ListModels(ctx context.Context) ([]AIModelInfo, error) {
	resp, err := c.doGet(ctx, "/models")
	if err != nil {
		return nil, err
	}
	modelsRaw, ok := resp["models"]
	if !ok {
		return nil, fmt.Errorf("no models in response")
	}
	data, err := json.Marshal(modelsRaw)
	if err != nil {
		return nil, err
	}
	var models []AIModelInfo
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("unmarshal models: %w", err)
	}
	return models, nil
}

func (c *AIClient) LoadModel(ctx context.Context, req map[string]interface{}) (*AIModelInfo, error) {
	resp, err := c.doPost(ctx, "/models/load", req)
	if err != nil {
		return nil, err
	}
	if err := sidecarError(resp); err != nil {
		return nil, err
	}
	modelRaw := resp["model_info"]
	data, _ := json.Marshal(modelRaw)
	var info AIModelInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *AIClient) UnloadModel(ctx context.Context, modelID string) error {
	resp, err := c.doPost(ctx, "/models/unload", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

func (c *AIClient) ReloadModel(ctx context.Context, modelID, modelPath string) (string, error) {
	resp, err := c.doPost(ctx, "/models/reload", map[string]string{"model_id": modelID, "model_path": modelPath})
	if err != nil {
		return "", err
	}
	if err := sidecarError(resp); err != nil {
		return "", err
	}
	// An unchecked assertion here panicked on any sidecar that reloaded without
	// reporting a new version.
	newVersion, _ := resp["new_version"].(string)
	return newVersion, nil
}

func (c *AIClient) EnableModel(ctx context.Context, modelID string) error {
	resp, err := c.doPost(ctx, "/models/enable", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

func (c *AIClient) DisableModel(ctx context.Context, modelID string) error {
	resp, err := c.doPost(ctx, "/models/disable", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

func (c *AIClient) RemoveModel(ctx context.Context, modelID string) error {
	resp, err := c.doPost(ctx, "/models/remove", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

func (c *AIClient) GetModelStatus(ctx context.Context, modelID string) (string, error) {
	resp, err := c.doGet(ctx, "/models/"+modelID+"/status")
	if err != nil {
		return "", err
	}
	status, _ := resp["status"].(string)
	return status, nil
}

// ─── Inference ───

func (c *AIClient) Infer(ctx context.Context, modelID string, inputData []float64) (*AIInferenceResult, error) {
	resp, err := c.doPost(ctx, "/infer", map[string]interface{}{
		"model_id":   modelID,
		"input_data": inputData,
	})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	var result AIInferenceResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal inference result: %w", err)
	}
	return &result, nil
}

// ─── Scheduled Inference ───

func (c *AIClient) StartScheduledInference(ctx context.Context, modelID, deviceID, pointName string, interval, windowSize int) error {
	resp, err := c.doPost(ctx, "/scheduled/start", map[string]interface{}{
		"model_id": modelID, "device_id": deviceID, "point_name": pointName,
		"interval_seconds": interval, "input_window_size": windowSize,
	})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

func (c *AIClient) StopScheduledInference(ctx context.Context, modelID string) error {
	resp, err := c.doPost(ctx, "/scheduled/stop", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	return sidecarError(resp)
}

// ─── Self-Learning ───

// AddSample records one observation. The sidecar rejects a request without a
// device_id/point_name with HTTP 400 + its error envelope, and answers a normal
// request without a success field, so both have to be read rather than assumed.
func (c *AIClient) AddSample(ctx context.Context, deviceID, pointName string, value float64, windowSize int) (bool, float64, error) {
	resp, err := c.doPost(ctx, "/self-learning/sample", map[string]interface{}{
		"device_id": deviceID, "point_name": pointName,
		"value": value, "window_size": windowSize,
	})
	if err != nil {
		return false, 0, err
	}
	if err := sidecarError(resp); err != nil {
		return false, 0, err
	}
	isAnomaly, _ := resp["is_anomaly"].(bool)
	confidence, _ := resp["confidence"].(float64)
	return isAnomaly, confidence, nil
}

func (c *AIClient) Predict(ctx context.Context, deviceID, pointName string) (float64, float64, error) {
	resp, err := c.doPost(ctx, "/self-learning/predict", map[string]string{
		"device_id": deviceID, "point_name": pointName,
	})
	if err != nil {
		return 0, 0, err
	}
	if err := sidecarError(resp); err != nil {
		return 0, 0, err
	}
	pv, _ := resp["predicted_value"].(float64)
	conf, _ := resp["confidence"].(float64)
	return pv, conf, nil
}

// GetSelfLearningStats returns (nil, ErrSelfLearningNotFound) when the sidecar
// is answering and has no model for this key: its documented "stats": null. It
// used to return (nil, nil), which the engine read as "ask the local model
// instead", so a live sidecar and an empty gateway looked the same.
func (c *AIClient) GetSelfLearningStats(ctx context.Context, deviceID, pointName string) (*AISelfLearningStats, error) {
	resp, err := c.doPost(ctx, "/self-learning/stats", map[string]string{
		"device_id": deviceID, "point_name": pointName,
	})
	if err != nil {
		return nil, err
	}
	if err := sidecarError(resp); err != nil {
		return nil, err
	}
	statsRaw, ok := resp["stats"]
	if !ok || statsRaw == nil {
		return nil, ErrSelfLearningNotFound
	}
	data, _ := json.Marshal(statsRaw)
	var stats AISelfLearningStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func (c *AIClient) GetAllSelfLearningStats(ctx context.Context) ([]AISelfLearningStats, error) {
	resp, err := c.doGet(ctx, "/self-learning/stats/all")
	if err != nil {
		return nil, err
	}
	statsRaw, ok := resp["stats"]
	if !ok {
		// An absent block is a malformed answer, not "no models": reading it as
		// empty made the gateway answer from local state a live sidecar owns.
		return nil, fmt.Errorf("sidecar self-learning stats response has no stats block")
	}
	data, _ := json.Marshal(statsRaw)
	var stats []AISelfLearningStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}
	return stats, nil
}

func (c *AIClient) ResetSelfLearning(ctx context.Context, deviceID, pointName string) error {
	resp, err := c.doPost(ctx, "/self-learning/reset", map[string]string{
		"device_id": deviceID, "point_name": pointName,
	})
	if err != nil {
		return err
	}
	if err := sidecarError(resp); err != nil {
		if code, _ := resp["error_code"].(string); code == errSelfLearningNotFoundCode {
			return fmt.Errorf("%w: %v", ErrSelfLearningNotFound, err)
		}
		return err
	}
	return nil
}

func (c *AIClient) SetSelfLearningThreshold(ctx context.Context, deviceID, pointName string, threshold float64) error {
	resp, err := c.doPost(ctx, "/self-learning/threshold", map[string]interface{}{
		"device_id": deviceID, "point_name": pointName, "threshold": threshold,
	})
	if err != nil {
		return err
	}
	if err := sidecarError(resp); err != nil {
		// Same mapping as reset: "no model for this key" is a statement about
		// which registry holds it, not a refusal, and the caller has to be able
		// to tell the two apart.
		if code, _ := resp["error_code"].(string); code == errSelfLearningNotFoundCode {
			return fmt.Errorf("%w: %v", ErrSelfLearningNotFound, err)
		}
		return err
	}
	return nil
}

// ─── Statistics & Health ───

// ErrSidecarRejected reports that the sidecar answered the request and said no.
// It is what separates "ask the sidecar again later" from "this model or version
// is not there", so callers can answer 4xx instead of a misleading 503.
var ErrSidecarRejected = errors.New("sidecar rejected the request")

// ErrSidecarUnreachable reports that no HTTP response came back at all: nothing
// is listening, the connection was refused, or the deadline passed before an
// answer arrived. This is the only condition under which the gateway may answer
// a self-learning call from its own in-process learner, because the sidecar's
// copy of that state cannot have been touched.
var ErrSidecarUnreachable = errors.New("ai sidecar unreachable")

// ErrSelfLearningNotFound reports that neither learner holds a model for the
// requested device/point pair. It is not a failure of the request, and it is not
// the same as a model that has learned nothing: the callers answer 404 with it
// instead of a 0 that reads as a measurement.
var ErrSelfLearningNotFound = errors.New("self-learning model not found")

// errSelfLearningNotFoundCode is the error_code the sidecar sends for a reset of
// a model it does not have (ai_sidecar/server.py handle_reset_self_learning).
const errSelfLearningNotFoundCode = "ERR_AI_SELF_LEARNING_NOT_FOUND"

// sidecarError turns the sidecar's business-error envelope into an error. It
// answers failures with HTTP 200 + {success: false, error_code, error_message},
// so a transport-level 200 alone proves nothing; responses that carry no
// success field at all (/health, /stats) are not error envelopes.
func sidecarError(resp map[string]interface{}) error {
	success, ok := resp["success"].(bool)
	if !ok || success {
		return nil
	}
	code, _ := resp["error_code"].(string)
	msg, _ := resp["error_message"].(string)
	switch {
	case code != "" && msg != "":
		return fmt.Errorf("%w: %s: %s", ErrSidecarRejected, code, msg)
	case code != "":
		return fmt.Errorf("%w: %s", ErrSidecarRejected, code)
	case msg != "":
		return fmt.Errorf("%w: %s", ErrSidecarRejected, msg)
	default:
		return fmt.Errorf("%w without an error code", ErrSidecarRejected)
	}
}

func (c *AIClient) GetStats(ctx context.Context) (*AIEngineStats, error) {
	resp, err := c.doGet(ctx, "/stats")
	if err != nil {
		return nil, err
	}
	statsRaw := resp["stats"]
	if statsRaw == nil {
		// (nil, nil) let callers dereference a nil struct: a sidecar that
		// answered without a stats block crashed the gateway on the next poll.
		return nil, fmt.Errorf("sidecar /stats response has no stats block")
	}
	data, _ := json.Marshal(statsRaw)
	var stats AIEngineStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func (c *AIClient) GetModelStats(ctx context.Context, modelID string) (map[string]interface{}, error) {
	resp, err := c.doGet(ctx, "/models/"+modelID+"/stats")
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *AIClient) HealthCheck(ctx context.Context) (*AIHealthStatus, error) {
	resp, err := c.doGet(ctx, "/health")
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(resp)
	var status AIHealthStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// ─── Execution Provider ───

func (c *AIClient) SetExecutionProvider(ctx context.Context, provider string) (string, error) {
	resp, err := c.doPost(ctx, "/execution-provider", map[string]string{"provider": provider})
	if err != nil {
		return "", err
	}
	if success, ok := resp["success"].(bool); !ok || !success {
		msg, _ := resp["error_message"].(string)
		if msg == "" {
			msg = "sidecar rejected the provider switch without an error message"
		}
		return "", fmt.Errorf("%s", msg)
	}
	actual, _ := resp["actual_provider"].(string)
	if actual == "" {
		// Reporting an empty provider as a success is how a switch that never
		// happened used to look like one.
		return "", fmt.Errorf("sidecar accepted the provider switch but reported no actual_provider")
	}
	return actual, nil
}

func (c *AIClient) GetAvailableProviders(ctx context.Context) ([]string, error) {
	resp, err := c.doGet(ctx, "/execution-providers")
	if err != nil {
		return nil, err
	}
	providersRaw, ok := resp["providers"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no providers in response")
	}
	providers := make([]string, 0, len(providersRaw))
	for _, p := range providersRaw {
		if s, ok := p.(string); ok {
			providers = append(providers, s)
		}
	}
	return providers, nil
}

// ─── Version Management ───

func (c *AIClient) GetModelVersionHistory(ctx context.Context, modelID string) ([]map[string]interface{}, error) {
	resp, err := c.doGet(ctx, "/models/"+modelID+"/history")
	if err != nil {
		return nil, err
	}
	if err := sidecarError(resp); err != nil {
		return nil, err
	}
	historyRaw, ok := resp["history"].([]interface{})
	if !ok {
		// An absent list is not an empty history: reading it as one told the
		// caller the model had no earlier versions when the sidecar had in
		// fact answered something else entirely.
		return nil, fmt.Errorf("sidecar model history response has no history list")
	}
	result := make([]map[string]interface{}, 0, len(historyRaw))
	for _, h := range historyRaw {
		if m, ok := h.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result, nil
}

func (c *AIClient) RollbackModelVersion(ctx context.Context, modelID, targetVersion string) error {
	resp, err := c.doPost(ctx, "/models/rollback", map[string]string{
		"model_id": modelID, "target_version": targetVersion,
	})
	if err != nil {
		return err
	}
	// The sidecar refuses a rollback (unknown model, unknown version) with
	// HTTP 200 + success:false, so only its verdict can decide whether this
	// succeeded.
	return sidecarError(resp)
}

// ─── Preset Models ───

func (c *AIClient) GeneratePresetModels(ctx context.Context) (map[string]bool, error) {
	resp, err := c.doPost(ctx, "/models/generate-presets", nil)
	if err != nil {
		return nil, err
	}
	results := make(map[string]bool)
	if r, ok := resp["results"].(map[string]interface{}); ok {
		for k, v := range r {
			if b, ok := v.(bool); ok {
				results[k] = b
			}
		}
	}
	return results, nil
}

// ─── Connection check ───

func (c *AIClient) IsAvailable(ctx context.Context) bool {
	_, err := c.HealthCheck(ctx)
	if err != nil {
		logrus.Debugf("AI sidecar not available: %v", err)
		return false
	}
	return true
}

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/models"
)

// AIInferenceEngine manages edge AI model inference.
//
// In the Go edition, this engine is a thin adapter that delegates all
// ONNX inference and self-learning operations to the Python AI sidecar
// via HTTP JSON. The Python sidecar wraps ONNX Runtime, self-learning
// models, and preset model generation.
//
// Architecture:
//
//	Go Gateway ─── HTTP JSON ───> Python AI Sidecar ───> ONNX Runtime
type AIInferenceEngine struct {
	mu        sync.RWMutex
	cfg       *config.AiInferenceConfig
	eventBus  *EventBus
	client    *AIClient
	semaphore chan struct{}

	// Statistics (Go-side, for events/metrics)
	muStats         sync.Mutex
	totalInferences int64
	totalErrors     int64
	totalLatencySum float64

	// Self-learning manager (Go-side, for local fast-path when sidecar is down)
	selfLearner *SelfLearningManager

	// Built-in preset models. Registered locally so the model list works
	// without the Python sidecar; inference still requires the sidecar.
	presets []presetModel

	// Scheduling is executed inside the sidecar, which exposes no list endpoint,
	// so the gateway records what it actually asked the sidecar to start.
	// /ai/schedules reports this registry instead of an always-empty array.
	muSched   sync.Mutex
	schedules map[string]ScheduledInference
}

// ScheduledInference is one inference loop this gateway started in the sidecar.
type ScheduledInference struct {
	ModelID    string    `json:"model_id"`
	DeviceID   string    `json:"device_id"`
	PointName  string    `json:"point_name"`
	Interval   int       `json:"interval_seconds"`
	WindowSize int       `json:"input_window_size"`
	StartedAt  time.Time `json:"started_at"`
}

type presetModel struct {
	ModelID   string
	ModelType string
	Version   string
	Enabled   bool
}

var builtinPresets = []presetModel{
	{ModelID: "preset-anomaly-v1", ModelType: "anomaly", Version: "v1.0.0", Enabled: true},
	{ModelID: "preset-trend-v1", ModelType: "trend", Version: "v1.0.0", Enabled: true},
	{ModelID: "preset-threshold-v1", ModelType: "threshold", Version: "v1.0.0", Enabled: true},
}

// NewAIInferenceEngine creates a new AIInferenceEngine.
func NewAIInferenceEngine(cfg *config.AiInferenceConfig, eventBus *EventBus) *AIInferenceEngine {
	maxConc := cfg.MaxConcurrentInferences
	if maxConc <= 0 {
		maxConc = 4
	}
	sidecarURL := cfg.SidecarURL
	if sidecarURL == "" {
		sidecarURL = "http://127.0.0.1:50052"
	}
	return &AIInferenceEngine{
		cfg:         cfg,
		eventBus:    eventBus,
		client:      NewAIClient(sidecarURL),
		semaphore:   make(chan struct{}, maxConc),
		selfLearner: NewSelfLearningManager(),
		presets:     append([]presetModel(nil), builtinPresets...),
		schedules:   map[string]ScheduledInference{},
	}
}

// ─── Model Management ───

// LoadModel loads an AI model (delegates to Python sidecar).
func (e *AIInferenceEngine) LoadModel(modelID, modelType, version string, fn InferenceFunc) error {
	// In sidecar mode, we don't use local InferenceFunc; the sidecar handles inference.
	// This method is kept for API compatibility.
	e.mu.Lock()
	defer e.mu.Unlock()
	logrus.WithField("model_id", modelID).Info("AI model load requested (delegated to sidecar)")
	return nil
}

// UnloadModel removes a loaded model.

// UnloadModel removes a loaded model in the sidecar and returns its verdict:
// the API layer used to answer status=unloaded even when the sidecar refused,
// so the UI reported a lifecycle change that never happened.
func (e *AIInferenceEngine) UnloadModel(modelID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.client.UnloadModel(ctx, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("Failed to unload model via sidecar")
		return err
	}
	return nil
}

// RemoveModel deregisters a model in the sidecar.
func (e *AIInferenceEngine) RemoveModel(modelID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.client.RemoveModel(ctx, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("Failed to remove model via sidecar")
		return err
	}
	return nil
}

// DisableModel unloads a model in the sidecar without deregistering it.
func (e *AIInferenceEngine) DisableModel(modelID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.client.DisableModel(ctx, modelID)
}

// EnableModel asks the sidecar to bring a model it already knows about back
// to life; the state change only counts once the sidecar has confirmed it.
func (e *AIInferenceEngine) EnableModel(modelID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.client.EnableModel(ctx, modelID)
}

// LoadModelFile registers an uploaded model file with the sidecar. Until now
// an upload wrote bytes to disk and told the UI "uploaded" while no engine
// could ever serve the model, so it never showed up in any list.
func (e *AIInferenceEngine) LoadModelFile(modelID, modelPath, modelType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := e.client.LoadModel(ctx, map[string]interface{}{
		"model_id":      modelID,
		"model_name":    modelID,
		"model_path":    modelPath,
		"model_type":    modelType,
		"model_version": "v1.0.0",
	})
	return err
}

// RunInference runs inference using the specified model via the Python sidecar.
func (e *AIInferenceEngine) RunInference(modelID string, input map[string]interface{}) (*models.AIInferenceResponse, error) {
	// Acquire semaphore (concurrency limit)
	e.semaphore <- struct{}{}
	defer func() { <-e.semaphore }()

	// Convert input map to float array
	inputData, err := normalizeInput(input)
	if err != nil {
		return nil, fmt.Errorf("normalize input: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := e.client.Infer(ctx, modelID, inputData)
	if err != nil {
		e.muStats.Lock()
		e.totalErrors++
		e.muStats.Unlock()
		return nil, fmt.Errorf("inference error: %w", err)
	}

	if result.Status != "success" {
		e.muStats.Lock()
		e.totalErrors++
		e.muStats.Unlock()
		return nil, fmt.Errorf("inference failed: %s", result.ErrorMessage)
	}

	latency := float64(result.LatencyMs) / 1000.0

	// Update stats
	e.muStats.Lock()
	e.totalInferences++
	e.totalLatencySum += latency
	e.muStats.Unlock()

	// Publish event
	if e.eventBus != nil {
		e.eventBus.Publish(Event{
			Type:   EventTypeAIInference,
			Source: "ai_inference_engine",
			Data: map[string]interface{}{
				"model_id":   modelID,
				"latency_s":  latency,
				"confidence": 1.0,
			},
		})
	}

	return &models.AIInferenceResponse{
		ModelID:    modelID,
		Output:     result.OutputData,
		Confidence: 1.0,
		LatencyMs:  float64(result.LatencyMs),
	}, nil
}

// GetModelInfo returns information about a loaded model.
func (e *AIInferenceEngine) GetModelInfo(modelID string) (*models.AIModelInfo, error) {
	if info, ok := e.presetInfo(modelID); ok {
		return info, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	modelsList, err := e.client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	for _, m := range modelsList {
		if m.ModelID == modelID {
			return &models.AIModelInfo{
				ModelID:   m.ModelID,
				ModelType: m.ModelType,
				Version:   m.ModelVersion,
				Status:    m.Status,
				CreatedAt: m.LoadedAt,
				UpdatedAt: m.LoadedAt,
			}, nil
		}
	}
	return nil, fmt.Errorf("model not found: %s", modelID)
}

// presetInfo builds an AIModelInfo for a built-in preset model, answered
// locally so model status works when the sidecar is unavailable.
func (e *AIInferenceEngine) presetInfo(modelID string) (*models.AIModelInfo, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, p := range e.presets {
		if p.ModelID != modelID {
			continue
		}
		now := time.Now().Format(time.RFC3339)
		status := "inactive"
		if p.Enabled {
			status = "active"
		}
		return &models.AIModelInfo{
			ModelID:      p.ModelID,
			ModelName:    presetDisplayName(p.ModelType),
			ModelType:    p.ModelType,
			ModelPath:    "models/" + p.ModelID + ".onnx",
			Version:      p.Version,
			ModelVersion: p.Version,
			Status:       status,
			IsPreset:     true,
			CreatedAt:    now,
			UpdatedAt:    now,
		}, true
	}
	return nil, false
}

// presetDisplayName returns the built-in human-readable name for a preset
// model type.
func presetDisplayName(modelType string) string {
	switch modelType {
	case "anomaly":
		return "Anomaly Detection"
	case "trend":
		return "Trend Prediction"
	case "threshold":
		return "Dynamic Threshold"
	default:
		return modelType
	}
}

// SetPresetEnabled flips the enabled flag of a built-in preset model and
// reports whether modelID matched a preset.
func (e *AIInferenceEngine) SetPresetEnabled(modelID string, enabled bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.presets {
		if e.presets[i].ModelID == modelID {
			e.presets[i].Enabled = enabled
			return true
		}
	}
	return false
}

// IsPresetModel reports whether modelID is a built-in preset model.
func (e *AIInferenceEngine) IsPresetModel(modelID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, p := range e.presets {
		if p.ModelID == modelID {
			return true
		}
	}
	return false
}

// PresetModelIDs returns the IDs of all built-in preset models.
func (e *AIInferenceEngine) PresetModelIDs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.presets))
	for _, p := range e.presets {
		ids = append(ids, p.ModelID)
	}
	return ids
}

// ListModels returns information about all known models: the built-in
// presets (always, so the list works without the sidecar) plus any custom
// models currently loaded in the sidecar.
func (e *AIInferenceEngine) ListModels() []models.AIModelInfo {
	e.mu.RLock()
	presets := make([]presetModel, len(e.presets))
	copy(presets, e.presets)
	e.mu.RUnlock()

	result := make([]models.AIModelInfo, 0, len(presets)+4)
	for _, p := range presets {
		info, _ := e.presetInfo(p.ModelID)
		if info != nil {
			result = append(result, *info)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	modelsList, err := e.client.ListModels(ctx)
	if err != nil {
		logrus.Debugf("Sidecar model list unavailable, showing presets only: %v", err)
		return result
	}
	for _, m := range modelsList {
		isPreset := false
		for _, p := range presets {
			if p.ModelID == m.ModelID {
				isPreset = true
				break
			}
		}
		if isPreset {
			continue
		}
		result = append(result, models.AIModelInfo{
			ModelID:      m.ModelID,
			ModelName:    m.ModelID,
			ModelType:    m.ModelType,
			Version:      m.ModelVersion,
			ModelVersion: m.ModelVersion,
			Status:       m.Status,
			CreatedAt:    m.LoadedAt,
			UpdatedAt:    m.LoadedAt,
		})
	}
	return result
}

// GetModelStats returns statistics for a model.
func (e *AIInferenceEngine) GetModelStats(modelID string) (*models.AIModelStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := e.client.GetModelStats(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return &models.AIModelStats{
		ModelID:        modelID,
		InferenceCount: int(toInt64(stats["call_count"])),
		ErrorCount:     int(toInt64(stats["error_count"])),
		AvgLatencyMs:   toFloat64OrZero(stats["avg_latency_ms"]),
	}, nil
}

// GetStats returns overall engine statistics. The latency fields are null until
// at least one inference has actually been measured: a 0 there reads as
// "measured, and fast", so a sidecar that never answered looked healthy.
func (e *AIInferenceEngine) GetStats() map[string]interface{} {
	// Read the preset list before muStats: the package lock order is mu -> muStats.
	e.mu.RLock()
	presetCount := len(e.presets)
	e.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := e.client.GetStats(ctx)
	if err != nil || stats == nil {
		// Fall back to local stats
		e.muStats.Lock()
		defer e.muStats.Unlock()
		var avgLatencyS, avgLatencyMs interface{}
		if e.totalInferences > 0 {
			// totalLatencySum accumulates seconds: RunInference divides
			// result.LatencyMs by 1000 before adding it.
			avgLatencyS = e.totalLatencySum / float64(e.totalInferences)
			avgLatencyMs = avgLatencyS.(float64) * 1000
		}
		// Emit both naming variants so the frontend (total_calls /
		// avg_latency_ms) and legacy consumers stay consistent.
		return map[string]interface{}{
			"total_inferences": e.totalInferences,
			"total_calls":      e.totalInferences,
			"total_errors":     e.totalErrors,
			"avg_latency_s":    avgLatencyS,
			"avg_latency_ms":   avgLatencyMs,
			// model_count is what the engine can actually list in this state:
			// without the sidecar that is the built-in presets, matching what
			// /ai/models returns. Omitting it left the dashboard card at 0.
			"model_count": presetCount,
			"sidecar":     "unavailable",
		}
	}
	var avgLatencyMs interface{}
	if stats.TotalCalls > 0 {
		avgLatencyMs = stats.AvgLatencyMs
	}
	return map[string]interface{}{
		"total_calls":    stats.TotalCalls,
		"total_errors":   stats.TotalErrors,
		"avg_latency_ms": avgLatencyMs,
		"loaded_models":  stats.LoadedModels,
		"model_count":    stats.LoadedModels, // alias for frontend compatibility
		"sidecar":        "available",
	}
}

// ExecutionProviders returns the ONNX execution providers the sidecar reports.
func (e *AIInferenceEngine) ExecutionProviders(ctx context.Context) ([]string, error) {
	return e.client.GetAvailableProviders(ctx)
}

// SetExecutionProvider switches the sidecar's provider and returns the one it
// actually activated; the sidecar may keep CPU when the requested one is absent.
func (e *AIInferenceEngine) SetExecutionProvider(ctx context.Context, provider string) (string, error) {
	return e.client.SetExecutionProvider(ctx, provider)
}

// ModelVersionHistory returns the versions the sidecar recorded for a model.
// The API used to answer an empty list for this question, which is what a model
// with no older versions looks like too.
func (e *AIInferenceEngine) ModelVersionHistory(ctx context.Context, modelID string) ([]map[string]interface{}, error) {
	return e.client.GetModelVersionHistory(ctx, modelID)
}

// RollbackModelVersion makes the sidecar load an earlier version of a model and
// reports its answer; a refused rollback is returned as an error rather than
// swallowed so the endpoint cannot claim success for work that did not happen.
func (e *AIInferenceEngine) RollbackModelVersion(ctx context.Context, modelID, targetVersion string) error {
	return e.client.RollbackModelVersion(ctx, modelID, targetVersion)
}

// ReloadModel reloads a model (hot-reload).

func (e *AIInferenceEngine) ReloadModel(modelID string, fn InferenceFunc) error {
	return e.ReloadModelFrom(modelID, "")
}

// ReloadModelFrom reloads a model, optionally from a new file path; an empty
// path keeps the one the sidecar already holds. The path parameter used to be
// bound, dropped on the floor (`_ = modelFilePath`) and reported as reloaded.
func (e *AIInferenceEngine) ReloadModelFrom(modelID, modelPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := e.client.ReloadModel(ctx, modelID, modelPath)
	return err
}

// StartScheduledInference asks the sidecar to run an inference loop and, only
// once it accepted, records the loop so the schedule list reflects real state.
func (e *AIInferenceEngine) StartScheduledInference(ctx context.Context, modelID, deviceID, pointName string, interval, windowSize int) error {
	if err := e.client.StartScheduledInference(ctx, modelID, deviceID, pointName, interval, windowSize); err != nil {
		return err
	}
	e.muSched.Lock()
	defer e.muSched.Unlock()
	if e.schedules == nil {
		e.schedules = map[string]ScheduledInference{}
	}
	e.schedules[modelID] = ScheduledInference{
		ModelID:    modelID,
		DeviceID:   deviceID,
		PointName:  pointName,
		Interval:   interval,
		WindowSize: windowSize,
		StartedAt:  time.Now(),
	}
	return nil
}

// StopScheduledInference stops the loop in the sidecar and drops the record.
func (e *AIInferenceEngine) StopScheduledInference(ctx context.Context, modelID string) error {
	if err := e.client.StopScheduledInference(ctx, modelID); err != nil {
		return err
	}
	e.muSched.Lock()
	delete(e.schedules, modelID)
	e.muSched.Unlock()
	return nil
}

// ListScheduledInferences returns the loops this gateway started, by model ID.
func (e *AIInferenceEngine) ListScheduledInferences() []ScheduledInference {
	e.muSched.Lock()
	defer e.muSched.Unlock()
	out := make([]ScheduledInference, 0, len(e.schedules))
	for _, s := range e.schedules {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

// SidecarHealth reports what the Python sidecar itself says about its runtime:
// ONNX version, availability and the active execution provider.
func (e *AIInferenceEngine) SidecarHealth(ctx context.Context) (*AIHealthStatus, error) {
	return e.client.HealthCheck(ctx)
}

// IsSidecarAvailable checks if the Python AI sidecar is running.
func (e *AIInferenceEngine) IsSidecarAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.client.IsAvailable(ctx)
}

// ─── Self-Learning (delegated to sidecar, with local fallback) ───

// Self-learning state lives in the sidecar while it answers and in the gateway's
// own EWMA registry while nothing answers. Writes go to that authoritative learner
// only - never to both, which would let the two histories drift apart. Reads ask
// it first and fall through to the registry the other learner has no copy of, so
// no state an operator trained can disappear from a status page. Every row says
// which learner it came from, which is what keeps the fall-through honest.
const (
	SelfLearningSourceSidecar = "sidecar"
	SelfLearningSourceLocal   = "local"
	SelfLearningSourceMixed   = "mixed"
)

// useLocalLearner is what justifies answering from gateway state after the
// sidecar was asked and did not cooperate: only a connection that got no answer
// at all. An actual refusal means the sidecar's copy of this model is alive and
// authoritative even when it says no.
func useLocalLearner(err error) bool {
	return errors.Is(err, ErrSidecarUnreachable)
}

// selfLearningKeyOwner says which learner a call about this key belongs to, so a
// write reaches the model the operator is looking at instead of the other
// registry. Without it a threshold change on a gateway-trained row was answered
// by the sidecar - which creates a model for any key it is asked about - and the
// row on screen kept its old threshold while the page reported "applied".
//
// Only a key the gateway already holds can be ambiguous, so that is the only
// case that costs a round trip: the sidecar is asked whether it holds this key
// too, and it wins when it does. decided is false when the probe itself got no
// answer (a refusal, or a sidecar that does not serve the stats route), which is
// not evidence about ownership: the caller then does what it did before routing
// existed - ask the sidecar, and fall back locally only if nothing answered.
func (e *AIInferenceEngine) selfLearningKeyOwner(ctx context.Context, deviceID, pointName string) (string, bool) {
	if e.selfLearner.GetModel(deviceID, pointName) == nil {
		return SelfLearningSourceSidecar, false
	}
	_, err := e.client.GetSelfLearningStats(ctx, deviceID, pointName)
	switch {
	case err == nil:
		return SelfLearningSourceSidecar, true
	case errors.Is(err, ErrSelfLearningNotFound), useLocalLearner(err):
		return SelfLearningSourceLocal, true
	default:
		return "", false
	}
}

// AddSample records one observation and reports what the learner made of it.
func (e *AIInferenceEngine) AddSample(ctx context.Context, deviceID, pointName string, value float64, windowSize int) (bool, float64, string, error) {
	if owner, decided := e.selfLearningKeyOwner(ctx, deviceID, pointName); decided && owner == SelfLearningSourceLocal {
		model := e.selfLearner.GetOrCreate(deviceID, pointName, windowSize)
		return model.AddSample(value), model.GetConfidence(), SelfLearningSourceLocal, nil
	}
	isAnomaly, confidence, err := e.client.AddSample(ctx, deviceID, pointName, value, windowSize)
	if err == nil {
		return isAnomaly, confidence, SelfLearningSourceSidecar, nil
	}
	if !useLocalLearner(err) {
		return false, 0, "", err
	}
	logrus.WithError(err).Debug("AI sidecar unreachable, recording the sample locally")
	model := e.selfLearner.GetOrCreate(deviceID, pointName, windowSize)
	return model.AddSample(value), model.GetConfidence(), SelfLearningSourceLocal, nil
}

// Predict predicts the next value using EWMA.
func (e *AIInferenceEngine) Predict(ctx context.Context, deviceID, pointName string) (float64, float64, string, error) {
	if owner, decided := e.selfLearningKeyOwner(ctx, deviceID, pointName); decided && owner == SelfLearningSourceLocal {
		model := e.selfLearner.GetModel(deviceID, pointName)
		// 0/0 here read as "the model predicts zero with no confidence"; there is
		// no model to predict anything.
		return model.Predict(), model.GetConfidence(), SelfLearningSourceLocal, nil
	}
	pv, conf, err := e.client.Predict(ctx, deviceID, pointName)
	if err == nil {
		return pv, conf, SelfLearningSourceSidecar, nil
	}
	if !useLocalLearner(err) {
		return 0, 0, "", err
	}
	model := e.selfLearner.GetModel(deviceID, pointName)
	if model == nil {
		return 0, 0, SelfLearningSourceLocal, ErrSelfLearningNotFound
	}
	return model.Predict(), model.GetConfidence(), SelfLearningSourceLocal, nil
}

// GetSelfLearningStats returns stats for a device point's self-learning model.
// The sidecar's "stats": null and a connection that got no answer both mean it
// has nothing for this key, so the gateway's own model is the only remaining
// source - reported as local, which is why a fall-through cannot be mistaken for
// a sidecar reading.
func (e *AIInferenceEngine) GetSelfLearningStats(ctx context.Context, deviceID, pointName string) (map[string]interface{}, string, error) {
	stats, err := e.client.GetSelfLearningStats(ctx, deviceID, pointName)
	if err == nil {
		out := map[string]interface{}{
			"device_id":     stats.DeviceID,
			"point_name":    stats.PointName,
			"total_samples": stats.TotalSamples,
			"window_size":   stats.WindowSize,
			"max_window":    stats.MaxWindow,
			"mean":          stats.Mean,
			"std_dev":       stats.StdDev,
			"ewma":          stats.Ewma,
			"anomaly_count": stats.AnomalyCount,
			"last_anomaly":  stats.LastAnomaly,
			"confidence":    stats.Confidence,
			"threshold":     stats.ThresholdValue(),
			"source":        SelfLearningSourceSidecar,
		}
		return out, SelfLearningSourceSidecar, nil
	}
	if !errors.Is(err, ErrSelfLearningNotFound) && !useLocalLearner(err) {
		return nil, "", err
	}
	model := e.selfLearner.GetModel(deviceID, pointName)
	if model == nil {
		return nil, SelfLearningSourceLocal, ErrSelfLearningNotFound
	}
	out := model.GetStats()
	out["source"] = SelfLearningSourceLocal
	return out, SelfLearningSourceLocal, nil
}

// ResetSelfLearning wipes one model's learned state, in whichever learner holds
// it. It reports ErrSelfLearningNotFound rather than success when neither does.
func (e *AIInferenceEngine) ResetSelfLearning(ctx context.Context, deviceID, pointName string) (string, error) {
	owner, decided := e.selfLearningKeyOwner(ctx, deviceID, pointName)
	if decided && owner == SelfLearningSourceLocal {
		// The sidecar disowns this key, so the gateway's copy is the only model
		// the operator can see and the only one a reset can reach.
		e.selfLearner.GetModel(deviceID, pointName).Reset()
		return SelfLearningSourceLocal, nil
	}
	err := e.client.ResetSelfLearning(ctx, deviceID, pointName)
	if err == nil {
		return SelfLearningSourceSidecar, nil
	}
	// Once the probe has said the sidecar owns the key, whatever it answers to the
	// write stands: wiping the gateway's copy instead would destroy state nobody
	// asked to touch, on a sidecar that contradicts itself.
	if decided || !useLocalLearner(err) {
		return "", err
	}
	model := e.selfLearner.GetModel(deviceID, pointName)
	if model == nil {
		return SelfLearningSourceLocal, ErrSelfLearningNotFound
	}
	model.Reset()
	return SelfLearningSourceLocal, nil
}

// SetSelfLearningThreshold sets the anomaly detection threshold. Like the
// sidecar, a key nobody has sampled yet gets a model with that threshold.
func (e *AIInferenceEngine) SetSelfLearningThreshold(ctx context.Context, deviceID, pointName string, threshold float64) (string, error) {
	owner, decided := e.selfLearningKeyOwner(ctx, deviceID, pointName)
	if decided && owner == SelfLearningSourceLocal {
		e.selfLearner.GetModel(deviceID, pointName).SetThreshold(threshold)
		return SelfLearningSourceLocal, nil
	}
	err := e.client.SetSelfLearningThreshold(ctx, deviceID, pointName, threshold)
	if err == nil {
		return SelfLearningSourceSidecar, nil
	}
	if decided || !useLocalLearner(err) {
		return "", err
	}
	e.selfLearner.GetOrCreate(deviceID, pointName, 100).SetThreshold(threshold)
	return SelfLearningSourceLocal, nil
}

// GetAllSelfLearningStats returns statistics for all self-learning models. The
// sidecar's registry comes first; models the gateway trained while no sidecar was
// answering are appended for the keys the sidecar does not report, stamped local,
// so switching the sidecar on does not make learned state vanish from the page
// that lists it.
func (e *AIInferenceEngine) GetAllSelfLearningStats(ctx context.Context) ([]map[string]interface{}, string, error) {
	stats, err := e.client.GetAllSelfLearningStats(ctx)
	if err != nil && !useLocalLearner(err) {
		return nil, "", err
	}
	result := make([]map[string]interface{}, 0, len(stats))
	seen := make(map[string]bool, len(stats))
	for _, s := range stats {
		seen[selfLearningKey(s.DeviceID, s.PointName)] = true
		result = append(result, map[string]interface{}{
			"device_id":     s.DeviceID,
			"point_name":    s.PointName,
			"total_samples": s.TotalSamples,
			"window_size":   s.WindowSize,
			"max_window":    s.MaxWindow,
			"mean":          s.Mean,
			"std_dev":       s.StdDev,
			"ewma":          s.Ewma,
			"anomaly_count": s.AnomalyCount,
			"last_anomaly":  s.LastAnomaly,
			"confidence":    s.Confidence,
			"threshold":     s.ThresholdValue(),
			"source":        SelfLearningSourceSidecar,
		})
	}
	local := e.selfLearner.GetAllStats()
	source := SelfLearningSourceSidecar
	if len(result) == 0 {
		source = SelfLearningSourceLocal
	}
	for _, row := range local {
		deviceID, _ := row["device_id"].(string)
		pointName, _ := row["point_name"].(string)
		if seen[selfLearningKey(deviceID, pointName)] {
			continue
		}
		row["source"] = SelfLearningSourceLocal
		result = append(result, row)
	}
	if source == SelfLearningSourceSidecar && len(result) > len(stats) {
		source = SelfLearningSourceMixed
	}
	return result, source, nil
}

// selfLearningKey is the SelfLearningManager's registry key.
func selfLearningKey(deviceID, pointName string) string {
	return fmt.Sprintf("%s:%s", deviceID, pointName)
}

// ─── Local Self-Learning Model (fallback when sidecar is down) ───

// MaxSelfLearningWindow bounds the sliding window a caller may ask for. The
// window is held in memory per (device, point), so an unbounded request is a
// memory lease the gateway cannot take back.
const MaxSelfLearningWindow = 100000

// SelfLearningModel implements a simple online learning model that can
// detect anomalies and predict future values based on historical data.
// It uses a sliding window with exponential weighted moving average (EWMA).
//
// The Python sidecar runs the same algorithm and the gateway reports one or the
// other without saying which, so the two have to agree field by field: see
// ai_sidecar/server.py SelfLearningModel.
type SelfLearningModel struct {
	mu              sync.Mutex
	deviceID        string
	pointName       string
	windowSize      int
	values          []float64
	ewma            float64
	ewmaInitialized bool
	ewmaAlpha       float64
	threshold       float64
	stdDev          float64
	lastAnomaly     time.Time
	anomalyCount    int
	totalSamples    int
}

// NewSelfLearningModel creates a new self-learning model for a device point.
func NewSelfLearningModel(deviceID, pointName string, windowSize int) *SelfLearningModel {
	if windowSize <= 0 {
		windowSize = 100
	}
	if windowSize > MaxSelfLearningWindow {
		windowSize = MaxSelfLearningWindow
	}
	return &SelfLearningModel{
		deviceID:   deviceID,
		pointName:  pointName,
		windowSize: windowSize,
		ewmaAlpha:  0.3,
		threshold:  3.0,
	}
}

// AddSample adds a new data sample and returns whether it's an anomaly.
func (m *SelfLearningModel) AddSample(value float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalSamples++
	m.values = append(m.values, value)
	if len(m.values) > m.windowSize {
		m.values = m.values[1:]
	}
	if len(m.values) < 10 {
		m.updateEWMA(value)
		return false
	}
	mean, std := m.calculateStats()
	m.updateEWMA(value)
	isAnomaly := false
	if std > 0 {
		zScore := math.Abs(value-mean) / std
		if zScore > m.threshold {
			isAnomaly = true
			m.anomalyCount++
			m.lastAnomaly = time.Now()
		}
	}
	return isAnomaly
}

func (m *SelfLearningModel) updateEWMA(value float64) {
	// A zero-valued field cannot double as "not seeded yet": for a signal that
	// sits at 0 and then jumps, `ewma == 0` restarts the average at the jump and
	// erases the history. The sidecar tracks the first sample explicitly, so the
	// gateway has to as well.
	if !m.ewmaInitialized {
		m.ewma = value
		m.ewmaInitialized = true
	} else {
		m.ewma = m.ewmaAlpha*value + (1-m.ewmaAlpha)*m.ewma
	}
}

func (m *SelfLearningModel) calculateStats() (float64, float64) {
	n := len(m.values)
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range m.values {
		sum += v
	}
	mean := sum / float64(n)
	var variance float64
	for _, v := range m.values {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(n)
	std := math.Sqrt(variance)
	m.stdDev = std
	return mean, std
}

// Predict predicts the next value using EWMA.
func (m *SelfLearningModel) Predict() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ewma
}

// GetConfidence returns the confidence of predictions (0-1).
func (m *SelfLearningModel) GetConfidence() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.confidenceLocked()
}

// confidenceLocked is the only confidence formula in this model. GetStats used
// to re-derive it with an inline expression that treats mean == 0 as
// confidence 1 while /predict reported 0, so one model answered its own page
// with two different numbers.
func (m *SelfLearningModel) confidenceLocked() float64 {
	if len(m.values) < 10 || m.stdDev == 0 {
		return 0
	}
	mean, _ := m.calculateStats()
	if mean == 0 {
		return 0
	}
	cv := m.stdDev / math.Abs(mean)
	confidence := 1.0 / (1.0 + cv)
	if confidence > 1 {
		confidence = 1
	}
	return confidence
}

// GetStats returns model statistics.
func (m *SelfLearningModel) GetStats() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	mean, std := m.calculateStats()
	// The sidecar reports "no anomaly yet" as an empty string; a zero time.Time
	// formatted to RFC3339 rendered as 0001-01-01, a date no device produced.
	var lastAnomaly string
	if !m.lastAnomaly.IsZero() {
		lastAnomaly = m.lastAnomaly.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"device_id":     m.deviceID,
		"point_name":    m.pointName,
		"total_samples": m.totalSamples,
		"window_size":   len(m.values),
		"max_window":    m.windowSize,
		"mean":          mean,
		"std_dev":       std,
		"ewma":          m.ewma,
		"anomaly_count": m.anomalyCount,
		"last_anomaly":  lastAnomaly,
		"confidence":    m.confidenceLocked(),
		// The threshold is part of the model's state, and the page lists these
		// rows next to the sidecar's: without it a threshold written to one
		// learner looked unchanged in the listing even though the call worked.
		"threshold": m.threshold,
	}
}

// SetThreshold sets the anomaly detection threshold (in standard deviations).
func (m *SelfLearningModel) SetThreshold(threshold float64) {
	m.mu.Lock()
	m.threshold = threshold
	m.mu.Unlock()
}

// Reset resets the model.
func (m *SelfLearningModel) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values = m.values[:0]
	m.ewma = 0
	m.ewmaInitialized = false
	m.stdDev = 0
	m.anomalyCount = 0
	m.totalSamples = 0
	// The sidecar clears the timestamp too; leaving it set reported "0 anomalies
	// so far, the last one at <time>" - a state no learner can be in.
	m.lastAnomaly = time.Time{}
}

// SelfLearningManager manages self-learning models for multiple device points.
type SelfLearningManager struct {
	mu     sync.RWMutex
	models map[string]*SelfLearningModel
}

// NewSelfLearningManager creates a new SelfLearningManager.
func NewSelfLearningManager() *SelfLearningManager {
	return &SelfLearningManager{
		models: make(map[string]*SelfLearningModel),
	}
}

// GetOrCreate returns or creates a self-learning model for a device point.
func (m *SelfLearningManager) GetOrCreate(deviceID, pointName string, windowSize int) *SelfLearningModel {
	key := fmt.Sprintf("%s:%s", deviceID, pointName)
	m.mu.RLock()
	if model, ok := m.models[key]; ok {
		m.mu.RUnlock()
		return model
	}
	m.mu.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if model, ok := m.models[key]; ok {
		return model
	}
	model := NewSelfLearningModel(deviceID, pointName, windowSize)
	m.models[key] = model
	return model
}

// GetModel returns a model for a device point, or nil if not found.
func (m *SelfLearningManager) GetModel(deviceID, pointName string) *SelfLearningModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.models[fmt.Sprintf("%s:%s", deviceID, pointName)]
}

// RemoveModel removes a model.
func (m *SelfLearningManager) RemoveModel(deviceID, pointName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.models, fmt.Sprintf("%s:%s", deviceID, pointName))
}

// GetAllStats returns statistics for all models.
func (m *SelfLearningManager) GetAllStats() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(m.models))
	for _, model := range m.models {
		result = append(result, model.GetStats())
	}
	return result
}

// ─── Helpers ───

// normalizeInput converts the input map to a float64 array for inference.
func normalizeInput(input map[string]interface{}) ([]float64, error) {
	// If input has "input_data" as an array, use it directly
	if data, ok := input["input_data"]; ok {
		if arr, err := toFloatArray(data); err == nil {
			return arr, nil
		}
	}
	// Try to convert all values to floats; a map whose value is a numeric
	// array ({"values":[...]}) contributes that array instead of being skipped.
	arr := make([]float64, 0, len(input))
	for _, v := range input {
		switch val := v.(type) {
		case float64:
			arr = append(arr, val)
		case float32:
			arr = append(arr, float64(val))
		case int:
			arr = append(arr, float64(val))
		case int64:
			arr = append(arr, float64(val))
		case json.Number:
			f, err := val.Float64()
			if err != nil {
				continue
			}
			arr = append(arr, f)
		case []interface{}:
			if sub, err := toFloatArray(val); err == nil {
				arr = append(arr, sub...)
			}
		case []float64:
			arr = append(arr, val...)
		case map[string]interface{}:
			// Nested maps ({"input_data":{"values":[...]}}) are flattened so
			// callers may wrap their arrays at any depth.
			if sub, err := normalizeInput(val); err == nil {
				arr = append(arr, sub...)
			}
		default:
			// Skip non-numeric values
		}
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("no numeric values in input")
	}
	return arr, nil
}

// toFloatArray converts an interface to []float64.
func toFloatArray(data interface{}) ([]float64, error) {
	switch v := data.(type) {
	case []float64:
		return v, nil
	case []interface{}:
		result := make([]float64, 0, len(v))
		for _, item := range v {
			switch n := item.(type) {
			case float64:
				result = append(result, n)
			case float32:
				result = append(result, float64(n))
			case int:
				result = append(result, float64(n))
			case int64:
				result = append(result, float64(n))
			default:
				return nil, fmt.Errorf("non-numeric element in array")
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected array, got %T", data)
	}
}

// toInt64 converts interface{} to int64.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// toFloat64OrZero converts interface{} to float64, returning 0 on failure.
// This is used in contexts where errors cannot be propagated (e.g. map lookups).
func toFloat64OrZero(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// InferenceFunc is retained for API compatibility but not used in sidecar mode.
type InferenceFunc func(input map[string]interface{}) (output interface{}, confidence float64, err error)

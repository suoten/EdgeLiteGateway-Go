package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/sirupsen/logrus"

	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/security"
)

// RegisterAIModelRoutes registers AI model management API routes.
// This mirrors the Python edgelite/api/ai_models.py router.
func RegisterAIModelRoutes(g *echo.Group) {
	// --- Model CRUD routes (frontend uses /ai/models prefix) ---
	g.GET("/models", handleListAIModels, requirePermission(security.PermSystemConfig))
	g.GET("/models/:model_id", handleGetAIModelStatus, requirePermission(security.PermSystemConfig))
	g.PUT("/models/:model_id", handleUpdateAIModel, requirePermission(security.PermSystemConfig))
	g.DELETE("/models/:model_id", handleRemoveAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/models/:model_id/enable", handleEnableAIModelByID, requirePermission(security.PermSystemConfig))
	g.POST("/models/:model_id/disable", handleDisableAIModelByID, requirePermission(security.PermSystemConfig))
	g.POST("/models/:model_id/reload", handleReloadAIModelByID, requirePermission(security.PermSystemConfig))
	g.GET("/models/:model_id/stats", handleGetAIModelStats, requirePermission(security.PermSystemConfig))
	g.GET("/models/:model_id/versions", handleGetAIModelHistory, requirePermission(security.PermSystemConfig))
	g.POST("/models/:model_id/rollback", handleRollbackAIModelByID, requirePermission(security.PermSystemConfig))
	g.POST("/models/:model_id/schedule", handleStartScheduledInferenceByID, requirePermission(security.PermSystemConfig))
	g.DELETE("/models/:model_id/schedule", handleStopScheduledInferenceByID, requirePermission(security.PermSystemConfig))
	g.PUT("/models/:model_id/preprocess", handleSetPreprocessConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/models/:model_id/postprocess", handleSetPostprocessConfig, requirePermission(security.PermSystemConfig))

	// --- Legacy routes (for backward compatibility) ---
	g.GET("", handleListAIModels, requirePermission(security.PermSystemConfig))
	g.POST("/load", handleLoadAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/unload", handleUnloadAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/reload", handleReloadAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/enable", handleEnableAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/disable", handleDisableAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/remove", handleRemoveAIModel, requirePermission(security.PermSystemConfig))
	g.GET("/:model_id/status", handleGetAIModelStatus, requirePermission(security.PermSystemConfig))
	g.GET("/:model_id/stats", handleGetAIModelStats, requirePermission(security.PermSystemConfig))
	g.GET("/:model_id/history", handleGetAIModelHistory, requirePermission(security.PermSystemConfig))
	g.POST("/rollback", handleRollbackAIModel, requirePermission(security.PermSystemConfig))
	g.POST("/generate-presets", handleGeneratePresets, requirePermission(security.PermSystemConfig))
	g.POST("/:model_id/preprocess", handleSetPreprocessConfig, requirePermission(security.PermSystemConfig))
	g.GET("/:model_id/preprocess", handleGetPreprocessConfig, requirePermission(security.PermSystemConfig))
	g.POST("/:model_id/postprocess", handleSetPostprocessConfig, requirePermission(security.PermSystemConfig))
	g.GET("/:model_id/postprocess", handleGetPostprocessConfig, requirePermission(security.PermSystemConfig))

	// --- Inference & stats ---
	g.POST("/infer", handleAIInfer, requirePermission(security.PermSystemConfig))
	g.POST("/inference", handleAIInfer, requirePermission(security.PermSystemConfig))
	g.GET("/stats", handleGetAIStats, requirePermission(security.PermSystemConfig))
	g.GET("/summary", handleGetAISummary, requirePermission(security.PermSystemConfig))
	g.GET("/inference/logs", handleGetInferenceLogs, requirePermission(security.PermSystemConfig))

	// --- Scheduled inference ---
	g.POST("/scheduled/start", handleStartScheduledInference, requirePermission(security.PermSystemConfig))
	g.POST("/scheduled/stop", handleStopScheduledInference, requirePermission(security.PermSystemConfig))
	g.GET("/schedules", handleListScheduledInferences, requirePermission(security.PermSystemConfig))

	// --- Self-learning ---
	g.GET("/self-learning/stats", handleGetSelfLearningStats, requirePermission(security.PermSystemConfig))
	g.GET("/self-learning/stats/all", handleGetAllSelfLearningStats, requirePermission(security.PermSystemConfig))
	g.POST("/self-learning/sample", handleAddSelfLearningSample, requirePermission(security.PermSystemConfig))
	g.POST("/self-learning/predict", handlePredictSelfLearning, requirePermission(security.PermSystemConfig))
	g.POST("/self-learning/reset", handleResetSelfLearning, requirePermission(security.PermSystemConfig))
	g.POST("/self-learning/threshold", handleSetSelfLearningThreshold, requirePermission(security.PermSystemConfig))

	// --- Health & execution providers ---
	g.GET("/health", handleAIHealthCheck, requirePermission(security.PermSystemConfig))
	g.GET("/execution-providers", handleGetExecutionProviders, requirePermission(security.PermSystemConfig))
	g.POST("/execution-provider", handleSetExecutionProvider, requirePermission(security.PermSystemConfig))

	// --- Model upload ---
	g.POST("/models/upload", handleUploadAIModel, requirePermission(security.PermSystemConfig))

	// --- AB test & hot-swap ---
	g.POST("/ab-test", handleCreateABTest, requirePermission(security.PermSystemConfig))
	g.GET("/ab-test", handleListABTests, requirePermission(security.PermSystemConfig))
	g.GET("/ab-test/:test_id", handleGetABTest, requirePermission(security.PermSystemConfig))
	g.POST("/ab-test/:test_id/split", handleABTestSplit, requirePermission(security.PermSystemConfig))
	g.POST("/ab-test/:test_id/promote", handleABTestPromote, requirePermission(security.PermSystemConfig))
	g.POST("/ab-test/:test_id/rollback", handleABTestRollback, requirePermission(security.PermSystemConfig))
	g.POST("/hot-swap", handleHotSwapModel, requirePermission(security.PermSystemConfig))
	g.GET("/hot-swap", handleListHotSwaps, requirePermission(security.PermSystemConfig))

	// --- Preprocess/postprocess steps ---
	g.GET("/preprocess/steps", handleListPreprocessSteps, requirePermission(security.PermSystemConfig))
	g.GET("/postprocess/steps", handleListPostprocessSteps, requirePermission(security.PermSystemConfig))

	// --- Cache & resources ---
	g.GET("/cache/stats", handleGetCacheStats, requirePermission(security.PermSystemConfig))
	g.POST("/cache/clear", handleClearCache, requirePermission(security.PermSystemConfig))
	g.GET("/resources", handleGetAIResources, requirePermission(security.PermSystemConfig))
	g.GET("/latency/:model_id", handleGetModelLatency, requirePermission(security.PermSystemConfig))
	g.GET("/devices", handleListAIDevices, requirePermission(security.PermSystemConfig))
	g.GET("/batch/stats", handleGetBatchStats, requirePermission(security.PermSystemConfig))
}

func handleListAIModels(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return OKPaged(c, []interface{}{}, 0, 1, 20)
	}
	models := cont.AIInference.ListModels()
	// Support pagination: frontend sends page & size query params
	page, size := parsePagination(c)
	// models is []interface{}, do simple pagination
	total := len(models)
	start := (page - 1) * size
	if start >= total {
		return OKPaged(c, []interface{}{}, total, page, size)
	}
	end := start + size
	if end > total {
		end = total
	}
	return OKPaged(c, enrichModelStats(cont, models[start:end]), total, page, size)
}

// aiModelRow is a /ai/models entry: the model info plus its performance
// counters. The counters are pointers because "the sidecar could not answer"
// must not serialize as the 0 the UI reads as a clean record.
type aiModelRow struct {
	models.AIModelInfo
	InferenceCount *int     `json:"inference_count"`
	ErrorCount     *int     `json:"error_count"`
	AvgLatencyMs   *float64 `json:"avg_latency_ms"`
}

func enrichModelStats(cont *ServiceContainer, page []models.AIModelInfo) []aiModelRow {
	out := make([]aiModelRow, 0, len(page))
	sidecarUp := cont.AIInference.IsSidecarAvailable()
	for i := range page {
		row := aiModelRow{AIModelInfo: page[i]}
		if sidecarUp {
			if ms, err := cont.AIInference.GetModelStats(page[i].ModelID); err == nil {
				row.InferenceCount = &ms.InferenceCount
				row.ErrorCount = &ms.ErrorCount
				if ms.InferenceCount > 0 {
					row.AvgLatencyMs = &ms.AvgLatencyMs
				}
			}
		}
		out = append(out, row)
	}
	return out
}

func handleLoadAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_AI_SIDECAR_UNAVAILABLE", "AI inference engine not initialized")
	}
	modelID, _ := req["model_id"].(string)
	modelPath, _ := req["model_path"].(string)
	modelType, _ := req["model_type"].(string)
	version, _ := req["model_version"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	if version == "" {
		version = "v1.0.0"
	}
	logrus.WithFields(logrus.Fields{
		"model_id":   modelID,
		"model_path": modelPath,
		"model_type": modelType,
		"version":    version,
	}).Info("Loading AI model")
	if err := cont.AIInference.LoadModel(modelID, modelType, version, nil); err != nil {
		logrus.WithError(err).Warn("Failed to load AI model")
		return ErrorCode(c, http.StatusInternalServerError, "ERR_AI_MODEL_LOAD_FAILED", "Failed to load model")
	}
	return Created(c, map[string]interface{}{"model_id": modelID, "status": "loaded"})
}

func handleUnloadAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := cont.AIInference.UnloadModel(modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model unload failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_UNLOAD_FAILED", "ERR_AI_MODEL_UNLOAD_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "unloaded"})
}

func handleReloadAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_AI_SIDECAR_UNAVAILABLE", "AI inference engine not initialized")
	}
	if err := cont.AIInference.ReloadModel(modelID, nil); err != nil {
		return ErrorCode(c, http.StatusInternalServerError, "ERR_AI_MODEL_RELOAD_FAILED", "Failed to reload model")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "reloaded"})
}

func handleEnableAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	// The handler used to answer status=enabled without touching any state at
	// all: the row flipped to active and flipped back on the next refresh.
	if err := enableAIModel(cont, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model enable failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_ENABLE_FAILED", "ERR_AI_MODEL_ENABLE_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "enabled"})
}

// enableAIModel turns a model on: presets are a gateway-side flag, everything
// else has to be confirmed by the sidecar that serves inference.
func enableAIModel(cont *ServiceContainer, modelID string) error {
	if cont.AIInference.IsPresetModel(modelID) {
		if cont.AIInference.SetPresetEnabled(modelID, true) && cont.Database != nil {
			_ = cont.Database.SetSetting("ai_preset_enabled_"+modelID, "1")
		}
		return nil
	}
	return cont.AIInference.EnableModel(modelID)
}

// disableAIModel is the mirror image: a preset is a flag, a sidecar model has
// to be unloaded where it runs.
func disableAIModel(cont *ServiceContainer, modelID string) error {
	if cont.AIInference.IsPresetModel(modelID) {
		if cont.AIInference.SetPresetEnabled(modelID, false) && cont.Database != nil {
			_ = cont.Database.SetSetting("ai_preset_enabled_"+modelID, "0")
		}
		return nil
	}
	return cont.AIInference.DisableModel(modelID)
}

// handleEnableAIModelByID enables a model by path parameter (frontend: POST /ai/models/:id/enable)

func handleEnableAIModelByID(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := enableAIModel(cont, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model enable failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_ENABLE_FAILED", "ERR_AI_MODEL_ENABLE_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "enabled"})
}

func handleDisableAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	// Disabling used to be an unload call whose error was thrown away.
	if err := disableAIModel(cont, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model disable failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_DISABLE_FAILED", "ERR_AI_MODEL_DISABLE_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "disabled"})
}

// handleDisableAIModelByID disables a model by path parameter (frontend: POST /ai/models/:id/disable)

func handleDisableAIModelByID(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := disableAIModel(cont, modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model disable failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_DISABLE_FAILED", "ERR_AI_MODEL_DISABLE_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "disabled"})
}

func handleRemoveAIModel(c echo.Context) error {
	// Support both path parameter (DELETE /ai/models/:model_id) and body JSON
	modelID := c.Param("model_id")
	if modelID == "" {
		var req map[string]interface{}
		if err := c.Bind(&req); err != nil {
			return BadRequest(c, "Invalid request body")
		}
		modelID, _ = req["model_id"].(string)
	}
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if cont.AIInference.IsPresetModel(modelID) {
		// The UI already refuses to delete presets; an API client must not be
		// able to "remove" one and get a success it cannot observe.
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_MODEL_PRESET_PROTECTED", "ERR_AI_MODEL_PRESET_PROTECTED")
	}
	if err := cont.AIInference.RemoveModel(modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI model remove failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_REMOVE_FAILED", "ERR_AI_MODEL_REMOVE_FAILED")
	}
	removeUploadedModelFile(modelID)
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "removed"})
}

// validModelID mirrors the charset the upload endpoint enforces, so a model ID
// can never escape the models directory when its file is deleted.
func validModelID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

// removeUploadedModelFile drops the model file after a confirmed removal; a
// file left behind could be reloaded behind the UI's back.
func removeUploadedModelFile(modelID string) {
	if !validModelID(modelID) {
		return
	}
	for _, ext := range aiModelFileExts {
		p := filepath.Join(aiModelsDir(), modelID+ext)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Remove(p); err != nil {
			logrus.WithError(err).WithField("path", p).Warn("AI model file left on disk after removal")
		}
		return
	}
}

func handleGetAIModelStatus(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return OK(c, map[string]string{"model_id": modelID, "status": "unavailable"})
	}
	info, err := cont.AIInference.GetModelInfo(modelID)
	if err != nil {
		return OK(c, map[string]string{"model_id": modelID, "status": "not_found"})
	}
	return OK(c, info)
}

func handleGetAIModelStats(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return OK(c, map[string]interface{}{"model_id": modelID})
	}
	stats, err := cont.AIInference.GetModelStats(modelID)
	if err != nil {
		return OK(c, map[string]interface{}{"model_id": modelID})
	}
	return OK(c, stats)
}

// handleGetAIModelHistory lists the versions the sidecar recorded for a model.
// It used to answer an empty list without asking anything, so the versions dialog
// showed "no earlier versions" for a sidecar that was never contacted.
func handleGetAIModelHistory(c echo.Context) error {
	modelID := c.Param("model_id")
	if modelID == "" {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_MODEL_ID_REQUIRED", "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	history, err := cont.AIInference.ModelVersionHistory(c.Request().Context(), modelID)
	if err != nil {
		return sidecarFailure(c, err, "ERR_AI_MODEL_HISTORY_REJECTED")
	}
	if history == nil {
		history = []map[string]interface{}{}
	}
	return OK(c, history)
}

// rollbackAIModelVersion asks the sidecar to load an earlier version and reports
// what it answered. Both rollback routes used to bind the body, change nothing and
// answer status:"rolled_back", so the versions dialog's Rollback button reported a
// success that never happened.
func rollbackAIModelVersion(c echo.Context, modelID, targetVersion string) error {
	if strings.TrimSpace(targetVersion) == "" {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_ROLLBACK_TARGET_REQUIRED", "target_version is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := cont.AIInference.RollbackModelVersion(c.Request().Context(), modelID, targetVersion); err != nil {
		recordAudit(c, "ai_model_rollback", "ai_model", modelID, "failure",
			map[string]interface{}{"target_version": targetVersion})
		return sidecarFailure(c, err, "ERR_AI_ROLLBACK_REJECTED")
	}
	recordAudit(c, "ai_model_rollback", "ai_model", modelID, "success",
		map[string]interface{}{"target_version": targetVersion})
	return OK(c, map[string]interface{}{
		"model_id":       modelID,
		"target_version": targetVersion,
		"status":         "rolled_back",
	})
}

func handleRollbackAIModel(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if strings.TrimSpace(modelID) == "" {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_MODEL_ID_REQUIRED", "model_id is required")
	}
	targetVersion, _ := req["target_version"].(string)
	return rollbackAIModelVersion(c, modelID, targetVersion)
}

// handleRollbackAIModelByID rolls back a model by path parameter (frontend: POST /ai/models/:id/rollback)
func handleRollbackAIModelByID(c echo.Context) error {
	modelID := c.Param("model_id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	targetVersion, _ := req["target_version"].(string)
	return rollbackAIModelVersion(c, modelID, targetVersion)
}

// handleUpdateAIModel updates a model by path parameter (frontend: PUT /ai/models/:id)

func handleUpdateAIModel(c echo.Context) error {
	// It bound the body, changed nothing and answered status=updated.
	return aiUnsupported(c, "ERR_AI_MODEL_UPDATE_UNSUPPORTED")
}

// handleReloadAIModelByID reloads a model by path parameter (frontend: POST /ai/models/:id/reload)
func handleReloadAIModelByID(c echo.Context) error {
	modelID := c.Param("model_id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelFilePath, _ := req["model_file_path"].(string)
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := cont.AIInference.ReloadModelFrom(modelID, modelFilePath); err != nil {
		// 502: the reload failed in the sidecar, not in this gateway.
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_MODEL_RELOAD_FAILED", "ERR_AI_MODEL_RELOAD_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "reloaded"})
}

// handleStartScheduledInferenceByID starts scheduled inference by path parameter (frontend: POST /ai/models/:id/schedule)
func handleStartScheduledInferenceByID(c echo.Context) error {
	modelID := c.Param("model_id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	interval, _ := req["interval"].(float64)
	inputWindowSize, _ := req["input_window_size"].(float64)
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	// The handler used to answer status=scheduled without telling anyone; the
	// dialog reported a loop that never existed.
	if err := cont.AIInference.StartScheduledInference(c.Request().Context(), modelID, "", "", int(interval), int(inputWindowSize)); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI scheduled inference start failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_SCHEDULED_START_FAILED", "ERR_AI_SCHEDULED_START_FAILED")
	}
	return OK(c, map[string]interface{}{
		"model_id":         modelID,
		"status":           "scheduled",
		"interval_seconds": int(interval),
	})
}

// handleStopScheduledInferenceByID stops scheduled inference by path parameter (frontend: DELETE /ai/models/:id/schedule)
func handleStopScheduledInferenceByID(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	if err := cont.AIInference.StopScheduledInference(c.Request().Context(), modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI scheduled inference stop failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_SCHEDULED_STOP_FAILED", "ERR_AI_SCHEDULED_STOP_FAILED")
	}
	return OK(c, map[string]interface{}{"model_id": modelID, "status": "unscheduled"})
}

func handleGeneratePresets(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_AI_SIDECAR_UNAVAILABLE", "AI inference engine not initialized")
	}
	available := cont.AIInference.IsSidecarAvailable()
	return OK(c, map[string]interface{}{
		"sidecar_available": available,
		"message":           "Preset model generation requested",
	})
}

func handleAIInfer(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "model_id is required")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_AI_SIDECAR_UNAVAILABLE", "AI inference engine not initialized")
	}
	// Pass the whole request: normalizeInput understands input_data as either a
	// numeric array or a map, and the handler must not pre-unwraps the one key
	// normalizeInput looks for (that dropped every array payload with a 500).
	result, err := cont.AIInference.RunInference(modelID, req)
	if err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI inference failed")
		return ErrorCode(c, http.StatusInternalServerError, "ERR_AI_INFERENCE_FAILED", err.Error())
	}
	return OK(c, result)
}

func handleStartScheduledInference(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION")
	}
	deviceID, _ := req["device_id"].(string)
	pointName, _ := req["point_name"].(string)
	interval, _ := req["interval_seconds"].(float64)
	windowSize, _ := req["input_window_size"].(float64)
	if err := cont.AIInference.StartScheduledInference(c.Request().Context(), modelID, deviceID, pointName, int(interval), int(windowSize)); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI scheduled inference start failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_SCHEDULED_START_FAILED", "ERR_AI_SCHEDULED_START_FAILED")
	}
	return OK(c, map[string]interface{}{"success": true, "model_id": modelID})
}

func handleStopScheduledInference(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	modelID, _ := req["model_id"].(string)
	if modelID == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION")
	}
	if err := cont.AIInference.StopScheduledInference(c.Request().Context(), modelID); err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Warn("AI scheduled inference stop failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_AI_SCHEDULED_STOP_FAILED", "ERR_AI_SCHEDULED_STOP_FAILED")
	}
	return OK(c, map[string]interface{}{"success": true, "model_id": modelID})
}

// selfLearningCtx bounds one sidecar round trip. The callers used to build their
// own detached 5s contexts inside the engine, so a client that gave up kept a
// sidecar request running with nothing left to report to.
func selfLearningCtx(c echo.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request().Context(), 5*time.Second)
}

// handleGetSelfLearningStats reports one model's learning state.
//
// It used to answer `data: null` both when the AI engine was missing and when
// the required device_id/point_name pair was absent, so the caller could not
// tell a bad request from a subsystem that is not running.
func handleGetSelfLearningStats(c echo.Context) error {
	deviceID := strings.TrimSpace(c.QueryParam("device_id"))
	pointName := strings.TrimSpace(c.QueryParam("point_name"))
	if deviceID == "" || pointName == "" {
		return selfLearningKeyRequired(c)
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	stats, _, err := cont.AIInference.GetSelfLearningStats(ctx, deviceID, pointName)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	return OK(c, stats)
}

func handleGetAllSelfLearningStats(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	stats, _, err := cont.AIInference.GetAllSelfLearningStats(ctx)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	if stats == nil {
		// An empty registry is a real answer; null makes the caller guess whether
		// it was measured at all.
		stats = []map[string]interface{}{}
	}
	return OK(c, stats)
}

func handleAddSelfLearningSample(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	deviceID, pointName, ok := selfLearningBodyKey(req)
	if !ok {
		return selfLearningKeyRequired(c)
	}
	// A missing or non-numeric value used to fall through as 0.0, which the
	// learner then treated as a real observation: one typo in a client poisoned
	// the model's mean, standard deviation and anomaly verdict for good.
	valueRaw, present := req["value"]
	if !present {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_SAMPLE_VALUE_REQUIRED", "value is required")
	}
	value, isNumber := valueRaw.(float64)
	if !isNumber || math.IsNaN(value) || math.IsInf(value, 0) {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_SAMPLE_VALUE_INVALID", "value must be a finite number")
	}
	windowSize := 100
	if raw, present := req["window_size"]; present {
		size, isNumber := raw.(float64)
		if !isNumber || size != math.Trunc(size) || size < 1 || size > engine.MaxSelfLearningWindow {
			return ErrorCode(c, http.StatusBadRequest, "ERR_AI_WINDOW_SIZE_INVALID",
				fmt.Sprintf("window_size must be an integer between 1 and %d", engine.MaxSelfLearningWindow))
		}
		windowSize = int(size)
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	isAnomaly, confidence, source, err := cont.AIInference.AddSample(ctx, deviceID, pointName, value, windowSize)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	return OK(c, map[string]interface{}{
		"is_anomaly": isAnomaly,
		"confidence": confidence,
		"source":     source,
	})
}

func handlePredictSelfLearning(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	deviceID, pointName, ok := selfLearningBodyKey(req)
	if !ok {
		return selfLearningKeyRequired(c)
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	predicted, confidence, source, err := cont.AIInference.Predict(ctx, deviceID, pointName)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	return OK(c, map[string]interface{}{
		"predicted_value": predicted,
		"confidence":      confidence,
		"source":          source,
	})
}

func handleResetSelfLearning(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	deviceID, pointName, ok := selfLearningBodyKey(req)
	if !ok {
		return selfLearningKeyRequired(c)
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	// The handler used to answer {"success": true} with the engine reference
	// never dereferenced for a result, so a reset that wiped nothing - or that
	// the sidecar refused - reported a wiped model.
	source, err := cont.AIInference.ResetSelfLearning(ctx, deviceID, pointName)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	return OK(c, map[string]interface{}{"success": true, "source": source})
}

func handleSetSelfLearningThreshold(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	deviceID, pointName, ok := selfLearningBodyKey(req)
	if !ok {
		return selfLearningKeyRequired(c)
	}
	// An absent threshold decoded as 0, and 0 is the opposite of a threshold:
	// every sample after it is more than 0 standard deviations from the mean, so
	// the call that "configured" the model turned it into an anomaly machine.
	thresholdRaw, present := req["threshold"]
	if !present {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_THRESHOLD_REQUIRED", "threshold is required")
	}
	threshold, isNumber := thresholdRaw.(float64)
	if !isNumber || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 {
		return ErrorCode(c, http.StatusBadRequest, "ERR_AI_THRESHOLD_INVALID", "threshold must be a finite number greater than 0")
	}
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	source, err := cont.AIInference.SetSelfLearningThreshold(ctx, deviceID, pointName, threshold)
	if err != nil {
		return selfLearningFailure(c, err)
	}
	return OK(c, map[string]interface{}{
		"success":   true,
		"threshold": threshold,
		"source":    source,
	})
}

// selfLearningBodyKey reads the (device_id, point_name) pair every self-learning
// request is keyed by.
func selfLearningBodyKey(req map[string]interface{}) (string, string, bool) {
	deviceID, _ := req["device_id"].(string)
	pointName, _ := req["point_name"].(string)
	deviceID = strings.TrimSpace(deviceID)
	pointName = strings.TrimSpace(pointName)
	if deviceID == "" || pointName == "" {
		return "", "", false
	}
	return deviceID, pointName, true
}

func selfLearningKeyRequired(c echo.Context) error {
	return ErrorCode(c, http.StatusBadRequest, "ERR_AI_SELF_LEARNING_KEY_REQUIRED",
		"device_id and point_name are required")
}

// selfLearningFailure answers a self-learning call that produced no result. A
// model that does not exist is its own case: it is not a request the caller got
// wrong, and not a subsystem that is down.
func selfLearningFailure(c echo.Context, err error) error {
	if errors.Is(err, engine.ErrSelfLearningNotFound) {
		return ErrorCode(c, http.StatusNotFound, "ERR_AI_SELF_LEARNING_NOT_FOUND",
			"no self-learning model for this device_id and point_name")
	}
	return sidecarFailure(c, err, "ERR_AI_SELF_LEARNING_REJECTED")
}

func handleAIHealthCheck(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return OK(c, map[string]interface{}{"healthy": false, "message": "AI inference engine not initialized"})
	}
	// version and execution_provider used to be constants here: the AI page
	// showed "ONNX Runtime / CPU" whether or not the sidecar was even running.
	status, err := cont.AIInference.SidecarHealth(c.Request().Context())
	if err != nil || status == nil {
		logrus.WithError(err).Debug("AI sidecar health unavailable")
		return OK(c, map[string]interface{}{
			"healthy":            false,
			"sidecar_available":  false,
			"version":            nil,
			"execution_provider": nil,
		})
	}
	return OK(c, map[string]interface{}{
		"healthy":            status.Healthy,
		"sidecar_available":  status.Healthy,
		"version":            status.Version,
		"execution_provider": status.ExecutionProvider,
		"onnx_available":     status.OnnxAvailable,
		"numpy_available":    status.NumpyAvailable,
		"loaded_model_count": status.LoadedModelCount,
	})
}

func handleGetExecutionProviders(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	providers, err := cont.AIInference.ExecutionProviders(c.Request().Context())
	if err != nil {
		// ["CPU"] was the honest-looking answer that hid a dead sidecar.
		logrus.WithError(err).Debug("AI execution provider list unavailable")
		return ServiceUnavailable(c, "ERR_AI_SIDECAR_UNAVAILABLE")
	}
	return OK(c, map[string]interface{}{"providers": providers})
}

func handleSetExecutionProvider(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	provider, _ := req["provider"].(string)
	if provider == "" {
		return BadRequest(c, "ERR_AI_PROVIDER_REQUIRED")
	}
	actual, err := cont.AIInference.SetExecutionProvider(c.Request().Context(), provider)
	if err != nil {
		logrus.WithError(err).Warn("AI execution provider switch failed")
		return ServiceUnavailable(c, "ERR_AI_SIDECAR_UNAVAILABLE")
	}
	return OK(c, map[string]interface{}{"success": true, "actual_provider": actual})
}

func handleGetAIStats(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	return OK(c, cont.AIInference.GetStats())
}

// handleGetAIMonitor returns inference engine monitoring data for the
// /system/ai-monitor page: engine counters, per-model status/stats and
// self-learning state.
func handleGetAIMonitor(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "AI inference engine not ready")
	}

	modelEntries := make([]map[string]interface{}, 0)
	for _, m := range cont.AIInference.ListModels() {
		entry := map[string]interface{}{
			"model_id":      m.ModelID,
			"model_name":    m.ModelName,
			"model_type":    m.ModelType,
			"model_version": m.ModelVersion,
			"status":        m.Status,
			"is_preset":     m.IsPreset,
		}
		if ms, err := cont.AIInference.GetModelStats(m.ModelID); err == nil {
			entry["inference_count"] = ms.InferenceCount
			entry["error_count"] = ms.ErrorCount
			entry["avg_latency_ms"] = ms.AvgLatencyMs
		}
		modelEntries = append(modelEntries, entry)
	}

	// The monitor page is one read of the whole AI subsystem, so a self-learning
	// registry the gateway could not reach is reported as a reason next to the
	// rows it does have, not as an empty list.
	ctx, cancel := selfLearningCtx(c)
	defer cancel()
	monitor := map[string]interface{}{
		"engine": cont.AIInference.GetStats(),
		"models": modelEntries,
	}
	selfLearning, source, err := cont.AIInference.GetAllSelfLearningStats(ctx)
	if err != nil {
		logrus.WithError(err).Debug("AI monitor self-learning registry unavailable")
		monitor["self_learning"] = []map[string]interface{}{}
		monitor["self_learning_error"] = "ERR_AI_SELF_LEARNING_UNAVAILABLE"
	} else {
		if selfLearning == nil {
			selfLearning = []map[string]interface{}{}
		}
		monitor["self_learning"] = selfLearning
		monitor["self_learning_source"] = source
	}
	return OK(c, monitor)
}

// --- Additional AI model handlers to match Python ai_models.py ---

func handleGetAISummary(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		// Zeros here were indistinguishable from an idle but healthy engine.
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	return OK(c, cont.AIInference.GetStats())
}

// handleGetInferenceLogs answers 501: this build has no inference-run store, so
// an empty page read as "no inferences recorded yet" instead of "not captured".
func handleGetInferenceLogs(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_AI_INFERENCE_LOGS_UNSUPPORTED",
		"ERR_AI_INFERENCE_LOGS_UNSUPPORTED")
}

func handleUploadAIModel(c echo.Context) error {
	file, err := c.FormFile("file")
	if err != nil {
		return BadRequest(c, "File upload required")
	}
	if file.Size > 100*1024*1024 {
		return BadRequest(c, "Model file too large (max 100MB)")
	}
	// The frontend has always sent `name`; the handler only read `model_name`,
	// so every upload was rejected with "model_name required".
	modelName := c.FormValue("model_name")
	if modelName == "" {
		modelName = c.FormValue("name")
	}

	if modelName == "" {
		return BadRequest(c, "model_name required")
	}

	// Security: validate file extension to prevent arbitrary file upload
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if !aiModelExtAllowed(ext) {
		return BadRequest(c, "Invalid file extension. Allowed: .onnx, .pt, .pth, .pb, .h5, .tflite, .joblib, .pkl, .tar, .gz, .zip")
	}

	// Security: sanitize model name to prevent path traversal — only allow
	// alphanumeric characters, hyphens, and underscores.
	for _, ch := range modelName {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return BadRequest(c, "model_name must contain only alphanumeric characters, hyphens, and underscores")
		}
	}

	// Open and validate the uploaded file
	src, err := file.Open()
	if err != nil {
		return InternalError(c, "ERR_AI_MODEL_FILE_OPEN_FAILED")
	}
	defer src.Close()

	// Read at most 512 bytes to detect content type for basic validation
	buf := make([]byte, 512)
	n, _ := src.Read(buf)
	detectedType := http.DetectContentType(buf[:n])
	// Rewind by seeking back to start
	src.Seek(0, io.SeekStart)

	// Reject obviously malicious content types
	maliciousTypes := map[string]bool{
		"text/html": true, "application/x-sh": true, "application/x-executable": true,
	}
	if maliciousTypes[detectedType] {
		logrus.WithField("model_name", modelName).
			WithField("content_type", detectedType).
			Warn("Model upload rejected: suspicious content type")
		return BadRequest(c, "File content type not allowed")
	}

	// Save to models directory
	modelsDir := aiModelsDir()
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return InternalError(c, "ERR_AI_MODEL_DIR_FAILED")
	}

	savePath := filepath.Join(modelsDir, modelName+ext)
	// Security: verify resolved path stays within modelsDir
	absSavePath, err := filepath.Abs(savePath)
	if err != nil {
		return InternalError(c, "ERR_AI_MODEL_PATH_FAILED")
	}
	absModelsDir, err := filepath.Abs(modelsDir)
	if err != nil {
		return InternalError(c, "ERR_AI_MODEL_PATH_FAILED")
	}
	if !strings.HasPrefix(absSavePath, absModelsDir+string(filepath.Separator)) {
		return BadRequest(c, "Invalid model path")
	}

	dst, err := os.Create(absSavePath)
	if err != nil {
		return InternalError(c, "ERR_AI_MODEL_SAVE_FAILED")
	}
	defer dst.Close()

	src.Seek(0, io.SeekStart)
	written, err := io.Copy(dst, src)
	if err != nil {
		return InternalError(c, "ERR_AI_MODEL_SAVE_FAILED")
	}

	logrus.WithFields(logrus.Fields{
		"model_name": modelName,
		"file_size":  written,
		"save_path":  absSavePath,
	}).Info("AI model uploaded")

	// Saving the file is not the same as making the model usable: the sidecar
	// has to load it, and the response says which of the two actually happened.
	loaded := false
	var loadError interface{}
	cont := GetContainer()
	switch {
	case cont.AIInference == nil:
		loadError = "ERR_AI_ENGINE_NOT_INITIALIZED"
	default:
		if err := cont.AIInference.LoadModelFile(modelName, absSavePath, c.FormValue("model_type")); err != nil {
			logrus.WithError(err).WithField("model_name", modelName).Warn("Uploaded model was not loaded by the sidecar")
			loadError = "ERR_AI_MODEL_LOAD_FAILED"
		} else {
			loaded = true
		}
	}
	return Created(c, map[string]interface{}{
		"model_name": modelName,
		"file_size":  written,
		"status":     "uploaded",
		"loaded":     loaded,
		"load_error": loadError,
	})
}

// aiModelsDir is where uploaded model files live.
func aiModelsDir() string { return filepath.Join("data", "models") }

// aiModelFileExts is the single source of truth for uploadable model
// extensions; the removal path uses the same list to find the file again.
var aiModelFileExts = []string{".onnx", ".pt", ".pth", ".pb", ".h5", ".tflite", ".joblib", ".pkl", ".tar", ".gz", ".zip"}

func aiModelExtAllowed(ext string) bool {
	for _, e := range aiModelFileExts {
		if e == ext {
			return true
		}
	}
	return false
}

func handleListScheduledInferences(c echo.Context) error {
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	// An always-empty list hid whether any schedule was running; this reports the
	// loops the gateway actually got the sidecar to start.
	return OK(c, cont.AIInference.ListScheduledInferences())
}

// aiUnsupported answers 501 with a stable ERR_* code. Used for the AI feature
// families that have no implementation in this build: they previously answered
// 200/201 with echoed or invented payloads, which a caller cannot tell from a
// real success.
func aiUnsupported(c echo.Context, code string) error {
	return ErrorCode(c, http.StatusNotImplemented, code, code)
}

// sidecarFailure answers a sidecar call that produced no result. A refusal means
// the sidecar is up and said no (the model or version is not there), which the
// operator has to be able to tell apart from a sidecar that is not running.
func sidecarFailure(c echo.Context, err error, rejectedCode string) error {
	if errors.Is(err, engine.ErrSidecarRejected) {
		return ErrorCode(c, http.StatusBadRequest, rejectedCode, err.Error())
	}
	logrus.WithError(err).Warn("AI sidecar call failed")
	return ServiceUnavailable(c, "ERR_AI_SIDECAR_UNAVAILABLE")
}

func handleCreateABTest(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleListABTests(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleGetABTest(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleABTestSplit(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleABTestPromote(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleABTestRollback(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_AB_TEST_UNSUPPORTED")
}

func handleHotSwapModel(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_HOT_SWAP_UNSUPPORTED")
}

func handleListHotSwaps(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_HOT_SWAP_UNSUPPORTED")
}

func handleSetPreprocessConfig(c echo.Context) error {
	// Nothing stores a per-model preprocess pipeline; the GET and the step
	// catalogue below answer the same way for the same reason.
	return aiUnsupported(c, "ERR_AI_PREPROCESS_UNSUPPORTED")
}

func handleGetPreprocessConfig(c echo.Context) error {
	// Answering 200 with an empty step list asserted "this model has a pipeline
	// and it happens to have no steps". Nothing stores a pipeline, so the read has
	// to say the same thing the write already says.
	return aiUnsupported(c, "ERR_AI_PREPROCESS_UNSUPPORTED")
}

func handleSetPostprocessConfig(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_POSTPROCESS_UNSUPPORTED")
}

func handleGetPostprocessConfig(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_POSTPROCESS_UNSUPPORTED")
}

func handleListPreprocessSteps(c echo.Context) error {
	// No engine code applies any of these names: the list was invented here, so
	// publishing it as a catalogue advertised steps that cannot be run.
	return aiUnsupported(c, "ERR_AI_PREPROCESS_UNSUPPORTED")
}

func handleListPostprocessSteps(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_POSTPROCESS_UNSUPPORTED")
}

// The gateway has an InferenceCache implementation but nothing constructs it, so
// there is no cache to report on or clear: zeros read as "cache is empty",
// cleared read as an operation that happened.
func handleGetCacheStats(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_CACHE_UNSUPPORTED")
}

func handleClearCache(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_CACHE_UNSUPPORTED")
}

// handleGetAIResources reports the host the inference engine runs on. The literal
// zeros it returned read as "an idle machine with no GPU", which nothing had
// measured: the percentages now come from the same gopsutil snapshots the status
// pages use, and GPU presence is the execution provider the sidecar actually
// activated — null when no sidecar answered to be asked.
func handleGetAIResources(c echo.Context) error {
	var cpuUsage interface{}
	if p, err := cpu.Percent(100*time.Millisecond, false); err == nil && len(p) > 0 {
		cpuUsage = p[0]
	}
	memTotal, _, memPercent := memorySnapshot()

	var gpuAvailable interface{}
	cont := GetContainer()
	if cont.AIInference != nil {
		// Far shorter than the client's own 30s HTTP timeout: this is one status
		// field of a dashboard read, not an operation the caller is waiting on.
		hctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		defer cancel()
		if status, err := cont.AIInference.SidecarHealth(hctx); err == nil {
			gpuAvailable = sidecarProviderHasGPU(status.ExecutionProvider)
		}
	}
	return OK(c, map[string]interface{}{
		"cpu_usage":     cpuUsage,
		"memory_usage":  percentOrUnmeasured(memTotal, memPercent),
		"gpu_available": gpuAvailable,
	})
}

// sidecarProviderHasGPU reads the provider the sidecar reports as active. Only the
// accelerator providers it names are a yes and only "CPU" is a no; an unrecognised
// provider stays null instead of being guessed at.
func sidecarProviderHasGPU(provider string) interface{} {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "cpu":
		return false
	case "cuda", "tensorrt", "rocm", "dml", "gpu":
		return true
	}
	return nil
}

func handleGetModelLatency(c echo.Context) error {
	modelID := c.Param("model_id")
	cont := GetContainer()
	if cont.AIInference == nil {
		return ServiceUnavailable(c, "ERR_AI_ENGINE_NOT_INITIALIZED")
	}
	// Zeros for avg/p50/p95/p99 were invented. Percentiles are not measured by
	// the sidecar, so they are gone; the average is null until a call exists.
	ms, err := cont.AIInference.GetModelStats(modelID)
	if err != nil {
		logrus.WithError(err).WithField("model_id", modelID).Debug("AI model latency unavailable")
		return ServiceUnavailable(c, "ERR_AI_SIDECAR_UNAVAILABLE")
	}
	var avg interface{}
	if ms.InferenceCount > 0 {
		avg = ms.AvgLatencyMs
	}
	return OK(c, map[string]interface{}{
		"model_id":        modelID,
		"inference_count": ms.InferenceCount,
		"error_count":     ms.ErrorCount,
		"avg_latency_ms":  avg,
	})
}

func handleListAIDevices(c echo.Context) error {
	return aiUnsupported(c, "ERR_AI_DEVICE_LIST_UNSUPPORTED")
}

func handleGetBatchStats(c echo.Context) error {
	// No batch inference path exists, so every counter would be an invented 0.
	return aiUnsupported(c, "ERR_AI_BATCH_STATS_UNSUPPORTED")
}

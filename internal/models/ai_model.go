package models

// AIModelInfo represents an AI model info response.
type AIModelInfo struct {
	ModelID      string                 `json:"model_id"`
	ModelName    string                 `json:"model_name"`
	ModelType    string                 `json:"model_type"`
	ModelPath    string                 `json:"model_path,omitempty"`
	Version      string                 `json:"version"`
	ModelVersion string                 `json:"model_version"`
	Status       string                 `json:"status"` // active, inactive, unavailable
	IsPreset     bool                   `json:"is_preset"`
	Config       map[string]interface{} `json:"config,omitempty"`
	Stats        map[string]interface{} `json:"stats,omitempty"`
	CreatedAt    string                 `json:"created_at,omitempty"`
	UpdatedAt    string                 `json:"updated_at,omitempty"`
}

// AIModelLoadRequest represents a model load request.
type AIModelLoadRequest struct {
	ModelPath string                 `json:"model_path"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

// AIInferenceRequest represents an inference request.
type AIInferenceRequest struct {
	ModelID   string                 `json:"model_id"`
	InputData map[string]interface{} `json:"input_data"`
}

// AIInferenceResponse represents an inference response.
type AIInferenceResponse struct {
	ModelID    string                 `json:"model_id"`
	Output     interface{}            `json:"output"`
	Confidence float64               `json:"confidence,omitempty"`
	LatencyMs  float64               `json:"latency_ms"`
}

// AIModelStats represents AI model statistics.
type AIModelStats struct {
	ModelID         string  `json:"model_id"`
	InferenceCount  int     `json:"inference_count"`
	ErrorCount      int     `json:"error_count"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	LastInferenceAt string  `json:"last_inference_at,omitempty"`
}

// AIModelCreateRequest represents a model creation request.
type AIModelCreateRequest struct {
	ModelID    string                 `json:"model_id"`
	ModelType  string                 `json:"model_type"`
	Version    string                 `json:"version"`
	Config     map[string]interface{} `json:"config,omitempty"`
}

// AIModelUpdateRequest represents a model update request.
type AIModelUpdateRequest struct {
	Version    string                 `json:"version,omitempty"`
	Config     map[string]interface{} `json:"config,omitempty"`
	Status     string                 `json:"status,omitempty"`
}

// AIModelDetailResponse represents a detailed model response.
type AIModelDetailResponse struct {
	AIModelInfo
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
	OutputSchema map[string]interface{} `json:"output_schema,omitempty"`
}

// ScheduleInferenceRequest represents a scheduled inference request.
type ScheduleInferenceRequest struct {
	ModelID    string                 `json:"model_id"`
	InputData  map[string]interface{} `json:"input_data"`
	ScheduleAt string                 `json:"schedule_at,omitempty"`
	Interval   float64                `json:"interval,omitempty"` // seconds, 0 = one-time
}

// AIInferenceLogResponse represents an inference log entry.
type AIInferenceLogResponse struct {
	ID         string                 `json:"id"`
	ModelID    string                 `json:"model_id"`
	InputData  map[string]interface{} `json:"input_data"`
	Output     interface{}            `json:"output"`
	Confidence float64                `json:"confidence,omitempty"`
	LatencyMs  float64                `json:"latency_ms"`
	Status     string                 `json:"status"`
	Error      string                 `json:"error,omitempty"`
	Timestamp  string                 `json:"timestamp"`
}

// ABTestCreateRequest represents an A/B test creation request.
type ABTestCreateRequest struct {
	TestID      string `json:"test_id"`
	ModelA      string `json:"model_a"`
	ModelB      string `json:"model_b"`
	TrafficSplit float64 `json:"traffic_split"` // 0-1, percentage to model B
	Description string `json:"description,omitempty"`
}

// ABTestResponse represents an A/B test response.
type ABTestResponse struct {
	TestID      string                 `json:"test_id"`
	ModelA      string                 `json:"model_a"`
	ModelB      string                 `json:"model_b"`
	TrafficSplit float64               `json:"traffic_split"`
	Status      string                 `json:"status"`
	StatsA      map[string]interface{} `json:"stats_a,omitempty"`
	StatsB      map[string]interface{} `json:"stats_b,omitempty"`
	CreatedAt   string                 `json:"created_at,omitempty"`
}

// HotSwapRequest represents a hot-swap model request.
type HotSwapRequest struct {
	OldModelID string `json:"old_model_id"`
	NewModelID string `json:"new_model_id"`
}

// PreprocessConfigUpdate represents a preprocessing config update.
type PreprocessConfigUpdate struct {
	ModelID string                 `json:"model_id"`
	Config  map[string]interface{} `json:"config"`
}

// PostprocessConfigUpdate represents a postprocessing config update.
type PostprocessConfigUpdate struct {
	ModelID string                 `json:"model_id"`
	Config  map[string]interface{} `json:"config"`
}

// AIModelVersion represents a model version.
type AIModelVersion struct {
	Version   string                 `json:"version"`
	CreatedAt string                 `json:"created_at"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

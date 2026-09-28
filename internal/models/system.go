package models

// LogEntry represents a log entry for aggregation.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Logger    string `json:"logger"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// LogLevelUpdate represents a log level update request.
type LogLevelUpdate struct {
	Level string `json:"level"`
}

// LogQuery represents a log query.
type LogQuery struct {
	Level    string `query:"level,omitempty"`
	Logger   string `query:"logger,omitempty"`
	Search   string `query:"search,omitempty"`
	StartTime string `query:"start_time,omitempty"`
	EndTime   string `query:"end_time,omitempty"`
	Page     int    `query:"page"`
	Size     int    `query:"size"`
}

// DBMonitorInfo represents database monitor information.
type DBMonitorInfo struct {
	Database   string                 `json:"database"`
	Size       int64                  `json:"size"`
	Tables     []map[string]interface{} `json:"tables"`
	SlowQueries []map[string]interface{} `json:"slow_queries,omitempty"`
}

// DataQualityReport represents a data quality report.
type DataQualityReport struct {
	DeviceID    string  `json:"device_id"`
	Score       float64 `json:"score"`
	CollectionRate float64 `json:"collection_rate"`
	Latency     float64 `json:"latency"`
	Completeness float64 `json:"completeness"`
	AnomalyRate float64 `json:"anomaly_rate"`
	Continuity  float64 `json:"continuity"`
}

// ShadowState represents a device shadow state.
type ShadowState struct {
	DeviceID    string                 `json:"device_id"`
	Reported    map[string]interface{} `json:"reported"`
	Desired     map[string]interface{} `json:"desired,omitempty"`
	LastReported string                `json:"last_reported,omitempty"`
	LastDesired  string                `json:"last_desired,omitempty"`
	Version     int                    `json:"version"`
}

// ShadowUpdateRequest represents a shadow desired state update.
type ShadowUpdateRequest struct {
	Desired map[string]interface{} `json:"desired"`
}

// ServiceInfo represents a service info for service manager.
type ServiceInfo struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // running, stopped, error
	PID     int    `json:"pid,omitempty"`
	Message string `json:"message,omitempty"`
}

// ServiceAction represents a service action request.
type ServiceAction struct {
	Action string `json:"action"` // start, stop, restart
}

// SCADAComponent represents a SCADA editor component.
type SCADAComponent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Props    map[string]interface{} `json:"props"`
	X        float64                `json:"x"`
	Y        float64                `json:"y"`
	Width    float64                `json:"width"`
	Height   float64                `json:"height"`
}

// SCADAScreen represents a SCADA screen.
type SCADAScreen struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Components []SCADAComponent `json:"components"`
	CreatedAt  string           `json:"created_at"`
	UpdatedAt  string           `json:"updated_at"`
}

// PreprocessRule represents a preprocessing rule.
type PreprocessRule struct {
	ID        string  `json:"id"`
	DeviceID  string  `json:"device_id"`
	PointName string  `json:"point_name"`
	Operation string  `json:"operation"` // scale, deadband, clamp, sqrt, aggregate, filter, etc.
	Params    map[string]interface{} `json:"params"`
	Enabled   bool    `json:"enabled"`
}

// ExpressionConfig represents an expression configuration.
type ExpressionConfig struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DeviceID string `json:"device_id,omitempty"`
	Expression string `json:"expression"`
	OutputPoint string `json:"output_point"`
	Enabled  bool   `json:"enabled"`
}

// ConfigVersion represents a config version snapshot.
type ConfigVersion struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by,omitempty"`
	Snapshot  map[string]interface{} `json:"snapshot"`
	ChangeSummary string `json:"change_summary,omitempty"`
}

// OTATask represents an OTA update task.
type OTATask struct {
	TaskID    string `json:"task_id"`
	DeviceID  string `json:"device_id"`
	Status    string `json:"status"` // pending, downloading, installing, success, failed
	Version   string `json:"version,omitempty"`
	Progress  int    `json:"progress,omitempty"`
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// DeviceLinkageRule represents a device linkage rule.
type DeviceLinkageRule struct {
	ID           string                 `json:"id"`
	SourceDevice string                 `json:"source_device"`
	SourcePoint  string                 `json:"source_point"`
	Condition    string                 `json:"condition"`
	TargetDevice string                 `json:"target_device"`
	TargetPoint  string                 `json:"target_point"`
	TargetValue  interface{}            `json:"target_value"`
	Enabled      bool                   `json:"enabled"`
}

// FirmwareSignature represents a firmware signature config.
type FirmwareSignature struct {
	ID        string `json:"id"`
	DeviceID  string `json:"device_id"`
	Algorithm string `json:"algorithm"` // rsa-2048, rsa-4096, ecdsa-p256, ecdsa-p384
	PublicKey string `json:"public_key"`
	Enabled   bool   `json:"enabled"`
}

// SystemStatusResponse represents a system status response.
type SystemStatusResponse struct {
	Status       string                 `json:"status"`
	Version      string                 `json:"version"`
	Uptime       float64                `json:"uptime"`
	Components   map[string]interface{} `json:"components"`
	Resources    map[string]interface{} `json:"resources,omitempty"`
}

// CascadeConfigRequest represents a cascade config request.
type CascadeConfigRequest struct {
	Enabled            bool                   `json:"enabled"`
	MaxConcurrentTasks int                    `json:"max_concurrent_tasks"`
	TaskTimeout        int                    `json:"task_timeout"` // seconds
	RetryCount         int                    `json:"retry_count"`
	RetryDelay         int                    `json:"retry_delay"` // seconds
	Custom             map[string]interface{} `json:"custom,omitempty"`
}

// ConfigSectionUpdateRequest represents a config section update request.
type ConfigSectionUpdateRequest struct {
	Section string                 `json:"section"`
	Values  map[string]interface{} `json:"values"`
}

// RetentionPolicyRequest represents a retention policy request.
type RetentionPolicyRequest struct {
	DataRetentionDays   int `json:"data_retention_days"`
	AlarmRetentionDays  int `json:"alarm_retention_days"`
	LogRetentionDays    int `json:"log_retention_days"`
	AuditRetentionDays  int `json:"audit_retention_days"`
}

// NtpConfigRequest represents an NTP configuration request.
type NtpConfigRequest struct {
	Enabled  bool     `json:"enabled"`
	Servers  []string `json:"servers"`
	Timezone string   `json:"timezone,omitempty"`
}

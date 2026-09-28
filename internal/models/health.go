package models

import "time"

// HealthStatus represents a health check result.
type HealthStatus struct {
	Status  string                 `json:"status"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// SystemInfo represents system information.
type SystemInfo struct {
	Version     string                 `json:"version"`
	StartTime   string                 `json:"start_time"`
	Uptime      float64                `json:"uptime"`
	DeviceInfo  map[string]interface{} `json:"device_info"`
	StorageInfo map[string]interface{} `json:"storage_info"`
	ProcessInfo map[string]interface{} `json:"process_info"`
}

// SystemStatus represents overall system status.
type SystemStatus struct {
	Status    string                 `json:"status"`
	Version   string                 `json:"version"`
	Uptime    float64                `json:"uptime"`
	Database  map[string]interface{} `json:"database"`
	InfluxDB  map[string]interface{} `json:"influxdb"`
	MQTT      map[string]interface{} `json:"mqtt"`
	Scheduler map[string]interface{} `json:"scheduler"`
	WebSocket map[string]interface{} `json:"websocket"`
	Drivers   map[string]interface{} `json:"drivers"`
}

// ComponentHealth represents a component's health status.
type ComponentHealth struct {
	Status      string                 `json:"status"`
	Message     string                 `json:"message,omitempty"`
	Details     map[string]interface{} `json:"details,omitempty"`
	LastChecked *time.Time             `json:"last_checked,omitempty"`
}

// HealthCheckResponse represents the health check response.
type HealthCheckResponse struct {
	Status     string                     `json:"status"`
	Components map[string]ComponentHealth `json:"components"`
}

// PerformanceData represents performance metrics.
type PerformanceData struct {
	CPUUsage       float64                `json:"cpu_usage"`
	MemoryUsage    float64                `json:"memory_usage"`
	GoroutineCount int                    `json:"goroutine_count"`
	GCStats        map[string]interface{} `json:"gc_stats,omitempty"`
}

// SystemResourcesResponse represents system resource information.
type SystemResourcesResponse struct {
	CPUUsage       float64                `json:"cpu_usage"`
	MemoryTotal    uint64                 `json:"memory_total"`
	MemoryUsed     uint64                 `json:"memory_used"`
	MemoryFree     uint64                 `json:"memory_free"`
	MemoryUsage    float64                `json:"memory_usage"`
	DiskTotal      uint64                 `json:"disk_total"`
	DiskUsed       uint64                 `json:"disk_used"`
	DiskFree       uint64                 `json:"disk_free"`
	DiskUsage      float64                `json:"disk_usage"`
	NetStats       map[string]interface{} `json:"net_stats,omitempty"`
	CPUCount       int                    `json:"cpu_count"`
	LoadAvg        map[string]float64     `json:"load_avg,omitempty"`
	GoroutineCount int                    `json:"goroutine_count"`
}

// DeviceHealthResponse represents device health status.
type DeviceHealthResponse struct {
	DeviceID               string                 `json:"device_id"`
	DeviceName             string                 `json:"device_name"`
	Protocol               string                 `json:"protocol"`
	Connected              bool                   `json:"connected"`
	State                  string                 `json:"state"`
	ReadErrorRate          float64                `json:"read_error_rate"`
	WriteErrorRate         float64                `json:"write_error_rate"`
	ConsecutiveFailures    int64                  `json:"consecutive_failures"`
	ConnectionQualityScore float64                `json:"connection_quality_score"`
	TotalDowntimeSeconds   float64                `json:"total_downtime_seconds"`
	AvgLatencyMs           float64                `json:"avg_latency_ms"`
	ReconnectCount         int64                  `json:"reconnect_count"`
	LastOnlineAt           *time.Time             `json:"last_online_at,omitempty"`
	LastOfflineAt          *time.Time             `json:"last_offline_at,omitempty"`
	DegradationReason      string                 `json:"degradation_reason,omitempty"`
	Details                map[string]interface{} `json:"details,omitempty"`
}

// DriverHealthResponse represents driver health status.
type DriverHealthResponse struct {
	DriverName       string                 `json:"driver_name"`
	Protocol         string                 `json:"protocol"`
	Version          string                 `json:"version"`
	DeviceCount      int                    `json:"device_count"`
	ConnectedDevices int                    `json:"connected_devices"`
	TotalReads       int64                  `json:"total_reads"`
	FailedReads      int64                  `json:"failed_reads"`
	TotalWrites      int64                  `json:"total_writes"`
	FailedWrites     int64                  `json:"failed_writes"`
	ReadErrorRate    float64                `json:"read_error_rate"`
	WriteErrorRate   float64                `json:"write_error_rate"`
	HealthScore      float64                `json:"health_score"`
	Devices          []DeviceHealthResponse `json:"devices,omitempty"`
}

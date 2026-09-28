package drivers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// DriverDisplayNames holds display names for drivers in multiple languages.
var DriverDisplayNames = map[string]map[string]string{
	"modbus_tcp":    {"en": "Modbus TCP", "zh": "Modbus TCP"},
	"modbus_rtu":    {"en": "Modbus RTU", "zh": "Modbus RTU"},
	"simulator":     {"en": "Simulator", "zh": "模拟器"},
	"mqtt_client":   {"en": "MQTT Client", "zh": "MQTT客户端"},
	"http_webhook":  {"en": "HTTP Webhook", "zh": "HTTP Webhook"},
	"opc_ua":        {"en": "OPC UA Client", "zh": "OPC UA客户端"},
	"opc_da":        {"en": "OPC DA Client", "zh": "OPC DA客户端"},
	"siemens_s7":    {"en": "Siemens S7", "zh": "西门子S7"},
	"s7":            {"en": "Siemens S7", "zh": "西门子S7"},
	"mitsubishi_mc": {"en": "Mitsubishi MC", "zh": "三菱MC"},
	"mc":            {"en": "Mitsubishi MC", "zh": "三菱MC"},
	"omron_fins":    {"en": "Omron FINS", "zh": "欧姆龙FINS"},
	"fins":          {"en": "Omron FINS", "zh": "欧姆龙FINS"},
	"allen_bradley": {"en": "Allen-Bradley", "zh": "Allen-Bradley"},
	"ab":            {"en": "Allen-Bradley", "zh": "Allen-Bradley"},
	"onvif":         {"en": "ONVIF Camera", "zh": "ONVIF摄像头"},
	"modbus_slave":  {"en": "Modbus Slave", "zh": "Modbus从站"},
}

// builtinProtocols is the set of built-in protocols that cannot be overridden by custom drivers.
var builtinProtocols = map[string]bool{
	"modbus_tcp":    true,
	"modbus_rtu":    true,
	"mqtt_client":   true,
	"http_webhook":  true,
	"opc_ua":        true,
	"opc_da":        true,
	"s7":            true,
	"siemens_s7":    true,
	"mc":            true,
	"mitsubishi_mc": true,
	"omron_fins":    true,
	"fins":          true,
	"allen_bradley": true,
	"ab":            true,
	"ab_cip":        true,
	"ab_pccc":       true,
	"onvif":         true,
	"modbus_slave":  true,
	"simulator":     true,
}

// GetDriverDisplayName returns the display name for a driver in the specified language.
func GetDriverDisplayName(pluginName, language string) string {
	if nameInfo, ok := DriverDisplayNames[pluginName]; ok {
		if name, ok := nameInfo[language]; ok {
			return name
		}
		if name, ok := nameInfo["en"]; ok {
			return name
		}
	}
	return pluginName
}

// DriverLoadStatus tracks the load status of a driver.
type DriverLoadStatus struct {
	Loaded bool
	Error  string
	Module string
	Class  string
}

// ExtendedRegistry extends the base Registry with additional tracking.
type ExtendedRegistry struct {
	*Registry
	mu                sync.RWMutex
	loadStatus        map[string]*DriverLoadStatus
	dependencyResults map[string]map[string]interface{}
}

// NewExtendedRegistry creates a new ExtendedRegistry.
func NewExtendedRegistry() *ExtendedRegistry {
	return &ExtendedRegistry{
		Registry:          GetRegistry(),
		loadStatus:        make(map[string]*DriverLoadStatus),
		dependencyResults: make(map[string]map[string]interface{}),
	}
}

// GetLoadStatus returns the load status for all drivers.
func (r *ExtendedRegistry) GetLoadStatus() map[string]*DriverLoadStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]*DriverLoadStatus, len(r.loadStatus))
	for k, v := range r.loadStatus {
		result[k] = v
	}
	return result
}

// SetLoadStatus sets the load status for a driver.
func (r *ExtendedRegistry) SetLoadStatus(name string, status *DriverLoadStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadStatus[name] = status
}

// GetDependencyResults returns dependency check results for all drivers.
func (r *ExtendedRegistry) GetDependencyResults() map[string]map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]map[string]interface{}, len(r.dependencyResults))
	for k, v := range r.dependencyResults {
		result[k] = v
	}
	return result
}

// IsBuiltinProtocol returns whether a protocol is a built-in protocol.
func IsBuiltinProtocol(protocol string) bool {
	return builtinProtocols[protocol]
}

// OTAManager manages Over-The-Air firmware updates for devices.
type OTAManager struct {
	mu   sync.Mutex
	jobs map[string]*OTAJob
}

// OTAJob represents an OTA firmware update job.
type OTAJob struct {
	JobID       string                 `json:"job_id"`
	DeviceID    string                 `json:"device_id"`
	FirmwareURL string                 `json:"firmware_url"`
	Version     string                 `json:"version"`
	Status      string                 `json:"status"`   // pending, downloading, verifying, applying, rebooting, success, failed
	Progress    float64                `json:"progress"` // 0-100
	StartedAt   *time.Time             `json:"started_at"`
	CompletedAt *time.Time             `json:"completed_at"`
	Error       string                 `json:"error"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// NewOTAManager creates a new OTAManager.
func NewOTAManager() *OTAManager {
	return &OTAManager{
		jobs: make(map[string]*OTAJob),
	}
}

// CreateJob creates a new OTA update job.
func (m *OTAManager) CreateJob(jobID, deviceID, firmwareURL, version string) *OTAJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	job := &OTAJob{
		JobID:       jobID,
		DeviceID:    deviceID,
		FirmwareURL: firmwareURL,
		Version:     version,
		Status:      "pending",
		Progress:    0,
		StartedAt:   &now,
		Metadata:    make(map[string]interface{}),
	}
	m.jobs[jobID] = job
	return job
}

// UpdateJobStatus updates the status of an OTA job.
func (m *OTAManager) UpdateJobStatus(jobID, status string, progress float64, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[jobID]
	if !ok {
		return fmt.Errorf("OTA job %s not found", jobID)
	}
	job.Status = status
	job.Progress = progress
	if errMsg != "" {
		job.Error = errMsg
	}
	if status == "success" || status == "failed" {
		now := time.Now()
		job.CompletedAt = &now
	}
	return nil
}

// GetJob returns an OTA job by ID.
func (m *OTAManager) GetJob(jobID string) (*OTAJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[jobID]
	if !ok {
		return nil, fmt.Errorf("OTA job %s not found", jobID)
	}
	return job, nil
}

// ListJobs returns all OTA jobs for a device.
func (m *OTAManager) ListJobs(deviceID string) []*OTAJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*OTAJob
	for _, job := range m.jobs {
		if deviceID == "" || job.DeviceID == deviceID {
			result = append(result, job)
		}
	}
	return result
}

// CancelJob cancels an OTA job.
func (m *OTAManager) CancelJob(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[jobID]
	if !ok {
		return fmt.Errorf("OTA job %s not found", jobID)
	}
	if job.Status == "success" || job.Status == "failed" {
		return fmt.Errorf("cannot cancel job in terminal state: %s", job.Status)
	}
	job.Status = "cancelled"
	now := time.Now()
	job.CompletedAt = &now
	return nil
}

// ExecuteUpdate runs the OTA update process.
func (m *OTAManager) ExecuteUpdate(ctx context.Context, jobID string, downloadFn func(ctx context.Context, url string) ([]byte, error), verifyFn func(data []byte) error, applyFn func(data []byte) error) error {
	job, err := m.GetJob(jobID)
	if err != nil {
		return err
	}

	// Download phase
	_ = m.UpdateJobStatus(jobID, "downloading", 10, "")
	data, err := downloadFn(ctx, job.FirmwareURL)
	if err != nil {
		_ = m.UpdateJobStatus(jobID, "failed", 10, fmt.Sprintf("download failed: %v", err))
		return err
	}
	_ = m.UpdateJobStatus(jobID, "downloading", 50, "")

	// Verify phase
	_ = m.UpdateJobStatus(jobID, "verifying", 60, "")
	if err := verifyFn(data); err != nil {
		_ = m.UpdateJobStatus(jobID, "failed", 60, fmt.Sprintf("verification failed: %v", err))
		return err
	}

	// Apply phase
	_ = m.UpdateJobStatus(jobID, "applying", 80, "")
	if err := applyFn(data); err != nil {
		_ = m.UpdateJobStatus(jobID, "failed", 80, fmt.Sprintf("apply failed: %v", err))
		return err
	}

	// Reboot phase
	_ = m.UpdateJobStatus(jobID, "rebooting", 90, "")
	// In a real implementation, this would trigger a device reboot

	// Success
	_ = m.UpdateJobStatus(jobID, "success", 100, "")
	logrus.WithField("job_id", jobID).Info("OTA update completed successfully")
	return nil
}

// TimeSeriesStore provides time-series data storage for driver measurements.
type TimeSeriesStore struct {
	mu            sync.Mutex
	data          map[string][]TimeSeriesEntry // "device:point" -> entries
	retentionDays int
	maxEntries    int
}

// TimeSeriesEntry represents a single time-series data point.
type TimeSeriesEntry struct {
	Timestamp time.Time
	Value     interface{}
	Quality   string
}

// NewTimeSeriesStore creates a new TimeSeriesStore.
func NewTimeSeriesStore(retentionDays int) *TimeSeriesStore {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	return &TimeSeriesStore{
		data:          make(map[string][]TimeSeriesEntry),
		retentionDays: retentionDays,
		maxEntries:    100000, // Max entries per point
	}
}

// WriteReadResult writes a read result to the time series store.
func (s *TimeSeriesStore) WriteReadResult(deviceID string, result map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for pointName, value := range result {
		key := deviceID + ":" + pointName
		entry := TimeSeriesEntry{
			Timestamp: now,
			Value:     value,
			Quality:   "good",
		}
		entries := s.data[key]
		entries = append(entries, entry)
		// Trim if over max entries
		if len(entries) > s.maxEntries {
			entries = entries[len(entries)-s.maxEntries:]
		}
		s.data[key] = entries
	}
}

// Query queries time-series data.
func (s *TimeSeriesStore) Query(deviceID, pointName string, startTime, endTime *time.Time, limit int) []TimeSeriesEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deviceID + ":" + pointName
	entries := s.data[key]
	var result []TimeSeriesEntry
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if startTime != nil && entry.Timestamp.Before(*startTime) {
			continue
		}
		if endTime != nil && entry.Timestamp.After(*endTime) {
			continue
		}
		result = append([]TimeSeriesEntry{entry}, result...)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

// QueryLatest returns the latest values for a set of points.
func (s *TimeSeriesStore) QueryLatest(deviceID string, pointNames []string) map[string]map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]map[string]interface{})
	for _, pointName := range pointNames {
		key := deviceID + ":" + pointName
		entries := s.data[key]
		if len(entries) > 0 {
			latest := entries[len(entries)-1]
			result[pointName] = map[string]interface{}{
				"value":     latest.Value,
				"timestamp": latest.Timestamp.Format(time.RFC3339),
				"quality":   latest.Quality,
			}
		}
	}
	return result
}

// QueryByQuality queries data by quality.
func (s *TimeSeriesStore) QueryByQuality(deviceID, pointName, quality string, limit int) []TimeSeriesEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deviceID + ":" + pointName
	entries := s.data[key]
	var result []TimeSeriesEntry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Quality == quality {
			result = append([]TimeSeriesEntry{entries[i]}, result...)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result
}

// CleanupOld removes entries older than the retention period.
func (s *TimeSeriesStore) CleanupOld() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
	removed := 0
	for key, entries := range s.data {
		var kept []TimeSeriesEntry
		for _, entry := range entries {
			if entry.Timestamp.After(cutoff) {
				kept = append(kept, entry)
			} else {
				removed++
			}
		}
		s.data[key] = kept
	}
	return removed
}

// GetStats returns statistics about the time series store.
func (s *TimeSeriesStore) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	totalEntries := 0
	pointCount := len(s.data)
	for _, entries := range s.data {
		totalEntries += len(entries)
	}
	return map[string]interface{}{
		"total_entries":  totalEntries,
		"point_count":    pointCount,
		"retention_days": s.retentionDays,
	}
}

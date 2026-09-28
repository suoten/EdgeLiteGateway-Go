package drivers

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ConfigVersionSnapshot represents a configuration snapshot.
type ConfigVersionSnapshot struct {
	Version     int                    `json:"version"`
	DeviceID    string                 `json:"device_id"`
	Timestamp   time.Time              `json:"timestamp"`
	Config      map[string]interface{} `json:"config"`
	ChangedKeys []string               `json:"changed_keys"`
	User        string                 `json:"user"`
}

// ConfigVersionManager manages configuration versioning and rollback.
type ConfigVersionManager struct {
	mu        sync.Mutex
	dbPath    string
	versions  map[string][]*ConfigVersionSnapshot // deviceID -> versions
	nextVer   map[string]int                       // deviceID -> next version number
}

// NewConfigVersionManager creates a new ConfigVersionManager.
func NewConfigVersionManager(dbPath string) *ConfigVersionManager {
	return &ConfigVersionManager{
		dbPath:   dbPath,
		versions: make(map[string][]*ConfigVersionSnapshot),
		nextVer:  make(map[string]int),
	}
}

// SnapshotDeviceConfig takes a snapshot of the current device configuration.
func (m *ConfigVersionManager) SnapshotDeviceConfig(deviceID string, config map[string]interface{}, changedKeys []string, user string) *ConfigVersionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	version := m.nextVer[deviceID]
	if version == 0 {
		version = 1
	}
	m.nextVer[deviceID] = version + 1

	// Deep copy config
	configCopy := deepCopyMap(config)

	snapshot := &ConfigVersionSnapshot{
		Version:     version,
		DeviceID:    deviceID,
		Timestamp:   time.Now(),
		Config:      configCopy,
		ChangedKeys: changedKeys,
		User:        user,
	}

	m.versions[deviceID] = append(m.versions[deviceID], snapshot)
	return snapshot
}

// Rollback rolls back to a specific version.
func (m *ConfigVersionManager) Rollback(deviceID string, version int) (*ConfigVersionSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.versions[deviceID]
	for _, v := range versions {
		if v.Version == version {
			return v, nil
		}
	}
	return nil, fmt.Errorf("version %d not found for device %s", version, deviceID)
}

// ListVersions returns all versions for a device.
func (m *ConfigVersionManager) ListVersions(deviceID string, limit, offset int) []*ConfigVersionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.versions[deviceID]
	if offset >= len(versions) {
		return nil
	}
	start := offset
	end := offset + limit
	if end > len(versions) {
		end = len(versions)
	}
	if start > end {
		return nil
	}
	result := make([]*ConfigVersionSnapshot, end-start)
	copy(result, versions[start:end])
	// Reverse to get most recent first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// GetVersion returns a specific version.
func (m *ConfigVersionManager) GetVersion(deviceID string, version int) (*ConfigVersionSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.versions[deviceID]
	for _, v := range versions {
		if v.Version == version {
			return v, nil
		}
	}
	return nil, fmt.Errorf("version %d not found for device %s", version, deviceID)
}

// DiffVersions returns the diff between two versions.
func (m *ConfigVersionManager) DiffVersions(deviceID string, v1, v2 int) (map[string]interface{}, error) {
	snap1, err := m.GetVersion(deviceID, v1)
	if err != nil {
		return nil, err
	}
	snap2, err := m.GetVersion(deviceID, v2)
	if err != nil {
		return nil, err
	}
	diff := map[string]interface{}{
		"v1":          v1,
		"v2":          v2,
		"v1_timestamp": snap1.Timestamp.Format(time.RFC3339),
		"v2_timestamp": snap2.Timestamp.Format(time.RFC3339),
		"changes":     deepDiffKeys(snap1.Config, snap2.Config),
	}
	return diff, nil
}

// ExportJSON exports all versions as JSON.
func (m *ConfigVersionManager) ExportJSON(deviceID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.versions[deviceID]
	data, err := json.MarshalIndent(versions, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ImportJSON imports versions from JSON.
func (m *ConfigVersionManager) ImportJSON(deviceID string, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var versions []*ConfigVersionSnapshot
	if err := json.Unmarshal([]byte(data), &versions); err != nil {
		return err
	}
	m.versions[deviceID] = versions
	for _, v := range versions {
		if v.Version >= m.nextVer[deviceID] {
			m.nextVer[deviceID] = v.Version + 1
		}
	}
	return nil
}

// deepCopyMap creates a deep copy of a map.
func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = deepCopyMap(val)
		case []interface{}:
			result[k] = deepCopySlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func deepCopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = deepCopyMap(val)
		case []interface{}:
			result[i] = deepCopySlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// deepDiffKeys finds keys that differ between two maps.
func deepDiffKeys(old, new map[string]interface{}) []string {
	var diff []string
	checked := make(map[string]bool)
	for k := range old {
		checked[k] = true
		newVal, exists := new[k]
		if !exists {
			diff = append(diff, k)
			continue
		}
		if !equalValues(old[k], newVal) {
			diff = append(diff, k)
		}
	}
	for k := range new {
		if !checked[k] {
			diff = append(diff, k)
		}
	}
	return diff
}

func equalValues(a, b interface{}) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// LinkRole represents the role of a link in a redundancy pair.
type LinkRole string

const (
	LinkRolePrimary LinkRole = "primary"
	LinkRoleBackup  LinkRole = "backup"
)

// RedundancyConfig holds configuration for link redundancy.
type RedundancyConfig struct {
	PrimaryHost      string
	BackupHost       string
	SwitchThreshold   int  // consecutive failures before switching
	SwitchbackDelay  int  // seconds before switching back to primary
	HealthCheckInterval int // seconds between health checks
}

// deviceRedundancyState tracks redundancy state for a device.
type deviceRedundancyState struct {
	mu                 sync.Mutex
	activeRole         LinkRole
	consecutiveFailures int
	switchbackTimer    *time.Timer
}

// LinkRedundancyManager manages link redundancy for devices.
type LinkRedundancyManager struct {
	mu       sync.Mutex
	states   map[string]*deviceRedundancyState
	configs  map[string]*RedundancyConfig
	onSwitch func(deviceID, fromHost, toHost string)
}

// NewLinkRedundancyManager creates a new LinkRedundancyManager.
func NewLinkRedundancyManager() *LinkRedundancyManager {
	return &LinkRedundancyManager{
		states:  make(map[string]*deviceRedundancyState),
		configs: make(map[string]*RedundancyConfig),
	}
}

// SetOnSwitchCallback sets the callback for switch events.
func (m *LinkRedundancyManager) SetOnSwitchCallback(cb func(deviceID, fromHost, toHost string)) {
	m.onSwitch = cb
}

// RegisterDevice registers a device for link redundancy management.
func (m *LinkRedundancyManager) RegisterDevice(deviceID string, config *RedundancyConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[deviceID] = config
	m.states[deviceID] = &deviceRedundancyState{
		activeRole: LinkRolePrimary,
	}
}

// UnregisterDevice removes a device from redundancy management.
func (m *LinkRedundancyManager) UnregisterDevice(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.configs, deviceID)
	if state, ok := m.states[deviceID]; ok {
		state.mu.Lock()
		if state.switchbackTimer != nil {
			state.switchbackTimer.Stop()
		}
		state.mu.Unlock()
	}
	delete(m.states, deviceID)
}

// RecordSuccess records a successful operation for the device.
func (m *LinkRedundancyManager) RecordSuccess(deviceID string) {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	m.mu.Unlock()
	if !ok {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.consecutiveFailures = 0
}

// RecordFailure records a failed operation for the device.
func (m *LinkRedundancyManager) RecordFailure(deviceID string) {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	config := m.configs[deviceID]
	m.mu.Unlock()
	if !ok || config == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.consecutiveFailures++
	if state.activeRole == LinkRolePrimary && state.consecutiveFailures >= config.SwitchThreshold {
		m.switchToBackupLocked(deviceID, state, config)
	}
}

// GetActiveRole returns the active link role for a device.
func (m *LinkRedundancyManager) GetActiveRole(deviceID string) LinkRole {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	m.mu.Unlock()
	if !ok {
		return LinkRolePrimary
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.activeRole
}

// GetActiveHost returns the active host for a device.
func (m *LinkRedundancyManager) GetActiveHost(deviceID string) string {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	config := m.configs[deviceID]
	m.mu.Unlock()
	if !ok || config == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.activeRole == LinkRoleBackup {
		return config.BackupHost
	}
	return config.PrimaryHost
}

// GetStatus returns the redundancy status for a device.
func (m *LinkRedundancyManager) GetStatus(deviceID string) map[string]interface{} {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	config := m.configs[deviceID]
	m.mu.Unlock()
	if !ok || config == nil {
		return map[string]interface{}{
			"enabled": false,
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return map[string]interface{}{
		"enabled":              true,
		"active_role":          string(state.activeRole),
		"primary_host":         config.PrimaryHost,
		"backup_host":          config.BackupHost,
		"consecutive_failures": state.consecutiveFailures,
		"switch_threshold":    config.SwitchThreshold,
	}
}

// MarkPrimaryHealthy marks the primary link as healthy, potentially switching back.
func (m *LinkRedundancyManager) MarkPrimaryHealthy(deviceID string) {
	m.mu.Lock()
	state, ok := m.states[deviceID]
	config := m.configs[deviceID]
	m.mu.Unlock()
	if !ok || config == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.activeRole == LinkRoleBackup {
		m.switchToPrimaryLocked(deviceID, state, config)
	}
}

func (m *LinkRedundancyManager) switchToBackupLocked(deviceID string, state *deviceRedundancyState, config *RedundancyConfig) {
	oldHost := config.PrimaryHost
	newHost := config.BackupHost
	state.activeRole = LinkRoleBackup
	state.consecutiveFailures = 0
	logrus.Infof("Link redundancy: switching device %s from primary (%s) to backup (%s)", deviceID, oldHost, newHost)
	if m.onSwitch != nil {
		m.onSwitch(deviceID, oldHost, newHost)
	}
}

func (m *LinkRedundancyManager) switchToPrimaryLocked(deviceID string, state *deviceRedundancyState, config *RedundancyConfig) {
	oldHost := config.BackupHost
	newHost := config.PrimaryHost
	state.activeRole = LinkRolePrimary
	state.consecutiveFailures = 0
	logrus.Infof("Link redundancy: switching device %s from backup (%s) to primary (%s)", deviceID, oldHost, newHost)
	if m.onSwitch != nil {
		m.onSwitch(deviceID, oldHost, newHost)
	}
}

// Stop stops all redundancy management.
func (m *LinkRedundancyManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, state := range m.states {
		state.mu.Lock()
		if state.switchbackTimer != nil {
			state.switchbackTimer.Stop()
		}
		state.mu.Unlock()
	}
}

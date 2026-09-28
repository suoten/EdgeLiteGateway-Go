package drivers

import (
	"math"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ConnectionState represents the connection state of a device.
type ConnectionState string

const (
	StateDisconnected  ConnectionState = "disconnected"
	StateConnecting   ConnectionState = "connecting"
	StatePDUNegotiating ConnectionState = "pdu_negotiating"
	StateCIPNegotiating ConnectionState = "cip_negotiating"
	StateConnected    ConnectionState = "connected"
	StateDegraded     ConnectionState = "degraded"
	StateOffline      ConnectionState = "offline"
)

// validStateTransitions defines allowed state transitions.
var validStateTransitions = map[ConnectionState]map[ConnectionState]bool{
	StateDisconnected: {
		StateConnecting: true,
		StateConnected:  true,
		StateOffline:    true,
	},
	StateConnecting: {
		StateConnected:      true,
		StateDisconnected:   true,
		StatePDUNegotiating: true,
		StateCIPNegotiating: true,
		StateOffline:        true,
	},
	StatePDUNegotiating: {
		StateCIPNegotiating: true,
		StateConnected:      true,
		StateDisconnected:   true,
		StateOffline:        true,
	},
	StateCIPNegotiating: {
		StateConnected:    true,
		StateDisconnected: true,
		StateOffline:      true,
	},
	StateConnected: {
		StateDisconnected: true,
		StateDegraded:     true,
		StateOffline:      true,
		// A live socket can die under us (peer restart, network reset). The
		// reconnect path dials afresh and needs to record Connecting first;
		// rejecting it wedged the driver in a phantom "connected" state with a
		// dead socket, so the circuit breaker never recovered (94+ consecutive
		// collection failures while pylogix connected fine).
		StateConnecting: true,
	},
	StateDegraded: {
		StateConnected:    true,
		StateDisconnected: true,
		StateOffline:      true,
	},
	StateOffline: {
		StateConnecting:   true,
		StateDisconnected: true,
	},
}

// DriverHealthStats tracks health statistics for a device.
type DriverHealthStats struct {
	mu                      sync.Mutex
	DeviceID                string
	TotalReads              int64
	FailedReads             int64
	TotalWrites             int64
	FailedWrites            int64
	TotalFailures           int64
	LastSuccessRead         *time.Time
	LastFailedRead          *time.Time
	ConsecutiveFailures     int64
	TotalDowntimeSeconds    float64
	LastOnlineAt            *time.Time
	LastOfflineAt          *time.Time
	ConnectionQualityScore  float64
	TotalReconnects        int64
	AvgLatencyMs           float64
	DegradationReason      string
	latencySamples         []float64
	maxLatencySamples      int
	movingAvgWindow        int
}

// NewDriverHealthStats creates a new DriverHealthStats.
func NewDriverHealthStats(deviceID string) *DriverHealthStats {
	return &DriverHealthStats{
		DeviceID:               deviceID,
		ConnectionQualityScore: 100.0,
		maxLatencySamples:      100,
		movingAvgWindow:        20,
	}
}

// ReadErrorRate returns the read error rate (0.0 to 1.0).
func (s *DriverHealthStats) ReadErrorRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.TotalReads == 0 {
		return 0.0
	}
	return float64(s.FailedReads) / float64(s.TotalReads)
}

// WriteErrorRate returns the write error rate (0.0 to 1.0).
func (s *DriverHealthStats) WriteErrorRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.TotalWrites == 0 {
		return 0.0
	}
	return float64(s.FailedWrites) / float64(s.TotalWrites)
}

// RecordLatency appends one latency sample. A driver that has no round trip to
// time -- a subscriber-style read that drains a local cache, a keep-alive write
// -- passes LatencyNotMeasured so the panels keep reporting null instead of a
// 0 that reads as a perfect link.
func (s *DriverHealthStats) RecordLatency(latencyMs float64) {
	if latencyMs < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencySamples = append(s.latencySamples, latencyMs)
	if len(s.latencySamples) > s.maxLatencySamples {
		s.latencySamples = s.latencySamples[1:]
	}
	n := len(s.latencySamples)
	window := s.movingAvgWindow
	if n < window {
		window = n
	}
	if window > 0 {
		sum := 0.0
		for i := n - window; i < n; i++ {
			sum += s.latencySamples[i]
		}
		s.AvgLatencyMs = sum / float64(window)
	}
}

// LatencySamples returns the latency samples still retained, oldest first, so a
// caller can chart what was measured instead of being handed an empty series and
// reading it as "no latency problems".
func (s *DriverHealthStats) LatencySamples() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]float64, len(s.latencySamples))
	copy(out, s.latencySamples)
	return out
}

// LatencyWindowCapacity reports how many samples this device's rolling window
// retains, so an aggregate can state that it charts recent traffic rather than
// all-time history.
func (s *DriverHealthStats) LatencyWindowCapacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxLatencySamples
}

// P95LatencyMs returns the 95th percentile latency.
func (s *DriverHealthStats) P95LatencyMs() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.latencySamples) == 0 {
		return 0.0
	}
	sorted := make([]float64, len(s.latencySamples))
	copy(sorted, s.latencySamples)
	sortFloat64s(sorted)
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// HealthCounters is a lock-consistent copy of the counters a driver records.
// The fields of DriverHealthStats are written by the collection goroutines, so an
// HTTP handler that reads them straight off the pointer races with every cycle;
// this is the accessor meant for reporting paths.
type HealthCounters struct {
	TotalReads             int64
	FailedReads            int64
	TotalWrites            int64
	FailedWrites           int64
	TotalReconnects        int64
	ConsecutiveFailures    int64
	ConnectionQualityScore float64
	AvgLatencyMs           float64
	DegradationReason      string
	HasLatencySample       bool
	TotalDowntimeSeconds   float64
	LastOnlineAt           *time.Time
	LastOfflineAt          *time.Time
}

// Counters copies the counters under the stats lock.
func (s *DriverHealthStats) Counters() HealthCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return HealthCounters{
		TotalReads:             s.TotalReads,
		FailedReads:            s.FailedReads,
		TotalWrites:            s.TotalWrites,
		FailedWrites:           s.FailedWrites,
		TotalReconnects:        s.TotalReconnects,
		ConsecutiveFailures:    s.ConsecutiveFailures,
		ConnectionQualityScore: s.ConnectionQualityScore,
		AvgLatencyMs:           s.AvgLatencyMs,
		DegradationReason:      s.DegradationReason,
		HasLatencySample:       len(s.latencySamples) > 0,
		TotalDowntimeSeconds:   s.TotalDowntimeSeconds,
		LastOnlineAt:           s.LastOnlineAt,
		LastOfflineAt:          s.LastOfflineAt,
	}
}

// IsHealthy returns whether the device is healthy.
func (s *DriverHealthStats) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ConsecutiveFailures < 5 && (s.TotalReads == 0 || float64(s.FailedReads)/float64(s.TotalReads) < 0.1)
}

// HealthScore returns a health score (0-100).
func (s *DriverHealthStats) HealthScore() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	score := 100.0
	// Deduct for consecutive failures (max 40)
	deductConsec := float64(s.ConsecutiveFailures) * 10
	if deductConsec > 40 {
		deductConsec = 40
	}
	score -= deductConsec

	// Deduct for read error rate (max 30)
	var readErrRate float64
	if s.TotalReads > 0 {
		readErrRate = float64(s.FailedReads) / float64(s.TotalReads)
	}
	deductErrRate := readErrRate * 150
	if deductErrRate > 30 {
		deductErrRate = 30
	}
	score -= deductErrRate

	// Deduct for high latency (max 20)
	if s.AvgLatencyMs > 1000 {
		deductLatency := (s.AvgLatencyMs - 1000) / 100
		if deductLatency > 20 {
			deductLatency = 20
		}
		score -= deductLatency
	}

	// Deduct for reconnects (max 10)
	deductReconn := float64(s.TotalReconnects) * 2
	if deductReconn > 10 {
		deductReconn = 10
	}
	score -= deductReconn

	if score < 0 {
		score = 0
	}
	return score
}

// EffectiveState returns the effective connection state based on stats.
func (s *DriverHealthStats) EffectiveState() ConnectionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var readErrRate float64
	if s.TotalReads > 0 {
		readErrRate = float64(s.FailedReads) / float64(s.TotalReads)
	}
	if s.ConsecutiveFailures == 0 && readErrRate < 0.1 {
		return StateConnected
	}
	if s.ConsecutiveFailures >= 5 {
		return StateOffline
	}
	if s.ConnectionQualityScore < 50 {
		return StateDegraded
	}
	return StateDegraded
}

// ConnectionStatus represents detailed connection status.
type ConnectionStatus struct {
	State     ConnectionState
	Reason    string
	Since     *time.Time
	LastError string
	Metrics   map[string]interface{}
}

// DriverCapabilities declares what a driver can do.
type DriverCapabilities struct {
	Discover   bool
	Read       bool
	Write      bool
	Subscribe  bool
	BatchRead  bool
	BatchWrite bool
}

// ConfigValidationResult holds config validation results.
type ConfigValidationResult struct {
	Valid   bool
	Errors  []string
	Warnings []string
}

// PointValue is a unified point value envelope.
type PointValue struct {
	Value     interface{}
	Timestamp *time.Time
	Quality   string // "good", "bad", "uncertain"
	Source    string // "device", "cache", "simulated", "subscribed"
	LatencyMs float64
}

// HealthStatsManager manages health stats for multiple devices.
type HealthStatsManager struct {
	mu          sync.RWMutex
	stats       map[string]*DriverHealthStats
	offlineSince map[string]time.Time
	connStates  map[string]*ConnectionStatus
}

// NewHealthStatsManager creates a new HealthStatsManager.
func NewHealthStatsManager() *HealthStatsManager {
	return &HealthStatsManager{
		stats:        make(map[string]*DriverHealthStats),
		offlineSince: make(map[string]time.Time),
		connStates:   make(map[string]*ConnectionStatus),
	}
}

// GetHealthStats returns the health stats for a device.
func (m *HealthStatsManager) GetHealthStats(deviceID string) *DriverHealthStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats[deviceID]
}

// GetAllHealthStats returns all device health stats.
func (m *HealthStatsManager) GetAllHealthStats() map[string]*DriverHealthStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]*DriverHealthStats, len(m.stats))
	for k, v := range m.stats {
		result[k] = v
	}
	return result
}

// ResetHealthStats resets health stats for a device.
func (m *HealthStatsManager) ResetHealthStats(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stats, deviceID)
	delete(m.offlineSince, deviceID)
	delete(m.connStates, deviceID)
}

// RecordReadSuccess records a successful read.
func (m *HealthStatsManager) RecordReadSuccess(deviceID string, latencyMs float64) {
	// Take m.mu once to both fetch/create stats and claim the offline window.
	// m.offlineSince is read+deleted here, under m.mu, because RecordReadFailure
	// writes it under the same lock; reading it while only holding stats.mu (the
	// old code) raced against that write and could crash the whole process with a
	// concurrent map read/write, since this manager is shared across every device.
	m.mu.Lock()
	stats, ok := m.stats[deviceID]
	if !ok {
		stats = NewDriverHealthStats(deviceID)
		stats.DeviceID = deviceID
		m.stats[deviceID] = stats
	}
	offlineSince, hadOffline := m.offlineSince[deviceID]
	if hadOffline {
		delete(m.offlineSince, deviceID)
	}
	m.mu.Unlock()

	stats.mu.Lock()
	stats.TotalReads++
	now := time.Now()
	stats.LastSuccessRead = &now
	// A completed read is the evidence that the device is online, so the field
	// that says "last seen online" has to be written here; it never was, which
	// left last_online_at and last_check null on every talking device.
	stats.LastOnlineAt = &now
	stats.ConsecutiveFailures = 0
	if hadOffline {
		stats.TotalDowntimeSeconds += time.Since(offlineSince).Seconds()
		stats.LastOfflineAt = nil
	}
	stats.mu.Unlock()

	stats.RecordLatency(latencyMs)
	m.evaluateDegradation(deviceID)
}

// RecordReadFailure records a failed read.
func (m *HealthStatsManager) RecordReadFailure(deviceID string) {
	m.mu.Lock()
	stats, ok := m.stats[deviceID]
	if !ok {
		stats = NewDriverHealthStats(deviceID)
		stats.DeviceID = deviceID
		m.stats[deviceID] = stats
	}
	m.mu.Unlock()

	stats.mu.Lock()
	stats.TotalReads++
	stats.FailedReads++
	now := time.Now()
	stats.LastFailedRead = &now
	stats.ConsecutiveFailures++
	stats.mu.Unlock()

	m.mu.Lock()
	if _, exists := m.offlineSince[deviceID]; !exists {
		m.offlineSince[deviceID] = time.Now()
		stats.mu.Lock()
		stats.LastOfflineAt = &now
		stats.mu.Unlock()
	}
	m.mu.Unlock()

	m.updateConnectionQuality(deviceID)
	m.evaluateDegradation(deviceID)
}

// RecordWriteSuccess records a successful write.
func (m *HealthStatsManager) RecordWriteSuccess(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats, ok := m.stats[deviceID]
	if !ok {
		stats = NewDriverHealthStats(deviceID)
		stats.DeviceID = deviceID
		m.stats[deviceID] = stats
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.TotalWrites++
	// A write that reached the peer is just as much proof of life as a read.
	now := time.Now()
	stats.LastOnlineAt = &now
}

// RecordWriteFailure records a failed write.
func (m *HealthStatsManager) RecordWriteFailure(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats, ok := m.stats[deviceID]
	if !ok {
		stats = NewDriverHealthStats(deviceID)
		stats.DeviceID = deviceID
		m.stats[deviceID] = stats
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.TotalWrites++
	stats.FailedWrites++
}

// updateConnectionQuality updates the connection quality score.
func (m *HealthStatsManager) updateConnectionQuality(deviceID string) {
	m.mu.RLock()
	stats, ok := m.stats[deviceID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	score := 100.0
	// Deduct for consecutive failures (max 50)
	deductConsec := float64(stats.ConsecutiveFailures) * 10
	if deductConsec > 50 {
		deductConsec = 50
	}
	score -= deductConsec
	// Deduct for read error rate (max 50)
	var readErrRate float64
	if stats.TotalReads > 0 {
		readErrRate = float64(stats.FailedReads) / float64(stats.TotalReads)
	}
	deductErrRate := readErrRate * 200
	if deductErrRate > 50 {
		deductErrRate = 50
	}
	score -= deductErrRate
	if score < 0 {
		score = 0
	}
	stats.ConnectionQualityScore = score
}

// evaluateDegradation evaluates and updates device degradation state.
func (m *HealthStatsManager) evaluateDegradation(deviceID string) {
	m.mu.RLock()
	stats, ok := m.stats[deviceID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if stats.ConnectionQualityScore < 50 {
		if stats.DegradationReason == "" {
			if stats.ConsecutiveFailures >= 5 {
				stats.DegradationReason = "consecutive_failures>=5"
			} else if stats.TotalReads > 0 && float64(stats.FailedReads)/float64(stats.TotalReads) > 0.1 {
				stats.DegradationReason = "read_error_rate>10%"
			} else {
				stats.DegradationReason = "quality_score<50"
			}
		}
	} else if stats.DegradationReason != "" {
		stats.DegradationReason = ""
	}
}

// GetConnectionStatus returns the connection status for a device.
func (m *HealthStatsManager) GetConnectionStatus(deviceID string) *ConnectionStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cs, ok := m.connStates[deviceID]; ok {
		return cs
	}
	return &ConnectionStatus{
		State: StateDisconnected,
		Since: nil,
	}
}

// SetConnectionState sets the connection state for a device with state machine validation.
func (m *HealthStatsManager) SetConnectionState(deviceID string, state ConnectionState, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.connStates[deviceID]
	if ok && existing.State != "" {
		allowed, exists := validStateTransitions[existing.State]
		if exists {
			if !allowed[state] {
				logrus.WithFields(logrus.Fields{
					"device_id":    deviceID,
					"from_state":   existing.State,
					"to_state":     state,
				}).Warn("Invalid state transition, rejected")
				return false
			}
		}
	}
	lastError := reason
	if state == StateConnected || state == StateConnecting {
		if existing != nil {
			lastError = existing.LastError
		} else {
			lastError = ""
		}
	}
	// A tracked device that is dialing again is retrying, so this is the one
	// place a reconnect can be counted. The first connect of a device the manager
	// has never seen is not a reconnect.
	if state == StateConnecting && ok && existing != nil && existing.State != StateConnecting {
		stats, hasStats := m.stats[deviceID]
		if !hasStats {
			stats = NewDriverHealthStats(deviceID)
			stats.DeviceID = deviceID
			m.stats[deviceID] = stats
		}
		stats.mu.Lock()
		stats.TotalReconnects++
		stats.mu.Unlock()
	}
	now := time.Now()
	m.connStates[deviceID] = &ConnectionStatus{
		State:     state,
		Reason:    reason,
		Since:     &now,
		LastError: lastError,
		Metrics:   make(map[string]interface{}),
	}
	return true
}

// GetObservabilityMetrics returns what the manager measured for a device. An
// unknown device has no measurements, so it returns nil rather than a quality
// score of 100 that would read as a perfectly healthy link.
func (m *HealthStatsManager) GetObservabilityMetrics(deviceID string) map[string]interface{} {
	m.mu.RLock()
	stats, ok := m.stats[deviceID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	var lastOnline, lastOffline interface{}
	if stats.LastOnlineAt != nil {
		lastOnline = stats.LastOnlineAt.Format(time.RFC3339)
	}
	if stats.LastOfflineAt != nil {
		lastOffline = stats.LastOfflineAt.Format(time.RFC3339)
	}
	// A ratio over zero attempts is not zero, it is unknown, and latency only
	// means something once a round trip has been timed.
	var readErrRate, writeErrRate, avgLatency interface{}
	if stats.TotalReads > 0 {
		readErrRate = float64(stats.FailedReads) / float64(stats.TotalReads)
	}
	if stats.TotalWrites > 0 {
		writeErrRate = float64(stats.FailedWrites) / float64(stats.TotalWrites)
	}
	if len(stats.latencySamples) > 0 {
		avgLatency = stats.AvgLatencyMs
	}
	return map[string]interface{}{
		"read_error_rate":          readErrRate,
		"write_error_rate":         writeErrRate,
		"consecutive_failures":     stats.ConsecutiveFailures,
		"connection_quality_score": stats.ConnectionQualityScore,
		"total_downtime_seconds":   stats.TotalDowntimeSeconds,
		"last_online_at":           lastOnline,
		"last_offline_at":          lastOffline,
		"avg_latency_ms":           avgLatency,
		"reconnect_count":          stats.TotalReconnects,
	}
}

// sortFloat64s sorts a slice of float64 in ascending order.
func sortFloat64s(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// IsNaN returns whether a float64 is NaN.
func isNaN(f float64) bool {
	return math.IsNaN(f)
}

// IsInf returns whether a float64 is Inf.
func isInf(f float64) bool {
	return math.IsInf(f, 0)
}

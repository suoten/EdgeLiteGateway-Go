package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
)

// MappingRule defines a mapping between a source point and a target point.
type MappingRule struct {
	RuleID         string  `json:"rule_id"`
	SourceDeviceID string  `json:"source_device_id"`
	SourcePoint    string  `json:"source_point"`
	TargetDeviceID string  `json:"target_device_id"`
	TargetPoint    string  `json:"target_point"`
	ConversionType string  `json:"conversion_type"` // "linear", "scale", "bool_to_int", etc.
	Scale          float64 `json:"scale"`
	Offset         float64 `json:"offset"`
	Enabled        bool    `json:"enabled"`
}

// BridgeWriteSink delivers a converted value to a target device point. The API
// layer injects the same write path the "write point" endpoint uses, so a bridge
// write is subject to the read-only / whitelist / rate-limit rules like any other.
type BridgeWriteSink func(ctx context.Context, deviceID, point string, value interface{}) error

// ProtocolBridge bridges data between different protocols/devices.
type ProtocolBridge struct {
	mu           sync.Mutex
	id           string
	name         string
	sourceDevice string
	targetDevice string
	enabled      bool
	rules        map[string]*MappingRule // ruleID -> rule
	transformCb  func(deviceID, point string, value interface{}) (interface{}, error)
	stats        *BridgeStats
}

// BridgeStats tracks statistics for a protocol bridge.
type BridgeStats struct {
	mu               sync.Mutex
	TotalTransferred int64
	TotalErrors      int64
	LastError        string
	LastTransferAt   *time.Time
}

// NewProtocolBridge creates a new ProtocolBridge.
func NewProtocolBridge(id, name, sourceDevice, targetDevice string) *ProtocolBridge {
	return &ProtocolBridge{
		id:           id,
		name:         name,
		sourceDevice: sourceDevice,
		targetDevice: targetDevice,
		enabled:      true,
		rules:        make(map[string]*MappingRule),
		stats:        &BridgeStats{},
	}
}

// SetEnabled toggles the bridge. A disabled bridge keeps its rules but stops
// transferring, so pausing a mapping does not require deleting it.
func (b *ProtocolBridge) SetEnabled(enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enabled = enabled
}

// Enabled reports whether the bridge transfers data.
func (b *ProtocolBridge) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

// Info returns the bridge definition plus live stats, for the API layer.
func (b *ProtocolBridge) Info() map[string]interface{} {
	b.mu.Lock()
	rules := make([]*MappingRule, 0, len(b.rules))
	for _, r := range b.rules {
		cp := *r
		rules = append(rules, &cp)
	}
	info := map[string]interface{}{
		"id":            b.id,
		"name":          b.name,
		"source_device": b.sourceDevice,
		"target_device": b.targetDevice,
		"enabled":       b.enabled,
	}
	b.mu.Unlock()

	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	info["rules"] = rules
	info["stats"] = b.GetStats()
	return info
}

// AddRule adds a mapping rule to the bridge.
func (b *ProtocolBridge) AddRule(rule *MappingRule) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rules[rule.RuleID] = rule
}

// RemoveRule removes a mapping rule from the bridge.
func (b *ProtocolBridge) RemoveRule(ruleID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.rules, ruleID)
}

// SetTransformCallback sets the callback for transforming values.
func (b *ProtocolBridge) SetTransformCallback(cb func(deviceID, point string, value interface{}) (interface{}, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transformCb = cb
}

// ProcessData processes incoming source data and applies mapping rules.
func (b *ProtocolBridge) ProcessData(ctx context.Context, deviceID, point string, value interface{}) (map[string]interface{}, error) {
	b.mu.Lock()
	if !b.enabled {
		b.mu.Unlock()
		return nil, nil
	}
	rulesCopy := make([]*MappingRule, 0, len(b.rules))
	for _, rule := range b.rules {
		if rule.Enabled && rule.SourceDeviceID == deviceID && rule.SourcePoint == point {
			rulesCopy = append(rulesCopy, rule)
		}
	}
	transformCb := b.transformCb
	b.mu.Unlock()

	if len(rulesCopy) == 0 {
		return nil, nil
	}

	results := make(map[string]interface{})
	for _, rule := range rulesCopy {
		convertedValue := convertValue(value, rule.ConversionType, rule.Scale, rule.Offset)

		if transformCb != nil {
			transformed, err := transformCb(rule.TargetDeviceID, rule.TargetPoint, convertedValue)
			if err != nil {
				b.stats.recordError("transform: " + err.Error())
				logrus.WithError(err).WithFields(logrus.Fields{
					"bridge_id": b.id,
					"rule_id":   rule.RuleID,
				}).Error("Protocol bridge transform failed")
				continue
			}
			convertedValue = transformed
		}

		results[rule.TargetPoint] = convertedValue
	}

	return results, nil
}

// TargetDevice returns the device this bridge writes into.
func (b *ProtocolBridge) TargetDevice() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.targetDevice
}

// recordError counts a failed transfer and keeps the reason visible in stats.
// Without this a write that never reached the device looked identical to one
// that was never attempted.
func (s *BridgeStats) recordError(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalErrors++
	s.LastError = reason
}

// recordTransfer counts one value that reached the target device.
func (s *BridgeStats) recordTransfer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalTransferred++
	now := time.Now()
	s.LastTransferAt = &now
}

// absorb folds another stats block into this one, keeping the newest error and
// transfer time. Callers hold the manager lock, so two stats blocks can never be
// absorbed in opposite directions at the same time.
func (s *BridgeStats) absorb(other *BridgeStats) {
	if other == nil || other == s {
		return
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalTransferred += other.TotalTransferred
	s.TotalErrors += other.TotalErrors
	if other.LastError != "" {
		s.LastError = other.LastError
	}
	if other.LastTransferAt != nil && (s.LastTransferAt == nil || other.LastTransferAt.After(*s.LastTransferAt)) {
		s.LastTransferAt = other.LastTransferAt
	}
}

// GetStats returns bridge statistics.
func (b *ProtocolBridge) GetStats() map[string]interface{} {
	b.stats.mu.Lock()
	defer b.stats.mu.Unlock()
	var lastTransfer interface{}
	if b.stats.LastTransferAt != nil {
		lastTransfer = b.stats.LastTransferAt.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"bridge_id":         b.id,
		"total_transferred": b.stats.TotalTransferred,
		"total_errors":      b.stats.TotalErrors,
		"last_error":        b.stats.LastError,
		"last_transfer_at":  lastTransfer,
		"rule_count":        len(b.rules),
	}
}

// ProtocolBridgeManager manages multiple protocol bridges.
type ProtocolBridgeManager struct {
	mu      sync.Mutex
	bridges map[string]*ProtocolBridge
	writeFn BridgeWriteSink
	started bool
}

// NewProtocolBridgeManager creates a new ProtocolBridgeManager.
func NewProtocolBridgeManager() *ProtocolBridgeManager {
	return &ProtocolBridgeManager{
		bridges: make(map[string]*ProtocolBridge),
	}
}

// Start marks the manager as running. Transfer happens on the data path fed by
// UpdateSourceData, so a stopped manager forwards nothing even while its bridge
// definitions remain loaded.
func (m *ProtocolBridgeManager) Start(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return fmt.Errorf("protocol bridge manager already started")
	}
	m.started = true
	logrus.Info("Protocol bridge manager started")
	return nil
}

// Stop stops the bridge manager.
func (m *ProtocolBridgeManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = false
}

// IsStarted reports whether the manager forwards data.
func (m *ProtocolBridgeManager) IsStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// SetWriteSink installs the function that delivers converted values to the
// target device. Without it UpdateSourceData computes results and throws them
// away, so the API exposes sink_wired to say which case an operator is in.
func (m *ProtocolBridgeManager) SetWriteSink(fn BridgeWriteSink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeFn = fn
}

// SinkWired reports whether a write path has been installed.
func (m *ProtocolBridgeManager) SinkWired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeFn != nil
}

// AddBridge adds a bridge to the manager, replacing one with the same id.
func (m *ProtocolBridgeManager) AddBridge(bridge *ProtocolBridge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bridges[bridge.id] = bridge
}

// SetBridges replaces the managed set with the given definitions, carrying each
// surviving bridge's counters over. Without the carry-over, editing or
// disabling a bridge zeroed the evidence of what it had already transferred.
func (m *ProtocolBridgeManager) SetBridges(next []*ProtocolBridge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wanted := make(map[string]bool, len(next))
	for _, nb := range next {
		wanted[nb.id] = true
		if old, ok := m.bridges[nb.id]; ok {
			nb.stats.absorb(old.stats)
		}
		m.bridges[nb.id] = nb
	}
	for id := range m.bridges {
		if !wanted[id] {
			delete(m.bridges, id)
		}
	}
}

// RemoveBridge removes a bridge from the manager and reports whether it existed.
func (m *ProtocolBridgeManager) RemoveBridge(bridgeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.bridges[bridgeID]; !ok {
		return false
	}
	delete(m.bridges, bridgeID)
	return true
}

// GetBridge returns one bridge, or nil when the id is unknown.
func (m *ProtocolBridgeManager) GetBridge(bridgeID string) *ProtocolBridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bridges[bridgeID]
}

// Count returns how many bridges the manager holds.
func (m *ProtocolBridgeManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bridges)
}

// Totals returns the transfer and error counts across all bridges.
func (m *ProtocolBridgeManager) Totals() (transferred, errors int64) {
	m.mu.Lock()
	bridges := make([]*ProtocolBridge, 0, len(m.bridges))
	for _, b := range m.bridges {
		bridges = append(bridges, b)
	}
	m.mu.Unlock()
	for _, b := range bridges {
		b.stats.mu.Lock()
		transferred += b.stats.TotalTransferred
		errors += b.stats.TotalErrors
		b.stats.mu.Unlock()
	}
	return transferred, errors
}

// UpdateSourceData feeds one collected point into every bridge and writes the
// converted values it produced to the target device. A computed value that was
// never written is an error, not a transfer.
func (m *ProtocolBridgeManager) UpdateSourceData(ctx context.Context, deviceID, point string, value interface{}) error {
	m.mu.Lock()
	sink := m.writeFn
	started := m.started
	bridgesCopy := make([]*ProtocolBridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		bridgesCopy = append(bridgesCopy, bridge)
	}
	m.mu.Unlock()

	if !started || len(bridgesCopy) == 0 {
		return nil
	}

	for _, bridge := range bridgesCopy {
		results, err := bridge.ProcessData(ctx, deviceID, point, value)
		if err != nil {
			logrus.WithError(err).WithField("bridge_id", bridge.id).Error("Bridge process failed")
			continue
		}
		if len(results) == 0 {
			continue
		}
		if sink == nil {
			bridge.stats.recordError("no write sink configured")
			continue
		}
		target := bridge.TargetDevice()
		for targetPoint, converted := range results {
			if err := sink(ctx, target, targetPoint, converted); err != nil {
				bridge.stats.recordError(fmt.Sprintf("write %s.%s: %s", target, targetPoint, err.Error()))
				logrus.WithError(err).WithFields(logrus.Fields{
					"bridge_id":     bridge.id,
					"target_device": target,
					"target_point":  targetPoint,
				}).Warn("Protocol bridge write failed")
				continue
			}
			bridge.stats.recordTransfer()
		}
	}
	return nil
}

// GetBridges returns all bridges, ordered by id.
func (m *ProtocolBridgeManager) GetBridges() []map[string]interface{} {
	m.mu.Lock()
	bridges := make([]*ProtocolBridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		bridges = append(bridges, bridge)
	}
	m.mu.Unlock()

	sort.Slice(bridges, func(i, j int) bool { return bridges[i].id < bridges[j].id })
	result := make([]map[string]interface{}, 0, len(bridges))
	for _, bridge := range bridges {
		result = append(result, bridge.Info())
	}
	return result
}

// GetBridgeStats returns statistics for a specific bridge.
func (m *ProtocolBridgeManager) GetBridgeStats(bridgeID string) map[string]interface{} {
	m.mu.Lock()
	bridge, ok := m.bridges[bridgeID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return bridge.GetStats()
}

// convertValue performs protocol value conversion.
func convertValue(value interface{}, conversionType string, scale, offset float64) interface{} {
	switch conversionType {
	case "linear", "scale":
		f, ok := toFloat64Bridge(value)
		if !ok {
			return value
		}
		return f*scale + offset
	case "bool_to_int":
		if b, ok := value.(bool); ok {
			if b {
				return int64(1)
			}
			return int64(0)
		}
		return value
	case "int_to_bool":
		f, ok := toFloat64Bridge(value)
		if !ok {
			return value
		}
		return f != 0
	default:
		return value
	}
}

// toFloat64Bridge converts an interface to float64 for bridge conversion.
func toFloat64Bridge(v interface{}) (float64, bool) {
	return constants.NumericAsFloat(v)
}

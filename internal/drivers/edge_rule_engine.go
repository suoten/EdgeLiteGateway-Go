package drivers

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// EdgeRuleType defines the type of edge rule.
type EdgeRuleType string

const (
	EdgeRuleThreshold    EdgeRuleType = "threshold"
	EdgeRuleRateOfChange EdgeRuleType = "rate_of_change"
	EdgeRuleState        EdgeRuleType = "state"
	EdgeRuleExpression   EdgeRuleType = "expression"
)

// EdgeRuleOperator defines comparison operators for edge rules.
type EdgeRuleOperator string

const (
	OpGT  EdgeRuleOperator = ">"
	OpGTE EdgeRuleOperator = ">="
	OpLT  EdgeRuleOperator = "<"
	OpLTE EdgeRuleOperator = "<="
	OpEQ  EdgeRuleOperator = "=="
	OpNEQ EdgeRuleOperator = "!="
)

// EdgeRule represents a single edge rule for real-time threshold/rule evaluation.
type EdgeRule struct {
	RuleID     string           `json:"rule_id"`
	DeviceID   string           `json:"device_id"`
	PointName  string           `json:"point_name"`
	RuleType   EdgeRuleType     `json:"rule_type"`
	Operator   EdgeRuleOperator `json:"operator"`
	Threshold  float64          `json:"threshold"`
	Severity   string           `json:"severity"`
	Enabled    bool             `json:"enabled"`
	CooldownMs float64          `json:"cooldown_ms"`
	DurationMs float64          `json:"duration_ms"`
	Deadband   float64          `json:"deadband"`
	Actions    []EdgeRuleAction `json:"actions"`
}

// EdgeRuleAction defines an action to take when a rule fires.
type EdgeRuleAction struct {
	Type   string                 `json:"type"` // "write", "mqtt_publish", "webhook"
	Target string                 `json:"target"`
	Params map[string]interface{} `json:"params"`
}

// AlarmRecord represents an alarm triggered by an edge rule.
type AlarmRecord struct {
	RuleID    string    `json:"rule_id"`
	DeviceID  string    `json:"device_id"`
	PointName string    `json:"point_name"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	Severity  string    `json:"severity"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

// ToDict converts an AlarmRecord to a map.
func (a *AlarmRecord) ToDict() map[string]interface{} {
	return map[string]interface{}{
		"rule_id":    a.RuleID,
		"device_id":  a.DeviceID,
		"point_name": a.PointName,
		"value":      a.Value,
		"threshold":  a.Threshold,
		"severity":   a.Severity,
		"timestamp":  a.Timestamp.Format(time.RFC3339),
		"message":    a.Message,
	}
}

// EdgeRuleEngine provides lightweight, in-process rule evaluation for drivers.
type EdgeRuleEngine struct {
	mu             sync.Mutex
	rules          map[string]*EdgeRule // rule_id -> rule
	alarmHistory   []AlarmRecord
	lastTriggered  map[string]time.Time // rule_id -> last trigger time
	conditionSince map[string]time.Time // rule_id -> condition start time
	eventBus       interface{}          // EventBus interface (avoid circular import)
	maxHistory     int
}

// NewEdgeRuleEngine creates a new EdgeRuleEngine.
func NewEdgeRuleEngine() *EdgeRuleEngine {
	return &EdgeRuleEngine{
		rules:          make(map[string]*EdgeRule),
		lastTriggered:  make(map[string]time.Time),
		conditionSince: make(map[string]time.Time),
		maxHistory:     1000,
	}
}

// SetEventBus sets the event bus for publishing alarm events.
func (e *EdgeRuleEngine) SetEventBus(bus interface{}) {
	e.eventBus = bus
}

// AddRule adds or updates a rule.
func (e *EdgeRuleEngine) AddRule(rule *EdgeRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules[rule.RuleID] = rule
}

// RemoveRule removes a rule by ID.
func (e *EdgeRuleEngine) RemoveRule(ruleID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rules, ruleID)
	delete(e.lastTriggered, ruleID)
	delete(e.conditionSince, ruleID)
}

// GetRules returns all rules.
func (e *EdgeRuleEngine) GetRules() []*EdgeRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]*EdgeRule, 0, len(e.rules))
	for _, r := range e.rules {
		result = append(result, r)
	}
	return result
}

// GetRulesForPoint returns rules matching a device and point.
func (e *EdgeRuleEngine) GetRulesForPoint(deviceID, pointName string) []*EdgeRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	var result []*EdgeRule
	for _, r := range e.rules {
		if r.Enabled && r.DeviceID == deviceID && r.PointName == pointName {
			result = append(result, r)
		}
	}
	return result
}

// EvaluatePoint evaluates all rules for a given point value.
// Returns the list of triggered alarm records.
func (e *EdgeRuleEngine) EvaluatePoint(deviceID, pointName string, value float64) []AlarmRecord {
	rules := e.GetRulesForPoint(deviceID, pointName)
	var alarms []AlarmRecord
	now := time.Now()

	for _, rule := range rules {
		if e.checkRule(rule, value, now) {
			alarm := AlarmRecord{
				RuleID:    rule.RuleID,
				DeviceID:  deviceID,
				PointName: pointName,
				Value:     value,
				Threshold: rule.Threshold,
				Severity:  rule.Severity,
				Timestamp: now,
				Message:   fmt.Sprintf("%s %s %.2f (threshold: %.2f)", pointName, rule.Operator, value, rule.Threshold),
			}
			alarms = append(alarms, alarm)
			e.recordAlarm(alarm)
		}
	}
	return alarms
}

// checkRule checks if a rule condition is met, considering cooldown and duration.
func (e *EdgeRuleEngine) checkRule(rule *EdgeRule, value float64, now time.Time) bool {
	// Check operator
	conditionMet := false
	switch rule.Operator {
	case OpGT:
		conditionMet = value > rule.Threshold
	case OpGTE:
		conditionMet = value >= rule.Threshold
	case OpLT:
		conditionMet = value < rule.Threshold
	case OpLTE:
		conditionMet = value <= rule.Threshold
	case OpEQ:
		conditionMet = abs(value-rule.Threshold) < 1e-9
	case OpNEQ:
		conditionMet = abs(value-rule.Threshold) >= 1e-9
	}

	if !conditionMet {
		// Reset condition start time
		e.mu.Lock()
		delete(e.conditionSince, rule.RuleID)
		e.mu.Unlock()
		return false
	}

	// Check duration requirement
	e.mu.Lock()
	condStart, exists := e.conditionSince[rule.RuleID]
	if !exists {
		e.conditionSince[rule.RuleID] = now
		condStart = now
	}
	e.mu.Unlock()

	if rule.DurationMs > 0 {
		duration := now.Sub(condStart).Seconds() * 1000
		if duration < rule.DurationMs {
			return false // Condition not sustained long enough
		}
	}

	// Check cooldown
	e.mu.Lock()
	lastTrigger, triggered := e.lastTriggered[rule.RuleID]
	e.mu.Unlock()

	if triggered {
		elapsed := now.Sub(lastTrigger).Seconds() * 1000
		if elapsed < rule.CooldownMs {
			return false // Still in cooldown
		}
	}

	// Update last triggered time
	e.mu.Lock()
	e.lastTriggered[rule.RuleID] = now
	e.mu.Unlock()

	return true
}

// recordAlarm records an alarm in history.
func (e *EdgeRuleEngine) recordAlarm(alarm AlarmRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alarmHistory = append(e.alarmHistory, alarm)
	if len(e.alarmHistory) > e.maxHistory {
		e.alarmHistory = e.alarmHistory[1:]
	}
	logrus.WithFields(logrus.Fields{
		"rule_id":    alarm.RuleID,
		"device_id":  alarm.DeviceID,
		"point_name": alarm.PointName,
		"severity":   alarm.Severity,
	}).Warn("Edge rule alarm triggered: " + alarm.Message)
}

// GetAlarmHistory returns recent alarm history.
func (e *EdgeRuleEngine) GetAlarmHistory(limit int) []AlarmRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	if limit <= 0 || limit > len(e.alarmHistory) {
		limit = len(e.alarmHistory)
	}
	result := make([]AlarmRecord, limit)
	copy(result, e.alarmHistory[len(e.alarmHistory)-limit:])
	return result
}

// LoadRulesFromStore loads rules from a RuleStore.
func (e *EdgeRuleEngine) LoadRulesFromStore(store *RuleStore) {
	if store == nil {
		return
	}
	rules := store.LoadRules()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rules {
		e.rules[r.RuleID] = r
	}
	logrus.Infof("Loaded %d edge rules from store", len(rules))
}

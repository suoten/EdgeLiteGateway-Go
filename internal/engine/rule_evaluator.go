package engine

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// RuleEvaluator evaluates rules against collected data points and triggers alarms.
// It supports threshold rules, AI inference rules, and script rules.
// Duration-based rules require the condition to be true for N consecutive seconds.
type RuleEvaluator struct {
	mu           sync.RWMutex
	rules        map[string]*activeRule
	eventBus     *EventBus
	ruleRepo     *storage.RuleRepo
	alarmRepo    *storage.AlarmRepo
	activeAlarms map[string]string // ruleID -> alarmID (currently firing)
	alarmHooks   AlarmHooks

	// trackerMu protects durationTracker and pointCache to avoid lock upgrades
	// from RLock (held during evaluateRulesForDevice) to Lock (in evaluateThresholdRule),
	// which would cause a deadlock.
	trackerMu sync.Mutex
	// Duration tracking: ruleID -> consecutive true count
	durationTracker map[string]int
	// Last known point values per device: deviceID -> pointName -> value
	pointCache map[string]map[string]interface{}
}

// activeRule wraps a rule response with runtime state.
type activeRule struct {
	rule        *models.RuleResponse
	loadedAt    time.Time
	evaluations int64
	errors      int64
}

// AlarmHooks lets the service layer react to alarm transitions the evaluator
// owns. The engine cannot import the services package (services imports engine),
// so notifications, escalation and statistics are driven from here instead.
type AlarmHooks struct {
	// OnFired runs after an alarm row has been created and the trigger event
	// published.
	OnFired func(alarm *models.AlarmResponse, rule *models.RuleResponse)
	// OnRecovered runs after the alarm row has been marked recovered.
	OnRecovered func(alarmID string, rule *models.RuleResponse)
}

// NewRuleEvaluator creates a new RuleEvaluator.
func NewRuleEvaluator(eventBus *EventBus, ruleRepo *storage.RuleRepo, alarmRepo *storage.AlarmRepo) *RuleEvaluator {
	e := &RuleEvaluator{
		rules:           make(map[string]*activeRule),
		eventBus:        eventBus,
		ruleRepo:        ruleRepo,
		alarmRepo:       alarmRepo,
		activeAlarms:    make(map[string]string),
		durationTracker: make(map[string]int),
		pointCache:      make(map[string]map[string]interface{}),
	}

	// Subscribe to data collected events
	if eventBus != nil {
		eventBus.Subscribe(EventTypeDataCollected, e.onDataCollected)
	}

	return e
}

// SetAlarmHooks registers the service-layer callbacks for alarm transitions.
func (e *RuleEvaluator) SetAlarmHooks(hooks AlarmHooks) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alarmHooks = hooks
}

// LoadRule loads or reloads a rule into the evaluator.
func (e *RuleEvaluator) LoadRule(rule *models.RuleResponse) {
	e.mu.Lock()
	e.rules[rule.RuleID] = &activeRule{
		rule:     rule,
		loadedAt: time.Now(),
	}
	e.mu.Unlock()

	// Re-registering a rule (startup preload, re-enable) starts with an empty
	// in-memory alarm map while the store may still hold this rule's alarm open
	// from before the restart. Adopt it now so the next evaluation recovers the
	// real row instead of raising a second one nobody can close.
	e.adoptOpenAlarm(rule.RuleID)

	logrus.WithField("rule_id", rule.RuleID).Debug("Rule loaded into evaluator")
}

// UnloadRule removes a rule from the evaluator.
func (e *RuleEvaluator) UnloadRule(ruleID string) {
	e.mu.Lock()
	delete(e.rules, ruleID)
	delete(e.activeAlarms, ruleID)
	e.mu.Unlock()

	e.trackerMu.Lock()
	delete(e.durationTracker, ruleID)
	e.trackerMu.Unlock()
}

// adoptOpenAlarm points this process at the alarm a rule already holds open in
// the store, and closes the extra open rows a rule can only have from an older
// build that raised one alarm per restart. It returns the adopted alarm ID, or
// "" when the rule holds none.
func (e *RuleEvaluator) adoptOpenAlarm(ruleID string) string {
	if e.alarmRepo == nil {
		return ""
	}
	openID, err := e.alarmRepo.OpenAlarmIDByRule(ruleID)
	if err != nil {
		logrus.WithField("rule_id", ruleID).
			WithError(err).
			Warn("Could not look for the alarm this rule still holds open")
		return ""
	}
	if openID == "" {
		return ""
	}

	e.mu.Lock()
	e.activeAlarms[ruleID] = openID
	e.mu.Unlock()
	logrus.WithField("rule_id", ruleID).
		WithField("alarm_id", openID).
		Info("Adopted the alarm this rule still holds open")

	// One rule has exactly one live alarm; the rest were left behind before the
	// adoption above existed and would sit on the board as firing forever.
	if n, err := e.alarmRepo.RecoverStaleByRule(ruleID, openID); err != nil {
		logrus.WithField("rule_id", ruleID).
			WithError(err).
			Warn("Could not close the leftover open alarms of this rule")
	} else if n > 0 {
		logrus.WithField("rule_id", ruleID).
			WithField("closed", n).
			Info("Recovered duplicate open alarms left by an earlier build")
	}
	return openID
}

// GetActiveAlarms returns currently firing alarm IDs.
func (e *RuleEvaluator) GetActiveAlarms() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]string, len(e.activeAlarms))
	for k, v := range e.activeAlarms {
		result[k] = v
	}
	return result
}

// onDataCollected handles data collected events.
func (e *RuleEvaluator) onDataCollected(event Event) {
	data, ok := event.Data.(DataCollectedEvent)
	if !ok {
		return
	}

	// Update point cache (use trackerMu to avoid RLock->Lock deadlock)
	e.trackerMu.Lock()
	if e.pointCache[data.DeviceID] == nil {
		e.pointCache[data.DeviceID] = make(map[string]interface{})
	}
	for _, p := range data.Points {
		e.pointCache[data.DeviceID][p.PointName] = p.Value
	}
	e.trackerMu.Unlock()

	// Evaluate rules for this device
	e.evaluateRulesForDevice(data.DeviceID, data.Points)
}

// evaluateRulesForDevice evaluates all rules matching the device.
func (e *RuleEvaluator) evaluateRulesForDevice(deviceID string, points []storage.PointData) {
	// Phase 1: Hold RLock only for reading rules and evaluating conditions.
	// Collect triggered/recovered results, then release RLock before calling
	// handleRuleTriggered/handleRuleRecovered (which acquire Lock) to avoid deadlock.
	type evalResult struct {
		rule          *models.RuleResponse
		triggered     bool
		triggerValues map[string]interface{}
	}

	var results []evalResult

	e.mu.RLock()
	for _, ar := range e.rules {
		rule := ar.rule
		if !rule.Enabled {
			continue
		}
		// Check if this rule applies to this device
		if rule.DeviceID != "" && rule.DeviceID != deviceID {
			continue
		}

		// Evaluate the rule
		triggered, triggerValues, err := e.evaluateRule(rule, points)
		ar.evaluations++

		if err != nil {
			ar.errors++
			if e.ruleRepo != nil {
				_ = e.ruleRepo.IncrementErrorCount(rule.RuleID)
			}
			logrus.WithField("rule_id", rule.RuleID).
				WithField("error", err.Error()).
				Warn("Rule evaluation error")
			continue
		}

		if e.ruleRepo != nil {
			_ = e.ruleRepo.IncrementInferenceCount(rule.RuleID)
		}

		results = append(results, evalResult{
			rule:          rule,
			triggered:     triggered,
			triggerValues: triggerValues,
		})
	}
	e.mu.RUnlock()

	// Phase 2: Process triggers/recoveries without holding RLock.
	for _, r := range results {
		if r.triggered {
			e.handleRuleTriggered(r.rule, r.triggerValues)
		} else {
			e.handleRuleRecovered(r.rule)
		}
	}
}

// evaluateRule evaluates a single rule against the collected points.
// Returns (triggered, triggerValues, error).
func (e *RuleEvaluator) evaluateRule(rule *models.RuleResponse, points []storage.PointData) (bool, map[string]interface{}, error) {
	// Build a map of point values
	pointValues := make(map[string]interface{})
	for _, p := range points {
		pointValues[p.PointName] = p.Value
	}

	// Also add cached values (use trackerMu to avoid nested RLock deadlock)
	e.trackerMu.Lock()
	if cached, ok := e.pointCache[rule.DeviceID]; ok {
		for k, v := range cached {
			if _, exists := pointValues[k]; !exists {
				pointValues[k] = v
			}
		}
	}
	e.trackerMu.Unlock()

	switch rule.RuleType {
	case "threshold", "":
		return e.evaluateThresholdRule(rule, pointValues)
	case "ai_inference":
		return e.evaluateAIInferenceRule(rule, pointValues)
	case "script":
		return e.evaluateScriptRule(rule, pointValues)
	default:
		return e.evaluateThresholdRule(rule, pointValues)
	}
}

// evaluateThresholdRule evaluates a threshold-based rule.
func (e *RuleEvaluator) evaluateThresholdRule(rule *models.RuleResponse, pointValues map[string]interface{}) (bool, map[string]interface{}, error) {
	if len(rule.Conditions) == 0 {
		return false, nil, fmt.Errorf("rule has no conditions")
	}

	results := make([]bool, len(rule.Conditions))
	triggerValues := make(map[string]interface{})

	for i, cond := range rule.Conditions {
		val, ok := pointValues[cond.Point]
		if !ok {
			results[i] = false
			continue
		}
		floatVal, err := toFloat64(val)
		if err != nil {
			results[i] = false
			continue
		}
		matched := evaluateCondition(cond.Operator, floatVal, cond.Threshold)
		results[i] = matched
		if matched {
			triggerValues[cond.Point] = val
		}
	}

	// Apply logic
	var triggered bool
	logic := strings.ToUpper(rule.Logic)
	if logic == "" {
		logic = "AND"
	}
	switch logic {
	case "AND":
		triggered = true
		for _, r := range results {
			if !r {
				triggered = false
				break
			}
		}
	case "OR":
		triggered = false
		for _, r := range results {
			if r {
				triggered = true
				break
			}
		}
	case "NOT":
		triggered = true
		for _, r := range results {
			if r {
				triggered = false
				break
			}
		}
	default:
		triggered = true
		for _, r := range results {
			if !r {
				triggered = false
				break
			}
		}
	}

	// Duration check: if duration > 0, require the condition to be true for N seconds
	if triggered && rule.Duration > 0 {
		e.trackerMu.Lock()
		e.durationTracker[rule.RuleID]++
		count := e.durationTracker[rule.RuleID]
		e.trackerMu.Unlock()

		// Duration is in seconds, check every collection cycle (assume 1s per cycle)
		if count < rule.Duration {
			triggered = false
		}
	} else if !triggered {
		e.trackerMu.Lock()
		e.durationTracker[rule.RuleID] = 0
		e.trackerMu.Unlock()
	}

	return triggered, triggerValues, nil
}

// evaluateAIInferenceRule evaluates an AI inference-based rule.
// In the Go edition, AI inference is delegated to the AIInferenceEngine.
func (e *RuleEvaluator) evaluateAIInferenceRule(rule *models.RuleResponse, pointValues map[string]interface{}) (bool, map[string]interface{}, error) {
	// For now, use threshold logic on AI model output
	// In production, this would call the AI inference engine
	return e.evaluateThresholdRule(rule, pointValues)
}

// evaluateScriptRule evaluates a script-based rule.
// In the Go edition, script execution uses a sandboxed interpreter.
func (e *RuleEvaluator) evaluateScriptRule(rule *models.RuleResponse, pointValues map[string]interface{}) (bool, map[string]interface{}, error) {
	// Script execution is handled by the expression engine
	// For now, fall back to threshold logic
	return e.evaluateThresholdRule(rule, pointValues)
}

// handleRuleTriggered processes a triggered rule.
func (e *RuleEvaluator) handleRuleTriggered(rule *models.RuleResponse, triggerValues map[string]interface{}) {
	// Suppress alarm creation entirely while an active silence window covers
	// this device/rule — suppressed alarms must not notify operators.
	if e.alarmRepo != nil && e.alarmRepo.IsSilenced(rule.DeviceID, rule.RuleID, "") {
		logrus.WithField("rule_id", rule.RuleID).
			WithField("device_id", rule.DeviceID).
			Debug("Rule triggered but suppressed by active silence window")
		return
	}

	e.mu.Lock()
	_, alreadyFiring := e.activeAlarms[rule.RuleID]
	e.mu.Unlock()

	if alreadyFiring {
		// Already firing, don't create duplicate
		return
	}

	// activeAlarms only lives in this process, so a restart forgets every alarm
	// the store still holds open. Adopt that row instead of raising a second
	// copy: the map would then point at the new one, the old row could never be
	// recovered, and each restart added another phantom to the board.
	if e.adoptOpenAlarm(rule.RuleID) != "" {
		return
	}

	// Create new alarm
	alarmID := generateAlarmID()
	alarm := &models.AlarmResponse{
		AlarmID:      alarmID,
		RuleID:       rule.RuleID,
		DeviceID:     rule.DeviceID,
		Severity:     rule.Severity,
		Status:       "firing",
		Message:      fmt.Sprintf("Rule '%s' triggered", rule.Name),
		TriggerValue: triggerValues,
		TriggerCount: 1,
		FiredAt:      time.Now().Format(time.RFC3339),
		RuleType:     rule.RuleType,
		Version:      1,
	}

	if e.alarmRepo != nil {
		if err := e.alarmRepo.Create(alarm); err != nil {
			logrus.WithField("rule_id", rule.RuleID).
				WithField("error", err.Error()).
				Error("Failed to create alarm")
			return
		}
	}

	e.mu.Lock()
	e.activeAlarms[rule.RuleID] = alarmID
	e.mu.Unlock()

	// Publish alarm triggered event
	if e.eventBus != nil {
		e.eventBus.Publish(Event{
			Type:     EventTypeAlarmTriggered,
			Source:   "rule_evaluator",
			DeviceID: rule.DeviceID,
			Data: AlarmEventPayload{
				AlarmID:  alarmID,
				RuleID:   rule.RuleID,
				DeviceID: rule.DeviceID,
				Severity: rule.Severity,
				Message:  alarm.Message,
				Values:   triggerValues,
			},
		})
	}

	logrus.WithField("rule_id", rule.RuleID).
		WithField("alarm_id", alarmID).
		WithField("severity", rule.Severity).
		Info("Alarm triggered")

	e.mu.RLock()
	onFired := e.alarmHooks.OnFired
	e.mu.RUnlock()
	if onFired != nil {
		onFired(alarm, rule)
	}
}

// handleRuleRecovered processes a recovered rule.
func (e *RuleEvaluator) handleRuleRecovered(rule *models.RuleResponse) {
	e.mu.Lock()
	alarmID, exists := e.activeAlarms[rule.RuleID]
	if exists {
		delete(e.activeAlarms, rule.RuleID)
	}
	e.mu.Unlock()

	if !exists {
		return
	}

	if e.alarmRepo != nil {
		if err := e.alarmRepo.Recover(alarmID); err != nil {
			logrus.WithField("alarm_id", alarmID).
				WithField("error", err.Error()).
				Warn("Failed to recover alarm")
		}
	}

	// Reset duration tracker
	e.trackerMu.Lock()
	e.durationTracker[rule.RuleID] = 0
	e.trackerMu.Unlock()

	// Publish alarm recovered event
	if e.eventBus != nil {
		e.eventBus.Publish(Event{
			Type:     EventTypeAlarmRecovered,
			Source:   "rule_evaluator",
			DeviceID: rule.DeviceID,
			Data: AlarmEventPayload{
				AlarmID:  alarmID,
				RuleID:   rule.RuleID,
				DeviceID: rule.DeviceID,
			},
		})
	}

	logrus.WithField("rule_id", rule.RuleID).
		WithField("alarm_id", alarmID).
		Info("Alarm recovered")

	e.mu.RLock()
	onRecovered := e.alarmHooks.OnRecovered
	e.mu.RUnlock()
	if onRecovered != nil {
		onRecovered(alarmID, rule)
	}
}

// CloseRuleAlarms recovers every alarm a rule still holds open, including ones
// this process no longer tracks in memory — the gateway may have restarted since
// the alarm fired. Deleting a rule takes its evaluation away with it, so an
// alarm left firing under a deleted rule could never recover on its own and
// stayed on the board as a phantom nothing could clear.
func (e *RuleEvaluator) CloseRuleAlarms(rule *models.RuleResponse) error {
	if rule == nil {
		return nil
	}
	// Recovers the alarm this process still tracks and publishes the recovered
	// event the console listens for.
	e.handleRuleRecovered(rule)
	if e.alarmRepo == nil {
		return nil
	}
	n, err := e.alarmRepo.RecoverByRule(rule.RuleID)
	if err != nil {
		return err
	}
	if n > 0 {
		logrus.WithField("rule_id", rule.RuleID).WithField("alarms", n).
			Info("Unresolved alarms closed with the rule")
	}
	return nil
}

// AcknowledgeAlarm acknowledges an active alarm.
func (e *RuleEvaluator) AcknowledgeAlarm(ruleID, userID string) error {
	e.mu.Lock()
	alarmID, exists := e.activeAlarms[ruleID]
	e.mu.Unlock()
	if !exists {
		return fmt.Errorf("no active alarm for rule %s", ruleID)
	}
	if e.alarmRepo != nil {
		return e.alarmRepo.Acknowledge(alarmID, userID)
	}
	return nil
}

// GetLoadedRules returns the list of loaded rule IDs.
func (e *RuleEvaluator) GetLoadedRules() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]string, 0, len(e.rules))
	for id := range e.rules {
		result = append(result, id)
	}
	return result
}

// Stats returns evaluator statistics.
func (e *RuleEvaluator) Stats() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var totalEval, totalErr int64
	for _, ar := range e.rules {
		totalEval += ar.evaluations
		totalErr += ar.errors
	}
	return map[string]interface{}{
		"loaded_rules":      len(e.rules),
		"active_alarms":     len(e.activeAlarms),
		"total_evaluations": totalEval,
		"total_errors":      totalErr,
	}
}

// --- Helper functions ---

// EvaluateConditionPublic is a public wrapper for evaluateCondition.
func EvaluateConditionPublic(operator string, value, threshold float64) bool {
	return evaluateCondition(operator, value, threshold)
}

// evaluateCondition evaluates a single condition against a value.
func evaluateCondition(operator string, value, threshold float64) bool {
	switch operator {
	case ">", "gt":
		return value > threshold
	case ">=", "gte", "ge":
		return value >= threshold
	case "<", "lt":
		return value < threshold
	case "<=", "lte", "le":
		return value <= threshold
	case "==", "eq", "=":
		return math.Abs(value-threshold) < 1e-9
	case "!=", "ne", "neq":
		return math.Abs(value-threshold) >= 1e-9
	default:
		return false
	}
}

// toFloat64 converts an interface{} to float64.
func toFloat64(val interface{}) (float64, error) {
	if f, ok := constants.NumericAsFloat(val); ok {
		return f, nil
	}
	return 0, fmt.Errorf("cannot convert %T to float64", val)
}

// generateAlarmID generates a unique alarm ID.
func generateAlarmID() string {
	return fmt.Sprintf("alm-%d", time.Now().UnixNano())
}

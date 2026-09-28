package drivers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"testing"
	"time"

	"edgelite/internal/models"
)

// ==================== Edge Rule Engine Tests ====================

func TestAlarmRecordToDict(t *testing.T) {
	alarm := AlarmRecord{
		RuleID:    "rule-1",
		DeviceID:  "device-1",
		PointName: "temperature",
		Value:     85.5,
		Threshold: 80.0,
		Severity:  "warning",
		Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		Message:   "temperature > 85.50",
	}
	dict := alarm.ToDict()
	if dict["rule_id"] != "rule-1" {
		t.Errorf("Expected rule_id=rule-1, got %v", dict["rule_id"])
	}
	if dict["device_id"] != "device-1" {
		t.Errorf("Expected device_id=device-1, got %v", dict["device_id"])
	}
	if dict["point_name"] != "temperature" {
		t.Errorf("Expected point_name=temperature, got %v", dict["point_name"])
	}
	if dict["severity"] != "warning" {
		t.Errorf("Expected severity=warning, got %v", dict["severity"])
	}
	if dict["value"] != 85.5 {
		t.Errorf("Expected value=85.5, got %v", dict["value"])
	}
	if dict["threshold"] != 80.0 {
		t.Errorf("Expected threshold=80.0, got %v", dict["threshold"])
	}
}

func TestEdgeRuleEngineAddRemoveRule(t *testing.T) {
	engine := NewEdgeRuleEngine()
	rule := &EdgeRule{
		RuleID:    "rule-1",
		DeviceID:  "device-1",
		PointName: "temp",
		RuleType:  EdgeRuleThreshold,
		Operator:  OpGT,
		Threshold: 100,
		Enabled:   true,
	}
	engine.AddRule(rule)
	rules := engine.GetRules()
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}
	if rules[0].RuleID != "rule-1" {
		t.Errorf("Expected rule-1, got %s", rules[0].RuleID)
	}

	engine.RemoveRule("rule-1")
	rules = engine.GetRules()
	if len(rules) != 0 {
		t.Fatalf("Expected 0 rules after remove, got %d", len(rules))
	}
}

func TestEdgeRuleEngineGetRulesForPoint(t *testing.T) {
	engine := NewEdgeRuleEngine()
	engine.AddRule(&EdgeRule{RuleID: "r1", DeviceID: "d1", PointName: "p1", Enabled: true})
	engine.AddRule(&EdgeRule{RuleID: "r2", DeviceID: "d1", PointName: "p2", Enabled: true})
	engine.AddRule(&EdgeRule{RuleID: "r3", DeviceID: "d2", PointName: "p1", Enabled: true})
	engine.AddRule(&EdgeRule{RuleID: "r4", DeviceID: "d1", PointName: "p1", Enabled: false})

	rules := engine.GetRulesForPoint("d1", "p1")
	if len(rules) != 1 {
		t.Errorf("Expected 1 enabled rule for d1/p1, got %d", len(rules))
	}
	if rules[0].RuleID != "r1" {
		t.Errorf("Expected r1, got %s", rules[0].RuleID)
	}

	rules = engine.GetRulesForPoint("d1", "p2")
	if len(rules) != 1 {
		t.Errorf("Expected 1 rule for d1/p2, got %d", len(rules))
	}

	rules = engine.GetRulesForPoint("d2", "p1")
	if len(rules) != 1 {
		t.Errorf("Expected 1 rule for d2/p1, got %d", len(rules))
	}
}

func TestEdgeRuleEngineEvaluateThresholdGT(t *testing.T) {
	engine := NewEdgeRuleEngine()
	engine.AddRule(&EdgeRule{
		RuleID: "r1", DeviceID: "d1", PointName: "temp",
		Operator: OpGT, Threshold: 100, Enabled: true, Severity: "critical",
	})

	// Below threshold - no alarm
	alarms := engine.EvaluatePoint("d1", "temp", 50)
	if len(alarms) != 0 {
		t.Errorf("Expected 0 alarms below threshold, got %d", len(alarms))
	}

	// Above threshold - alarm
	alarms = engine.EvaluatePoint("d1", "temp", 150)
	if len(alarms) != 1 {
		t.Fatalf("Expected 1 alarm above threshold, got %d", len(alarms))
	}
	if alarms[0].Value != 150 {
		t.Errorf("Expected value=150, got %f", alarms[0].Value)
	}
}

func TestEdgeRuleEngineEvaluateAllOperators(t *testing.T) {
	tests := []struct {
		op        EdgeRuleOperator
		value     float64
		threshold float64
		expect    bool
	}{
		{OpGT, 101, 100, true},
		{OpGT, 100, 100, false},
		{OpGTE, 100, 100, true},
		{OpGTE, 99, 100, false},
		{OpLT, 99, 100, true},
		{OpLT, 100, 100, false},
		{OpLTE, 100, 100, true},
		{OpLTE, 101, 100, false},
		{OpEQ, 100, 100, true},
		{OpEQ, 99, 100, false},
		{OpNEQ, 99, 100, true},
		{OpNEQ, 100, 100, false},
	}

	for _, tt := range tests {
		engine := NewEdgeRuleEngine()
		engine.AddRule(&EdgeRule{
			RuleID: "r1", DeviceID: "d1", PointName: "p1",
			Operator: tt.op, Threshold: tt.threshold, Enabled: true,
		})
		alarms := engine.EvaluatePoint("d1", "p1", tt.value)
		got := len(alarms) > 0
		if got != tt.expect {
			t.Errorf("op=%s value=%f threshold=%f: expected %v, got %v", tt.op, tt.value, tt.threshold, tt.expect, got)
		}
	}
}

func TestEdgeRuleEngineCooldown(t *testing.T) {
	engine := NewEdgeRuleEngine()
	engine.AddRule(&EdgeRule{
		RuleID: "r1", DeviceID: "d1", PointName: "p1",
		Operator: OpGT, Threshold: 50, Enabled: true,
		CooldownMs: 1000, // 1 second cooldown
	})

	// First trigger
	alarms := engine.EvaluatePoint("d1", "p1", 100)
	if len(alarms) != 1 {
		t.Fatalf("Expected first alarm, got %d", len(alarms))
	}

	// Second trigger within cooldown - should be suppressed
	alarms = engine.EvaluatePoint("d1", "p1", 100)
	if len(alarms) != 0 {
		t.Errorf("Expected no alarm during cooldown, got %d", len(alarms))
	}
}

func TestEdgeRuleEngineAlarmHistory(t *testing.T) {
	engine := NewEdgeRuleEngine()
	engine.AddRule(&EdgeRule{
		RuleID: "r1", DeviceID: "d1", PointName: "p1",
		Operator: OpGT, Threshold: 50, Enabled: true,
	})

	engine.EvaluatePoint("d1", "p1", 100)
	engine.EvaluatePoint("d1", "p1", 200)

	history := engine.GetAlarmHistory(10)
	if len(history) != 2 {
		t.Fatalf("Expected 2 alarms in history, got %d", len(history))
	}

	// Test limit
	history = engine.GetAlarmHistory(1)
	if len(history) != 1 {
		t.Errorf("Expected 1 alarm with limit=1, got %d", len(history))
	}

	// Test default (0 = all)
	history = engine.GetAlarmHistory(0)
	if len(history) != 2 {
		t.Errorf("Expected 2 alarms with limit=0, got %d", len(history))
	}
}

func TestEdgeRuleEngineSetEventBus(t *testing.T) {
	engine := NewEdgeRuleEngine()
	bus := struct{}{}
	engine.SetEventBus(bus)
	// Just verify it doesn't panic
}

func TestEdgeRuleEngineLoadRulesFromStoreNil(t *testing.T) {
	engine := NewEdgeRuleEngine()
	engine.LoadRulesFromStore(nil) // Should not panic
}

func TestEdgeRuleEngineLoadRulesFromStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	store.SaveRule(&EdgeRule{
		RuleID: "r1", DeviceID: "d1", PointName: "p1",
		Operator: OpGT, Threshold: 50, Enabled: true,
	})

	engine := NewEdgeRuleEngine()
	engine.LoadRulesFromStore(store)

	rules := engine.GetRules()
	if len(rules) != 1 {
		t.Fatalf("Expected 1 loaded rule, got %d", len(rules))
	}
	if rules[0].RuleID != "r1" {
		t.Errorf("Expected r1, got %s", rules[0].RuleID)
	}
}

// ==================== Edge Trigger Executor Tests ====================

func TestEdgeTriggerExecutorWriteAction(t *testing.T) {
	executor := NewEdgeTriggerExecutor()

	var capturedDevice, capturedPoint string
	var capturedValue interface{}
	executor.SetWriteHandler(func(deviceID, point string, value interface{}) error {
		capturedDevice = deviceID
		capturedPoint = point
		capturedValue = value
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "write", Target: "d2", Params: map[string]interface{}{"point": "setpoint", "value": 42.0}},
	}}

	executor.ExecuteActions(alarm, rule)
	if capturedDevice != "d2" {
		t.Errorf("Expected device d2, got %s", capturedDevice)
	}
	if capturedPoint != "setpoint" {
		t.Errorf("Expected point setpoint, got %s", capturedPoint)
	}
	if capturedValue != 42.0 {
		t.Errorf("Expected value 42.0, got %v", capturedValue)
	}
}

func TestEdgeTriggerExecutorWriteActionDefaultTarget(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	var capturedDevice string
	executor.SetWriteHandler(func(deviceID, point string, value interface{}) error {
		capturedDevice = deviceID
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "write", Params: map[string]interface{}{"point": "setpoint", "value": 1}},
	}}

	executor.ExecuteActions(alarm, rule)
	if capturedDevice != "d1" {
		t.Errorf("Expected default device d1, got %s", capturedDevice)
	}
}

func TestEdgeTriggerExecutorWriteActionMissingPoint(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	executor.SetWriteHandler(func(deviceID, point string, value interface{}) error {
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "write", Params: map[string]interface{}{"value": 1}},
	}}

	// Should not panic, just log warning
	executor.ExecuteActions(alarm, rule)
}

func TestEdgeTriggerExecutorWriteActionNoHandler(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1"}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "write", Params: map[string]interface{}{"point": "p", "value": 1}},
	}}
	executor.ExecuteActions(alarm, rule) // Should not panic
}

func TestEdgeTriggerExecutorMQTTPublishAction(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	var capturedTopic string
	var capturedPayload []byte
	executor.SetMQTTPublisher(func(topic string, payload []byte) error {
		capturedTopic = topic
		capturedPayload = payload
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100, Severity: "high", Message: "too hot"}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "mqtt_publish", Target: "alerts/temperature"},
	}}

	executor.ExecuteActions(alarm, rule)
	if capturedTopic != "alerts/temperature" {
		t.Errorf("Expected topic alerts/temperature, got %s", capturedTopic)
	}
	if len(capturedPayload) == 0 {
		t.Error("Expected non-empty payload")
	}
}

func TestEdgeTriggerExecutorMQTTPublishDefaultTopic(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	var capturedTopic string
	executor.SetMQTTPublisher(func(topic string, payload []byte) error {
		capturedTopic = topic
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "dev1", PointName: "temp", Value: 100, Severity: "high", Message: "hot"}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "mqtt_publish"},
	}}

	executor.ExecuteActions(alarm, rule)
	if capturedTopic != "edgelite/alarms/dev1" {
		t.Errorf("Expected default topic edgelite/alarms/dev1, got %s", capturedTopic)
	}
}

func TestEdgeTriggerExecutorWebhookAction(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	var capturedURL string
	var capturedPayload []byte
	executor.SetWebhookSender(func(url string, payload []byte) error {
		capturedURL = url
		capturedPayload = payload
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100, Severity: "high", Timestamp: time.Now()}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "webhook", Target: "http://example.com/hook"},
	}}

	executor.ExecuteActions(alarm, rule)
	if capturedURL != "http://example.com/hook" {
		t.Errorf("Expected URL http://example.com/hook, got %s", capturedURL)
	}
	if len(capturedPayload) == 0 {
		t.Error("Expected non-empty payload")
	}
}

func TestEdgeTriggerExecutorWebhookMissingURL(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	executor.SetWebhookSender(func(url string, payload []byte) error {
		return nil
	})

	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1"}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "webhook"},
	}}
	executor.ExecuteActions(alarm, rule) // Should not panic
}

func TestEdgeTriggerExecutorLogAction(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1", PointName: "temp", Value: 100}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "log", Params: map[string]interface{}{"level": "info"}},
	}}
	executor.ExecuteActions(alarm, rule) // Should not panic
}

func TestEdgeTriggerExecutorUnknownAction(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	alarm := &AlarmRecord{RuleID: "r1", DeviceID: "d1"}
	rule := &EdgeRule{RuleID: "r1", Actions: []EdgeRuleAction{
		{Type: "unknown_action"},
	}}
	executor.ExecuteActions(alarm, rule) // Should not panic, just log
}

func TestEdgeTriggerExecutorNilRule(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	executor.ExecuteActions(&AlarmRecord{}, nil) // Should not panic
}

func TestEdgeTriggerExecutorEmptyActions(t *testing.T) {
	executor := NewEdgeTriggerExecutor()
	rule := &EdgeRule{RuleID: "r1"}
	executor.ExecuteActions(&AlarmRecord{}, rule) // Should not panic
}

// ==================== SafeEvalExpr Tests ====================

func TestSafeEvalExprSimple(t *testing.T) {
	result, err := SafeEvalExpr("1 + 2", nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if math.Abs(result-3) > 1e-9 {
		t.Errorf("Expected 3, got %f", result)
	}
}

func TestSafeEvalExprArithmetic(t *testing.T) {
	tests := []struct {
		expr     string
		expected float64
	}{
		{"2 + 3 * 4", 14},
		{"(2 + 3) * 4", 20},
		{"10 / 2", 5},
		{"10 - 3 - 2", 5},
		{"2 ^ 3", 8},
		{"10 % 3", 1},
		{"-5", -5},
		{"3 + -2", 1},
	}

	for _, tt := range tests {
		result, err := SafeEvalExpr(tt.expr, nil)
		if err != nil {
			t.Errorf("Expr %q: unexpected error: %v", tt.expr, err)
			continue
		}
		if math.Abs(result-tt.expected) > 1e-9 {
			t.Errorf("Expr %q: expected %f, got %f", tt.expr, tt.expected, result)
		}
	}
}

func TestSafeEvalExprVariables(t *testing.T) {
	vars := map[string]float64{"x": 10, "y": 20}
	result, err := SafeEvalExpr("x + y", vars)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result != 30 {
		t.Errorf("Expected 30, got %f", result)
	}
}

func TestSafeEvalExprConstants(t *testing.T) {
	result, err := SafeEvalExpr("pi", nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if math.Abs(result-math.Pi) > 1e-9 {
		t.Errorf("Expected pi, got %f", result)
	}

	result, err = SafeEvalExpr("e", nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if math.Abs(result-math.E) > 1e-9 {
		t.Errorf("Expected e, got %f", result)
	}
}

func TestSafeEvalExprMathFunctions(t *testing.T) {
	tests := []struct {
		expr     string
		expected float64
	}{
		{"abs(-5)", 5},
		{"sqrt(16)", 4},
		{"ceil(3.2)", 4},
		{"floor(3.8)", 3},
		{"round(3.5)", 4},
	}

	for _, tt := range tests {
		result, err := SafeEvalExpr(tt.expr, nil)
		if err != nil {
			t.Errorf("Expr %q: unexpected error: %v", tt.expr, err)
			continue
		}
		if math.Abs(result-tt.expected) > 1e-9 {
			t.Errorf("Expr %q: expected %f, got %f", tt.expr, tt.expected, result)
		}
	}
}

func TestSafeEvalExprDivisionByZero(t *testing.T) {
	_, err := SafeEvalExpr("1 / 0", nil)
	if err == nil {
		t.Error("Expected division by zero error")
	}
}

func TestSafeEvalExprUndefinedVariable(t *testing.T) {
	_, err := SafeEvalExpr("unknown_var", nil)
	if err == nil {
		t.Error("Expected undefined variable error")
	}
}

func TestSafeEvalExprUnknownFunction(t *testing.T) {
	_, err := SafeEvalExpr("foobar(1)", nil)
	if err == nil {
		t.Error("Expected unknown function error")
	}
}

func TestSafeEvalExprEmpty(t *testing.T) {
	_, err := SafeEvalExpr("", nil)
	if err == nil {
		t.Error("Expected error for empty expression")
	}
}

func TestSafeEvalExprUnexpectedToken(t *testing.T) {
	_, err := SafeEvalExpr(")", nil)
	// May or may not error depending on tokenizer, but should not panic
	_ = err
}

func TestSafeEvalExprMissingCloseParen(t *testing.T) {
	_, err := SafeEvalExpr("(1 + 2", nil)
	if err == nil {
		t.Error("Expected error for missing closing parenthesis")
	}
}

func TestTokenizeExpr(t *testing.T) {
	tokens := tokenizeExpr("1 + 2 * 3")
	expected := []string{"1", "+", "2", "*", "3"}
	if len(tokens) != len(expected) {
		t.Fatalf("Expected %d tokens, got %d", len(expected), len(tokens))
	}
	for i, tok := range tokens {
		if tok != expected[i] {
			t.Errorf("Token %d: expected %s, got %s", i, expected[i], tok)
		}
	}
}

func TestTokenizeExprWithWhitespace(t *testing.T) {
	tokens := tokenizeExpr("  1\t+\n2  ")
	if len(tokens) != 3 {
		t.Fatalf("Expected 3 tokens, got %d", len(tokens))
	}
}

func TestTokenizeExprUnknownChars(t *testing.T) {
	tokens := tokenizeExpr("1 @ 2")
	// @ should be skipped
	if len(tokens) != 2 {
		t.Errorf("Expected 2 tokens (skip @), got %d", len(tokens))
	}
}

// ==================== Offline Sync Manager Tests ====================

func TestOfflineSyncManagerCreate(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	if mgr == nil {
		t.Fatal("Expected non-nil manager")
	}
	if mgr.IsOnline() {
		t.Error("Expected offline by default")
	}
}

func TestOfflineSyncManagerDefaultParams(t *testing.T) {
	mgr := NewOfflineSyncManager(0, 0)
	if mgr.maxQueueSize != 10000 {
		t.Errorf("Expected default maxQueueSize=10000, got %d", mgr.maxQueueSize)
	}
}

func TestOfflineSyncManagerSetOnline(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetOnline(true)
	if !mgr.IsOnline() {
		t.Error("Expected online after SetOnline(true)")
	}
	mgr.SetOnline(false)
	if mgr.IsOnline() {
		t.Error("Expected offline after SetOnline(false)")
	}
}

func TestOfflineSyncManagerEnqueue(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	data := map[string]interface{}{"temp": 25.5}
	ok := mgr.Enqueue(data)
	if !ok {
		t.Error("Expected Enqueue to return true")
	}
	stats := mgr.GetStats()
	if stats["queue_size"] != 1 {
		t.Errorf("Expected queue_size=1, got %v", stats["queue_size"])
	}
}

func TestOfflineSyncManagerEnqueueOverflow(t *testing.T) {
	mgr := NewOfflineSyncManager(3, 5)
	for i := 0; i < 5; i++ {
		mgr.Enqueue(map[string]interface{}{"i": i})
	}
	stats := mgr.GetStats()
	if stats["queue_size"].(int) > 3 {
		t.Errorf("Expected queue_size <= 3, got %v", stats["queue_size"])
	}
}

func TestOfflineSyncManagerForceSyncSuccess(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetUploadCallback(func(data []map[string]interface{}) (int, error) {
		return len(data), nil
	})

	mgr.Enqueue(map[string]interface{}{"a": 1})
	mgr.Enqueue(map[string]interface{}{"b": 2})

	count := mgr.ForceSync()
	if count != 2 {
		t.Errorf("Expected 2 synced, got %d", count)
	}

	stats := mgr.GetStats()
	if stats["total_synced"].(int64) != 2 {
		t.Errorf("Expected total_synced=2, got %v", stats["total_synced"])
	}
}

func TestOfflineSyncManagerForceSyncNoCallback(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.Enqueue(map[string]interface{}{"a": 1})
	count := mgr.ForceSync()
	if count != 0 {
		t.Errorf("Expected 0 synced with no callback, got %d", count)
	}
}

func TestOfflineSyncManagerForceSyncEmptyQueue(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetUploadCallback(func(data []map[string]interface{}) (int, error) {
		return 0, nil
	})
	count := mgr.ForceSync()
	if count != 0 {
		t.Errorf("Expected 0 synced for empty queue, got %d", count)
	}
}

func TestOfflineSyncManagerForceSyncFailure(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetUploadCallback(func(data []map[string]interface{}) (int, error) {
		return 0, fmt.Errorf("upload failed")
	})

	mgr.Enqueue(map[string]interface{}{"a": 1})
	count := mgr.ForceSync()
	if count != 0 {
		t.Errorf("Expected 0 synced on failure, got %d", count)
	}

	stats := mgr.GetStats()
	if stats["total_failed"].(int64) != 1 {
		t.Errorf("Expected total_failed=1, got %v", stats["total_failed"])
	}
	// Data should be re-queued
	if stats["queue_size"].(int) != 1 {
		t.Errorf("Expected queue_size=1 after re-queue, got %v", stats["queue_size"])
	}
}

func TestOfflineSyncManagerStartStop(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr.SetUploadCallback(func(data []map[string]interface{}) (int, error) {
		return len(data), nil
	})
	mgr.SetOnline(true)
	mgr.Enqueue(map[string]interface{}{"a": 1})
	mgr.Start(ctx)

	time.Sleep(100 * time.Millisecond)
	mgr.Stop()
}

func TestOfflineSyncManagerGetStats(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetOnline(true)
	mgr.Enqueue(map[string]interface{}{"x": 1})

	stats := mgr.GetStats()
	if stats["online"] != true {
		t.Errorf("Expected online=true, got %v", stats["online"])
	}
	if stats["queue_size"] != 1 {
		t.Errorf("Expected queue_size=1, got %v", stats["queue_size"])
	}
	if stats["max_queue_size"] != 100 {
		t.Errorf("Expected max_queue_size=100, got %v", stats["max_queue_size"])
	}
	if stats["total_synced"].(int64) != 0 {
		t.Errorf("Expected total_synced=0, got %v", stats["total_synced"])
	}
	if stats["last_sync_time"] != nil {
		t.Errorf("Expected nil last_sync_time, got %v", stats["last_sync_time"])
	}
}

func TestOfflineSyncManagerGetStatsAfterSync(t *testing.T) {
	mgr := NewOfflineSyncManager(100, 5)
	mgr.SetUploadCallback(func(data []map[string]interface{}) (int, error) {
		return len(data), nil
	})
	mgr.Enqueue(map[string]interface{}{"x": 1})
	mgr.ForceSync()

	stats := mgr.GetStats()
	if stats["last_sync_time"] == nil {
		t.Error("Expected non-nil last_sync_time after sync")
	}
}

// ==================== Rule Store Tests ====================

func TestRuleStoreCRUD(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_crud.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	// Save
	rule := &EdgeRule{
		RuleID:    "rule-crud",
		DeviceID:  "device-1",
		PointName: "temperature",
		Operator:  OpGT,
		Threshold: 80,
		Enabled:   true,
		Severity:  "warning",
	}
	err := store.SaveRule(rule)
	if err != nil {
		t.Fatalf("SaveRule failed: %v", err)
	}

	// Load
	rules := store.LoadRules()
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}
	if rules[0].RuleID != "rule-crud" {
		t.Errorf("Expected rule-crud, got %s", rules[0].RuleID)
	}
	if rules[0].Threshold != 80 {
		t.Errorf("Expected threshold 80, got %f", rules[0].Threshold)
	}

	// Delete
	err = store.DeleteRule("rule-crud")
	if err != nil {
		t.Fatalf("DeleteRule failed: %v", err)
	}
	rules = store.LoadRules()
	if len(rules) != 0 {
		t.Errorf("Expected 0 rules after delete, got %d", len(rules))
	}
}

func TestRuleStoreUpdateRule(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_update.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	rule := &EdgeRule{RuleID: "r1", DeviceID: "d1", PointName: "p1", Threshold: 50}
	store.SaveRule(rule)

	// Update with new threshold
	rule.Threshold = 100
	store.SaveRule(rule)

	rules := store.LoadRules()
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}
	if rules[0].Threshold != 100 {
		t.Errorf("Expected updated threshold 100, got %f", rules[0].Threshold)
	}
}

func TestRuleStoreVersions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_versions.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	rule := &EdgeRule{RuleID: "r-v1", DeviceID: "d1", PointName: "p1", Threshold: 50}
	store.SaveRule(rule)
	rule.Threshold = 75
	store.SaveRule(rule)
	rule.Threshold = 100
	store.SaveRule(rule)

	versions, err := store.GetRuleVersions("r-v1")
	if err != nil {
		t.Fatalf("GetRuleVersions failed: %v", err)
	}
	if len(versions) != 3 {
		t.Errorf("Expected 3 versions, got %d", len(versions))
	}
}

func TestRuleStoreRollback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_rollback.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	rule := &EdgeRule{RuleID: "r-rb", DeviceID: "d1", PointName: "p1", Threshold: 50}
	store.SaveRule(rule) // v1
	rule.Threshold = 100
	store.SaveRule(rule) // v2

	// Rollback to v1
	rolledBack, err := store.RollbackRule("r-rb", 1)
	if err != nil {
		t.Fatalf("RollbackRule failed: %v", err)
	}
	if rolledBack.Threshold != 50 {
		t.Errorf("Expected rolled back threshold 50, got %f", rolledBack.Threshold)
	}
}

func TestRuleStoreRollbackNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_rb_notfound.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	_, err := store.RollbackRule("nonexistent", 1)
	if err == nil {
		t.Error("Expected error for rollback of nonexistent rule")
	}
}

func TestRuleStorePing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_ping.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	err := store.Ping()
	if err != nil {
		t.Errorf("Ping failed: %v", err)
	}
}

func TestRuleStoreIsHealthy(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_health.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	if !store.IsHealthy() {
		t.Error("Expected healthy store")
	}
}

func TestRuleStoreLoadRulesEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_empty.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	rules := store.LoadRules()
	if len(rules) != 0 {
		t.Errorf("Expected 0 rules, got %d", len(rules))
	}
}

func TestRuleStoreDeleteNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_del_ne.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	err := store.DeleteRule("nonexistent")
	// Should not error even if rule doesn't exist
	if err != nil {
		t.Errorf("DeleteRule should not error for non-existent rule: %v", err)
	}
}

func TestRuleStoreGetRuleVersionsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rules_gv_empty.db")
	store := NewRuleStore(dbPath)
	defer store.Close()

	versions, err := store.GetRuleVersions("nonexistent")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("Expected 0 versions, got %d", len(versions))
	}
}

// ==================== Packet Recorder Tests ====================

func TestPacketRecorderRecordAndGet(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus_tcp", "device-1", "010300000001840A")

	buf := pr.GetBuffer("modbus_tcp")
	if len(buf) != 1 {
		t.Fatalf("Expected 1 packet, got %d", len(buf))
	}
	if buf[0].Direction != "tx" {
		t.Errorf("Expected direction tx, got %s", buf[0].Direction)
	}
	if buf[0].Protocol != "modbus_tcp" {
		t.Errorf("Expected protocol modbus_tcp, got %s", buf[0].Protocol)
	}
	if buf[0].DeviceID != "device-1" {
		t.Errorf("Expected device-1, got %s", buf[0].DeviceID)
	}
	if buf[0].Content != "010300000001840A" {
		t.Errorf("Expected content, got %s", buf[0].Content)
	}
}

func TestPacketRecorderGetBufferAll(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus_tcp", "d1", "data1")
	pr.Record("rx", "s7", "d2", "data2")

	buf := pr.GetBuffer("")
	if len(buf) != 2 {
		t.Errorf("Expected 2 packets in combined buffer, got %d", len(buf))
	}

	buf = pr.GetBuffer("__all__")
	if len(buf) != 2 {
		t.Errorf("Expected 2 packets in __all__ buffer, got %d", len(buf))
	}
}

func TestPacketRecorderGetBufferSince(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus", "d1", "a")
	pr.Record("rx", "modbus", "d1", "b")
	pr.Record("tx", "s7", "d2", "c")

	// Get all modbus packets since seq 0
	buf := pr.GetBufferSince("modbus", 0)
	if len(buf) != 2 {
		t.Errorf("Expected 2 modbus packets since 0, got %d", len(buf))
	}

	// Get packets since first seq
	if len(buf) > 0 {
		buf = pr.GetBufferSince("modbus", buf[0].Seq)
		if len(buf) != 1 {
			t.Errorf("Expected 1 packet since first seq, got %d", len(buf))
		}
	}

	// Get all since 0
	buf = pr.GetBufferSince("", 0)
	if len(buf) != 3 {
		t.Errorf("Expected 3 all packets since 0, got %d", len(buf))
	}
}

func TestPacketRecorderClear(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus", "d1", "a")
	pr.Record("rx", "s7", "d2", "b")

	pr.Clear()
	buf := pr.GetBuffer("")
	if len(buf) != 0 {
		t.Errorf("Expected 0 packets after clear, got %d", len(buf))
	}
}

func TestPacketRecorderClearProtocol(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus", "d1", "a")
	pr.Record("rx", "s7", "d2", "b")

	pr.ClearProtocol("modbus")
	buf := pr.GetBuffer("modbus")
	if len(buf) != 0 {
		t.Errorf("Expected 0 modbus packets after clear, got %d", len(buf))
	}
	buf = pr.GetBuffer("s7")
	if len(buf) != 1 {
		t.Errorf("Expected 1 s7 packet, got %d", len(buf))
	}
}

func TestPacketRecorderGetStats(t *testing.T) {
	pr := NewPacketRecorder()
	pr.Record("tx", "modbus", "d1", "a")
	pr.Record("rx", "modbus", "d1", "b")
	pr.Record("tx", "s7", "d2", "c")

	stats := pr.GetStats()
	if stats["total_packets"] != 3 {
		t.Errorf("Expected 3 total packets, got %v", stats["total_packets"])
	}
	byProto := stats["by_protocol"].(map[string]int)
	if byProto["modbus"] != 2 {
		t.Errorf("Expected 2 modbus packets, got %v", byProto["modbus"])
	}
	if byProto["s7"] != 1 {
		t.Errorf("Expected 1 s7 packet, got %v", byProto["s7"])
	}
}

func TestPacketRecorderOverflow(t *testing.T) {
	pr := NewPacketRecorder()
	pr.maxSize = 3
	for i := 0; i < 5; i++ {
		pr.Record("tx", "test", "d1", "data")
	}
	buf := pr.GetBuffer("test")
	if len(buf) != 3 {
		t.Errorf("Expected 3 packets (overflow), got %d", len(buf))
	}
}

func TestRecordPacketGlobal(t *testing.T) {
	// Test the global function
	recorder := GetPacketRecorder()
	recorder.Clear()
	RecordPacket("tx", "global_test", "d1", "test_data")
	buf := recorder.GetBuffer("global_test")
	if len(buf) != 1 {
		t.Errorf("Expected 1 global packet, got %d", len(buf))
	}
}

// ==================== Modbus Address Parsing Tests ====================

func TestParseModbusAddressHolding(t *testing.T) {
	regType, addr, qty, err := parseModbusAddress("HR100")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "holding" {
		t.Errorf("Expected holding, got %s", regType)
	}
	if addr != 100 {
		t.Errorf("Expected addr 100 (ProtoForge/PLC convention: HR100 -> offset 100), got %d", addr)
	}
	if qty != 0 {
		t.Errorf("Expected qty 0 (infer from data type) for address without .N suffix, got %d", qty)
	}
}

func TestParseModbusAddressInput(t *testing.T) {
	regType, _, _, err := parseModbusAddress("IR200")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "input" {
		t.Errorf("Expected input, got %s", regType)
	}
}

func TestParseModbusAddressCoil(t *testing.T) {
	regType, _, _, err := parseModbusAddress("COIL5")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "coil" {
		t.Errorf("Expected coil, got %s", regType)
	}
}

func TestParseModbusAddressDiscrete(t *testing.T) {
	regType, _, _, err := parseModbusAddress("DI10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "discrete" {
		t.Errorf("Expected discrete, got %s", regType)
	}
}

func TestParseModbusAddress4xNotation(t *testing.T) {
	regType, _, _, err := parseModbusAddress("4x100")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "holding" {
		t.Errorf("Expected holding, got %s", regType)
	}
}

func TestParseModbusAddress3xNotation(t *testing.T) {
	regType, _, _, err := parseModbusAddress("3x200")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "input" {
		t.Errorf("Expected input, got %s", regType)
	}
}

func TestParseModbusAddress0xNotation(t *testing.T) {
	regType, _, _, err := parseModbusAddress("0x5")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "coil" {
		t.Errorf("Expected coil, got %s", regType)
	}
}

func TestParseModbusAddress1xNotation(t *testing.T) {
	regType, _, _, err := parseModbusAddress("1x10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "discrete" {
		t.Errorf("Expected discrete, got %s", regType)
	}
}

func TestParseModbusAddressWithQuantity(t *testing.T) {
	_, _, qty, err := parseModbusAddress("HR100.4")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if qty != 4 {
		t.Errorf("Expected qty 4, got %d", qty)
	}
}

func TestParseModbusAddressDefaultHolding(t *testing.T) {
	regType, _, _, err := parseModbusAddress("100")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if regType != "holding" {
		t.Errorf("Expected default holding, got %s", regType)
	}
}

func TestParseModbusAddressEmpty(t *testing.T) {
	_, _, _, err := parseModbusAddress("")
	if err == nil {
		t.Error("Expected error for empty address")
	}
}

func TestParseModbusAddressInvalid(t *testing.T) {
	_, _, _, err := parseModbusAddress("HRabc")
	if err == nil {
		t.Error("Expected error for invalid address")
	}
}

func TestParseModbusAddressInvalidQuantity(t *testing.T) {
	_, _, qty, err := parseModbusAddress("HR100.abc")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if qty != 0 {
		t.Errorf("Expected qty=0 (infer from data type) for unparseable quantity, got %d", qty)
	}
}

// ==================== Modbus Value Decoding Tests ====================

func TestDecodeModbusValueInt16(t *testing.T) {
	val, err := decodeModbusValue([]uint16{0x7FFF}, "int16")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if val.(int16) != 32767 {
		t.Errorf("Expected 32767, got %v", val)
	}
}

func TestDecodeModbusValueUInt16(t *testing.T) {
	val, err := decodeModbusValue([]uint16{65535}, "uint16")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if val.(uint16) != 65535 {
		t.Errorf("Expected 65535, got %v", val)
	}
}

func TestDecodeModbusValueInt32(t *testing.T) {
	val, err := decodeModbusValue([]uint16{0x0001, 0x0000}, "int32")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if val.(int32) != 65536 {
		t.Errorf("Expected 65536, got %v", val)
	}
}

func TestDecodeModbusValueFloat32(t *testing.T) {
	// 1.0 in IEEE 754 = 0x3F800000
	val, err := decodeModbusValue([]uint16{0x3F80, 0x0000}, "float32")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if math.Abs(float64(val.(float32))-1.0) > 1e-6 {
		t.Errorf("Expected 1.0, got %v", val)
	}
}

func TestDecodeModbusValueBool(t *testing.T) {
	val, err := decodeModbusValue([]uint16{1}, "bool")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if val.(bool) != true {
		t.Errorf("Expected true, got %v", val)
	}
}

func TestDecodeModbusValueDefaultType(t *testing.T) {
	val, err := decodeModbusValue([]uint16{42}, "unknown_type")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if val.(uint16) != 42 {
		t.Errorf("Expected 42, got %v", val)
	}
}

func TestDecodeModbusValueEmpty(t *testing.T) {
	_, err := decodeModbusValue([]uint16{}, "int16")
	if err == nil {
		t.Error("Expected error for empty registers")
	}
}

func TestDecodeModbusValueInsufficientRegs(t *testing.T) {
	_, err := decodeModbusValue([]uint16{1}, "int32")
	if err == nil {
		t.Error("Expected error for insufficient registers")
	}
}

// ==================== Modbus CRC Tests ====================

func TestCalculateCRC16(t *testing.T) {
	// Known CRC-16/Modbus test vector
	// CRC of "123456789" (ASCII) = 0x4B37
	data := []byte("123456789")
	crc := calculateCRC16(data)
	if crc != 0x4B37 {
		t.Errorf("Expected CRC 0x4B37, got 0x%04X", crc)
	}
}

func TestVerifyCRCValid(t *testing.T) {
	data := []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01}
	crc := calculateCRC16(data)
	frame := append(data, byte(crc), byte(crc>>8))
	if !verifyCRC(frame) {
		t.Error("Expected CRC to be valid")
	}
}

func TestVerifyCRCInvalid(t *testing.T) {
	data := []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01, 0xFF, 0xFF}
	if verifyCRC(data) {
		t.Error("Expected CRC to be invalid")
	}
}

func TestVerifyCRCTooShort(t *testing.T) {
	if verifyCRC([]byte{0x01}) {
		t.Error("Expected false for too-short frame")
	}
}

// ==================== Modbus Utility Tests ====================

func TestUint16ToBytes(t *testing.T) {
	b := uint16ToBytes(0x1234, 0x5678)
	if len(b) != 4 {
		t.Fatalf("Expected 4 bytes, got %d", len(b))
	}
	if b[0] != 0x12 || b[1] != 0x34 || b[2] != 0x56 || b[3] != 0x78 {
		t.Errorf("Expected [0x12, 0x34, 0x56, 0x78], got %v", b)
	}
}

func TestFloat32FromBits(t *testing.T) {
	// 1.0 in IEEE 754 = 0x3F800000
	val := float32FromBits(0x3F800000)
	if val != 1.0 {
		t.Errorf("Expected 1.0, got %v", val)
	}
}

func TestFloat64FromBits(t *testing.T) {
	// 1.0 in IEEE 754 double = 0x3FF0000000000000
	val := float64FromBits(0x3FF0000000000000)
	if val != 1.0 {
		t.Errorf("Expected 1.0, got %v", val)
	}
}

func TestReadUint16BEDriver(t *testing.T) {
	b := []byte{0x12, 0x34, 0x56}
	val := readUint16BE(b, 0)
	if val != 0x1234 {
		t.Errorf("Expected 0x1234, got 0x%04X", val)
	}
}

func TestReadUint16BEDriverOutOfBounds(t *testing.T) {
	b := []byte{0x12}
	val := readUint16BE(b, 0)
	if val != 0 {
		t.Errorf("Expected 0 for out-of-bounds, got 0x%04X", val)
	}
}

func TestReadUint32BEDriver(t *testing.T) {
	b := []byte{0x12, 0x34, 0x56, 0x78}
	val := readUint32BE(b, 0)
	if val != 0x12345678 {
		t.Errorf("Expected 0x12345678, got 0x%08X", val)
	}
}

func TestReadUint32BEDriverOutOfBounds(t *testing.T) {
	b := []byte{0x12, 0x34}
	val := readUint32BE(b, 0)
	if val != 0 {
		t.Errorf("Expected 0 for out-of-bounds, got 0x%08X", val)
	}
}

// ==================== S7 Address Parsing Tests ====================

func TestParseS7AddressDBBit(t *testing.T) {
	area, dbNum, dt, byteOff, bitOff, err := parseS7Address("DB1.DBX0.0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaDB {
		t.Errorf("Expected area s7AreaDB, got 0x%02X", area)
	}
	if dbNum != 1 {
		t.Errorf("Expected dbNum 1, got %d", dbNum)
	}
	if dt != s7TypeBit {
		t.Errorf("Expected type bit, got %c", dt)
	}
	if byteOff != 0 {
		t.Errorf("Expected byteOff 0, got %d", byteOff)
	}
	if bitOff != 0 {
		t.Errorf("Expected bitOff 0, got %d", bitOff)
	}
}

func TestParseS7AddressDBByte(t *testing.T) {
	_, _, dt, byteOff, _, err := parseS7Address("DB1.DBB10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dt != s7TypeByte {
		t.Errorf("Expected type byte, got %c", dt)
	}
	if byteOff != 10 {
		t.Errorf("Expected byteOff 10, got %d", byteOff)
	}
}

func TestParseS7AddressDBWord(t *testing.T) {
	_, _, dt, byteOff, _, err := parseS7Address("DB1.DBW10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dt != s7TypeWord {
		t.Errorf("Expected type word, got %c", dt)
	}
	if byteOff != 10 {
		t.Errorf("Expected byteOff 10, got %d", byteOff)
	}
}

func TestParseS7AddressDBDWord(t *testing.T) {
	_, _, dt, byteOff, _, err := parseS7Address("DB1.DBD10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dt != s7TypeDWord {
		t.Errorf("Expected type dword, got %c", dt)
	}
	if byteOff != 10 {
		t.Errorf("Expected byteOff 10, got %d", byteOff)
	}
}

func TestParseS7AddressMerker(t *testing.T) {
	area, _, dt, byteOff, _, err := parseS7Address("MW10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaMK {
		t.Errorf("Expected area s7AreaMK, got 0x%02X", area)
	}
	if dt != s7TypeWord {
		t.Errorf("Expected type word, got %c", dt)
	}
	if byteOff != 10 {
		t.Errorf("Expected byteOff 10, got %d", byteOff)
	}
}

func TestParseS7AddressInput(t *testing.T) {
	area, _, _, _, _, err := parseS7Address("IW0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaPE {
		t.Errorf("Expected area s7AreaPE, got 0x%02X", area)
	}
}

func TestParseS7AddressOutput(t *testing.T) {
	area, _, _, _, _, err := parseS7Address("QW0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaPA {
		t.Errorf("Expected area s7AreaPA, got 0x%02X", area)
	}
}

func TestParseS7AddressEInput(t *testing.T) {
	area, _, _, _, _, err := parseS7Address("EW0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaPE {
		t.Errorf("Expected area s7AreaPE (E prefix), got 0x%02X", area)
	}
}

func TestParseS7AddressAOutput(t *testing.T) {
	area, _, _, _, _, err := parseS7Address("AW0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != s7AreaPA {
		t.Errorf("Expected area s7AreaPA (A prefix), got 0x%02X", area)
	}
}

func TestParseS7AddressBitWithDot(t *testing.T) {
	_, _, dt, _, bitOff, err := parseS7Address("M10.5")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dt != s7TypeBit {
		t.Errorf("Expected type bit, got %c", dt)
	}
	if bitOff != 5 {
		t.Errorf("Expected bitOff 5, got %d", bitOff)
	}
}

func TestParseS7AddressEmpty(t *testing.T) {
	_, _, _, _, _, err := parseS7Address("")
	if err == nil {
		t.Error("Expected error for empty address")
	}
}

func TestParseS7AddressInvalidDB(t *testing.T) {
	_, _, _, _, _, err := parseS7Address("DB1")
	if err == nil {
		t.Error("Expected error for invalid DB address")
	}
}

func TestParseS7AddressInvalidDBNum(t *testing.T) {
	_, _, _, _, _, err := parseS7Address("DBX.DBW0")
	if err == nil {
		t.Error("Expected error for invalid DB number")
	}
}

func TestParseS7AddressUnsupported(t *testing.T) {
	_, _, _, _, _, err := parseS7Address("Z10")
	if err == nil {
		t.Error("Expected error for unsupported address format")
	}
}

func TestParseS7AddressInvalidBitNumber(t *testing.T) {
	_, _, _, _, _, err := parseS7Address("M10.8")
	if err == nil {
		t.Error("Expected error for bit number > 7")
	}
}

func TestParseS7TypeAndOffsetEmpty(t *testing.T) {
	_, _, _, _, _, err := parseS7TypeAndOffset("", s7AreaMK, 0)
	if err == nil {
		t.Error("Expected error for empty type and offset")
	}
}

func TestToInt64(t *testing.T) {
	tests := []struct {
		input    interface{}
		expected int64
	}{
		{int(42), 42},
		{int8(8), 8},
		{int16(16), 16},
		{int32(32), 32},
		{int64(64), 64},
		{uint(42), 42},
		{uint8(8), 8},
		{uint16(16), 16},
		{uint32(32), 32},
		{uint64(64), 64},
		{float32(3.14), 3},
		{float64(3.99), 3},
		{true, 1},
		{false, 0},
	}

	for _, tt := range tests {
		result := toInt64(tt.input)
		if result != tt.expected {
			t.Errorf("toInt64(%v): expected %d, got %d", tt.input, tt.expected, result)
		}
	}
}

func TestToInt64UnknownType(t *testing.T) {
	result := toInt64("not a number")
	if result != 0 {
		t.Errorf("Expected 0 for unknown type, got %d", result)
	}
}

// ==================== FINS Address Parsing Tests ====================

func TestParseFINSAddressCIO(t *testing.T) {
	area, offset, _, isBit, err := parseFINSAddress("CIO100")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != finsAreaCIO {
		t.Errorf("Expected area finsAreaCIO, got 0x%02X", area)
	}
	if offset != 100 {
		t.Errorf("Expected offset 100, got %d", offset)
	}
	if isBit {
		t.Error("Expected isBit=false")
	}
}

func TestParseFINSAddressDM(t *testing.T) {
	area, _, _, _, err := parseFINSAddress("D200")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != finsAreaDM {
		t.Errorf("Expected area finsAreaDM, got 0x%02X", area)
	}
}

func TestParseFINSAddressWR(t *testing.T) {
	area, _, _, _, err := parseFINSAddress("W300")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != finsAreaWR {
		t.Errorf("Expected area finsAreaWR, got 0x%02X", area)
	}
}

func TestParseFINSAddressHR(t *testing.T) {
	area, _, _, _, err := parseFINSAddress("H400")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != finsAreaHR {
		t.Errorf("Expected area finsAreaHR, got 0x%02X", area)
	}
}

func TestParseFINSAddressAR(t *testing.T) {
	area, _, _, _, err := parseFINSAddress("A500")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if area != finsAreaAR {
		t.Errorf("Expected area finsAreaAR, got 0x%02X", area)
	}
}

func TestParseFINSAddressWithBit(t *testing.T) {
	_, _, bitOff, isBit, err := parseFINSAddress("CIO100.5")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !isBit {
		t.Error("Expected isBit=true")
	}
	if bitOff != 5 {
		t.Errorf("Expected bitOff 5, got %d", bitOff)
	}
}

func TestParseFINSAddressEmpty(t *testing.T) {
	_, _, _, _, err := parseFINSAddress("")
	if err == nil {
		t.Error("Expected error for empty address")
	}
}

func TestParseFINSAddressUnknownArea(t *testing.T) {
	_, _, _, _, err := parseFINSAddress("Z100")
	if err == nil {
		t.Error("Expected error for unknown area")
	}
}

func TestParseFINSAddressInvalidOffset(t *testing.T) {
	_, _, _, _, err := parseFINSAddress("Dabc")
	if err == nil {
		t.Error("Expected error for invalid offset")
	}
}

func TestParseFINSAddressInvalidBit(t *testing.T) {
	_, _, _, _, err := parseFINSAddress("D100.16")
	if err == nil {
		t.Error("Expected error for bit > 15")
	}
}

// ==================== MC Address Parsing Tests ====================

func TestParseMCAddressD(t *testing.T) {
	device, address, err := parseMCAddress("D100")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if device != "D" {
		t.Errorf("Expected D, got %s", device)
	}
	if address != 100 {
		t.Errorf("Expected 100, got %d", address)
	}
}

func TestParseMCAddressM(t *testing.T) {
	device, _, err := parseMCAddress("M50")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if device != "M" {
		t.Errorf("Expected M, got %s", device)
	}
}

func TestParseMCAddressTwoLetterDevice(t *testing.T) {
	device, _, err := parseMCAddress("CN10")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if device != "CN" {
		t.Errorf("Expected CN, got %s", device)
	}
}

func TestParseMCAddressEmpty(t *testing.T) {
	_, _, err := parseMCAddress("")
	if err == nil {
		t.Error("Expected error for empty address")
	}
}

func TestParseMCAddressNoDevice(t *testing.T) {
	_, _, err := parseMCAddress("100")
	if err == nil {
		t.Error("Expected error for no device prefix")
	}
}

func TestParseMCAddressTooLongDevice(t *testing.T) {
	_, _, err := parseMCAddress("ABCD100")
	if err == nil {
		t.Error("Expected error for too-long device prefix")
	}
}

func TestParseMCAddressMissingNumber(t *testing.T) {
	_, _, err := parseMCAddress("D")
	if err == nil {
		t.Error("Expected error for missing address number")
	}
}

func TestParseMCAddressInvalidNumber(t *testing.T) {
	_, _, err := parseMCAddress("Dabc")
	if err == nil {
		t.Error("Expected error for invalid address number")
	}
}

func TestIsMCBitDevice(t *testing.T) {
	bitDevices := []string{"M", "X", "Y", "S", "B", "F"}
	for _, d := range bitDevices {
		if !isMCBitDevice(d) {
			t.Errorf("Expected %s to be a bit device", d)
		}
	}
	if isMCBitDevice("D") {
		t.Error("Expected D to NOT be a bit device")
	}
	if isMCBitDevice("CN") {
		t.Error("Expected CN to NOT be a bit device")
	}
}

func TestDecodeMCValueInt16(t *testing.T) {
	val := decodeMCValue([]uint16{100}, "int16")
	if val.(int16) != 100 {
		t.Errorf("Expected 100, got %v", val)
	}
}

func TestDecodeMCValueUInt16(t *testing.T) {
	val := decodeMCValue([]uint16{200}, "uint16")
	if val.(uint16) != 200 {
		t.Errorf("Expected 200, got %v", val)
	}
}

func TestDecodeMCValueInt32(t *testing.T) {
	val := decodeMCValue([]uint16{0x0000, 0x0001}, "int32")
	if val.(int32) != 65536 {
		t.Errorf("Expected 65536, got %v", val)
	}
}

func TestDecodeMCValueFloat32(t *testing.T) {
	val := decodeMCValue([]uint16{0x0000, 0x3F80}, "float32")
	if float32(val.(float32)) != 1.0 {
		t.Errorf("Expected 1.0, got %v", val)
	}
}

func TestDecodeMCValueBool(t *testing.T) {
	val := decodeMCValue([]uint16{1}, "bool")
	if val.(bool) != true {
		t.Errorf("Expected true, got %v", val)
	}
}

func TestDecodeMCValueDefault(t *testing.T) {
	val := decodeMCValue([]uint16{42}, "unknown")
	if val.(uint16) != 42 {
		t.Errorf("Expected 42, got %v", val)
	}
}

func TestDecodeMCValueEmpty(t *testing.T) {
	val := decodeMCValue([]uint16{}, "int16")
	if val != nil {
		t.Errorf("Expected nil for empty regs, got %v", val)
	}
}

// ==================== OPC UA / ONVIF Utility Tests ====================

func TestParseOPCUAEndpoint(t *testing.T) {
	host, port := parseOPCUAEndpoint("opc.tcp://192.168.1.1:4840")
	if host != "192.168.1.1" {
		t.Errorf("Expected host 192.168.1.1, got %s", host)
	}
	if port != 4840 {
		t.Errorf("Expected port 4840, got %d", port)
	}
}

func TestParseOPCUAEndpointDefaultPort(t *testing.T) {
	host, port := parseOPCUAEndpoint("opc.tcp://192.168.1.1")
	if host != "192.168.1.1" {
		t.Errorf("Expected host 192.168.1.1, got %s", host)
	}
	if port != 4840 {
		t.Errorf("Expected default port 4840, got %d", port)
	}
}

func TestParseOPCUAEndpointWithPath(t *testing.T) {
	host, port := parseOPCUAEndpoint("opc.tcp://192.168.1.1:4840/path/to/endpoint")
	if host != "192.168.1.1" {
		t.Errorf("Expected host 192.168.1.1, got %s", host)
	}
	if port != 4840 {
		t.Errorf("Expected port 4840, got %d", port)
	}
}

func TestIndexOf(t *testing.T) {
	if indexOf("hello world", "world") != 6 {
		t.Error("Expected index 6")
	}
	if indexOf("hello", "xyz") >= 0 {
		t.Error("Expected -1 for not found")
	}
	if indexOf("", "test") >= 0 {
		t.Error("Expected -1 for empty string")
	}
}

func TestParseONVIFResponseDeviceInfo(t *testing.T) {
	soap := `<tds:Manufacturer>Axis</tds:Manufacturer>`
	val := parseONVIFResponse(soap, "device_info")
	if val != "Axis" {
		t.Errorf("Expected Axis, got %v", val)
	}
}

func TestParseONVIFResponseDeviceInfoUnknown(t *testing.T) {
	val := parseONVIFResponse("no data", "device_info")
	if val != "unknown" {
		t.Errorf("Expected unknown, got %v", val)
	}
}

func TestParseONVIFResponseVideo(t *testing.T) {
	soap := `<trt:Profiles token="1"/><trt:Profiles token="2"/>`
	val := parseONVIFResponse(soap, "video")
	if val.(int) != 2 {
		t.Errorf("Expected 2 profiles, got %v", val)
	}
}

func TestParseONVIFResponsePTZ(t *testing.T) {
	soap := `<tptz:Status>idle</tptz:Status>`
	val := parseONVIFResponse(soap, "ptz")
	if val != "idle" {
		t.Errorf("Expected idle, got %v", val)
	}
}

func TestParseONVIFResponseMotion(t *testing.T) {
	soap := `<tds:Motion>true</tds:Motion>`
	val := parseONVIFResponse(soap, "motion")
	if val.(bool) != true {
		t.Errorf("Expected true, got %v", val)
	}
}

func TestParseONVIFResponseIO(t *testing.T) {
	soap := `<tds:idle>false</tds:idle>`
	val := parseONVIFResponse(soap, "io")
	if val.(bool) != false {
		t.Errorf("Expected false, got %v", val)
	}
}

func TestParseONVIFDeviceAddress(t *testing.T) {
	soap := `<XAddrs>http://192.168.1.1/onvif/device_service</XAddrs>`
	addr, ok := parseONVIFDeviceAddress(soap)
	if !ok {
		t.Error("Expected ok=true")
	}
	if addr != "http://192.168.1.1/onvif/device_service" {
		t.Errorf("Expected URL, got %s", addr)
	}
}

func TestParseONVIFDeviceAddressNotFound(t *testing.T) {
	_, ok := parseONVIFDeviceAddress("no xaddrs here")
	if ok {
		t.Error("Expected ok=false")
	}
}

func TestGenerateSimulatedValue(t *testing.T) {
	min := 0.0
	max := 100.0
	pt := models.PointDef{
		Name:     "temp",
		DataType: "float64",
		Min:      &min,
		Max:      &max,
	}

	val := generateSimulatedValue(pt)
	f := val.(float64)
	if f < min || f > max {
		t.Errorf("Expected value in range [%f, %f], got %f", min, max, f)
	}
}

func TestGenerateSimulatedValueInt16(t *testing.T) {
	min := 0.0
	max := 100.0
	pt := models.PointDef{
		Name:     "count",
		DataType: "int16",
		Min:      &min,
		Max:      &max,
	}

	val := generateSimulatedValue(pt)
	_, ok := val.(int16)
	if !ok {
		t.Errorf("Expected int16, got %T", val)
	}
}

func TestGenerateSimulatedValueBool(t *testing.T) {
	min := 0.0
	max := 100.0
	pt := models.PointDef{
		Name:     "flag",
		DataType: "bool",
		Min:      &min,
		Max:      &max,
	}

	val := generateSimulatedValue(pt)
	_, ok := val.(bool)
	if !ok {
		t.Errorf("Expected bool, got %T", val)
	}
}

func TestGenerateSimulatedValueDefaultRange(t *testing.T) {
	pt := models.PointDef{
		Name:     "default",
		DataType: "float64",
	}

	val := generateSimulatedValue(pt)
	f := val.(float64)
	if f < 0 || f > 100 {
		t.Errorf("Expected value in default range [0, 100], got %f", f)
	}
}

func TestMathSin(t *testing.T) {
	result := mathSin(0)
	if math.Abs(result) > 1e-9 {
		t.Errorf("Expected sin(0)=0, got %f", result)
	}
	result = mathSin(math.Pi / 2)
	if math.Abs(result-1) > 1e-9 {
		t.Errorf("Expected sin(pi/2)=1, got %f", result)
	}
}

func TestGenerateNonce(t *testing.T) {
	nonce := generateNonce()
	if nonce == "" {
		t.Error("Expected non-empty nonce")
	}
	// Should be base64 encoded 16 bytes = 24 chars
	if len(nonce) < 20 {
		t.Errorf("Expected nonce length >= 20, got %d", len(nonce))
	}
}

func TestComputeSoapDigest(t *testing.T) {
	nonce := "dGVzdG5vbmNl" // base64 of "testnonce"
	created := "2025-01-01T00:00:00Z"
	password := "secret"
	digest := computeSoapDigest(nonce, created, password)
	if digest == "" {
		t.Error("Expected non-empty digest")
	}
}

func TestComputeSoapDigestInvalidNonce(t *testing.T) {
	// Invalid base64 nonce should fall back to raw bytes
	digest := computeSoapDigest("!!!invalidbase64!!!", "2025-01-01T00:00:00Z", "pass")
	if digest == "" {
		t.Error("Expected non-empty digest even with invalid nonce")
	}
}

func TestMustNewRequest(t *testing.T) {
	req := mustNewRequest("GET", "http://example.com", nil)
	if req == nil {
		t.Fatal("Expected non-nil request")
	}
	if req.Method != "GET" {
		t.Errorf("Expected GET, got %s", req.Method)
	}
}

// ==================== TCP Write Test ====================

func TestTcpWrite(t *testing.T) {
	// Create a simple pipe to test tcpWrite
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		buf := make([]byte, 10)
		n, _ := server.Read(buf)
		_ = n
	}()

	err := tcpWrite(client, []byte("test"), 5*time.Second)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

// ==================== Modbus Slave Driver Tests ====================

func TestModbusSlaveDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":          "127.0.0.1",
		"port":          10502,
		"holding_size":  100,
		"input_size":    50,
		"coil_size":     50,
		"discrete_size": 30,
	}
	driver, err := NewModbusSlaveDriver("slave-1", config)
	if err != nil {
		t.Fatalf("NewModbusSlaveDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "modbus_slave" {
		t.Errorf("Expected name modbus_slave, got %s", driver.Name())
	}
}

func TestModbusSlaveDriverReadPointsDisconnected(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 10503,
	}
	driver, _ := NewModbusSlaveDriver("slave-2", config)
	pts := []models.PointDef{{Name: "test", Address: "HR1", DataType: "uint16"}}
	_, _ = driver.ReadPoints(context.Background(), pts)
}

func TestModbusSlaveDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 10504,
	}
	driver, _ := NewModbusSlaveDriver("slave-3", config)
	err := driver.HealthCheck(context.Background())
	// Not connected, should return error or nil
	_ = err
}

func TestModbusSlaveDriverDiscover(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 10505,
	}
	driver, _ := NewModbusSlaveDriver("slave-4", config)
	// A slave is what gets found, not a scanner: it has no peer to ask, and the
	// old empty-list answer made that look like "nothing on the network".
	if _, err := driver.Discover(context.Background(), nil); !errors.Is(err, ErrDiscoveryUnsupported) {
		t.Fatalf("Discover error = %v, want ErrDiscoveryUnsupported", err)
	}
}

// ==================== Modbus RTU Driver Tests ====================

func TestModbusRTUDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":     "127.0.0.1",
		"port":     502,
		"slave_id": 1,
		"timeout":  5.0,
	}
	driver, err := NewModbusRTUDriver("rtu-1", config)
	if err != nil {
		t.Fatalf("NewModbusRTUDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "modbus_rtu" {
		t.Errorf("Expected name modbus_rtu, got %s", driver.Name())
	}
}

func TestModbusRTUDriverConnect(t *testing.T) {
	config := map[string]interface{}{
		"host":     "127.0.0.1",
		"port":     19999, // Non-existent server
		"slave_id": 1,
		"timeout":  0.5,
	}
	driver, _ := NewModbusRTUDriver("rtu-2", config)
	_ = driver.Connect(context.Background())
}

// ==================== Modbus TCP Driver Tests ====================

func TestModbusTCPDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":     "127.0.0.1",
		"port":     502,
		"slave_id": 1,
		"timeout":  5.0,
	}
	driver, err := NewModbusTCPDriver("mbtcp-1", config)
	if err != nil {
		t.Fatalf("NewModbusTCPDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "modbus_tcp" {
		t.Errorf("Expected name modbus_tcp, got %s", driver.Name())
	}
}

// ==================== OPC UA Driver Tests ====================

func TestOPCUADriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:4840",
		"timeout":  5.0,
	}
	driver, err := NewOPCUADriver("opcua-1", config)
	if err != nil {
		t.Fatalf("NewOPCUADriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "opc_ua" {
		t.Errorf("Expected name opc_ua, got %s", driver.Name())
	}
}

// ==================== OPC DA Driver Tests ====================

func TestOPCDADriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"prog_id": "Matrikon.OPC.Simulation",
	}
	driver, err := NewOPCDADriver("opcda-1", config)
	if err != nil {
		t.Fatalf("NewOPCDADriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "opc_da" {
		t.Errorf("Expected name opc_da, got %s", driver.Name())
	}
}

// ==================== ONVIF Driver Tests ====================

func TestONVIFDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":     "192.168.1.100",
		"port":     80,
		"username": "admin",
		"password": "pass",
	}
	driver, err := NewONVIFDriver("onvif-1", config)
	if err != nil {
		t.Fatalf("NewONVIFDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "onvif" {
		t.Errorf("Expected name onvif, got %s", driver.Name())
	}
}

// ==================== S7 Driver Tests ====================

func TestS7DriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"port":    102,
		"rack":    0,
		"slot":    1,
		"timeout": 5.0,
	}
	driver, err := NewS7Driver("s7-1", config)
	if err != nil {
		t.Fatalf("NewS7Driver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "siemens_s7" {
		t.Errorf("Expected name siemens_s7, got %s", driver.Name())
	}
}

// ==================== FINS Driver Tests ====================

func TestFINSDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"port":    9600,
		"node":    1,
		"timeout": 5.0,
	}
	driver, err := NewFINSDriver("fins-1", config)
	if err != nil {
		t.Fatalf("NewFINSDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "omron_fins" {
		t.Errorf("Expected name omron_fins, got %s", driver.Name())
	}
}

// ==================== MC Driver Tests ====================

func TestMCDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"port":    5007,
		"timeout": 5.0,
	}
	driver, err := NewMCDriver("mc-1", config)
	if err != nil {
		t.Fatalf("NewMCDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "mitsubishi_mc" {
		t.Errorf("Expected name mitsubishi_mc, got %s", driver.Name())
	}
}

// ==================== AB Driver Tests ====================

func TestABDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"port":    44818,
		"timeout": 5.0,
	}
	driver, err := NewABDriver("ab-1", config)
	if err != nil {
		t.Fatalf("NewABDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "allen_bradley" {
		t.Errorf("Expected name allen_bradley, got %s", driver.Name())
	}
}

// ==================== MQTT/HTTP Driver Tests ====================

func TestMQTTClientDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host":    "127.0.0.1",
		"port":    1883,
		"topic":   "test/topic",
		"timeout": 5.0,
	}
	driver, err := NewMQTTClientDriver("mqtt-1", config)
	if err != nil {
		t.Fatalf("NewMQTTClientDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "mqtt_client" {
		t.Errorf("Expected name mqtt_client, got %s", driver.Name())
	}
}

func TestHTTPWebhookDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"url":     "http://127.0.0.1:8080/webhook",
		"timeout": 5.0,
	}
	driver, err := NewHTTPWebhookDriver("http-1", config)
	if err != nil {
		t.Fatalf("NewHTTPWebhookDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Expected non-nil driver")
	}
	if driver.Name() != "http_webhook" {
		t.Errorf("Expected name http_webhook, got %s", driver.Name())
	}
}

func TestHTTPWebhookDriverHandleWebhook(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-2", config)
	wd := driver.(*HTTPWebhookDriver)
	payload := []byte(`{"temp":25.5,"hum":60}`)
	err := wd.HandleWebhook(payload)
	// May or may not error depending on point matching, just verify no panic
	_ = err
}

func TestHTTPWebhookDriverReadPoints(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-3", config)
	pts := []models.PointDef{{Name: "temp", DataType: "float64"}}
	_, err := driver.ReadPoints(context.Background(), pts)
	// Not connected, should handle gracefully
	_ = err
}

func TestHTTPWebhookDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-4", config)
	_ = driver.HealthCheck(context.Background())
}

func TestHTTPWebhookDriverDiscover(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-5", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestHTTPWebhookDriverWritePoint(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-6", config)
	_ = driver.WritePoint(context.Background(), "test_point", 42)
}

func TestHTTPWebhookDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{
		"url": "http://127.0.0.1:8080/hook",
	}
	driver, _ := NewHTTPWebhookDriver("http-7", config)
	_ = driver.Disconnect()
}

// ==================== MQTT Driver Additional Tests ====================

func TestMQTTClientDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 1883,
	}
	driver, _ := NewMQTTClientDriver("mqtt-2", config)
	_ = driver.Disconnect()
}

func TestMQTTClientDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 1883,
	}
	driver, _ := NewMQTTClientDriver("mqtt-3", config)
	_ = driver.HealthCheck(context.Background())
}

func TestMQTTClientDriverDiscover(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 1883,
	}
	driver, _ := NewMQTTClientDriver("mqtt-4", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestMQTTClientDriverWritePoint(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 1883,
	}
	driver, _ := NewMQTTClientDriver("mqtt-5", config)
	_ = driver.WritePoint(context.Background(), "test", 42)
}

// ==================== S7 Driver Additional Tests ====================

func TestS7DriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 102, "rack": 0, "slot": 1,
	}
	driver, _ := NewS7Driver("s7-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestS7DriverDiscover(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 102, "rack": 0, "slot": 1,
	}
	driver, _ := NewS7Driver("s7-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestS7DriverDisconnect(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 102, "rack": 0, "slot": 1,
	}
	driver, _ := NewS7Driver("s7-4", config)
	_ = driver.Disconnect()
}

// ==================== FINS Driver Additional Tests ====================

func TestFINSDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 9600}
	driver, _ := NewFINSDriver("fins-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestFINSDriverDiscover(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 9600}
	driver, _ := NewFINSDriver("fins-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestFINSDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 9600}
	driver, _ := NewFINSDriver("fins-4", config)
	_ = driver.Disconnect()
}

// ==================== MC Driver Additional Tests ====================

func TestMCDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 5007}
	driver, _ := NewMCDriver("mc-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestMCDriverDiscover(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 5007}
	driver, _ := NewMCDriver("mc-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestMCDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 5007}
	driver, _ := NewMCDriver("mc-4", config)
	_ = driver.Disconnect()
}

// ==================== AB Driver Additional Tests ====================

func TestABDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 44818}
	driver, _ := NewABDriver("ab-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestABDriverDiscover(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 44818}
	driver, _ := NewABDriver("ab-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestABDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "port": 44818}
	driver, _ := NewABDriver("ab-4", config)
	_ = driver.Disconnect()
}

// ==================== OPC UA Driver Additional Tests ====================

func TestOPCUADriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"endpoint": "opc.tcp://127.0.0.1:4840"}
	driver, _ := NewOPCUADriver("opcua-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestOPCUADriverDiscover(t *testing.T) {
	config := map[string]interface{}{"endpoint": "opc.tcp://127.0.0.1:4840"}
	driver, _ := NewOPCUADriver("opcua-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestOPCUADriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"endpoint": "opc.tcp://127.0.0.1:4840"}
	driver, _ := NewOPCUADriver("opcua-4", config)
	_ = driver.Disconnect()
}

// ==================== OPC DA Driver Additional Tests ====================

func TestOPCDADriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "prog_id": "Test"}
	driver, _ := NewOPCDADriver("opcda-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestOPCDADriverDiscover(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "prog_id": "Test"}
	driver, _ := NewOPCDADriver("opcda-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestOPCDADriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"host": "127.0.0.1", "prog_id": "Test"}
	driver, _ := NewOPCDADriver("opcda-4", config)
	_ = driver.Disconnect()
}

// ==================== ONVIF Driver Additional Tests ====================

func TestONVIFDriverHealthCheck(t *testing.T) {
	config := map[string]interface{}{"host": "192.168.1.100", "port": 80}
	driver, _ := NewONVIFDriver("onvif-2", config)
	_ = driver.HealthCheck(context.Background())
}

func TestONVIFDriverDiscover(t *testing.T) {
	config := map[string]interface{}{"host": "192.168.1.100", "port": 80}
	driver, _ := NewONVIFDriver("onvif-3", config)
	_, _ = driver.Discover(context.Background(), nil)
}

func TestONVIFDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{"host": "192.168.1.100", "port": 80}
	driver, _ := NewONVIFDriver("onvif-4", config)
	_ = driver.Disconnect()
}

// ==================== Modbus Slave Write Tests ====================

func TestModbusSlaveDriverWritePoint(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10510,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-5", config)
	_ = driver.WritePoint(context.Background(), "HR1", 42)
}

func TestModbusSlaveDriverDisconnect(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10511,
	}
	driver, _ := NewModbusSlaveDriver("slave-6", config)
	_ = driver.Disconnect()
}

func TestModbusSlaveDriverConnect(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10512,
	}
	driver, _ := NewModbusSlaveDriver("slave-7", config)
	_ = driver.Connect(context.Background())
	_ = driver.Disconnect()
}

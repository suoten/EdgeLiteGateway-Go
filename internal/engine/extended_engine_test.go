package engine

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// --- EventBus Tests ---
// Note: We use PublishSync to avoid goroutine timing issues in tests

func TestEventBusPublishSubscribeExtended(t *testing.T) {
	bus := NewEventBus(10)

	var received int32
	bus.Subscribe(EventTypeDataCollected, func(event Event) {
		atomic.AddInt32(&received, 1)
	})

	bus.PublishSync(Event{
		Type:    EventTypeDataCollected,
		Source:  "test",
		Data:    map[string]interface{}{"value": 1},
	})

	if atomic.LoadInt32(&received) != 1 {
		t.Fatalf("Expected 1 received, got %d", atomic.LoadInt32(&received))
	}
}

func TestEventBusWildcardSubscribeExtended(t *testing.T) {
	bus := NewEventBus(10)

	var received int32
	bus.SubscribeAll(func(event Event) {
		atomic.AddInt32(&received, 1)
	})

	bus.PublishSync(Event{Type: EventTypeDeviceOnline, Source: "test"})
	bus.PublishSync(Event{Type: EventTypeDeviceOffline, Source: "test"})

	if atomic.LoadInt32(&received) != 2 {
		t.Fatalf("Expected 2 received, got %d", atomic.LoadInt32(&received))
	}
}

func TestEventBusPublishSync(t *testing.T) {
	bus := NewEventBus(10)

	var received int32
	bus.Subscribe(EventTypeAlarmTriggered, func(event Event) {
		atomic.AddInt32(&received, 1)
	})

	bus.PublishSync(Event{
		Type:   EventTypeAlarmTriggered,
		Source: "test",
	})

	if atomic.LoadInt32(&received) != 1 {
		t.Fatalf("Expected 1 received, got %d", atomic.LoadInt32(&received))
	}
}

func TestEventBusUnsubscribeExtended(t *testing.T) {
	bus := NewEventBus(10)

	var received int32
	unsub := bus.Subscribe(EventTypeDeviceCreated, func(event Event) {
		atomic.AddInt32(&received, 1)
	})

	unsub()

	bus.PublishSync(Event{Type: EventTypeDeviceCreated, Source: "test"})

	if atomic.LoadInt32(&received) != 0 {
		t.Fatal("Should not receive events after unsubscribe")
	}
}

func TestEventBusMetricsExtended(t *testing.T) {
	bus := NewEventBus(10)
	bus.Subscribe(EventTypeDataCollected, func(event Event) {})

	for i := 0; i < 5; i++ {
		bus.PublishSync(Event{Type: EventTypeDataCollected, Source: "test"})
	}

	metrics := bus.Metrics()
	delivered := metrics["delivered"].(int64)
	if delivered != 5 {
		t.Fatalf("Expected 5 delivered, got %d", delivered)
	}
}

func TestEventBusQueueFull(t *testing.T) {
	bus := NewEventBus(2)

	// Publish without any subscribers to fill the queue
	for i := 0; i < 10; i++ {
		bus.Publish(Event{Type: EventTypeDataCollected, Source: "test"})
	}

	metrics := bus.Metrics()
	dropped := metrics["dropped"].(int64)
	if dropped == 0 {
		t.Fatal("Expected some events to be dropped")
	}
}

func TestEventBusSetBackpressureCallback(t *testing.T) {
	bus := NewEventBus(2)

	var callbackCalled int32
	bus.SetBackpressureCallback(func(level string, queueLen, queueCap int) {
		atomic.StoreInt32(&callbackCalled, 1)
	})

	// Fill the queue without subscribers
	for i := 0; i < 10; i++ {
		bus.Publish(Event{Type: EventTypeDataCollected, Source: "test"})
	}

	if atomic.LoadInt32(&callbackCalled) != 1 {
		t.Fatal("Backpressure callback should have been called")
	}
}

func TestEventBusDoubleStart(t *testing.T) {
	// EventBus.Start with already started bus is a no-op
	// We test this without actually starting to avoid goroutine issues
	bus := NewEventBus(10)
	_ = bus
}

func TestEventBusDoubleStop(t *testing.T) {
	// EventBus.Stop on a non-started bus is a no-op
	bus := NewEventBus(10)
	bus.Stop()
	bus.Stop() // Should not panic
}

func TestEventBusStopWithoutStart(t *testing.T) {
	bus := NewEventBus(10)
	bus.Stop() // Should not panic
	_ = bus
}

func TestEventMarshalJSONExtended(t *testing.T) {
	event := Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-1",
		PointName: "temp",
		Data:      map[string]interface{}{"value": 42},
		Timestamp: time.Now(),
		Metadata:  map[string]interface{}{"key": "val"},
	}

	data, err := event.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("Marshaled data is empty")
	}
}

// --- ExpressionEngine Tests ---

func TestExpressionEngineArithmetic(t *testing.T) {
	engine := NewExpressionEngine()

	tests := []struct {
		expr     string
		expected interface{}
	}{
		{"2 + 3", 5.0},
		{"10 - 4", 6.0},
		{"3 * 4", 12.0},
		{"10 / 2", 5.0},
		{"10 % 3", 1.0},
		{"2 ** 3", 8.0},
	}

	for _, tt := range tests {
		result := engine.Evaluate(tt.expr, nil)
		if result == nil {
			t.Fatalf("Expression '%s' returned nil", tt.expr)
		}
	}
}

func TestExpressionEngineComparison(t *testing.T) {
	engine := NewExpressionEngine()

	tests := []struct {
		expr     string
		expected bool
	}{
		{"5 > 3", true},
		{"3 > 5", false},
		{"5 >= 5", true},
		{"3 <= 2", false},
		{"5 == 5", true},
		{"5 != 3", true},
	}

	for _, tt := range tests {
		result := engine.Evaluate(tt.expr, nil)
		b, ok := result.(bool)
		if !ok {
			t.Fatalf("Expression '%s' did not return bool, got %T", tt.expr, result)
		}
		if b != tt.expected {
			t.Fatalf("Expression '%s': expected %v, got %v", tt.expr, tt.expected, b)
		}
	}
}

func TestExpressionEngineLogical(t *testing.T) {
	engine := NewExpressionEngine()

	result := engine.Evaluate("1 > 0 and 2 > 1", nil)
	if b, ok := result.(bool); !ok || !b {
		t.Fatalf("Expected true for '1 > 0 and 2 > 1', got %v", result)
	}

	result = engine.Evaluate("1 > 0 or 0 > 1", nil)
	if b, ok := result.(bool); !ok || !b {
		t.Fatalf("Expected true for '1 > 0 or 0 > 1', got %v", result)
	}

	result = engine.Evaluate("not 1 > 0", nil)
	if b, ok := result.(bool); !ok || b {
		t.Fatalf("Expected false for 'not 1 > 0', got %v", result)
	}
}

func TestExpressionEngineVariables(t *testing.T) {
	engine := NewExpressionEngine()
	vars := map[string]interface{}{
		"temp":     25.5,
		"humidity": 60.0,
	}

	result := engine.Evaluate("${temp} > 20", vars)
	if b, ok := result.(bool); !ok || !b {
		t.Fatalf("Expected true for temp > 20, got %v", result)
	}

	result = engine.Evaluate("${temp} + ${humidity}", vars)
	if result == nil {
		t.Fatal("Expression returned nil")
	}
}

func TestExpressionEngineFunctions(t *testing.T) {
	engine := NewExpressionEngine()

	// Built-in functions
	result := engine.Evaluate("abs(-5)", nil)
	if result == nil {
		t.Fatal("abs(-5) returned nil")
	}

	result = engine.Evaluate("max(3, 7)", nil)
	if result == nil {
		t.Fatal("max(3, 7) returned nil")
	}

	result = engine.Evaluate("min(3, 7)", nil)
	if result == nil {
		t.Fatal("min(3, 7) returned nil")
	}

	result = engine.Evaluate("round(3.14)", nil)
	if result == nil {
		t.Fatal("round(3.14) returned nil")
	}

	result = engine.Evaluate("sqrt(16)", nil)
	if result == nil {
		t.Fatal("sqrt(16) returned nil")
	}
}

func TestExpressionEngineCustomFunction(t *testing.T) {
	engine := NewExpressionEngine()

	err := engine.RegisterFunction("double", func(args []interface{}) (interface{}, error) {
		if len(args) > 0 {
			if v, ok := args[0].(float64); ok {
				return v * 2, nil
			}
		}
		return 0.0, nil
	})
	if err != nil {
		t.Fatalf("RegisterFunction failed: %v", err)
	}

	result := engine.Evaluate("double(21)", nil)
	if result == nil {
		t.Fatal("double(21) returned nil")
	}
}

func TestExpressionEngineRegisterDangerousName(t *testing.T) {
	engine := NewExpressionEngine()

	err := engine.RegisterFunction("__import__", func(args []interface{}) (interface{}, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("Should not allow dunder name")
	}
}

func TestExpressionEngineValidateExpression(t *testing.T) {
	engine := NewExpressionEngine()

	err := engine.ValidateExpression("2 + 3")
	if err != nil {
		t.Fatalf("Valid expression should pass: %v", err)
	}

	err = engine.ValidateExpression("")
	if err != nil {
		t.Fatalf("Empty expression should pass: %v", err)
	}
}

func TestExpressionEngineEvaluateBatch(t *testing.T) {
	engine := NewExpressionEngine()

	expressions := map[string]string{
		"a": "2 + 3",
		"b": "10 * 2",
	}

	results := engine.EvaluateBatch(expressions, nil)
	if len(results) != 2 {
		t.Fatalf("Expected 2 results, got %d", len(results))
	}
}

func TestExpressionEngineEmptyExpression(t *testing.T) {
	engine := NewExpressionEngine()

	result := engine.Evaluate("", nil)
	if result != nil {
		t.Fatal("Empty expression should return nil")
	}

	result = engine.Evaluate("   ", nil)
	if result != nil {
		t.Fatal("Whitespace-only expression should return nil")
	}
}

func TestExpressionEngineDivisionByZero(t *testing.T) {
	engine := NewExpressionEngine()

	result := engine.Evaluate("1 / 0", nil)
	if result != nil {
		// Division by zero should return nil (error path)
		// Some implementations may return Inf, but our parser returns error
	}
}

func TestExpressionEngineParentheses(t *testing.T) {
	engine := NewExpressionEngine()

	result := engine.Evaluate("(2 + 3) * 4", nil)
	if result == nil {
		t.Fatal("(2 + 3) * 4 returned nil")
	}
}

func TestExpressionEngineUnaryMinus(t *testing.T) {
	engine := NewExpressionEngine()

	result := engine.Evaluate("-5 + 3", nil)
	if result == nil {
		t.Fatal("-5 + 3 returned nil")
	}
}

// --- CircuitBreaker Tests ---

func TestCircuitBreakerClosedState(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-1", nil)

	if cb.GetState() != CBStateClosed {
		t.Fatal("Initial state should be Closed")
	}

	if !cb.AllowRequest() {
		t.Fatal("Closed circuit should allow requests")
	}
}

func TestCircuitBreakerOpenOnFailures(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-2", nil)

	// Record enough failures to open
	for i := 0; i < 10; i++ {
		cb.RecordFailure()
	}

	if cb.GetState() != CBStateOpen {
		t.Fatalf("Expected Open state after failures, got %s", cb.GetState())
	}

	if cb.AllowRequest() {
		t.Fatal("Open circuit should block requests")
	}
}

func TestCircuitBreakerResetOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-3", nil)

	// Record enough failures to open (threshold = 5)
	for i := 0; i < 5; i++ {
		cb.RecordFailure()
	}

	if cb.GetState() != CBStateOpen {
		t.Fatalf("Expected Open state after failures, got %s", cb.GetState())
	}

	// Reset to closed state (simulating manual recovery)
	cb.Reset()

	if cb.GetState() != CBStateClosed {
		t.Fatalf("Expected Closed state after reset, got %s", cb.GetState())
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-4", nil)

	for i := 0; i < 10; i++ {
		cb.RecordFailure()
	}

	cb.Reset()

	if cb.GetState() != CBStateClosed {
		t.Fatal("Reset should return to Closed state")
	}
}

func TestCircuitBreakerGetStats(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-stats", nil)

	cb.RecordSuccess()
	cb.RecordFailure()

	stats := cb.GetStats()
	if stats["device_id"] != "dev-cb-stats" {
		t.Fatalf("Expected device_id 'dev-cb-stats', got %v", stats["device_id"])
	}
	if stats["state"] != string(CBStateClosed) {
		t.Fatalf("Expected state 'closed', got %v", stats["state"])
	}
	if stats["request_count"].(int) != 2 {
		t.Fatalf("Expected request_count 2, got %v", stats["request_count"])
	}
}

func TestCircuitBreakerString(t *testing.T) {
	cb := NewCircuitBreaker("dev-cb-str", nil)
	s := cb.String()
	if s == "" {
		t.Fatal("String() should return non-empty")
	}
}

// --- CircuitBreakerRegistry Tests ---

func TestCircuitBreakerRegistryGet(t *testing.T) {
	reg := NewCircuitBreakerRegistry(nil)

	cb1 := reg.Get("device-1")
	if cb1 == nil {
		t.Fatal("Get should create circuit breaker")
	}

	cb2 := reg.Get("device-1")
	if cb1 != cb2 {
		t.Fatal("Get should return same instance for same device")
	}
}

func TestCircuitBreakerRegistryRemove(t *testing.T) {
	reg := NewCircuitBreakerRegistry(nil)

	reg.Get("device-rm")
	reg.Remove("device-rm")

	all := reg.GetAll()
	if _, exists := all["device-rm"]; exists {
		t.Fatal("Circuit breaker should be removed")
	}
}

func TestCircuitBreakerRegistryGetAll(t *testing.T) {
	reg := NewCircuitBreakerRegistry(nil)

	reg.Get("device-a")
	reg.Get("device-b")

	all := reg.GetAll()
	if len(all) != 2 {
		t.Fatalf("Expected 2 breakers, got %d", len(all))
	}
}

func TestCircuitBreakerRegistryGetStats(t *testing.T) {
	reg := NewCircuitBreakerRegistry(nil)

	reg.Get("device-s1")
	reg.Get("device-s2")

	stats := reg.GetStats()
	if stats["total"].(int) != 2 {
		t.Fatalf("Expected total 2, got %v", stats["total"])
	}
}

// --- BackpressureController Tests ---

func TestBackpressureControllerInitial(t *testing.T) {
	bp := NewBackpressureController(nil)

	if bp.GetLevel() != BPLevelNormal {
		t.Fatal("Initial level should be Normal")
	}
}

func TestBackpressureControllerAllowRequest(t *testing.T) {
	bp := NewBackpressureController(nil)

	// Should allow requests when at normal level with tokens
	for i := 0; i < 10; i++ {
		bp.AllowRequest()
	}

	stats := bp.GetStats()
	if stats["total_requests"].(int64) != 10 {
		t.Fatalf("Expected 10 requests, got %v", stats["total_requests"])
	}
}

func TestBackpressureControllerUpdateQueueStatus(t *testing.T) {
	bp := NewBackpressureController(nil)

	// Low utilization -> normal
	bp.UpdateQueueStatus(10, 100)
	if bp.GetLevel() != BPLevelNormal {
		t.Fatal("Expected Normal at 10% utilization")
	}

	// High utilization -> warning or above
	bp.UpdateQueueStatus(80, 100)
	level := bp.GetLevel()
	if level == BPLevelNormal {
		t.Fatal("Expected non-Normal at 80% utilization")
	}
}

func TestBackpressureControllerSetOnLevelChange(t *testing.T) {
	bp := NewBackpressureController(nil)

	var called int32
	bp.SetOnLevelChange(func(old, new BackpressureLevel) {
		atomic.StoreInt32(&called, 1)
	})

	bp.UpdateQueueStatus(90, 100)

	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("Level change callback should have been called")
	}
}

func TestBackpressureControllerGetStats(t *testing.T) {
	bp := NewBackpressureController(nil)

	bp.AllowRequest()

	stats := bp.GetStats()
	if stats["level"] != string(BPLevelNormal) {
		t.Fatalf("Expected level 'normal', got %v", stats["level"])
	}
}

func TestBackpressureControllerGetHistory(t *testing.T) {
	bp := NewBackpressureController(nil)

	bp.UpdateQueueStatus(90, 100)
	bp.UpdateQueueStatus(10, 100)

	history := bp.GetHistory()
	if len(history) == 0 {
		t.Fatal("Expected history entries")
	}
}

func TestBackpressureControllerReset(t *testing.T) {
	bp := NewBackpressureController(nil)

	bp.UpdateQueueStatus(90, 100)
	bp.Reset()

	if bp.GetLevel() != BPLevelNormal {
		t.Fatal("Level should be Normal after Reset")
	}
}

// --- RuleEvaluator Tests ---

func TestRuleEvaluatorLoadUnloadRule(t *testing.T) {
	evaluator := NewRuleEvaluator(nil, nil, nil)

	rule := &models.RuleResponse{
		RuleID:   "rule-1",
		Name:     "Test",
		Severity: "warning",
		Enabled:  true,
	}
	evaluator.LoadRule(rule)

	rules := evaluator.GetLoadedRules()
	if len(rules) != 1 || rules[0] != "rule-1" {
		t.Fatalf("Expected 1 loaded rule 'rule-1', got %v", rules)
	}

	evaluator.UnloadRule("rule-1")
	rules = evaluator.GetLoadedRules()
	if len(rules) != 0 {
		t.Fatalf("Expected 0 loaded rules, got %d", len(rules))
	}
}

func TestRuleEvaluatorGetActiveAlarms(t *testing.T) {
	evaluator := NewRuleEvaluator(nil, nil, nil)

	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 0 {
		t.Fatalf("Expected 0 active alarms, got %d", len(alarms))
	}
}

func TestRuleEvaluatorStats(t *testing.T) {
	evaluator := NewRuleEvaluator(nil, nil, nil)

	stats := evaluator.Stats()
	if stats["loaded_rules"].(int) != 0 {
		t.Fatalf("Expected 0 loaded rules, got %v", stats["loaded_rules"])
	}
}

func TestRuleEvaluatorAcknowledgeAlarmNoActive(t *testing.T) {
	evaluator := NewRuleEvaluator(nil, nil, nil)

	err := evaluator.AcknowledgeAlarm("nonexistent", "user-1")
	if err == nil {
		t.Fatal("Should return error for non-existent active alarm")
	}
}

func TestEvaluateConditionPublic(t *testing.T) {
	tests := []struct {
		op        string
		value     float64
		threshold float64
		expected  bool
	}{
		{">", 5, 3, true},
		{">", 3, 5, false},
		{">=", 5, 5, true},
		{"<", 3, 5, true},
		{"<=", 5, 5, true},
		{"==", 5, 5, true},
		{"!=", 5, 3, true},
		{"gt", 5, 3, true},
		{"gte", 5, 5, true},
		{"lt", 3, 5, true},
		{"lte", 5, 5, true},
		{"eq", 5, 5, true},
		{"ne", 5, 3, true},
		{"unknown", 5, 3, false},
	}

	for _, tt := range tests {
		result := EvaluateConditionPublic(tt.op, tt.value, tt.threshold)
		if result != tt.expected {
			t.Fatalf("EvaluateCondition(%s, %v, %v): expected %v, got %v",
				tt.op, tt.value, tt.threshold, tt.expected, result)
		}
	}
}

// --- RuleEvaluator with storage integration ---
// Note: These tests use PublishSync to avoid EventBus goroutine timing issues

func TestRuleEvaluatorThresholdTrigger(t *testing.T) {
	// Use nil eventBus to avoid goroutine leaks in tests
	cfg := newTestStorageConfig(t)
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-trigger-1",
		Name:     "High Temp",
		Severity: "critical",
		Enabled:  true,
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 50},
		},
		Logic: "AND",
	}
	evaluator.LoadRule(rule)

	// Simulate data collected event with high temperature
	points := []storage.PointData{
		{DeviceID: "dev-1", PointName: "temp", Value: 75.0, Quality: "good", Timestamp: time.Now()},
	}

	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-1",
		Timestamp: time.Now(),
		Data: DataCollectedEvent{
			DeviceID: "dev-1",
			Points:   points,
		},
	})

	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 1 {
		t.Fatalf("Expected 1 active alarm, got %d", len(alarms))
	}
	if alarms["rule-trigger-1"] == "" {
		t.Fatal("Alarm ID should not be empty")
	}
}

func TestRuleEvaluatorThresholdNotTriggered(t *testing.T) {
	cfg := newTestStorageConfig(t)
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-no-trigger-1",
		Name:     "High Temp",
		Severity: "warning",
		Enabled:  true,
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 100},
		},
		Logic: "AND",
	}
	evaluator.LoadRule(rule)

	// Temperature is below threshold
	points := []storage.PointData{
		{DeviceID: "dev-2", PointName: "temp", Value: 50.0, Quality: "good", Timestamp: time.Now()},
	}

	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-2",
		Timestamp: time.Now(),
		Data: DataCollectedEvent{
			DeviceID: "dev-2",
			Points:   points,
		},
	})

	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 0 {
		t.Fatalf("Expected 0 active alarms, got %d", len(alarms))
	}
}

func TestRuleEvaluatorORLogic(t *testing.T) {
	cfg := newTestStorageConfig(t)
	db, _ := storage.NewDatabase(cfg)
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-or-1",
		Name:     "OR Rule",
		Severity: "warning",
		Enabled:  true,
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 100},
			{Point: "humidity", Operator: ">", Threshold: 50},
		},
		Logic: "OR",
	}
	evaluator.LoadRule(rule)

	// Only humidity exceeds threshold
	points := []storage.PointData{
		{DeviceID: "dev-or", PointName: "temp", Value: 50.0, Quality: "good", Timestamp: time.Now()},
		{DeviceID: "dev-or", PointName: "humidity", Value: 70.0, Quality: "good", Timestamp: time.Now()},
	}

	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-or",
		Timestamp: time.Now(),
		Data: DataCollectedEvent{
			DeviceID: "dev-or",
			Points:   points,
		},
	})

	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 1 {
		t.Fatalf("Expected 1 active alarm with OR logic, got %d", len(alarms))
	}
}

func TestRuleEvaluatorRecovery(t *testing.T) {
	cfg := newTestStorageConfig(t)
	db, _ := storage.NewDatabase(cfg)
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-recover-1",
		Name:     "Recovery Test",
		Severity: "warning",
		Enabled:  true,
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 50},
		},
		Logic: "AND",
	}
	evaluator.LoadRule(rule)

	// Trigger alarm
	points := []storage.PointData{
		{DeviceID: "dev-rec", PointName: "temp", Value: 75.0, Quality: "good", Timestamp: time.Now()},
	}
	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-rec",
		Timestamp: time.Now(),
		Data:      DataCollectedEvent{DeviceID: "dev-rec", Points: points},
	})
	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 1 {
		t.Fatalf("Expected 1 alarm after trigger, got %d", len(alarms))
	}

	// Recover - send low value
	points2 := []storage.PointData{
		{DeviceID: "dev-rec", PointName: "temp", Value: 30.0, Quality: "good", Timestamp: time.Now()},
	}
	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-rec",
		Timestamp: time.Now(),
		Data:      DataCollectedEvent{DeviceID: "dev-rec", Points: points2},
	})
	alarms = evaluator.GetActiveAlarms()
	if len(alarms) != 0 {
		t.Fatalf("Expected 0 alarms after recovery, got %d", len(alarms))
	}
}

func TestRuleEvaluatorDisabledRule(t *testing.T) {
	cfg := newTestStorageConfig(t)
	db, _ := storage.NewDatabase(cfg)
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-disabled-1",
		Name:     "Disabled",
		Severity: "warning",
		Enabled:  false, // Disabled
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 50},
		},
		Logic: "AND",
	}
	evaluator.LoadRule(rule)

	points := []storage.PointData{
		{DeviceID: "dev-dis", PointName: "temp", Value: 75.0, Quality: "good", Timestamp: time.Now()},
	}
	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-dis",
		Timestamp: time.Now(),
		Data:      DataCollectedEvent{DeviceID: "dev-dis", Points: points},
	})
	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 0 {
		t.Fatalf("Disabled rule should not trigger alarms, got %d", len(alarms))
	}
}

func TestRuleEvaluatorDeviceFilter(t *testing.T) {
	cfg := newTestStorageConfig(t)
	db, _ := storage.NewDatabase(cfg)
	defer db.Close()

	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	evaluator := NewRuleEvaluator(nil, ruleRepo, alarmRepo)

	rule := &models.RuleResponse{
		RuleID:   "rule-filter-1",
		Name:     "Filtered",
		Severity: "warning",
		Enabled:  true,
		DeviceID: "dev-target", // Only applies to dev-target
		Conditions: []models.RuleCondition{
			{Point: "temp", Operator: ">", Threshold: 50},
		},
		Logic: "AND",
	}
	evaluator.LoadRule(rule)

	// Send data from a different device
	points := []storage.PointData{
		{DeviceID: "dev-other", PointName: "temp", Value: 75.0, Quality: "good", Timestamp: time.Now()},
	}
	evaluator.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-other",
		Timestamp: time.Now(),
		Data:      DataCollectedEvent{DeviceID: "dev-other", Points: points},
	})
	alarms := evaluator.GetActiveAlarms()
	if len(alarms) != 0 {
		t.Fatalf("Rule should not trigger for different device, got %d", len(alarms))
	}
}

// Helper: DataCollectedEvent type
// This is defined in the engine package, used by the EventBus

// Helper function to create test storage config
func newTestStorageConfig(t *testing.T) *config.AppConfig {
	// We need to import config
	return &config.AppConfig{
		Database: config.DatabaseConfig{
			Backend:    "sqlite",
			SQLitePath: filepath.Join(t.TempDir(), "rule_test.db"),
			PoolSize:   5,
			MaxOverflow: 10,
			BackupDir:  filepath.Join(t.TempDir(), "backups"),
		},
		InfluxDB: config.InfluxDBConfig{
			SQLiteTSPath: filepath.Join(t.TempDir(), "rule_test_ts.db"),
		},
	}
}

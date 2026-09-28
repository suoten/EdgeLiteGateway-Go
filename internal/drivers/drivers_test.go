package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"edgelite/internal/models"
)

func TestSimulatorDriverCreate(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 502,
	}
	driver, err := NewSimulatorDriver("test-device-1", config)
	if err != nil {
		t.Fatalf("Failed to create simulator driver: %v", err)
	}
	if driver.Name() != "simulator" {
		t.Errorf("Expected name 'simulator', got '%s'", driver.Name())
	}
	if !driver.IsConnected() {
		t.Error("Simulator should be connected by default")
	}
}

func TestSimulatorDriverConnectDisconnect(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-2", nil)

	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if !driver.IsConnected() {
		t.Error("Should be connected after Connect()")
	}

	if err := driver.Disconnect(); err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}
	if driver.IsConnected() {
		t.Error("Should be disconnected after Disconnect()")
	}

	// Reconnect
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("Reconnect failed: %v", err)
	}
	if !driver.IsConnected() {
		t.Error("Should be connected after reconnect")
	}
}

func TestSimulatorDriverReadPoints(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-3", nil)
	driver.Connect(context.Background())

	minVal := 0.0
	maxVal := 100.0
	scale := 1.0
	offset := 0.0

	points := []models.PointDef{
		{Name: "temperature", Mode: "sine", Min: &minVal, Max: &maxVal, Scale: &scale, Offset: &offset},
		{Name: "pressure", Mode: "random", Min: &minVal, Max: &maxVal},
		{Name: "flow", Mode: "ramp", Min: &minVal, Max: &maxVal},
		{Name: "constant_val", Mode: "constant", Min: &minVal, Max: &maxVal},
		{Name: "step_val", Mode: "step", Min: &minVal, Max: &maxVal},
		{Name: "default_mode", Min: &minVal, Max: &maxVal}, // Empty mode → default sine
	}

	data, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("ReadPoints failed: %v", err)
	}
	if len(data) != len(points) {
		t.Fatalf("Expected %d points, got %d", len(points), len(data))
	}

	for _, pd := range data {
		if pd.DeviceID != "test-device-3" {
			t.Errorf("Expected device_id 'test-device-3', got '%s'", pd.DeviceID)
		}
		if pd.Quality != "good" {
			t.Errorf("Expected quality 'good', got '%s'", pd.Quality)
		}
		if pd.Timestamp.IsZero() {
			t.Error("Timestamp should not be zero")
		}
	}

	// Read again to test phase advancement
	data2, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("Second ReadPoints failed: %v", err)
	}
	if len(data2) != len(points) {
		t.Fatalf("Expected %d points on second read, got %d", len(points), len(data2))
	}
}

func TestSimulatorDriverReadPointsDisconnected(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-4", nil)
	driver.Disconnect()

	points := []models.PointDef{{Name: "test_point"}}
	_, err := driver.ReadPoints(context.Background(), points)
	if err == nil {
		t.Error("Expected error when reading from disconnected simulator")
	}
}

func TestSimulatorDriverWritePoint(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-5", nil)

	// Write float64 value
	if err := driver.WritePoint(context.Background(), "setpoint", 42.5); err != nil {
		t.Fatalf("WritePoint failed: %v", err)
	}

	// A value the simulator cannot hold must be rejected, not acknowledged.
	if err := driver.WritePoint(context.Background(), "setpoint", "not-a-number"); err == nil {
		t.Fatal("WritePoint with a non-numeric value should error")
	}

	// The write has to be observable: without the hold window the next poll
	// regenerated the waveform and a successful write could never be read back.
	sd := driver.(*SimulatorDriver)
	sd.mu.Lock()
	held, ok := sd.held["setpoint"]
	sd.mu.Unlock()
	if !ok || held.value != 42.5 {
		t.Fatalf("written value not held: %+v (present=%v)", held, ok)
	}
	got, err := driver.ReadPoints(context.Background(), []models.PointDef{{Name: "setpoint"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Value != 42.5 {
		t.Fatalf("read-back of the written point = %+v, want 42.5", got)
	}
}

func TestSimulatorDriverDiscover(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-6", nil)

	// A simulator has no network to scan. Returning an empty list made the UI show
	// "0 devices found", which reads as "nothing on this segment" rather than the
	// truth: this protocol cannot answer the question at all.
	results, err := driver.Discover(context.Background(), nil)
	if !errors.Is(err, ErrDiscoveryUnsupported) {
		t.Fatalf("Discover error = %v, want ErrDiscoveryUnsupported", err)
	}
	if len(results) != 0 {
		t.Errorf("an unsupported Discover must return no entries, got %d", len(results))
	}
}

func TestSimulatorDriverHealthCheck(t *testing.T) {
	driver, _ := NewSimulatorDriver("test-device-7", nil)

	err := driver.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("HealthCheck should always pass for simulator, got: %v", err)
	}
}

// --- Registry Tests ---

func TestRegistryRegisterAndCreate(t *testing.T) {
	r := &Registry{
		factories: make(map[string]DriverFactory),
	}

	r.Register("test_protocol", func(deviceID string, config map[string]interface{}) (Driver, error) {
		return NewSimulatorDriver(deviceID, config)
	})

	if !r.IsSupported("test_protocol") {
		t.Error("test_protocol should be supported")
	}
	if r.IsSupported("nonexistent") {
		t.Error("nonexistent should not be supported")
	}

	protocols := r.SupportedProtocols()
	found := false
	for _, p := range protocols {
		if p == "test_protocol" {
			found = true
			break
		}
	}
	if !found {
		t.Error("test_protocol should be in supported protocols list")
	}

	driver, err := r.CreateDriver("test_protocol", "dev-1", nil)
	if err != nil {
		t.Fatalf("CreateDriver failed: %v", err)
	}
	if driver == nil {
		t.Fatal("Driver should not be nil")
	}
	if driver.Name() != "simulator" {
		t.Errorf("Expected driver name 'simulator', got '%s'", driver.Name())
	}
}

func TestRegistryCreateUnsupportedProtocol(t *testing.T) {
	r := &Registry{
		factories: make(map[string]DriverFactory),
	}

	_, err := r.CreateDriver("nonexistent", "dev-1", nil)
	if err == nil {
		t.Error("Expected error for unsupported protocol")
	}
}

func TestRegistrySetInfrastructure(t *testing.T) {
	r := &Registry{
		factories: make(map[string]DriverFactory),
	}
	hsm := NewHealthStatsManager()
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	rm := NewReconnectManager()

	r.SetInfrastructure(hsm, cb, rm)

	r.Register("test_proto", func(deviceID string, config map[string]interface{}) (Driver, error) {
		return NewSimulatorDriver(deviceID, config)
	})

	driver, _ := r.CreateDriver("test_proto", "dev-infra", nil)
	simDriver, ok := driver.(*SimulatorDriver)
	if !ok {
		t.Fatalf("Expected *SimulatorDriver, got %T", driver)
	}
	if simDriver.GetHealthStatsManager() == nil {
		t.Error("HealthStatsManager should be injected")
	}
	if simDriver.GetCircuitBreaker() == nil {
		t.Error("CircuitBreaker should be injected")
	}
}

// --- Circuit Breaker Tests ---

func TestCircuitBreakerClosedState(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())

	if !cb.AllowRequest("device-1") {
		t.Error("Should allow request in closed state")
	}
	if cb.GetState("device-1") != CircuitClosed {
		t.Errorf("Expected closed state, got %s", cb.GetState("device-1"))
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:  3,
		RecoveryTimeout:   100 * time.Millisecond,
		HalfOpenMaxCalls:  2,
	}
	cb := NewCircuitBreaker(config)

	// Record failures to trigger open state
	cb.RecordFailure("device-1", 1)
	cb.RecordFailure("device-1", 2)
	if cb.GetState("device-1") != CircuitClosed {
		t.Errorf("Expected closed after 2 failures, got %s", cb.GetState("device-1"))
	}

	cb.RecordFailure("device-1", 3)
	if cb.GetState("device-1") != CircuitOpen {
		t.Errorf("Expected open after 3 failures, got %s", cb.GetState("device-1"))
	}

	// Should not allow requests when open
	if cb.AllowRequest("device-1") {
		t.Error("Should not allow request when circuit is open")
	}
}

func TestCircuitBreakerHalfOpenRecovery(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:  2,
		RecoveryTimeout:   50 * time.Millisecond,
		HalfOpenMaxCalls:  2,
	}
	cb := NewCircuitBreaker(config)

	// Open the circuit
	cb.RecordFailure("device-1", 1)
	cb.RecordFailure("device-1", 2)
	if cb.GetState("device-1") != CircuitOpen {
		t.Fatalf("Expected open state, got %s", cb.GetState("device-1"))
	}

	// Wait for recovery timeout
	time.Sleep(60 * time.Millisecond)

	// Should transition to half-open and allow request
	if !cb.AllowRequest("device-1") {
		t.Error("Should allow request after recovery timeout (half-open)")
	}
	if cb.GetState("device-1") != CircuitHalfOpen {
		t.Errorf("Expected half-open state, got %s", cb.GetState("device-1"))
	}

	// Record success should close the circuit
	cb.RecordSuccess("device-1")
	if cb.GetState("device-1") != CircuitClosed {
		t.Errorf("Expected closed after success in half-open, got %s", cb.GetState("device-1"))
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())

	cb.RecordFailure("device-1", 10)
	cb.Reset("device-1")

	if cb.GetState("device-1") != CircuitClosed {
		t.Errorf("Expected closed after reset, got %s", cb.GetState("device-1"))
	}
}

// --- Health Stats Tests ---

func TestHealthStatsRecordReads(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordReadSuccess("device-1", 5.0)
	hsm.RecordReadSuccess("device-1", 10.0)
	hsm.RecordReadFailure("device-1")

	stats := hsm.GetHealthStats("device-1")
	if stats == nil {
		t.Fatal("Expected health stats for device-1")
	}
	if stats.TotalReads != 3 {
		t.Errorf("Expected 3 total reads, got %d", stats.TotalReads)
	}
	if stats.FailedReads != 1 {
		t.Errorf("Expected 1 failed read, got %d", stats.FailedReads)
	}
}

func TestHealthStatsRecordWrites(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordWriteSuccess("device-1")
	hsm.RecordWriteSuccess("device-1")
	hsm.RecordWriteFailure("device-1")

	stats := hsm.GetHealthStats("device-1")
	if stats == nil {
		t.Fatal("Expected health stats for device-1")
	}
	if stats.TotalWrites != 3 {
		t.Errorf("Expected 3 total writes, got %d", stats.TotalWrites)
	}
	if stats.FailedWrites != 1 {
		t.Errorf("Expected 1 failed write, got %d", stats.FailedWrites)
	}
}

func TestHealthStatsErrorRates(t *testing.T) {
	hs := NewDriverHealthStats("device-1")

	// Test empty error rates
	if hs.ReadErrorRate() != 0.0 {
		t.Errorf("Expected 0 read error rate with no reads, got %f", hs.ReadErrorRate())
	}
	if hs.WriteErrorRate() != 0.0 {
		t.Errorf("Expected 0 write error rate with no writes, got %f", hs.WriteErrorRate())
	}

	// Add some reads
	hs.TotalReads = 10
	hs.FailedReads = 2
	if rate := hs.ReadErrorRate(); rate != 0.2 {
		t.Errorf("Expected 0.2 read error rate, got %f", rate)
	}

	// Add some writes
	hs.TotalWrites = 5
	hs.FailedWrites = 1
	if rate := hs.WriteErrorRate(); rate != 0.2 {
		t.Errorf("Expected 0.2 write error rate, got %f", rate)
	}
}

func TestHealthStatsLatencyTracking(t *testing.T) {
	hs := NewDriverHealthStats("device-1")

	hs.RecordLatency(10.0)
	hs.RecordLatency(20.0)
	hs.RecordLatency(30.0)

	if hs.AvgLatencyMs < 10.0 || hs.AvgLatencyMs > 30.0 {
		t.Errorf("Expected avg latency between 10-30ms, got %f", hs.AvgLatencyMs)
	}

	// P95 should be within the recorded range
	p95 := hs.P95LatencyMs()
	if p95 < 10.0 || p95 > 30.0 {
		t.Errorf("Expected P95 latency between 10-30ms, got %f", p95)
	}
}

func TestHealthStatsP95Empty(t *testing.T) {
	hs := NewDriverHealthStats("device-1")
	if hs.P95LatencyMs() != 0.0 {
		t.Errorf("Expected 0 P95 with no samples, got %f", hs.P95LatencyMs())
	}
}

func TestHealthStatsConnectionState(t *testing.T) {
	hsm := NewHealthStatsManager()

	// Initial state should be disconnected
	ok := hsm.SetConnectionState("device-1", StateConnecting, "starting connection")
	if !ok {
		t.Error("Expected first state transition to succeed")
	}

	status := hsm.GetConnectionStatus("device-1")
	if status == nil {
		t.Fatal("Expected connection status after SetConnectionState")
	}
	if status.State != StateConnecting {
		t.Errorf("Expected state 'connecting', got '%s'", status.State)
	}

	// Transition to connected
	ok = hsm.SetConnectionState("device-1", StateConnected, "connection established")
	if !ok {
		t.Error("Expected transition from connecting to connected")
	}
	status = hsm.GetConnectionStatus("device-1")
	if status.State != StateConnected {
		t.Errorf("Expected state 'connected', got '%s'", status.State)
	}

	// Transition to disconnected
	ok = hsm.SetConnectionState("device-1", StateDisconnected, "connection lost")
	if !ok {
		t.Error("Expected transition from connected to disconnected")
	}
	status = hsm.GetConnectionStatus("device-1")
	if status.State != StateDisconnected {
		t.Errorf("Expected state 'disconnected', got '%s'", status.State)
	}
}

func TestHealthStatsIsHealthy(t *testing.T) {
	hs := NewDriverHealthStats("device-1")

	// New device with no reads should be healthy
	if !hs.IsHealthy() {
		t.Error("New device should be healthy")
	}

	// Degrade with consecutive failures
	hs.ConsecutiveFailures = 5
	if hs.IsHealthy() {
		t.Error("Device with 5 consecutive failures should not be healthy")
	}

	// Reset failures
	hs.ConsecutiveFailures = 0
	hs.TotalReads = 10
	hs.FailedReads = 0 // 0% error rate, healthy
	if !hs.IsHealthy() {
		t.Error("Device with 0% error rate should be healthy")
	}

	// High error rate
	hs.FailedReads = 5 // 50% error rate
	if hs.IsHealthy() {
		t.Error("Device with 50% error rate should not be healthy")
	}
}

// --- Config Helper Tests ---

func TestGetConfigString(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": float64(502),
	}

	if v := GetConfigString(config, "host", "default"); v != "127.0.0.1" {
		t.Errorf("Expected '127.0.0.1', got '%s'", v)
	}
	if v := GetConfigString(config, "port", "0"); v != "502" {
		t.Errorf("Expected '502', got '%s'", v)
	}
	if v := GetConfigString(config, "missing", "default"); v != "default" {
		t.Errorf("Expected 'default', got '%s'", v)
	}
}

func TestGetConfigInt(t *testing.T) {
	config := map[string]interface{}{
		"port":     float64(502),
		"timeout":  30,
		"int64val": int64(100),
	}

	if v := GetConfigInt(config, "port", 0); v != 502 {
		t.Errorf("Expected 502, got %d", v)
	}
	if v := GetConfigInt(config, "timeout", 0); v != 30 {
		t.Errorf("Expected 30, got %d", v)
	}
	if v := GetConfigInt(config, "int64val", 0); v != 100 {
		t.Errorf("Expected 100, got %d", v)
	}
	if v := GetConfigInt(config, "missing", 99); v != 99 {
		t.Errorf("Expected 99, got %d", v)
	}
}

func TestGetConfigFloat(t *testing.T) {
	config := map[string]interface{}{
		"ratio":   0.5,
		"count":   10,
		"int64ct": int64(20),
	}

	if v := GetConfigFloat(config, "ratio", 0.0); v != 0.5 {
		t.Errorf("Expected 0.5, got %f", v)
	}
	if v := GetConfigFloat(config, "count", 0.0); v != 10.0 {
		t.Errorf("Expected 10.0, got %f", v)
	}
	if v := GetConfigFloat(config, "int64ct", 0.0); v != 20.0 {
		t.Errorf("Expected 20.0, got %f", v)
	}
	if v := GetConfigFloat(config, "missing", 1.0); v != 1.0 {
		t.Errorf("Expected 1.0, got %f", v)
	}
}

// --- Display Name Tests ---

func TestGetDriverDisplayName(t *testing.T) {
	if name := GetDriverDisplayName("modbus_tcp", "zh"); name != "Modbus TCP" {
		t.Errorf("Expected 'Modbus TCP', got '%s'", name)
	}
	if name := GetDriverDisplayName("simulator", "zh"); name != "模拟器" {
		t.Errorf("Expected '模拟器', got '%s'", name)
	}
	if name := GetDriverDisplayName("simulator", "en"); name != "Simulator" {
		t.Errorf("Expected 'Simulator', got '%s'", name)
	}
	// Unknown protocol should return the protocol name itself
	if name := GetDriverDisplayName("unknown_proto", "en"); name != "unknown_proto" {
		t.Errorf("Expected 'unknown_proto', got '%s'", name)
	}
	// Unknown language should fallback to English
	if name := GetDriverDisplayName("modbus_tcp", "fr"); name != "Modbus TCP" {
		t.Errorf("Expected 'Modbus TCP' fallback, got '%s'", name)
	}
}

// --- Reconnect Manager Tests ---

func TestReconnectManager(t *testing.T) {
	rm := NewReconnectManager()

	// Initially no attempts
	if attempts := rm.GetReconnectAttempts("device-1"); attempts != 0 {
		t.Errorf("Expected 0 attempts initially, got %d", attempts)
	}

	// Simulate reconnect with success
	success, err := rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("ReconnectWithBackoff failed: %v", err)
	}
	if !success {
		t.Error("Expected reconnect to succeed")
	}

	// After success, attempts should be reset to 0
	if attempts := rm.GetReconnectAttempts("device-1"); attempts != 0 {
		t.Errorf("Expected 0 attempts after success, got %d", attempts)
	}

	// Reset state
	rm.ResetReconnectState("device-1")
	if attempts := rm.GetReconnectAttempts("device-1"); attempts != 0 {
		t.Errorf("Expected 0 attempts after reset, got %d", attempts)
	}
}

func TestReconnectManagerMaxAttempts(t *testing.T) {
	rm := NewReconnectManager()
	rm.maxAttempts = 2
	rm.baseDelay = 1 * time.Millisecond // Speed up test

	// First attempt (will fail)
	success, _ := rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
		return false, nil
	})
	if success {
		t.Error("First reconnect should fail")
	}

	// Second attempt (will fail)
	success, _ = rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
		return false, nil
	})
	if success {
		t.Error("Second reconnect should fail")
	}

	// Third attempt should be blocked by max attempts
	success, _ = rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
		return true, nil
	})
	if success {
		t.Error("Should not succeed after max attempts")
	}
}

// --- LRU Cache Tests ---

func TestLRUCache(t *testing.T) {
	cache := NewLRUCache(3)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	if v, ok := cache.Get("a"); !ok || v.(int) != 1 {
		t.Errorf("Expected a=1, got %v (%v)", v, ok)
	}

	// Add "d" → should evict "b" (least recently used, since "a" was just accessed)
	cache.Set("d", 4)
	if _, ok := cache.Get("b"); ok {
		t.Error("Expected 'b' to be evicted")
	}
	if _, ok := cache.Get("d"); !ok {
		t.Error("Expected 'd' to exist")
	}
}

func TestLRUCacheCapacity(t *testing.T) {
	cache := NewLRUCache(2)
	cache.Set("x", 1)
	cache.Set("y", 2)

	if cache.Len() != 2 {
		t.Errorf("Expected len=2, got %d", cache.Len())
	}

	cache.Set("z", 3) // Evicts "x"
	if cache.Len() != 2 {
		t.Errorf("Expected len=2 after eviction, got %d", cache.Len())
	}
	if _, ok := cache.Get("x"); ok {
		t.Error("Expected 'x' to be evicted")
	}
}

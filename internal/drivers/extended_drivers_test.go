package drivers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// --- DeadbandFilter Tests ---

func TestDeadbandFilterAbsolute(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "absolute", DeadbandThreshold: 5.0}

	// Change > threshold → pass new value
	result := filter.Apply(110.0, 100.0)
	if result != 110.0 {
		t.Errorf("Expected 110.0, got %v", result)
	}

	// Change < threshold → keep last value
	result = filter.Apply(103.0, 100.0)
	if result != 100.0 {
		t.Errorf("Expected 100.0 (last), got %v", result)
	}
}

func TestDeadbandFilterPercent(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "percent", DeadbandThreshold: 10.0}

	// 10% of 100 = 10 threshold
	result := filter.Apply(115.0, 100.0)
	if result != 115.0 {
		t.Errorf("Expected 115.0, got %v", result)
	}

	// Change < 10% → keep last
	result = filter.Apply(105.0, 100.0)
	if result != 100.0 {
		t.Errorf("Expected 100.0 (last), got %v", result)
	}
}

func TestDeadbandFilterNilValues(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "absolute", DeadbandThreshold: 5.0}

	// nil lastValue → pass new value
	result := filter.Apply(50.0, nil)
	if result != 50.0 {
		t.Errorf("Expected 50.0 with nil last, got %v", result)
	}

	// nil newValue → pass nil
	result = filter.Apply(nil, 50.0)
	if result != nil {
		t.Errorf("Expected nil with nil new, got %v", result)
	}
}

func TestDeadbandFilterNonFloatValues(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "absolute", DeadbandThreshold: 5.0}

	// Non-float values should pass through
	result := filter.Apply("hello", "world")
	if result != "hello" {
		t.Errorf("Expected 'hello', got %v", result)
	}
}

func TestDeadbandFilterPercentZeroLast(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "percent", DeadbandThreshold: 10.0}

	// lastFloat == 0 → pass new value
	result := filter.Apply(50.0, 0.0)
	if result != 50.0 {
		t.Errorf("Expected 50.0 with zero last, got %v", result)
	}
}

func TestDeadbandFilterZeroThresholdPercent(t *testing.T) {
	filter := &DeadbandFilter{DeadbandType: "percent", DeadbandThreshold: 0}

	// Zero threshold → pass new value
	result := filter.Apply(50.0, 100.0)
	if result != 50.0 {
		t.Errorf("Expected 50.0 with zero threshold, got %v", result)
	}
}

// --- ScalingConfig Tests ---

func TestScalingConfigApply(t *testing.T) {
	sc := &ScalingConfig{Ratio: 2.0, Offset: 10.0}
	result := sc.Apply(5.0)
	if result != 20.0 {
		t.Errorf("Expected 20.0 (5*2+10), got %v", result)
	}
}

func TestScalingConfigApplyNil(t *testing.T) {
	sc := &ScalingConfig{Ratio: 2.0, Offset: 10.0}
	result := sc.Apply(nil)
	if result != nil {
		t.Errorf("Expected nil, got %v", result)
	}
}

func TestScalingConfigApplyNonFloat(t *testing.T) {
	sc := &ScalingConfig{Ratio: 2.0, Offset: 10.0}
	result := sc.Apply("hello")
	if result != "hello" {
		t.Errorf("Expected 'hello', got %v", result)
	}
}

// --- ClampConfig Tests ---

func TestClampConfigWithinRange(t *testing.T) {
	min := 0.0
	max := 100.0
	cc := &ClampConfig{Min: &min, Max: &max}

	result, inRange := cc.Apply(50.0)
	if result != 50.0 || !inRange {
		t.Errorf("Expected 50.0, inRange=true, got %v, %v", result, inRange)
	}
}

func TestClampConfigBelowMin(t *testing.T) {
	min := 10.0
	max := 100.0
	cc := &ClampConfig{Min: &min, Max: &max}

	result, inRange := cc.Apply(5.0)
	if result != 10.0 || inRange {
		t.Errorf("Expected 10.0 (clamped), inRange=false, got %v, %v", result, inRange)
	}
}

func TestClampConfigAboveMax(t *testing.T) {
	min := 0.0
	max := 100.0
	cc := &ClampConfig{Min: &min, Max: &max}

	result, inRange := cc.Apply(150.0)
	if result != 100.0 || inRange {
		t.Errorf("Expected 100.0 (clamped), inRange=false, got %v, %v", result, inRange)
	}
}

func TestClampConfigNilValue(t *testing.T) {
	cc := &ClampConfig{}
	result, inRange := cc.Apply(nil)
	if result != nil || !inRange {
		t.Errorf("Expected nil, inRange=true, got %v, %v", result, inRange)
	}
}

func TestClampConfigNaN(t *testing.T) {
	cc := &ClampConfig{}
	nan := math.NaN()
	result, inRange := cc.Apply(nan)
	if inRange {
		t.Errorf("Expected inRange=false for NaN, got %v, %v", result, inRange)
	}
}

func TestClampConfigNonFloat(t *testing.T) {
	cc := &ClampConfig{}
	result, inRange := cc.Apply("hello")
	if result != "hello" || !inRange {
		t.Errorf("Expected 'hello', inRange=true, got %v, %v", result, inRange)
	}
}

// --- WritePolicy Tests ---

func TestWritePolicyDisabled(t *testing.T) {
	wp := WritePolicy{WriteEnabled: false}
	if wp.CheckWriteAllowed("point1", true) {
		t.Error("Should not allow write when disabled")
	}
}

func TestWritePolicyNoCanWrite(t *testing.T) {
	wp := WritePolicy{WriteEnabled: true}
	if wp.CheckWriteAllowed("point1", false) {
		t.Error("Should not allow write when canWrite=false")
	}
}

func TestWritePolicyOpenMode(t *testing.T) {
	wp := WritePolicy{WriteEnabled: true, WhitelistMode: false}
	if !wp.CheckWriteAllowed("any_point", true) {
		t.Error("Should allow write in open mode")
	}
}

func TestWritePolicyWhitelistAllowed(t *testing.T) {
	wp := WritePolicy{
		WriteEnabled:      true,
		WhitelistMode:     true,
		WhitelistedPoints: []string{"point1", "point2"},
	}
	if !wp.CheckWriteAllowed("point1", true) {
		t.Error("Should allow write for whitelisted point")
	}
}

func TestWritePolicyWhitelistBlocked(t *testing.T) {
	wp := WritePolicy{
		WriteEnabled:      true,
		WhitelistMode:     true,
		WhitelistedPoints: []string{"point1"},
	}
	if wp.CheckWriteAllowed("point3", true) {
		t.Error("Should not allow write for non-whitelisted point")
	}
}

func TestDefaultWritePolicy(t *testing.T) {
	wp := DefaultWritePolicy()
	if !wp.WriteEnabled {
		t.Error("Default policy should have WriteEnabled=true")
	}
	if wp.WhitelistMode {
		t.Error("Default policy should not use whitelist mode")
	}
}

// --- RateLimiter Tests ---

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(100) // 100ms interval

	// First call should be allowed
	if !rl.Allow("key1") {
		t.Error("First call should be allowed")
	}

	// Immediate second call should be blocked
	if rl.Allow("key1") {
		t.Error("Immediate second call should be blocked")
	}
}

func TestRateLimiterDifferentKeys(t *testing.T) {
	rl := NewRateLimiter(100)

	// Different keys should be independent
	if !rl.Allow("key1") {
		t.Error("key1 first call should be allowed")
	}
	if !rl.Allow("key2") {
		t.Error("key2 first call should be allowed")
	}
}

func TestRateLimiterAfterInterval(t *testing.T) {
	rl := NewRateLimiter(10) // 10ms interval

	rl.Allow("key1")
	time.Sleep(15 * time.Millisecond)

	if !rl.Allow("key1") {
		t.Error("Should allow after interval passed")
	}
}

// --- ConfigValidator Tests ---

func TestConfigValidatorValid(t *testing.T) {
	cv := &ConfigValidator{RequiredFields: []string{"host", "port"}}
	config := map[string]interface{}{
		"host": "127.0.0.1",
		"port": 502,
	}

	result := cv.Validate(config)
	if !result.Valid {
		t.Errorf("Expected valid config, got errors: %v", result.Errors)
	}
}

func TestConfigValidatorMissingFields(t *testing.T) {
	cv := &ConfigValidator{RequiredFields: []string{"host", "port", "timeout"}}
	config := map[string]interface{}{
		"host": "127.0.0.1",
	}

	result := cv.Validate(config)
	if result.Valid {
		t.Error("Expected invalid config with missing fields")
	}
	if len(result.Errors) != 2 {
		t.Errorf("Expected 2 errors, got %d", len(result.Errors))
	}
}

// --- DriverExceptionMapper Tests ---

func TestExceptionMapperConnectionRefused(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("connection refused"), "modbus_tcp")
	if code != "ERR_NETWORK_CONNECTION_REFUSED" {
		t.Errorf("Expected ERR_NETWORK_CONNECTION_REFUSED, got %s", code)
	}
}

func TestExceptionMapperTimeout(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("i/o timeout"), "modbus_tcp")
	if code != "ERR_NETWORK_TIMEOUT" {
		t.Errorf("Expected ERR_NETWORK_TIMEOUT, got %s", code)
	}
}

func TestExceptionMapperDNSFailed(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("no such host"), "modbus_tcp")
	if code != "ERR_NETWORK_DNS_FAILED" {
		t.Errorf("Expected ERR_NETWORK_DNS_FAILED, got %s", code)
	}
}

func TestExceptionMapperPermissionDenied(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("permission denied"), "modbus_tcp")
	if code != "ERR_AUTH_PERMISSION_DENIED" {
		t.Errorf("Expected ERR_AUTH_PERMISSION_DENIED, got %s", code)
	}
}

func TestExceptionMapperNotImplemented(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("not implemented"), "modbus_tcp")
	if code != "ERR_DRIVER_NOT_FOUND" {
		t.Errorf("Expected ERR_DRIVER_NOT_FOUND, got %s", code)
	}
}

func TestExceptionMapperConfigInvalid(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("invalid config"), "modbus_tcp")
	if code != "ERR_DEVICE_CONFIG_INVALID" {
		t.Errorf("Expected ERR_DEVICE_CONFIG_INVALID, got %s", code)
	}
}

func TestExceptionMapperNetworkUnreachable(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("network is unreachable"), "modbus_tcp")
	if code != "ERR_NETWORK_HOST_UNREACHABLE" {
		t.Errorf("Expected ERR_NETWORK_HOST_UNREACHABLE, got %s", code)
	}
}

func TestExceptionMapperConnectionReset(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("connection reset"), "modbus_tcp")
	if code != "ERR_NETWORK_CONNECTION_REFUSED" {
		t.Errorf("Expected ERR_NETWORK_CONNECTION_REFUSED, got %s", code)
	}
}

func TestExceptionMapperUnknownError(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(errors.New("some random error"), "modbus_tcp")
	if code != "ERR_COMMON_INTERNAL_ERROR" {
		t.Errorf("Expected ERR_COMMON_INTERNAL_ERROR, got %s", code)
	}
}

func TestExceptionMapperNilError(t *testing.T) {
	mapper := DriverExceptionMapper{}
	code := mapper.MapException(nil, "modbus_tcp")
	if code != "" {
		t.Errorf("Expected empty code for nil error, got %s", code)
	}
}

// --- DriverWatchdog Tests ---

func TestDriverWatchdogNilError(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	if !w.HandleException(nil, "test") {
		t.Error("Should return true (continue) for nil error")
	}
}

func TestDriverWatchdogContextCanceled(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	if w.HandleException(context.Canceled, "test") {
		t.Error("Should return false (stop) for context canceled")
	}
}

func TestDriverWatchdogConnectionError(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	// Should continue on connection errors
	if !w.HandleException(errors.New("connection refused"), "test") {
		t.Error("Should return true (continue) for connection error")
	}
}

func TestDriverWatchdogUnknownError(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	if !w.HandleException(errors.New("some unknown error"), "test") {
		t.Error("Should return true (continue) for unknown error")
	}
}

func TestDriverWatchdogGetRecentExceptions(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	w.HandleException(errors.New("error 1"), "ctx1")
	w.HandleException(errors.New("error 2"), "ctx2")

	exceptions := w.GetRecentExceptions()
	if len(exceptions) != 2 {
		t.Errorf("Expected 2 exceptions, got %d", len(exceptions))
	}
}

func TestDriverWatchdogMaxHistory(t *testing.T) {
	w := NewDriverWatchdog(nil, nil)
	w.maxHistory = 3

	for i := 0; i < 5; i++ {
		w.HandleException(fmt.Errorf("error %d", i), "ctx")
	}

	exceptions := w.GetRecentExceptions()
	if len(exceptions) != 3 {
		t.Errorf("Expected 3 exceptions (max history), got %d", len(exceptions))
	}
}

// --- PointHealthTracker Tests ---

func TestPointHealthTrackerRecordSuccess(t *testing.T) {
	pht := NewPointHealthTracker()
	pht.RecordSuccess("device-1", "temperature", 10.0)
	pht.RecordSuccess("device-1", "temperature", 20.0)

	rate := pht.GetSuccessRate("device-1", "temperature")
	if rate != 1.0 {
		t.Errorf("Expected 1.0 success rate, got %f", rate)
	}

	avgLatency := pht.GetAvgLatency("device-1", "temperature")
	if avgLatency != 15.0 {
		t.Errorf("Expected 15.0 avg latency, got %f", avgLatency)
	}
}

func TestPointHealthTrackerRecordFailure(t *testing.T) {
	pht := NewPointHealthTracker()
	pht.RecordSuccess("device-1", "temperature", 10.0)
	pht.RecordFailure("device-1", "temperature")

	rate := pht.GetSuccessRate("device-1", "temperature")
	if rate != 0.5 {
		t.Errorf("Expected 0.5 success rate, got %f", rate)
	}
}

func TestPointHealthTrackerEmpty(t *testing.T) {
	pht := NewPointHealthTracker()
	rate := pht.GetSuccessRate("device-1", "temperature")
	if rate != 1.0 {
		t.Errorf("Expected 1.0 for no data, got %f", rate)
	}

	avgLatency := pht.GetAvgLatency("device-1", "temperature")
	if avgLatency != 0.0 {
		t.Errorf("Expected 0.0 for no data, got %f", avgLatency)
	}
}

func TestPointHealthTrackerCheckFrozen(t *testing.T) {
	pht := NewPointHealthTracker()
	pht.frozenThresholds["device-1:temperature"] = 0.1 // 0.1 second threshold

	// First call - not frozen
	frozen := pht.CheckFrozen("device-1", "temperature", 50.0)
	if frozen {
		t.Error("Should not be frozen on first call")
	}

	// Immediately call again with same value - should be frozen (time elapsed > threshold)
	// But since we just set it, need to wait
	time.Sleep(200 * time.Millisecond)
	frozen = pht.CheckFrozen("device-1", "temperature", 50.0)
	if !frozen {
		t.Error("Should be frozen after threshold with same value")
	}
}

func TestPointHealthTrackerCheckRateOfChange(t *testing.T) {
	pht := NewPointHealthTracker()

	// First call - no previous data
	exceeded := pht.CheckRateOfChange("device-1", "temperature", 50.0, 100.0)
	if exceeded {
		t.Error("Should not exceed rate on first call")
	}

	// Wait and call with big jump
	time.Sleep(100 * time.Millisecond)
	exceeded = pht.CheckRateOfChange("device-1", "temperature", 5000.0, 10.0)
	if !exceeded {
		t.Error("Should exceed rate with large jump")
	}
}

// --- OTAManager Tests ---

func TestOTAManagerCreateJob(t *testing.T) {
	mgr := NewOTAManager()
	job := mgr.CreateJob("job-1", "device-1", "http://example.com/firmware.bin", "1.0.0")

	if job.JobID != "job-1" {
		t.Errorf("Expected job ID 'job-1', got '%s'", job.JobID)
	}
	if job.Status != "pending" {
		t.Errorf("Expected status 'pending', got '%s'", job.Status)
	}
	if job.Progress != 0 {
		t.Errorf("Expected progress 0, got %f", job.Progress)
	}
}

func TestOTAManagerUpdateJobStatus(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "http://example.com/fw.bin", "1.0.0")

	err := mgr.UpdateJobStatus("job-1", "downloading", 50.0, "")
	if err != nil {
		t.Fatalf("UpdateJobStatus failed: %v", err)
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "downloading" {
		t.Errorf("Expected 'downloading', got '%s'", job.Status)
	}
	if job.Progress != 50.0 {
		t.Errorf("Expected progress 50.0, got %f", job.Progress)
	}
}

func TestOTAManagerUpdateJobNotFound(t *testing.T) {
	mgr := NewOTAManager()
	err := mgr.UpdateJobStatus("nonexistent", "downloading", 50.0, "")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

func TestOTAManagerGetJobNotFound(t *testing.T) {
	mgr := NewOTAManager()
	_, err := mgr.GetJob("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

func TestOTAManagerListJobs(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "url1", "1.0.0")
	mgr.CreateJob("job-2", "device-1", "url2", "1.0.1")
	mgr.CreateJob("job-3", "device-2", "url3", "2.0.0")

	// List all
	all := mgr.ListJobs("")
	if len(all) != 3 {
		t.Errorf("Expected 3 jobs, got %d", len(all))
	}

	// List by device
	device1Jobs := mgr.ListJobs("device-1")
	if len(device1Jobs) != 2 {
		t.Errorf("Expected 2 jobs for device-1, got %d", len(device1Jobs))
	}
}

func TestOTAManagerCancelJob(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "url", "1.0.0")

	err := mgr.CancelJob("job-1")
	if err != nil {
		t.Fatalf("CancelJob failed: %v", err)
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "cancelled" {
		t.Errorf("Expected 'cancelled', got '%s'", job.Status)
	}
}

func TestOTAManagerCancelJobTerminal(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "url", "1.0.0")
	mgr.UpdateJobStatus("job-1", "success", 100.0, "")

	err := mgr.CancelJob("job-1")
	if err == nil {
		t.Error("Should not cancel job in terminal state")
	}
}

func TestOTAManagerCancelJobNotFound(t *testing.T) {
	mgr := NewOTAManager()
	err := mgr.CancelJob("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

func TestOTAManagerExecuteUpdateSuccess(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "http://example.com/fw.bin", "1.0.0")

	err := mgr.ExecuteUpdate(
		context.Background(),
		"job-1",
		func(ctx context.Context, url string) ([]byte, error) {
			return []byte("firmware data"), nil
		},
		func(data []byte) error {
			return nil
		},
		func(data []byte) error {
			return nil
		},
	)

	if err != nil {
		t.Fatalf("ExecuteUpdate failed: %v", err)
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "success" {
		t.Errorf("Expected 'success', got '%s'", job.Status)
	}
	if job.Progress != 100.0 {
		t.Errorf("Expected progress 100.0, got %f", job.Progress)
	}
}

func TestOTAManagerExecuteUpdateDownloadFail(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "http://example.com/fw.bin", "1.0.0")

	err := mgr.ExecuteUpdate(
		context.Background(),
		"job-1",
		func(ctx context.Context, url string) ([]byte, error) {
			return nil, errors.New("download failed")
		},
		func(data []byte) error { return nil },
		func(data []byte) error { return nil },
	)

	if err == nil {
		t.Error("Expected error for download failure")
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "failed" {
		t.Errorf("Expected 'failed', got '%s'", job.Status)
	}
}

func TestOTAManagerExecuteUpdateVerifyFail(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "http://example.com/fw.bin", "1.0.0")

	err := mgr.ExecuteUpdate(
		context.Background(),
		"job-1",
		func(ctx context.Context, url string) ([]byte, error) {
			return []byte("data"), nil
		},
		func(data []byte) error {
			return errors.New("verification failed")
		},
		func(data []byte) error { return nil },
	)

	if err == nil {
		t.Error("Expected error for verification failure")
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "failed" {
		t.Errorf("Expected 'failed', got '%s'", job.Status)
	}
}

func TestOTAManagerExecuteUpdateApplyFail(t *testing.T) {
	mgr := NewOTAManager()
	mgr.CreateJob("job-1", "device-1", "http://example.com/fw.bin", "1.0.0")

	err := mgr.ExecuteUpdate(
		context.Background(),
		"job-1",
		func(ctx context.Context, url string) ([]byte, error) {
			return []byte("data"), nil
		},
		func(data []byte) error { return nil },
		func(data []byte) error {
			return errors.New("apply failed")
		},
	)

	if err == nil {
		t.Error("Expected error for apply failure")
	}

	job, _ := mgr.GetJob("job-1")
	if job.Status != "failed" {
		t.Errorf("Expected 'failed', got '%s'", job.Status)
	}
}

func TestOTAManagerExecuteUpdateJobNotFound(t *testing.T) {
	mgr := NewOTAManager()

	err := mgr.ExecuteUpdate(
		context.Background(),
		"nonexistent",
		func(ctx context.Context, url string) ([]byte, error) { return nil, nil },
		func(data []byte) error { return nil },
		func(data []byte) error { return nil },
	)

	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

// --- TimeSeriesStore Tests ---

func TestTimeSeriesStoreWriteAndQuery(t *testing.T) {
	store := NewTimeSeriesStore(7)
	store.WriteReadResult("device-1", map[string]interface{}{
		"temperature": 25.5,
		"humidity":    60.0,
	})

	// Query latest
	latest := store.QueryLatest("device-1", []string{"temperature", "humidity"})
	if len(latest) != 2 {
		t.Fatalf("Expected 2 points, got %d", len(latest))
	}
	if latest["temperature"]["value"] != 25.5 {
		t.Errorf("Expected 25.5, got %v", latest["temperature"]["value"])
	}
}

func TestTimeSeriesStoreQueryWithTimeRange(t *testing.T) {
	store := NewTimeSeriesStore(7)
	store.WriteReadResult("device-1", map[string]interface{}{
		"temperature": 25.5,
	})

	// Query with nil time range (should return all)
	entries := store.Query("device-1", "temperature", nil, nil, 10)
	if len(entries) != 1 {
		t.Errorf("Expected 1 entry, got %d", len(entries))
	}

	// Query with future start time (should return nothing)
	future := time.Now().Add(1 * time.Hour)
	entries = store.Query("device-1", "temperature", &future, nil, 10)
	if len(entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(entries))
	}
}

func TestTimeSeriesStoreQueryByQuality(t *testing.T) {
	store := NewTimeSeriesStore(7)
	store.WriteReadResult("device-1", map[string]interface{}{
		"temperature": 25.5,
	})

	entries := store.QueryByQuality("device-1", "temperature", "good", 10)
	if len(entries) != 1 {
		t.Errorf("Expected 1 entry with quality 'good', got %d", len(entries))
	}

	entries = store.QueryByQuality("device-1", "temperature", "bad", 10)
	if len(entries) != 0 {
		t.Errorf("Expected 0 entries with quality 'bad', got %d", len(entries))
	}
}

func TestTimeSeriesStoreGetStats(t *testing.T) {
	store := NewTimeSeriesStore(7)
	store.WriteReadResult("device-1", map[string]interface{}{
		"temperature": 25.5,
		"humidity":    60.0,
	})

	stats := store.GetStats()
	totalEntries, ok := stats["total_entries"].(int)
	if !ok || totalEntries != 2 {
		t.Errorf("Expected 2 total entries, got %v", stats["total_entries"])
	}
	pointCount, ok := stats["point_count"].(int)
	if !ok || pointCount != 2 {
		t.Errorf("Expected 2 points, got %v", stats["point_count"])
	}
}

func TestTimeSeriesStoreCleanupOld(t *testing.T) {
	store := NewTimeSeriesStore(7)
	store.WriteReadResult("device-1", map[string]interface{}{
		"temperature": 25.5,
	})

	// Cleanup should not remove recent entries
	removed := store.CleanupOld()
	if removed != 0 {
		t.Errorf("Expected 0 removed, got %d", removed)
	}
}

func TestTimeSeriesStoreDefaultRetention(t *testing.T) {
	store := NewTimeSeriesStore(0) // Should default to 7
	if store.retentionDays != 7 {
		t.Errorf("Expected 7 retention days, got %d", store.retentionDays)
	}
}

// --- ExtendedRegistry Tests ---

func TestExtendedRegistryLoadStatus(t *testing.T) {
	r := NewExtendedRegistry()
	r.SetLoadStatus("modbus_tcp", &DriverLoadStatus{Loaded: true, Module: "modbus"})

	status := r.GetLoadStatus()
	if status["modbus_tcp"] == nil {
		t.Fatal("Expected load status for modbus_tcp")
	}
	if !status["modbus_tcp"].Loaded {
		t.Error("Expected Loaded=true")
	}
}

func TestExtendedRegistryDependencyResults(t *testing.T) {
	r := NewExtendedRegistry()
	r.dependencyResults["modbus_tcp"] = map[string]interface{}{"version": "1.0"}

	results := r.GetDependencyResults()
	if results["modbus_tcp"] == nil {
		t.Fatal("Expected dependency results for modbus_tcp")
	}
}

func TestIsBuiltinProtocol(t *testing.T) {
	if !IsBuiltinProtocol("modbus_tcp") {
		t.Error("modbus_tcp should be a built-in protocol")
	}
	if IsBuiltinProtocol("custom_protocol") {
		t.Error("custom_protocol should not be a built-in protocol")
	}
}

// --- ConfigVersionManager Tests ---

func TestConfigVersionManagerSnapshot(t *testing.T) {
	mgr := NewConfigVersionManager("")

	snap1 := mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{
		"host": "127.0.0.1",
		"port": 502,
	}, []string{"host"}, "admin")

	if snap1.Version != 1 {
		t.Errorf("Expected version 1, got %d", snap1.Version)
	}

	snap2 := mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{
		"host": "192.168.1.1",
		"port": 502,
	}, []string{"host"}, "admin")

	if snap2.Version != 2 {
		t.Errorf("Expected version 2, got %d", snap2.Version)
	}
}

func TestConfigVersionManagerRollback(t *testing.T) {
	mgr := NewConfigVersionManager("")

	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "127.0.0.1"}, nil, "admin")
	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "192.168.1.1"}, nil, "admin")

	snap, err := mgr.Rollback("device-1", 1)
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Expected version 1, got %d", snap.Version)
	}
}

func TestConfigVersionManagerRollbackNotFound(t *testing.T) {
	mgr := NewConfigVersionManager("")
	_, err := mgr.Rollback("device-1", 99)
	if err == nil {
		t.Error("Expected error for non-existent version")
	}
}

func TestConfigVersionManagerListVersions(t *testing.T) {
	mgr := NewConfigVersionManager("")

	for i := 0; i < 5; i++ {
		mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{
			"val": i,
		}, nil, "admin")
	}

	// ListVersions with limit=3, offset=2 → returns versions [5, 4, 3] (most recent first)
	versions := mgr.ListVersions("device-1", 3, 2)
	if len(versions) != 3 {
		t.Fatalf("Expected 3 versions, got %d", len(versions))
	}

	// Should be most recent first
	if versions[0].Version != 5 {
		t.Errorf("Expected version 5 first, got %d", versions[0].Version)
	}
}

func TestConfigVersionManagerListVersionsOffset(t *testing.T) {
	mgr := NewConfigVersionManager("")

	for i := 0; i < 3; i++ {
		mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{
			"val": i,
		}, nil, "admin")
	}

	versions := mgr.ListVersions("device-1", 10, 2)
	if len(versions) != 1 {
		t.Errorf("Expected 1 version, got %d", len(versions))
	}
}

func TestConfigVersionManagerGetVersion(t *testing.T) {
	mgr := NewConfigVersionManager("")
	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "127.0.0.1"}, nil, "admin")

	snap, err := mgr.GetVersion("device-1", 1)
	if err != nil {
		t.Fatalf("GetVersion failed: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Expected version 1, got %d", snap.Version)
	}
}

func TestConfigVersionManagerGetVersionNotFound(t *testing.T) {
	mgr := NewConfigVersionManager("")
	_, err := mgr.GetVersion("device-1", 99)
	if err == nil {
		t.Error("Expected error for non-existent version")
	}
}

func TestConfigVersionManagerDiffVersions(t *testing.T) {
	mgr := NewConfigVersionManager("")

	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "127.0.0.1", "port": 502}, nil, "admin")
	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "192.168.1.1", "port": 502}, nil, "admin")

	diff, err := mgr.DiffVersions("device-1", 1, 2)
	if err != nil {
		t.Fatalf("DiffVersions failed: %v", err)
	}
	changes, ok := diff["changes"].([]string)
	if !ok {
		t.Fatal("Expected changes to be []string")
	}
	if len(changes) != 1 {
		t.Errorf("Expected 1 change, got %d", len(changes))
	}
}

func TestConfigVersionManagerExportImportJSON(t *testing.T) {
	mgr := NewConfigVersionManager("")

	mgr.SnapshotDeviceConfig("device-1", map[string]interface{}{"host": "127.0.0.1"}, nil, "admin")

	jsonStr, err := mgr.ExportJSON("device-1")
	if err != nil {
		t.Fatalf("ExportJSON failed: %v", err)
	}

	// Import to new manager
	mgr2 := NewConfigVersionManager("")
	err = mgr2.ImportJSON("device-1", jsonStr)
	if err != nil {
		t.Fatalf("ImportJSON failed: %v", err)
	}

	snap, err := mgr2.GetVersion("device-1", 1)
	if err != nil {
		t.Fatalf("GetVersion after import failed: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Expected version 1, got %d", snap.Version)
	}
}

// --- AuditLog Tests ---

func TestAuditLogWrite(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")

	records := auditLog.GetRecent(10)
	if len(records) != 1 {
		t.Fatalf("Expected 1 record, got %d", len(records))
	}
	if records[0].Action != AuditActionWrite {
		t.Errorf("Expected action 'write', got '%s'", records[0].Action)
	}
}

func TestAuditLogConfigChange(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogConfigChange("device-1", "admin",
		map[string]interface{}{"host": "127.0.0.1"},
		map[string]interface{}{"host": "192.168.1.1"})

	records := auditLog.GetRecent(10)
	if len(records) != 1 {
		t.Fatalf("Expected 1 record, got %d", len(records))
	}
	if records[0].Action != AuditActionConfigChange {
		t.Errorf("Expected action 'config_change', got '%s'", records[0].Action)
	}
}

func TestAuditLogFailover(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogFailover("device-1", "host-a", "host-b")

	records := auditLog.GetByDevice("device-1", 10)
	if len(records) != 1 {
		t.Fatalf("Expected 1 record, got %d", len(records))
	}
	if records[0].Action != AuditActionFailover {
		t.Errorf("Expected action 'failover', got '%s'", records[0].Action)
	}
}

func TestAuditLogReconnect(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogReconnect("device-1", true)
	auditLog.LogReconnect("device-2", false)

	records := auditLog.GetByAction(AuditActionReconnect, 10)
	if len(records) != 2 {
		t.Fatalf("Expected 2 records, got %d", len(records))
	}
}

func TestAuditLogGetByDevice(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")
	auditLog.LogWrite("device-2", "point-2", "admin", 30.0, 40.0, "success", "")
	auditLog.LogWrite("device-1", "point-3", "admin", 50.0, 60.0, "success", "")

	records := auditLog.GetByDevice("device-1", 10)
	if len(records) != 2 {
		t.Errorf("Expected 2 records for device-1, got %d", len(records))
	}
}

func TestAuditLogGetByAction(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")
	auditLog.LogConfigChange("device-1", "admin", nil, nil)
	auditLog.LogWrite("device-2", "point-2", "admin", 30.0, 40.0, "success", "")

	records := auditLog.GetByAction(AuditActionWrite, 10)
	if len(records) != 2 {
		t.Errorf("Expected 2 write records, got %d", len(records))
	}
}

func TestAuditLogGetStats(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")
	auditLog.LogConfigChange("device-1", "admin", nil, nil)
	auditLog.LogReconnect("device-1", true)

	stats := auditLog.GetStats()
	totalRecords, ok := stats["total_records"].(int)
	if !ok || totalRecords != 3 {
		t.Errorf("Expected 3 total records, got %v", stats["total_records"])
	}
}

func TestAuditLogExportCSV(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")

	csv := auditLog.ExportCSV(nil, nil)
	if !strings.Contains(csv, "timestamp") {
		t.Error("CSV should contain header")
	}
	if !strings.Contains(csv, "device-1") {
		t.Error("CSV should contain device-1")
	}
}

func TestAuditLogExportCSVWithTimeRange(t *testing.T) {
	auditLog := NewAuditLog(100)
	auditLog.LogWrite("device-1", "point-1", "admin", 10.0, 20.0, "success", "")

	// Future time range → should exclude all
	future := time.Now().Add(1 * time.Hour)
	csv := auditLog.ExportCSV(&future, nil)
	// Should only contain header
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) != 1 {
		t.Errorf("Expected only header, got %d lines", len(lines))
	}
}

func TestAuditLogMaxSize(t *testing.T) {
	auditLog := NewAuditLog(3) // small max size

	for i := 0; i < 5; i++ {
		auditLog.LogWrite("device-1", "point-1", "admin", float64(i), float64(i+1), "success", "")
	}

	records := auditLog.GetRecent(10)
	if len(records) != 3 {
		t.Errorf("Expected 3 records (max size), got %d", len(records))
	}
}

func TestAuditLogDefaultMaxSize(t *testing.T) {
	auditLog := NewAuditLog(0)
	if auditLog.maxSize != 1000 {
		t.Errorf("Expected default maxSize 1000, got %d", auditLog.maxSize)
	}
}

func TestAuditRecordToDict(t *testing.T) {
	record := AuditRecord{
		DeviceID: "device-1",
		Action:   AuditActionWrite,
		PointID:  "point-1",
		User:     "admin",
	}

	dict := record.ToDict()
	if dict["device_id"] != "device-1" {
		t.Errorf("Expected 'device-1', got %v", dict["device_id"])
	}
	if dict["action"] != "write" {
		t.Errorf("Expected 'write', got %v", dict["action"])
	}
}

// --- LinkRedundancyManager Tests ---

func TestLinkRedundancyRegisterDevice(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	config := &RedundancyConfig{
		PrimaryHost:         "host-a",
		BackupHost:          "host-b",
		SwitchThreshold:      3,
		SwitchbackDelay:      10,
		HealthCheckInterval: 5,
	}
	mgr.RegisterDevice("device-1", config)

	role := mgr.GetActiveRole("device-1")
	if role != LinkRolePrimary {
		t.Errorf("Expected primary role, got %s", role)
	}

	host := mgr.GetActiveHost("device-1")
	if host != "host-a" {
		t.Errorf("Expected 'host-a', got '%s'", host)
	}
}

func TestLinkRedundancyRecordSuccess(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	config := &RedundancyConfig{
		PrimaryHost:    "host-a",
		BackupHost:     "host-b",
		SwitchThreshold: 3,
	}
	mgr.RegisterDevice("device-1", config)

	mgr.RecordFailure("device-1")
	mgr.RecordSuccess("device-1") // Should reset failures

	status := mgr.GetStatus("device-1")
	if status["consecutive_failures"].(int) != 0 {
		t.Errorf("Expected 0 failures after success, got %v", status["consecutive_failures"])
	}
}

func TestLinkRedundancySwitchToBackup(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	switched := false
	mgr.SetOnSwitchCallback(func(deviceID, fromHost, toHost string) {
		switched = true
	})

	config := &RedundancyConfig{
		PrimaryHost:    "host-a",
		BackupHost:     "host-b",
		SwitchThreshold: 2,
	}
	mgr.RegisterDevice("device-1", config)

	// Trigger switch
	mgr.RecordFailure("device-1")
	mgr.RecordFailure("device-1")

	if !switched {
		t.Error("Expected switch callback to be called")
	}

	role := mgr.GetActiveRole("device-1")
	if role != LinkRoleBackup {
		t.Errorf("Expected backup role, got %s", role)
	}

	host := mgr.GetActiveHost("device-1")
	if host != "host-b" {
		t.Errorf("Expected 'host-b', got '%s'", host)
	}
}

func TestLinkRedundancyMarkPrimaryHealthy(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	config := &RedundancyConfig{
		PrimaryHost:    "host-a",
		BackupHost:     "host-b",
		SwitchThreshold: 2,
	}
	mgr.RegisterDevice("device-1", config)

	// Switch to backup first
	mgr.RecordFailure("device-1")
	mgr.RecordFailure("device-1")

	// Mark primary healthy → should switch back
	mgr.MarkPrimaryHealthy("device-1")

	role := mgr.GetActiveRole("device-1")
	if role != LinkRolePrimary {
		t.Errorf("Expected primary role after MarkPrimaryHealthy, got %s", role)
	}
}

func TestLinkRedundancyUnregisterDevice(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	config := &RedundancyConfig{
		PrimaryHost:    "host-a",
		BackupHost:     "host-b",
		SwitchThreshold: 2,
	}
	mgr.RegisterDevice("device-1", config)
	mgr.UnregisterDevice("device-1")

	role := mgr.GetActiveRole("device-1")
	if role != LinkRolePrimary {
		t.Errorf("Expected primary for unregistered device, got %s", role)
	}

	host := mgr.GetActiveHost("device-1")
	if host != "" {
		t.Errorf("Expected empty host for unregistered device, got '%s'", host)
	}
}

func TestLinkRedundancyGetStatusUnregistered(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	status := mgr.GetStatus("nonexistent")
	if status["enabled"] != false {
		t.Errorf("Expected enabled=false for unregistered device, got %v", status["enabled"])
	}
}

func TestLinkRedundancyStop(t *testing.T) {
	mgr := NewLinkRedundancyManager()
	config := &RedundancyConfig{
		PrimaryHost:    "host-a",
		BackupHost:     "host-b",
		SwitchThreshold: 2,
	}
	mgr.RegisterDevice("device-1", config)
	mgr.Stop() // Should not panic
}

// --- LRU Cache Extended Tests ---

func TestLRUCachePop(t *testing.T) {
	cache := NewLRUCache(10)
	cache.Set("a", 1)

	val, ok := cache.Pop("a")
	if !ok || val.(int) != 1 {
		t.Errorf("Expected a=1, got %v (%v)", val, ok)
	}

	// After pop, should not exist
	_, ok = cache.Get("a")
	if ok {
		t.Error("Expected 'a' to not exist after Pop")
	}
}

func TestLRUCachePopNonExistent(t *testing.T) {
	cache := NewLRUCache(10)
	_, ok := cache.Pop("nonexistent")
	if ok {
		t.Error("Expected false for non-existent key")
	}
}

func TestLRUCacheClear(t *testing.T) {
	cache := NewLRUCache(10)
	cache.Set("a", 1)
	cache.Set("b", 2)

	cache.Clear()

	if cache.Len() != 0 {
		t.Errorf("Expected 0 after clear, got %d", cache.Len())
	}
}

func TestLRUCacheKeys(t *testing.T) {
	cache := NewLRUCache(10)
	cache.Set("a", 1)
	cache.Set("b", 2)

	keys := cache.Keys()
	if len(keys) != 2 {
		t.Errorf("Expected 2 keys, got %d", len(keys))
	}
}

func TestLRUCacheUpdateExisting(t *testing.T) {
	cache := NewLRUCache(10)
	cache.Set("a", 1)
	cache.Set("a", 2) // Update

	val, ok := cache.Get("a")
	if !ok || val.(int) != 2 {
		t.Errorf("Expected a=2 after update, got %v (%v)", val, ok)
	}
}

func TestLRUCacheDefaultSize(t *testing.T) {
	cache := NewLRUCache(0) // Should default to 10000
	if cache.maxSize != 10000 {
		t.Errorf("Expected default max size 10000, got %d", cache.maxSize)
	}
}

// --- BaseDriver Extended Tests ---

func TestBaseDriverSettersAndGetters(t *testing.T) {
	bd := &BaseDriver{}

	bd.SetDeviceID("test-device")
	if bd.DeviceID() != "test-device" {
		t.Errorf("Expected 'test-device', got '%s'", bd.DeviceID())
	}

	config := map[string]interface{}{"host": "127.0.0.1"}
	bd.SetConfig(config)
	if bd.GetConfig()["host"] != "127.0.0.1" {
		t.Errorf("Expected config host '127.0.0.1'")
	}

	bd.SetConnected(true)
	if !bd.IsConnected() {
		t.Error("Expected connected=true")
	}

	bd.SetConnected(false)
	if bd.IsConnected() {
		t.Error("Expected connected=false")
	}
}

func TestBaseDriverIsCircuitOpenNoBreaker(t *testing.T) {
	bd := &BaseDriver{}
	// Without circuit breaker, should always allow (return false)
	if bd.IsCircuitOpen() {
		t.Error("Expected IsCircuitOpen=false without circuit breaker")
	}
}

func TestBaseDriverRecordReadSuccessNoStats(t *testing.T) {
	bd := &BaseDriver{}
	// Should not panic with nil stats and breaker
	bd.RecordReadSuccess(10.0)
}

func TestBaseDriverRecordReadFailureNoStats(t *testing.T) {
	bd := &BaseDriver{}
	// Should not panic with nil stats and breaker
	bd.RecordReadFailure()
}

func TestBaseDriverRecordWriteSuccessNoStats(t *testing.T) {
	bd := &BaseDriver{}
	bd.RecordWriteSuccess()
}

func TestBaseDriverRecordWriteFailureNoStats(t *testing.T) {
	bd := &BaseDriver{}
	bd.RecordWriteFailure()
}

func TestBaseDriverSetConnectionStateNoStats(t *testing.T) {
	bd := &BaseDriver{}
	bd.SetConnectionState(StateConnected, "test")
}

func TestBaseDriverWithInfrastructure(t *testing.T) {
	bd := &BaseDriver{}
	bd.SetDeviceID("device-1")

	hsm := NewHealthStatsManager()
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())

	bd.SetHealthStatsManager(hsm)
	bd.SetCircuitBreaker(cb)

	if bd.GetHealthStatsManager() == nil {
		t.Error("HealthStatsManager should be set")
	}
	if bd.GetCircuitBreaker() == nil {
		t.Error("CircuitBreaker should be set")
	}

	// Record success
	bd.RecordReadSuccess(15.0)
	stats := hsm.GetHealthStats("device-1")
	if stats == nil {
		t.Fatal("Expected health stats after RecordReadSuccess")
	}
	if stats.TotalReads != 1 {
		t.Errorf("Expected 1 read, got %d", stats.TotalReads)
	}

	// Record failure
	bd.RecordReadFailure()
	stats = hsm.GetHealthStats("device-1")
	if stats.FailedReads != 1 {
		t.Errorf("Expected 1 failed read, got %d", stats.FailedReads)
	}

	// Record write
	bd.RecordWriteSuccess()
	if stats.TotalWrites != 1 {
		t.Errorf("Expected 1 write, got %d", stats.TotalWrites)
	}

	bd.RecordWriteFailure()
	if stats.FailedWrites != 1 {
		t.Errorf("Expected 1 failed write, got %d", stats.FailedWrites)
	}
}

// --- HealthStats Extended Tests ---

func TestHealthStatsGetAllHealthStats(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordReadSuccess("device-1", 10.0)
	hsm.RecordReadSuccess("device-2", 20.0)

	all := hsm.GetAllHealthStats()
	if len(all) != 2 {
		t.Errorf("Expected 2 devices, got %d", len(all))
	}
}

func TestHealthStatsResetHealthStats(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordReadSuccess("device-1", 10.0)

	hsm.ResetHealthStats("device-1")

	stats := hsm.GetHealthStats("device-1")
	if stats != nil {
		t.Error("Expected nil after reset")
	}
}

func TestHealthStatsGetObservabilityMetricsNoDevice(t *testing.T) {
	hsm := NewHealthStatsManager()
	// A device the manager never saw has no measurements. Reporting quality 100
	// for it would show a healthy device that does not exist.
	if metrics := hsm.GetObservabilityMetrics("nonexistent"); metrics != nil {
		t.Errorf("Expected nil for an unmeasured device, got %v", metrics)
	}
}

func TestHealthStatsGetObservabilityMetricsWithDevice(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordReadSuccess("device-1", 10.0)
	hsm.RecordReadFailure("device-1")

	metrics := hsm.GetObservabilityMetrics("device-1")
	ctr := hsm.GetHealthStats("device-1").Counters()
	if metrics["consecutive_failures"] == int64(0) {
		t.Error("Expected non-zero consecutive failures")
	}
	// Every other field is a pass-through of a counter the manager keeps, so
	// compare against the same snapshot rather than a number typed by hand.
	for key, want := range map[string]interface{}{
		"consecutive_failures":     ctr.ConsecutiveFailures,
		"connection_quality_score": ctr.ConnectionQualityScore,
		"total_downtime_seconds":   ctr.TotalDowntimeSeconds,
		"reconnect_count":          ctr.TotalReconnects,
	} {
		if got := metrics[key]; got != want {
			t.Errorf("%s = %v, want the counted %v", key, got, want)
		}
	}
	if _, ok := metrics["last_offline_at"].(string); !ok {
		t.Errorf("last_offline_at = %v (%T), want the read failure to stamp it",
			metrics["last_offline_at"], metrics["last_offline_at"])
	}
	if rate, ok := metrics["read_error_rate"].(float64); !ok || rate != 0.5 {
		t.Errorf("read_error_rate = %v, want the measured 0.5 (1 failure over 2 attempts)",
			metrics["read_error_rate"])
	}
	if avg, ok := metrics["avg_latency_ms"].(float64); !ok || avg != 10.0 {
		t.Errorf("avg_latency_ms = %v, want the timed 10ms sample", metrics["avg_latency_ms"])
	}
}

func TestHealthStatsObservabilityMetricsNullWhatWasNotMeasured(t *testing.T) {
	hsm := NewHealthStatsManager()
	// A push-style read is a real attempt but has no round trip to time.
	hsm.RecordReadSuccess("device-2", LatencyNotMeasured)

	metrics := hsm.GetObservabilityMetrics("device-2")
	if metrics["avg_latency_ms"] != nil {
		t.Errorf("avg_latency_ms = %v, want nil when no sample was timed", metrics["avg_latency_ms"])
	}
	if metrics["write_error_rate"] != nil {
		t.Errorf("write_error_rate = %v, want nil over zero write attempts", metrics["write_error_rate"])
	}
	if rate, ok := metrics["read_error_rate"].(float64); !ok || rate != 0.0 {
		t.Errorf("read_error_rate = %v, want the measured 0 over 1 attempt", metrics["read_error_rate"])
	}

	// A device that has only ever written has no read attempts to divide by.
	hsm.RecordWriteSuccess("device-3")
	onlyWrites := hsm.GetObservabilityMetrics("device-3")
	if onlyWrites["read_error_rate"] != nil {
		t.Errorf("read_error_rate = %v, want nil over zero read attempts", onlyWrites["read_error_rate"])
	}
	if rate, ok := onlyWrites["write_error_rate"].(float64); !ok || rate != 0.0 {
		t.Errorf("write_error_rate = %v, want the measured 0 over 1 attempt", onlyWrites["write_error_rate"])
	}
	if onlyWrites["last_online_at"] == nil {
		t.Error("last_online_at = nil, want the completed write to stamp it")
	}
}

// Both counters used to have no writer at all, so a device that had retried and
// spent real minutes offline still reported reconnect_count 0 and downtime 0.
func TestHealthStatsObservabilityMetricsCountTheRealRecovery(t *testing.T) {
	const deviceID = "device-flap"
	hsm := NewHealthStatsManager()

	hsm.RecordReadSuccess(deviceID, 10.0)
	hsm.RecordReadFailure(deviceID)
	time.Sleep(5 * time.Millisecond)
	hsm.RecordReadSuccess(deviceID, 20.0) // closes the offline window

	if !hsm.SetConnectionState(deviceID, StateConnecting, "first dial") {
		t.Fatal("the first dial was rejected by the state machine")
	}
	if !hsm.SetConnectionState(deviceID, StateConnected, "up") {
		t.Fatal("connecting -> connected was rejected")
	}
	if !hsm.SetConnectionState(deviceID, StateDisconnected, "drop") {
		t.Fatal("connected -> disconnected was rejected")
	}
	if !hsm.SetConnectionState(deviceID, StateConnecting, "retry") {
		t.Fatal("disconnected -> connecting was rejected")
	}

	metrics := hsm.GetObservabilityMetrics(deviceID)
	if got, ok := metrics["reconnect_count"].(int64); !ok || got != 1 {
		t.Errorf("reconnect_count = %v, want the single retry after a drop", metrics["reconnect_count"])
	}
	if got, ok := metrics["total_downtime_seconds"].(float64); !ok || got <= 0 {
		t.Errorf("total_downtime_seconds = %v, want the offline window counted",
			metrics["total_downtime_seconds"])
	}
}

func TestHealthStatsHealthScore(t *testing.T) {
	hs := NewDriverHealthStats("device-1")
	hs.ConsecutiveFailures = 3
	hs.TotalReads = 10
	hs.FailedReads = 2

	score := hs.HealthScore()
	if score >= 100.0 {
		t.Errorf("Expected score < 100 with failures, got %f", score)
	}
	if score < 0 {
		t.Errorf("Expected score >= 0, got %f", score)
	}
}

func TestHealthStatsHealthScoreHighLatency(t *testing.T) {
	hs := NewDriverHealthStats("device-1")
	hs.AvgLatencyMs = 2000 // High latency

	score := hs.HealthScore()
	if score >= 100.0 {
		t.Errorf("Expected score < 100 with high latency, got %f", score)
	}
}

func TestHealthStatsHealthScoreReconnects(t *testing.T) {
	hs := NewDriverHealthStats("device-1")
	hs.TotalReconnects = 10

	score := hs.HealthScore()
	if score >= 100.0 {
		t.Errorf("Expected score < 100 with reconnects, got %f", score)
	}
}

func TestHealthStatsEffectiveState(t *testing.T) {
	hs := NewDriverHealthStats("device-1")

	// No reads → connected
	if hs.EffectiveState() != StateConnected {
		t.Errorf("Expected connected state, got %s", hs.EffectiveState())
	}

	// Consecutive failures >= 5 → offline
	hs.ConsecutiveFailures = 5
	if hs.EffectiveState() != StateOffline {
		t.Errorf("Expected offline state, got %s", hs.EffectiveState())
	}

	// Some failures but < 5 → degraded
	hs.ConsecutiveFailures = 2
	hs.TotalReads = 10
	hs.FailedReads = 5 // 50% error rate
	if hs.EffectiveState() != StateDegraded {
		t.Errorf("Expected degraded state, got %s", hs.EffectiveState())
	}
}

func TestHealthStatsConnectionStateInvalidTransition(t *testing.T) {
	hsm := NewHealthStatsManager()

	// Set to connected
	hsm.SetConnectionState("device-1", StateConnected, "connected")

	// connected → connecting is now LEGAL: a live socket can die under the
	// driver (peer restart), and the reconnect path dials afresh. Rejecting it
	// wedged a driver in a phantom "connected" state with a dead socket, so the
	// circuit breaker never recovered (94+ consecutive collection failures in
	// joint debugging while the peer served other clients fine).
	if ok := hsm.SetConnectionState("device-1", StateConnecting, "reconnect after peer loss"); !ok {
		t.Error("connected → connecting (reconnect) must be allowed")
	}

	// Still invalid: disconnected has no path into degraded directly.
	hsm.SetConnectionState("device-1", StateDisconnected, "down")
	if ok := hsm.SetConnectionState("device-1", StateDegraded, "invalid"); ok {
		t.Error("Expected invalid transition disconnected → degraded to be rejected")
	}
}

func TestHealthStatsConnectionStateOfflineTransition(t *testing.T) {
	hsm := NewHealthStatsManager()

	// Disconnected → Connecting → Connected → Offline → Connecting
	hsm.SetConnectionState("device-1", StateConnecting, "start")
	hsm.SetConnectionState("device-1", StateConnected, "up")
	ok := hsm.SetConnectionState("device-1", StateOffline, "down")
	if !ok {
		t.Error("Expected connected → offline transition to succeed")
	}

	// Offline → Connecting (valid)
	ok = hsm.SetConnectionState("device-1", StateConnecting, "reconnect")
	if !ok {
		t.Error("Expected offline → connecting transition to succeed")
	}
}

func TestHealthStatsWriteErrorRate(t *testing.T) {
	hsm := NewHealthStatsManager()
	hsm.RecordWriteSuccess("device-1")
	hsm.RecordWriteSuccess("device-1")
	hsm.RecordWriteFailure("device-1")

	stats := hsm.GetHealthStats("device-1")
	if stats == nil {
		t.Fatal("Expected stats")
	}
	rate := stats.WriteErrorRate()
	if rate < 0.3 || rate > 0.4 { // ~33.3%
		t.Errorf("Expected ~0.33 write error rate, got %f", rate)
	}
}

// --- Protocols Helper Tests ---

func TestReadUint16BE(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	if v := readUint16BE(data, 0); v != 0x0102 {
		t.Errorf("Expected 0x0102, got 0x%04x", v)
	}
	if v := readUint16BE(data, 2); v != 0x0304 {
		t.Errorf("Expected 0x0304, got 0x%04x", v)
	}
	// Out of bounds
	if v := readUint16BE(data, 3); v != 0 {
		t.Errorf("Expected 0 for out-of-bounds, got 0x%04x", v)
	}
}

func TestReadUint32BE(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if v := readUint32BE(data, 0); v != 0x01020304 {
		t.Errorf("Expected 0x01020304, got 0x%08x", v)
	}
	if v := readUint32BE(data, 2); v != 0x03040506 {
		t.Errorf("Expected 0x03040506, got 0x%08x", v)
	}
	// Out of bounds
	if v := readUint32BE(data, 3); v != 0 {
		t.Errorf("Expected 0 for out-of-bounds, got 0x%08x", v)
	}
}

// --- Registry Global Functions Tests ---

func TestGetGlobalHealthStatsManager(t *testing.T) {
	mgr := GetHealthStatsManager()
	if mgr == nil {
		t.Error("Expected non-nil global HealthStatsManager")
	}
}

func TestGetGlobalCircuitBreaker(t *testing.T) {
	cb := GetCircuitBreaker()
	if cb == nil {
		t.Error("Expected non-nil global CircuitBreaker")
	}
}

func TestGetGlobalReconnectManager(t *testing.T) {
	rm := GetReconnectManager()
	if rm == nil {
		t.Error("Expected non-nil global ReconnectManager")
	}
}

func TestRegisterAllAndSupportedProtocols(t *testing.T) {
	RegisterAll()
	r := GetRegistry()

	protocols := r.SupportedProtocols()
	if len(protocols) == 0 {
		t.Error("Expected non-empty supported protocols after RegisterAll")
	}

	// Check some expected protocols
	expected := []string{"modbus_tcp", "simulator", "siemens_s7", "mqtt_client"}
	for _, p := range expected {
		if !r.IsSupported(p) {
			t.Errorf("Expected protocol '%s' to be supported", p)
		}
	}
}

// --- Concurrency Tests ---

func TestLRUCacheConcurrentAccess(t *testing.T) {
	cache := NewLRUCache(100)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cache.Set(fmt.Sprintf("key-%d-%d", idx, j), j)
				cache.Get(fmt.Sprintf("key-%d-%d", idx, j))
			}
		}(i)
	}

	wg.Wait()
	// Should not panic or deadlock
}

func TestCircuitBreakerConcurrentAccess(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold:  100,
		RecoveryTimeout:   1 * time.Second,
		HalfOpenMaxCalls:  10,
	})

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("device-%d", idx)
			for j := 0; j < 50; j++ {
				cb.AllowRequest(deviceID)
				cb.RecordSuccess(deviceID)
			}
		}(i)
	}

	wg.Wait()
	// Should not panic or deadlock
}

func TestHealthStatsManagerConcurrentAccess(t *testing.T) {
	hsm := NewHealthStatsManager()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("device-%d", idx)
			for j := 0; j < 50; j++ {
				hsm.RecordReadSuccess(deviceID, float64(j))
				hsm.GetHealthStats(deviceID)
				hsm.GetObservabilityMetrics(deviceID)
			}
		}(i)
	}

	wg.Wait()
}

// --- SimulatorDriver Extended Tests ---

func TestSimulatorDriverReconnect(t *testing.T) {
	driver, _ := NewSimulatorDriver("recon-device", nil)
	driver.Disconnect()
	if driver.IsConnected() {
		t.Error("Should be disconnected")
	}

	// Reconnect
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("Reconnect failed: %v", err)
	}
	if !driver.IsConnected() {
		t.Error("Should be connected after reconnect")
	}
}

func TestSimulatorDriverWriteFloat(t *testing.T) {
	driver, _ := NewSimulatorDriver("write-device", nil)
	err := driver.WritePoint(context.Background(), "setpoint", 42.5)
	if err != nil {
		t.Fatalf("WritePoint failed: %v", err)
	}
}

func TestSimulatorDriverReadPointsWithScaleOffset(t *testing.T) {
	driver, _ := NewSimulatorDriver("scale-device", nil)
	driver.Connect(context.Background())

	scale := 2.0
	offset := 10.0
	points := []models.PointDef{
		{Name: "scaled", Mode: "constant", Scale: &scale, Offset: &offset, Min: &scale, Max: &offset},
	}

	data, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("ReadPoints failed: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(data))
	}
}

func TestSimulatorDriverReadPointsEmptyMode(t *testing.T) {
	driver, _ := NewSimulatorDriver("mode-device", nil)
	driver.Connect(context.Background())

	minVal := 0.0
	maxVal := 100.0
	points := []models.PointDef{
		{Name: "empty_mode", Mode: "", Min: &minVal, Max: &maxVal},
	}

	data, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("ReadPoints failed: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(data))
	}
}

func TestSimulatorDriverReadPointsRampMode(t *testing.T) {
	driver, _ := NewSimulatorDriver("ramp-device", nil)
	driver.Connect(context.Background())

	minVal := 0.0
	maxVal := 50.0
	points := []models.PointDef{
		{Name: "ramp", Mode: "ramp", Min: &minVal, Max: &maxVal},
	}

	// Read multiple times to test ramp progression
	for i := 0; i < 5; i++ {
		data, err := driver.ReadPoints(context.Background(), points)
		if err != nil {
			t.Fatalf("ReadPoints[%d] failed: %v", i, err)
		}
		if len(data) != 1 {
			t.Fatalf("Expected 1 point, got %d", len(data))
		}
		val := data[0].Value.(float64)
		if val < 0 || val > 100 {
			t.Errorf("Ramp value out of range: %f", val)
		}
	}
}

func TestSimulatorDriverReadPointsStepMode(t *testing.T) {
	driver, _ := NewSimulatorDriver("step-device", nil)
	driver.Connect(context.Background())

	minVal := 0.0
	maxVal := 100.0
	points := []models.PointDef{
		{Name: "step", Mode: "step", Min: &minVal, Max: &maxVal},
	}

	data, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("ReadPoints failed: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(data))
	}
}

func TestSimulatorDriverReadPointsDefaultMode(t *testing.T) {
	driver, _ := NewSimulatorDriver("default-device", nil)
	driver.Connect(context.Background())

	minVal := 0.0
	maxVal := 100.0
	points := []models.PointDef{
		{Name: "default", Mode: "unknown_mode", Min: &minVal, Max: &maxVal},
	}

	data, err := driver.ReadPoints(context.Background(), points)
	if err != nil {
		t.Fatalf("ReadPoints failed: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(data))
	}
}

func TestReconnectManagerReset(t *testing.T) {
	rm := NewReconnectManager()
	rm.maxAttempts = 5
	rm.baseDelay = 1 * time.Millisecond

	// Fail a few times
	for i := 0; i < 3; i++ {
		rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
			return false, nil
		})
	}

	// Reset
	rm.ResetReconnectState("device-1")
	if attempts := rm.GetReconnectAttempts("device-1"); attempts != 0 {
		t.Errorf("Expected 0 attempts after reset, got %d", attempts)
	}
}

func TestReconnectManagerContextCancel(t *testing.T) {
	rm := NewReconnectManager()
	rm.baseDelay = 10 * time.Second // Long delay

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := rm.ReconnectWithBackoff(ctx, "device-1", func() (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Error("Expected context canceled error")
	}
}

func TestReconnectManagerWithError(t *testing.T) {
	rm := NewReconnectManager()
	rm.baseDelay = 1 * time.Millisecond

	_, err := rm.ReconnectWithBackoff(context.Background(), "device-1", func() (bool, error) {
		return false, errors.New("connection failed")
	})
	if err == nil {
		t.Error("Expected error from reconnect function")
	}
}

// --- GetConfigString with Various Types ---

func TestGetConfigStringWithInt(t *testing.T) {
	config := map[string]interface{}{
		"port": float64(502), // JSON-style float64
	}
	if v := GetConfigString(config, "port", "default"); v != "502" {
		t.Errorf("Expected '502', got '%s'", v)
	}
}

func TestGetConfigStringMissingKey(t *testing.T) {
	config := map[string]interface{}{}
	if v := GetConfigString(config, "nonexistent", "fallback"); v != "fallback" {
		t.Errorf("Expected 'fallback', got '%s'", v)
	}
}

func TestGetConfigStringNilConfig(t *testing.T) {
	if v := GetConfigString(nil, "key", "default"); v != "default" {
		t.Errorf("Expected 'default' for nil config, got '%s'", v)
	}
}

// --- Float64 conversion tests ---

func TestToFloat64UintTypes(t *testing.T) {
	if f, ok := toFloat64(uint16(16)); !ok || f != 16.0 {
		t.Errorf("Expected 16.0, got %f", f)
	}
	if f, ok := toFloat64(uint32(32)); !ok || f != 32.0 {
		t.Errorf("Expected 32.0, got %f", f)
	}
	if f, ok := toFloat64(uint64(64)); !ok || f != 64.0 {
		t.Errorf("Expected 64.0, got %f", f)
	}
}

func TestToFloat64Float32(t *testing.T) {
	if f, ok := toFloat64(float32(3.14)); !ok || f < 3.0 || f > 4.0 {
		t.Errorf("Expected ~3.14, got %f", f)
	}
}

func TestToFloat64Int32(t *testing.T) {
	if f, ok := toFloat64(int32(42)); !ok || f != 42.0 {
		t.Errorf("Expected 42.0, got %f", f)
	}
}

func TestToFloat64InvalidType(t *testing.T) {
	if _, ok := toFloat64("string"); ok {
		t.Error("Expected false for string type")
	}
}

func TestAbsFunction(t *testing.T) {
	if abs(-5.0) != 5.0 {
		t.Errorf("Expected 5.0, got %f", abs(-5.0))
	}
	if abs(5.0) != 5.0 {
		t.Errorf("Expected 5.0, got %f", abs(5.0))
	}
	if abs(0.0) != 0.0 {
		t.Errorf("Expected 0.0, got %f", abs(0.0))
	}
}

// --- Helper function tests ---

func TestContainsAny(t *testing.T) {
	if !containsAny("Connection Refused", "connection refused") {
		t.Error("Expected match for 'connection refused'")
	}
	if containsAny("Hello World", "missing") {
		t.Error("Expected no match for 'missing'")
	}
}

func TestToLower(t *testing.T) {
	if s := toLower("Hello"); s != "hello" {
		t.Errorf("Expected 'hello', got '%s'", s)
	}
	if s := toLower("ALLCAPS"); s != "allcaps" {
		t.Errorf("Expected 'allcaps', got '%s'", s)
	}
}

func TestContains(t *testing.T) {
	if !contains("hello world", "world") {
		t.Error("Expected to find 'world'")
	}
	if contains("hello", "world") {
		t.Error("Expected not to find 'world' in 'hello'")
	}
	// Empty substring
	if !contains("anything", "") {
		t.Error("Empty substring should match")
	}
}

func TestIsNaNFunction(t *testing.T) {
	if !isNaN(math.NaN()) {
		t.Error("Expected NaN to be detected")
	}
	if isNaN(1.0) {
		t.Error("Expected 1.0 to not be NaN")
	}
}

func TestIsInfFunction(t *testing.T) {
	if !isInf(math.Inf(1)) {
		t.Error("Expected +Inf to be detected")
	}
	if !isInf(math.Inf(-1)) {
		t.Error("Expected -Inf to be detected")
	}
	if isInf(1.0) {
		t.Error("Expected 1.0 to not be Inf")
	}
}
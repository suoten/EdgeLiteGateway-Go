package api

import (
	"fmt"
	"time"

	"github.com/labstack/echo/v4"
)

// selfTestCheck is one named diagnostic result shared by device and system
// self-test endpoints.
type selfTestCheck struct {
	Category   string `json:"category"`
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	DurationMs int64  `json:"duration_ms"`
	Message    string `json:"message"`
}

// runSelfTestCheck executes one check and records its own latency.
func runSelfTestCheck(category, name string, run func() (bool, string)) selfTestCheck {
	started := time.Now()
	passed, msg := run()
	return selfTestCheck{
		Category:   category,
		Name:       name,
		Passed:     passed,
		DurationMs: time.Since(started).Milliseconds(),
		Message:    msg,
	}
}

// handleDeviceSelfTest runs lightweight collection diagnostics for one device
// using live scheduler state.
func handleDeviceSelfTest(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	started := time.Now()

	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	checks := make([]selfTestCheck, 0, 4)

	checks = append(checks, runSelfTestCheck("environment", "device_registered", func() (bool, string) {
		return true, fmt.Sprintf("protocol=%s status=%s", device.Protocol, device.Status)
	}))

	// Scheduler diagnostics: nil-safe at every level so a half-booted gateway
	// still produces a report instead of a panic.
	var snap map[string]interface{}
	if cont.Scheduler != nil {
		if s, ok := cont.Scheduler.GetDeviceStatus(deviceID); ok {
			snap = s
		}
	}

	checks = append(checks, runSelfTestCheck("connection", "collector_running", func() (bool, string) {
		if snap == nil {
			return false, "no collect task for this device"
		}
		enabled, _ := snap["enabled"].(bool)
		if !enabled {
			return false, "collect task is disabled"
		}
		interval, _ := snap["collect_interval"].(int)
		return true, fmt.Sprintf("collect interval %ds", interval)
	}))

	checks = append(checks, runSelfTestCheck("readTest", "recent_data", func() (bool, string) {
		if snap == nil {
			return false, "no collect task for this device"
		}
		last, _ := snap["last_collect_at"].(string)
		if last == "" {
			return false, "no data collected yet"
		}
		return true, "last collect at " + last
	}))

	checks = append(checks, runSelfTestCheck("connection", "no_errors", func() (bool, string) {
		if snap == nil {
			return false, "no collect task for this device"
		}
		errs, _ := snap["consecutive_errors"].(int)
		if errs == 0 {
			return true, "no consecutive errors"
		}
		lastErr, _ := snap["last_error"].(string)
		return false, fmt.Sprintf("%d consecutive errors, last: %s", errs, lastErr)
	}))

	failed := 0
	for _, ck := range checks {
		if !ck.Passed {
			failed++
		}
	}
	status := "healthy"
	if failed > 0 {
		status = "unhealthy"
	}
	summary := "all checks passed"
	if failed > 0 {
		summary = fmt.Sprintf("%d of %d checks failed", failed, len(checks))
	}

	details := map[string]interface{}{
		"last_collect_at":    "",
		"consecutive_errors": 0,
		"last_error":         "",
		"success_count":      int64(0),
		"fail_count":         int64(0),
		"latency_ms":         int64(0),
	}
	if snap != nil {
		details["last_collect_at"] = snap["last_collect_at"]
		details["consecutive_errors"] = snap["consecutive_errors"]
		details["last_error"] = snap["last_error"]
		details["success_count"] = snap["success_count"]
		details["fail_count"] = snap["fail_count"]
		details["latency_ms"] = snap["latency_ms"]
	}

	return OK(c, map[string]interface{}{
		"device_id":   deviceID,
		"status":      status,
		"passed":      failed == 0,
		"message":     summary,
		"duration":    time.Since(started).Milliseconds(),
		"duration_ms": time.Since(started).Milliseconds(),
		"timestamp":   time.Now().Format(time.RFC3339),
		"checks":      checks,
		"details":     details,
	})
}

// handleSystemSelfTest runs component diagnostics for the whole gateway.
func handleSystemSelfTest(c echo.Context) error {
	cont := GetContainer()
	started := time.Now()

	tests := make([]selfTestCheck, 0, 7)

	tests = append(tests, runSelfTestCheck("environment", "database", func() (bool, string) {
		if cont.Database == nil {
			return false, "database not initialized"
		}
		if err := cont.Database.HealthCheck(); err != nil {
			return false, "database ping failed: " + err.Error()
		}
		return true, "sqlite ready (" + cont.Database.Path() + ")"
	}))

	tests = append(tests, runSelfTestCheck("environment", "ts_storage", func() (bool, string) {
		if cont.TsStorage == nil {
			return false, "time-series storage not initialized"
		}
		if !cont.TsStorage.CheckHealth() {
			return false, "time-series storage unhealthy"
		}
		if cont.TsStorage.IsUsingFallback() {
			return true, "healthy (sqlite fallback)"
		}
		return true, "healthy"
	}))

	tests = append(tests, runSelfTestCheck("observability", "scheduler", func() (bool, string) {
		if cont.Scheduler == nil {
			return false, "collect scheduler not initialized"
		}
		stats := cont.Scheduler.Stats()
		active, _ := stats["active_collectors"].(int)
		return true, fmt.Sprintf("running, %d active collectors", active)
	}))

	tests = append(tests, runSelfTestCheck("observability", "event_bus", func() (bool, string) {
		if cont.EventBus == nil {
			return false, "event bus not initialized"
		}
		if !cont.EventBus.IsStarted() {
			return false, "event bus not started"
		}
		return true, "running"
	}))

	tests = append(tests, runSelfTestCheck("connection", "mqtt_forwarder", func() (bool, string) {
		if cont.MqttForward == nil {
			return true, "not enabled"
		}
		if !cont.MqttForward.IsConfigured() {
			return true, "no broker configured, skipped"
		}
		if cont.MqttForward.IsConnected() {
			return true, "connected"
		}
		return false, "broker configured but not connected"
	}))

	tests = append(tests, runSelfTestCheck("connection", "websocket", func() (bool, string) {
		if cont.WSManager == nil {
			return false, "websocket manager not initialized"
		}
		stats := cont.WSManager.GetStats()
		clients := 0
		if v, ok := stats["total_connections"].(int); ok {
			clients = v
		}
		return true, fmt.Sprintf("running, %d live connections", clients)
	}))

	tests = append(tests, runSelfTestCheck("observability", "rule_evaluator", func() (bool, string) {
		if cont.Evaluator == nil {
			return false, "rule evaluator not initialized"
		}
		return true, "ready"
	}))

	failed := 0
	for _, tc := range tests {
		if !tc.Passed {
			failed++
		}
	}

	return OK(c, map[string]interface{}{
		"passed":       failed == 0,
		"total":        len(tests),
		"passed_count": len(tests) - failed,
		"failed_count": failed,
		"duration":     time.Since(started).Milliseconds(),
		"timestamp":    time.Now().Format(time.RFC3339),
		"version":      gatewayVersion,
		"tests":        tests,
	})
}

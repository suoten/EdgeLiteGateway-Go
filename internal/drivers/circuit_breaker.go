package drivers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// CircuitBreakerConfig holds configuration for a circuit breaker.
type CircuitBreakerConfig struct {
	FailureThreshold  int           // Number of consecutive failures before opening
	RecoveryTimeout   time.Duration // How long to wait before trying half-open
	HalfOpenMaxCalls  int           // Max allowed calls in half-open state
}

// DefaultCircuitBreakerConfig returns default configuration.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:  5,
		RecoveryTimeout:   30 * time.Second,
		HalfOpenMaxCalls:  3,
	}
}

// CircuitBreaker implements a device-level circuit breaker.
type CircuitBreaker struct {
	mu               sync.Mutex
	states           map[string]CircuitState
	openSince        map[string]*time.Time
	halfOpenCalls    map[string]int
	config           CircuitBreakerConfig
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		states:        make(map[string]CircuitState),
		openSince:     make(map[string]*time.Time),
		halfOpenCalls: make(map[string]int),
		config:        config,
	}
}

// AllowRequest checks if the circuit breaker allows a request for the given device.
func (cb *CircuitBreaker) AllowRequest(deviceID string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state, ok := cb.states[deviceID]
	if !ok {
		state = CircuitClosed
		cb.states[deviceID] = state
	}
	switch state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		openSince := cb.openSince[deviceID]
		if openSince == nil {
			return true
		}
		elapsed := time.Since(*openSince)
		if elapsed >= cb.config.RecoveryTimeout {
			cb.states[deviceID] = CircuitHalfOpen
			cb.halfOpenCalls[deviceID] = 1
			logrus.WithField("device_id", deviceID).Info("Circuit breaker transitioned to half_open")
			return true
		}
		return false
	case CircuitHalfOpen:
		calls := cb.halfOpenCalls[deviceID]
		if calls < cb.config.HalfOpenMaxCalls {
			cb.halfOpenCalls[deviceID] = calls + 1
			return true
		}
		return false
	default:
		return true
	}
}

// RecordSuccess records a successful call for the device.
func (cb *CircuitBreaker) RecordSuccess(deviceID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state, ok := cb.states[deviceID]
	if !ok {
		return
	}
	if state == CircuitHalfOpen {
		cb.states[deviceID] = CircuitClosed
		cb.openSince[deviceID] = nil
		logrus.WithField("device_id", deviceID).Info("Circuit breaker closed after successful call")
	}
}

// RecordFailure records a failed call for the device.
func (cb *CircuitBreaker) RecordFailure(deviceID string, consecutiveFailures int64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state, ok := cb.states[deviceID]
	if !ok {
		state = CircuitClosed
		cb.states[deviceID] = state
	}
	if state == CircuitHalfOpen {
		cb.states[deviceID] = CircuitOpen
		now := time.Now()
		cb.openSince[deviceID] = &now
		logrus.WithField("device_id", deviceID).Warn("Circuit breaker reopened from half_open")
		return
	}
	if consecutiveFailures >= int64(cb.config.FailureThreshold) {
		cb.states[deviceID] = CircuitOpen
		now := time.Now()
		cb.openSince[deviceID] = &now
		logrus.WithFields(logrus.Fields{
			"device_id":           deviceID,
			"failure_threshold":   cb.config.FailureThreshold,
		}).Warn("Circuit breaker opened")
	}
}

// GetState returns the circuit breaker state for a device.
func (cb *CircuitBreaker) GetState(deviceID string) CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state, ok := cb.states[deviceID]
	if !ok {
		return CircuitClosed
	}
	return state
}

// Reset resets the circuit breaker for a device.
func (cb *CircuitBreaker) Reset(deviceID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.states, deviceID)
	delete(cb.openSince, deviceID)
	delete(cb.halfOpenCalls, deviceID)
}

// ReconnectManager manages device reconnection with exponential backoff.
type ReconnectManager struct {
	mu           sync.Mutex
	states       map[string]*reconnectState
	baseDelay    time.Duration
	maxDelay     time.Duration
	maxAttempts  int
	jitterFactor float64
}

type reconnectState struct {
	attempt int
}

// NewReconnectManager creates a new ReconnectManager.
func NewReconnectManager() *ReconnectManager {
	return &ReconnectManager{
		states:       make(map[string]*reconnectState),
		baseDelay:    5 * time.Second,
		maxDelay:     60 * time.Second,
		maxAttempts:  3,
		jitterFactor: 0.5,
	}
}

// ReconnectWithBackoff attempts to reconnect with exponential backoff.
// It calls the provided reconnectFn and returns whether it succeeded.
func (rm *ReconnectManager) ReconnectWithBackoff(ctx context.Context, deviceID string, reconnectFn func() (bool, error)) (bool, error) {
	rm.mu.Lock()
	state, ok := rm.states[deviceID]
	if !ok {
		state = &reconnectState{attempt: 0}
		rm.states[deviceID] = state
	}

	if state.attempt >= rm.maxAttempts {
		rm.mu.Unlock()
		logrus.WithFields(logrus.Fields{
			"device_id":    deviceID,
			"max_attempts": rm.maxAttempts,
		}).Warn("Max reconnect attempts reached, marking offline")
		return false, nil
	}

	attempt := state.attempt
	// Calculate delay: min(base * 2^attempt, maxDelay)
	delay := float64(rm.baseDelay) * math.Pow(2, float64(attempt))
	if delay > float64(rm.maxDelay) {
		delay = float64(rm.maxDelay)
	}
	// Add jitter
	jitterRange := delay * rm.jitterFactor
	delay = delay - jitterRange + rand.Float64()*jitterRange*2
	if delay < 0 {
		delay = 0
	}

	state.attempt = attempt + 1
	rm.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"device_id": deviceID,
		"attempt":   attempt,
		"delay":     delay / float64(time.Second),
	}).Info("Reconnect with backoff")

	// Wait outside the lock
	select {
	case <-time.After(time.Duration(delay)):
	case <-ctx.Done():
		return false, ctx.Err()
	}

	success, err := reconnectFn()
	if err != nil {
		logrus.WithError(err).WithField("device_id", deviceID).Error("Reconnect with backoff exception")
		return false, err
	}
	if success {
		rm.mu.Lock()
		if s, ok := rm.states[deviceID]; ok {
			s.attempt = 0
		}
		rm.mu.Unlock()
		return true, nil
	}
	return false, nil
}

// ResetReconnectState resets the reconnect state for a device.
func (rm *ReconnectManager) ResetReconnectState(deviceID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	delete(rm.states, deviceID)
}

// GetReconnectAttempts returns the number of reconnect attempts for a device.
func (rm *ReconnectManager) GetReconnectAttempts(deviceID string) int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if state, ok := rm.states[deviceID]; ok {
		return state.attempt
	}
	return 0
}

// DriverExceptionMapper maps driver exceptions to standardized error codes.
type DriverExceptionMapper struct{}

// MapException maps an error to a standardized error code.
func (DriverExceptionMapper) MapException(err error, protocol string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case isConnectionRefused(err):
		return "ERR_NETWORK_CONNECTION_REFUSED"
	case isConnectionReset(err):
		return "ERR_NETWORK_CONNECTION_REFUSED"
	case isTimeout(err):
		return "ERR_NETWORK_TIMEOUT"
	case isNetworkUnreachable(err):
		return "ERR_NETWORK_HOST_UNREACHABLE"
	case isDNSFailed(msg):
		return "ERR_NETWORK_DNS_FAILED"
	case isPermissionDenied(err):
		return "ERR_AUTH_PERMISSION_DENIED"
	case isNotImplemented(err):
		return "ERR_DRIVER_NOT_FOUND"
	case isConfigInvalid(err):
		return "ERR_DEVICE_CONFIG_INVALID"
	default:
		return "ERR_COMMON_INTERNAL_ERROR"
	}
}

// Helper functions for error classification
func isConnectionRefused(err error) bool {
	return containsAny(err.Error(), "connection refused", "connect: connection refused")
}

func isConnectionReset(err error) bool {
	return containsAny(err.Error(), "connection reset", "broken pipe", "connection aborted")
}

func isTimeout(err error) bool {
	return containsAny(err.Error(), "timeout", "i/o timeout", "deadline exceeded")
}

func isNetworkUnreachable(err error) bool {
	return containsAny(err.Error(), "network is unreachable", "no route to host", "host is unreachable")
}

func isDNSFailed(msg string) bool {
	return containsAny(msg, "dns", "no such host", "lookup")
}

func isPermissionDenied(err error) bool {
	return containsAny(err.Error(), "permission denied", "access denied")
}

func isNotImplemented(err error) bool {
	return containsAny(err.Error(), "not implemented", "not supported")
}

func isConfigInvalid(err error) bool {
	return containsAny(err.Error(), "config", "invalid", "missing required")
}

func containsAny(s string, substrs ...string) bool {
	sLower := toLower(s)
	for _, sub := range substrs {
		if contains(sLower, toLower(sub)) {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result[i] = c
	}
	return string(result)
}

func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// DeadbandFilter applies a deadband filter to determine if a value should be reported.
type DeadbandFilter struct {
	DeadbandType      string  // "absolute" or "percent"
	DeadbandThreshold float64 // threshold value
}

// Apply checks if the new value should pass the deadband filter.
// Returns the value to report (either new value or last value if filtered).
func (df *DeadbandFilter) Apply(newValue, lastValue interface{}) interface{} {
	if lastValue == nil || newValue == nil {
		return newValue
	}
	newFloat, ok1 := toFloat64(newValue)
	lastFloat, ok2 := toFloat64(lastValue)
	if !ok1 || !ok2 {
		return newValue
	}
	var threshold float64
	switch df.DeadbandType {
	case "percent":
		if df.DeadbandThreshold <= 0 {
			return newValue
		}
		if lastFloat == 0 {
			return newValue
		}
		threshold = abs(lastFloat) * (df.DeadbandThreshold / 100.0)
	default:
		threshold = df.DeadbandThreshold
	}
	if abs(newFloat-lastFloat) < threshold {
		return lastValue
	}
	return newValue
}

// ScalingConfig holds linear scaling configuration.
type ScalingConfig struct {
	Ratio  float64
	Offset float64
}

// Apply applies linear scaling to a value.
func (sc *ScalingConfig) Apply(value interface{}) interface{} {
	if value == nil {
		return value
	}
	f, ok := toFloat64(value)
	if !ok {
		return value
	}
	return f*sc.Ratio + sc.Offset
}

// ClampConfig holds min/max clamping configuration.
type ClampConfig struct {
	Min *float64
	Max *float64
}

// Apply applies clamping to a value. Returns (clamped_value, in_range).
func (cc *ClampConfig) Apply(value interface{}) (interface{}, bool) {
	if value == nil {
		return value, true
	}
	f, ok := toFloat64(value)
	if !ok {
		return value, true
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return value, false
	}
	if cc.Min != nil && f < *cc.Min {
		return *cc.Min, false
	}
	if cc.Max != nil && f > *cc.Max {
		return *cc.Max, false
	}
	return value, true
}

// WritePolicy defines the write policy for a device.
type WritePolicy struct {
	WriteEnabled      bool
	WhitelistMode     bool
	WhitelistedPoints []string
}

// DefaultWritePolicy returns a default write policy.
func DefaultWritePolicy() WritePolicy {
	return WritePolicy{
		WriteEnabled:      true,
		WhitelistMode:     false,
		WhitelistedPoints: []string{},
	}
}

// CheckWriteAllowed checks if writing is allowed for a point.
func (wp *WritePolicy) CheckWriteAllowed(point string, canWrite bool) bool {
	if !canWrite {
		return false
	}
	if !wp.WriteEnabled {
		return false
	}
	if wp.WhitelistMode {
		for _, p := range wp.WhitelistedPoints {
			if p == point {
				return true
			}
		}
		return false
	}
	return true
}

// toFloat64 converts an interface{} to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// RateLimiter implements a simple rate limiter for write operations.
type RateLimiter struct {
	mu         sync.Mutex
	lastTimes  map[string]time.Time
	minInterval time.Duration
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter(minIntervalMs float64) *RateLimiter {
	return &RateLimiter{
		lastTimes:  make(map[string]time.Time),
		minInterval: time.Duration(minIntervalMs * float64(time.Millisecond)),
	}
}

// Allow checks if an operation is allowed and updates the last time.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	last, ok := rl.lastTimes[key]
	if ok && now.Sub(last) < rl.minInterval {
		return false
	}
	rl.lastTimes[key] = now
	return true
}

// ConfigValidator provides basic configuration validation.
type ConfigValidator struct {
	RequiredFields []string
}

// Validate performs basic configuration validation.
func (cv *ConfigValidator) Validate(config map[string]interface{}) *ConfigValidationResult {
	result := &ConfigValidationResult{
		Valid:    true,
		Errors:   []string{},
		Warnings: []string{},
	}
	for _, field := range cv.RequiredFields {
		if _, ok := config[field]; !ok {
			result.Errors = append(result.Errors, fmt.Sprintf("Missing required field: %s", field))
		}
	}
	if len(result.Errors) > 0 {
		result.Valid = false
	}
	return result
}

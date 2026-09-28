package engine

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
)

// CircuitBreakerState represents the state of a circuit breaker.
type CircuitBreakerState string

const (
	CBStateClosed   CircuitBreakerState = "closed"
	CBStateOpen     CircuitBreakerState = "open"
	CBStateHalfOpen CircuitBreakerState = "half_open"
)

// CircuitBreaker implements the circuit breaker pattern for device connections.
// When failures exceed the threshold, the circuit opens and blocks requests.
// After a cooldown period, it transitions to half-open and allows probe requests.
type CircuitBreaker struct {
	mu              sync.Mutex
	state           CircuitBreakerState
	failureCount    int
	successCount    int
	requestCount    int
	errorCount      int
	lastFailureTime time.Time
	openedAt        time.Time

	// Configuration
	failureThreshold         int
	errorRateThreshold       float64
	windowSeconds            float64
	initialOpenDuration      float64
	maxOpenDuration          float64
	multiplier               float64
	halfOpenProbeInterval    float64
	halfOpenSuccessThreshold int

	// Current open duration (exponential backoff)
	currentOpenDuration float64

	// Event bus for state change notifications
	eventBus *EventBus
	deviceID string
}

// NewCircuitBreaker creates a new CircuitBreaker for a device.
func NewCircuitBreaker(deviceID string, eventBus *EventBus) *CircuitBreaker {
	return &CircuitBreaker{
		state:                    CBStateClosed,
		failureThreshold:         constants.CBFailureThreshold,
		errorRateThreshold:       constants.CBErrorRateThreshold,
		windowSeconds:            constants.CBErrorRateWindowSeconds,
		initialOpenDuration:      constants.CBInitialOpenDuration,
		maxOpenDuration:          constants.CBMaxOpenDuration,
		multiplier:               constants.CBOpenDurationMultiplier,
		halfOpenProbeInterval:    constants.CBHalfOpenProbeInterval,
		halfOpenSuccessThreshold: constants.CBHalfOpenSuccessThreshold,
		currentOpenDuration:      constants.CBInitialOpenDuration,
		eventBus:                 eventBus,
		deviceID:                 deviceID,
	}
}

// AllowRequest checks if a request should be allowed through the circuit breaker.
// Returns true if the request is allowed, false if blocked.
func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CBStateClosed:
		return true
	case CBStateOpen:
		// Check if cooldown period has elapsed
		elapsed := time.Since(cb.openedAt).Seconds()
		if elapsed >= cb.currentOpenDuration {
			cb.transitionTo(CBStateHalfOpen)
			return true
		}
		return false
	case CBStateHalfOpen:
		// Allow probe requests
		return true
	default:
		return true
	}
}

// RecordSuccess records a successful request.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.requestCount++
	cb.successCount++

	switch cb.state {
	case CBStateHalfOpen:
		if cb.successCount >= cb.halfOpenSuccessThreshold {
			cb.transitionTo(CBStateClosed)
		}
	case CBStateClosed:
		// Reset failure count on success
		cb.failureCount = 0
	}
}

// RecordFailure records a failed request.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.requestCount++
	cb.errorCount++
	cb.failureCount++
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case CBStateHalfOpen:
		// A failure in half-open immediately reopens the circuit
		cb.transitionTo(CBStateOpen)
	case CBStateClosed:
		// Check if we should open the circuit
		if cb.shouldOpen() {
			cb.transitionTo(CBStateOpen)
		}
	}
}

// shouldOpen determines if the circuit should open based on failures and error rate.
func (cb *CircuitBreaker) shouldOpen() bool {
	// Check absolute failure count
	if cb.failureCount >= cb.failureThreshold {
		return true
	}
	// Check error rate within the window
	if cb.requestCount > 0 {
		errorRate := float64(cb.errorCount) / float64(cb.requestCount)
		if errorRate >= cb.errorRateThreshold && cb.requestCount >= cb.failureThreshold {
			return true
		}
	}
	return false
}

// transitionTo changes the circuit breaker state and publishes an event.
func (cb *CircuitBreaker) transitionTo(newState CircuitBreakerState) {
	oldState := cb.state
	cb.state = newState

	switch newState {
	case CBStateClosed:
		cb.failureCount = 0
		cb.successCount = 0
		cb.requestCount = 0
		cb.errorCount = 0
		cb.currentOpenDuration = cb.initialOpenDuration
	case CBStateOpen:
		cb.successCount = 0
		cb.openedAt = time.Now()
		// Exponential backoff with cap
		cb.currentOpenDuration = math.Min(
			cb.currentOpenDuration*cb.multiplier,
			cb.maxOpenDuration,
		)
	case CBStateHalfOpen:
		cb.successCount = 0
		cb.failureCount = 0
	}

	logrus.WithField("device_id", cb.deviceID).
		WithField("old_state", oldState).
		WithField("new_state", newState).
		Warn("Circuit breaker state change")

	if cb.eventBus != nil {
		cb.eventBus.Publish(Event{
			Type:     EventTypeCircuitBreaker,
			Source:   "circuit_breaker",
			DeviceID: cb.deviceID,
			Data: map[string]interface{}{
				"old_state":     string(oldState),
				"new_state":     string(newState),
				"failure_count": cb.failureCount,
				"error_count":   cb.errorCount,
			},
		})
	}
}

// GetState returns the current state.
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// GetStats returns statistics.
func (cb *CircuitBreaker) GetStats() map[string]interface{} {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return map[string]interface{}{
		"state":                 string(cb.state),
		"failure_count":         cb.failureCount,
		"success_count":         cb.successCount,
		"request_count":         cb.requestCount,
		"error_count":           cb.errorCount,
		"current_open_duration": cb.currentOpenDuration,
		"device_id":             cb.deviceID,
	}
}

// Reset resets the circuit breaker to closed state.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.transitionTo(CBStateClosed)
}

// String returns a human-readable description.
func (cb *CircuitBreaker) String() string {
	return fmt.Sprintf("CircuitBreaker{device=%s, state=%s, failures=%d}",
		cb.deviceID, cb.state, cb.failureCount)
}

// CircuitBreakerRegistry manages circuit breakers per device.
type CircuitBreakerRegistry struct {
	mu       sync.RWMutex
	breakers map[string]*CircuitBreaker
	eventBus *EventBus
}

// NewCircuitBreakerRegistry creates a new registry.
func NewCircuitBreakerRegistry(eventBus *EventBus) *CircuitBreakerRegistry {
	return &CircuitBreakerRegistry{
		breakers: make(map[string]*CircuitBreaker),
		eventBus: eventBus,
	}
}

// Get returns or creates a circuit breaker for a device.
func (r *CircuitBreakerRegistry) Get(deviceID string) *CircuitBreaker {
	r.mu.RLock()
	if cb, ok := r.breakers[deviceID]; ok {
		r.mu.RUnlock()
		return cb
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock
	if cb, ok := r.breakers[deviceID]; ok {
		return cb
	}
	cb := NewCircuitBreaker(deviceID, r.eventBus)
	r.breakers[deviceID] = cb
	return cb
}

// Remove removes a circuit breaker.
func (r *CircuitBreakerRegistry) Remove(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breakers, deviceID)
}

// GetAll returns all circuit breakers.
func (r *CircuitBreakerRegistry) GetAll() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		result[k] = v
	}
	return result
}

// GetStats returns statistics for all circuit breakers.
func (r *CircuitBreakerRegistry) GetStats() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	breakers := make(map[string]interface{}, len(r.breakers))
	openCount := 0
	for deviceID, cb := range r.breakers {
		stats := cb.GetStats()
		breakers[deviceID] = stats
		if stats["state"] == string(CBStateOpen) {
			openCount++
		}
	}
	return map[string]interface{}{
		"total":      len(r.breakers),
		"open_count": openCount,
		"breakers":   breakers,
	}
}

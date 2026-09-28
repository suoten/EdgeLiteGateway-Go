package engine

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
)

// BackpressureLevel represents the severity of backpressure.
type BackpressureLevel string

const (
	BPLevelNormal   BackpressureLevel = "normal"
	BPLevelWarning  BackpressureLevel = "warning"
	BPLevelDanger   BackpressureLevel = "danger"
	BPLevelCritical BackpressureLevel = "critical"
)

// BackpressureController monitors system resource usage and applies backpressure
// when the system is overloaded. It uses a token bucket algorithm to rate-limit
// incoming requests and prevents cascading failures.
type BackpressureController struct {
	mu            sync.Mutex
	level         BackpressureLevel
	previousLevel BackpressureLevel

	// Token bucket
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time

	// Thresholds
	thresholdWarning  float64
	thresholdDanger   float64
	thresholdCritical float64
	resumeThreshold   float64
	resumeHoldSeconds float64

	// History
	history        []bpEvent
	historyMaxSize int

	// Resume hold timer
	resumeAt time.Time

	// Callbacks
	onLevelChange func(oldLevel, newLevel BackpressureLevel)
	eventBus      *EventBus

	// Statistics
	totalRequests int64
	totalBlocked  int64
	totalSlowDown int64
}

// bpEvent represents a backpressure level change event.
type bpEvent struct {
	timestamp time.Time
	level     BackpressureLevel
	queueLen  int
	queueCap  int
}

// NewBackpressureController creates a new BackpressureController.
func NewBackpressureController(eventBus *EventBus) *BackpressureController {
	return &BackpressureController{
		level:             BPLevelNormal,
		previousLevel:     BPLevelNormal,
		tokens:            constants.BPTokenBucketBurst,
		maxTokens:         constants.BPTokenBucketBurst,
		refillRate:        constants.BPTokenBucketRate,
		lastRefill:        time.Now(),
		thresholdWarning:  constants.BPThresholdWarning,
		thresholdDanger:   constants.BPThresholdDanger,
		thresholdCritical: constants.BPThresholdCritical,
		resumeThreshold:   constants.BPResumeThreshold,
		resumeHoldSeconds: constants.BPResumeHoldSeconds,
		historyMaxSize:    constants.BPHistoryMaxEvents,
		eventBus:          eventBus,
	}
}

// SetCallbacks sets callbacks for level changes.
func (b *BackpressureController) SetOnLevelChange(fn func(oldLevel, newLevel BackpressureLevel)) {
	b.mu.Lock()
	b.onLevelChange = fn
	b.mu.Unlock()
}

// AllowRequest checks if a request should be allowed based on current backpressure.
// Uses the token bucket algorithm: if no tokens are available, the request is blocked.
func (b *BackpressureController) AllowRequest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.totalRequests++

	// Refill tokens
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	// Check resume hold
	if b.level != BPLevelNormal && now.Before(b.resumeAt) {
		// Still in hold period, check if tokens available
		if b.tokens < 1 {
			b.totalBlocked++
			return false
		}
		b.tokens--
		return true
	}

	// Consume a token
	if b.tokens < 1 {
		b.totalBlocked++
		return false
	}
	b.tokens--

	// Apply slow-down factor based on level
	switch b.level {
	case BPLevelWarning:
		// 80% throughput
		if b.totalRequests%10 < 8 {
			return true
		}
		b.totalSlowDown++
		b.totalBlocked++
		return false
	case BPLevelDanger:
		// 50% throughput
		if b.totalRequests%10 < 5 {
			return true
		}
		b.totalSlowDown++
		b.totalBlocked++
		return false
	case BPLevelCritical:
		// 10% throughput (probes only)
		if b.totalRequests%10 < 1 {
			return true
		}
		b.totalSlowDown++
		b.totalBlocked++
		return false
	default:
		return true
	}
}

// UpdateQueueStatus updates the backpressure level based on queue utilization.
// queueLen is the current queue length, queueCap is the queue capacity.
func (b *BackpressureController) UpdateQueueStatus(queueLen, queueCap int) {
	if queueCap <= 0 {
		return
	}
	utilization := float64(queueLen) / float64(queueCap)
	b.setLevel(utilization, queueLen, queueCap)
}

// setLevel updates the backpressure level based on utilization.
func (b *BackpressureController) setLevel(utilization float64, queueLen, queueCap int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var newLevel BackpressureLevel
	switch {
	case utilization >= b.thresholdCritical:
		newLevel = BPLevelCritical
	case utilization >= b.thresholdDanger:
		newLevel = BPLevelDanger
	case utilization >= b.thresholdWarning:
		newLevel = BPLevelWarning
	default:
		newLevel = BPLevelNormal
	}

	// Hysteresis: don't resume to normal immediately
	if newLevel == BPLevelNormal && b.level != BPLevelNormal {
		// Check if we should resume
		if utilization < b.resumeThreshold {
			// Start resume hold
			if b.resumeAt.IsZero() {
				b.resumeAt = time.Now().Add(time.Duration(b.resumeHoldSeconds) * time.Second)
			}
			if time.Now().Before(b.resumeAt) {
				newLevel = b.level // Stay at current level
			} else {
				b.resumeAt = time.Time{} // Clear hold
			}
		} else {
			newLevel = b.level // Stay at current level
		}
	} else if newLevel != BPLevelNormal {
		b.resumeAt = time.Time{} // Reset hold timer
	}

	if newLevel != b.level {
		oldLevel := b.level
		b.previousLevel = oldLevel
		b.level = newLevel

		// Record history
		b.history = append(b.history, bpEvent{
			timestamp: time.Now(),
			level:     newLevel,
			queueLen:  queueLen,
			queueCap:  queueCap,
		})
		if len(b.history) > b.historyMaxSize {
			b.history = b.history[1:]
		}

		logrus.WithField("old_level", oldLevel).
			WithField("new_level", newLevel).
			WithField("utilization", utilization).
			Warn("Backpressure level changed")

		// Publish event
		if b.eventBus != nil {
			b.eventBus.Publish(Event{
				Type:   EventTypeBackpressure,
				Source: "backpressure_controller",
				Data: map[string]interface{}{
					"old_level":   string(oldLevel),
					"new_level":   string(newLevel),
					"utilization": utilization,
					"queue_len":   queueLen,
					"queue_cap":   queueCap,
				},
			})
		}

		// Call callback
		if b.onLevelChange != nil {
			b.onLevelChange(oldLevel, newLevel)
		}
	}
}

// GetLevel returns the current backpressure level.
func (b *BackpressureController) GetLevel() BackpressureLevel {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.level
}

// GetStats returns backpressure statistics.
func (b *BackpressureController) GetStats() map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]interface{}{
		"level":           string(b.level),
		"previous_level":  string(b.previousLevel),
		"tokens":          b.tokens,
		"max_tokens":      b.maxTokens,
		"total_requests":  b.totalRequests,
		"total_blocked":   b.totalBlocked,
		"total_slow_down": b.totalSlowDown,
		"history_count":   len(b.history),
	}
}

// GetHistory returns the backpressure event history.
func (b *BackpressureController) GetHistory() []bpEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]bpEvent, len(b.history))
	copy(result, b.history)
	return result
}

// Reset resets the backpressure controller to normal.
func (b *BackpressureController) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.level = BPLevelNormal
	b.previousLevel = BPLevelNormal
	b.tokens = b.maxTokens
	b.resumeAt = time.Time{}
	b.history = b.history[:0]
}

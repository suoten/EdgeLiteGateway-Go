package drivers

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// LRUCache is a thread-safe LRU cache implementation.
// It limits the size of point value caches and other dictionaries to prevent unbounded growth.
type LRUCache struct {
	mu      sync.Mutex
	maxSize int
	data    map[interface{}]*list.Element
	order   *list.List
}

type lruEntry struct {
	key   interface{}
	value interface{}
}

// NewLRUCache creates a new LRU cache with the specified max size.
func NewLRUCache(maxSize int) *LRUCache {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &LRUCache{
		maxSize: maxSize,
		data:    make(map[interface{}]*list.Element, maxSize),
		order:   list.New(),
	}
}

// Get retrieves a value from the cache, updating access order.
func (c *LRUCache) Get(key interface{}) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.data[key]; ok {
		c.order.MoveToBack(elem)
		return elem.Value.(*lruEntry).value, true
	}
	return nil, false
}

// Set sets a value in the cache, evicting the oldest entry if over capacity.
func (c *LRUCache) Set(key, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.data[key]; ok {
		c.order.MoveToBack(elem)
		elem.Value.(*lruEntry).value = value
		return
	}
	entry := &lruEntry{key: key, value: value}
	elem := c.order.PushBack(entry)
	c.data[key] = elem
	if c.order.Len() > c.maxSize {
		oldest := c.order.Front()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.data, oldest.Value.(*lruEntry).key)
		}
	}
}

// Pop removes and returns a value from the cache.
func (c *LRUCache) Pop(key interface{}) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.data[key]; ok {
		c.order.Remove(elem)
		delete(c.data, key)
		return elem.Value.(*lruEntry).value, true
	}
	return nil, false
}

// Clear clears the cache.
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = make(map[interface{}]*list.Element, c.maxSize)
	c.order = list.New()
}

// Len returns the number of entries in the cache.
func (c *LRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Keys returns all keys in the cache.
func (c *LRUCache) Keys() []interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]interface{}, 0, len(c.data))
	for k := range c.data {
		keys = append(keys, k)
	}
	return keys
}

// DriverWatchdog monitors driver health and handles exceptions.
type DriverWatchdog struct {
	mu               sync.Mutex
	exceptionHistory []watchdogException
	maxHistory       int
	stats            *HealthStatsManager
	circuitBreaker   *CircuitBreaker
}

type watchdogException struct {
	timestamp time.Time
	errType   string
	message   string
}

// NewDriverWatchdog creates a new DriverWatchdog.
func NewDriverWatchdog(stats *HealthStatsManager, cb *CircuitBreaker) *DriverWatchdog {
	return &DriverWatchdog{
		maxHistory:     60,
		stats:          stats,
		circuitBreaker: cb,
	}
}

// HandleException handles an exception in the watchdog loop.
// Returns true if the exception was handled (should continue), false if should stop.
func (w *DriverWatchdog) HandleException(err error, ctx string) bool {
	if err == nil {
		return true
	}

	// Check for context cancellation
	if err == context.Canceled || isContextCanceled(err) {
		logrus.WithField("context", ctx).Info("Watchdog cancelled normally")
		return false
	}

	w.mu.Lock()
	w.exceptionHistory = append(w.exceptionHistory, watchdogException{
		timestamp: time.Now(),
		errType:   getErrorType(err),
		message:   err.Error(),
	})
	if len(w.exceptionHistory) > w.maxHistory {
		w.exceptionHistory = w.exceptionHistory[1:]
	}
	recentCount := len(w.exceptionHistory)
	w.mu.Unlock()

	// Known connection errors
	if isConnectionRefused(err) || isConnectionReset(err) || isTimeout(err) || isNetworkUnreachable(err) {
		if recentCount >= 10 {
			logrus.WithFields(logrus.Fields{
				"context": ctx,
				"count":   recentCount,
				"error":   err,
			}).Error("Too many connection errors, driver may be unhealthy")
		} else {
			logrus.WithFields(logrus.Fields{
				"context": ctx,
				"count":   recentCount,
				"error":   err,
			}).Warn("Connection error in watchdog")
		}
		return true
	}

	// Unknown exceptions
	if recentCount >= 10 {
		logrus.WithFields(logrus.Fields{
			"context": ctx,
			"count":   recentCount,
			"error":   err,
		}).Error("Too many exceptions, driver may be unhealthy")
	} else {
		logrus.WithFields(logrus.Fields{
			"context": ctx,
			"count":   recentCount,
			"error":   err,
		}).Error("Unexpected error in watchdog")
	}
	return true
}

// GetRecentExceptions returns recent exceptions.
func (w *DriverWatchdog) GetRecentExceptions() []watchdogException {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]watchdogException, len(w.exceptionHistory))
	copy(result, w.exceptionHistory)
	return result
}

// getErrorType returns a string representation of the error type.
func getErrorType(err error) string {
	if err == nil {
		return "nil"
	}
	// Simple type detection based on message patterns
	switch {
	case isConnectionRefused(err):
		return "ConnectionRefused"
	case isConnectionReset(err):
		return "ConnectionReset"
	case isTimeout(err):
		return "Timeout"
	case isNetworkUnreachable(err):
		return "NetworkUnreachable"
	default:
		return "Unknown"
	}
}

// isContextCanceled checks if the error is a context cancellation.
func isContextCanceled(err error) bool {
	return containsAny(err.Error(), "context canceled", "context deadline exceeded")
}

// PointHealthTracker tracks per-point health statistics.
type PointHealthTracker struct {
	mu                sync.Mutex
	totalReads        map[string]int64     // "device:point" -> count
	failedReads       map[string]int64     // "device:point" -> count
	latencySamples    map[string][]float64 // "device:point" -> samples
	maxLatencySamples int
	lastValues        *LRUCache            // "device:point" -> last value
	frozenThresholds  map[string]float64   // "device:point" -> frozen threshold
	lastUpdateTimes   map[string]time.Time // "device:point" -> last update time
}

// NewPointHealthTracker creates a new PointHealthTracker.
func NewPointHealthTracker() *PointHealthTracker {
	return &PointHealthTracker{
		totalReads:        make(map[string]int64),
		failedReads:       make(map[string]int64),
		latencySamples:    make(map[string][]float64),
		maxLatencySamples: 100,
		lastValues:        NewLRUCache(10000),
		frozenThresholds:  make(map[string]float64),
		lastUpdateTimes:   make(map[string]time.Time),
	}
}

// RecordSuccess records a successful point read.
func (p *PointHealthTracker) RecordSuccess(deviceID, pointName string, latencyMs float64) {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalReads[key]++
	samples := p.latencySamples[key]
	samples = append(samples, latencyMs)
	if len(samples) > p.maxLatencySamples {
		samples = samples[1:]
	}
	p.latencySamples[key] = samples
	p.lastUpdateTimes[key] = time.Now()
}

// RecordFailure records a failed point read.
func (p *PointHealthTracker) RecordFailure(deviceID, pointName string) {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalReads[key]++
	p.failedReads[key]++
}

// GetSuccessRate returns the success rate for a point.
func (p *PointHealthTracker) GetSuccessRate(deviceID, pointName string) float64 {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	total := p.totalReads[key]
	if total == 0 {
		return 1.0
	}
	failed := p.failedReads[key]
	return 1.0 - float64(failed)/float64(total)
}

// GetAvgLatency returns the average latency for a point.
func (p *PointHealthTracker) GetAvgLatency(deviceID, pointName string) float64 {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	samples := p.latencySamples[key]
	if len(samples) == 0 {
		return 0.0
	}
	sum := 0.0
	for _, s := range samples {
		sum += s
	}
	return sum / float64(len(samples))
}

// CheckFrozen checks if a point value is frozen (not changing).
func (p *PointHealthTracker) CheckFrozen(deviceID, pointName string, newValue float64) bool {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	if lastUpdate, ok := p.lastUpdateTimes[key]; ok {
		threshold := p.frozenThresholds[key]
		if threshold > 0 && time.Since(lastUpdate).Seconds() > threshold {
			// Check if value has changed
			if lastVal, ok := p.lastValues.Get(key); ok {
				if lastFloat, ok := toFloat64(lastVal); ok && lastFloat == newValue {
					return true // Value is frozen
				}
			}
		}
	}
	p.lastValues.Set(key, newValue)
	p.lastUpdateTimes[key] = time.Now()
	return false
}

// CheckRateOfChange checks if the rate of change exceeds the threshold.
func (p *PointHealthTracker) CheckRateOfChange(deviceID, pointName string, newValue, maxRatePerSecond float64) bool {
	key := deviceID + ":" + pointName
	p.mu.Lock()
	defer p.mu.Unlock()
	if lastUpdate, ok := p.lastUpdateTimes[key]; ok {
		if lastVal, ok := p.lastValues.Get(key); ok {
			if lastFloat, ok := toFloat64(lastVal); ok {
				elapsed := time.Since(lastUpdate).Seconds()
				if elapsed > 0 {
					rate := abs(newValue-lastFloat) / elapsed
					if rate > maxRatePerSecond {
						return true // Rate of change exceeded
					}
				}
			}
		}
	}
	p.lastValues.Set(key, newValue)
	p.lastUpdateTimes[key] = time.Now()
	return false
}

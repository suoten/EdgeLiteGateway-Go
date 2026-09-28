package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/constants"
	"edgelite/internal/storage"
)

// CollectScheduler manages periodic data collection from devices.
// Each device has its own ticker goroutine that calls the registered collect function.
// The scheduler includes a watchdog that detects stalled collectors and restarts them.
type CollectScheduler struct {
	mu             sync.RWMutex
	collectors      map[string]*collectorEntry
	eventBus       *EventBus
	tsStorage      *storage.TimeSeriesStorage
	cache          *storage.CacheManager
	maxConcurrent  int
	watchdogTicker *time.Ticker
	watchdogStop   chan struct{}
	started        atomic.Bool

	// preprocessor cleans each collected batch before anything downstream reads
	// it. It is set from the assembly point because it is built after the
	// scheduler and shared with the API handlers that edit the rules.
	preprocessor *Preprocessor

	// expressions derives additional points from the ones a batch collected.
	expressions *PreprocessorExpressionEngine

	// Statistics
	muStats   sync.Mutex
	totalCollects int64
	totalErrors   int64
	totalPoints   int64
}

// collectorEntry holds the state of a single device collector.
type collectorEntry struct {
	entryMu         sync.Mutex
	deviceID        string
	collectInterval int
	collectFunc     CollectFunc
	cancel          context.CancelFunc
	lastCollectAt   time.Time
	lastError       string
	consecutiveErr  int
	successCount    int64
	failCount       int64
	lastLatencyMs   int64
	enabled         bool
}

// snapshotEntry safely reads all mutable fields of an entry under entryMu.
// Returns local copies that can be used without holding the lock.
type entrySnapshot struct {
	deviceID        string
	collectInterval int
	enabled         bool
	lastCollectAt   time.Time
	lastError       string
	consecutiveErr  int
	successCount    int64
	failCount       int64
	lastLatencyMs   int64
}

func (e *collectorEntry) snapshot() entrySnapshot {
	e.entryMu.Lock()
	defer e.entryMu.Unlock()
	return entrySnapshot{
		deviceID:        e.deviceID,
		collectInterval: e.collectInterval,
		enabled:         e.enabled,
		lastCollectAt:   e.lastCollectAt,
		lastError:       e.lastError,
		consecutiveErr:  e.consecutiveErr,
		successCount:    e.successCount,
		failCount:       e.failCount,
		lastLatencyMs:   e.lastLatencyMs,
	}
}

// CollectFunc is the function called to collect data from a device.
// It returns a slice of PointData or an error.
type CollectFunc func(ctx context.Context, deviceID string) ([]storage.PointData, error)

// NewCollectScheduler creates a new CollectScheduler.
func NewCollectScheduler(eventBus *EventBus, tsStorage *storage.TimeSeriesStorage, cache *storage.CacheManager, cfg *config.SchedulerConfig) *CollectScheduler {
	maxConc := cfg.MaxConcurrentCollects
	if maxConc <= 0 {
		maxConc = 50
	}
	return &CollectScheduler{
		collectors:     make(map[string]*collectorEntry),
		eventBus:       eventBus,
		tsStorage:      tsStorage,
		cache:          cache,
		maxConcurrent:  maxConc,
		watchdogStop:   make(chan struct{}, 1),
	}
}

// SetPreprocessor installs the edge preprocessor whose rules clean every batch
// this scheduler collects. Call it before Start: collectors run on their own
// goroutines and read the field without locking.
func (s *CollectScheduler) SetPreprocessor(p *Preprocessor) {
	s.preprocessor = p
}

// SetExpressionEngine installs the derived-point expressions. Like
// SetPreprocessor it must be called before Start.
func (s *CollectScheduler) SetExpressionEngine(e *PreprocessorExpressionEngine) {
	s.expressions = e
}

// Preprocessor returns the installed preprocessor, or nil when the collect path
// has none.
func (s *CollectScheduler) Preprocessor() *Preprocessor {
	return s.preprocessor
}

// RegisterCollector registers a data collection function for a device.
// If a collector already exists for this device, it is replaced.
func (s *CollectScheduler) RegisterCollector(deviceID string, intervalSeconds int, fn CollectFunc) {
	s.mu.Lock()

	// Stop existing collector — capture cancel before releasing lock to avoid
	// calling cancel while still holding s.mu, which could deadlock if the
	// goroutine's exit path tries to acquire s.mu.
	var oldCancel context.CancelFunc
	if existing, ok := s.collectors[deviceID]; ok {
		existing.entryMu.Lock()
		existing.enabled = false
		oldCancel = existing.cancel
		existing.cancel = nil
		existing.entryMu.Unlock()
	}
	s.mu.Unlock()

	// Cancel old collector outside s.mu to prevent deadlock
	if oldCancel != nil {
		oldCancel()
	}

	entry := &collectorEntry{
		deviceID:        deviceID,
		collectInterval: intervalSeconds,
		collectFunc:     fn,
		enabled:         true,
	}

	s.mu.Lock()
	s.collectors[deviceID] = entry
	started := s.started.Load()
	if started {
		ctx, cancel := context.WithCancel(context.Background())
		entry.entryMu.Lock()
		entry.cancel = cancel
		entry.entryMu.Unlock()
		go s.collectLoop(ctx, entry)
	}
	s.mu.Unlock()

	logrus.WithField("device_id", deviceID).
		WithField("interval", intervalSeconds).
		Debug("Collector registered")
}

// UnregisterCollector removes a device's collector.
func (s *CollectScheduler) UnregisterCollector(deviceID string) {
	s.mu.Lock()
	var oldCancel context.CancelFunc
	if entry, ok := s.collectors[deviceID]; ok {
		entry.entryMu.Lock()
		entry.enabled = false
		oldCancel = entry.cancel
		entry.cancel = nil
		entry.entryMu.Unlock()
		delete(s.collectors, deviceID)
	}
	s.mu.Unlock()

	// Cancel outside s.mu to prevent deadlock
	if oldCancel != nil {
		oldCancel()
	}
}

// StartCollector starts collection for a specific device (if registered and stopped).
func (s *CollectScheduler) StartCollector(deviceID string) {
	s.mu.Lock()
	entry, ok := s.collectors[deviceID]
	if !ok || !s.started.Load() {
		s.mu.Unlock()
		return
	}
	entry.entryMu.Lock()
	if entry.enabled {
		entry.entryMu.Unlock()
		s.mu.Unlock()
		return
	}
	entry.enabled = true
	entry.entryMu.Unlock()
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	entry.entryMu.Lock()
	entry.cancel = cancel
	entry.entryMu.Unlock()
	go s.collectLoop(ctx, entry)
}

// StopCollector stops collection for a specific device.
func (s *CollectScheduler) StopCollector(deviceID string) {
	s.mu.Lock()
	entry, ok := s.collectors[deviceID]
	if !ok {
		s.mu.Unlock()
		return
	}
	entry.entryMu.Lock()
	entry.enabled = false
	cancel := entry.cancel
	entry.cancel = nil
	entry.entryMu.Unlock()
	s.mu.Unlock()

	// Cancel outside s.mu to prevent deadlock
	if cancel != nil {
		cancel()
	}
}

// IsCollecting reports whether a device currently has an active collector.
func (s *CollectScheduler) IsCollecting(deviceID string) bool {
	s.mu.Lock()
	entry, ok := s.collectors[deviceID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	entry.entryMu.Lock()
	enabled := entry.enabled
	entry.entryMu.Unlock()
	s.mu.Unlock()
	return enabled
}

// Start begins the scheduler and watchdog.
func (s *CollectScheduler) Start(ctx context.Context) {
	if s.started.Swap(true) {
		return
	}

	// Start all registered collectors
	s.mu.Lock()
	for _, entry := range s.collectors {
		entry.entryMu.Lock()
		entry.enabled = true
		entry.entryMu.Unlock()
		collectCtx, cancel := context.WithCancel(ctx)
		entry.entryMu.Lock()
		entry.cancel = cancel
		entry.entryMu.Unlock()
		go s.collectLoop(collectCtx, entry)
	}
	s.mu.Unlock()

	// Start watchdog
	interval := time.Duration(constants.SchedulerInterval) * time.Second
	s.watchdogTicker = time.NewTicker(interval)
	go s.watchdogLoop(ctx)

	logrus.Info("CollectScheduler started")
}

// Stop gracefully stops all collectors and the watchdog.
func (s *CollectScheduler) Stop() {
	if !s.started.Swap(false) {
		return
	}

	// Stop watchdog
	if s.watchdogTicker != nil {
		s.watchdogTicker.Stop()
	}
	select {
	case s.watchdogStop <- struct{}{}:
	default:
	}

	// Stop all collectors — capture cancel funcs first, then cancel outside s.mu
	var cancels []context.CancelFunc
	s.mu.Lock()
	for _, entry := range s.collectors {
		entry.entryMu.Lock()
		entry.enabled = false
		c := entry.cancel
		entry.cancel = nil
		entry.entryMu.Unlock()
		cancels = append(cancels, c)
	}
	s.mu.Unlock()

	for _, cancel := range cancels {
		if cancel != nil {
			cancel()
		}
	}

	logrus.Info("CollectScheduler stopped")
}

// collectLoop runs the periodic collection for a single device.
func (s *CollectScheduler) collectLoop(ctx context.Context, entry *collectorEntry) {
	snap := entry.snapshot()
	interval := time.Duration(snap.collectInterval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.doCollect(ctx, entry)
		}
	}
}

// doCollect performs a single collection cycle for a device.
func (s *CollectScheduler) doCollect(ctx context.Context, entry *collectorEntry) {
	startTime := time.Now()
	snap := entry.snapshot()

	points, err := entry.collectFunc(ctx, snap.deviceID)

	entry.entryMu.Lock()
	entry.lastCollectAt = startTime
	entry.lastLatencyMs = time.Since(startTime).Milliseconds()
	if err != nil {
		entry.lastError = err.Error()
		entry.consecutiveErr++
		entry.failCount++
	} else {
		entry.lastError = ""
		entry.consecutiveErr = 0
		entry.successCount++
	}
	entry.entryMu.Unlock()

	s.muStats.Lock()
	s.totalCollects++
	if err != nil {
		s.totalErrors++
	} else {
		s.totalPoints += int64(len(points))
	}
	s.muStats.Unlock()

	if err != nil {
		snap = entry.snapshot()
		logrus.WithField("device_id", snap.deviceID).
			WithField("error", err.Error()).
			WithField("consecutive_errors", snap.consecutiveErr).
			Warn("Collection failed")
		s.eventBus.Publish(Event{
			Type:     EventTypeDeviceError,
			Source:   "collect_scheduler",
			DeviceID: snap.deviceID,
			Data:     map[string]interface{}{"error": err.Error(), "consecutive_errors": snap.consecutiveErr},
		})
		return
	}

	if len(points) == 0 {
		return
	}

	// Edge preprocessing runs before anything consumes the batch, so the cache,
	// the stored history and every EventTypeDataCollected subscriber (rules,
	// MQTT forwarder, AI) all see the same filtered values the operator asked
	// for. The preprocessor used to be built and registered nowhere, so the
	// preprocess rules and config.preprocess section filtered nothing.
	if s.preprocessor != nil {
		points = s.preprocessor.Process(snap.deviceID, points)
		if len(points) == 0 {
			return
		}
	}

	// Derived points come after filtering so they are computed from the values the
	// operator chose to keep, and are pushed alongside the collected ones so the
	// cache, history and subscribers all see them the same way.
	if s.expressions != nil {
		vals := make(map[string]interface{}, len(points))
		for _, p := range points {
			vals[p.PointName] = p.Value
		}
		points = append(points, s.expressions.Evaluate(snap.deviceID, vals)...)
	}

	// Push to cache
	for _, p := range points {
		s.cache.Push(p)
	}

	// Write to time-series storage (non-blocking)
	if s.tsStorage != nil {
		if err := s.tsStorage.WritePoints(points); err != nil {
			logrus.WithField("device_id", snap.deviceID).
				WithField("error", err.Error()).
				Warn("Failed to write points to time-series storage")
		}
	}

	// Publish data collected event
	s.eventBus.Publish(Event{
		Type:     EventTypeDataCollected,
		Source:   "collect_scheduler",
		DeviceID: snap.deviceID,
		Data: DataCollectedEvent{
			DeviceID: snap.deviceID,
			Points:   points,
			Source:   "collect_scheduler",
		},
	})
}

// watchdogLoop monitors collectors for stalls and restarts them.
func (s *CollectScheduler) watchdogLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.watchdogStop:
			return
		case <-s.watchdogTicker.C:
			s.watchdogCheck()
		}
	}
}

// watchdogCheck checks for stalled collectors.
func (s *CollectScheduler) watchdogCheck() {
	staleCycles := constants.SchedulerInterval
	restartCycles := constants.SchedulerInterval * 3

	now := time.Now()

	// Snapshot collector entries under RLock to avoid holding s.mu during event publishing.
	s.mu.RLock()
	entries := make([]*collectorEntry, 0, len(s.collectors))
	for _, entry := range s.collectors {
		entries = append(entries, entry)
	}
	s.mu.RUnlock()

	for _, entry := range entries {
		snap := entry.snapshot()
		if !snap.enabled {
			continue
		}
		if snap.lastCollectAt.IsZero() {
			continue // Not started yet
		}
		sinceLast := now.Sub(snap.lastCollectAt).Seconds()
		if sinceLast > float64(staleCycles) && snap.consecutiveErr > 0 {
			logrus.WithField("device_id", snap.deviceID).
				WithField("seconds_since_last", sinceLast).
				Warn("Collector appears stalled")
		}
		if sinceLast > float64(restartCycles) {
			logrus.WithField("device_id", snap.deviceID).
				Error("Collector severely stalled, may need restart")
			s.eventBus.Publish(Event{
				Type:     EventTypeDeviceError,
				Source:   "watchdog",
				DeviceID: snap.deviceID,
				Data:     map[string]interface{}{"reason": "collector_stalled", "seconds_since_last": sinceLast},
			})
		}
	}
}

// GetDeviceStatus returns the collection status for a device.
func (s *CollectScheduler) GetDeviceStatus(deviceID string) (map[string]interface{}, bool) {
	s.mu.RLock()
	entry, ok := s.collectors[deviceID]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	snap := entry.snapshot()
	return map[string]interface{}{
		"device_id":          snap.deviceID,
		"enabled":            snap.enabled,
		"collect_interval":   snap.collectInterval,
		"last_collect_at":    snap.lastCollectAt.Format(time.RFC3339),
		"last_error":         snap.lastError,
		"consecutive_errors": snap.consecutiveErr,
		"success_count":      snap.successCount,
		"fail_count":         snap.failCount,
		"latency_ms":         snap.lastLatencyMs,
	}, true
}

// ResetDeviceStats zeroes the health counters (success/fail/error/latency)
// of a device's collector. Returns false when no collector exists.
func (s *CollectScheduler) ResetDeviceStats(deviceID string) bool {
	s.mu.RLock()
	entry, ok := s.collectors[deviceID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	entry.entryMu.Lock()
	defer entry.entryMu.Unlock()
	entry.lastError = ""
	entry.consecutiveErr = 0
	entry.successCount = 0
	entry.failCount = 0
	entry.lastLatencyMs = 0
	return true
}

// GetAllStatus returns the status of all collectors.
func (s *CollectScheduler) GetAllStatus() []map[string]interface{} {
	s.mu.RLock()
	entries := make([]*collectorEntry, 0, len(s.collectors))
	for _, entry := range s.collectors {
		entries = append(entries, entry)
	}
	s.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		snap := entry.snapshot()
		result = append(result, map[string]interface{}{
			"device_id":          snap.deviceID,
			"enabled":            snap.enabled,
			"collect_interval":   snap.collectInterval,
			"last_collect_at":    snap.lastCollectAt.Format(time.RFC3339),
			"last_error":         snap.lastError,
			"consecutive_errors": snap.consecutiveErr,
		})
	}
	return result
}

// GetDriverStats returns per-device collection counters (success/fail/latency)
// for the metrics view.
func (s *CollectScheduler) GetDriverStats() []map[string]interface{} {
	s.mu.RLock()
	entries := make([]*collectorEntry, 0, len(s.collectors))
	for _, entry := range s.collectors {
		entries = append(entries, entry)
	}
	s.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		snap := entry.snapshot()
		result = append(result, map[string]interface{}{
			"device_id":       snap.deviceID,
			"enabled":         snap.enabled,
			"success_count":   snap.successCount,
			"fail_count":      snap.failCount,
			"last_latency_ms": snap.lastLatencyMs,
		})
	}
	return result
}

// Stats returns scheduler statistics.
func (s *CollectScheduler) Stats() map[string]interface{} {
	// Lock ordering: acquire mu before muStats to prevent AB-BA deadlock.
	s.mu.RLock()
	collectorCount := len(s.collectors)
	s.mu.RUnlock()

	s.muStats.Lock()
	defer s.muStats.Unlock()
	return map[string]interface{}{
		"total_collects": s.totalCollects,
		"total_errors":   s.totalErrors,
		"total_points":   s.totalPoints,
		"active_collectors": collectorCount,
		"started":         s.started.Load(),
	}
}



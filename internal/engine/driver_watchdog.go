package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// DriverWatchdog monitors driver health and restarts stalled drivers.
// This is a Go port of the Python edgelite/engine/driver_watchdog.py.
//
// Features:
//   - Periodic health checks for all drivers
//   - Automatic restart of stalled drivers
//   - Circuit breaker integration
//   - Configurable check intervals and thresholds

const (
	watchdogCheckInterval    = 30 * time.Second
	watchdogStallThreshold   = 60  // seconds without data
	watchdogRestartThreshold = 180 // seconds, force restart
)

// DriverHealth represents the health status of a driver.
type DriverHealth struct {
	DeviceID          string    `json:"device_id"`
	Protocol          string    `json:"protocol"`
	Status            string    `json:"status"` // healthy, degraded, stalled, restarted
	LastDataAt        time.Time `json:"last_data_at"`
	LastCheckAt       time.Time `json:"last_check_at"`
	ConsecutiveStalls int       `json:"consecutive_stalls"`
	RestartCount      int       `json:"restart_count"`
}

// DriverWatchdog monitors driver health.
type DriverWatchdog struct {
	mu          sync.RWMutex
	health      map[string]*DriverHealth
	restartFunc func(deviceID string) error
	started     bool
	cancelFunc  context.CancelFunc
}

// NewDriverWatchdog creates a new DriverWatchdog.
func NewDriverWatchdog(restartFunc func(deviceID string) error) *DriverWatchdog {
	return &DriverWatchdog{
		health:      make(map[string]*DriverHealth),
		restartFunc: restartFunc,
	}
}

// RegisterDriver registers a driver for monitoring.
func (w *DriverWatchdog) RegisterDriver(deviceID, protocol string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.health[deviceID] = &DriverHealth{
		DeviceID: deviceID,
		Protocol: protocol,
		Status:   "healthy",
	}
}

// UnregisterDriver removes a driver from monitoring.
func (w *DriverWatchdog) UnregisterDriver(deviceID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.health, deviceID)
}

// RecordData records that data was received from a driver.
func (w *DriverWatchdog) RecordData(deviceID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h, ok := w.health[deviceID]; ok {
		h.LastDataAt = time.Now()
		h.Status = "healthy"
		h.ConsecutiveStalls = 0
	}
}

// Start begins the watchdog monitoring loop.
func (w *DriverWatchdog) Start(ctx context.Context) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()

	childCtx, cancel := context.WithCancel(ctx)
	w.cancelFunc = cancel

	go w.monitorLoop(childCtx)
	logrus.Info("DriverWatchdog started")
}

// Stop stops the watchdog.
func (w *DriverWatchdog) Stop() {
	w.mu.Lock()
	w.started = false
	w.mu.Unlock()

	if w.cancelFunc != nil {
		w.cancelFunc()
		w.cancelFunc = nil
	}
	logrus.Info("DriverWatchdog stopped")
}

func (w *DriverWatchdog) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(watchdogCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkAll()
		}
	}
}

func (w *DriverWatchdog) checkAll() {
	w.mu.RLock()
	defer w.mu.RUnlock()

	now := time.Now()
	for _, h := range w.health {
		h.LastCheckAt = now

		if h.LastDataAt.IsZero() {
			continue // Not started yet
		}

		sinceData := now.Sub(h.LastDataAt).Seconds()
		if sinceData > float64(watchdogRestartThreshold) {
			h.Status = "stalled"
			h.ConsecutiveStalls++
			logrus.WithFields(logrus.Fields{
				"device_id":          h.DeviceID,
				"protocol":           h.Protocol,
				"seconds_since_data": sinceData,
			}).Error("Driver severely stalled, attempting restart")
			w.restartDriver(h)
		} else if sinceData > float64(watchdogStallThreshold) {
			h.Status = "degraded"
			logrus.WithFields(logrus.Fields{
				"device_id":          h.DeviceID,
				"protocol":           h.Protocol,
				"seconds_since_data": sinceData,
			}).Warn("Driver appears degraded (no recent data)")
		} else {
			h.Status = "healthy"
		}
	}
}

func (w *DriverWatchdog) restartDriver(h *DriverHealth) {
	if w.restartFunc == nil {
		return
	}

	err := w.restartFunc(h.DeviceID)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"device_id": h.DeviceID,
			"error":     err.Error(),
		}).Error("Driver restart failed")
		return
	}

	h.RestartCount++
	h.LastDataAt = time.Now()
	h.Status = "restarted"
	logrus.WithFields(logrus.Fields{
		"device_id":     h.DeviceID,
		"restart_count": h.RestartCount,
	}).Info("Driver restarted successfully")
}

// GetHealth returns the health status of all drivers.
func (w *DriverWatchdog) GetHealth() []DriverHealth {
	w.mu.RLock()
	defer w.mu.RUnlock()
	result := make([]DriverHealth, 0, len(w.health))
	for _, h := range w.health {
		result = append(result, *h)
	}
	return result
}

// GetDeviceHealth returns the health status of a specific driver.
func (w *DriverWatchdog) GetDeviceHealth(deviceID string) (*DriverHealth, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	h, ok := w.health[deviceID]
	if !ok {
		return nil, false
	}
	return h, true
}

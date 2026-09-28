// Package northbound wires the platform handlers (internal/platform) into the
// gateway runtime: it owns platform connection lifecycles, forwards collected
// telemetry / device status events from the EventBus to every connected
// northbound platform, and tracks per-platform publish statistics.
package northbound

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/engine"
	"edgelite/internal/platform"
)

// PlatformStats tracks per-platform runtime statistics.
type PlatformStats struct {
	MessagesSent    int64     `json:"messages_sent"`
	MessagesFailed  int64     `json:"messages_failed"`
	LastActive      time.Time `json:"last_active"`
	ConnectedAt     time.Time `json:"connected_at"`
	LastTestLatency float64   `json:"last_test_latency_ms"`
}

// Manager owns the runtime instances of northbound platform handlers.
type Manager struct {
	mu       sync.RWMutex
	handlers map[string]platform.Handler
	configs  map[string]map[string]interface{}
	stats    map[string]*PlatformStats
	eventBus *engine.EventBus
	unsub    []func()
	started  bool
	stopCh   chan struct{}
	stopOnce sync.Once

	// watchdogInterval controls how often the reconnect watchdog scans for
	// dropped platform connections. Defaults to 15s; tests shrink it.
	watchdogInterval time.Duration
}

// NewManager creates a Manager and registers all built-in platform handlers.
func NewManager(eventBus *engine.EventBus) *Manager {
	platform.RegisterAll()
	return &Manager{
		handlers:         make(map[string]platform.Handler),
		configs:          make(map[string]map[string]interface{}),
		stats:            make(map[string]*PlatformStats),
		eventBus:         eventBus,
		stopCh:           make(chan struct{}),
		watchdogInterval: 15 * time.Second,
	}
}

// Start subscribes the manager to EventBus data and device-status events and
// launches the reconnect watchdog. The watchdog also runs when the event bus
// is unavailable (e.g. unit tests), so connection recovery is always active.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	if m.eventBus != nil {
		m.unsub = append(m.unsub,
			m.eventBus.Subscribe(engine.EventTypeDataCollected, m.onDataCollected),
			m.eventBus.Subscribe(engine.EventTypeDeviceOnline, m.onDeviceStatus(true)),
			m.eventBus.Subscribe(engine.EventTypeDeviceOffline, m.onDeviceStatus(false)),
		)
	}
	go m.watchdog()
	m.mu.Unlock()
	logrus.Info("Northbound platform manager started")
}

// watchdog periodically checks previously-connected platforms and reconnects
// any whose runtime connection dropped unexpectedly (broker restart, network
// blip, keep-alive timeout). Enabled platforms registered via AutoConnect are
// also covered: AutoConnect pre-registers their config so the watchdog keeps
// retrying even after the initial retry budget is exhausted.
func (m *Manager) watchdog() {
	interval := m.watchdogInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}

		type target struct {
			name string
			cfg  map[string]interface{}
		}
		m.mu.RLock()
		targets := make([]target, 0, len(m.configs))
		for name, cfg := range m.configs {
			if enabled, ok := cfg["enabled"].(bool); ok && !enabled {
				continue
			}
			if h, ok := m.handlers[name]; ok && h.IsConnected() {
				continue
			}
			targets = append(targets, target{name: name, cfg: cfg})
		}
		m.mu.RUnlock()

		for _, t := range targets {
			select {
			case <-m.stopCh:
				return
			default:
			}
			if err := m.Connect(t.name, t.cfg); err != nil {
				logrus.WithError(err).WithField("platform", t.name).
					Debug("Watchdog platform reconnect failed, will retry")
				continue
			}
			logrus.WithField("platform", t.name).Info("Watchdog reconnected platform")
		}
	}
}

// Stop disconnects all platforms and unsubscribes from the EventBus.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	handlers := m.handlers
	m.mu.Unlock()

	m.stopOnce.Do(func() { close(m.stopCh) })
	for _, u := range m.unsub {
		u()
	}
	m.unsub = nil
	for name, h := range handlers {
		if err := h.Disconnect(); err != nil {
			logrus.WithError(err).WithField("platform", name).Warn("Failed to disconnect platform on stop")
		}
	}
	logrus.Info("Northbound platform manager stopped")
}

// Connect connects (or reconnects) the platform identified by name using the
// supplied configuration. The platform type is taken from config["type"] when
// present, falling back to the instance name itself (1:1 with Python, where
// instance name == platform type).
func (m *Manager) Connect(name string, cfg map[string]interface{}) error {
	typeName := name
	if cfg != nil {
		if t, ok := cfg["type"].(string); ok && t != "" {
			typeName = t
		}
	}
	handler, err := platform.GetPlatformRegistry().Create(typeName)
	if err != nil {
		return fmt.Errorf("platform %s: %w", name, err)
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := handler.Connect(connectCtx, cfg); err != nil {
		return fmt.Errorf("platform %s connect: %w", name, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Disconnect a previously connected handler for the same platform, if any.
	if old, ok := m.handlers[name]; ok && old != handler {
		_ = old.Disconnect()
	}
	m.handlers[name] = handler
	m.configs[name] = cfg
	if m.stats[name] == nil {
		m.stats[name] = &PlatformStats{}
	}
	m.stats[name].ConnectedAt = time.Now()
	logrus.WithField("platform", name).WithField("type", typeName).Info("Northbound platform connected")
	return nil
}

// Disconnect disconnects the platform and drops its stored runtime state so
// the watchdog does not resurrect an intentionally disconnected platform.
// The persisted config file entry is untouched — the platform can be
// reconnected later via the connect API.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	handler, ok := m.handlers[name]
	if ok {
		delete(m.handlers, name)
	}
	delete(m.configs, name)
	delete(m.stats, name)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if err := handler.Disconnect(); err != nil {
		return err
	}
	logrus.WithField("platform", name).Info("Northbound platform disconnected")
	return nil
}

// TestConnection dials a platform with the given config using a throwaway
// handler instance, returning the round-trip connect latency in ms.
func (m *Manager) TestConnection(name string, cfg map[string]interface{}) (map[string]interface{}, error) {
	typeName := name
	if cfg != nil {
		if t, ok := cfg["type"].(string); ok && t != "" {
			typeName = t
		}
	}
	handler, err := platform.GetPlatformRegistry().Create(typeName)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := handler.Connect(ctx, cfg); err != nil {
		return nil, err
	}
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	_ = handler.Disconnect()
	return map[string]interface{}{
		"platform":   name,
		"success":    true,
		"latency_ms": latencyMs,
		"message":    "Platform connection test successful",
	}, nil
}

// IsConnected reports whether the platform has a live runtime connection.
func (m *Manager) IsConnected(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.handlers[name]
	return ok && h.IsConnected()
}

// ConnectedNames returns the names of all currently connected platforms.
func (m *Manager) ConnectedNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.handlers))
	for name, h := range m.handlers {
		if h.IsConnected() {
			names = append(names, name)
		}
	}
	return names
}

// GetStats returns a copy of the runtime statistics for a platform.
func (m *Manager) GetStats(name string) PlatformStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.stats[name]; ok {
		return *s
	}
	return PlatformStats{}
}

// GetAllStats returns runtime statistics for every tracked platform.
func (m *Manager) GetAllStats() map[string]PlatformStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]PlatformStats, len(m.stats))
	for k, v := range m.stats {
		out[k] = *v
	}
	return out
}

// AutoConnect connects every enabled platform from the config map
// (cfg.Platforms) at startup. Because platform brokers (or the gateway's own
// embedded MQTT server) may not be listening yet during early startup, each
// connection is attempted asynchronously with retries for up to one minute.
func (m *Manager) AutoConnect(platforms map[string]interface{}) {
	for name, p := range platforms {
		cfg, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		enabled, _ := cfg["enabled"].(bool)
		if !enabled {
			continue
		}
		go func(name string, cfg map[string]interface{}) {
			// Pre-register the config so the watchdog keeps retrying even if
			// all initial attempts fail (e.g. the broker comes up later).
			m.mu.Lock()
			m.configs[name] = cfg
			m.mu.Unlock()

			const attempts = 12
			const retryDelay = 5 * time.Second
			for i := 1; i <= attempts; i++ {
				if err := m.Connect(name, cfg); err != nil {
					logrus.WithError(err).WithField("platform", name).
						WithField("attempt", i).Warn("Auto-connect platform attempt failed, retrying")
					time.Sleep(retryDelay)
					continue
				}
				return
			}
			logrus.WithField("platform", name).Error("Auto-connect platform failed after all retries, watchdog will keep trying")
		}(name, cfg)
	}
}

// onDataCollected forwards a collected data batch to every connected platform.
func (m *Manager) onDataCollected(event engine.Event) {
	dce, ok := event.Data.(engine.DataCollectedEvent)
	if !ok || len(dce.Points) == 0 {
		return
	}
	data := make(map[string]interface{}, len(dce.Points))
	for _, pt := range dce.Points {
		data[pt.PointName] = pt.Value
	}
	m.ForwardTelemetry(dce.DeviceID, data)
}

// onDeviceStatus returns a handler that forwards device online/offline events.
func (m *Manager) onDeviceStatus(online bool) engine.EventHandler {
	return func(event engine.Event) {
		dse, ok := event.Data.(engine.DeviceStatusEvent)
		if !ok || dse.DeviceID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for name, h := range m.snapshotHandlers() {
			if !h.IsConnected() {
				continue
			}
			if err := h.PublishDeviceStatus(ctx, dse.DeviceID, online); err != nil {
				logrus.WithError(err).WithField("platform", name).
					WithField("device_id", dse.DeviceID).Debug("Publish device status failed")
			}
		}
	}
}

// ForwardTelemetry publishes one telemetry batch to every connected platform
// and updates per-platform statistics.
func (m *Manager) ForwardTelemetry(deviceID string, data map[string]interface{}) {
	if len(data) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for name, h := range m.snapshotHandlers() {
		if !h.IsConnected() {
			continue
		}
		err := h.PublishTelemetry(ctx, deviceID, data)
		m.mu.Lock()
		s := m.stats[name]
		if s == nil {
			s = &PlatformStats{}
			m.stats[name] = s
		}
		if err != nil {
			s.MessagesFailed++
			logrus.WithError(err).WithField("platform", name).
				WithField("device_id", deviceID).Warn("Publish telemetry to platform failed")
		} else {
			s.MessagesSent++
			s.LastActive = time.Now()
		}
		m.mu.Unlock()
	}
}

// snapshotHandlers returns a copy of the current handler map.
func (m *Manager) snapshotHandlers() map[string]platform.Handler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]platform.Handler, len(m.handlers))
	for name, h := range m.handlers {
		out[name] = h
	}
	return out
}

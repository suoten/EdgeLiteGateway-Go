package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// LifecycleState represents the state of a managed component.
type LifecycleState string

const (
	LifecycleStateCreated   LifecycleState = "created"
	LifecycleStateStarting  LifecycleState = "starting"
	LifecycleStateRunning   LifecycleState = "running"
	LifecycleStateStopping  LifecycleState = "stopping"
	LifecycleStateStopped   LifecycleState = "stopped"
	LifecycleStateError     LifecycleState = "error"
)

// LifecycleComponent is an interface for components managed by the lifecycle manager.
type LifecycleComponent interface {
	Name() string
	Start(ctx context.Context) error
	Stop() error
}

// LifecycleManager manages the lifecycle of all engine components.
// It ensures orderly startup and shutdown, handles signals, and provides health checks.
type LifecycleManager struct {
	mu         sync.RWMutex
	components []LifecycleComponent
	states     map[string]LifecycleState
	startTime  time.Time
	started    atomic.Bool

	// Graceful shutdown timeout
	shutdownTimeout time.Duration

	// Health check callback
	healthCheck func() map[string]interface{}
}

// NewLifecycleManager creates a new LifecycleManager.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		components:      make([]LifecycleComponent, 0),
		states:          make(map[string]LifecycleState),
		shutdownTimeout: 30 * time.Second,
	}
}

// AddComponent registers a component for lifecycle management.
// Components are started in registration order and stopped in reverse order.
func (m *LifecycleManager) AddComponent(c LifecycleComponent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.components = append(m.components, c)
	m.states[c.Name()] = LifecycleStateCreated
}

// SetShutdownTimeout sets the graceful shutdown timeout.
func (m *LifecycleManager) SetShutdownTimeout(d time.Duration) {
	m.shutdownTimeout = d
}

// SetHealthCheck sets the health check callback.
func (m *LifecycleManager) SetHealthCheck(fn func() map[string]interface{}) {
	m.healthCheck = fn
}

// Start starts all registered components in order.
func (m *LifecycleManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started.Load() {
		m.mu.Unlock()
		return fmt.Errorf("lifecycle manager already started")
	}
	m.mu.Unlock()

	logrus.Info("LifecycleManager starting all components...")

	for _, c := range m.components {
		m.mu.Lock()
		m.states[c.Name()] = LifecycleStateStarting
		m.mu.Unlock()

		logrus.Infof("Starting component: %s", c.Name())
		if err := c.Start(ctx); err != nil {
			m.mu.Lock()
			m.states[c.Name()] = LifecycleStateError
			m.mu.Unlock()
			logrus.WithField("component", c.Name()).
				WithField("error", err.Error()).
				Error("Failed to start component")
			// Rollback: stop already-started components
			m.rollbackStart(len(m.components))
			return fmt.Errorf("failed to start %s: %w", c.Name(), err)
		}

		m.mu.Lock()
		m.states[c.Name()] = LifecycleStateRunning
		m.mu.Unlock()
		logrus.Infof("Component started: %s", c.Name())
	}

	m.startTime = time.Now()
	m.started.Store(true)
	logrus.Info("LifecycleManager: all components started successfully")
	return nil
}

// rollbackStart stops components that were already started before a failure.
func (m *LifecycleManager) rollbackStart(upToIndex int) {
	for i := upToIndex - 1; i >= 0; i-- {
		c := m.components[i]
		m.mu.Lock()
		state := m.states[c.Name()]
		m.mu.Unlock()
		if state == LifecycleStateRunning || state == LifecycleStateStarting {
			logrus.Infof("Rolling back component: %s", c.Name())
			if err := c.Stop(); err != nil {
				logrus.WithField("component", c.Name()).
					WithField("error", err.Error()).
					Warn("Error during rollback stop")
			}
			m.mu.Lock()
			m.states[c.Name()] = LifecycleStateStopped
			m.mu.Unlock()
		}
	}
}

// Stop stops all components in reverse order.
// Each component stop is given a bounded timeout; if it exceeds the timeout,
// the goroutine is abandoned (with a warning) to prevent indefinite blocking.
func (m *LifecycleManager) Stop() {
	if !m.started.Swap(false) {
		return
	}

	logrus.Info("LifecycleManager stopping all components...")

	// Stop in reverse order
	for i := len(m.components) - 1; i >= 0; i-- {
		c := m.components[i]
		m.mu.Lock()
		m.states[c.Name()] = LifecycleStateStopping
		m.mu.Unlock()

		logrus.Infof("Stopping component: %s", c.Name())

		// Use a timeout for each stop. The goroutine is given a cancelable
		// context so that callers can signal abandonment. However, since
		// c.Stop() may not be context-aware, we also enforce a hard timeout.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
		done := make(chan error, 1)
		go func() {
			defer func() {
				// Recover from panics in Stop() to avoid crashing the manager
				if r := recover(); r != nil {
					done <- fmt.Errorf("panic in Stop(): %v", r)
				}
				stopCancel() // release context resources
			}()
			done <- c.Stop()
		}()

		select {
		case err := <-done:
			if err != nil {
				logrus.WithField("component", c.Name()).
					WithField("error", err.Error()).
					Warn("Error stopping component")
			}
		case <-stopCtx.Done():
			logrus.WithField("component", c.Name()).
				Error("Timeout stopping component, goroutine abandoned")
			// Note: the goroutine may still be running. It will be leaked if
			// c.Stop() never returns. This is logged for observability.
		}

		m.mu.Lock()
		m.states[c.Name()] = LifecycleStateStopped
		m.mu.Unlock()
		logrus.Infof("Component stopped: %s", c.Name())
	}

	logrus.Info("LifecycleManager: all components stopped")
}

// WaitForSignal blocks until a termination signal is received, then calls Stop.
func (m *LifecycleManager) WaitForSignal() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	logrus.Infof("Received signal: %v, initiating graceful shutdown...", sig)
	m.Stop()
}

// GetStates returns the current state of all components.
func (m *LifecycleManager) GetStates() map[string]LifecycleState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]LifecycleState, len(m.states))
	for k, v := range m.states {
		result[k] = v
	}
	return result
}

// IsRunning returns true if the lifecycle manager is running.
func (m *LifecycleManager) IsRunning() bool {
	return m.started.Load()
}

// Uptime returns the duration since the manager started.
func (m *LifecycleManager) Uptime() time.Duration {
	if !m.started.Load() {
		return 0
	}
	return time.Since(m.startTime)
}

// Health returns a health check report.
func (m *LifecycleManager) Health() map[string]interface{} {
	m.mu.RLock()
	states := make(map[string]string, len(m.states))
	allRunning := true
	for k, v := range m.states {
		states[k] = string(v)
		if v != LifecycleStateRunning {
			allRunning = false
		}
	}
	m.mu.RUnlock()

	result := map[string]interface{}{
		"healthy":  allRunning,
		"running":  m.started.Load(),
		"uptime_s": m.Uptime().Seconds(),
		"components": states,
	}

	if m.healthCheck != nil {
		result["checks"] = m.healthCheck()
	}

	return result
}

// ComponentState is a simple lifecycle component adapter for start/stop functions.
type ComponentState struct {
	name     string
	startFn  func(ctx context.Context) error
	stopFn   func() error
}

// NewComponentState creates a lifecycle component from start/stop functions.
func NewComponentState(name string, startFn func(ctx context.Context) error, stopFn func() error) LifecycleComponent {
	return &ComponentState{
		name:    name,
		startFn: startFn,
		stopFn:  stopFn,
	}
}

func (c *ComponentState) Name() string { return c.name }
func (c *ComponentState) Start(ctx context.Context) error {
	if c.startFn != nil {
		return c.startFn(ctx)
	}
	return nil
}
func (c *ComponentState) Stop() error {
	if c.stopFn != nil {
		return c.stopFn()
	}
	return nil
}

// AdapterComponent wraps an EventBus to implement LifecycleComponent.
type EventBusComponent struct {
	bus     *EventBus
	ctx     context.Context
}

// NewEventBusComponent creates a lifecycle component wrapper for EventBus.
func NewEventBusComponent(bus *EventBus) LifecycleComponent {
	return &EventBusComponent{bus: bus}
}

func (e *EventBusComponent) Name() string { return "event_bus" }
func (e *EventBusComponent) Start(ctx context.Context) error {
	e.ctx = ctx
	e.bus.Start(ctx)
	return nil
}
func (e *EventBusComponent) Stop() error {
	e.bus.Stop()
	return nil
}

// CollectSchedulerComponent wraps a CollectScheduler as a LifecycleComponent.
type CollectSchedulerComponent struct {
	scheduler *CollectScheduler
	ctx       context.Context
}

// NewCollectSchedulerComponent creates a lifecycle component wrapper for CollectScheduler.
func NewCollectSchedulerComponent(scheduler *CollectScheduler) LifecycleComponent {
	return &CollectSchedulerComponent{scheduler: scheduler}
}

func (c *CollectSchedulerComponent) Name() string { return "collect_scheduler" }
func (c *CollectSchedulerComponent) Start(ctx context.Context) error {
	c.ctx = ctx
	c.scheduler.Start(ctx)
	return nil
}
func (c *CollectSchedulerComponent) Stop() error {
	c.scheduler.Stop()
	return nil
}

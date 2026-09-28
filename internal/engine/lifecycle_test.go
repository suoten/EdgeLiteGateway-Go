package engine

import (
	"context"
	"testing"
	"time"
)

// --- LifecycleManager Tests ---

func TestNewLifecycleManager(t *testing.T) {
	m := NewLifecycleManager()
	if m == nil {
		t.Fatal("NewLifecycleManager returned nil")
	}
	if m.IsRunning() {
		t.Fatal("New manager should not be running")
	}
	if m.Uptime() != 0 {
		t.Fatal("Uptime should be 0 for not-started manager")
	}
}

func TestLifecycleManagerAddComponent(t *testing.T) {
	m := NewLifecycleManager()
	comp := NewComponentState("test_component",
		func(ctx context.Context) error { return nil },
		func() error { return nil },
	)
	m.AddComponent(comp)

	states := m.GetStates()
	if _, ok := states["test_component"]; !ok {
		t.Fatal("Component not found in states after AddComponent")
	}
	if states["test_component"] != LifecycleStateCreated {
		t.Fatalf("Expected state 'created', got '%s'", states["test_component"])
	}
}

func TestLifecycleManagerStartStop(t *testing.T) {
	m := NewLifecycleManager()
	started := false
	stopped := false

	comp := NewComponentState("test_component",
		func(ctx context.Context) error {
			started = true
			return nil
		},
		func() error {
			stopped = true
			return nil
		},
	)
	m.AddComponent(comp)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !started {
		t.Fatal("Component was not started")
	}

	if !m.IsRunning() {
		t.Fatal("Manager should be running after Start")
	}

	if m.Uptime() < 0 {
		t.Fatal("Uptime should not be negative")
	}

	m.Stop()

	if !stopped {
		t.Fatal("Component was not stopped")
	}

	if m.IsRunning() {
		t.Fatal("Manager should not be running after Stop")
	}
}

func TestLifecycleManagerStartTwice(t *testing.T) {
	m := NewLifecycleManager()
	m.AddComponent(NewComponentState("comp",
		func(ctx context.Context) error { return nil },
		func() error { return nil },
	))

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("First Start failed: %v", err)
	}

	if err := m.Start(context.Background()); err == nil {
		t.Fatal("Second Start should fail")
	}
}

func TestLifecycleManagerStopWithoutStart(t *testing.T) {
	m := NewLifecycleManager()
	// Should not panic
	m.Stop()
}

func TestLifecycleManagerStartFailure(t *testing.T) {
	m := NewLifecycleManager()

	// First component starts successfully
	m.AddComponent(NewComponentState("comp1",
		func(ctx context.Context) error { return nil },
		func() error { return nil },
	))

	// Second component fails to start
	m.AddComponent(NewComponentState("comp2",
		func(ctx context.Context) error { return context.DeadlineExceeded },
		func() error { return nil },
	))

	err := m.Start(context.Background())
	if err == nil {
		t.Fatal("Start should fail when a component fails")
	}

	states := m.GetStates()
	if states["comp1"] != LifecycleStateStopped {
		t.Fatalf("comp1 should be 'stopped' after rollback, got '%s'", states["comp1"])
	}
	if states["comp2"] != LifecycleStateError {
		t.Fatalf("comp2 should be 'error', got '%s'", states["comp2"])
	}
}

func TestLifecycleManagerHealth(t *testing.T) {
	m := NewLifecycleManager()
	m.AddComponent(NewComponentState("comp",
		func(ctx context.Context) error { return nil },
		func() error { return nil },
	))

	_ = m.Start(context.Background())
	defer m.Stop()

	health := m.Health()
	if health == nil {
		t.Fatal("Health returned nil")
	}

	healthy, ok := health["healthy"].(bool)
	if !ok || !healthy {
		t.Fatal("Expected healthy=true after successful start")
	}
}

func TestLifecycleManagerSetShutdownTimeout(t *testing.T) {
	m := NewLifecycleManager()
	m.SetShutdownTimeout(5 * time.Second)
	// No direct way to verify, but should not panic
}

func TestLifecycleManagerSetHealthCheck(t *testing.T) {
	m := NewLifecycleManager()
	m.SetHealthCheck(func() map[string]interface{} {
		return map[string]interface{}{"custom": "check"}
	})

	_ = m.Start(context.Background())
	defer m.Stop()

	health := m.Health()
	checks, ok := health["checks"].(map[string]interface{})
	if !ok {
		t.Fatal("Health check callback not invoked")
	}
	if checks["custom"] != "check" {
		t.Fatal("Custom health check value mismatch")
	}
}

// --- ComponentState Tests ---

func TestComponentStateName(t *testing.T) {
	c := NewComponentState("my_component", nil, nil)
	if c.Name() != "my_component" {
		t.Fatalf("Expected name 'my_component', got '%s'", c.Name())
	}
}

func TestComponentStateNilFunctions(t *testing.T) {
	c := NewComponentState("nil_comp", nil, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start with nil function should not fail: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop with nil function should not fail: %v", err)
	}
}

// --- EventBusComponent Tests ---

func TestEventBusComponent(t *testing.T) {
	bus := NewEventBus(100)
	comp := NewEventBusComponent(bus)

	if comp.Name() != "event_bus" {
		t.Fatalf("Expected name 'event_bus', got '%s'", comp.Name())
	}

	ctx := context.Background()
	if err := comp.Start(ctx); err != nil {
		t.Fatalf("EventBusComponent Start failed: %v", err)
	}

	if err := comp.Stop(); err != nil {
		t.Fatalf("EventBusComponent Stop failed: %v", err)
	}
}

// --- CollectSchedulerComponent Tests ---

func TestCollectSchedulerComponent(t *testing.T) {
	bus := NewEventBus(100)
	// Note: CollectScheduler needs more deps, but we test the component wrapper
	// We'll skip full integration and just test the name
	comp := NewCollectSchedulerComponent(nil)
	if comp.Name() != "collect_scheduler" {
		t.Fatalf("Expected name 'collect_scheduler', got '%s'", comp.Name())
	}
	_ = bus // keep bus referenced
}

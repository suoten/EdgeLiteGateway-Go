package engine

import (
	"context"
	"testing"
	"time"
)

// --- EventBus Tests ---

func TestNewEventBus(t *testing.T) {
	bus := NewEventBus(100)
	if bus == nil {
		t.Fatal("NewEventBus returned nil")
	}
}

func TestEventBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan Event, 1)
	bus.Subscribe(EventTypeDataCollected, func(event Event) {
		received <- event
	})

	bus.Start(ctx)

	bus.Publish(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev1",
		Timestamp: time.Now(),
	})

	select {
	case event := <-received:
		if event.Type != EventTypeDataCollected {
			t.Fatalf("Expected event type %s, got %s", EventTypeDataCollected, event.Type)
		}
		if event.DeviceID != "dev1" {
			t.Fatalf("Expected device_id 'dev1', got '%s'", event.DeviceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for event")
	}

	bus.Stop()
}

func TestEventBusSubscribeAll(t *testing.T) {
	bus := NewEventBus(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	count := 0
	done := make(chan struct{})
	bus.SubscribeAll(func(event Event) {
		count++
		if count >= 2 {
			close(done)
		}
	})

	bus.Start(ctx)

	bus.Publish(Event{Type: EventTypeDataCollected, Source: "test", Timestamp: time.Now()})
	bus.Publish(Event{Type: EventTypeDeviceOnline, Source: "test", Timestamp: time.Now()})

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for events in SubscribeAll")
	}

	bus.Stop()
}

func TestEventBusMetrics(t *testing.T) {
	bus := NewEventBus(100)
	metrics := bus.Metrics()
	if metrics == nil {
		t.Fatal("Metrics returned nil")
	}
}

func TestEventMarshalJSON(t *testing.T) {
	event := Event{
		Type:      EventTypeDataCollected,
		Source:    "test_source",
		DeviceID:  "dev1",
		PointName: "temperature",
		Data:      42.5,
		Timestamp: time.Now(),
	}

	data, err := event.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("MarshalJSON returned empty data")
	}
}

// --- CircuitBreaker Tests ---

func TestNewCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker("device1", nil)
	if cb == nil {
		t.Fatal("NewCircuitBreaker returned nil")
	}
}

func TestCircuitBreakerClosed(t *testing.T) {
	cb := NewCircuitBreaker("device1", nil)
	if !cb.AllowRequest() {
		t.Fatal("Circuit breaker should allow requests when closed")
	}
}

func TestCircuitBreakerRecordSuccess(t *testing.T) {
	cb := NewCircuitBreaker("device1", nil)
	cb.RecordSuccess()
	if !cb.AllowRequest() {
		t.Fatal("Circuit breaker should remain closed after success")
	}
}

func TestCircuitBreakerStats(t *testing.T) {
	cb := NewCircuitBreaker("device1", nil)
	stats := cb.GetStats()
	if stats == nil {
		t.Fatal("GetStats returned nil")
	}
}

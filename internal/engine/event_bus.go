// Package engine provides the core runtime engine for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/engine/ package.
// It includes the event bus, collect scheduler, rule evaluator, preprocessor,
// lifecycle manager, MQTT forwarder, circuit breaker, backpressure controller,
// AI inference engine, and self-learning model.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/storage"
)

// EventType represents the type of event in the event bus.
type EventType string

const (
	EventTypeDataCollected     EventType = "data_collected"
	EventTypeDataProcessed     EventType = "data_processed"
	EventTypeAlarmTriggered    EventType = "alarm_triggered"
	EventTypeAlarmRecovered    EventType = "alarm_recovered"
	EventTypeAlarmAcknowledged EventType = "alarm_acknowledged"
	EventTypeDeviceOnline      EventType = "device_online"
	EventTypeDeviceOffline     EventType = "device_offline"
	EventTypeDeviceError       EventType = "device_error"
	EventTypeDeviceCreated     EventType = "device_created"
	EventTypeDeviceUpdated     EventType = "device_updated"
	EventTypeDeviceDeleted     EventType = "device_deleted"
	EventTypeRuleCreated       EventType = "rule_created"
	EventTypeRuleUpdated       EventType = "rule_updated"
	EventTypeRuleDeleted       EventType = "rule_deleted"
	EventTypeMQTTConnected     EventType = "mqtt_connected"
	EventTypeMQTTDisconnected  EventType = "mqtt_disconnected"
	EventTypeConfigChanged     EventType = "config_changed"
	EventTypeBackpressure      EventType = "backpressure"
	EventTypeCircuitBreaker    EventType = "circuit_breaker"
	EventTypeAIInference       EventType = "ai_inference"
	EventTypeStreamResult      EventType = "stream_result"
	EventTypeSystemShutdown    EventType = "system_shutdown"
)

// Event is the core event structure passed through the bus.
type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	DeviceID  string                 `json:"device_id,omitempty"`
	PointName string                 `json:"point_name,omitempty"`
	Data      interface{}            `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// MarshalJSON implements custom JSON marshaling.
func (e *Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      EventType              `json:"type"`
		Source    string                 `json:"source"`
		DeviceID  string                 `json:"device_id,omitempty"`
		PointName string                 `json:"point_name,omitempty"`
		Data      interface{}            `json:"data,omitempty"`
		Timestamp string                 `json:"timestamp"`
		Metadata  map[string]interface{} `json:"metadata,omitempty"`
	}{
		Type:      e.Type,
		Source:    e.Source,
		DeviceID:  e.DeviceID,
		PointName: e.PointName,
		Data:      e.Data,
		Timestamp: e.Timestamp.Format(time.RFC3339Nano),
		Metadata:  e.Metadata,
	})
}

// EventHandler is a function that handles an event.
type EventHandler func(event Event)

// EventBus is a publish-subscribe event bus with async dispatch.
// It supports topic-based subscriptions and guarantees at-least-once delivery
// within the queue capacity. When the queue is full, backpressure is applied.
type EventBus struct {
	mu           sync.RWMutex
	subscribers  map[EventType][]EventHandler
	wildcardSubs []EventHandler
	queue        chan Event
	wg           sync.WaitGroup
	ctx          context.Context
	cancel       context.CancelFunc
	started      bool

	// Metrics
	muMetrics  sync.Mutex
	published  int64
	delivered  int64
	dropped    int64
	latencySum float64
	latencyCnt int64

	// Backpressure callback
	onBackpressure func(level string, queueLen, queueCap int)
}

// NewEventBus creates a new EventBus with the specified queue capacity.
func NewEventBus(queueCap int) *EventBus {
	if queueCap <= 0 {
		queueCap = constants.EventBusMaxQueue
	}
	return &EventBus{
		subscribers: make(map[EventType][]EventHandler),
		queue:       make(chan Event, queueCap),
	}
}

// Start begins the event dispatch goroutine.
func (b *EventBus) Start(ctx context.Context) {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.started = true
	b.mu.Unlock()

	b.wg.Add(1)
	go b.dispatchLoop()
	logrus.Info("EventBus started")
}

// Stop gracefully stops the event bus, waiting for in-flight events to complete.
func (b *EventBus) Stop() {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	b.started = false
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()

	b.wg.Wait()
	logrus.Info("EventBus stopped")
}

// Subscribe registers a handler for a specific event type.
// Returns an unsubscribe function.
func (b *EventBus) Subscribe(eventType EventType, handler EventHandler) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[eventType] = append(b.subscribers[eventType], handler)
	idx := len(b.subscribers[eventType]) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if handlers, ok := b.subscribers[eventType]; ok && idx < len(handlers) {
			b.subscribers[eventType] = append(handlers[:idx], handlers[idx+1:]...)
		}
	}
}

// SubscribeAll registers a handler that receives all event types (wildcard).
func (b *EventBus) SubscribeAll(handler EventHandler) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wildcardSubs = append(b.wildcardSubs, handler)
	idx := len(b.wildcardSubs) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if idx < len(b.wildcardSubs) {
			b.wildcardSubs = append(b.wildcardSubs[:idx], b.wildcardSubs[idx+1:]...)
		}
	}
}

// Publish enqueues an event for async dispatch.
// If the queue is full, the event is dropped and the drop counter is incremented.
func (b *EventBus) Publish(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	b.muMetrics.Lock()
	b.published++
	b.muMetrics.Unlock()

	select {
	case b.queue <- event:
	default:
		b.muMetrics.Lock()
		b.dropped++
		queueLen := len(b.queue)
		b.muMetrics.Unlock()
		if b.onBackpressure != nil {
			b.onBackpressure("warning", queueLen, cap(b.queue))
		}
		logrus.WithField("event_type", event.Type).
			Warn("EventBus queue full, dropping event")
	}
}

// PublishSync publishes an event and waits for all handlers to complete.
// This is useful for critical events that must be processed before continuing.
func (b *EventBus) PublishSync(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	b.dispatch(event)
}

// dispatchLoop reads events from the queue and dispatches them to subscribers.
func (b *EventBus) dispatchLoop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.ctx.Done():
			// Drain remaining events
			for {
				select {
				case event := <-b.queue:
					b.dispatch(event)
				default:
					return
				}
			}
		case event := <-b.queue:
			b.dispatch(event)
		}
	}
}

// dispatch sends an event to all matching subscribers.
func (b *EventBus) dispatch(event Event) {
	startTime := time.Now()

	b.mu.RLock()
	handlers := b.subscribers[event.Type]
	wildcard := b.wildcardSubs
	b.mu.RUnlock()

	// Combine specific and wildcard handlers
	allHandlers := make([]EventHandler, 0, len(handlers)+len(wildcard))
	allHandlers = append(allHandlers, handlers...)
	allHandlers = append(allHandlers, wildcard...)

	for _, handler := range allHandlers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logrus.WithField("event_type", event.Type).
						WithField("panic", r).
						Error("Event handler panicked")
				}
			}()
			handler(event)
		}()
	}

	elapsed := time.Since(startTime).Seconds()

	b.muMetrics.Lock()
	b.delivered++
	b.latencySum += elapsed
	b.latencyCnt++
	b.muMetrics.Unlock()
}

// Metrics returns the current event bus metrics.
func (b *EventBus) Metrics() map[string]interface{} {
	b.muMetrics.Lock()
	defer b.muMetrics.Unlock()
	var avgLatency float64
	if b.latencyCnt > 0 {
		avgLatency = b.latencySum / float64(b.latencyCnt)
	}
	return map[string]interface{}{
		"published":      b.published,
		"delivered":      b.delivered,
		"dropped":        b.dropped,
		"queue_length":   len(b.queue),
		"queue_capacity": cap(b.queue),
		"avg_latency_s":  avgLatency,
	}
}

// SetBackpressureCallback sets a callback invoked when the queue is near capacity.
func (b *EventBus) SetBackpressureCallback(cb func(level string, queueLen, queueCap int)) {
	b.mu.Lock()
	b.onBackpressure = cb
	b.mu.Unlock()
}

// QueueLen returns the current queue length.
func (b *EventBus) QueueLen() int {
	return len(b.queue)
}

// QueueCap returns the queue capacity.
func (b *EventBus) QueueCap() int {
	return cap(b.queue)
}

// IsStarted returns whether the bus is running.
func (b *EventBus) IsStarted() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.started
}

// --- Convenience data structures for events ---

// DataCollectedEvent represents a data collection event payload.
type DataCollectedEvent struct {
	DeviceID string              `json:"device_id"`
	Points   []storage.PointData `json:"points"`
	Source   string              `json:"source"`
}

// AlarmEventPayload represents an alarm event payload.
type AlarmEventPayload struct {
	AlarmID  string                 `json:"alarm_id"`
	RuleID   string                 `json:"rule_id"`
	DeviceID string                 `json:"device_id"`
	Severity string                 `json:"severity"`
	Message  string                 `json:"message"`
	Values   map[string]interface{} `json:"values,omitempty"`
}

// DeviceStatusEvent represents a device status change payload.
type DeviceStatusEvent struct {
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

// String returns a human-readable description of the event.
func (e *Event) String() string {
	return fmt.Sprintf("Event{Type=%s, Source=%s, DeviceID=%s, Timestamp=%s}",
		e.Type, e.Source, e.DeviceID, e.Timestamp.Format(time.RFC3339))
}

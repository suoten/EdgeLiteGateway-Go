package ws

import (
	"encoding/json"
	"testing"
	"time"

	"edgelite/internal/engine"
)

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.GetClientCount() != 0 {
		t.Fatalf("Expected 0 clients, got %d", m.GetClientCount())
	}
}

func TestManagerStartStop(t *testing.T) {
	m := NewManager()
	m.Start()
	if !m.started {
		t.Fatal("Manager should be started")
	}
	m.Stop()
	if m.started {
		t.Fatal("Manager should be stopped")
	}
}

func TestManagerGetStats(t *testing.T) {
	m := NewManager()
	stats := m.GetStats()
	if stats["total_clients"] != 0 {
		t.Fatalf("Expected 0 clients in stats, got %v", stats["total_clients"])
	}
	channels, ok := stats["channels"].(map[string]int)
	if !ok {
		t.Fatal("Expected channels map in stats")
	}
	if len(channels) != 0 {
		t.Fatalf("Expected 0 channels, got %d", len(channels))
	}
}

func TestGenerateClientID(t *testing.T) {
	id1 := generateClientID()
	time.Sleep(1 * time.Millisecond)
	id2 := generateClientID()
	if id1 == id2 {
		t.Fatalf("Expected unique client IDs, got same: %s", id1)
	}
	if len(id1) == 0 {
		t.Fatal("Expected non-empty client ID")
	}
	if len(id1) < 5 {
		t.Fatalf("Client ID too short: %s", id1)
	}
}

func TestBroadcastNoClients(t *testing.T) {
	m := NewManager()
	// Should not panic with no clients
	m.Broadcast(ChannelRealtime, "test", map[string]string{"key": "value"})
}

func TestBroadcastEventRouting(t *testing.T) {
	m := NewManager()

	tests := []struct {
		eventType engine.EventType
		channel   ChannelType
	}{
		{engine.EventTypeDataCollected, ChannelRealtime},
		{engine.EventTypeDataProcessed, ChannelRealtime},
		{engine.EventTypeAlarmTriggered, ChannelAlarm},
		{engine.EventTypeAlarmRecovered, ChannelAlarm},
		{engine.EventTypeAlarmAcknowledged, ChannelAlarm},
		{engine.EventTypeDeviceOnline, ChannelDevice},
		{engine.EventTypeDeviceOffline, ChannelDevice},
		{engine.EventTypeDeviceError, ChannelDevice},
		{engine.EventTypeDeviceCreated, ChannelDevice},
		{engine.EventTypeDeviceUpdated, ChannelDevice},
		{engine.EventTypeDeviceDeleted, ChannelDevice},
		{engine.EventTypeAIInference, ChannelAI},
	}

	for _, tt := range tests {
		event := engine.Event{
			Type:      tt.eventType,
			Source:    "test",
			Data:      map[string]string{"test": "data"},
			Timestamp: time.Now(),
		}
		// Should not panic
		m.BroadcastEvent(event)
	}
}

func TestBroadcastEventUnknownType(t *testing.T) {
	m := NewManager()
	event := engine.Event{
		Type:      "unknown_event_type",
		Source:    "test",
		Timestamp: time.Now(),
	}
	// Unknown event type should be ignored (no panic)
	m.BroadcastEvent(event)
}

func TestManagerRemoveClient(t *testing.T) {
	m := NewManager()
	// Add a fake client directly
	client := &Client{
		ID:       "test-client-1",
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 256),
	}
	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	if m.GetClientCount() != 1 {
		t.Fatalf("Expected 1 client, got %d", m.GetClientCount())
	}

	m.removeClient(client)

	if m.GetClientCount() != 0 {
		t.Fatalf("Expected 0 clients after removal, got %d", m.GetClientCount())
	}
}

func TestManagerBroadcastToSubscribedClient(t *testing.T) {
	m := NewManager()
	client := &Client{
		ID:       "test-client-2",
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 256),
	}
	client.channels[ChannelAlarm] = true

	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	m.Broadcast(ChannelAlarm, "alert", map[string]string{"severity": "high"})

	select {
	case msg := <-client.send:
		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			t.Fatalf("Failed to unmarshal message: %v", err)
		}
		if data["channel"] != string(ChannelAlarm) {
			t.Fatalf("Expected channel 'alarm', got %v", data["channel"])
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Did not receive broadcast message")
	}
}

func TestManagerBroadcastToUnsubscribedClient(t *testing.T) {
	m := NewManager()
	client := &Client{
		ID:       "test-client-3",
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 256),
	}
	// Client not subscribed to any channel

	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	m.Broadcast(ChannelAlarm, "alert", "test")

	select {
	case <-client.send:
		t.Fatal("Should not receive message on unsubscribed channel")
	case <-time.After(50 * time.Millisecond):
		// Expected - no message
	}
}

// A client whose send buffer is full used to lose frames with no trace: the
// tab kept rendering "connected" while alarms never arrived. The drops must be
// counted and surfaced through GetStats.
func TestManagerBroadcastCountsDroppedFrames(t *testing.T) {
	m := NewManager()
	client := &Client{
		ID:       "slow-client",
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 1),
	}
	client.channels[ChannelAlarm] = true

	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	for i := 0; i < 3; i++ {
		m.Broadcast(ChannelAlarm, "alert", map[string]string{"n": "x"})
	}

	if got := client.dropped.Load(); got != 2 {
		t.Fatalf("client drop counter = %d, want 2", got)
	}
	stats := m.GetStats()
	if stats["dropped_frames"] != uint64(2) {
		t.Fatalf("stats dropped_frames = %v, want 2", stats["dropped_frames"])
	}
	if stats["dropping_count"] != 1 {
		t.Fatalf("stats dropping_count = %v, want 1", stats["dropping_count"])
	}
	byClient, ok := stats["dropping_clients"].(map[string]uint64)
	if !ok || byClient["slow-client"] != 2 {
		t.Fatalf("stats must name the dropping client, got %#v", stats["dropping_clients"])
	}
	// The one frame that fit is still delivered: counting must not change the
	// delivery policy.
	select {
	case <-client.send:
	default:
		t.Fatal("the frame that fit the buffer must still be queued")
	}
}

func TestManagerGetStatsWithClient(t *testing.T) {
	m := NewManager()
	client := &Client{
		ID:       "test-client-4",
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 256),
	}
	client.channels[ChannelRealtime] = true
	client.channels[ChannelAlarm] = true

	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	stats := m.GetStats()
	if stats["total_clients"] != 1 {
		t.Fatalf("Expected 1 client, got %v", stats["total_clients"])
	}
	channels := stats["channels"].(map[string]int)
	if channels[string(ChannelRealtime)] != 1 {
		t.Fatalf("Expected 1 realtime channel, got %v", channels[string(ChannelRealtime)])
	}
	if channels[string(ChannelAlarm)] != 1 {
		t.Fatalf("Expected 1 alarm channel, got %v", channels[string(ChannelAlarm)])
	}
}

func TestChannelTypeConstants(t *testing.T) {
	if ChannelRealtime != "realtime" {
		t.Fatalf("Expected 'realtime', got %s", ChannelRealtime)
	}
	if ChannelAlarm != "alarm" {
		t.Fatalf("Expected 'alarm', got %s", ChannelAlarm)
	}
	if ChannelDevice != "device" {
		t.Fatalf("Expected 'device', got %s", ChannelDevice)
	}
	if ChannelIntegration != "integration" {
		t.Fatalf("Expected 'integration', got %s", ChannelIntegration)
	}
	if ChannelAI != "ai" {
		t.Fatalf("Expected 'ai', got %s", ChannelAI)
	}
}

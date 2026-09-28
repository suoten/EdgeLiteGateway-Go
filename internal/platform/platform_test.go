package platform

import (
	"context"
	"testing"
)

func TestPlatformRegistry(t *testing.T) {
	// Use a fresh registry to avoid interference with global state
	r := &PlatformRegistry{
		handlers: make(map[string]PlatformHandlerFactory),
	}

	r.Register("test_platform", func() PlatformHandler {
		return &TestPlatformHandler{name: "test_platform"}
	})

	if len(r.SupportedPlatforms()) != 1 {
		t.Errorf("Expected 1 platform, got %d", len(r.SupportedPlatforms()))
	}

	handler, err := r.Create("test_platform")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if handler.Name() != "test_platform" {
		t.Errorf("Expected 'test_platform', got '%s'", handler.Name())
	}

	// Unknown platform
	_, err = r.Create("unknown")
	if err == nil {
		t.Error("Expected error for unknown platform")
	}
}

func TestBasePlatformConnected(t *testing.T) {
	bp := NewBasePlatform()

	if bp.IsConnected() {
		t.Error("Should be disconnected initially")
	}

	bp.SetConnected(true)
	if !bp.IsConnected() {
		t.Error("Should be connected after SetConnected(true)")
	}

	bp.SetConnected(false)
	if bp.IsConnected() {
		t.Error("Should be disconnected after SetConnected(false)")
	}
}

func TestBasePlatformOfflineQueue(t *testing.T) {
	bp := NewBasePlatform()

	// Queue should start empty
	queue := bp.DrainOfflineQueue()
	if len(queue) != 0 {
		t.Errorf("Expected empty queue, got %d items", len(queue))
	}

	// Add messages
	msg1 := OfflineMessage{Topic: "test/1", Payload: []byte("data1"), QoS: 0}
	msg2 := OfflineMessage{Topic: "test/2", Payload: []byte("data2"), QoS: 1}

	bp.EnqueueOffline(msg1)
	bp.EnqueueOffline(msg2)

	// Drain should return both and clear queue
	queue = bp.DrainOfflineQueue()
	if len(queue) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(queue))
	}

	// Second drain should be empty
	queue = bp.DrainOfflineQueue()
	if len(queue) != 0 {
		t.Errorf("Expected empty queue after drain, got %d", len(queue))
	}
}

func TestBasePlatformOfflineQueueOverflow(t *testing.T) {
	bp := NewBasePlatform()
	bp.offlineQueueMax = 3

	for i := 0; i < 5; i++ {
		bp.EnqueueOffline(OfflineMessage{
			Topic:   "test",
			Payload: []byte{byte(i)},
			QoS:     0,
		})
	}

	queue := bp.DrainOfflineQueue()
	if len(queue) != 3 {
		t.Errorf("Expected 3 messages (max), got %d", len(queue))
	}

	// Verify oldest were dropped
	if queue[0].Payload[0] != 2 {
		t.Errorf("Expected first message to be index 2, got %d", queue[0].Payload[0])
	}
}

func TestBasePlatformRPCCallback(t *testing.T) {
	bp := NewBasePlatform()

	if bp.GetRPCCallback() != nil {
		t.Error("Expected nil RPC callback initially")
	}

	callback := func(deviceID string, method string, params map[string]interface{}) (interface{}, error) {
		return "result", nil
	}

	bp.SetRPCCallback(callback)

	cb := bp.GetRPCCallback()
	if cb == nil {
		t.Fatal("Expected RPC callback to be set")
	}

	result, err := cb("device-1", "read", nil)
	if err != nil {
		t.Fatalf("RPC callback failed: %v", err)
	}
	if result != "result" {
		t.Errorf("Expected 'result', got %v", result)
	}
}

func TestRegisterAll(t *testing.T) {
	// RegisterAll registers all built-in platform handlers
	RegisterAll()

	r := GetPlatformRegistry()
	platforms := r.SupportedPlatforms()

	expected := []string{"thingsboard", "huawei_iotda", "iotsharp", "thingspanel", "thingscloud", "custom_mqtt"}
	for _, name := range expected {
		found := false
		for _, p := range platforms {
			if p == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected platform '%s' to be registered", name)
		}
	}

	// Verify we can create each platform
	for _, name := range expected {
		handler, err := r.Create(name)
		if err != nil {
			t.Errorf("Failed to create platform '%s': %v", name, err)
		}
		if handler == nil {
			t.Errorf("Handler for '%s' is nil", name)
		}
		if handler.Name() == "" {
			t.Errorf("Handler for '%s' has empty name", name)
		}
	}
}

func TestMarshalPayload(t *testing.T) {
	data := map[string]interface{}{
		"key":  "value",
		"num":  42,
	}
	result := marshalPayload(data)
	if len(result) == 0 {
		t.Error("Expected non-empty payload")
	}

	// Test nil data
	result2 := marshalPayload(nil)
	// nil map marshals to "null" in JSON
	if len(result2) == 0 {
		t.Error("Expected non-empty payload for nil")
	}
}

// TestPlatformHandler is a test implementation of PlatformHandler.
type TestPlatformHandler struct {
	BasePlatform
	name string
}

func (h *TestPlatformHandler) Name() string { return h.name }
func (h *TestPlatformHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.SetConnected(true)
	return nil
}
func (h *TestPlatformHandler) Disconnect() error {
	h.SetConnected(false)
	return nil
}
func (h *TestPlatformHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	return nil
}
func (h *TestPlatformHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	return nil
}
func (h *TestPlatformHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	return nil
}
func (h *TestPlatformHandler) OnRPCRequest(callback RPCCallback) {}

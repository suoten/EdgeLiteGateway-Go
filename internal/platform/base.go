// Package platform provides north-bound IoT platform integration.
//
// This package is a 1:1 port of the Python edgelite/platform/ package.
// It includes platform handlers for ThingsBoard, Huawei IoTDA, IoTSharp,
// ThingsPanel, ThingsCloud, and custom MQTT platforms.
package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Handler is the abstract base for all north-bound platform handlers.
// All platform implementations must embed this struct and implement the
// Connect, Disconnect, PublishTelemetry, PublishAttributes methods.
type Handler interface {
	// Name returns the platform name.
	Name() string
	// Connect connects to the platform.
	Connect(ctx context.Context, config map[string]interface{}) error
	// Disconnect disconnects from the platform.
	Disconnect() error
	// IsConnected returns whether the platform is connected.
	IsConnected() bool
	// PublishTelemetry publishes device telemetry data.
	PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error
	// PublishAttributes publishes device attributes.
	PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error
	// PublishDeviceStatus publishes device online/offline status.
	PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error
	// OnRPCRequest registers a callback for RPC requests from the platform.
	OnRPCRequest(callback RPCCallback)
}

// RPCCallback is the callback function for RPC requests.
type RPCCallback func(deviceID string, method string, params map[string]interface{}) (interface{}, error)

// BasePlatform provides common functionality for all platform handlers.
type BasePlatform struct {
	mu               sync.Mutex
	connected        bool
	offlineQueue     []OfflineMessage
	offlineQueueMax  int
	reconnectBackoff float64
	config           map[string]interface{}
	rpcCallback      RPCCallback
}

// OfflineMessage represents a message buffered when the platform is offline.
type OfflineMessage struct {
	Topic     string
	Payload   []byte
	QoS       int
	Timestamp time.Time
}

// NewBasePlatform creates a new BasePlatform with defaults.
func NewBasePlatform() BasePlatform {
	return BasePlatform{
		offlineQueue:     make([]OfflineMessage, 0),
		offlineQueueMax:  10000,
		reconnectBackoff: 1.0,
	}
}

// IsConnected returns whether the platform is connected.
func (b *BasePlatform) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// SetConnected sets the connection state.
func (b *BasePlatform) SetConnected(connected bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected = connected
}

// EnqueueOffline adds a message to the offline queue.
func (b *BasePlatform) EnqueueOffline(msg OfflineMessage) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.offlineQueue) >= b.offlineQueueMax {
		// Drop oldest message
		b.offlineQueue = b.offlineQueue[1:]
	}
	b.offlineQueue = append(b.offlineQueue, msg)
	return true
}

// DrainOfflineQueue returns all queued messages and clears the queue.
func (b *BasePlatform) DrainOfflineQueue() []OfflineMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue := b.offlineQueue
	b.offlineQueue = make([]OfflineMessage, 0)
	return queue
}

// SetRPCCallback sets the RPC callback.
func (b *BasePlatform) SetRPCCallback(cb RPCCallback) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rpcCallback = cb
}

// GetRPCCallback returns the RPC callback.
func (b *BasePlatform) GetRPCCallback() RPCCallback {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rpcCallback
}

// configInt extracts an integer field from a platform config map tolerating
// the numeric types produced by different decoders: JSON yields float64,
// YAML yields int, and some encoders yield int64. A zero value falls back to
// the provided default.
func configInt(config map[string]interface{}, key string, def int) int {
	switch v := config[key].(type) {
	case float64:
		if v != 0 {
			return int(v)
		}
	case int:
		if v != 0 {
			return v
		}
	case int64:
		if v != 0 {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n != 0 {
			return n
		}
	}
	return def
}

// Registry manages all registered platform handlers.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]HandlerFactory
}

// HandlerFactory creates a new platform handler instance.
type HandlerFactory func() Handler

var globalPlatformRegistry = &Registry{
	handlers: make(map[string]HandlerFactory),
}

// GetPlatformRegistry returns the global platform registry.
func GetPlatformRegistry() *Registry {
	return globalPlatformRegistry
}

// Register registers a platform handler factory.
func (r *Registry) Register(name string, factory HandlerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[name] = factory
	logrus.WithField("platform", name).Debug("Platform handler registered")
}

// Create creates a platform handler instance.
func (r *Registry) Create(name string) (Handler, error) {
	r.mu.RLock()
	factory, ok := r.handlers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown platform: %s", name)
	}
	return factory(), nil
}

// SupportedPlatforms returns a list of registered platform names.
func (r *Registry) SupportedPlatforms() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		result = append(result, k)
	}
	return result
}

// RegisterAll registers all built-in platform handlers.
func RegisterAll() {
	r := GetPlatformRegistry()
	r.Register("thingsboard", NewThingsBoardHandler)
	r.Register("huawei_iotda", NewHuaweiIoTDAHandler)
	r.Register("iotsharp", NewIoTSharpHandler)
	r.Register("thingspanel", NewThingsPanelHandler)
	r.Register("thingscloud", NewThingsCloudHandler)
	r.Register("custom_mqtt", NewCustomMQTTHandler)
	logrus.Info("All platform handlers registered")
}

// marshalPayload marshals a map to JSON bytes.
func marshalPayload(data map[string]interface{}) []byte {
	b, err := json.Marshal(data)
	if err != nil {
		logrus.WithField("error", err.Error()).Error("Failed to marshal platform payload")
		return nil
	}
	return b
}

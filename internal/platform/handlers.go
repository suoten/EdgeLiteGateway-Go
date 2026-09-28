package platform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ============================================================================
// ThingsBoard Platform Handler
// Implements ThingsBoard Gateway MQTT protocol for device telemetry,
// attributes, and RPC.
// ============================================================================

type ThingsBoardHandler struct {
	BasePlatform
	broker     string
	port       int
	token      string
	password   string
	mqttClient *LightweightMQTTClient
	mu         sync.Mutex
}

func NewThingsBoardHandler() Handler {
	return &ThingsBoardHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *ThingsBoardHandler) Name() string { return "thingsboard" }

func (h *ThingsBoardHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.broker, _ = config["broker"].(string)
	if h.broker == "" {
		return fmt.Errorf("broker is required")
	}
	h.port = configInt(config, "port", 1883)
	h.token, _ = config["token"].(string)
	if h.token == "" {
		h.token, _ = config["username"].(string)
	}
	h.password, _ = config["password"].(string)
	h.config = config

	// Create and connect MQTT client
	clientID := fmt.Sprintf("edgelite-tb-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if h.token != "" {
		client.SetCredentials(h.token, h.password)
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("thingsboard mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	logrus.WithField("broker", h.broker).
		WithField("port", h.port).
		Info("ThingsBoard platform connected via MQTT")
	return nil
}

func (h *ThingsBoardHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *ThingsBoardHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		// Queue for offline delivery
		payload := marshalPayload(map[string]interface{}{
			deviceID: []map[string]interface{}{
				{
					"ts":     time.Now().UnixMilli(),
					"values": data,
				},
			},
		})
		if payload != nil {
			h.EnqueueOffline(OfflineMessage{
				Topic:     "v1/gateway/telemetry",
				Payload:   payload,
				QoS:       1,
				Timestamp: time.Now(),
			})
		}
		return fmt.Errorf("platform not connected, data queued")
	}

	// ThingsBoard gateway telemetry topic: v1/gateway/telemetry
	// Payload format: {"<deviceID>": [{"ts": <ts>, "values": {"key1": val1}}]}
	payload := map[string]interface{}{
		deviceID: []map[string]interface{}{
			{
				"ts":     time.Now().UnixMilli(),
				"values": data,
			},
		},
	}
	payloadBytes := marshalPayload(payload)
	if err := h.mqttClient.PublishCtx(ctx, "v1/gateway/telemetry", 1, payloadBytes); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("publish telemetry: %w", err)
	}
	logrus.WithField("device_id", deviceID).
		WithField("topic", "v1/gateway/telemetry").
		Debug("ThingsBoard telemetry published")
	return nil
}

func (h *ThingsBoardHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	// ThingsBoard gateway attributes topic: v1/gateway/attributes
	payload := marshalPayload(map[string]interface{}{
		deviceID: attrs,
	})
	if err := h.mqttClient.PublishCtx(ctx, "v1/gateway/attributes", 1, payload); err != nil {
		return fmt.Errorf("publish attributes: %w", err)
	}
	logrus.WithField("device_id", deviceID).
		Debug("ThingsBoard attributes published")
	return nil
}

func (h *ThingsBoardHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := "v1/gateway/connect"
	if !online {
		topic = "v1/gateway/disconnect"
	}
	payload := marshalPayload(map[string]interface{}{"device": deviceID})
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish device status: %w", err)
	}
	logrus.WithField("device_id", deviceID).
		WithField("online", online).
		Debug("ThingsBoard device status published")
	return nil
}

func (h *ThingsBoardHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

// ============================================================================
// Huawei IoTDA Platform Handler
// ============================================================================

type HuaweiIoTDAHandler struct {
	BasePlatform
	broker     string
	port       int
	deviceID   string
	secret     string
	mqttClient *LightweightMQTTClient
	mu         sync.Mutex
}

func NewHuaweiIoTDAHandler() Handler {
	return &HuaweiIoTDAHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *HuaweiIoTDAHandler) Name() string { return "huawei_iotda" }

func (h *HuaweiIoTDAHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.broker, _ = config["broker"].(string)
	h.port = configInt(config, "port", 1883)
	h.deviceID, _ = config["device_id"].(string)
	h.secret, _ = config["secret"].(string)
	h.config = config

	clientID := fmt.Sprintf("edgelite-iotda-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if h.deviceID != "" {
		client.SetCredentials(h.deviceID, h.secret)
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("huawei iotda mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	logrus.WithField("broker", h.broker).
		Info("Huawei IoTDA platform connected via MQTT")
	return nil
}

func (h *HuaweiIoTDAHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *HuaweiIoTDAHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("$oc/devices/%s/sys/properties/report", deviceID)
	payload := marshalPayload(map[string]interface{}{
		"services": []map[string]interface{}{
			{
				"service_id": "edgelite",
				"properties": data,
				"event_time": time.Now().Format(time.RFC3339),
			},
		},
	})
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish telemetry: %w", err)
	}
	logrus.WithField("device_id", deviceID).
		WithField("topic", topic).
		Debug("Huawei IoTDA telemetry published")
	return nil
}

func (h *HuaweiIoTDAHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("$oc/devices/%s/sys/shadow/data/report", deviceID)
	payload := marshalPayload(attrs)
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish attributes: %w", err)
	}
	logrus.WithField("device_id", deviceID).
		WithField("topic", topic).
		Debug("Huawei IoTDA attributes published")
	return nil
}

func (h *HuaweiIoTDAHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	status := "ONLINE"
	if !online {
		status = "OFFLINE"
	}
	topic := fmt.Sprintf("$oc/devices/%s/sys/lifecycle", deviceID)
	payload := marshalPayload(map[string]interface{}{
		"status":    status,
		"timestamp": time.Now().UnixMilli(),
	})
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish device status: %w", err)
	}
	return nil
}

func (h *HuaweiIoTDAHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

// ============================================================================
// IoTSharp Platform Handler
// ============================================================================

type IoTSharpHandler struct {
	BasePlatform
	broker     string
	port       int
	mqttClient *LightweightMQTTClient
	mu         sync.Mutex
}

func NewIoTSharpHandler() Handler {
	return &IoTSharpHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *IoTSharpHandler) Name() string { return "iotsharp" }

func (h *IoTSharpHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broker, _ = config["broker"].(string)
	h.port = configInt(config, "port", 1883)
	h.config = config

	clientID := fmt.Sprintf("edgelite-iotsharp-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if un, ok := config["username"].(string); ok && un != "" {
		pw, _ := config["password"].(string)
		client.SetCredentials(un, pw)
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("iotsharp mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	return nil
}

func (h *IoTSharpHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *IoTSharpHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("devices/telemetry/%s", deviceID)
	payload := marshalPayload(data)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *IoTSharpHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("devices/attributes/%s", deviceID)
	payload := marshalPayload(attrs)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *IoTSharpHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	return nil
}

func (h *IoTSharpHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

// ============================================================================
// ThingsPanel Platform Handler
// ============================================================================

type ThingsPanelHandler struct {
	BasePlatform
	broker     string
	port       int
	token      string
	mqttClient *LightweightMQTTClient
	mu         sync.Mutex
}

func NewThingsPanelHandler() Handler {
	return &ThingsPanelHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *ThingsPanelHandler) Name() string { return "thingspanel" }

func (h *ThingsPanelHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broker, _ = config["broker"].(string)
	h.port = configInt(config, "port", 1883)
	h.token, _ = config["token"].(string)
	h.config = config

	clientID := fmt.Sprintf("edgelite-tpanel-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if h.token != "" {
		client.SetCredentials(h.token, "")
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("thingspanel mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	return nil
}

func (h *ThingsPanelHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *ThingsPanelHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("device/%s/telemetry", deviceID)
	payload := marshalPayload(data)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *ThingsPanelHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("device/%s/attributes", deviceID)
	payload := marshalPayload(attrs)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *ThingsPanelHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	return nil
}

func (h *ThingsPanelHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

// ============================================================================
// ThingsCloud Platform Handler
// ============================================================================

type ThingsCloudHandler struct {
	BasePlatform
	broker     string
	port       int
	token      string
	mqttClient *LightweightMQTTClient
	mu         sync.Mutex
}

func NewThingsCloudHandler() Handler {
	return &ThingsCloudHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *ThingsCloudHandler) Name() string { return "thingscloud" }

func (h *ThingsCloudHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broker, _ = config["broker"].(string)
	h.port = configInt(config, "port", 1883)
	h.token, _ = config["token"].(string)
	h.config = config

	clientID := fmt.Sprintf("edgelite-tcloud-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if h.token != "" {
		client.SetCredentials(h.token, "")
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("thingscloud mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	return nil
}

func (h *ThingsCloudHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *ThingsCloudHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("thingscloud/%s/telemetry", deviceID)
	payload := marshalPayload(data)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *ThingsCloudHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("thingscloud/%s/attributes", deviceID)
	payload := marshalPayload(attrs)
	return h.mqttClient.PublishCtx(ctx, topic, 1, payload)
}

func (h *ThingsCloudHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	return nil
}

func (h *ThingsCloudHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

// ============================================================================
// Custom MQTT Platform Handler
// ============================================================================

type CustomMQTTHandler struct {
	BasePlatform
	broker         string
	port           int
	username       string
	password       string
	telemetryTopic string
	attrsTopic     string
	mqttClient     *LightweightMQTTClient
	mu             sync.Mutex
}

func NewCustomMQTTHandler() Handler {
	return &CustomMQTTHandler{
		BasePlatform: NewBasePlatform(),
	}
}

func (h *CustomMQTTHandler) Name() string { return "custom_mqtt" }

func (h *CustomMQTTHandler) Connect(ctx context.Context, config map[string]interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broker, _ = config["broker"].(string)
	h.port = configInt(config, "port", 1883)
	h.username, _ = config["username"].(string)
	h.password, _ = config["password"].(string)
	h.telemetryTopic, _ = config["telemetry_topic"].(string)
	if h.telemetryTopic == "" {
		h.telemetryTopic = "edgelite/telemetry"
	}
	h.attrsTopic, _ = config["attributes_topic"].(string)
	if h.attrsTopic == "" {
		h.attrsTopic = "edgelite/attributes"
	}
	h.config = config

	clientID := fmt.Sprintf("edgelite-custom-%d", time.Now().UnixNano())
	client := NewLightweightMQTTClient(h.broker, h.port, clientID)
	client.SetKeepAlive(60)
	client.SetDisconnectHandler(func() { h.SetConnected(false) })
	if h.username != "" {
		client.SetCredentials(h.username, h.password)
	}
	if err := client.Connect(); err != nil {
		h.SetConnected(false)
		return fmt.Errorf("custom mqtt connect: %w", err)
	}
	h.mqttClient = client
	h.SetConnected(true)
	logrus.WithField("broker", h.broker).
		WithField("port", h.port).
		Info("Custom MQTT platform connected")
	return nil
}

func (h *CustomMQTTHandler) Disconnect() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mqttClient != nil {
		h.mqttClient.Disconnect()
		h.mqttClient = nil
	}
	h.SetConnected(false)
	return nil
}

func (h *CustomMQTTHandler) PublishTelemetry(ctx context.Context, deviceID string, data map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("%s/%s", h.telemetryTopic, deviceID)
	payload := marshalPayload(data)
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish telemetry: %w", err)
	}
	return nil
}

func (h *CustomMQTTHandler) PublishAttributes(ctx context.Context, deviceID string, attrs map[string]interface{}) error {
	if !h.IsConnected() || h.mqttClient == nil {
		return fmt.Errorf("platform not connected")
	}
	topic := fmt.Sprintf("%s/%s", h.attrsTopic, deviceID)
	payload := marshalPayload(attrs)
	if err := h.mqttClient.PublishCtx(ctx, topic, 1, payload); err != nil {
		return fmt.Errorf("publish attributes: %w", err)
	}
	return nil
}

func (h *CustomMQTTHandler) PublishDeviceStatus(ctx context.Context, deviceID string, online bool) error {
	return nil
}

func (h *CustomMQTTHandler) OnRPCRequest(callback RPCCallback) {
	h.SetRPCCallback(callback)
}

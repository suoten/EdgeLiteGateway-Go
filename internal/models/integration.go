package models

// NorthPlatformConfig represents a northbound platform configuration.
type NorthPlatformConfig struct {
	Name    string                 `json:"name"`
	Enabled bool                   `json:"enabled"`
	Type    string                 `json:"type"`
	Config  map[string]interface{} `json:"config"`
}

// NorthPlatformStatus represents a northbound platform status.
type NorthPlatformStatus struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"` // connected, disconnected, error
	Message string `json:"message,omitempty"`
}

// IntegrationSession represents an integration session.
type IntegrationSession struct {
	SessionID string                 `json:"session_id"`
	Status    string                 `json:"status"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

// IntegrationHandshake represents an integration handshake message.
type IntegrationHandshake struct {
	Type    string                 `json:"type"`
	Version string                 `json:"version,omitempty"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

// HandshakeRequest represents an integration handshake request.
type HandshakeRequest struct {
	GatewayID    string   `json:"gateway_id"`
	GatewayName  string   `json:"gateway_name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// HandshakeResponse represents an integration handshake response.
type HandshakeResponse struct {
	Success   bool                   `json:"success"`
	SessionID string                 `json:"session_id,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

// BackhaulConfig represents a backhaul configuration.
type BackhaulConfig struct {
	Enabled       bool                   `json:"enabled"`
	Protocol      string                 `json:"protocol"` // mqtt, http, grpc
	Endpoint      string                 `json:"endpoint"`
	AuthMode      string                 `json:"auth_mode,omitempty"`
	AuthToken     string                 `json:"auth_token,omitempty"`
	Username      string                 `json:"username,omitempty"`
	Password      string                 `json:"password,omitempty"`
	Topic         string                 `json:"topic,omitempty"`
	DataFormat    string                 `json:"data_format"` // json, protobuf
	BatchSize     int                    `json:"batch_size,omitempty"`
	FlushInterval int                    `json:"flush_interval,omitempty"` // seconds
	RetryCount    int                    `json:"retry_count,omitempty"`
	RetryDelay    int                    `json:"retry_delay,omitempty"` // seconds
	Custom        map[string]interface{} `json:"custom,omitempty"`
}

// MqttTLSConfig represents MQTT TLS configuration.
type MqttTLSConfig struct {
	Enabled            bool   `json:"enabled"`
	CertFile           string `json:"cert_file,omitempty"`
	KeyFile            string `json:"key_file,omitempty"`
	CAFile             string `json:"ca_file,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

// MqttWillConfig represents MQTT will message configuration.
type MqttWillConfig struct {
	Topic   string `json:"topic,omitempty"`
	Payload string `json:"payload,omitempty"`
	QoS     int    `json:"qos,omitempty"`
	Retain  bool   `json:"retain,omitempty"`
}

// MqttConnectionConfig represents MQTT connection configuration.
type MqttConnectionConfig struct {
	Broker   string         `json:"broker"`
	ClientID string         `json:"client_id"`
	Username string         `json:"username,omitempty"`
	Password string         `json:"password,omitempty"`
	QoS      int            `json:"qos,omitempty"`
	TLS      MqttTLSConfig  `json:"tls,omitempty"`
	Will     MqttWillConfig `json:"will,omitempty"`
}

// TopicTemplateConfig represents topic template configuration.
type TopicTemplateConfig struct {
	Template string `json:"template"` // e.g. "edgelite/{device_id}/{point_name}"
}

// PayloadConfig represents payload format configuration.
type PayloadConfig struct {
	Format   string                 `json:"format"` // json, protobuf, csv
	Template map[string]interface{} `json:"template,omitempty"`
}

// QosPolicy represents QoS policy configuration.
type QosPolicy struct {
	MinQoS        int `json:"min_qos"`
	MaxQoS        int `json:"max_qos"`
	BatchSize     int `json:"batch_size"`
	FlushInterval int `json:"flush_interval"` // seconds
}

// NorthConfig represents northbound integration configuration.
type NorthConfig struct {
	Enabled      bool                   `json:"enabled"`
	PlatformType string                 `json:"platform_type"`
	Mqtt         MqttConnectionConfig   `json:"mqtt,omitempty"`
	Topic        TopicTemplateConfig    `json:"topic,omitempty"`
	Payload      PayloadConfig          `json:"payload,omitempty"`
	QoS          QosPolicy              `json:"qos,omitempty"`
	Custom       map[string]interface{} `json:"custom,omitempty"`
}

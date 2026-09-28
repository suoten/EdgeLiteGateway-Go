package platform

// Platform catalog and per-platform configuration schemas.
//
// This mirrors the Python services/platform_service.py _PLATFORM_REGISTRY and
// _build_full_schema: each platform exposes a set of base fields plus generic
// sections (MQTT Connection / TLS / Last Will / MQTT 5.0 / Topic / Payload /
// QoS / Dedup). The section titles are consumed verbatim by the frontend
// (web PlatformConfig.vue switches on section.title), so they must not be
// renamed without updating the frontend mapping.

// ConfigField describes a single configuration field in a platform schema.
type ConfigField struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Type        string      `json:"type"` // "string" | "integer"
	Required    bool        `json:"required,omitempty"`
	Secret      bool        `json:"secret,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
}

// ConfigSection groups fields under a titled collapsible section.
type ConfigSection struct {
	Title  string        `json:"title"`
	Fields []ConfigField `json:"fields"`
}

// PlatformSchema is the full schema payload returned by the
// GET /platforms/config-schema/:name endpoint.
type PlatformSchema struct {
	Fields   []ConfigField   `json:"fields"`
	Sections []ConfigSection `json:"sections"`
}

// PlatformMeta describes a supported platform for the frontend card picker.
type PlatformMeta struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// PlatformCatalog lists all supported northbound platforms with display
// metadata. Keys must match the handler names registered in RegisterAll.
var PlatformCatalog = []PlatformMeta{
	{Name: "thingsboard", Label: "ThingsBoard", Description: "ThingsBoard 网关协议（遥测/属性/RPC），使用网关 Access Token 认证", Version: "1.0"},
	{Name: "huawei_iotda", Label: "华为云 IoTDA", Description: "华为云 IoT 设备接入，使用设备 ID + 密钥认证，属性上报格式", Version: "1.0"},
	{Name: "thingspanel", Label: "ThingsPanel", Description: "ThingsPanel 开源物联网平台，使用设备 Token 认证", Version: "1.0"},
	{Name: "thingscloud", Label: "ThingsCloud", Description: "ThingsCloud 物联网平台，使用设备 Access Key 认证", Version: "1.0"},
	{Name: "iotsharp", Label: "IoTSharp", Description: "IoTSharp 开源物联网平台，遥测与属性上报", Version: "1.0"},
	{Name: "custom_mqtt", Label: "自定义 MQTT", Description: "自定义 MQTT Broker，可配置遥测/属性 Topic 前缀", Version: "1.0"},
}

// GetPlatformMeta returns display metadata for a platform name.
func GetPlatformMeta(name string) (PlatformMeta, bool) {
	for _, m := range PlatformCatalog {
		if m.Name == name {
			return m, true
		}
	}
	return PlatformMeta{}, false
}

// secretFields are masked when exporting platform configurations.
var secretFields = map[string]bool{
	"token":       true,
	"secret":      true,
	"password":    true,
	"access_key":  true,
	"access_secret": true,
}

// IsSecretField reports whether the given config key holds a secret that is
// masked as "***" on export. API layers use it to avoid writing the mask back
// over the real secret value.
func IsSecretField(key string) bool {
	return secretFields[key]
}

// MaskConfig returns a copy of the config with secret fields masked.
func MaskConfig(cfg map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(cfg))
	for k, v := range cfg {
		if secretFields[k] {
			if s, ok := v.(string); ok && s != "" {
				out[k] = "***"
				continue
			}
		}
		out[k] = v
	}
	return out
}

// baseFields returns the platform-specific (base) fields.
func baseFields(name string) []ConfigField {
	switch name {
	case "thingsboard":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true, Placeholder: "例如 127.0.0.1 或 tb.example.com"},
			{Name: "token", Label: "Access Token", Type: "string", Required: true, Secret: true, Placeholder: "ThingsBoard 网关 Access Token"},
		}
	case "huawei_iotda":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true, Placeholder: "例如 xxx.st1.iotda-device.cn-north-4.myhuaweicloud.com"},
			{Name: "device_id", Label: "设备 ID", Type: "string", Required: true, Placeholder: "华为云平台设备 ID"},
			{Name: "secret", Label: "设备密钥", Type: "string", Required: true, Secret: true, Placeholder: "华为云平台设备密钥"},
		}
	case "thingspanel":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true},
			{Name: "token", Label: "Device Token", Type: "string", Required: true, Secret: true, Placeholder: "ThingsPanel 设备 Token"},
		}
	case "thingscloud":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true},
			{Name: "token", Label: "Access Key", Type: "string", Required: true, Secret: true, Placeholder: "ThingsCloud 设备 Access Key"},
		}
	case "iotsharp":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true},
			{Name: "username", Label: "设备 ID / 用户名", Type: "string", Required: true},
			{Name: "password", Label: "密码", Type: "string", Secret: true},
		}
	case "custom_mqtt":
		return []ConfigField{
			{Name: "broker", Label: "Broker 地址", Type: "string", Required: true},
			{Name: "username", Label: "用户名", Type: "string"},
			{Name: "password", Label: "密码", Type: "string", Secret: true},
			{Name: "telemetry_topic", Label: "遥测 Topic 前缀", Type: "string", Default: "edgelite/telemetry", Placeholder: "实际发布到 {前缀}/{设备ID}"},
			{Name: "attributes_topic", Label: "属性 Topic 前缀", Type: "string", Default: "edgelite/attributes", Placeholder: "实际发布到 {前缀}/{设备ID}"},
		}
	}
	return nil
}

// GetPlatformSchema returns the full config schema for a platform name.
func GetPlatformSchema(name string) *PlatformSchema {
	schema := &PlatformSchema{
		Fields: baseFields(name),
		Sections: []ConfigSection{
			{
				Title: "MQTT Connection",
				Fields: []ConfigField{
					{Name: "port", Label: "端口", Type: "integer", Default: 1883, Placeholder: "1883"},
					{Name: "keep_alive", Label: "Keep Alive (秒)", Type: "integer", Default: 60},
					{Name: "client_id", Label: "Client ID 前缀", Type: "string", Placeholder: "默认自动生成"},
				},
			},
			{
				Title: "TLS/SSL",
				Fields: []ConfigField{
					{Name: "ca_cert", Label: "CA 证书 (PEM)", Type: "string", Placeholder: "可选，PEM 文本"},
					{Name: "client_cert", Label: "客户端证书 (PEM)", Type: "string", Placeholder: "可选，PEM 文本"},
					{Name: "client_key", Label: "客户端私钥 (PEM)", Type: "string", Placeholder: "可选，PEM 文本"},
				},
			},
			{
				Title: "Last Will",
				Fields: []ConfigField{
					{Name: "will_topic", Label: "遗嘱 Topic", Type: "string"},
					{Name: "will_payload", Label: "遗嘱 Payload", Type: "string"},
					{Name: "will_qos", Label: "遗嘱 QoS", Type: "integer", Default: 0},
				},
			},
			{
				Title: "MQTT 5.0 Properties",
				Fields: []ConfigField{
					{Name: "protocol_version", Label: "MQTT 协议版本 (5=5.0)", Type: "integer", Default: 3},
					{Name: "session_expiry", Label: "会话过期时间 (秒)", Type: "integer"},
					{Name: "receive_maximum", Label: "Receive Maximum", Type: "integer"},
				},
			},
			{
				Title: "Topic Template",
				Fields: []ConfigField{
					{Name: "topic_template", Label: "Topic 模板", Type: "string", Default: "{prefix}/{device_id}/data", Placeholder: "支持 {device_id} 变量"},
				},
			},
			{
				Title: "Payload Format",
				Fields: []ConfigField{
					{Name: "payload_format", Label: "Payload 格式", Type: "string", Default: "json", Placeholder: "json / cbor / protobuf / custom"},
					{Name: "custom_template", Label: "自定义模板", Type: "string", Placeholder: "payload_format 为 custom 时生效"},
				},
			},
			{
				Title: "QoS Policy",
				Fields: []ConfigField{
					{Name: "default_qos", Label: "默认 QoS", Type: "integer", Default: 1},
					{Name: "alarm_qos", Label: "告警 QoS", Type: "integer", Default: 1},
				},
			},
			{
				Title: "Deduplication",
				Fields: []ConfigField{
					{Name: "dedup_window_ms", Label: "去重窗口 (毫秒)", Type: "integer", Default: 0, Placeholder: "0 表示不去重"},
				},
			},
		},
	}
	return schema
}

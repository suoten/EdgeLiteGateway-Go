// Package config provides configuration loading and hot-reload for EdgeLiteGateway.
//
// This is a 1:1 port of the Python edgelite/config.py module.
// It supports YAML config files, .env overrides, environment variable interpolation,
// sensitive field encryption, and hot-reload with change detection.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Host               string   `yaml:"host" json:"host"`
	Port               int      `yaml:"port" json:"port"`
	CORSAllowedOrigins []string `yaml:"cors_allowed_origins" json:"cors_allowed_origins"`
	CORSOrigins        []string `yaml:"cors_origins" json:"cors_origins"`
	WebhookAPIKey      string   `yaml:"webhook_api_key" json:"webhook_api_key"`
	DebugAPIEnabled    bool     `yaml:"debug_api_enabled" json:"debug_api_enabled"`
	DebugAPIAllowedIPs []string `yaml:"debug_api_allowed_ips" json:"debug_api_allowed_ips"`
	TrustedProxies     []string `yaml:"trusted_proxies" json:"trusted_proxies"`
	AllowedHosts       []string `yaml:"allowed_hosts" json:"allowed_hosts"`
}

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		Host:               "127.0.0.1",
		Port:               8080,
		CORSAllowedOrigins: []string{},
		CORSOrigins:        []string{"http://localhost:3000"},
		WebhookAPIKey:      "",
		DebugAPIEnabled:    false,
		DebugAPIAllowedIPs: []string{"127.0.0.1", "::1"},
		TrustedProxies:     []string{},
		AllowedHosts:       []string{},
	}
}

// DatabaseConfig holds database configuration.
type DatabaseConfig struct {
	Backend                string  `yaml:"backend" json:"backend"`
	SQLitePath             string  `yaml:"sqlite_path" json:"sqlite_path"`
	Host                   string  `yaml:"host" json:"host"`
	Port                   int     `yaml:"port" json:"port"`
	Username               string  `yaml:"username" json:"username"`
	Password               string  `yaml:"password" json:"password"`
	Database               string  `yaml:"database" json:"database"`
	BackupDir              string  `yaml:"backup_dir" json:"backup_dir"`
	PoolSize               int     `yaml:"pool_size" json:"pool_size"`
	MaxOverflow            int     `yaml:"max_overflow" json:"max_overflow"`
	Echo                   bool    `yaml:"echo" json:"echo"`
	OptimisticLockRetries  int     `yaml:"optimistic_lock_retries" json:"optimistic_lock_retries"`
	TrustServerCertificate bool    `yaml:"trust_server_certificate" json:"trust_server_certificate"`
	SlowQueryThresholdS    float64 `yaml:"slow_query_threshold_s" json:"slow_query_threshold_s"`
}

func defaultDatabaseConfig() DatabaseConfig {
	return DatabaseConfig{
		Backend:                "sqlite",
		SQLitePath:             "data/edgelite.db",
		Host:                   "localhost",
		Port:                   3306,
		Username:               "",
		Password:               "",
		Database:               "edgelite",
		BackupDir:              "data/backups",
		PoolSize:               5,
		MaxOverflow:            10,
		Echo:                   false,
		OptimisticLockRetries:  3,
		TrustServerCertificate: false,
		SlowQueryThresholdS:    1.0,
	}
}

// DownsampleConfig holds data downsample configuration.
type DownsampleConfig struct {
	Enabled          bool `yaml:"enabled" json:"enabled"`
	Tier1AgeDays     int  `yaml:"tier1_age_days" json:"tier1_age_days"`
	Tier2AgeDays     int  `yaml:"tier2_age_days" json:"tier2_age_days"`
	Tier3AgeDays     int  `yaml:"tier3_age_days" json:"tier3_age_days"`
	AutoRun          bool `yaml:"auto_run" json:"auto_run"`
	RunIntervalHours int  `yaml:"run_interval_hours" json:"run_interval_hours"`
}

func defaultDownsampleConfig() DownsampleConfig {
	return DownsampleConfig{Enabled: true, Tier1AgeDays: 7, Tier2AgeDays: 30, Tier3AgeDays: 90, AutoRun: false, RunIntervalHours: 24}
}

// InfluxDBConfig holds InfluxDB configuration.
type InfluxDBConfig struct {
	URL                string           `yaml:"url" json:"url"`
	Token              string           `yaml:"token" json:"token"`
	Org                string           `yaml:"org" json:"org"`
	Bucket             string           `yaml:"bucket" json:"bucket"`
	BatchSize          int              `yaml:"batch_size" json:"batch_size"`
	FlushInterval      int              `yaml:"flush_interval" json:"flush_interval"`
	RetentionDays      int              `yaml:"retention_days" json:"retention_days"`
	FallbackBackend    string           `yaml:"fallback_backend" json:"fallback_backend"`
	SQLiteTSPath       string           `yaml:"sqlite_ts_path" json:"sqlite_ts_path"`
	AutoSyncOnRecovery bool             `yaml:"auto_sync_on_recovery" json:"auto_sync_on_recovery"`
	SyncBatchSize      int              `yaml:"sync_batch_size" json:"sync_batch_size"`
	SyncInterval       int              `yaml:"sync_interval" json:"sync_interval"`
	Downsample         DownsampleConfig `yaml:"downsample" json:"downsample"`
	NetworkProbeURL    string           `yaml:"network_probe_url" json:"network_probe_url"`
}

func defaultInfluxDBConfig() InfluxDBConfig {
	return InfluxDBConfig{
		URL: "http://localhost:8086", Token: "", Org: "edgelite", Bucket: "edgelite",
		BatchSize: 1000, FlushInterval: 5000, RetentionDays: 30,
		FallbackBackend: "sqlite", SQLiteTSPath: "data/edgelite_ts.db",
		AutoSyncOnRecovery: true, SyncBatchSize: 500, SyncInterval: 30,
		Downsample: defaultDownsampleConfig(), NetworkProbeURL: "https://www.baidu.com",
	}
}

// CacheConfig holds ring buffer and incremental sync configuration.
type CacheConfig struct {
	RingBufferCapacity     int     `yaml:"ring_buffer_capacity" json:"ring_buffer_capacity"`
	RingBufferCompress     bool    `yaml:"ring_buffer_compress" json:"ring_buffer_compress"`
	IncrementalSyncEnabled bool    `yaml:"incremental_sync_enabled" json:"incremental_sync_enabled"`
	HighWatermarkPct       float64 `yaml:"high_watermark_pct" json:"high_watermark_pct"`
	CriticalWatermarkPct   float64 `yaml:"critical_watermark_pct" json:"critical_watermark_pct"`
}

func defaultCacheConfig() CacheConfig {
	return CacheConfig{RingBufferCapacity: 100000, RingBufferCompress: true, IncrementalSyncEnabled: true, HighWatermarkPct: 0.8, CriticalWatermarkPct: 0.9}
}

// MQTTConfig holds MQTT client configuration.
type MQTTConfig struct {
	Broker              string  `yaml:"broker" json:"broker"`
	Port                int     `yaml:"port" json:"port"`
	Username            string  `yaml:"username" json:"username"`
	Password            string  `yaml:"password" json:"password"`
	TopicPrefix         string  `yaml:"topic_prefix" json:"topic_prefix"`
	OfflineCacheEnabled bool    `yaml:"offline_cache_enabled" json:"offline_cache_enabled"`
	OfflineDBPath       string  `yaml:"offline_db_path" json:"offline_db_path"`
	MaxQueueSize        int     `yaml:"max_queue_size" json:"max_queue_size"`
	MaxRetries          int     `yaml:"max_retries" json:"max_retries"`
	RetryInterval       float64 `yaml:"retry_interval" json:"retry_interval"`
	RingBufferCapacity  int     `yaml:"ring_buffer_capacity" json:"ring_buffer_capacity"`
	RingBufferCompress  bool    `yaml:"ring_buffer_compress" json:"ring_buffer_compress"`
}

func defaultMQTTConfig() MQTTConfig {
	return MQTTConfig{Broker: "localhost", Port: 1883, Username: "", Password: "", TopicPrefix: "edgelite", OfflineCacheEnabled: true, OfflineDBPath: "data/mqtt_offline_queue.db", MaxQueueSize: 10000, MaxRetries: 100, RetryInterval: 5.0, RingBufferCapacity: 50000, RingBufferCompress: true}
}

// PyGBSentryConfig holds PyGBSentry video platform configuration.
type PyGBSentryConfig struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	APIKey   string `yaml:"api_key" json:"api_key"`
	Timeout  int    `yaml:"timeout" json:"timeout"`
}

// VideoConfig holds video configuration.
type VideoConfig struct {
	PyGBSentry PyGBSentryConfig `yaml:"pygbsentry" json:"pygbsentry"`
}

func defaultVideoConfig() VideoConfig {
	return VideoConfig{PyGBSentry: PyGBSentryConfig{Endpoint: "", APIKey: "", Timeout: 10}}
}

// SecurityConfig holds security configuration.
type SecurityConfig struct {
	SecretKey                  string   `yaml:"secret_key" json:"secret_key"`
	AccessTokenExpireMinutes   int      `yaml:"access_token_expire_minutes" json:"access_token_expire_minutes"`
	RefreshTokenExpireDays     int      `yaml:"refresh_token_expire_days" json:"refresh_token_expire_days"`
	Algorithm                  string   `yaml:"algorithm" json:"algorithm"`
	MaxTokenTTLDays            int      `yaml:"max_token_ttl_days" json:"max_token_ttl_days"`
	SecretKeyPrevious          string   `yaml:"secret_key_previous" json:"secret_key_previous"`
	KeyID                      string   `yaml:"key_id" json:"key_id"`
	PreviousKeyID              string   `yaml:"previous_key_id" json:"previous_key_id"`
	RateLimitRequestsPerMinute int      `yaml:"rate_limit_requests_per_minute" json:"rate_limit_requests_per_minute"`
	RateLimitLoginPerMinute    int      `yaml:"rate_limit_login_per_minute" json:"rate_limit_login_per_minute"`
	LoginLockoutThreshold      int      `yaml:"login_lockout_threshold" json:"login_lockout_threshold"`
	LoginLockoutMinutes        int      `yaml:"login_lockout_minutes" json:"login_lockout_minutes"`
	GlobalLockoutThreshold     int      `yaml:"global_lockout_threshold" json:"global_lockout_threshold"`
	GlobalLockoutDuration      int      `yaml:"global_lockout_duration" json:"global_lockout_duration"`
	GlobalFailureRateThreshold int      `yaml:"global_failure_rate_threshold" json:"global_failure_rate_threshold"`
	GlobalLockoutWindow        int      `yaml:"global_lockout_window" json:"global_lockout_window"`
	ProtectedRoles             []string `yaml:"protected_roles" json:"protected_roles"`
	CSRFSecret                 string   `yaml:"csrf_secret" json:"csrf_secret"`
	CookieSecure               bool     `yaml:"cookie_secure" json:"cookie_secure"`
}

func defaultSecurityConfig() SecurityConfig {
	return SecurityConfig{
		SecretKey: "", AccessTokenExpireMinutes: 30, RefreshTokenExpireDays: 7,
		Algorithm: "HS256", MaxTokenTTLDays: 30, SecretKeyPrevious: "",
		KeyID: "default", PreviousKeyID: "",
		// 600/min: SPA dashboard polls ~8 endpoints every 5s (~100/min per view);
		// 120 caused 429 storms on normal dashboard usage. Still throttles abuse.
		RateLimitRequestsPerMinute: 600, RateLimitLoginPerMinute: 5,
		LoginLockoutThreshold: 5, LoginLockoutMinutes: 15,
		GlobalLockoutThreshold: 10, GlobalLockoutDuration: 30,
		GlobalFailureRateThreshold: 50, GlobalLockoutWindow: 15,
		ProtectedRoles: []string{"admin"}, CSRFSecret: "", CookieSecure: false,
	}
}

// LoggingConfig holds logging configuration.
type LoggingConfig struct {
	Level       string `yaml:"level" json:"level"`
	Format      string `yaml:"format" json:"format"`
	JSONFormat  bool   `yaml:"json_format" json:"json_format"`
	LogDir      string `yaml:"log_dir" json:"log_dir"`
	MaxBytes    int    `yaml:"max_bytes" json:"max_bytes"`
	BackupCount int    `yaml:"backup_count" json:"backup_count"`
}

func defaultLoggingConfig() LoggingConfig {
	return LoggingConfig{Level: "INFO", Format: "%(asctime)s | %(levelname)-8s | %(name)s | %(message)s", JSONFormat: false, LogDir: "data/logs", MaxBytes: 52428800, BackupCount: 10}
}

// SimulatorPointConfig holds a simulator point definition.
type SimulatorPointConfig struct {
	Name       string  `yaml:"name" json:"name"`
	DataType   string  `yaml:"data_type" json:"data_type"`
	Unit       string  `yaml:"unit" json:"unit"`
	Address    string  `yaml:"address" json:"address"`
	AccessMode string  `yaml:"access_mode" json:"access_mode"`
	Min        float64 `yaml:"min" json:"min"`
	Max        float64 `yaml:"max" json:"max"`
	Mode       string  `yaml:"mode" json:"mode"`
}

// SimulatorDeviceConfig holds a simulator device definition.
type SimulatorDeviceConfig struct {
	DeviceID        string                 `yaml:"device_id" json:"device_id"`
	Name            string                 `yaml:"name" json:"name"`
	Points          []SimulatorPointConfig `yaml:"points" json:"points"`
	CollectInterval int                    `yaml:"collect_interval" json:"collect_interval"`
}

// SimulatorConfig holds simulator configuration.
type SimulatorConfig struct {
	AutoCreate     bool                    `yaml:"auto_create" json:"auto_create"`
	DefaultDevices []SimulatorDeviceConfig `yaml:"default_devices" json:"default_devices"`
}

func defaultSimulatorConfig() SimulatorConfig {
	return SimulatorConfig{AutoCreate: false, DefaultDevices: []SimulatorDeviceConfig{}}
}

// NotifyDingtalkConfig holds DingTalk notification configuration.
type NotifyDingtalkConfig struct {
	Enabled         bool     `yaml:"enabled" json:"enabled"`
	Name            string   `yaml:"name" json:"name"`
	WebhookURL      string   `yaml:"webhook_url" json:"webhook_url"`
	Secret          string   `yaml:"secret" json:"secret"`
	ATMobiles       []string `yaml:"at_mobiles" json:"at_mobiles"`
	IsAtAll         bool     `yaml:"is_at_all" json:"is_at_all"`
	MaxPerMinute    int      `yaml:"max_per_minute" json:"max_per_minute"`
	CooldownSeconds float64  `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

func defaultNotifyDingtalkConfig() NotifyDingtalkConfig {
	return NotifyDingtalkConfig{Enabled: true, Name: "钉钉通知", WebhookURL: "", Secret: "", ATMobiles: []string{}, IsAtAll: false, MaxPerMinute: 10, CooldownSeconds: 60.0}
}

// NotifyEmailConfig holds email notification configuration.
type NotifyEmailConfig struct {
	SMTPHost        string   `yaml:"smtp_host" json:"smtp_host"`
	SMTPPort        int      `yaml:"smtp_port" json:"smtp_port"`
	SMTPUser        string   `yaml:"smtp_user" json:"smtp_user"`
	SMTPPassword    string   `yaml:"smtp_password" json:"smtp_password"`
	UseTLS          bool     `yaml:"use_tls" json:"use_tls"`
	UseSSL          bool     `yaml:"use_ssl" json:"use_ssl"`
	UseStartTLS     bool     `yaml:"use_starttls" json:"use_starttls"`
	FromAddr        string   `yaml:"from_addr" json:"from_addr"`
	ToAddrs         []string `yaml:"to_addrs" json:"to_addrs"`
	MaxPerMinute    int      `yaml:"max_per_minute" json:"max_per_minute"`
	CooldownSeconds float64  `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

func defaultNotifyEmailConfig() NotifyEmailConfig {
	return NotifyEmailConfig{SMTPHost: "", SMTPPort: 587, SMTPUser: "", SMTPPassword: "", UseTLS: true, UseStartTLS: false, FromAddr: "", ToAddrs: []string{}, MaxPerMinute: 60, CooldownSeconds: 60.0}
}

// NotifyWechatConfig holds WeChat notification configuration.
type NotifyWechatConfig struct {
	WebhookURL      string  `yaml:"webhook_url" json:"webhook_url"`
	MaxPerMinute    int     `yaml:"max_per_minute" json:"max_per_minute"`
	CooldownSeconds float64 `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

func defaultNotifyWechatConfig() NotifyWechatConfig {
	return NotifyWechatConfig{WebhookURL: "", MaxPerMinute: 10, CooldownSeconds: 60.0}
}

// NotifyWebhookConfig holds custom webhook notification configuration.
type NotifyWebhookConfig struct {
	Enabled         bool              `yaml:"enabled" json:"enabled"`
	Name            string            `yaml:"name" json:"name"`
	URL             string            `yaml:"url" json:"url"`
	Method          string            `yaml:"method" json:"method"`
	Headers         map[string]string `yaml:"headers" json:"headers"`
	AuthType        string            `yaml:"auth_type" json:"auth_type"`
	AuthToken       string            `yaml:"auth_token" json:"auth_token"`
	AuthUsername    string            `yaml:"auth_username" json:"auth_username"`
	AuthPassword    string            `yaml:"auth_password" json:"auth_password"`
	MaxPerMinute    int               `yaml:"max_per_minute" json:"max_per_minute"`
	CooldownSeconds float64           `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

func defaultNotifyWebhookConfig() NotifyWebhookConfig {
	return NotifyWebhookConfig{Enabled: true, Name: "自定义Webhook", URL: "", Method: "POST", Headers: map[string]string{}, AuthType: "none", AuthToken: "", AuthUsername: "", AuthPassword: "", MaxPerMinute: 10, CooldownSeconds: 60.0}
}

// NotifyConfig holds notification configuration.
type NotifyConfig struct {
	Dingtalk NotifyDingtalkConfig `yaml:"dingtalk" json:"dingtalk"`
	Email    NotifyEmailConfig    `yaml:"email" json:"email"`
	Wechat   NotifyWechatConfig   `yaml:"wechat" json:"wechat"`
	Webhook  NotifyWebhookConfig  `yaml:"webhook" json:"webhook"`
}

func defaultNotifyConfig() NotifyConfig {
	return NotifyConfig{Dingtalk: defaultNotifyDingtalkConfig(), Email: defaultNotifyEmailConfig(), Wechat: defaultNotifyWechatConfig(), Webhook: defaultNotifyWebhookConfig()}
}

// MqttServerConfig holds internal MQTT server configuration.
type MqttServerConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	Host        string `yaml:"host" json:"host"`
	Port        int    `yaml:"port" json:"port"`
	WSPort      *int   `yaml:"ws_port" json:"ws_port"`
	Username    string `yaml:"username" json:"username"`
	Password    string `yaml:"password" json:"password"`
	AllowNoAuth bool   `yaml:"allow_no_auth" json:"allow_no_auth"`
}

func defaultMqttServerConfig() MqttServerConfig {
	return MqttServerConfig{Enabled: false, Host: "127.0.0.1", Port: 1888, WSPort: nil, Username: "", Password: "", AllowNoAuth: false}
}

// ModbusSlaveConfig holds internal Modbus slave configuration.
type ModbusSlaveConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	Host         string `yaml:"host" json:"host"`
	Port         int    `yaml:"port" json:"port"`
	HoldingSize  int    `yaml:"holding_size" json:"holding_size"`
	InputSize    int    `yaml:"input_size" json:"input_size"`
	CoilSize     int    `yaml:"coil_size" json:"coil_size"`
	DiscreteSize int    `yaml:"discrete_size" json:"discrete_size"`
}

func defaultModbusSlaveConfig() ModbusSlaveConfig {
	return ModbusSlaveConfig{Enabled: false, Host: "127.0.0.1", Port: 5020, HoldingSize: 1000, InputSize: 1000, CoilSize: 1000, DiscreteSize: 1000}
}

// SerialBridgeConfig holds serial bridge configuration.
type SerialBridgeConfig struct {
	Enabled     bool     `yaml:"enabled" json:"enabled"`
	SerialPort  string   `yaml:"serial_port" json:"serial_port"`
	BaudRate    int      `yaml:"baud_rate" json:"baud_rate"`
	DataBits    int      `yaml:"data_bits" json:"data_bits"`
	Parity      string   `yaml:"parity" json:"parity"`
	StopBits    int      `yaml:"stop_bits" json:"stop_bits"`
	TCPPort     int      `yaml:"tcp_port" json:"tcp_port"`
	IPWhitelist []string `yaml:"ip_whitelist" json:"ip_whitelist"`
	MaxClients  int      `yaml:"max_clients" json:"max_clients"`
}

func defaultSerialBridgeConfig() SerialBridgeConfig {
	portName := "/dev/ttyUSB0"
	if filepath.Separator == '\\' {
		portName = "COM1"
	}
	// The line settings are part of the default because the bridge applies them
	// to the device: 8N1 is what an unset config means, not a suggestion.
	return SerialBridgeConfig{Enabled: false, SerialPort: portName, BaudRate: 9600, DataBits: 8, Parity: "N", StopBits: 1, TCPPort: 9000, IPWhitelist: []string{}, MaxClients: 5}
}

// PreprocessGlobalConfig holds edge data preprocessing global configuration.
type PreprocessGlobalConfig struct {
	Enabled                   bool    `yaml:"enabled" json:"enabled"`
	DefaultDeadband           float64 `yaml:"default_deadband" json:"default_deadband"`
	DefaultFilterWindow       int     `yaml:"default_filter_window" json:"default_filter_window"`
	DefaultAggregateWindowSec int     `yaml:"default_aggregate_window_sec" json:"default_aggregate_window_sec"`
}

func defaultPreprocessGlobalConfig() PreprocessGlobalConfig {
	return PreprocessGlobalConfig{Enabled: false, DefaultDeadband: 0.0, DefaultFilterWindow: 3, DefaultAggregateWindowSec: 0}
}

// WebhookAuthConfig holds HTTP webhook authentication configuration.
type WebhookAuthConfig struct {
	Mode     string `yaml:"mode" json:"mode"`
	Token    string `yaml:"token" json:"token"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
}

func defaultWebhookAuthConfig() WebhookAuthConfig {
	return WebhookAuthConfig{Mode: "none", Token: "", Username: "", Password: ""}
}

// MqttTLSConfigModel holds MQTT TLS/SSL configuration.
type MqttTLSConfigModel struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	CACert     string `yaml:"ca_cert" json:"ca_cert"`
	ClientCert string `yaml:"client_cert" json:"client_cert"`
	ClientKey  string `yaml:"client_key" json:"client_key"`
	CertReqs   string `yaml:"cert_reqs" json:"cert_reqs"`
}

func defaultMqttTLSConfig() MqttTLSConfigModel {
	return MqttTLSConfigModel{Enabled: false, CACert: "", ClientCert: "", ClientKey: "", CertReqs: "required"}
}

// McpServerConfig holds MCP Server configuration.
type McpServerConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// GrafanaConfig holds Grafana integration configuration.
type GrafanaConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	URL        string `yaml:"url" json:"url"`
	APIKey     string `yaml:"api_key" json:"api_key"`
	Datasource string `yaml:"datasource" json:"datasource"`
}

func defaultGrafanaConfig() GrafanaConfig {
	return GrafanaConfig{Enabled: false, URL: "http://localhost:3001", APIKey: "", Datasource: "InfluxDB"}
}

// AiInferenceConfig holds edge AI inference engine configuration.
type AiInferenceConfig struct {
	Enabled                 bool   `yaml:"enabled" json:"enabled"`
	ModelsDir               string `yaml:"models_dir" json:"models_dir"`
	SidecarURL              string `yaml:"sidecar_url" json:"sidecar_url"`
	HotReloadTimeout        int    `yaml:"hot_reload_timeout" json:"hot_reload_timeout"`
	InferenceTimeout        int    `yaml:"inference_timeout" json:"inference_timeout"`
	MaxConcurrentInferences int    `yaml:"max_concurrent_inferences" json:"max_concurrent_inferences"`
	StatsRetentionDays      int    `yaml:"stats_retention_days" json:"stats_retention_days"`
}

func defaultAiInferenceConfig() AiInferenceConfig {
	return AiInferenceConfig{Enabled: true, ModelsDir: "models", SidecarURL: "http://127.0.0.1:50052", HotReloadTimeout: 30, InferenceTimeout: 10, MaxConcurrentInferences: 4, StatsRetentionDays: 7}
}

// DriversConfig holds driver configuration.
type DriversConfig struct {
	CustomDir  string `yaml:"custom_dir" json:"custom_dir"`
	AutoReload bool   `yaml:"auto_reload" json:"auto_reload"`
}

// BackupConfig holds automatic backup scheduler configuration.
type BackupConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	IntervalHours int    `yaml:"interval_hours" json:"interval_hours"`
	RetainDays    int    `yaml:"retain_days" json:"retain_days"`
	BackupDir     string `yaml:"backup_dir" json:"backup_dir"`
	MinFreeMB     int    `yaml:"min_free_mb" json:"min_free_mb"`
}

func defaultBackupConfig() BackupConfig {
	return BackupConfig{Enabled: true, IntervalHours: 24, RetainDays: 7, BackupDir: "data/backups", MinFreeMB: 100}
}

// BackupDirectory returns the single directory every backup path shares. The UI
// list, the manual button and the scheduler must agree, otherwise a configured
// backup.backup_dir silently hides backups that still exist on disk.
func (c *AppConfig) BackupDirectory() string {
	if c != nil && c.Backup.BackupDir != "" {
		return c.Backup.BackupDir
	}
	if c != nil && c.Database.BackupDir != "" {
		return c.Database.BackupDir
	}
	return "data/backups"
}

// SchedulerConfig holds collection scheduler configuration.
type SchedulerConfig struct {
	MaxConcurrentCollects int     `yaml:"max_concurrent_collects" json:"max_concurrent_collects"`
	ErrorRateThreshold    float64 `yaml:"error_rate_threshold" json:"error_rate_threshold"`
	WatchdogInterval      int     `yaml:"watchdog_interval" json:"watchdog_interval"`
	WatchdogStaleCycles   int     `yaml:"watchdog_stale_cycles" json:"watchdog_stale_cycles"`
	WatchdogRestartCycles int     `yaml:"watchdog_restart_cycles" json:"watchdog_restart_cycles"`
}

func defaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{MaxConcurrentCollects: 50, ErrorRateThreshold: 0.1, WatchdogInterval: 30, WatchdogStaleCycles: 3, WatchdogRestartCycles: 10}
}

// AppConfig is the root configuration model.
type AppConfig struct {
	Server       ServerConfig           `yaml:"server" json:"server"`
	Database     DatabaseConfig         `yaml:"database" json:"database"`
	InfluxDB     InfluxDBConfig         `yaml:"influxdb" json:"influxdb"`
	MQTT         MQTTConfig             `yaml:"mqtt" json:"mqtt"`
	MqttServer   MqttServerConfig       `yaml:"mqtt_server" json:"mqtt_server"`
	ModbusSlave  ModbusSlaveConfig      `yaml:"modbus_slave" json:"modbus_slave"`
	Video        VideoConfig            `yaml:"video" json:"video"`
	Security     SecurityConfig         `yaml:"security" json:"security"`
	Logging      LoggingConfig          `yaml:"logging" json:"logging"`
	Simulator    SimulatorConfig        `yaml:"simulator" json:"simulator"`
	Notify       NotifyConfig           `yaml:"notify" json:"notify"`
	Platforms    map[string]interface{} `yaml:"platforms" json:"platforms"`
	SerialBridge SerialBridgeConfig     `yaml:"serial_bridge" json:"serial_bridge"`
	Preprocess   PreprocessGlobalConfig `yaml:"preprocess" json:"preprocess"`
	WebhookAuth  WebhookAuthConfig      `yaml:"webhook_auth" json:"webhook_auth"`
	MqttTLS      MqttTLSConfigModel     `yaml:"mqtt_tls" json:"mqtt_tls"`
	McpServer    McpServerConfig        `yaml:"mcp_server" json:"mcp_server"`
	Grafana      GrafanaConfig          `yaml:"grafana" json:"grafana"`
	Drivers      DriversConfig          `yaml:"drivers" json:"drivers"`
	Scheduler    SchedulerConfig        `yaml:"scheduler" json:"scheduler"`
	Backup       BackupConfig           `yaml:"backup" json:"backup"`
	AiInference  AiInferenceConfig      `yaml:"ai_inference" json:"ai_inference"`
	Cache        CacheConfig            `yaml:"cache" json:"cache"`
	OTAUpdateURL string                 `yaml:"ota_update_url" json:"ota_update_url"`

	ConfigVersion int `yaml:"-" json:"-"`
}

// DefaultAppConfig returns a config with all defaults applied.
func DefaultAppConfig() *AppConfig {
	return &AppConfig{
		Server:        defaultServerConfig(),
		Database:      defaultDatabaseConfig(),
		InfluxDB:      defaultInfluxDBConfig(),
		MQTT:          defaultMQTTConfig(),
		MqttServer:    defaultMqttServerConfig(),
		ModbusSlave:   defaultModbusSlaveConfig(),
		Video:         defaultVideoConfig(),
		Security:      defaultSecurityConfig(),
		Logging:       defaultLoggingConfig(),
		Simulator:     defaultSimulatorConfig(),
		Notify:        defaultNotifyConfig(),
		Platforms:     map[string]interface{}{},
		SerialBridge:  defaultSerialBridgeConfig(),
		Preprocess:    defaultPreprocessGlobalConfig(),
		WebhookAuth:   defaultWebhookAuthConfig(),
		MqttTLS:       defaultMqttTLSConfig(),
		McpServer:     McpServerConfig{},
		Grafana:       defaultGrafanaConfig(),
		Drivers:       DriversConfig{},
		Scheduler:     defaultSchedulerConfig(),
		Backup:        defaultBackupConfig(),
		AiInference:   defaultAiInferenceConfig(),
		Cache:         defaultCacheConfig(),
		OTAUpdateURL:  "",
		ConfigVersion: 0,
	}
}

// InsecureDefaultValues is a set of known insecure default secret values.
var InsecureDefaultValues = map[string]bool{
	"changeme": true, "change-me": true, "change_me": true,
	"please-change-me": true, "please_change_me": true,
	"pleasechangethis": true, "secret": true, "secret-key": true,
	"secretkey": true, "your-secret-key": true, "your_secret_key": true,
	"your-secret-key-here": true, "your-csrf-secret": true,
	"your_csrf_secret": true, "your-csrf-secret-here": true,
	"example": true, "default": true, "test": true,
	"placeholder": true, "admin": true, "password": true,
	"123456": true, "edgelite": true, "edgelite-secret": true,
	"admin@2026": true,
}

// SensitiveConfigPaths maps config dot-paths to human-readable labels.
var SensitiveConfigPaths = map[string]string{
	"mqtt.broker": "MQTT Broker地址", "mqtt.port": "MQTT Broker端口",
	"mqtt.username": "MQTT用户名", "mqtt.password": "MQTT密码",
	"influxdb.url": "InfluxDB地址", "influxdb.token": "InfluxDB Token",
	"influxdb.org": "InfluxDB组织", "influxdb.bucket": "InfluxDB Bucket",
	"security.secret_key": "安全密钥", "security.algorithm": "JWT算法",
	"security.access_token_expire_minutes": "AccessToken过期时间（分钟）",
	"security.refresh_token_expire_days":   "RefreshToken过期时间（天）",
	"security.max_token_ttl_days":          "Token TTL上限（天）",
	"server.webhook_api_key":               "Webhook API Key",
	"database.host":                        "数据库主机", "database.port": "数据库端口",
	"database.username": "数据库用户名", "database.password": "数据库密码",
}

// EncryptedSecretPaths is the list of fields that should be encrypted at rest.
var EncryptedSecretPaths = []string{
	"mqtt.password", "influxdb.token", "security.secret_key",
	"database.password", "notify.dingtalk.secret",
	"notify.email.smtp_password", "server.webhook_api_key",
	"video.pygbsentry.api_key", "mqtt_server.password",
	"webhook_auth.token", "webhook_auth.password",
	"grafana.api_key",
}

var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolveEnvVars recursively replaces ${VAR_NAME} patterns with env values.
func resolveEnvVars(obj interface{}) interface{} {
	switch v := obj.(type) {
	case map[string]interface{}:
		for k, val := range v {
			v[k] = resolveEnvVars(val)
		}
		return v
	case []interface{}:
		result := make([]interface{}, 0, len(v))
		for _, item := range v {
			resolved := resolveEnvVars(item)
			if s, ok := resolved.(string); ok && envVarPattern.MatchString(s) {
				continue // Skip unresolved placeholders in lists
			}
			result = append(result, resolved)
		}
		return result
	case string:
		return envVarPattern.ReplaceAllStringFunc(v, func(match string) string {
			inner := match[2 : len(match)-1]
			var varName, defaultVal string
			if idx := strings.Index(inner, ":"); idx >= 0 {
				varName = inner[:idx]
				defaultVal = inner[idx+1:]
			} else {
				varName = inner
			}
			val := os.Getenv(varName)
			if val != "" {
				return val
			}
			if defaultVal != "" {
				return defaultVal
			}
			lower := strings.ToLower(varName)
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
				strings.Contains(lower, "token") || strings.Contains(lower, "key") {
				return ""
			}
			return match
		})
	default:
		return obj
	}
}

// coerceEnvOverrideTypes 将环境变量覆盖值对齐到默认配置中对应字段的类型。
// 环境变量值永远是字符串；若直接深合并后由 yaml.Unmarshal 解码，
// int/bool/float 字段会报 "cannot unmarshal !!str `8080` into int" FATAL。
// 这里以（已加载的）默认配置为类型参照，做无损类型转换；
// 无法转换或无参照的值保持原样，交由后续校验报错。
func coerceEnvOverrideTypes(defaults, overrides interface{}) interface{} {
	om, ok := overrides.(map[string]interface{})
	if !ok {
		return overrides
	}
	dm, _ := defaults.(map[string]interface{})
	result := make(map[string]interface{}, len(om))
	for k, v := range om {
		dv, exists := dm[k]
		if !exists {
			result[k] = coerceEnvOverrideTypes(nil, v)
			continue
		}
		if vm, ok := v.(map[string]interface{}); ok {
			result[k] = coerceEnvOverrideTypes(dv, vm)
			continue
		}
		result[k] = coerceScalarToType(dv, v)
	}
	return result
}

// coerceScalarToType 按参照值的类型尝试转换字符串值。
func coerceScalarToType(defaultVal, newVal interface{}) interface{} {
	s, ok := newVal.(string)
	if !ok {
		return newVal
	}
	trimmed := strings.TrimSpace(s)
	switch defaultVal.(type) {
	case int:
		if n, err := strconv.Atoi(trimmed); err == nil {
			return n
		}
	case int64:
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return n
		}
	case float64:
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
	case float32:
		if f, err := strconv.ParseFloat(trimmed, 32); err == nil {
			return float32(f)
		}
	case bool:
		if b, err := strconv.ParseBool(trimmed); err == nil {
			return b
		}
	case uint:
		if n, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
			return uint(n)
		}
	case uint64:
		if n, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
			return n
		}
	}
	return newVal
}

// loadEnvOverrides reads EDGELITE_ prefixed env vars into a nested map.
func loadEnvOverrides() map[string]interface{} {
	overrides := make(map[string]interface{})
	prefix := "EDGELITE_"
	for _, kv := range os.Environ() {
		idx := strings.Index(kv, "=")
		if idx < 0 {
			continue
		}
		key, value := kv[:idx], kv[idx+1:]
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		raw := key[len(prefix):]
		var parsedValue interface{} = value
		if strings.Contains(value, ";") {
			parts := strings.Split(value, ";")
			list := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					list = append(list, p)
				}
			}
			parsedValue = list
		}
		if strings.Contains(raw, "__") {
			parts := strings.Split(strings.ToLower(raw), "__")
			d := overrides
			for i := 0; i < len(parts)-1; i++ {
				if _, ok := d[parts[i]].(map[string]interface{}); !ok {
					d[parts[i]] = make(map[string]interface{})
				}
				d = d[parts[i]].(map[string]interface{})
			}
			d[parts[len(parts)-1]] = parsedValue
		} else {
			overrides[strings.ToLower(raw)] = parsedValue
		}
	}
	return overrides
}

// deepMerge deeply merges override into base.
func deepMerge(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		if existing, ok := result[k]; ok {
			if existingMap, ok1 := existing.(map[string]interface{}); ok1 {
				if overrideMap, ok2 := v.(map[string]interface{}); ok2 {
					result[k] = deepMerge(existingMap, overrideMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

// deepCopyMap deeply copies a map.
// getNestedValue retrieves a value at a dot-separated path.
func getNestedValue(d map[string]interface{}, path string) interface{} {
	keys := strings.Split(path, ".")
	var current interface{} = d
	for _, key := range keys {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current, ok = m[key]
		if !ok {
			return nil
		}
	}
	return current
}

// setNestedValue sets a value at a dot-separated path.
func setNestedValue(d map[string]interface{}, path string, value interface{}) {
	keys := strings.Split(path, ".")
	current := d
	for i := 0; i < len(keys)-1; i++ {
		if _, ok := current[keys[i]].(map[string]interface{}); !ok {
			current[keys[i]] = make(map[string]interface{})
		}
		current = current[keys[i]].(map[string]interface{})
	}
	current[keys[len(keys)-1]] = value
}

// detectSensitiveChanges returns the list of sensitive config paths that changed.
func detectSensitiveChanges(oldDict, newDict map[string]interface{}) []string {
	var changed []string
	for path := range SensitiveConfigPaths {
		oldVal := getNestedValue(oldDict, path)
		newVal := getNestedValue(newDict, path)
		if fmt.Sprintf("%v", oldVal) != fmt.Sprintf("%v", newVal) {
			changed = append(changed, path)
		}
	}
	return changed
}

// ChangeCallback is a function called when config changes.
type ChangeCallback func(changeInfo map[string]interface{})

var (
	configChangeCallbacks []ChangeCallback
	callbacksLock         sync.Mutex
)

// RegisterConfigChangeCallback registers a callback for config changes.
func RegisterConfigChangeCallback(cb ChangeCallback) {
	callbacksLock.Lock()
	defer callbacksLock.Unlock()
	configChangeCallbacks = append(configChangeCallbacks, cb)
}

// notifyConfigChange notifies all registered callbacks.
func notifyConfigChange(changeInfo map[string]interface{}) {
	callbacksLock.Lock()
	cbs := make([]ChangeCallback, len(configChangeCallbacks))
	copy(cbs, configChangeCallbacks)
	callbacksLock.Unlock()
	for _, cb := range cbs {
		func() {
			defer func() {
				_ = recover()
			}()
			cb(changeInfo)
		}()
	}
}

// IsDevMode returns true if DEV_MODE is enabled.
// This is the authoritative implementation used across all packages.
func IsDevMode() bool {
	v := strings.ToLower(os.Getenv("DEV_MODE"))
	return v == "true" || v == "1" || v == "yes"
}

// isDevMode is kept for backward compatibility within the config package.
func isDevMode() bool {
	return IsDevMode()
}

// generateTokenURLSafe generates a cryptographically random URL-safe string.
// Fixed development secrets used in DEV_MODE when none is configured.
// Kept stable across boots so sessions/CSRF tokens survive restarts.
// These are >=32 chars and intentionally NOT in InsecureDefaultValues.
const (
	devFallbackSecretKey  = "edgelite-dev-fallback-secret-key-stable-across-boots"
	devFallbackCSRFSecret = "edgelite-dev-fallback-csrf-secret-stable-across-boots"
)

// LoadConfig loads configuration from YAML file, .env, and env overrides.
func LoadConfig(configPath string) (*AppConfig, error) {
	configPath = resolveConfigPath(configPath)
	setLoadedConfigPath(configPath)

	configData := map[string]interface{}{}

	data, err := os.ReadFile(configPath)
	if err == nil {
		if yamlErr := yaml.Unmarshal(data, &configData); yamlErr != nil {
			logrus.Warnf("config file %s parse error: %v, using defaults", configPath, yamlErr)
			configData = map[string]interface{}{}
		}
	}

	// Resolve ${VAR_NAME} env var interpolation
	configData = resolveEnvVars(configData).(map[string]interface{})

	// Apply env overrides (priority: env > .env > config.yaml)
	envOverrides := loadEnvOverrides()
	if len(envOverrides) > 0 {
		envOverrides = coerceEnvOverrideTypes(configData, envOverrides).(map[string]interface{})
		configData = deepMerge(configData, envOverrides)
	}

	// Dev mode: fall back to stable development secrets when none is configured.
	// A random value per boot would invalidate every session/CSRF token on restart,
	// so the fallback is a fixed, clearly-labelled constant (>=32 chars, not in
	// InsecureDefaultValues). Production must always configure real secrets.
	devMode := isDevMode()
	if devMode {
		sec, _ := configData["security"].(map[string]interface{})
		if sec == nil {
			sec = map[string]interface{}{}
		}
		if sec["secret_key"] == nil || sec["secret_key"] == "" {
			sec["secret_key"] = devFallbackSecretKey
			logrus.Warn("DEV_MODE: security.secret_key not configured, using fixed development fallback secret")
		}
		if sec["csrf_secret"] == nil || sec["csrf_secret"] == "" {
			sec["csrf_secret"] = devFallbackCSRFSecret
			logrus.Warn("DEV_MODE: security.csrf_secret not configured, using fixed development fallback secret")
		}
		configData["security"] = sec
	}

	// Marshal configData to YAML then unmarshal into AppConfig
	// This ensures env overrides (which are map[string]interface{}) are properly applied
	finalYAML, err := yaml.Marshal(configData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config data: %w", err)
	}
	cfg := DefaultAppConfig()
	if err := yaml.Unmarshal(finalYAML, cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Production safety: force-disable debug_api_enabled
	if !devMode && cfg.Server.DebugAPIEnabled {
		cfg.Server.DebugAPIEnabled = false
	}

	// Production safety: force-enable cookie_secure
	if !devMode && !cfg.Security.CookieSecure {
		cfg.Security.CookieSecure = true
	}

	// Validate secret key
	if err := validateSecretKey(cfg, devMode); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateSecretKey validates the JWT secret key is strong enough.
func validateSecretKey(cfg *AppConfig, devMode bool) error {
	if cfg.Security.SecretKey == "" {
		if devMode {
			// Dev mode allows empty (auto-generated), already handled above
			return nil
		}
		return fmt.Errorf("security.secret_key is empty — JWT tokens can be forged! Set EDGELITE_SECURITY__SECRET_KEY environment variable")
	}
	if strings.HasPrefix(cfg.Security.SecretKey, "${") && strings.HasSuffix(cfg.Security.SecretKey, "}") {
		return fmt.Errorf("security.secret_key contains unresolved env placeholder '%s' — JWT tokens can be forged", cfg.Security.SecretKey)
	}
	trimmed := strings.Trim(cfg.Security.SecretKey, "<>")
	lowerKey := strings.ToLower(cfg.Security.SecretKey)
	if InsecureDefaultValues[trimmed] || InsecureDefaultValues[lowerKey] {
		return fmt.Errorf("security.secret_key is set to a placeholder value '%s' — this is insecure", cfg.Security.SecretKey)
	}
	if len(cfg.Security.SecretKey) < 32 {
		return fmt.Errorf("security.secret_key is too short (%d chars, minimum 32)", len(cfg.Security.SecretKey))
	}
	return nil
}

// Global config instance
var (
	globalConfig *AppConfig
	configLock   sync.Mutex
	pathLock     sync.RWMutex
	// loadedPath is the file this process actually read its config from.
	loadedPath string
)

// LoadedConfigPath returns the config file the running instance loaded, or ""
// when LoadConfig has not run yet.
func LoadedConfigPath() string {
	pathLock.RLock()
	defer pathLock.RUnlock()
	return loadedPath
}

func setLoadedConfigPath(p string) {
	pathLock.Lock()
	defer pathLock.Unlock()
	loadedPath = p
}

// resolveConfigPath decides which file to read or write: an explicit argument
// wins, then the path this process loaded from, then EDGELITE_CONFIG, then the
// packaged default. Without the loaded-path step an instance started with
// --config /etc/edgelite/config.yaml would persist UI edits to
// ./configs/config.yaml and lose every one of them on the next boot.
func resolveConfigPath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if p := LoadedConfigPath(); p != "" {
		return p
	}
	if p := os.Getenv("EDGELITE_CONFIG"); p != "" {
		return p
	}
	return "configs/config.yaml"
}

// GetConfig returns the global config instance (thread-safe, lazy-init).
func GetConfig() *AppConfig {
	configLock.Lock()
	defer configLock.Unlock()
	if globalConfig == nil {
		cfg, err := LoadConfig("")
		if err != nil {
			// Fall back to defaults on error
			globalConfig = DefaultAppConfig()
		} else {
			globalConfig = cfg
		}
	}
	return globalConfig
}

// SetGlobalConfig sets the global config instance (for testing).
func SetGlobalConfig(cfg *AppConfig) {
	configLock.Lock()
	defer configLock.Unlock()
	globalConfig = cfg
}

// ResetConfig resets the global config (for testing).
func ResetConfig() {
	configLock.Lock()
	defer configLock.Unlock()
	globalConfig = nil
}

// SaveConfig persists config to YAML file atomically.
func SaveConfig(cfg *AppConfig, configPath string) error {
	configPath = resolveConfigPath(configPath)
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	tmpFile := configPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp config: %w", err)
	}
	if err := os.Rename(tmpFile, configPath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to rename config: %w", err)
	}
	configLock.Lock()
	globalConfig = cfg
	configLock.Unlock()
	return nil
}

// ReloadConfig reloads config from file and hot-updates the global instance.
func ReloadConfig(configPath string) (*AppConfig, []string, error) {
	if configPath == "" {
		configPath = os.Getenv("EDGELITE_CONFIG")
		if configPath == "" {
			configPath = "configs/config.yaml"
		}
	}
	newCfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, nil, err
	}

	old := GetConfig()
	oldMap := toMap(old)
	newMap := toMap(newCfg)
	changed := detectSensitiveChanges(oldMap, newMap)

	configLock.Lock()
	globalConfig = newCfg
	newCfg.ConfigVersion = old.ConfigVersion + 1
	configLock.Unlock()

	changeInfo := map[string]interface{}{
		"version":      newCfg.ConfigVersion,
		"changed_keys": changed,
	}
	notifyConfigChange(changeInfo)

	return newCfg, changed, nil
}

// toMap converts the config to a map using JSON field names, so API consumers
// can send it back and it decodes cleanly into AppConfig (json tags).
func toMap(cfg *AppConfig) map[string]interface{} {
	data, _ := json.Marshal(cfg)
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	return m
}

// LiveConfigMap returns the running config as a map keyed by the JSON field
// names, with real credential values. GetSanitizedConfig is not usable as a
// merge base because its masked secrets would be persisted as credentials.
func LiveConfigMap() map[string]interface{} {
	return toMap(GetConfig())
}

// MaskedSensitivePaths lists config dot-paths whose values are masked in API
// responses and must never be persisted back in masked/blank form.
var MaskedSensitivePaths = []string{
	"mqtt.password", "mqtt.username", "influxdb.token", "security.secret_key",
	"security.secret_key_previous", "security.csrf_secret", "security.key_id", "security.previous_key_id",
	"database.password", "database.username", "notify.dingtalk.secret",
	"notify.email.smtp_password", "notify.email.smtp_user",
	"server.webhook_api_key", "video.pygbsentry.api_key",
	"mqtt_server.password", "mqtt_server.username",
	"webhook_auth.token", "webhook_auth.password", "webhook_auth.username",
	"grafana.api_key",
}

func GetSanitizedConfig() map[string]interface{} {
	cfg := GetConfig()
	m := toMap(cfg)
	// Mask sensitive fields
	for _, path := range MaskedSensitivePaths {
		val := getNestedValue(m, path)
		if s, ok := val.(string); ok && len(s) > 2 {
			setNestedValue(m, path, string(s[0])+"***"+string(s[len(s)-1]))
		} else if s, ok := val.(string); ok && len(s) > 0 {
			setNestedValue(m, path, "***")
		}
	}
	m["_config_version"] = cfg.ConfigVersion
	return m
}

// RestoreMaskedSecrets replaces masked ("a***z"), empty, or absent sensitive
// values in a client-submitted config body with the current in-memory values,
// so that saving a sanitized config from the UI cannot wipe real credentials.
func RestoreMaskedSecrets(body map[string]interface{}) {
	existing := toMap(GetConfig())
	for _, path := range MaskedSensitivePaths {
		cur := getNestedValue(body, path)
		needsRestore := cur == nil
		if s, ok := cur.(string); ok {
			needsRestore = s == "" || strings.Contains(s, "***")
		}
		if needsRestore {
			if prev, ok := getNestedValue(existing, path).(string); ok && prev != "" {
				setNestedValue(body, path, prev)
			}
		}
	}
}

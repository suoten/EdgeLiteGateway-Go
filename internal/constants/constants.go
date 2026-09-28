// Package constants defines global constants shared across modules.
package constants

import (
	"reflect"
	"strconv"
	"strings"
)

// MQTT constants
const (
	MQTTKeepalive         = 60
	MQTTQueueMaxSize      = 10000
	MQTTReconnectDelay    = 5
	MQTTHeartbeatInterval = 1
	MQTTQueuePollInterval = 0.1
	// MQTTOfflineMaxRetries caps how often one offline row is retried before it
	// is dropped, so a poison message cannot block the backlog forever.
	MQTTOfflineMaxRetries = 100
)

// HTTP timeouts
const (
	HTTPTimeout          = 10.0
	OTADownloadTimeout   = 60.0
	NotifyHTTPTimeout    = 10.0
	NotifySMTPTimeout    = 15
)

// Device/serial timeouts
const (
	DeviceConnectTimeout         = 2.0
	SerialReadTimeout            = 0.1
	SerialWriteWait              = 0.1
	SerialPollInterval           = 0.05
	SerialRetryDelay             = 0.5
	SerialBridgeRawPollInterval  = 0.01
	SerialBridgeErrorRecovery    = 0.1
)

// InfluxDB
const (
	InfluxConnectTimeoutMs = 5000
	InfluxWriteTimeoutS    = 5.0
)

// Cache
const (
	CacheMaxSize        = 100_000
	CacheBatchLimit     = 500
	CacheFlushMaxRetries = 3
	EventBusMaxQueue    = 10000
	PreprocessorMaxPoints = 10000
	TokenRevocationMax  = 100000
	IntegrationSessionTTL = 300
)

// Rule/point cache
const (
	RuleCacheTTL       = 5.0
	PointValueCacheTTL = 300.0
	PointValueCacheMax = 10000
)

// Expression engine
const (
	ExpressionEvalMaxWorkers = 4
	ExpressionEvalTimeout    = 5.0
	ExpressionMaxLength      = 2048
	ExpressionBatchLimit     = 50
)

// Auth
const (
	AuthMaxAttempts          = 5
	AuthAttemptsLimit        = 10000
	AuthPasswordMaxLength   = 128
	AuthLoginWindowSeconds  = 300
	AuthResetIPWindowSeconds = 3600
	AuthResetUserWindowSeconds = 3600
	AuthResetIPMax           = 5
	AuthResetUserMax         = 3
	AuthResetIPMaxAttempts   = 3
)

// Pagination
const (
	DefaultPageSize  = 20
	MaxQuerySize     = 5000
	ExportQuerySize  = 10000
	ExportMaxRecords = 100_000
	MCPQuerySize     = 200
)

// Scheduler
const (
	SchedulerInterval          = 30
	MQTTForwarderReconnect      = 30
	MQTTDriverReconnect         = 30
	SparkplugReconnectMaxDelay  = 30.0
	PlatformReconnectMaxBackoff = 60
	IntegrationMaxSessions     = 10
	TokenRevocationDefaultTTL   = 86400
	CacheEvictionRatio         = 10
	ShortIDLength              = 16
)

// Auto backup
const (
	AutoBackupIntervalSeconds = 86400
	AutoBackupMaxRetention    = 50
	AutoBackupHour            = 2
)

// OTA / ONVIF / Allen-Bradley
const (
	OTADownloadChunk         = 8192
	ONVIFMulticastPort       = 3702
	ONVIFMulticastTTL        = 4
	AllenBradleyDefaultPort  = 44818 // EtherNet/IP (CIP); not SSH 2222
)

// North platform
const (
	NorthQueueMaxSize           = 10000
	NorthRetryInitialBackoff     = 1.0
	NorthRetryMaxBackoff         = 60.0
	NorthRetryMaxAttempts        = 10
	NorthPoolMaxConnections      = 5
	NorthPoolIdleTimeout         = 300
	NorthPoolProbeInterval       = 60
	NorthBatchDefaultSize        = 100
	NorthPeriodicDefaultInterval = 10.0
	NorthCompressThreshold       = 1024
	NorthDedupWindowSeconds      = 300
	NorthMessagePreviewMax       = 50
	NorthMQTT5Protocol           = 5
	NorthMQTT311Protocol         = 4

	NorthPersistDBName           = "north_queue.db"
	NorthPersistFlushInterval     = 5.0
	NorthPersistFlushBatchSize    = 100
	NorthPersistDequeueBatchSize  = 50
	NorthPersistMemoryThreshold   = 1000
	NorthPersistMemoryMax         = 100_000
	NorthPersistDiskMax           = 1_000_000
	NorthPersistSentTTLDays       = 7
	NorthPersistFailedTTLDays     = 30
	NorthPersistStuckSentTimeout = 300.0
)

// Ack tracker
const (
	AckTimeoutSeconds     = 30.0
	AckMaxRetries         = 10
	AckMaxInflight        = 10000
	AckDedupWindowSeconds = 86400
	AckTrackerFlushInterval = 5.0
	DLQAlertRatePerMinute = 10
	DLQMaxEntries         = 100_000
)

// Backpressure
const (
	BPLevelNormal          = "normal"
	BPLevelWarning         = "warning"
	BPLevelDanger          = "danger"
	BPLevelCritical        = "critical"
	BPThresholdWarning     = 0.6
	BPThresholdDanger      = 0.8
	BPThresholdCritical    = 0.95
	BPResumeThreshold      = 0.5
	BPResumeHoldSeconds    = 30.0
	BPSlowDownWarningFactor = 0.8
	BPSlowDownDangerFactor  = 0.5
	BPTokenBucketRate       = 1000.0
	BPTokenBucketBurst      = 2000.0
	BPHistoryMaxEvents      = 1000
)

// Circuit breaker
const (
	CBStateClosed            = "closed"
	CBStateOpen              = "open"
	CBStateHalfOpen          = "half_open"
	CBFailureThreshold       = 5
	CBErrorRateThreshold     = 0.5
	CBErrorRateWindowSeconds = 60.0
	CBInitialOpenDuration    = 30.0
	CBMaxOpenDuration        = 600.0
	CBOpenDurationMultiplier = 2.0
	CBHalfOpenProbeInterval  = 30.0
	CBHalfOpenSuccessThreshold = 3
	HPNormalInterval          = 30.0
	HPAbnormalInterval       = 5.0
	HPMQTTPingTimeout        = 10.0
	HPHTTPTimeout            = 5.0
	RCInitialBackoff         = 1.0
	RCMaxBackoff             = 60.0
	RCBackoffMultiplier      = 1.5
	RCJitterFactor           = 0.5
	RCGlobalMaxConcurrent    = 3
)

// Observability
var OBSLatencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0}

const (
	OBSLatencyMaxSamples   = 10000
	OBSTraceMaxTraces      = 100000
	OBSTraceDBName         = "message_traces.db"
	OBSAlertDBName          = "alert_events.db"
	OBSAlertCooldownSeconds = 300
	OBSAlertMaxEvents       = 10000
	OBSOTelSampleRate       = 0.01
	OBSOTelServiceName      = "edgelite-gateway"
	OBSK8sStartupDelaySeconds = 30
	OBSK8sLiveTimeoutSeconds  = 5
	OBSK8sReadyTimeoutSeconds  = 10
)

// ThingsBoard
const (
	TBHTTPPoolSize      = 50
	TBRPCTimeout        = 30.0
	TBDeviceCacheTTL    = 300
)

// SQLite
const (
	SQLiteBusyTimeout = 5000
	SQLiteWALMode     = "wal"
	SQLiteSynchronous = "NORMAL"
)

// KDF / Encryption
const (
	KDFSaltEnv        = "EDGELITE_KDF_SALT"
	EncryptionKeyEnv  = "EDGELITE_ENCRYPTION_KEY"
)

// Script sandbox
const (
	ScriptSandboxTimeout = 5.0
	ScriptSandboxMemoryMB = 64
	CustomMQTTMaxBrokers  = 10
)

// Cascade
const (
	CascadeTokenEnv    = "EDGELITE_CASCADE_TOKEN"
	CascadeHopLimit    = 16
	CascadeTokenTTL    = 300
	CascadeTokenHashLen = 16
)

// Logging
const (
	LogDir          = "logs"
	LogMaxBytes     = 50 * 1024 * 1024
	LogBackupCount  = 10
)

// ValidDeviceProtocols is the set of canonical device protocol names.
var ValidDeviceProtocols = map[string]bool{
	"modbus_tcp":     true,
	"modbus_rtu":     true,
	"simulator":      true,
	"mqtt_client":    true,
	"http_webhook":   true,
	"opc_ua":         true,
	"siemens_s7":     true,
	"mitsubishi_mc":   true,
	"omron_fins":     true,
	"allen_bradley":  true,
	"opc_da":         true,
	"onvif":          true,
	"modbus_slave":   true,
}

// ProtocolAliases maps legacy/short-form names to canonical names.
var ProtocolAliases = map[string]string{
	"modbus-tcp":   "modbus_tcp",
	"modbus-rtu":   "modbus_rtu",
	"opcua":        "opc_ua",
	"ethernet-ip":  "allen_bradley",
	"mqtt":         "mqtt_client",
	"opc-da":       "opc_da",
	"s7":           "siemens_s7",
	"mc":           "mitsubishi_mc",
	"fins":         "omron_fins",
	"ab":           "allen_bradley",
	"http":         "http_webhook",
	"opc_da_client": "opc_da",
}

// NormalizeProtocol returns the canonical protocol name, or "" if unknown.
func NormalizeProtocol(protocol string) string {
	if ValidDeviceProtocols[protocol] {
		return protocol
	}
	if canonical, ok := ProtocolAliases[protocol]; ok {
		return canonical
	}
	return ""
}

// TBAlarmSeverityMap maps alarm severity to ThingsBoard severity.
var TBAlarmSeverityMap = map[string]string{
	"critical":     "CRITICAL",
	"major":        "MAJOR",
	"minor":        "MINOR",
	"warning":      "WARNING",
	"indeterminate": "INDETERMINATE",
}

// QueuePollTimeout is the default poll timeout for queues.
const QueuePollTimeout = 1.0

// NumericAsFloat converts a collected or stored value to a float64 so it can
// take part in arithmetic: alarm thresholds, linkage conditions, statistics.
//
// It switches on reflect.Kind instead of a hand-written type list because the
// register drivers return uint16/int16/uint32 for a read. The per-package
// helpers this replaces only knew float64/float32/int/int32/int64, so every
// Modbus, S7 and FINS integer sample came back as "not a number" and a
// threshold rule on those tags silently never fired.
//
// Numeric text parses too: a tag typed as string can still carry a number, and
// answering "not a number" for "75.5" hides the reading rather than using it.
func NumericAsFloat(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	case reflect.Bool:
		if rv.Bool() {
			return 1, true
		}
		return 0, true
	case reflect.String:
		f, err := strconv.ParseFloat(strings.TrimSpace(rv.String()), 64)
		return f, err == nil
	}
	return 0, false
}

package constants

import (
	"testing"
)

func TestNormalizeProtocolCanonical(t *testing.T) {
	protocols := []string{
		"modbus_tcp", "modbus_rtu", "simulator", "mqtt_client",
		"http_webhook", "opc_ua", "siemens_s7", "mitsubishi_mc",
		"omron_fins", "allen_bradley", "opc_da", "onvif", "modbus_slave",
	}
	for _, p := range protocols {
		result := NormalizeProtocol(p)
		if result != p {
			t.Fatalf("NormalizeProtocol(%q) = %q, expected %q", p, result, p)
		}
	}
}

func TestNormalizeProtocolAlias(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"modbus-tcp", "modbus_tcp"},
		{"modbus-rtu", "modbus_rtu"},
		{"opcua", "opc_ua"},
		{"ethernet-ip", "allen_bradley"},
		{"mqtt", "mqtt_client"},
		{"opc-da", "opc_da"},
		{"s7", "siemens_s7"},
		{"mc", "mitsubishi_mc"},
		{"fins", "omron_fins"},
		{"ab", "allen_bradley"},
		{"http", "http_webhook"},
		{"opc_da_client", "opc_da"},
	}
	for _, tt := range tests {
		result := NormalizeProtocol(tt.input)
		if result != tt.expected {
			t.Fatalf("NormalizeProtocol(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestNormalizeProtocolUnknown(t *testing.T) {
	result := NormalizeProtocol("unknown_protocol")
	if result != "" {
		t.Fatalf("NormalizeProtocol('unknown_protocol') = %q, expected empty", result)
	}
}

func TestNormalizeProtocolEmpty(t *testing.T) {
	result := NormalizeProtocol("")
	if result != "" {
		t.Fatalf("NormalizeProtocol('') = %q, expected empty", result)
	}
}

func TestValidDeviceProtocols(t *testing.T) {
	if len(ValidDeviceProtocols) < 13 {
		t.Fatalf("Expected at least 13 valid protocols, got %d", len(ValidDeviceProtocols))
	}
	if !ValidDeviceProtocols["modbus_tcp"] {
		t.Fatal("modbus_tcp should be valid")
	}
	if !ValidDeviceProtocols["simulator"] {
		t.Fatal("simulator should be valid")
	}
}

func TestProtocolAliases(t *testing.T) {
	if len(ProtocolAliases) < 12 {
		t.Fatalf("Expected at least 12 aliases, got %d", len(ProtocolAliases))
	}
	if ProtocolAliases["mqtt"] != "mqtt_client" {
		t.Fatal("mqtt alias should map to mqtt_client")
	}
}

func TestTBAlarmSeverityMap(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"critical", "CRITICAL"},
		{"major", "MAJOR"},
		{"minor", "MINOR"},
		{"warning", "WARNING"},
		{"indeterminate", "INDETERMINATE"},
	}
	for _, tt := range tests {
		if TBAlarmSeverityMap[tt.input] != tt.expected {
			t.Fatalf("TBAlarmSeverityMap[%q] = %q, expected %q",
				tt.input, TBAlarmSeverityMap[tt.input], tt.expected)
		}
	}
}

func TestOBSLatencyBuckets(t *testing.T) {
	if len(OBSLatencyBuckets) < 10 {
		t.Fatalf("Expected at least 10 latency buckets, got %d", len(OBSLatencyBuckets))
	}
	// Verify ascending order
	for i := 1; i < len(OBSLatencyBuckets); i++ {
		if OBSLatencyBuckets[i] <= OBSLatencyBuckets[i-1] {
			t.Fatalf("Latency buckets should be ascending: %f <= %f at index %d",
				OBSLatencyBuckets[i], OBSLatencyBuckets[i-1], i)
		}
	}
}

func TestBackpressureLevels(t *testing.T) {
	if BPLevelNormal != "normal" {
		t.Fatalf("Expected 'normal', got %s", BPLevelNormal)
	}
	if BPLevelWarning != "warning" {
		t.Fatalf("Expected 'warning', got %s", BPLevelWarning)
	}
	if BPLevelDanger != "danger" {
		t.Fatalf("Expected 'danger', got %s", BPLevelDanger)
	}
	if BPLevelCritical != "critical" {
		t.Fatalf("Expected 'critical', got %s", BPLevelCritical)
	}
}

func TestCircuitBreakerStates(t *testing.T) {
	if CBStateClosed != "closed" {
		t.Fatalf("Expected 'closed', got %s", CBStateClosed)
	}
	if CBStateOpen != "open" {
		t.Fatalf("Expected 'open', got %s", CBStateOpen)
	}
	if CBStateHalfOpen != "half_open" {
		t.Fatalf("Expected 'half_open', got %s", CBStateHalfOpen)
	}
}

func TestSQLiteConstants(t *testing.T) {
	if SQLiteBusyTimeout != 5000 {
		t.Fatalf("Expected 5000, got %d", SQLiteBusyTimeout)
	}
	if SQLiteWALMode != "wal" {
		t.Fatalf("Expected 'wal', got %s", SQLiteWALMode)
	}
	if SQLiteSynchronous != "NORMAL" {
		t.Fatalf("Expected 'NORMAL', got %s", SQLiteSynchronous)
	}
}

func TestAuthConstants(t *testing.T) {
	if AuthMaxAttempts != 5 {
		t.Fatalf("Expected 5, got %d", AuthMaxAttempts)
	}
	if AuthPasswordMaxLength != 128 {
		t.Fatalf("Expected 128, got %d", AuthPasswordMaxLength)
	}
}

func TestPaginationConstants(t *testing.T) {
	if DefaultPageSize != 20 {
		t.Fatalf("Expected 20, got %d", DefaultPageSize)
	}
	if MaxQuerySize != 5000 {
		t.Fatalf("Expected 5000, got %d", MaxQuerySize)
	}
}

func TestCacheConstants(t *testing.T) {
	if CacheMaxSize != 100_000 {
		t.Fatalf("Expected 100000, got %d", CacheMaxSize)
	}
	if EventBusMaxQueue != 10000 {
		t.Fatalf("Expected 10000, got %d", EventBusMaxQueue)
	}
}

func TestKDFEnvironmentVariables(t *testing.T) {
	if KDFSaltEnv != "EDGELITE_KDF_SALT" {
		t.Fatalf("Expected 'EDGELITE_KDF_SALT', got %s", KDFSaltEnv)
	}
	if EncryptionKeyEnv != "EDGELITE_ENCRYPTION_KEY" {
		t.Fatalf("Expected 'EDGELITE_ENCRYPTION_KEY', got %s", EncryptionKeyEnv)
	}
}

func TestQueuePollTimeout(t *testing.T) {
	if QueuePollTimeout != 1.0 {
		t.Fatalf("Expected 1.0, got %f", QueuePollTimeout)
	}
}

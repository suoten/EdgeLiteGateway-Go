// Package drivers provides industrial protocol driver implementations for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/drivers/ package.
// It includes drivers for Modbus TCP/RTU, Siemens S7, Mitsubishi MC, Omron FINS,
// Allen-Bradley, OPC UA/DA, MQTT, HTTP Webhook, ONVIF, and a Simulator.
package drivers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// ErrDiscoveryUnsupported is returned by drivers whose protocol has no
// discovery the gateway performs. Answering with an empty list instead made the
// UI report "scan finished, no devices found" for a protocol that never scanned
// anything, which is indistinguishable from an empty plant.
var ErrDiscoveryUnsupported = errors.New("protocol discovery is not implemented for this driver")

// Driver is the interface that all protocol drivers must implement.
type Driver interface {
	// Name returns the protocol name (e.g. "modbus_tcp", "siemens_s7")
	Name() string
	// Connect establishes a connection to the device
	Connect(ctx context.Context) error
	// Disconnect closes the connection
	Disconnect() error
	// IsConnected returns whether the device is connected
	IsConnected() bool
	// ReadPoints reads all configured points from the device
	ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error)
	// WritePoint writes a value to a point
	WritePoint(ctx context.Context, point string, value interface{}) error
	// Discover finds devices on the network. A driver that cannot must return
	// ErrDiscoveryUnsupported rather than an empty list.
	Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error)
	// HealthCheck checks if the device is responsive
	HealthCheck(ctx context.Context) error
}

// BrowseEntry is one child of a node in a device's address space, as reported by
// the protocol's own browse/discovery service rather than from stored config.
type BrowseEntry struct {
	NodeID      string `json:"node_id"`
	BrowseName  string `json:"browse_name"`
	DisplayName string `json:"display_name"`
	NodeClass   string `json:"node_class"`
	DataType    string `json:"data_type,omitempty"`
	Writable    bool   `json:"writable"`
	// IsContainer means the node is a folder-like object worth browsing into; the
	// browse service does not report child counts, so this is not "has children".
	IsContainer bool `json:"is_container"`
}

// NodeBrowser is implemented by drivers whose protocol can enumerate a server's
// address space (OPC UA Browse). Endpoints that offer node picking must bind to
// this, so an unsupported protocol reports a capability gap instead of an empty
// list that looks like "the server has no nodes".
type NodeBrowser interface {
	BrowseChildren(ctx context.Context, nodeID string) ([]BrowseEntry, error)
}

// TypedWritePointer is implemented by drivers whose wire format depends on the
// point's declared data type: a Modbus register pair holds either an int32 or a
// float32, and the JSON number arriving from the API cannot tell them apart.
// The device service knows the point definition, so it hands the type down.
type TypedWritePointer interface {
	WritePointTyped(ctx context.Context, point string, value interface{}, dataType string) error
}

// WireAddressWriter is implemented by drivers whose write target is the point's
// wire address - Modbus "HR620", S7 "DB1.DBW0", an OPC UA NodeID - rather than
// the operator's point name. It exists because the read path receives whole
// point definitions while WritePoint receives one string: a driver that parses
// that string as an address cannot use a name, and the service is the only place
// that holds the name-to-address mapping. Without this, writing a point named
// "pf_mb" at address "HR620" reached the driver as "pf_mb" and every PLC write
// from the UI failed with "invalid point address".
type WireAddressWriter interface {
	WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error
}

// BaseDriver provides common functionality for all drivers.
// It includes health statistics tracking and circuit breaker integration
// to ensure industrial-grade reliability across all protocol drivers.
type BaseDriver struct {
	mu             sync.Mutex
	connected      bool
	deviceID       string
	config         map[string]interface{}
	healthStats    *HealthStatsManager
	circuitBreaker *CircuitBreaker
	reconnectMgr   *ReconnectManager
}

// SetHealthStatsManager injects the health stats manager.
func (b *BaseDriver) SetHealthStatsManager(mgr *HealthStatsManager) {
	b.healthStats = mgr
}

// SetCircuitBreaker injects the circuit breaker.
func (b *BaseDriver) SetCircuitBreaker(cb *CircuitBreaker) {
	b.circuitBreaker = cb
}

// SetReconnectManager injects the reconnect manager.
func (b *BaseDriver) SetReconnectManager(rm *ReconnectManager) {
	b.reconnectMgr = rm
}

// GetHealthStatsManager returns the health stats manager.
func (b *BaseDriver) GetHealthStatsManager() *HealthStatsManager {
	return b.healthStats
}

// GetCircuitBreaker returns the circuit breaker.
func (b *BaseDriver) GetCircuitBreaker() *CircuitBreaker {
	return b.circuitBreaker
}

// IsCircuitOpen checks if the circuit breaker is open for this device.
// Returns false (allow) if no circuit breaker is set.
func (b *BaseDriver) IsCircuitOpen() bool {
	if b.circuitBreaker == nil {
		return false
	}
	return !b.circuitBreaker.AllowRequest(b.deviceID)
}

// LatencyNotMeasured is what a driver passes to RecordReadSuccess when there is
// no device round trip to time: a subscriber-style read that answers out of a
// local cache, or a keep-alive probe. Recording 0 instead stored a sample that
// every panel then drew as a perfect 0 ms link.
const LatencyNotMeasured = -1.0

// ElapsedMs reports how long work started at `started` took, in milliseconds.
// Nanosecond resolution matters: an in-memory read finishes well inside a
// microsecond, and converting through Milliseconds() or Microseconds() truncated
// it to a flat 0 -- or, when the wrong unit was divided out, reported it a
// thousand times too large.
func ElapsedMs(started time.Time) float64 {
	return float64(time.Since(started).Nanoseconds()) / 1e6
}

// RecordReadSuccess records a successful read with latency. Pass
// LatencyNotMeasured when the read has no measurable round trip.
func (b *BaseDriver) RecordReadSuccess(latencyMs float64) {
	if b.healthStats != nil {
		b.healthStats.RecordReadSuccess(b.deviceID, latencyMs)
	}
	if b.circuitBreaker != nil {
		b.circuitBreaker.RecordSuccess(b.deviceID)
	}
}

// RecordReadFailure records a failed read.
func (b *BaseDriver) RecordReadFailure() {
	if b.healthStats != nil {
		b.healthStats.RecordReadFailure(b.deviceID)
	}
	if b.circuitBreaker == nil {
		return
	}
	// The stats manager and the breaker are injected independently, so healthStats
	// can be nil here; calling a method on that nil pointer took the whole gateway
	// down on the first failed read of a device.
	var consecFailures int64
	if b.healthStats != nil {
		if stats := b.healthStats.GetHealthStats(b.deviceID); stats != nil {
			stats.mu.Lock()
			consecFailures = stats.ConsecutiveFailures
			stats.mu.Unlock()
		}
	}
	b.circuitBreaker.RecordFailure(b.deviceID, consecFailures)
}

// RecordWriteSuccess records a successful write.
func (b *BaseDriver) RecordWriteSuccess() {
	if b.healthStats != nil {
		b.healthStats.RecordWriteSuccess(b.deviceID)
	}
	if b.circuitBreaker != nil {
		b.circuitBreaker.RecordSuccess(b.deviceID)
	}
}

// RecordWriteFailure records a failed write.
func (b *BaseDriver) RecordWriteFailure() {
	if b.healthStats != nil {
		b.healthStats.RecordWriteFailure(b.deviceID)
	}
	if b.circuitBreaker != nil {
		b.circuitBreaker.RecordFailure(b.deviceID, 1)
	}
}

// SetConnectionState sets the connection state for this device.
func (b *BaseDriver) SetConnectionState(state ConnectionState, reason string) {
	if b.healthStats != nil {
		b.healthStats.SetConnectionState(b.deviceID, state, reason)
	}
}

// IsConnected returns whether the driver is connected.
func (b *BaseDriver) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// SetConnected sets the connection state.
func (b *BaseDriver) SetConnected(connected bool) {
	b.mu.Lock()
	b.connected = connected
	b.mu.Unlock()
}

// DeviceID returns the device ID.
func (b *BaseDriver) DeviceID() string {
	return b.deviceID
}

// SetDeviceID sets the device ID.
func (b *BaseDriver) SetDeviceID(id string) {
	b.deviceID = id
}

// GetConfig returns the driver configuration.
func (b *BaseDriver) GetConfig() map[string]interface{} {
	return b.config
}

// SetConfig sets the driver configuration.
func (b *BaseDriver) SetConfig(config map[string]interface{}) {
	b.config = config
}

// GetConfigString safely gets a string value from the config map.
func GetConfigString(config map[string]interface{}, key string, defaultVal string) string {
	if v, ok := config[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		if f, ok := v.(float64); ok {
			return fmt.Sprintf("%g", f)
		}
	}
	return defaultVal
}

// GetConfigInt safely gets an int value from the config map.
func GetConfigInt(config map[string]interface{}, key string, defaultVal int) int {
	if v, ok := config[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return defaultVal
}

// GetConfigFloat safely gets a float64 value from the config map.
func GetConfigFloat(config map[string]interface{}, key string, defaultVal float64) float64 {
	if v, ok := config[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return defaultVal
}

// GetConfigBool safely gets a bool value from the config map.
func GetConfigBool(config map[string]interface{}, key string, defaultVal bool) bool {
	if v, ok := config[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			switch strings.ToLower(strings.TrimSpace(b)) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off", "":
				return false
			}
		case float64:
			return b != 0
		case int:
			return b != 0
		}
	}
	return defaultVal
}

// configKeyAliases maps, per canonical protocol, a driver-read config key to the
// frontend device-form key names that carry the same value. The form in
// frontend/src/constants/protocolConfig.ts stores user input under keys that do
// not always match what the driver constructors read (the form uses "ip" while the
// Allen-Bradley/ONVIF drivers read "host"; "baudrate" vs "baud_rate"; FINS
// "dest_node" vs "node"; RTU "port" for the serial path vs "serial_port"). Without
// reconciliation the driver silently falls back to its default and the operator's
// settings are ignored.
//
// Reconciliation is fill-on-missing only: a value already present under the driver
// key is never overwritten, so hand-authored configs using driver-native keys keep
// working and already-persisted devices are repaired on load.
var configKeyAliases = map[string]map[string][]string{
	"allen_bradley": {
		"host":     {"ip"},
		"plc_type": {"plc_model"},
	},
	"onvif": {
		"host": {"ip"},
	},
	"omron_fins": {
		"node": {"dest_node", "source_node"},
		"unit": {"unit_no"},
	},
	"modbus_rtu": {
		"baud_rate": {"baudrate"},
		"slave_id":  {"unit_id"},
		"stop_bits": {"stopbits"},
		"data_bits": {"bytesize"},
	},
	"mqtt_client": {
		// The form labels this single field "topic" and treats it as required,
		// while the driver subscribes to "subscribe_topic". Without the bridge the
		// operator's topic was dropped and only the derived per-point topics were
		// ever subscribed.
		"subscribe_topic": {"topic"},
	},
}

// NormalizeDriverConfig copies frontend-keyed values onto the driver-native keys
// each driver actually reads and normalizes value vocabularies (parity). Called
// from CreateDriver so it applies to every driver instance, from the UI or a file.
func NormalizeDriverConfig(protocol string, config map[string]interface{}) {
	if config == nil {
		return
	}
	// The RTU serial device path: the form labels it "port" (a string such as "COM3"
	// or "/dev/ttyUSB0") while the driver reads "serial_port" for the path and "port"
	// for an integer TCP bridge port. Only bridge a string "port" so an integer TCP
	// port is left untouched.
	if protocol == "modbus_rtu" {
		if isEmptyValue(config["serial_port"]) {
			if s, ok := config["port"].(string); ok && s != "" {
				config["serial_port"] = s
			}
		}
	}
	for dst, aliases := range configKeyAliases[protocol] {
		if !isEmptyValue(config[dst]) {
			continue
		}
		for _, a := range aliases {
			if v, ok := config[a]; ok && !isEmptyValue(v) {
				config[dst] = v
				break
			}
		}
	}
	// Parity vocabulary: the RTU form uses single letters (N/E/O) but the driver
	// documents "none"/"even"/"odd".
	if p, ok := config["parity"].(string); ok {
		switch strings.ToLower(p) {
		case "n", "none":
			config["parity"] = "none"
		case "e", "even":
			config["parity"] = "even"
		case "o", "odd":
			config["parity"] = "odd"
		}
	}
}

// isEmptyValue reports whether a config value is absent or an empty string. A
// non-string numeric zero is intentionally NOT empty (0 is a valid rack/node/unit).
func isEmptyValue(v interface{}) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return s == ""
	}
	return false
}

// Registry is the central driver registry. It maps protocol names to driver factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]DriverFactory
	// Global infrastructure components injected into every driver
	healthStats    *HealthStatsManager
	circuitBreaker *CircuitBreaker
	reconnectMgr   *ReconnectManager
}

// InfrastructureInjector is an interface for drivers that can receive
// global infrastructure components (health stats, circuit breaker, etc.).
// BaseDriver already implements this interface, so all drivers embedding
// BaseDriver automatically satisfy it.
type InfrastructureInjector interface {
	SetHealthStatsManager(mgr *HealthStatsManager)
	SetCircuitBreaker(cb *CircuitBreaker)
	SetReconnectManager(rm *ReconnectManager)
}

// DriverFactory creates a new driver instance for a device.
type DriverFactory func(deviceID string, config map[string]interface{}) (Driver, error)

var globalRegistry = &Registry{
	factories: make(map[string]DriverFactory),
}

// GetRegistry returns the global driver registry.
func GetRegistry() *Registry {
	return globalRegistry
}

// SetInfrastructure injects global health stats, circuit breaker, and reconnect manager
// into the registry. These will be automatically injected into every driver created.
func (r *Registry) SetInfrastructure(hsm *HealthStatsManager, cb *CircuitBreaker, rm *ReconnectManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthStats = hsm
	r.circuitBreaker = cb
	r.reconnectMgr = rm
	logrus.Info("Driver registry infrastructure initialized (health stats, circuit breaker, reconnect manager)")
}

// Register registers a driver factory for a protocol.
func (r *Registry) Register(protocol string, factory DriverFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[protocol] = factory
	logrus.WithField("protocol", protocol).Debug("Driver registered")
}

// CreateDriver creates a driver instance for a protocol.
// It automatically injects global infrastructure (health stats, circuit breaker,
// reconnect manager) into the created driver if available.
func (r *Registry) CreateDriver(protocol, deviceID string, config map[string]interface{}) (Driver, error) {
	r.mu.RLock()
	factory, ok := r.factories[protocol]
	hsm := r.healthStats
	cb := r.circuitBreaker
	rm := r.reconnectMgr
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}
	// Reconcile frontend device-form config keys onto the keys the driver reads
	// before construction (see NormalizeDriverConfig). Operate on a copy so the
	// caller's map (e.g. device.Config, read by other goroutines) is never mutated.
	normalized := make(map[string]interface{}, len(config)+4)
	for k, v := range config {
		normalized[k] = v
	}
	NormalizeDriverConfig(protocol, normalized)
	driver, err := factory(deviceID, normalized)
	if err != nil {
		return nil, err
	}
	// Inject infrastructure into the driver if it supports it
	if injector, ok := driver.(InfrastructureInjector); ok {
		if hsm != nil {
			injector.SetHealthStatsManager(hsm)
		}
		if cb != nil {
			injector.SetCircuitBreaker(cb)
		}
		if rm != nil {
			injector.SetReconnectManager(rm)
		}
	}
	return driver, nil
}

// SupportedProtocols returns a list of registered protocol names.
func (r *Registry) SupportedProtocols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, 0, len(r.factories))
	for k := range r.factories {
		result = append(result, k)
	}
	return result
}

// IsSupported returns whether a protocol is registered.
func (r *Registry) IsSupported(protocol string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[protocol]
	return ok
}

// Global infrastructure instances shared across all drivers.
var (
	globalHealthStats    = NewHealthStatsManager()
	globalCircuitBreaker = NewCircuitBreaker(DefaultCircuitBreakerConfig())
	globalReconnectMgr   = NewReconnectManager()
)

// GetHealthStatsManager returns the global health stats manager.
func GetHealthStatsManager() *HealthStatsManager {
	return globalHealthStats
}

// GetCircuitBreaker returns the global circuit breaker.
func GetCircuitBreaker() *CircuitBreaker {
	return globalCircuitBreaker
}

// GetReconnectManager returns the global reconnect manager.
func GetReconnectManager() *ReconnectManager {
	return globalReconnectMgr
}

// dataTypeIntRange returns the values a declared integer data type can carry.
// ok is false for the floating-point and compound types, and for an undeclared
// one, where the driver has nothing to check against. The aliases follow the
// encode tables of the drivers that read them (a Modbus/FINS "long" is 32 bits).
func dataTypeIntRange(dataType string) (min, max float64, ok bool) {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "int8", "sbyte":
		return math.MinInt8, math.MaxInt8, true
	case "uint8", "byte":
		return 0, math.MaxUint8, true
	case "int16", "short":
		return math.MinInt16, math.MaxInt16, true
	case "uint16", "word":
		return 0, math.MaxUint16, true
	case "int32", "long", "dint":
		return math.MinInt32, math.MaxInt32, true
	case "uint32", "dword":
		return 0, math.MaxUint32, true
	}
	return 0, 0, false
}

// checkWriteInRange refuses a numeric value the declared type cannot represent.
//
// Every PLC driver narrows the JSON number it received into a fixed-width
// register, and Go's conversion wraps rather than failing: writing 70000 to a
// uint16 point stored 4464 and the API answered success, which put a number
// nobody asked for into running equipment. A refused write is recoverable; a
// silently wrong one is not.
func checkWriteInRange(value interface{}, dataType string) error {
	min, max, ok := dataTypeIntRange(dataType)
	if !ok {
		return nil
	}
	f, isNum := toFloat64(value)
	if !isNum {
		return nil
	}
	if f < min || f > max {
		return fmt.Errorf("value %v out of range for %s (%v to %v)", f, strings.ToLower(strings.TrimSpace(dataType)), min, max)
	}
	return nil
}

// RegisterAll registers all built-in drivers.
func RegisterAll() {
	r := GetRegistry()
	r.SetInfrastructure(globalHealthStats, globalCircuitBreaker, globalReconnectMgr)
	r.Register("modbus_tcp", NewModbusTCPDriver)
	r.Register("modbus_rtu", NewModbusRTUDriver)
	r.Register("simulator", NewSimulatorDriver)
	r.Register("siemens_s7", NewS7Driver)
	r.Register("mitsubishi_mc", NewMCDriver)
	r.Register("omron_fins", NewFINSDriver)
	r.Register("allen_bradley", NewABDriver)
	r.Register("opc_ua", NewOPCUADriver)
	r.Register("opc_da", NewOPCDADriver)
	r.Register("mqtt_client", NewMQTTClientDriver)
	r.Register("http_webhook", NewHTTPWebhookDriver)
	r.Register("onvif", NewONVIFDriver)
	r.Register("modbus_slave", NewModbusSlaveDriver)
	logrus.Info("All drivers registered with infrastructure integration")
}

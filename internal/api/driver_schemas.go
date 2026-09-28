package api

import (
	"github.com/labstack/echo/v4"
)

// schemaField describes one device-config field of a protocol driver.
type schemaField struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Type        string      `json:"type"`
	Required    bool        `json:"required"`
	Secret      bool        `json:"secret,omitempty"`
	Description string      `json:"description,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Options     []string    `json:"options,omitempty"`
}

type schemaBody struct {
	Description string        `json:"description,omitempty"`
	Fields      []schemaField `json:"fields"`
}

// driverConfigSchemas documents the config keys each Go driver actually
// reads (see the GetConfig* calls in internal/drivers). Labels mirror the
// driverField i18n maps in DriverConfig.vue so the UI can translate them.
var driverConfigSchemas = map[string]schemaBody{
	"modbus_tcp": {
		Description: "Modbus TCP industrial standard protocol for reading/writing PLC/ineter coils and registers",
		Fields: []schemaField{
			{Name: "host", Label: "IP Address", Type: "string", Required: true, Description: "PLC or gateway IP address"},
			{Name: "port", Label: "Port", Type: "integer", Description: "Modbus TCP port, default 502", Default: 502},
			{Name: "slave_id", Label: "Slave ID", Type: "integer", Description: "Device slave address (Unit ID), usually 1", Default: 1},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"modbus_rtu": {
		Description: "Modbus RTU serial protocol for RS-485/RS-232 devices",
		Fields: []schemaField{
			{Name: "serial_port", Label: "Serial Port", Type: "string", Required: true, Description: "Serial port device path"},
			{Name: "baud_rate", Label: "Baud Rate", Type: "integer", Description: "Communication baud rate", Default: 9600},
			{Name: "parity", Label: "Parity", Type: "string", Description: "Serial parity check", Default: "none", Options: []string{"none", "odd", "even"}},
			{Name: "data_bits", Label: "Data Bits", Type: "integer", Description: "Number of data bits (7 or 8)", Default: 8},
			{Name: "stop_bits", Label: "Stop Bits", Type: "integer", Description: "Number of stop bits", Default: 1},
			{Name: "slave_id", Label: "Slave ID", Type: "integer", Description: "Modbus slave address, usually 1-247", Default: 1},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"siemens_s7": {
		Description: "Siemens S7 protocol for S7-200/300/400/1200/1500 PLCs",
		Fields: []schemaField{
			{Name: "host", Label: "Host", Type: "string", Required: true, Description: "PLC IP address"},
			{Name: "port", Label: "Port", Type: "integer", Description: "S7 communication port (default 102)", Default: 102},
			{Name: "rack", Label: "Rack", Type: "integer", Description: "PLC rack number", Default: 0},
			{Name: "slot", Label: "Slot", Type: "integer", Description: "PLC slot number", Default: 2},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"mitsubishi_mc": {
		Description: "Mitsubishi MC protocol for Q/FX series PLCs",
		Fields: []schemaField{
			{Name: "host", Label: "IP Address", Type: "string", Required: true, Description: "PLC IP address"},
			{Name: "port", Label: "Port", Type: "integer", Description: "MC protocol port", Default: 5000},
			{Name: "plc_type", Label: "PLC Type", Type: "string", Description: "Mitsubishi PLC series", Default: "Q"},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"omron_fins": {
		Description: "Omron FINS protocol for CJ/CP series PLCs",
		Fields: []schemaField{
			{Name: "host", Label: "IP Address", Type: "string", Required: true, Description: "Omron PLC IP address"},
			{Name: "port", Label: "Port", Type: "integer", Description: "FINS protocol port (default 9600)", Default: 9600},
			{Name: "node", Label: "Source Node", Type: "integer", Description: "Local gateway FINS node number", Default: 0},
			{Name: "unit", Label: "Unit No.", Type: "integer", Description: "FINS unit number", Default: 0},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"allen_bradley": {
		Description: "Allen-Bradley CIP protocol for ControlLogix/CompactLogix",
		Fields: []schemaField{
			{Name: "host", Label: "IP Address", Type: "string", Required: true, Description: "AB PLC IP address"},
			{Name: "port", Label: "Port", Type: "integer", Description: "EtherNet/IP port", Default: 44818},
			{Name: "plc_type", Label: "PLC Model", Type: "string", Description: "Allen-Bradley PLC model (ControlLogix/CompactLogix/Micro800)", Default: "ControlLogix"},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"opc_ua": {
		Description: "OPC UA industrial communication protocol",
		Fields: []schemaField{
			{Name: "endpoint", Label: "Server URL", Type: "string", Required: true, Description: "OPC UA server address", Default: "opc.tcp://127.0.0.1:4840"},
			{Name: "security", Label: "Security Mode", Type: "string", Description: "OPC UA security mode", Default: "None", Options: []string{"None", "Basic128Rsa15", "Basic256", "Basic256Sha256"}},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"opc_da": {
		Description: "OPC DA classic COM-based protocol. Requires a Windows COM/DCOM bridge, which this build does not include: devices using it stay offline.",
		Fields: []schemaField{
			{Name: "server", Label: "Server", Type: "string", Required: true, Description: "OPC DA server ProgID", Default: "Matrikon.OPC.Simulation"},
			{Name: "node", Label: "Node", Type: "string", Description: "OPC DA server host node", Default: "localhost"},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"mqtt_client": {
		Description: "MQTT message queue telemetry transport protocol",
		Fields: []schemaField{
			{Name: "broker", Label: "Broker", Type: "string", Required: true, Description: "MQTT broker address", Default: "localhost"},
			{Name: "port", Label: "Port", Type: "integer", Description: "MQTT broker port (default 1883, TLS 8883)", Default: 1883},
			{Name: "username", Label: "Username", Type: "string", Description: "MQTT authentication username"},
			{Name: "password", Label: "Password", Type: "string", Secret: true, Description: "MQTT authentication password"},
			{Name: "topic_prefix", Label: "Topic", Type: "string", Description: "MQTT subscribe/publish topic", Default: "edgelite"},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Connection and read timeout", Default: 2},
		},
	},
	"http_webhook": {
		Description: "HTTP Webhook for receiving external data push",
		Fields: []schemaField{
			{Name: "api_key", Label: "Auth Token", Type: "string", Secret: true, Description: "Bearer token or API key for authentication"},
		},
	},
	"onvif": {
		Description: "ONVIF IP camera protocol",
		Fields: []schemaField{
			{Name: "host", Label: "Camera IP", Type: "string", Required: true, Description: "ONVIF camera IP address", Default: "127.0.0.1"},
			{Name: "port", Label: "Port", Type: "integer", Description: "ONVIF port (default 80)", Default: 80},
			{Name: "username", Label: "Username", Type: "string", Description: "ONVIF device authentication username", Default: "admin"},
			{Name: "password", Label: "Password", Type: "string", Secret: true, Description: "ONVIF device authentication password"},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "ONVIF request timeout in seconds", Default: 2},
		},
	},
	"modbus_slave": {
		Description: "Modbus TCP slave/server: exposes the gateway register map to external SCADA/HMI masters",
		Fields: []schemaField{
			{Name: "host", Label: "Bind Address", Type: "string", Description: "IP address to bind the Modbus TCP slave server", Default: "0.0.0.0"},
			{Name: "port", Label: "Port", Type: "integer", Description: "Modbus TCP slave port", Default: 5020},
			{Name: "byte_order", Label: "Byte Order", Type: "string", Description: "Register order of multi-register values", Default: "ABCD", Options: []string{"ABCD", "CDAB", "BADC", "DCBA"}},
			{Name: "holding_size", Label: "Holding Registers", Type: "integer", Description: "Number of holding registers", Default: 1000},
			{Name: "input_size", Label: "Input Registers", Type: "integer", Description: "Number of input registers", Default: 1000},
			{Name: "coil_size", Label: "Coils", Type: "integer", Description: "Number of coils", Default: 1000},
			{Name: "discrete_size", Label: "Discrete Inputs", Type: "integer", Description: "Number of discrete inputs", Default: 1000},
			{Name: "allowed_ips", Label: "Allowed Peers", Type: "string", Description: "Comma-separated master IPs or CIDR blocks; empty accepts any peer"},
			{Name: "max_connections", Label: "Max Connections", Type: "integer", Description: "Concurrent masters served; 0 is unlimited", Default: 0},
			{Name: "abuse_threshold", Label: "Request Budget", Type: "integer", Description: "Requests a peer may send per window before it is banned; 0 disables", Default: 0},
			{Name: "abuse_window", Label: "Budget Window (s)", Type: "integer", Description: "Length of the request-budget window", Default: 60},
			{Name: "ban_duration", Label: "Ban Duration (s)", Type: "integer", Description: "How long an over-budget peer is refused", Default: 300},
		},
	},
	"simulator": {
		Description: "Built-in simulator for testing and demo",
		Fields: []schemaField{
			{Name: "period", Label: "Update Period (s)", Type: "number", Description: "Seconds between generated samples", Default: 60},
			{Name: "sim_mode", Label: "Waveform", Type: "string", Description: "Value generator", Default: "sine", Options: []string{"sine", "random", "ramp", "triangle", "sawtooth", "square", "step", "random_walk", "fixed", "formula"}},
			{Name: "formula", Label: "Formula", Type: "string", Description: "Expression evaluated each sample when the waveform is 'formula'"},
			{Name: "value_range_min", Label: "Range Min", Type: "number", Description: "Lower bound of generated values", Default: 0},
			{Name: "value_range_max", Label: "Range Max", Type: "number", Description: "Upper bound of generated values", Default: 100},
			{Name: "noise_amplitude", Label: "Noise Amplitude", Type: "number", Description: "Random deviation added to each sample", Default: 0},
			{Name: "trend_drift", Label: "Trend Drift", Type: "number", Description: "Per-sample drift applied to the waveform", Default: 0},
			{Name: "write_hold_seconds", Label: "Write Hold (s)", Type: "number", Description: "How long a written value overrides the waveform", Default: 10},
			{Name: "fault_simulation", Label: "Fault Injection", Type: "string", Description: "Injected failure mode applied to reads", Default: "none", Options: []string{"none", "timeout", "disconnect", "data_error", "random"}},
			{Name: "fault_rate", Label: "Fault Rate (%)", Type: "number", Description: "Percentage of samples that take the injected failure", Default: 0},
			{Name: "timeout", Label: "Timeout (s)", Type: "number", Description: "Read timeout", Default: 5},
		},
	},
}

// driverCapabilities reports per-driver capability flags derived from the actual
// Go driver methods. A flag is only set when the implementation really performs
// the operation: a driver whose Discover returns an empty slice is NOT discover,
// an OPC UA write without MonitoredItems is NOT subscribe, and ReadPoints that
// issues one request per point has NO batch read. The UI renders these flags as
// the "supported" column, so an over-claim advertises behaviour the gateway will
// never exhibit.
var driverCapabilities = map[string]map[string]bool{
	"modbus_tcp":    {"read": true, "write": true, "discover": true},
	"modbus_rtu":    {"read": true, "write": true},
	"siemens_s7":    {"read": true, "write": true},
	"mitsubishi_mc": {"read": true, "write": true},
	"omron_fins":    {"read": true, "write": true},
	"allen_bradley": {"read": true, "write": true},
	"opc_ua":        {"read": true, "write": true, "discover": true},
	"opc_da":        {"read": false, "write": false}, // COM/DCOM only: drivers.ErrOPCDAUnsupported
	"mqtt_client":   {"read": true, "write": true, "subscribe": true},
	"http_webhook":  {"read": true, "write": true},
	"onvif":         {"read": true, "discover": true}, // WritePoint: drivers.ErrONVIFWriteUnsupported
	"modbus_slave":  {"read": true, "write": true},
	"simulator":     {"read": true, "write": true},
}

// driverConstraint is one limit an operator cannot infer from the capability
// flags alone. Code is the i18n key the frontend translates ("driverConstraint."
// + Code); Message stays as the English text for a client that has no entry for
// the code, so the constraint is never silently dropped.
type driverConstraint struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// driverConstraints states what a driver cannot do even though it loads and is
// selectable. Every entry here is backed by the driver returning an explicit
// error or by a config key it never reads; the UI shows them next to the config
// schema, which is the only place an operator learns that an ONVIF write or an
// OPC DA subscription will not happen.
var driverConstraints = map[string][]driverConstraint{
	"onvif": {{
		Type:    "capability",
		Code:    "onvif_read_only",
		Message: "ONVIF is read-only in this build: WritePoint returns an error because no PTZ or media control command is implemented",
	}},
	"opc_da": {{
		Type:    "platform",
		Code:    "opc_da_unsupported",
		Message: "OPC DA needs the Windows COM/DCOM registry, which this build does not link; connect, read, write and discovery all fail",
	}},
	"opc_ua": {{
		Type:    "capability",
		Code:    "opc_ua_polled",
		Message: "Subscriptions and MonitoredItems are not implemented: points are read once per collect interval, so subscription settings have no effect",
	}},
	"modbus_rtu": {{
		Type:    "protocol_note",
		Code:    "rtu_shared_bus",
		Message: "RS-485 is a half-duplex shared bus: every unit id behind one port waits behind the same polls",
	}},
	"modbus_slave": {{
		Type:    "protocol_note",
		Code:    "slave_server_mode",
		Message: "The gateway listens as a Modbus slave and serves its own register map; it does not poll a remote PLC",
	}},
	"mqtt_client": {{
		Type:    "interop_risk",
		Code:    "mqtt_311_only",
		Message: "Only MQTT 3.1.1 is spoken: shared subscriptions, user properties and the v5 reason codes are not available",
	}},
}

// driverExperimental marks drivers whose implementation is partial enough that
// picking them should be a deliberate choice. It mirrors the drivers that answer
// a supported operation with an explicit "not implemented" error.
var driverExperimental = map[string]bool{
	"onvif":  true,
	"opc_da": true,
}

// handleGetDriverConfigSchema returns the config schema of one driver.
func handleGetDriverConfigSchema(c echo.Context) error {
	driverName := c.Param("driver_name")
	schema, ok := driverConfigSchemas[driverName]
	if !ok {
		schema = schemaBody{Fields: []schemaField{}}
	}
	required := make([]string, 0, len(schema.Fields))
	optional := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f.Required {
			required = append(required, f.Name)
		} else {
			optional = append(optional, f.Name)
		}
	}
	return OK(c, map[string]interface{}{
		"driver":        driverName,
		"config_schema": schema,
		"schema":        schema,
		"required":      required,
		"optional":      optional,
	})
}

// handleGetDriverMeta returns per-driver metadata for the driver table.
func handleGetDriverMeta(c echo.Context) error {
	names := registeredDrivers()
	metas := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		// An unlisted driver gets no capabilities: the table then shows "-", and
		// claiming "read" would advertise a driver the gateway may not even run.
		caps := driverCapabilities[name]
		if caps == nil {
			caps = map[string]bool{}
		}
		constraints := driverConstraints[name]
		if constraints == nil {
			constraints = []driverConstraint{}
		}
		metas = append(metas, map[string]interface{}{
			"name":         name,
			"capabilities": caps,
			"experimental": driverExperimental[name],
			"constraints":  constraints,
		})
	}
	return OK(c, metas)
}

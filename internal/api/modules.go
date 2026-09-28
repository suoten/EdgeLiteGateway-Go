package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"go.bug.st/serial"

	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// RegisterMQTTServerRoutes registers MQTT server management API routes.
func RegisterMQTTServerRoutes(g *echo.Group) {
	g.GET("/status", handleGetMQTTServerStatus, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartMQTTServer, requirePermission(security.PermSystemConfig))
	g.POST("/stop", handleStopMQTTServer, requirePermission(security.PermSystemConfig))
	g.GET("/clients", handleListMQTTClients, requirePermission(security.PermSystemConfig))
	g.POST("/clients/:client_id/kick", handleKickMQTTClient, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetMQTTServerConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateMQTTServerConfig, requirePermission(security.PermSystemConfig))
}

func handleGetMQTTServerStatus(c echo.Context) error {
	cont := GetContainer()
	cfg := config.GetConfig()
	running := cont.MqttServer != nil && cont.MqttServer.IsRunning()
	clients := 0
	if running {
		if n, ok := cont.MqttServer.GetStats()["client_count"].(int); ok {
			clients = n
		}
	}
	return OK(c, map[string]interface{}{
		"enabled": cfg.MqttServer.Enabled,
		"running": running,
		"host":    cfg.MqttServer.Host,
		"port":    cfg.MqttServer.Port,
		"clients": clients,
	})
}

func handleStartMQTTServer(c echo.Context) error {
	if err := startEmbeddedMqttServer(GetContainer()); err != nil {
		return BadRequest(c, err.Error())
	}
	if err := setEmbeddedServiceEnabled("mqtt_server", true); err != nil {
		logrus.WithError(err).Warn("Failed to persist service enabled state")
	}
	return OK(c, map[string]string{"status": "started"})
}

func handleStopMQTTServer(c echo.Context) error {
	stopEmbeddedMqttServer(GetContainer())
	if err := setEmbeddedServiceEnabled("mqtt_server", false); err != nil {
		logrus.WithError(err).Warn("Failed to persist service disabled state")
	}
	return OK(c, map[string]string{"status": "stopped"})
}

func handleListMQTTClients(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.MqttServer == nil || !cont.MqttServer.IsRunning() {
		return OK(c, []engine.ClientInfo{})
	}
	return OK(c, cont.MqttServer.ListClients())
}

func handleKickMQTTClient(c echo.Context) error {
	clientID := c.Param("client_id")
	cont := GetContainer()
	if cont == nil || cont.MqttServer == nil || !cont.MqttServer.IsRunning() {
		return BadRequest(c, "ERR_MQTT_SERVER_NOT_RUNNING")
	}
	if err := cont.MqttServer.KickClient(clientID); err != nil {
		return NotFound(c, err.Error())
	}
	logrus.WithField("client_id", clientID).Info("MQTT client kicked")
	return OK(c, map[string]string{"client_id": clientID, "status": "kicked"})
}

func handleGetMQTTServerConfig(c echo.Context) error {
	cfg := config.GetConfig()
	// Security: mask the password field to prevent credential leakage in API responses
	sanitized := cfg.MqttServer
	if sanitized.Password != "" {
		sanitized.Password = security.MaskString(sanitized.Password)
	}
	return OK(c, sanitized)
}

func handleUpdateMQTTServerConfig(c echo.Context) error {
	// Callers round-trip what GET /config returned, and that carries masked
	// credentials ("g***y"); binding straight into the struct persisted the mask
	// as the real password, after which no client could connect.
	var body map[string]interface{}
	if err := c.Bind(&body); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if body == nil {
		body = map[string]interface{}{}
	}
	wrapped := map[string]interface{}{"mqtt_server": body}
	config.RestoreMaskedSecrets(wrapped)
	section, err := json.Marshal(wrapped["mqtt_server"])
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	var req config.MqttServerConfig
	if err := json.Unmarshal(section, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	wasRunning := cont.MqttServer != nil && cont.MqttServer.IsRunning()

	cfg := config.GetConfig()
	previous := cfg.MqttServer
	cfg.MqttServer = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	// A running broker keeps its old listener and credentials, so saving alone
	// used to report success while port and auth changes took effect only after
	// a process restart. Roll back to the previous section if the new one cannot
	// listen, so the file never claims a state the broker is not in.
	if wasRunning {
		stopEmbeddedMqttServer(cont)
		if err := startEmbeddedMqttServer(cont); err != nil {
			cfg.MqttServer = previous
			if rbErr := config.SaveConfig(cfg, ""); rbErr != nil {
				logrus.WithError(rbErr).Error("Failed to roll back MQTT server config")
			}
			if rbErr := startEmbeddedMqttServer(cont); rbErr != nil {
				logrus.WithError(rbErr).Error("Failed to restart MQTT server with the previous config")
			}
			logrus.WithError(err).Warn("MQTT server could not start with the new config")
			return InternalError(c, "ERR_MQTT_SERVER_RESTART_FAILED")
		}
	}

	// Security: mask the password in the response
	if req.Password != "" {
		req.Password = security.MaskString(req.Password)
	}
	return OK(c, req)
}

// RegisterModbusSlaveRoutes registers Modbus slave API routes.
func RegisterModbusSlaveRoutes(g *echo.Group) {
	g.GET("/status", handleGetModbusSlaveStatus, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartModbusSlave, requirePermission(security.PermSystemConfig))
	g.POST("/stop", handleStopModbusSlave, requirePermission(security.PermSystemConfig))
	g.GET("/registers", handleGetModbusRegisters, requirePermission(security.PermSystemConfig))
	g.PUT("/registers/:type/:offset", handleSetModbusRegister, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetModbusSlaveConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateModbusSlaveConfig, requirePermission(security.PermSystemConfig))
}

func handleGetModbusSlaveStatus(c echo.Context) error {
	cont := GetContainer()
	cfg := config.GetConfig()
	return OK(c, map[string]interface{}{
		"enabled": cfg.ModbusSlave.Enabled,
		"running": cont.ModbusSlaveServer != nil && cont.ModbusSlaveServer.IsConnected(),
		"host":    cfg.ModbusSlave.Host,
		"port":    cfg.ModbusSlave.Port,
	})
}

func handleStartModbusSlave(c echo.Context) error {
	cont := GetContainer()
	if err := startEmbeddedModbusSlave(cont); err != nil {
		return BadRequest(c, err.Error())
	}
	if err := setEmbeddedServiceEnabled("modbus_slave", true); err != nil {
		logrus.WithError(err).Warn("Failed to persist service enabled state")
	}
	return OK(c, map[string]string{"status": "started"})
}

func handleStopModbusSlave(c echo.Context) error {
	stopEmbeddedModbusSlave(GetContainer())
	if err := setEmbeddedServiceEnabled("modbus_slave", false); err != nil {
		logrus.WithError(err).Warn("Failed to persist service disabled state")
	}
	return OK(c, map[string]string{"status": "stopped"})
}

func handleGetModbusRegisters(c echo.Context) error {
	d, ok := GetContainer().ModbusSlaveServer.(*drivers.ModbusSlaveDriver)
	if !ok || d == nil {
		return ServiceUnavailable(c, "ERR_MODBUS_SLAVE_NOT_RUNNING")
	}
	return OK(c, map[string]interface{}{
		"holding":  d.SnapshotHolding(),
		"input":    d.SnapshotInput(),
		"coil":     d.SnapshotCoils(),
		"discrete": d.SnapshotDiscrete(),
	})
}

func handleSetModbusRegister(c echo.Context) error {
	regType := c.Param("type")
	offsetStr := c.Param("offset")
	var req struct {
		Value interface{} `json:"value"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	offset, err := strconv.Atoi(offsetStr)
	if err != nil || offset < 0 {
		return BadRequest(c, "ERR_COMMON_VALIDATION: invalid offset")
	}
	d, ok := GetContainer().ModbusSlaveServer.(*drivers.ModbusSlaveDriver)
	if !ok || d == nil {
		return ServiceUnavailable(c, "ERR_MODBUS_SLAVE_NOT_RUNNING")
	}
	written, err := d.SetRegister(regType, offset, req.Value)
	if err != nil {
		return BadRequest(c, err.Error())
	}
	recordAudit(c, "modbus_slave_register_set", "modbus_slave", regType, "success",
		map[string]interface{}{"offset": offset, "value": written})
	logrus.WithFields(logrus.Fields{"type": regType, "offset": offset, "value": written}).Info("Modbus register set")
	return OK(c, map[string]interface{}{"type": regType, "offset": offset, "value": written})
}

func handleGetModbusSlaveConfig(c echo.Context) error {
	cfg := config.GetConfig()
	return OK(c, cfg.ModbusSlave)
}

func handleUpdateModbusSlaveConfig(c echo.Context) error {
	var req config.ModbusSlaveConfig
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// Persist the updated config section to the config file
	cfg := config.GetConfig()
	cfg.ModbusSlave = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	return OK(c, req)
}

// RegisterSerialBridgeRoutes registers serial bridge API routes.
func RegisterSerialBridgeRoutes(g *echo.Group) {
	g.GET("/status", handleGetSerialBridgeStatus, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartSerialBridge, requirePermission(security.PermSystemConfig))
	g.POST("/stop", handleStopSerialBridge, requirePermission(security.PermSystemConfig))
	g.GET("/ports", handleListSerialPorts, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetSerialBridgeConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateSerialBridgeConfig, requirePermission(security.PermSystemConfig))
}

func handleGetSerialBridgeStatus(c echo.Context) error {
	cont := GetContainer()
	cfg := config.GetConfig()
	running := false
	var bridgeStats map[string]interface{}
	if cont.SerialBridge != nil {
		bridgeStats = cont.SerialBridge.GetStatus()
		if st, ok := bridgeStats["started"].(bool); ok {
			running = st
		}
	}
	resp := map[string]interface{}{
		"enabled":  cfg.SerialBridge.Enabled,
		"running":  running,
		"port":     cfg.SerialBridge.SerialPort,
		"baud":     cfg.SerialBridge.BaudRate,
		"tcp_port": cfg.SerialBridge.TCPPort,
	}
	if bridgeStats != nil {
		resp["stats"] = bridgeStats
	}
	return OK(c, resp)
}

func handleStartSerialBridge(c echo.Context) error {
	if err := startEmbeddedSerialBridge(GetContainer()); err != nil {
		return BadRequest(c, err.Error())
	}
	if err := setEmbeddedServiceEnabled("serial_bridge", true); err != nil {
		logrus.WithError(err).Warn("Failed to persist service enabled state")
	}
	return OK(c, map[string]string{"status": "started"})
}

func handleStopSerialBridge(c echo.Context) error {
	stopEmbeddedSerialBridge(GetContainer())
	if err := setEmbeddedServiceEnabled("serial_bridge", false); err != nil {
		logrus.WithError(err).Warn("Failed to persist service disabled state")
	}
	return OK(c, map[string]string{"status": "stopped"})
}

// serialPortsEnumerator is the OS query this handler answers from; tests replace
// it because the real answer depends on the machine running them.
var serialPortsEnumerator = serial.GetPortsList

// handleListSerialPorts reports the serial devices this host actually has.
//
// The endpoint used to answer a fixed {"COM1","COM2","COM3","/dev/ttyS0",
// "/dev/ttyUSB0"} - a list that is not true on any single machine (it mixes
// Windows and Linux names), was never produced by a query, and read to a caller
// as detected hardware. When the OS cannot be asked the caller is told so,
// instead of being handed the invention.
func handleListSerialPorts(c echo.Context) error {
	found, err := serialPortsEnumerator()
	if err != nil {
		return ServiceUnavailable(c, "ERR_SERIAL_PORTS_ENUMERATION_FAILED: the operating system could not list its serial devices: "+err.Error())
	}
	ports := append([]string{}, found...)
	sort.Strings(ports)
	resp := map[string]interface{}{
		"ports": ports,
		"total": len(ports),
	}
	if cfg := config.GetConfig(); cfg != nil && cfg.SerialBridge.SerialPort != "" {
		// The port the bridge is configured for is reported even when it is not
		// present, so the page can say the configured device is missing rather
		// than silently offering an empty chooser.
		configured := cfg.SerialBridge.SerialPort
		resp["configured"] = configured
		present := false
		for _, p := range ports {
			if strings.EqualFold(p, configured) {
				present = true
				break
			}
		}
		resp["configured_present"] = present
	}
	if len(ports) == 0 {
		resp["note"] = "the host reports no serial devices; a bridge started on a configured port will fail to open it"
	}
	return OK(c, resp)
}

func handleGetSerialBridgeConfig(c echo.Context) error {
	cfg := config.GetConfig()
	return OK(c, cfg.SerialBridge)
}

// serialBridgeIsRunning reports whether this process has a bridge carrying
// bytes right now, as opposed to a section that merely says enabled.
func serialBridgeIsRunning(cont *ServiceContainer) bool {
	if cont == nil || cont.SerialBridge == nil {
		return false
	}
	st, _ := cont.SerialBridge.GetStatus()["started"].(bool)
	return st
}

// validateSerialBridgeSection rejects a section the bridge could not run with,
// so a save cannot answer success for settings that would only fail at Start.
func validateSerialBridgeSection(sec config.SerialBridgeConfig) error {
	if err := engine.ValidateSerialBridgeConfig(engineSerialBridgeConfig(sec)); err != nil {
		return err
	}
	// The engine sees only ListenAddr, where 0 reads as "any free port"; the
	// operator's tcp_port has to be a port they can actually dial.
	if sec.TCPPort < 1 || sec.TCPPort > 65535 {
		return fmt.Errorf("ERR_SVC_CONFIG_INVALID: serial_bridge.tcp_port is %d; expected a port between 1 and 65535", sec.TCPPort)
	}
	return nil
}

func handleUpdateSerialBridgeConfig(c echo.Context) error {
	var req config.SerialBridgeConfig
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	cfg := config.GetConfig()
	// The enabled flag belongs to the service toggle, not to this form; keeping
	// the live value means a page that loaded before the bridge was started
	// cannot switch it off in the file on save.
	req.Enabled = cfg.SerialBridge.Enabled

	if err := validateSerialBridgeSection(req); err != nil {
		return BadRequest(c, err.Error())
	}

	previous := cfg.SerialBridge
	wasRunning := serialBridgeIsRunning(cont)
	cfg.SerialBridge = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		cfg.SerialBridge = previous
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	// A bridge that is already running holds the device it opened and the TCP
	// port it bound, so saving alone used to answer success while the change
	// reached nobody until a process restart.
	if err := restartSerialBridgeForConfig(wasRunning, previous); err != nil {
		return BadRequest(c, err.Error())
	}
	return OK(c, req)
}

// restartSerialBridgeForConfig brings a running bridge onto the section that was
// just stored, and rolls that section back when the new settings cannot run, so
// the config file never claims more than the service is actually doing.
func restartSerialBridgeForConfig(wasRunning bool, previous config.SerialBridgeConfig) error {
	if !wasRunning {
		return nil
	}
	cont := GetContainer()
	stopEmbeddedSerialBridge(cont)
	if err := startEmbeddedSerialBridge(cont); err != nil {
		cfg := config.GetConfig()
		cfg.SerialBridge = previous
		if rbErr := config.SaveConfig(cfg, ""); rbErr != nil {
			logrus.WithError(rbErr).Error("Failed to roll back serial bridge config")
		}
		if rbErr := startEmbeddedSerialBridge(cont); rbErr != nil {
			logrus.WithError(rbErr).Error("Failed to restart serial bridge with the previous config")
		}
		logrus.WithError(err).Warn("Serial bridge could not restart with the new config")
		return fmt.Errorf("ERR_SVC_SERIAL_BRIDGE_RESTART_FAILED: %w", err)
	}
	return nil
}

// RegisterIntegrationRoutes registers integration platform API routes.
func RegisterIntegrationRoutes(g *echo.Group) {
	g.GET("/status", handleGetIntegrationStatus, requirePermission(security.PermIntegrationManage))
	g.POST("/test", handleTestIntegration, requirePermission(security.PermIntegrationManage))
	g.GET("/endpoints", handleListIntegrationEndpoints, requirePermission(security.PermIntegrationManage))
	g.POST("/endpoints", handleCreateIntegrationEndpoint, requirePermission(security.PermIntegrationManage))
	g.PUT("/endpoints/:id", handleUpdateIntegrationEndpoint, requirePermission(security.PermIntegrationManage))
	g.DELETE("/endpoints/:id", handleDeleteIntegrationEndpoint, requirePermission(security.PermIntegrationManage))
}

// handleGetIntegrationStatus counts what the gateway really has: platform rows in
// the live config and connections the northbound manager reports. It used to
// answer enabled:false / endpoints:0 / active_connections:0 no matter what was
// configured, so a working platform looked broken to whoever asked.
func handleGetIntegrationStatus(c echo.Context) error {
	cfg := config.GetConfig()
	configured := 0
	enabledPlatforms := 0
	for _, raw := range cfg.Platforms {
		configured++
		if m, ok := raw.(map[string]interface{}); ok {
			if b, _ := m["enabled"].(bool); b {
				enabledPlatforms++
			}
		}
	}
	mgr := getPlatformManager()
	if mgr == nil {
		return ServiceUnavailable(c, "ERR_INTEG_BACKHAUL_NOT_READY")
	}
	connected := mgr.ConnectedNames()
	sort.Strings(connected)
	return OK(c, map[string]interface{}{
		"enabled":             enabledPlatforms > 0,
		"endpoints":           configured,
		"enabled_endpoints":   enabledPlatforms,
		"active_connections":  len(connected),
		"connected_platforms": connected,
	})
}

// handleTestIntegration performs the test it claims to perform, through the
// same throwaway dial /platforms/:name/test uses. The old handler returned
// success:true without contacting anything, which is the one answer a test
// endpoint must never give.
func handleTestIntegration(c echo.Context) error {
	var req struct {
		Name   string                 `json:"name"`
		Type   string                 `json:"type"`
		Config map[string]interface{} `json:"config"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	name := req.Name
	if name == "" {
		name = req.Type
	}
	if name == "" {
		return BadRequest(c, "ERR_INTEG_TEST_NAME_REQUIRED")
	}
	return OK(c, runPlatformTest(name, req.Config))
}

// handleListIntegrationEndpoints and its three write siblings used to fabricate
// a registry: create echoed the request back, update promised "updated":true,
// delete logged a deletion that never happened, and list insisted there were no
// endpoints while /platforms held the real ones. The gateway has exactly one
// northbound registry (cfg.Platforms, driven by the manager), so these routes
// refuse and point at it instead of maintaining a second, disconnected store.
func integrationEndpointsRefuse(c echo.Context) error {
	code := "ERR_INTEG_ENDPOINTS_UNSUPPORTED_USE_PLATFORMS"
	return ErrorCode(c, http.StatusNotImplemented, code, code)
}

func handleListIntegrationEndpoints(c echo.Context) error {
	return integrationEndpointsRefuse(c)
}

func handleCreateIntegrationEndpoint(c echo.Context) error {
	return integrationEndpointsRefuse(c)
}

func handleUpdateIntegrationEndpoint(c echo.Context) error {
	return integrationEndpointsRefuse(c)
}

func handleDeleteIntegrationEndpoint(c echo.Context) error {
	return integrationEndpointsRefuse(c)
}

// RegisterMCPRoutes registers MCP (Model Context Protocol) server API routes.
func RegisterMCPRoutes(g *echo.Group) {
	g.GET("/status", handleGetMCPStatus, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartMCP, requirePermission(security.PermSystemConfig))
	g.POST("/stop", handleStopMCP, requirePermission(security.PermSystemConfig))
	g.GET("/tools", handleListMCPTools, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetMCPConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateMCPConfig, requirePermission(security.PermSystemConfig))
}

func handleGetMCPStatus(c echo.Context) error {
	// The MCP service is an in-process tool registry with no listener, so the
	// operator's toggle is the running state. This used to answer running:false
	// regardless of the config, and start/stop answered "started" for a service
	// that has no lifecycle to drive.
	cont := GetContainer()
	enabled := mcpServiceEnabled()
	status := map[string]interface{}{
		"enabled":       enabled,
		"running":       enabled,
		"has_lifecycle": false,
		"tool_count":    len(mcpToolList(cont)),
	}
	if !enabled {
		// tool_count stays non-zero because the tools are registered; saying so
		// keeps the page from reading that number as the service being up.
		status["refuses_calls"] = true
		status["note"] = "the tools are registered but every call is refused until mcp_server is enabled"
	}
	return OK(c, status)
}

// mcpServiceName is the key both the console's service toggle and the persisted
// setting use for this service.
const mcpServiceName = "mcp_server"

// mcpServiceEnabled answers whether the MCP tool surface is switched on. An
// explicit toggle (in memory, then the persisted setting) wins; with none, the
// config file's mcp_server.enabled decides - which is the only reader that field
// ever had, so a gateway configured with mcp_server.enabled: false now serves no
// tools instead of registering them and answering every call anyway.
func mcpServiceEnabled() bool {
	cont := GetContainer()
	if cont != nil {
		cont.ServiceEnabledMu.RLock()
		enabled, explicit := cont.ServiceEnabledMap[mcpServiceName]
		cont.ServiceEnabledMu.RUnlock()
		if explicit {
			return enabled
		}
		if cont.Database != nil {
			if stored, err := cont.Database.GetSetting("service_enabled_" + mcpServiceName); err == nil {
				switch stored {
				case "1":
					return true
				case "0":
					return false
				}
			}
		}
	}
	if cfg := config.GetConfig(); cfg != nil {
		return cfg.McpServer.Enabled
	}
	return false
}

// setMCPServiceEnabled records the toggle the way the /services/:name/enable
// routes do, so the two ways of switching this service agree and survive a
// restart.
func setMCPServiceEnabled(enabled bool) {
	cont := GetContainer()
	if cont == nil {
		return
	}
	cont.ServiceEnabledMu.Lock()
	cont.ServiceEnabledMap[mcpServiceName] = enabled
	cont.ServiceEnabledMu.Unlock()
	if cont.Database != nil {
		value := "0"
		if enabled {
			value = "1"
		}
		_ = cont.Database.SetSetting("service_enabled_"+mcpServiceName, value)
	}
}

// mcpToolList snapshots the tool registry in a stable order.
func mcpToolList(cont *ServiceContainer) []map[string]interface{} {
	if cont == nil || cont.MCPService == nil {
		return []map[string]interface{}{}
	}
	registered := cont.MCPService.ListTools()
	out := make([]map[string]interface{}, 0, len(registered))
	for _, tool := range registered {
		entry := map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
		}
		if tool.Parameters != nil {
			entry["parameters"] = tool.Parameters
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return out
}

func handleStartMCP(c echo.Context) error {
	return ServiceUnavailable(c, "mcp_server has no start/stop lifecycle; set mcp_server.enabled through PUT /mcp/config")
}

func handleStopMCP(c echo.Context) error {
	return ServiceUnavailable(c, "mcp_server has no start/stop lifecycle; set mcp_server.enabled through PUT /mcp/config")
}

func handleListMCPTools(c echo.Context) error {
	if refuseWhenMCPOff(c) {
		return nil
	}
	return OK(c, mcpToolList(GetContainer()))
}

func handleGetMCPConfig(c echo.Context) error {
	// `enabled` is what the /mcp/* routes enforce, not what the file says: after
	// POST /services/mcp_server/enable the toggle is on while the file still reads
	// false, and a config view that contradicted the behaviour was a second
	// source of truth for the same switch.
	configDefault := false
	if cfg := config.GetConfig(); cfg != nil {
		configDefault = cfg.McpServer.Enabled
	}
	return OK(c, map[string]interface{}{
		"enabled":        mcpServiceEnabled(),
		"config_default": configDefault,
	})
}

func handleUpdateMCPConfig(c echo.Context) error {
	var req config.McpServerConfig
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	setMCPServiceEnabled(req.Enabled)
	cfg := config.GetConfig()
	if cfg == nil {
		// Nothing to write the file to; the toggle above is what the routes use.
		return OK(c, req)
	}
	cfg.McpServer = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	return OK(c, req)
}

// RegisterServiceRoutes registers service management API routes.
func RegisterServiceRoutes(g *echo.Group) {
	g.GET("", handleListServices, requirePermission(security.PermSystemConfig))
	g.GET("/list", handleListServices, requirePermission(security.PermSystemConfig))
	g.GET("/:name/status", handleGetServiceStatus, requirePermission(security.PermSystemConfig))
	g.POST("/:name/start", handleStartService, requirePermission(security.PermSystemConfig))
	g.POST("/:name/stop", handleStopService, requirePermission(security.PermSystemConfig))
	g.POST("/:name/restart", handleRestartService, requirePermission(security.PermSystemConfig))
}

// handleListServices is declared in system.go — this file's RegisterServiceRoutes
// uses system.go's implementation to avoid redeclaration conflicts.

func handleGetServiceStatus(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()
	status := "stopped"
	var stats map[string]interface{}
	switch name {
	case "collect_scheduler":
		if cont.Scheduler != nil {
			stats = cont.Scheduler.Stats()
			if v, ok := stats["started"].(bool); ok && v {
				status = "running"
			}
		}
	case "event_bus":
		if cont.EventBus != nil && cont.EventBus.IsStarted() {
			status = "running"
			stats = cont.EventBus.Metrics()
		}
	case "websocket":
		if cont.WSManager != nil {
			status = "running"
			stats = cont.WSManager.GetStats()
		}
	case "mqtt_forwarder":
		if cont.MqttForward != nil {
			if cont.MqttForward.IsConnected() {
				status = "connected"
			} else {
				status = "disconnected"
			}
			stats = cont.MqttForward.GetStats()
		}
	case "mcp_server":
		// The MCP service is an in-process tool registry with no start/stop
		// lifecycle, so the user toggle is the running state. Reporting "stopped"
		// while mcp_server.enabled is false made a switched-off service look
		// startable, unlike its siblings below. This is the same predicate the
		// /mcp/* routes refuse on, so the page cannot show running while calls
		// are being turned away.
		if mcpServiceEnabled() {
			status = "running"
		} else {
			status = "disabled"
		}
	case "modbus_slave":
		if cont.ModbusSlaveServer != nil && cont.ModbusSlaveServer.IsConnected() {
			status = "running"
		} else if embeddedServiceEnabled(name) {
			status = "stopped"
		} else {
			status = "disabled"
		}
	case "serial_bridge":
		// The bridge's own counters (bytes each way, live and total clients,
		// rejections, last fault) are the page's only evidence that data is
		// actually crossing, so they are passed through as stats.
		if cont.SerialBridge != nil {
			bridgeStatus := cont.SerialBridge.GetStatus()
			if running, _ := bridgeStatus["started"].(bool); running {
				status = "running"
				stats = bridgeStatus
			} else if embeddedServiceEnabled(name) {
				status = "stopped"
			} else {
				status = "disabled"
			}
		} else if embeddedServiceEnabled(name) {
			status = "stopped"
		} else {
			status = "disabled"
		}
	case "mqtt_server":
		if cont.MqttServer != nil && cont.MqttServer.IsRunning() {
			status = "running"
			stats = cont.MqttServer.GetStats()
		} else if embeddedServiceEnabled(name) {
			status = "stopped"
		} else {
			status = "disabled"
		}
	default:
		if serviceEnabled(cont, name) {
			status = "running"
		}
	}
	result := map[string]interface{}{
		"name":    name,
		"status":  status,
		"state":   status,
		"enabled": status != "disabled",
		"uptime":  int(time.Since(startTime).Seconds()),
	}
	switch name {
	case "modbus_slave":
		cfg := config.GetConfig()
		result["current_config"] = map[string]interface{}{
			"host":          cfg.ModbusSlave.Host,
			"port":          cfg.ModbusSlave.Port,
			"holding_size":  cfg.ModbusSlave.HoldingSize,
			"input_size":    cfg.ModbusSlave.InputSize,
			"coil_size":     cfg.ModbusSlave.CoilSize,
			"discrete_size": cfg.ModbusSlave.DiscreteSize,
		}
	case "serial_bridge":
		cfg := config.GetConfig()
		// The whole section, so the page can show and edit every setting the
		// bridge reads instead of the three this map used to carry.
		result["current_config"] = cfg.SerialBridge
	case "mqtt_server":
		cfg := config.GetConfig()
		result["current_config"] = map[string]interface{}{
			"host": cfg.MqttServer.Host,
			"port": cfg.MqttServer.Port,
		}
	}
	if stats != nil {
		result["stats"] = stats
	}
	return OK(c, result)
}

// serviceEnabled reports whether a service has been toggled on by the user,
// consulting the in-memory map first and then the persisted setting so the
// state survives process restarts.
func serviceEnabled(cont *ServiceContainer, name string) bool {
	cont.ServiceEnabledMu.RLock()
	enabled, ok := cont.ServiceEnabledMap[name]
	cont.ServiceEnabledMu.RUnlock()
	if ok {
		return enabled
	}
	if cont.Database != nil {
		if v, err := cont.Database.GetSetting("service_enabled_" + name); err == nil && v == "1" {
			return true
		}
	}
	return false
}

// embeddedServiceEnabled reports the enabled flag of the config-driven
// embedded services (modbus slave, serial bridge, MQTT server).
func embeddedServiceEnabled(name string) bool {
	cfg := config.GetConfig()
	switch name {
	case "modbus_slave":
		return cfg.ModbusSlave.Enabled
	case "serial_bridge":
		return cfg.SerialBridge.Enabled
	case "mqtt_server":
		return cfg.MqttServer.Enabled
	}
	return false
}

// startEmbeddedModbusSlave launches the built-in Modbus TCP slave server.
func startEmbeddedModbusSlave(cont *ServiceContainer) error {
	if cont.ModbusSlaveServer != nil && cont.ModbusSlaveServer.IsConnected() {
		return nil
	}
	cfg := config.GetConfig()
	slave, err := drivers.NewModbusSlaveDriver("embedded-modbus-slave", map[string]interface{}{
		"host":          cfg.ModbusSlave.Host,
		"port":          cfg.ModbusSlave.Port,
		"holding_size":  cfg.ModbusSlave.HoldingSize,
		"input_size":    cfg.ModbusSlave.InputSize,
		"coil_size":     cfg.ModbusSlave.CoilSize,
		"discrete_size": cfg.ModbusSlave.DiscreteSize,
	})
	if err != nil {
		return err
	}
	if err := slave.Connect(context.Background()); err != nil {
		return err
	}
	cont.ModbusSlaveServer = slave
	logrus.Infof("Modbus Slave server started on %s:%d", cfg.ModbusSlave.Host, cfg.ModbusSlave.Port)
	return nil
}

// stopEmbeddedModbusSlave shuts down the built-in Modbus TCP slave server.
func stopEmbeddedModbusSlave(cont *ServiceContainer) {
	if cont.ModbusSlaveServer != nil {
		if err := cont.ModbusSlaveServer.Disconnect(); err != nil {
			logrus.WithError(err).Warn("Failed to stop Modbus Slave server")
		}
		cont.ModbusSlaveServer = nil
		logrus.Info("Modbus Slave server stopped")
	}
}

// startEmbeddedMqttServer launches the built-in MQTT broker.
func startEmbeddedMqttServer(cont *ServiceContainer) error {
	if cont.MqttServer != nil && cont.MqttServer.IsRunning() {
		return nil
	}
	cfg := config.GetConfig()
	srv := engine.NewMqttServer()
	if err := srv.Start(context.Background(), engine.MqttServerConfig{
		Enabled:     true,
		Host:        cfg.MqttServer.Host,
		Port:        cfg.MqttServer.Port,
		Username:    cfg.MqttServer.Username,
		Password:    cfg.MqttServer.Password,
		AllowNoAuth: cfg.MqttServer.AllowNoAuth,
		MaxClients:  100,
	}); err != nil {
		return err
	}
	cont.MqttServer = srv
	logrus.Infof("MQTT server started on %s:%d", cfg.MqttServer.Host, cfg.MqttServer.Port)
	return nil
}

// stopEmbeddedMqttServer shuts down the built-in MQTT broker.
func stopEmbeddedMqttServer(cont *ServiceContainer) {
	if cont.MqttServer != nil {
		if err := cont.MqttServer.Stop(); err != nil {
			logrus.WithError(err).Warn("Failed to stop MQTT server")
		}
		cont.MqttServer = nil
		logrus.Info("MQTT server stopped")
	}
}

// serialBridgeFactory builds the bridge this process runs. Tests replace it with
// one that serves an in-memory device, because the machine running them has no
// serial hardware to give the bridge.
var serialBridgeFactory = engine.NewSerialTcpBridge

// engineSerialBridgeConfig turns the persisted section into the bridge's own
// settings. The two views are kept in one place so the config the page saves,
// the config that is validated and the config a bridge starts with cannot drift.
func engineSerialBridgeConfig(sec config.SerialBridgeConfig) engine.SerialBridgeConfig {
	return engine.SerialBridgeConfig{
		SerialPort:  sec.SerialPort,
		BaudRate:    sec.BaudRate,
		DataBits:    sec.DataBits,
		Parity:      sec.Parity,
		StopBits:    sec.StopBits,
		ListenAddr:  fmt.Sprintf(":%d", sec.TCPPort),
		MaxClients:  sec.MaxClients,
		IPWhitelist: sec.IPWhitelist,
	}
}

// startEmbeddedSerialBridge launches the serial-over-TCP bridge.
func startEmbeddedSerialBridge(cont *ServiceContainer) error {
	if cont.SerialBridge != nil {
		if st, ok := cont.SerialBridge.GetStatus()["started"].(bool); ok && st {
			return nil
		}
		cont.SerialBridge = nil
	}
	cfg := config.GetConfig()
	bridge := serialBridgeFactory()
	if err := bridge.Start(context.Background(), engineSerialBridgeConfig(cfg.SerialBridge)); err != nil {
		return err
	}
	cont.SerialBridge = bridge
	logrus.Infof("Serial bridge started on TCP :%d", cfg.SerialBridge.TCPPort)
	return nil
}

// stopEmbeddedSerialBridge shuts down the serial-over-TCP bridge.
func stopEmbeddedSerialBridge(cont *ServiceContainer) {
	if cont.SerialBridge != nil {
		if err := cont.SerialBridge.Stop(); err != nil {
			logrus.WithError(err).Warn("Failed to stop serial bridge")
		}
		cont.SerialBridge = nil
		logrus.Info("Serial bridge stopped")
	}
}

// setEmbeddedServiceEnabled flips the enabled flag of an embedded service and
// persists the config file so the state survives restarts.
func setEmbeddedServiceEnabled(name string, enabled bool) error {
	cfg := config.GetConfig()
	switch name {
	case "modbus_slave":
		cfg.ModbusSlave.Enabled = enabled
	case "serial_bridge":
		cfg.SerialBridge.Enabled = enabled
	case "mqtt_server":
		cfg.MqttServer.Enabled = enabled
	default:
		return nil
	}
	return config.SaveConfig(cfg, "")
}

func handleStartService(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()

	switch name {
	case "collect_scheduler":
		if cont.Scheduler != nil {
			// Use context.Background() so the service outlives the HTTP request.
			// The service lifecycle is managed via its Stop() method.
			cont.Scheduler.Start(context.Background())
			logrus.WithField("service", name).Info("Service started")
		}
	case "mqtt_forwarder":
		if cont.MqttForward != nil {
			// Use context.Background() so the service outlives the HTTP request.
			// The service lifecycle is managed via its Stop() method.
			if err := cont.MqttForward.Start(context.Background()); err != nil {
				logrus.WithError(err).WithField("service", name).Error("Service start failed")
				return InternalError(c, "ERR_SERVICE_START_FAILED")
			}
			logrus.WithField("service", name).Info("Service started")
		}
	case "modbus_slave":
		if err := startEmbeddedModbusSlave(cont); err != nil {
			logrus.WithError(err).WithField("service", name).Error("Service start failed")
			return BadRequest(c, err.Error())
		}
	case "mqtt_server":
		if err := startEmbeddedMqttServer(cont); err != nil {
			logrus.WithError(err).WithField("service", name).Error("Service start failed")
			return BadRequest(c, err.Error())
		}
	case "serial_bridge":
		if err := startEmbeddedSerialBridge(cont); err != nil {
			logrus.WithError(err).WithField("service", name).Error("Service start failed")
			return BadRequest(c, err.Error())
		}
	default:
		logrus.WithField("service", name).Warn("Start requested for unknown service")
	}
	if err := setEmbeddedServiceEnabled(name, true); err != nil {
		logrus.WithError(err).WithField("service", name).Warn("Failed to persist service enabled state")
	}
	return OK(c, map[string]interface{}{"name": name, "status": "running"})
}

func handleStopService(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()

	switch name {
	case "collect_scheduler":
		if cont.Scheduler != nil {
			cont.Scheduler.Stop()
			logrus.WithField("service", name).Info("Service stopped")
		}
	case "mqtt_forwarder":
		if cont.MqttForward != nil {
			if err := cont.MqttForward.Stop(); err != nil {
				logrus.WithError(err).Warn("Failed to stop MQTT forwarder")
			}
			logrus.WithField("service", name).Info("Service stopped")
		}
	case "modbus_slave":
		stopEmbeddedModbusSlave(cont)
	case "mqtt_server":
		stopEmbeddedMqttServer(cont)
	case "serial_bridge":
		stopEmbeddedSerialBridge(cont)
	default:
		logrus.WithField("service", name).Warn("Stop requested for unknown service")
	}
	if err := setEmbeddedServiceEnabled(name, false); err != nil {
		logrus.WithError(err).WithField("service", name).Warn("Failed to persist service disabled state")
	}
	return OK(c, map[string]interface{}{"name": name, "status": "stopped"})
}

func handleRestartService(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()

	switch name {
	case "collect_scheduler":
		if cont.Scheduler != nil {
			cont.Scheduler.Stop()
			// Use context.Background() so the service outlives the HTTP request.
			cont.Scheduler.Start(context.Background())
			logrus.WithField("service", name).Info("Service restarted")
		}
	case "mqtt_forwarder":
		if cont.MqttForward != nil {
			_ = cont.MqttForward.Stop()
			// Use context.Background() so the service outlives the HTTP request.
			_ = cont.MqttForward.Start(context.Background())
			logrus.WithField("service", name).Info("Service restarted")
		}
	case "modbus_slave":
		stopEmbeddedModbusSlave(cont)
		if err := startEmbeddedModbusSlave(cont); err != nil {
			return BadRequest(c, err.Error())
		}
	case "mqtt_server":
		stopEmbeddedMqttServer(cont)
		if err := startEmbeddedMqttServer(cont); err != nil {
			return BadRequest(c, err.Error())
		}
	case "serial_bridge":
		stopEmbeddedSerialBridge(cont)
		if err := startEmbeddedSerialBridge(cont); err != nil {
			return BadRequest(c, err.Error())
		}
	default:
		logrus.WithField("service", name).Warn("Restart requested for unknown service")
	}
	return OK(c, map[string]interface{}{"name": name, "status": "running"})
}

// RegisterGrafanaRoutes registers Grafana integration API routes.
func RegisterGrafanaRoutes(g *echo.Group) {
	g.GET("/status", handleGetGrafanaStatus, requirePermission(security.PermSystemConfig))
	g.GET("/dashboards", handleListGrafanaDashboards, requirePermission(security.PermSystemConfig))
	g.POST("/dashboards/import", handleImportGrafanaDashboard, requirePermission(security.PermSystemConfig))
	g.GET("/datasources", handleListGrafanaDatasources, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetGrafanaConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateGrafanaConfig, requirePermission(security.PermSystemConfig))
}

// grafanaDisabled reports the state where the operator has not switched the
// integration on, which the dashboard page shows as an information banner
// rather than a failed request.
var grafanaDisabled = errors.New("grafana integration is not enabled")

// grafanaRejected carries an answer Grafana itself gave, so the operator sees
// "Grafana said 401, the configured API key does not work" instead of an empty
// table that looks like a Grafana with no dashboards.
type grafanaRejected struct {
	status int
	detail string
}

func (e *grafanaRejected) Error() string { return e.detail }

// grafanaUpstream issues one request against the Grafana instance this gateway
// is configured with. The client is a package var so a test can stand in a
// transport that answers instead of reaching for a real Grafana.
var grafanaUpstreamClient = &http.Client{Timeout: 8 * time.Second}

func grafanaUpstream(method, path string, body, out interface{}) error {
	cfg := config.GetConfig()
	if !cfg.Grafana.Enabled && !serviceEnabled(GetContainer(), "grafana") {
		return grafanaDisabled
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Grafana.URL), "/")
	if base == "" {
		return fmt.Errorf("ERR_GRAFANA_URL_MISSING: grafana.url is empty, so there is no Grafana instance to ask")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("ERR_GRAFANA_URL_MISSING: grafana.url %q is not an http(s) address", cfg.Grafana.URL)
	}

	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ERR_COMMON_INTERNAL: the dashboard JSON to import could not be encoded")
		}
		payload = raw
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ERR_GRAFANA_URL_MISSING: grafana.url %q could not be used as a Grafana address", cfg.Grafana.URL)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cfg.Grafana.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Grafana.APIKey)
	}

	resp, err := grafanaUpstreamClient.Do(req)
	if err != nil {
		return fmt.Errorf("ERR_GRAFANA_UNREACHABLE: Grafana at %s did not answer: %w", base, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("ERR_GRAFANA_UNREACHABLE: Grafana at %s closed the connection while answering %s %s", base, method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(gjsonGetString(responseBody, "message"))
		if detail == "" {
			detail = strings.TrimSpace(string(responseBody))
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return &grafanaRejected{status: resp.StatusCode, detail: fmt.Sprintf("Grafana at %s answered %d: %s", base, resp.StatusCode, detail)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("ERR_GRAFANA_BAD_RESPONSE: Grafana at %s answered %s %s with something other than the JSON this gateway reads", base, method, path)
	}
	return nil
}

// gjsonGetString pulls one string field out of a JSON object without failing on
// anything else, because Grafana's error bodies vary by endpoint and version.
func gjsonGetString(raw []byte, field string) string {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// respondGrafanaUpstream turns the three kinds of failure above into a status
// the operator can act on, never a success carrying an empty list.
func respondGrafanaUpstream(c echo.Context, err error) error {
	if errors.Is(err, grafanaDisabled) {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_GRAFANA_DISABLED", "Grafana integration is not enabled")
	}
	var rejected *grafanaRejected
	if errors.As(err, &rejected) {
		code := "ERR_GRAFANA_REJECTED"
		if rejected.status == http.StatusUnauthorized || rejected.status == http.StatusForbidden {
			code = "ERR_GRAFANA_KEY_REJECTED"
		}
		message := code + ": " + rejected.detail
		return ErrorCode(c, http.StatusBadGateway, message, message)
	}
	message := err.Error()
	if !strings.HasPrefix(message, "ERR_") {
		message = "ERR_GRAFANA_UNREACHABLE: " + message
	}
	return ErrorCode(c, http.StatusBadGateway, message, message)
}

func handleGetGrafanaStatus(c echo.Context) error {
	cfg := config.GetConfig()
	enabled := cfg.Grafana.Enabled || serviceEnabled(GetContainer(), "grafana")
	return OK(c, map[string]interface{}{
		"enabled": enabled,
		"url":     cfg.Grafana.URL,
	})
}

// grafanaDashboardRow keeps the fields the dashboard table and the panel links
// need; Grafana returns more, and forwarding it whole would couple this
// response to whatever a given Grafana version adds.
func grafanaDashboardRow(item map[string]interface{}) map[string]interface{} {
	row := map[string]interface{}{}
	for _, key := range []string{"uid", "title", "type", "uri", "url", "tags", "folderTitle", "folderId"} {
		if v, ok := item[key]; ok {
			row[key] = v
		}
	}
	return row
}

func handleListGrafanaDashboards(c echo.Context) error {
	// This used to answer 200 with a literal empty array, so the table was empty
	// whether Grafana held forty dashboards, was down, or had rejected the key.
	var items []map[string]interface{}
	if err := grafanaUpstream(http.MethodGet, "/api/search?type=dash-db&type=dash-folder&limit=500", nil, &items); err != nil {
		return respondGrafanaUpstream(c, err)
	}
	dashboards := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		dashboards = append(dashboards, grafanaDashboardRow(item))
	}
	// The page reads data.dashboards; returning a bare array here meant a real
	// Grafana with dashboards still rendered an empty table.
	return OK(c, map[string]interface{}{"dashboards": dashboards, "count": len(dashboards)})
}

func handleImportGrafanaDashboard(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// The handler echoed the request body back as 201 Created without sending it
	// anywhere, so the caller believed the dashboard existed in Grafana.
	if _, ok := req["dashboard"]; !ok {
		return BadRequest(c, "ERR_COMMON_VALIDATION: the import body must carry a dashboard object under the dashboard field")
	}
	var out map[string]interface{}
	if err := grafanaUpstream(http.MethodPost, "/api/dashboards/db", req, &out); err != nil {
		return respondGrafanaUpstream(c, err)
	}
	return Created(c, out)
}

func handleListGrafanaDatasources(c echo.Context) error {
	// Same literal as the dashboards route: an empty list that was indistinguishable
	// from a Grafana that could not be reached.
	var items []map[string]interface{}
	if err := grafanaUpstream(http.MethodGet, "/api/datasources", nil, &items); err != nil {
		return respondGrafanaUpstream(c, err)
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		row := map[string]interface{}{}
		for _, key := range []string{"id", "uid", "name", "type", "url", "access", "isDefault", "database"} {
			if v, ok := item[key]; ok {
				row[key] = v
			}
		}
		out = append(out, row)
	}
	return OK(c, out)
}

func handleGetGrafanaConfig(c echo.Context) error {
	cfg := config.GetConfig()
	enabled := cfg.Grafana.Enabled || serviceEnabled(GetContainer(), "grafana")
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	// grafana.api_key is listed in config.MaskedSensitivePaths, so every other
	// route that shows it masks it; this one handed the key out in the clear.
	apiKey := cfg.Grafana.APIKey
	if apiKey != "" {
		apiKey = security.MaskString(apiKey)
	}
	return OK(c, map[string]interface{}{
		"enabled":      enabled,
		"state":        state,
		"url":          cfg.Grafana.URL,
		"api_key":      apiKey,
		"datasource":   cfg.Grafana.Datasource,
		"dependencies": []interface{}{},
	})
}

func handleUpdateGrafanaConfig(c echo.Context) error {
	// Callers round-trip what GET /grafana/config showed, and that now carries a
	// masked api_key; binding straight into the struct stored the mask as the
	// real key, after which every Grafana API call was unauthenticated.
	var body map[string]interface{}
	if err := c.Bind(&body); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if body == nil {
		body = map[string]interface{}{}
	}
	wrapped := map[string]interface{}{"grafana": body}
	config.RestoreMaskedSecrets(wrapped)
	section, err := json.Marshal(wrapped["grafana"])
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	var req config.GrafanaConfig
	if err := json.Unmarshal(section, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cfg := config.GetConfig()
	// The enabled flag belongs to the service toggle, not to this form: a page
	// that never sends it must not switch Grafana off as a side effect.
	req.Enabled = cfg.Grafana.Enabled
	cfg.Grafana = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	if req.APIKey != "" {
		req.APIKey = security.MaskString(req.APIKey)
	}
	return OK(c, req)
}

// RegisterSimulationRoutes registers simulation API routes.
func RegisterSimulationRoutes(g *echo.Group) {
	g.GET("/devices", handleListSimDevices, requirePermission(security.PermDeviceRead))
	g.POST("/devices", handleCreateSimDevice, requirePermission(security.PermDeviceCreate))
	g.DELETE("/devices/:id", handleDeleteSimDevice, requirePermission(security.PermDeviceDelete))
	g.POST("/devices/:id/start", handleStartSimDevice, requirePermission(security.PermDeviceUpdate))
	g.POST("/devices/:id/stop", handleStopSimDevice, requirePermission(security.PermDeviceUpdate))
	g.GET("/config", handleGetSimConfig, requirePermission(security.PermDeviceRead))
	g.PUT("/auto-create", handleSetSimAutoCreate, requirePermission(security.PermSystemConfig))
	g.POST("/export", handleExportSimData, requirePermission(security.PermDataRead))
}

// handleGetSimConfig returns simulator configuration (auto-create flag).
func handleGetSimConfig(c echo.Context) error {
	cfg := config.GetConfig()
	return OK(c, map[string]interface{}{
		"auto_create": cfg.Simulator.AutoCreate,
	})
}

// handleSetSimAutoCreate toggles simulator auto device creation and persists it.
func handleSetSimAutoCreate(c echo.Context) error {
	var req struct {
		AutoCreate *bool `json:"auto_create"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.AutoCreate == nil {
		return BadRequest(c, "auto_create required")
	}
	cfg := config.GetConfig()
	cfg.Simulator.AutoCreate = *req.AutoCreate
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	return OK(c, map[string]interface{}{"auto_create": cfg.Simulator.AutoCreate})
}

// simDeviceProtocol is the driver a simulation-page device runs on. The
// simulator driver is compiled in (drivers.RegisterAll registers it), and the
// gateway already has a real create/collect/delete path for any protocol, so
// these routes drive that path instead of answering about nothing. Listing them
// used to return {"items":[]}, creating returned the request body as a 201, and
// deleting logged "Sim device deleted" while the row stayed in the database --
// so the page showed a successful create that then vanished on refresh.
const simDeviceProtocol = "simulator"

// simDeviceReq mirrors the body Simulation.vue sends: a device identity plus the
// point list whose keys line up with models.PointDef (data_type/min/max/mode).
type simDeviceReq struct {
	DeviceID        string                 `json:"device_id"`
	Name            string                 `json:"name"`
	CollectInterval int                    `json:"collect_interval"`
	Points          []models.PointDef      `json:"points"`
	Config          map[string]interface{} `json:"config"`
}

func simDeviceItem(d models.DeviceResponse) map[string]interface{} {
	return map[string]interface{}{
		"device_id":        d.DeviceID,
		"name":             d.Name,
		"point_count":      len(d.Points),
		"collect_interval": d.CollectInterval,
		"status":           d.Status,
	}
}

func handleListSimDevices(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	devices, _, err := cont.DeviceService.List(1, 1000)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_DEVICE_LIST_FAILED", err.Error())
	}
	items := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		if d.Protocol != simDeviceProtocol {
			continue
		}
		items = append(items, simDeviceItem(d))
	}
	return OK(c, map[string]interface{}{"items": items})
}

func handleCreateSimDevice(c echo.Context) error {
	var req simDeviceReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Name == "" {
		return BadRequest(c, "ERR_DEVICE_CONFIG_INVALID: name required")
	}
	if len(req.Points) == 0 {
		return BadRequest(c, "ERR_DEVICE_CONFIG_INVALID: points required")
	}
	cont := GetContainer()
	if cont == nil || cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	user := getUserFromContext(c)
	dev, err := cont.DeviceService.Create(&models.DeviceCreate{
		DeviceID:        req.DeviceID,
		Name:            req.Name,
		Protocol:        simDeviceProtocol,
		Config:          req.Config,
		Points:          req.Points,
		CollectInterval: req.CollectInterval,
	}, user.UserID)
	if err != nil {
		msg := err.Error()
		logrus.WithError(err).WithField("device_id", req.DeviceID).Warn("Create simulator device failed")
		if strings.Contains(msg, "already exists") || strings.Contains(msg, "duplicate") {
			return Conflict(c, "ERR_DEVICE_ALREADY_EXISTS")
		}
		if strings.Contains(msg, "unsupported protocol") {
			return ServiceUnavailable(c, "ERR_DRIVER_UNAVAILABLE: simulator driver is not registered")
		}
		return BadRequest(c, "ERR_DEVICE_CONFIG_INVALID: "+msg)
	}
	recordAudit(c, "device_create", "device", dev.DeviceID, "success", map[string]interface{}{
		"name": dev.Name, "protocol": dev.Protocol, "point_count": len(dev.Points),
	})
	return Created(c, simDeviceItem(*dev))
}

func handleDeleteSimDevice(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	dev, err := cont.DeviceService.Get(id)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_DEVICE_READ_FAILED", err.Error())
	}
	if dev == nil {
		return NotFound(c, "Device not found: "+id)
	}
	// Refuse to delete through this page anything the simulator did not create:
	// the route takes a bare device id, and a typo here would remove a real PLC.
	if dev.Protocol != simDeviceProtocol {
		return BadRequest(c, fmt.Sprintf("ERR_DEVICE_NOT_SIMULATOR: %s uses protocol %s", id, dev.Protocol))
	}
	if err := cont.DeviceService.Delete(id); err != nil {
		logrus.WithError(err).WithField("device_id", id).Warn("Delete simulator device failed")
		return InternalError(c, "ERR_DEVICE_DELETE_FAILED: "+err.Error())
	}
	recordAudit(c, "device_delete", "device", id, "success", map[string]interface{}{"protocol": simDeviceProtocol})
	return OK(c, map[string]interface{}{"deleted": id})
}

// handleStartSimDevice and handleStopSimDevice used to answer running/stopped
// for a device the route had never looked up. Collection is a property of the
// device record (collect_interval) and the scheduler that already owns it, so
// there is no separate simulator run state to flip.
func handleStartSimDevice(c echo.Context) error {
	return simLifecycleUnsupported(c)
}

func handleStopSimDevice(c echo.Context) error {
	return simLifecycleUnsupported(c)
}

func simLifecycleUnsupported(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_SIM_DEVICE_LIFECYCLE_UNSUPPORTED",
		"a simulator device collects as soon as it exists; change collect_interval through PUT /devices/:device_id instead")
}

// handleExportSimData used to hand back a CSV containing only the header row,
// which reads as "the simulator produced no data" on a gateway whose time-series
// store is full of it. There is no export query behind this route, so it refuses.
func handleExportSimData(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_SIM_EXPORT_UNSUPPORTED",
		"simulation CSV export is not implemented; query GET /data/query per point instead")
}

// RegisterMQTTForwarderRoutes registers MQTT forwarder management API routes.
func RegisterMQTTForwarderRoutes(g *echo.Group) {
	g.GET("/status", handleGetMQTTForwarderStatus, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartMQTTForwarder, requirePermission(security.PermSystemConfig))
	g.POST("/stop", handleStopMQTTForwarder, requirePermission(security.PermSystemConfig))
	g.GET("/queue/stats", handleGetMQTTQueueStats, requirePermission(security.PermSystemConfig))
	g.POST("/queue/clear", handleClearMQTTQueue, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetMQTTForwarderConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateMQTTForwarderConfig, requirePermission(security.PermSystemConfig))
}

func handleGetMQTTForwarderStatus(c echo.Context) error {
	cont := GetContainer()
	running := cont.MqttForward != nil
	broker := ""
	cfg := config.GetConfig()
	if cfg.MQTT.Broker != "" {
		broker = cfg.MQTT.Broker
	}
	if cont.MqttForward != nil {
		stats := cont.MqttForward.GetStats()
		return OK(c, map[string]interface{}{
			"running":    running,
			"broker":     broker,
			"topic":      cfg.MQTT.TopicPrefix,
			"queue_size": stats["queue_length"],
			"connected":  stats["connected"],
		})
	}
	return OK(c, map[string]interface{}{
		"running":    running,
		"broker":     broker,
		"topic":      cfg.MQTT.TopicPrefix,
		"queue_size": 0,
		"connected":  false,
	})
}

// handleStartMQTTForwarder actually starts the forwarder. It used to answer
// {"status":"started"} without touching the instance, so the page showed a
// running northbound path while nothing was publishing.
func handleStartMQTTForwarder(c echo.Context) error {
	cont := GetContainer()
	if cont.MqttForward == nil {
		return ServiceUnavailable(c, "ERR_MQTT_FORWARDER_NOT_AVAILABLE")
	}
	if err := cont.MqttForward.Start(context.Background()); err != nil {
		logrus.WithError(err).Error("MQTT forwarder start failed")
		return InternalError(c, "ERR_SERVICE_START_FAILED")
	}
	logrus.Info("MQTT forwarder started")
	return OK(c, map[string]string{"status": "started"})
}

func handleStopMQTTForwarder(c echo.Context) error {
	cont := GetContainer()
	if cont.MqttForward == nil {
		return ServiceUnavailable(c, "ERR_MQTT_FORWARDER_NOT_AVAILABLE")
	}
	if err := cont.MqttForward.Stop(); err != nil {
		logrus.WithError(err).Error("MQTT forwarder stop failed")
		return InternalError(c, "ERR_SERVICE_STOP_FAILED")
	}
	logrus.Info("MQTT forwarder stopped")
	return OK(c, map[string]string{"status": "stopped"})
}

func handleGetMQTTQueueStats(c echo.Context) error {
	cont := GetContainer()
	if cont.MqttForward != nil {
		return OK(c, cont.MqttForward.GetStats())
	}
	// Same keys as the live stats: a caller must not have to guess which
	// spelling this endpoint uses depending on the service state.
	return OK(c, map[string]interface{}{
		"connected":       false,
		"last_error":      "MQTT forwarder is not running",
		"total_published": int64(0),
		"total_queued":    int64(0),
		"total_failed":    int64(0),
		"total_retried":   int64(0),
		"queue_length":    0,
		"queue_capacity":  0,
		"last_flush_at":   "",
	})
}

func handleClearMQTTQueue(c echo.Context) error {
	cont := GetContainer()
	if cont.MqttForward == nil {
		return ServiceUnavailable(c, "ERR_MQTT_FORWARDER_NOT_AVAILABLE")
	}
	cleared, err := cont.MqttForward.ClearOfflineQueue()
	if err != nil {
		logrus.WithError(err).Error("MQTT offline queue clear failed")
		return InternalError(c, "ERR_MQTT_QUEUE_CLEAR_FAILED")
	}
	logrus.WithField("cleared", cleared).Info("MQTT offline queue cleared")
	return OK(c, map[string]interface{}{"status": "cleared", "cleared": cleared})
}

func handleGetMQTTForwarderConfig(c echo.Context) error {
	cfg := config.GetConfig()
	// The broker password is a credential: every other config surface masks it,
	// and this endpoint handed it out in plaintext to anyone with the system
	// config permission.
	out := cfg.MQTT
	if out.Password != "" {
		out.Password = security.MaskString(out.Password)
	}
	return OK(c, out)
}

func handleUpdateMQTTForwarderConfig(c echo.Context) error {
	// Same round-trip hazard as the broker config: the page re-sends the masked
	// password it was shown, which must not become the stored credential.
	var body map[string]interface{}
	if err := c.Bind(&body); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if body == nil {
		body = map[string]interface{}{}
	}
	wrapped := map[string]interface{}{"mqtt": body}
	config.RestoreMaskedSecrets(wrapped)
	section, err := json.Marshal(wrapped["mqtt"])
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	var req config.MQTTConfig
	if err := json.Unmarshal(section, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	cfg := config.GetConfig()
	previous := cfg.MQTT
	wasStarted := cont.MqttForward != nil && cont.MqttForward.IsStarted()
	cfg.MQTT = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	// The forwarder reads broker/port/credentials when it connects, so a saved
	// change only took effect after a process restart. Reconnect it now, and put
	// the previous section back if the new one cannot start.
	if wasStarted {
		if err := cont.MqttForward.Stop(); err != nil {
			logrus.WithError(err).Warn("Failed to stop MQTT forwarder before applying the new config")
		}
		if err := cont.MqttForward.Start(context.Background()); err != nil {
			cfg.MQTT = previous
			if rbErr := config.SaveConfig(cfg, ""); rbErr != nil {
				logrus.WithError(rbErr).Error("Failed to roll back MQTT config")
			}
			if rbErr := cont.MqttForward.Start(context.Background()); rbErr != nil {
				logrus.WithError(rbErr).Error("Failed to restart MQTT forwarder with the previous config")
			}
			logrus.WithError(err).Warn("MQTT forwarder could not start with the new config")
			return InternalError(c, "ERR_MQTT_FORWARDER_RESTART_FAILED")
		}
	}

	if req.MaxQueueSize != previous.MaxQueueSize {
		// The publish channel is allocated once at construction, so this one
		// field still needs a process restart; say so instead of implying the
		// change landed.
		logrus.WithField("max_queue_size", req.MaxQueueSize).
			Warn("mqtt.max_queue_size is applied when the forwarder is built; restart the gateway to resize the in-memory publish queue")
	}

	if req.Password != "" {
		req.Password = security.MaskString(req.Password)
	}
	return OK(c, req)
}

// RegisterMetricsRoutes registers Prometheus metrics endpoint.
func RegisterMetricsRoutes(g *echo.Group) {
	g.GET("", handleGetMetrics)
	g.GET("/summary", handleGetMetricsSummary)
}

// handleGetMetricsSummary returns the JSON metrics snapshot consumed by the
// frontend metrics view; the bare /metrics endpoint stays Prometheus text
// for scrapers.
func handleGetMetricsSummary(c echo.Context) error {
	cont := GetContainer()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	_, _, memPercent := memorySnapshot()
	_, _, diskPercent := diskSnapshot(".")
	avgGCPauseMs := 0.0
	if mem.NumGC > 0 {
		avgGCPauseMs = float64(mem.PauseTotalNs) / float64(mem.NumGC) / 1e6
	}
	system := map[string]interface{}{
		"cpu_percent":    cpuPercentCached(),
		"memory_percent": memPercent,
		"disk_percent":   diskPercent,
		"goroutines":     runtime.NumGoroutine(),
		"uptime_hours":   time.Since(startTime).Hours(),
		"gc_pauses_ms":   avgGCPauseMs,
	}

	devices := []models.DeviceResponse{}
	if cont.DeviceRepo != nil {
		if list, err := cont.DeviceRepo.ListAll(); err == nil {
			devices = list
		}
	}

	driverStats := map[string]map[string]interface{}{}
	collectRate := 0.0
	totalPoints := int64(0)
	if cont.Scheduler != nil {
		for _, st := range cont.Scheduler.GetDriverStats() {
			if id, _ := st["device_id"].(string); id != "" {
				driverStats[id] = st
			}
		}
		uptimeSec := time.Since(startTime).Seconds()
		st := cont.Scheduler.Stats()
		if tc, ok := st["total_collects"].(int64); ok && uptimeSec > 0 {
			collectRate = float64(tc) / uptimeSec
		}
		if tp, ok := st["total_points"].(int64); ok {
			totalPoints = tp
		}
	}

	var breakers map[string]*engine.CircuitBreaker
	if cont.CBRegistry != nil {
		breakers = cont.CBRegistry.GetAll()
	}
	// The drivers keep their own latency samples and circuit state; a device the
	// scheduler has not polled yet still has a real health record to read.
	var healthByID map[string]*drivers.DriverHealthStats
	if mgr := drivers.GetHealthStatsManager(); mgr != nil {
		healthByID = mgr.GetAllHealthStats()
	}
	onlineDevices := int64(0)
	driverRows := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		// Every measurement starts unreported. The previous 0.0 / "closed"
		// defaults drew a device that had never completed a round trip as a
		// healthy link with zero errors.
		row := map[string]interface{}{
			"device_id":      d.DeviceID,
			"device_name":    d.Name,
			"protocol":       d.Protocol,
			"status":         d.Status,
			"read_count":     nil,
			"error_count":    nil,
			"error_rate":     nil,
			"avg_latency_ms": nil,
			"circuit_state":  nil,
		}
		if st, ok := driverStats[d.DeviceID]; ok {
			enabled, _ := st["enabled"].(bool)
			success, _ := st["success_count"].(int64)
			fail, _ := st["fail_count"].(int64)
			latency, _ := st["last_latency_ms"].(int64)
			if enabled {
				onlineDevices++
			}
			row["read_count"] = success
			row["error_count"] = fail
			if total := success + fail; total > 0 {
				row["error_rate"] = float64(fail) / float64(total)
			}
			row["avg_latency_ms"] = float64(latency)
		}
		if hs := healthByID[d.DeviceID]; hs != nil {
			ct := hs.Counters()
			// The health manager's average is over a rolling window of real
			// samples, which is what this column claims to show; the scheduler
			// only knows the last round trip.
			if ct.HasLatencySample {
				row["avg_latency_ms"] = ct.AvgLatencyMs
			}
			if ct.TotalReads > 0 || ct.TotalWrites > 0 {
				row["read_count"] = ct.TotalReads
				row["error_count"] = ct.FailedReads
				row["error_rate"] = hs.ReadErrorRate()
			}
		}
		if cb, ok := breakers[d.DeviceID]; ok {
			if stats := cb.GetStats(); stats != nil {
				if state, _ := stats["state"].(string); state != "" {
					row["circuit_state"] = state
				}
			}
		}
		driverRows = append(driverRows, row)
	}

	activeRules := int64(0)
	if cont.RuleRepo != nil {
		if rules, _, err := cont.RuleRepo.List(1, 1000, ""); err == nil {
			for _, r := range rules {
				if r.Enabled {
					activeRules++
				}
			}
		}
	}

	alarmsToday := int64(0)
	if cont.AlarmRepo != nil {
		midnight := time.Now().Truncate(24 * time.Hour).Format(time.RFC3339)
		if _, total, err := cont.AlarmRepo.List(models.AlarmFilter{StartTime: midnight}, 1, 1); err == nil {
			alarmsToday = int64(total)
		}
	}

	engine := map[string]interface{}{
		"total_devices":  int64(len(devices)),
		"online_devices": onlineDevices,
		"total_points":   totalPoints,
		"collect_rate":   collectRate,
		"active_rules":   activeRules,
		"alarms_today":   alarmsToday,
	}

	return OK(c, map[string]interface{}{
		"system":  system,
		"engine":  engine,
		"drivers": driverRows,
		// No middleware times HTTP handlers in this build, so the endpoint table
		// has nothing to list. An empty array alone looks like "no traffic".
		"api_endpoints": []interface{}{},
		"not_collected": []string{"api_endpoints"},
	})
}

func handleGetMetrics(c echo.Context) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	c.Response().Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	cont := GetContainer()

	var deviceCount, ruleCount, activeAlarms int
	if cont.DeviceRepo != nil {
		_, total, err := cont.DeviceRepo.List(1, 1)
		if err == nil {
			deviceCount = total
		}
	}
	if cont.RuleRepo != nil {
		_, total, err := cont.RuleRepo.List(1, 1, "")
		if err == nil {
			ruleCount = total
		}
	}
	if cont.AlarmRepo != nil {
		_, total, err := cont.AlarmRepo.List(models.AlarmFilter{Status: "active"}, 1, 1)
		if err == nil {
			activeAlarms = total
		}
	}

	// Return Prometheus text format metrics
	metrics := "# EdgeLite Gateway Metrics\n"
	metrics += "# TYPE edgelite_uptime_seconds gauge\n"
	metrics += "edgelite_uptime_seconds " + formatFloat(time.Since(startTime).Seconds()) + "\n"
	metrics += "# TYPE edgelite_mem_alloc_bytes gauge\n"
	metrics += "edgelite_mem_alloc_bytes " + formatUint(m.Alloc) + "\n"
	metrics += "# TYPE edgelite_mem_sys_bytes gauge\n"
	metrics += "edgelite_mem_sys_bytes " + formatUint(m.Sys) + "\n"
	metrics += "# TYPE edgelite_goroutines gauge\n"
	metrics += "edgelite_goroutines " + formatInt(int64(runtime.NumGoroutine())) + "\n"
	metrics += "# TYPE edgelite_devices_total gauge\n"
	metrics += "edgelite_devices_total " + formatInt(int64(deviceCount)) + "\n"
	metrics += "# TYPE edgelite_rules_total gauge\n"
	metrics += "edgelite_rules_total " + formatInt(int64(ruleCount)) + "\n"
	metrics += "# TYPE edgelite_alarms_active gauge\n"
	metrics += "edgelite_alarms_active " + formatInt(int64(activeAlarms)) + "\n"
	return c.String(http.StatusOK, metrics)
}

func formatUint(v uint64) string {
	return strconv.FormatUint(v, 10)
}

func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// RegisterDataQualityRoutes registers data quality monitoring API routes.
func RegisterDataQualityRoutes(g *echo.Group) {
	g.GET("/summary", handleGetDataQualitySummary, requirePermission(security.PermDataRead))
	g.GET("/trend", handleGetDataQualityTrend, requirePermission(security.PermDataRead))
	g.GET("/devices/:device_id", handleGetDeviceDataQuality, requirePermission(security.PermDataRead))
}

func handleGetDataQualitySummary(c echo.Context) error {
	hours := 24
	if h, err := strconv.Atoi(c.QueryParam("hours")); err == nil && h > 0 && h <= 24*30 {
		hours = h
	}

	resp := map[string]interface{}{
		"window_hours":     hours,
		"total_points":     0,
		"good_points":      0,
		"bad_points":       0,
		"missing_points":   0,
		"completeness_pct": 100.0,
		"accuracy_pct":     100.0,
		"timeliness_pct":   100.0,
		"quality_score":    100.0,
		"overall_score":    100.0,
	}

	cont := GetContainer()
	if cont == nil || cont.TsStorage == nil {
		return OK(c, resp)
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := cont.TsStorage.QualityByDevice(since)
	if err != nil {
		logrus.WithError(err).Error("Failed to compute data quality summary")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}

	var total, good, bad int64
	rowByDevice := make(map[string]storage.DeviceQualityRow, len(rows))
	for _, r := range rows {
		total += r.TotalRows
		good += r.ValidRows
		bad += r.InvalidRows
		rowByDevice[r.DeviceID] = r
	}

	// Expected sample volume derived from device point definitions and collect intervals.
	var expected int64
	devTotal, devFresh := 0, 0
	windowSec := hours * 3600
	if cont.DeviceRepo != nil {
		devices, err := cont.DeviceRepo.ListAll()
		if err != nil {
			logrus.WithError(err).Error("Failed to list devices for data quality summary")
			return InternalError(c, "ERR_DATA_QUALITY_FAILED")
		}
		for _, d := range devices {
			if len(d.Points) == 0 {
				continue
			}
			interval := d.CollectInterval
			if interval <= 0 {
				interval = 5
			}
			samples := int64(windowSec / interval)
			if samples < 1 {
				samples = 1
			}
			expected += int64(len(d.Points)) * samples

			devTotal++
			staleAfter := time.Duration(2*interval) * time.Second
			if staleAfter < 2*time.Minute {
				staleAfter = 2 * time.Minute
			}
			if r, ok := rowByDevice[d.DeviceID]; ok {
				if last, err := time.Parse(time.RFC3339Nano, r.LastTimestamp); err == nil && time.Since(last) <= staleAfter {
					devFresh++
				}
			}
		}
	}

	missing := expected - total
	if missing < 0 {
		missing = 0
	}
	completeness := 100.0
	if expected > 0 {
		completeness = float64(total) / float64(expected) * 100
	}
	accuracy := 100.0
	if total > 0 {
		accuracy = float64(good) / float64(total) * 100
	}
	timeliness := 100.0
	if devTotal > 0 {
		timeliness = float64(devFresh) / float64(devTotal) * 100
	}
	overall := (completeness + accuracy + timeliness) / 3

	resp["total_points"] = total
	resp["good_points"] = good
	resp["bad_points"] = bad
	resp["missing_points"] = missing
	resp["completeness_pct"] = math.Round(completeness*10) / 10
	resp["accuracy_pct"] = math.Round(accuracy*10) / 10
	resp["timeliness_pct"] = math.Round(timeliness*10) / 10
	resp["quality_score"] = resp["accuracy_pct"]
	resp["overall_score"] = math.Round(overall*10) / 10
	return OK(c, resp)
}

// qualityWindowHours reads the hours parameter the quality pages send.
func qualityWindowHours(c echo.Context) int {
	hours := 24
	if h, err := strconv.Atoi(c.QueryParam("hours")); err == nil && h > 0 && h <= 24*30 {
		hours = h
	}
	return hours
}

// handleGetDataQualityTrend buckets measured quality by hour. It used to answer
// [] unconditionally, which the chart read as "quality never changed".
func handleGetDataQualityTrend(c echo.Context) error {
	cont := GetContainer()
	hours := qualityWindowHours(c)
	if cont == nil || cont.TsStorage == nil {
		return ServiceUnavailable(c, "ERR_TS_STORAGE_UNAVAILABLE")
	}
	rows, err := cont.TsStorage.QualityHourlyBuckets(c.QueryParam("device_id"), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		logrus.WithError(err).Error("Failed to compute data quality trend")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]interface{}{
			"bucket":      r.Bucket,
			"total_rows":  r.TotalRows,
			"valid_rows":  r.ValidRows,
			"quality_pct": math.Round(float64(r.ValidRows)/float64(r.TotalRows)*1000) / 10,
		})
	}
	return OK(c, out)
}

// handleGetDeviceDataQuality aggregates the device's own time-series rows. Any
// device id - including one that does not exist - used to be answered with
// quality_score 100 and no points, i.e. "perfect".
func handleGetDeviceDataQuality(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont == nil || cont.TsStorage == nil {
		return ServiceUnavailable(c, "ERR_TS_STORAGE_UNAVAILABLE")
	}
	if cont.DeviceRepo != nil {
		dev, err := cont.DeviceRepo.Get(deviceID)
		if err != nil {
			logrus.WithError(err).Error("Failed to look up device for data quality")
			return InternalError(c, "ERR_DATA_QUALITY_FAILED")
		}
		if dev == nil {
			return NotFound(c, "ERR_DEVICE_NOT_FOUND")
		}
	}
	hours := qualityWindowHours(c)
	pts, err := cont.TsStorage.QualityByPoint(deviceID, time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		logrus.WithError(err).Error("Failed to compute per-point data quality")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}

	var total, valid, invalid int64
	last := ""
	pointRows := make([]map[string]interface{}, 0, len(pts))
	for _, p := range pts {
		total += p.TotalRows
		valid += p.ValidRows
		invalid += p.InvalidRows
		if p.LastTimestamp > last {
			last = p.LastTimestamp
		}
		pointRows = append(pointRows, map[string]interface{}{
			"point_name":     p.PointName,
			"total_rows":     p.TotalRows,
			"valid_rows":     p.ValidRows,
			"invalid_rows":   p.InvalidRows,
			"quality_pct":    math.Round(float64(p.ValidRows)/float64(p.TotalRows)*1000) / 10,
			"last_timestamp": p.LastTimestamp,
		})
	}

	entry := map[string]interface{}{
		"device_id":    deviceID,
		"window_hours": hours,
		"total_rows":   total,
		"valid_rows":   valid,
		"invalid_rows": invalid,
		"point_count":  len(pts),
		"points":       pointRows,
		// A device with no samples in the window has not been measured; that is
		// not the same as a score of 100.
		"quality_score":  nil,
		"last_timestamp": nil,
	}
	if total > 0 {
		entry["quality_score"] = math.Round(float64(valid)/float64(total)*1000) / 10
		entry["last_timestamp"] = last
	}
	return OK(c, entry)
}

// RegisterDBMonitorRoutes registers database monitoring API routes.
func RegisterDBMonitorRoutes(g *echo.Group) {
	g.GET("/status", handleGetDBMonitorStatus, requirePermission(security.PermSystemConfig))
	g.GET("/stats", handleGetDBStats, requirePermission(security.PermSystemConfig))
	g.GET("/tables", handleGetDBTables, requirePermission(security.PermSystemConfig))
	g.GET("/slow-queries", handleGetSlowQueries, requirePermission(security.PermSystemConfig))
	g.POST("/vacuum", handleDBVacuum, requirePermission(security.PermSystemConfig))
	g.POST("/reindex", handleDBReindex, requirePermission(security.PermSystemConfig))
}

func handleGetDBMonitorStatus(c echo.Context) error {
	cont := GetContainer()
	if cont.DBMonitor == nil {
		return ServiceUnavailable(c, "Database monitor is not ready")
	}
	// CheckHealth only carries the fields it could stat, so an absent
	// db_size_bytes means "unknown" rather than a fabricated 0, and the old
	// fallback's "connections: 1" was a literal no code had measured.
	return OK(c, cont.DBMonitor.CheckHealth())
}

func handleGetDBTables(c echo.Context) error {
	cont := GetContainer()
	if cont.DBMonitor == nil {
		return ServiceUnavailable(c, "Database monitor is not ready")
	}
	// An empty list used to cover both "the database has no tables" and "the
	// monitor could not read it"; only the first one is now answered with [].
	tables, err := cont.DBMonitor.GetTableDetails()
	if err != nil {
		logrus.WithError(err).Error("Failed to list database tables")
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_DB_NOT_CONNECTED",
			"Database is not connected, table details are unavailable")
	}
	return OK(c, tables)
}

// handleGetSlowQueries is honest about a capability this build does not have:
// SQLite exposes no slow-query log and the gateway wraps no statement timer, so
// an empty 200 was indistinguishable from "the database has no slow queries".
func handleGetSlowQueries(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented,
		"ERR_DB_SLOW_QUERIES_UNSUPPORTED", "ERR_DB_SLOW_QUERIES_UNSUPPORTED")
}

// RegisterLogAggregationRoutes registers log aggregation API routes.
func RegisterLogAggregationRoutes(g *echo.Group) {
	g.GET("", handleGetLogs, requirePermission(security.PermSystemLogs))
	g.GET("/levels", handleGetLogLevels, requirePermission(security.PermSystemLogs))
	g.GET("/export", handleExportLogs, requirePermission(security.PermSystemLogs))
}

// parseLogTime accepts the shapes the log UI sends: RFC3339, "YYYY-MM-DD HH:MM:SS"
// and a bare date. A bare date used to be rejected silently, which widened the
// window instead of narrowing it; for end_time it now means end of day.
func parseLogTime(v string, endOfDay bool) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, true
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		if endOfDay {
			return t.Add(24*time.Hour - time.Second), true
		}
		return t, true
	}
	return time.Time{}, false
}

// collectLogRows applies the log filters to the aggregator buffer, newest first.
// /logs, /logs/export and /logs/query share it so their semantics cannot drift.
// Callers must go through logRowsOrRefuse rather than calling this directly: a nil
// buffer is not the same answer as an empty one.
func collectLogRows(c echo.Context) []map[string]interface{} {
	cont := GetContainer()
	if cont == nil || cont.LogAggregator == nil {
		return nil
	}

	var since, until time.Time
	if t, ok := parseLogTime(c.QueryParam("start_time"), false); ok {
		since = t
	}
	if t, ok := parseLogTime(c.QueryParam("end_time"), true); ok {
		until = t
	}

	level := engine.NormalizeLogLevel(c.QueryParam("level"))
	source := c.QueryParam("source")
	deviceID := c.QueryParam("device_id")
	search := strings.ToLower(strings.TrimSpace(c.QueryParam("search")))

	entries := cont.LogAggregator.Query(level, source, since, 0)
	rows := make([]map[string]interface{}, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !until.IsZero() && e.Timestamp.After(until) {
			continue
		}
		if deviceID != "" && e.DeviceID != deviceID {
			continue
		}
		if search != "" &&
			!strings.Contains(strings.ToLower(e.Message), search) &&
			!strings.Contains(strings.ToLower(e.Source), search) &&
			!strings.Contains(strings.ToLower(e.RequestID), search) {
			continue
		}
		rows = append(rows, map[string]interface{}{
			"level":     engine.NormalizeLogLevel(e.Level),
			"timestamp": e.Timestamp.Format(time.RFC3339),
			"source":    e.Source,
			"message":   e.Message,
			"trace_id":  e.RequestID,
			"device_id": e.DeviceID,
		})
	}
	return rows
}

// logRowsOrRefuse answers with the filtered buffer, or with a distinct code when
// there is no aggregator to read from. An empty list under status 200 would tell
// an operator "the gateway logged nothing" instead of "logging is not running".
func logRowsOrRefuse(c echo.Context) ([]map[string]interface{}, error) {
	cont := GetContainer()
	if cont == nil || cont.LogAggregator == nil {
		return nil, ServiceUnavailable(c, "ERR_LOG_AGGREGATOR_NOT_READY: log aggregation is not running")
	}
	return collectLogRows(c), nil
}

func handleGetLogs(c echo.Context) error {
	items, err := logRowsOrRefuse(c)
	if err != nil {
		return err
	}
	page, size := parsePagination(c)

	total := len(items)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return OK(c, map[string]interface{}{
		"items": items[start:end],
		"total": total,
	})
}

func handleGetLogLevels(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"current":   strings.ToUpper(logrus.GetLevel().String()),
		"available": []string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"},
	})
}

// csvSafeCell stops a log line that starts with a formula sigil from executing
// when the export is opened in a spreadsheet, and quotes what csv.Writer does
// not (commas, quotes, newlines in the message).
func csvSafeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}

func handleExportLogs(c echo.Context) error {
	rows, err := logRowsOrRefuse(c)
	if err != nil {
		return err
	}

	if strings.EqualFold(c.QueryParam("format"), "json") {
		c.Response().Header().Set("Content-Type", "application/json")
		c.Response().Header().Set("Content-Disposition", `attachment; filename="logs.json"`)
		return c.JSON(http.StatusOK, rows)
	}

	c.Response().Header().Set("Content-Type", "text/csv; charset=utf-8")
	c.Response().Header().Set("Content-Disposition", `attachment; filename="logs.csv"`)
	w := csv.NewWriter(c.Response())
	if err := w.Write([]string{"timestamp", "level", "source", "device_id", "trace_id", "message"}); err != nil {
		return err
	}
	cell := func(row map[string]interface{}, key string) string {
		s, _ := row[key].(string)
		return csvSafeCell(s)
	}
	for _, row := range rows {
		if err := w.Write([]string{
			cell(row, "timestamp"), cell(row, "level"), cell(row, "source"),
			cell(row, "device_id"), cell(row, "trace_id"), cell(row, "message"),
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// RegisterResourceShareRoutes registers resource sharing API routes.
func RegisterResourceShareRoutes(g *echo.Group) {
	g.GET("", handleListResourceShares, requirePermission(security.PermSystemConfig))
	g.POST("/transfer", handleTransferResource, requirePermission(security.PermSystemConfig))
	g.GET("/resource/:type/:id", handleGetResourceShare, requirePermission(security.PermSystemConfig))
}

func handleListResourceShares(c echo.Context) error {
	shares, err := resourceSharesWithStatus()
	if err != nil {
		return shareStoreError(c, err)
	}
	return OK(c, shares)
}

// handleTransferResource moves ownership of devices or rules to another user.
// It used to answer {"success":true} having changed nothing: the operator saw
// "transfer successful", the list refetched, and the device stayed in the old
// account. Ownership lives in the created_by column, which is also what the
// non-admin visibility filter reads, so that is the value a transfer has to move.
func handleTransferResource(c echo.Context) error {
	var req struct {
		ResourceType string   `json:"resource_type"`
		ResourceIDs  []string `json:"resource_ids"`
		ResourceID   string   `json:"resource_id"`
		TargetUserID string   `json:"target_user_id"`
		NewOwnerID   string   `json:"new_owner_id"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// The device-list modal sends resource_ids/target_user_id; the typed client
	// sends resource_id/new_owner_id. Accept both rather than picking a winner.
	ids := make([]string, 0, len(req.ResourceIDs)+1)
	seen := make(map[string]bool, len(req.ResourceIDs)+1)
	for _, id := range append(req.ResourceIDs, req.ResourceID) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	newOwner := req.TargetUserID
	if newOwner == "" {
		newOwner = req.NewOwnerID
	}
	if len(ids) == 0 {
		return BadRequest(c, "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED")
	}
	if newOwner == "" {
		return BadRequest(c, "ERR_RESOURCE_SHARE_TARGET_REQUIRED")
	}

	cont := GetContainer()
	if cont.Database == nil || cont.UserRepo == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	target, err := cont.UserRepo.GetByID(newOwner)
	if err != nil {
		logrus.WithError(err).Error("Failed to load transfer target user")
		return InternalError(c, "ERR_COMMON_INTERNAL")
	}
	if target == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	type transferredItem struct {
		ResourceID    string `json:"resource_id"`
		PreviousOwner string `json:"previous_owner"`
	}
	type failedItem struct {
		ResourceID string `json:"resource_id"`
		Error      string `json:"error"`
	}
	transferred := make([]transferredItem, 0, len(ids))
	failed := make([]failedItem, 0)

	setOwner := func(get func(string) (string, error), apply func(string, string) error) {
		for _, id := range ids {
			previous, err := get(id)
			if err != nil {
				failed = append(failed, failedItem{id, err.Error()})
				continue
			}
			if err := apply(id, newOwner); err != nil {
				failed = append(failed, failedItem{id, err.Error()})
				continue
			}
			transferred = append(transferred, transferredItem{id, previous})
			recordAudit(c, "resource_transfer", req.ResourceType, id, "success", map[string]interface{}{
				"previous_owner": previous,
				"new_owner":      newOwner,
			})
		}
	}

	switch req.ResourceType {
	case "device":
		if cont.DeviceRepo == nil {
			return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
		}
		setOwner(func(id string) (string, error) {
			dev, err := cont.DeviceRepo.Get(id)
			if err != nil {
				return "", err
			}
			if dev == nil {
				return "", fmt.Errorf("ERR_DEVICE_NOT_FOUND")
			}
			return dev.CreatedBy, nil
		}, cont.DeviceRepo.SetOwner)
	case "rule":
		if cont.RuleRepo == nil {
			return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
		}
		setOwner(func(id string) (string, error) {
			rule, err := cont.RuleRepo.Get(id)
			if err != nil {
				return "", err
			}
			if rule == nil {
				return "", fmt.Errorf("ERR_RULE_NOT_FOUND")
			}
			return rule.CreatedBy, nil
		}, cont.RuleRepo.SetOwner)
	default:
		// Templates and the other shareable kinds have no owner column, so there
		// is nothing this gateway could transfer. An empty success hid that.
		return ErrorCode(c, http.StatusNotImplemented, "ERR_RESOURCE_SHARE_TRANSFER_UNSUPPORTED",
			"ERR_RESOURCE_SHARE_TRANSFER_UNSUPPORTED")
	}

	// The status stays 200 even when every id failed: transferred[] and failed[]
	// carry the per-resource truth, and the modal renders both. A blanket 500
	// would hide which devices moved.
	logrus.WithFields(logrus.Fields{"resource_type": req.ResourceType,
		"new_owner":   newOwner,
		"transferred": len(transferred),
		"failed":      len(failed),
	}).Info("Resource ownership transfer completed")
	return OK(c, map[string]interface{}{
		"resource_type":      req.ResourceType,
		"new_owner":          newOwner,
		"new_owner_username": target.Username,
		"transferred":        transferred,
		"failed":             failed,
		"count":              len(transferred),
	})
}

func handleGetResourceShare(c echo.Context) error {
	rType := c.Param("type")
	rID := c.Param("id")
	all, err := resourceSharesWithStatus()
	if err != nil {
		return shareStoreError(c, err)
	}
	shares := make([]resourceShare, 0, 2)
	active := 0
	for _, s := range all {
		if s.ResourceType != rType || s.ResourceID != rID {
			continue
		}
		shares = append(shares, s)
		if s.Status != "expired" {
			active++
		}
	}
	// "shared" answers whether anyone can reach this resource right now, which
	// an expired record does not.
	return OK(c, map[string]interface{}{
		"resource_type": rType,
		"resource_id":   rID,
		"shared":        active > 0,
		"shares":        shares,
	})
}

// RegisterConfigVersionRoutes registers config version management API routes.
func RegisterConfigVersionRoutes(g *echo.Group) {
	g.GET("", handleListConfigVersions, requirePermission(security.PermSystemConfig))
	g.GET("/versions", handleListConfigVersions, requirePermission(security.PermSystemConfig))
	g.GET("/versions/:version/diff", handleGetConfigVersionDiff, requirePermission(security.PermSystemConfig))
	g.POST("/versions/:version/rollback", handleRollbackConfigVersion, requirePermission(security.PermSystemConfig))
	g.POST("", handleSaveConfigVersion, requirePermission(security.PermSystemConfig))
	g.GET("/:version", handleGetConfigVersion, requirePermission(security.PermSystemConfig))
	g.POST("/rollback", handleRollbackConfig, requirePermission(security.PermSystemConfig))
}

// handleListConfigVersions is declared in system.go — this file's RegisterConfigVersionRoutes
// uses system.go's implementation to avoid redeclaration conflicts.

// handleSaveConfigVersion stores a real snapshot in config_versions. It used to
// echo the request body back as 201 Created, which made a save that wrote nothing
// indistinguishable from one that worked.
func handleSaveConfigVersion(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	db := cont.Database.DB()

	var body map[string]interface{}
	if err := c.Bind(&body); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// An empty body means "snapshot what this process is running right now".
	rawConfig, _ := json.Marshal(config.GetSanitizedConfig())
	summary := "手动保存配置快照"
	if body != nil {
		if cfgPart, ok := body["config"]; ok {
			encoded, err := json.Marshal(cfgPart)
			if err != nil {
				return BadRequest(c, "ERR_CONFIG_VERSION_ENCODE_FAILED")
			}
			rawConfig = encoded
		}
		if s, ok := body["change_summary"].(string); ok && strings.TrimSpace(s) != "" {
			summary = strings.TrimSpace(s)
		}
	}
	updatedBy := "system"
	if u := getUserFromContext(c); u != nil {
		updatedBy = u.Username
	}

	res, err := db.Exec(
		"INSERT INTO config_versions (config_json, created_at, created_by, change_summary) VALUES (?, ?, ?, ?)",
		string(rawConfig), time.Now().Format(time.RFC3339), updatedBy, summary)
	if err != nil {
		logrus.WithError(err).Error("Save config version failed")
		return InternalError(c, "ERR_CONFIG_VERSION_SAVE_FAILED")
	}
	version, err := res.LastInsertId()
	if err != nil || version == 0 {
		// SQLite always reports the rowid, so falling back to MAX keeps the response
		// truthful instead of claiming a version we never read back.
		if qerr := db.QueryRow("SELECT MAX(version) FROM config_versions").Scan(&version); qerr != nil {
			logrus.WithError(qerr).Warn("Config version inserted but its id could not be read back")
		}
	}
	var createdAt string
	_ = db.QueryRow("SELECT created_at FROM config_versions WHERE version = ?", version).Scan(&createdAt)
	return Created(c, map[string]interface{}{
		"version":        version,
		"created_at":     createdAt,
		"created_by":     updatedBy,
		"change_summary": summary,
	})
}

func handleGetConfigVersion(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	raw := c.Param("version")
	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return BadRequest(c, "ERR_CONFIG_VERSION_INVALID")
	}
	var configJSON, createdAt, createdBy, summary string
	scanErr := cont.Database.DB().QueryRow(
		"SELECT config_json, created_at, IFNULL(created_by,''), IFNULL(change_summary,'') FROM config_versions WHERE version = ?",
		version).Scan(&configJSON, &createdAt, &createdBy, &summary)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return NotFound(c, "ERR_CONFIG_VERSION_NOT_FOUND")
		}
		logrus.WithError(scanErr).WithField("version", version).Warn("Config version lookup failed")
		return InternalError(c, "ERR_CONFIG_VERSION_READ_FAILED")
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(configJSON), &parsed); err != nil {
		// The column is NOT NULL TEXT with no JSON constraint, so a hand-edited or
		// half-written row is a real possibility. Returning {} hid that.
		logrus.WithError(err).WithField("version", version).Warn("Config version row is not valid JSON")
		return ErrorCode(c, http.StatusConflict, "ERR_CONFIG_VERSION_CORRUPT", err.Error())
	}
	return OK(c, map[string]interface{}{
		"version":        version,
		"config":         parsed,
		"created_at":     createdAt,
		"created_by":     createdBy,
		"change_summary": summary,
	})
}

// handleRollbackConfig is declared in system.go — this file's RegisterConfigVersionRoutes
// uses system.go's implementation to avoid redeclaration conflicts.

// RegisterProtocolBridgeRoutes registers protocol bridge API routes.
// The handlers live in protocol_bridge.go and are backed by
// engine.ProtocolBridgeManager plus the persisted definitions in system_settings.
func RegisterProtocolBridgeRoutes(g *echo.Group) {
	g.GET("/status", handleGetProtocolBridgeStatus, requirePermission(security.PermSystemConfig))
	g.GET("/bridges", handleListProtocolBridges, requirePermission(security.PermSystemConfig))
	g.POST("/bridges", handleCreateProtocolBridge, requirePermission(security.PermSystemConfig))
	g.GET("/bridges/:id", handleGetProtocolBridge, requirePermission(security.PermSystemConfig))
	g.PUT("/bridges/:id", handleUpdateProtocolBridge, requirePermission(security.PermSystemConfig))
	g.DELETE("/bridges/:id", handleDeleteProtocolBridge, requirePermission(security.PermSystemConfig))
	g.POST("/bridges/:id/enable", handleSetProtocolBridgeEnabled(true), requirePermission(security.PermSystemConfig))
	g.POST("/bridges/:id/disable", handleSetProtocolBridgeEnabled(false), requirePermission(security.PermSystemConfig))
}

// RegisterObservabilityRoutes registers observability API routes.
func RegisterObservabilityRoutes(g *echo.Group) {
	g.GET("/overview", handleGetObservabilityOverview, requirePermission(security.PermSystemConfig))
	g.GET("/health", handleGetObservabilityHealth, requirePermission(security.PermSystemConfig))
	g.GET("/events", handleGetObservabilityEvents, requirePermission(security.PermSystemConfig))
	g.GET("/rules", handleGetObservabilityRules, requirePermission(security.PermSystemConfig))
	g.GET("/traces", handleGetTraces, requirePermission(security.PermSystemConfig))
	g.GET("/spans", handleGetSpans, requirePermission(security.PermSystemConfig))
}

// handleGetObservabilityOverview reports what the gateway can actually measure.
// HTTP request totals need a request counter that this build does not have, so
// those keys come back null and are named in not_collected: the previous
// hardcoded zeros read as "no traffic and no errors" on a loaded gateway.
func handleGetObservabilityOverview(c echo.Context) error {
	resp := map[string]interface{}{
		"requests_total": nil,
		"error_rate":     nil,
		"avg_latency_ms": nil,
		"p99_latency_ms": nil,
		"active_alerts":  0,
		"uptime_seconds": int64(time.Since(startTime).Seconds()),
		"goroutines":     runtime.NumGoroutine(),
		"not_collected":  []string{"requests_total", "error_rate", "avg_latency_ms", "p99_latency_ms"},
	}
	cont := GetContainer()
	if cont != nil && cont.AlarmRepo != nil {
		if _, total, err := cont.AlarmRepo.List(models.AlarmFilter{Status: "active"}, 1, 1); err == nil {
			resp["active_alerts"] = total
		}
	}
	if cont != nil && cont.EventBus != nil {
		em := cont.EventBus.Metrics()
		resp["events_published"] = em["published"]
		resp["events_dropped"] = em["dropped"]
	}
	return OK(c, resp)
}

// percentOrUnmeasured turns a host percentage into null when the snapshot that
// produced it read nothing at all. gopsutil returns 0 for both "the disk is
// empty" and "the syscall failed", and a green 0% bar cannot tell those apart.
func percentOrUnmeasured(total uint64, percent float64) interface{} {
	if total == 0 {
		return nil
	}
	return percent
}

// handleGetObservabilityHealth returns system health metrics for the observability dashboard.
//
// The CPU / memory / disk percentages were literal zeros, which rendered as
// three green 0% progress bars: a page whose only job is to signal distress
// reported all-clear no matter what the host was doing. gopsutil already backs
// these numbers for the status endpoint, so this reuses the same sources.
func handleGetObservabilityHealth(c echo.Context) error {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	memTotal, memUsed, memPercent := memorySnapshot()
	diskTotal, diskUsed, diskPercent := diskSnapshot(".")

	resp := map[string]interface{}{
		"cpu_percent":       cpuPercentCached(),
		"memory_percent":    percentOrUnmeasured(memTotal, memPercent),
		"disk_percent":      percentOrUnmeasured(diskTotal, diskPercent),
		"memory_used_bytes": memUsed,
		"disk_used_bytes":   diskUsed,
		"mem_alloc_bytes":   mem.Alloc,
		"goroutines":        runtime.NumGoroutine(),
		"uptime_seconds":    int64(time.Since(startTime).Seconds()),
		// Nothing counts HTTP requests or tracks live connections in this build.
		"total_requests":     nil,
		"error_count":        nil,
		"avg_response_ms":    nil,
		"active_connections": nil,
		"not_collected":      []string{"total_requests", "error_count", "avg_response_ms", "active_connections"},
	}

	cont := GetContainer()
	if cont != nil && cont.EventBus != nil {
		em := cont.EventBus.Metrics()
		resp["events_published"] = em["published"]
		resp["events_delivered"] = em["delivered"]
		resp["events_dropped"] = em["dropped"]
		resp["event_queue_length"] = em["queue_length"]
		resp["event_queue_capacity"] = em["queue_capacity"]
	}
	return OK(c, resp)
}

// handleGetObservabilityEvents returns observability events for the dashboard.
// No event store backs this in the current build, so the payload says so: an
// empty list alone is indistinguishable from "nothing happened".
func handleGetObservabilityEvents(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"items":     []interface{}{},
		"supported": false,
	})
}

// handleGetObservabilityRules returns observability alert rules for the dashboard.
func handleGetObservabilityRules(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"items":     []interface{}{},
		"supported": false,
	})
}

func handleGetTraces(c echo.Context) error {
	// No trace collector is wired in this build. The response says so because a
	// bare empty page is what a working, quiet tracer would also return.
	return OK(c, map[string]interface{}{
		"items":     []interface{}{},
		"total":     0,
		"page":      1,
		"size":      20,
		"supported": false,
	})
}

func handleGetSpans(c echo.Context) error {
	return OKPaged(c, []interface{}{}, 0, 1, 20)
}

// RegisterScriptsRoutes registers script management API routes.
// Handlers (list/create/update/delete/execute/test) live in script_engine.go.
func RegisterScriptsRoutes(g *echo.Group) {
	g.GET("", handleListScripts, requirePermission(security.PermSystemConfig))
	g.POST("", handleCreateScript, requirePermission(security.PermSystemConfig))
	g.POST("/test", handleTestScriptCode, requirePermission(security.PermSystemConfig))
	g.PUT("/:id", handleUpdateScript, requirePermission(security.PermSystemConfig))
	g.DELETE("/:id", handleDeleteScript, requirePermission(security.PermSystemConfig))
	g.POST("/:id/execute", handleExecuteScript, requirePermission(security.PermSystemConfig))
}

// RegisterFirmwareSignatureRoutes registers firmware signature API routes.
// Handlers (list/sign/verify) live in firmware_signature.go.
func RegisterFirmwareSignatureRoutes(g *echo.Group) {
	g.GET("", handleListFirmwareSignatures, requirePermission(security.PermOTAManage))
	g.POST("/verify", handleVerifyFirmwareNoID, requirePermission(security.PermOTAManage))
	g.POST("/:id/verify", handleVerifyFirmware, requirePermission(security.PermOTAManage))
	g.POST("/sign", handleSignFirmware, requirePermission(security.PermOTAManage))
	g.POST("", handleSignFirmware, requirePermission(security.PermOTAManage))
}

// RegisterDeviceLinkageRoutes registers device linkage API routes.
func RegisterDeviceLinkageRoutes(g *echo.Group) {
	g.GET("/status", handleGetLinkageStatus, requirePermission(security.PermDeviceRead))
	g.GET("", handleListDeviceLinkages, requirePermission(security.PermDeviceRead))
	g.POST("", handleCreateDeviceLinkage, requirePermission(security.PermDeviceUpdate))
	g.GET("/:id/stats", handleGetLinkageRuleStats, requirePermission(security.PermDeviceRead))
	g.PATCH("/:id", handlePatchDeviceLinkage, requirePermission(security.PermDeviceUpdate))
	g.DELETE("/:id", handleDeleteDeviceLinkage, requirePermission(security.PermDeviceUpdate))
}

func deviceNameLookup() map[string]string {
	names := map[string]string{}
	if cont := GetContainer(); cont != nil && cont.DeviceRepo != nil {
		devs, _, _ := cont.DeviceRepo.List(1, 1000)
		for _, d := range devs {
			names[d.DeviceID] = d.Name
		}
	}
	return names
}

func handleListDeviceLinkages(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return OK(c, map[string]interface{}{"items": []interface{}{}})
	}
	rules, err := cont.AlarmRepo.ListDeviceLinkages()
	if err != nil {
		logrus.WithError(err).Error("List device linkages failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	names := deviceNameLookup()
	items := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		sn, _ := names[r.SourceDeviceID]
		tn, _ := names[r.TargetDeviceID]
		if sn == "" {
			sn = r.SourceDeviceID
		}
		if tn == "" {
			tn = r.TargetDeviceID
		}
		item := map[string]interface{}{
			"id":                 r.ID,
			"name":               r.Name,
			"source_device_id":   r.SourceDeviceID,
			"source_device_name": sn,
			"source_point":       r.SourcePoint,
			"condition_op":       r.ConditionOp,
			"threshold":          r.Threshold,
			"target_device_id":   r.TargetDeviceID,
			"target_device_name": tn,
			"target_point":       r.TargetPoint,
			"target_value":       r.TargetValue,
			"enabled":            r.Enabled,
			"trigger_count":      r.TriggerCount,
			"last_triggered_at":  r.LastTriggeredAt,
		}
		// The persisted counter only says a rule fired; the live stats say why it
		// has not (device offline, non-numeric source, rejected write).
		if cont.LinkageEvaluator != nil {
			if ls := cont.LinkageEvaluator.RuleStats(r.ID); ls != nil {
				item["runtime"] = map[string]interface{}{
					"watching":        ls.HasValue,
					"satisfied":       ls.Satisfied,
					"last_value":      ls.LastValue,
					"live_transfers":  ls.Transfers,
					"live_errors":     ls.Errors,
					"skipped_samples": ls.SkippedSamples,
					"retry_pending":   ls.RetryPending,
					"last_error":      ls.LastError,
				}
			}
		}
		items = append(items, item)
	}
	return OK(c, map[string]interface{}{"items": items})
}

func handleCreateDeviceLinkage(c echo.Context) error {
	var req struct {
		Name           string  `json:"name"`
		SourceDeviceID string  `json:"source_device_id"`
		SourcePoint    string  `json:"source_point"`
		ConditionOp    string  `json:"condition_op"`
		Threshold      float64 `json:"threshold"`
		TargetDeviceID string  `json:"target_device_id"`
		TargetPoint    string  `json:"target_point"`
		TargetValue    string  `json:"target_value"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	rule := &storage.LinkageRuleRecord{
		ID:             newLinkageRuleID(),
		Name:           req.Name,
		SourceDeviceID: req.SourceDeviceID,
		SourcePoint:    req.SourcePoint,
		ConditionOp:    req.ConditionOp,
		Threshold:      req.Threshold,
		TargetDeviceID: req.TargetDeviceID,
		TargetPoint:    req.TargetPoint,
		TargetValue:    req.TargetValue,
		Enabled:        true,
	}
	if err := validateDeviceLinkage(cont, rule); err != nil {
		return linkageErrResponse(c, err)
	}
	if err := cont.AlarmRepo.CreateDeviceLinkage(rule); err != nil {
		logrus.WithError(err).Error("Create device linkage failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if err := applyLinkageRules(cont); err != nil {
		logrus.WithError(err).Error("Linkage rules could not be reloaded after create")
	}
	return Created(c, rule)
}

func handlePatchDeviceLinkage(c echo.Context) error {
	id := c.Param("id")
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Enabled == nil {
		return BadRequest(c, "enabled required")
	}
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if err := cont.AlarmRepo.SetDeviceLinkageEnabled(id, *req.Enabled); err != nil {
		return NotFound(c, "ERR_LINKAGE_RULE_NOT_FOUND")
	}
	if err := applyLinkageRules(cont); err != nil {
		logrus.WithError(err).Error("Linkage rules could not be reloaded after enable change")
	}
	return OK(c, map[string]interface{}{"id": id, "enabled": *req.Enabled})
}

func handleDeleteDeviceLinkage(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if err := cont.AlarmRepo.DeleteDeviceLinkage(id); err != nil {
		return NotFound(c, "ERR_LINKAGE_RULE_NOT_FOUND")
	}
	if err := applyLinkageRules(cont); err != nil {
		logrus.WithError(err).Error("Linkage rules could not be reloaded after delete")
	}
	logrus.WithField("linkage_id", id).Info("Device linkage deleted")
	return OK(c, map[string]interface{}{"id": id, "deleted": true})
}

// RegisterAnomalyLearnerRoutes registers anomaly learner API routes.
func RegisterAnomalyLearnerRoutes(g *echo.Group) {
	g.GET("/stats", handleGetAnomalyLearnerStats, requirePermission(security.PermSystemConfig))
	g.POST("/reset", handleResetAnomalyLearner, requirePermission(security.PermSystemConfig))
	g.POST("/threshold", handleSetAnomalyThreshold, requirePermission(security.PermSystemConfig))
}

// The anomaly / trend / threshold learner route groups have no counterpart in
// internal/services: nothing trains a model, nothing stores a threshold, so a
// reset answered {"status":"reset"} and a threshold POST answered {"success":true}
// for values that were never kept anywhere. Stats still report zeros because that
// is the true count, but now they also say the learner is not implemented so an
// operator reading the page is not told a model set exists.
func learnerNotImplemented(c echo.Context, feature string) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_LEARNER_UNSUPPORTED",
		feature+" is not implemented in this build: the gateway ships no self-learning service, so nothing to reset or configure")
}

func learnerStats(feature string) map[string]interface{} {
	return map[string]interface{}{
		"total_models": 0,
		"implemented":  false,
		"message":      feature + " is not implemented in this build; these counters are structural zeros, not measurements",
	}
}

func handleGetAnomalyLearnerStats(c echo.Context) error {
	resp := learnerStats("anomaly learner")
	resp["total_anomalies"] = 0
	return OK(c, resp)
}

func handleResetAnomalyLearner(c echo.Context) error {
	return learnerNotImplemented(c, "anomaly learner reset")
}

func handleSetAnomalyThreshold(c echo.Context) error {
	return learnerNotImplemented(c, "anomaly threshold configuration")
}

// RegisterTrendLearnerRoutes registers trend learner API routes.
func RegisterTrendLearnerRoutes(g *echo.Group) {
	g.GET("/stats", handleGetTrendLearnerStats, requirePermission(security.PermSystemConfig))
	g.POST("/reset", handleResetTrendLearner, requirePermission(security.PermSystemConfig))
}

func handleGetTrendLearnerStats(c echo.Context) error {
	resp := learnerStats("trend learner")
	resp["total_predictions"] = 0
	return OK(c, resp)
}

func handleResetTrendLearner(c echo.Context) error {
	return learnerNotImplemented(c, "trend learner reset")
}

// RegisterThresholdLearnerRoutes registers threshold learner API routes.
func RegisterThresholdLearnerRoutes(g *echo.Group) {
	g.GET("/stats", handleGetThresholdLearnerStats, requirePermission(security.PermSystemConfig))
	g.POST("/reset", handleResetThresholdLearner, requirePermission(security.PermSystemConfig))
}

func handleGetThresholdLearnerStats(c echo.Context) error {
	resp := learnerStats("threshold learner")
	resp["total_adjustments"] = 0
	return OK(c, resp)
}

func handleResetThresholdLearner(c echo.Context) error {
	return learnerNotImplemented(c, "threshold learner reset")
}

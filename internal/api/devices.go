package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/services"
)

// RegisterDeviceRoutes registers device management API routes.
func RegisterDeviceRoutes(g *echo.Group) {
	g.GET("", handleListDevices, requirePermission(security.PermDeviceRead))
	g.POST("", handleCreateDevice, requirePermission(security.PermDeviceCreate))
	g.POST("/simulator", handleCreateSimulator, requirePermission(security.PermDeviceCreate))
	g.POST("/discover", handleDiscoverDevices, requirePermission(security.PermDeviceCreate))
	g.POST("/test-connection", handleTestConnection, requirePermission(security.PermDeviceCreate))
	g.GET("/health/all", handleListAllDeviceHealth, requirePermission(security.PermDeviceRead))
	g.GET("/health", handleListDeviceHealthByIDs, requirePermission(security.PermDeviceRead))
	g.GET("/collect-stats", handleGetCollectStats, requirePermission(security.PermDeviceRead))
	g.GET("/device-quality-stats", handleGetDeviceQualityStats, requirePermission(security.PermDeviceRead))
	g.POST("/batch/delete", handleBatchDeleteDevices, requirePermission(security.PermDeviceDelete))
	g.POST("/batch/start-collect", handleBatchStartCollect, requirePermission(security.PermDeviceUpdate))
	g.POST("/batch/stop-collect", handleBatchStopCollect, requirePermission(security.PermDeviceUpdate))
	g.POST("/batch-deploy", handleBatchDeployConfig, requirePermission(security.PermSystemConfig))
	g.POST("/export", handleExportDevices, requirePermission(security.PermDeviceRead))
	g.POST("/import", handleImportDevices, requirePermission(security.PermDeviceCreate))
	g.POST("/templates", handleCreateTemplate, requirePermission(security.PermDeviceCreate))
	g.GET("/templates", handleListTemplates, requirePermission(security.PermDeviceRead))
	g.POST("/from-template", handleCreateFromTemplate, requirePermission(security.PermDeviceCreate))
	g.DELETE("/templates/:name", handleDeleteTemplate, requirePermission(security.PermDeviceDelete))

	// Device-specific routes (dynamic :device_id routes — registered after all static routes)
	g.GET("/:device_id", handleGetDevice, requirePermission(security.PermDeviceRead))
	g.PUT("/:device_id", handleUpdateDevice, requirePermission(security.PermDeviceUpdate))
	g.PUT("/:device_id/write-policy", handleUpdateWritePolicy, requirePermission(security.PermDeviceWritePolicyEdit))
	g.DELETE("/:device_id", handleDeleteDevice, requirePermission(security.PermDeviceDelete))
	g.GET("/:device_id/points", handleGetDevicePoints, requirePermission(security.PermDeviceRead))
	g.POST("/:device_id/points", handleWriteDevicePoint, requirePermission(security.PermDeviceWrite))
	g.POST("/:device_id/push", handlePushDeviceData, requirePermission(security.PermDeviceWrite))
	g.GET("/:device_id/health", handleGetDeviceHealth, requirePermission(security.PermDeviceRead))
	g.POST("/:device_id/health/reset", handleResetDeviceHealth, requirePermission(security.PermDeviceUpdate))
	g.POST("/:device_id/self-test", handleDeviceSelfTest, requirePermission(security.PermDeviceRead))
	g.GET("/:device_id/ops", handleGetDeviceOps, requirePermission(security.PermDeviceRead))
	g.POST("/:device_id/probe-primary", handleProbePrimaryLink, requirePermission(security.PermDeviceRead))
	g.GET("/:device_id/point-health", handleGetPointHealth, requirePermission(security.PermDeviceRead))
	g.GET("/:device_id/write-audit", handleGetWriteAudit, requirePermission(security.PermDeviceRead))
	g.GET("/:device_id/metrics", handleGetDeviceMetrics, requirePermission(security.PermSystemConfig))
	g.GET("/:device_id/collect-status", handleGetDeviceCollectStatus, requirePermission(security.PermDeviceRead))

	// Device config version management
	g.GET("/:device_id/config-versions", handleListDeviceConfigVersions, requirePermission(security.PermSystemConfig))
	g.GET("/:device_id/config-versions/current", handleGetDeviceConfigCurrent, requirePermission(security.PermSystemConfig))
	g.GET("/:device_id/config-versions/:version", handleGetDeviceConfigVersion, requirePermission(security.PermSystemConfig))
	g.POST("/:device_id/config-versions", handleSaveDeviceConfigVersion, requirePermission(security.PermSystemConfig))
	g.POST("/:device_id/config-versions/rollback", handleRollbackDeviceConfig, requirePermission(security.PermSystemConfig))
	g.GET("/:device_id/config-versions/audit", handleGetDeviceConfigAuditTrail, requirePermission(security.PermSystemConfig))
	g.GET("/:device_id/config-versions/diff", handleDiffDeviceConfigVersions, requirePermission(security.PermSystemConfig))
}

// handleListDevices returns a paginated list of devices.
func handleListDevices(c echo.Context) error {
	user := getUserFromContext(c)
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	page, size := parsePagination(c)

	status := c.QueryParam("status")
	protocol := c.QueryParam("protocol")
	search := c.QueryParam("search")
	collectingParam := c.QueryParam("collecting")

	// The repo pages before these filters can be applied, so whenever a filter is
	// in play the whole set has to be loaded first: paging a pre-paginated slice
	// hides matches on later pages and reports the page length as `total`.
	needFullScan := status != "" || protocol != "" || search != "" ||
		collectingParam == "true" || collectingParam == "false" ||
		user.Role != security.RoleAdmin

	var devices []models.DeviceResponse
	var total int
	var err error
	if needFullScan {
		devices, err = cont.DeviceRepo.ListAll()
	} else {
		devices, total, err = cont.DeviceService.List(page, size)
	}
	if err != nil {
		logrus.WithError(err).Error("List devices failed")
		return InternalError(c, "ERR_DEVICE_LIST_FAILED")
	}

	// Decorate with live collection state (drives start/stop collection UI + filter)
	if cont.Scheduler != nil {
		for i := range devices {
			devices[i].Collecting = cont.Scheduler.IsCollecting(devices[i].DeviceID)
		}
	}

	// Filter by status/protocol/search (in-memory, as repo doesn't support these)
	if status != "" || protocol != "" || search != "" {
		filtered := devices[:0]
		for _, d := range devices {
			if status != "" && d.Status != status {
				continue
			}
			if protocol != "" && d.Protocol != protocol {
				continue
			}
			if search != "" && !containsInsensitive(d.Name, search) && !containsInsensitive(d.DeviceID, search) {
				continue
			}
			filtered = append(filtered, d)
		}
		devices = filtered
	}

	// Filter by collection state: collecting=true|false
	if collectingParam == "true" || collectingParam == "false" {
		want := collectingParam == "true"
		filtered := devices[:0]
		for _, d := range devices {
			if d.Collecting == want {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	// Filter by ownership for non-admin
	if user.Role != security.RoleAdmin {
		filtered := devices[:0]
		for _, d := range devices {
			if d.CreatedBy == user.UserID {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	if needFullScan {
		total = len(devices)
		devices = paginateSlice(devices, page, size)
	}

	if devices == nil {
		devices = []models.DeviceResponse{}
	}

	return OKPaged(c, devices, total, page, size)
}

// handleCreateDevice creates a new device.
func handleCreateDevice(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.DeviceCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Name == "" {
		return BadRequest(c, "ERR_DEVICE_CONFIG_INVALID: name required")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	device, err := cont.DeviceService.Create(&req, user.UserID)
	if err != nil {
		logrus.WithError(err).WithField("device_id", req.DeviceID).Error("Create device failed")
		errMsg := err.Error()
		if containsStr(errMsg, "already exists") || containsStr(errMsg, "duplicate") {
			return Conflict(c, "ERR_DEVICE_ALREADY_EXISTS")
		}
		if containsStr(errMsg, "unsupported protocol") {
			return BadRequest(c, "ERR_DEVICE_DRIVER_UNAVAILABLE")
		}
		return BadRequest(c, "ERR_DEVICE_CONFIG_INVALID")
	}

	recordAudit(c, "device_create", "device", device.DeviceID, "success", map[string]interface{}{"name": device.Name, "protocol": device.Protocol})

	return Created(c, device)
}

// handleCreateSimulator creates a simulator device.
func handleCreateSimulator(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.SimulatorCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Name == "" {
		return BadRequest(c, "Name required")
	}

	// Convert simulator request to device create
	createReq := &models.DeviceCreate{
		DeviceID:        req.DeviceID,
		Name:            req.Name,
		Protocol:        "simulator",
		Config:          map[string]interface{}{"timeout": 5.0},
		Points:          req.Points,
		CollectInterval: req.CollectInterval,
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	device, err := cont.DeviceService.Create(createReq, user.UserID)
	if err != nil {
		logrus.WithError(err).Error("Create simulator failed")
		return InternalError(c, "ERR_DEVICE_SIMULATOR_FAILED")
	}

	return Created(c, device)
}

// discoverProtocolAvailable reports whether a discovery request names a driver this
// build carries. Both discovery routes used to report an unknown protocol as
// ERR_DEVICE_DISCOVER_FAILED (500), blaming the gateway for a request it never
// tried to serve.
func discoverProtocolAvailable(protocol string) bool {
	return constants.NormalizeProtocol(protocol) != ""
}

// handleDiscoverDevices discovers devices on the network.
func handleDiscoverDevices(c echo.Context) error {
	var req models.DiscoverRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	if !discoverProtocolAvailable(req.Protocol) {
		return ErrorCode(c, http.StatusBadRequest, "ERR_DRIVER_NOT_AVAILABLE",
			fmt.Sprintf("No driver named %q is installed in this gateway, so it cannot be scanned for devices", req.Protocol))
	}
	devices, err := cont.DeviceService.Discover(c.Request().Context(), &req)
	if err != nil {
		// "This protocol has no discovery" is not a server fault: answering 500
		// made the dialog look like a broken network rather than a missing feature.
		// opc_da needs the Windows COM registry, which this build does not link.
		if errors.Is(err, drivers.ErrDiscoveryUnsupported) || errors.Is(err, drivers.ErrOPCDAUnsupported) {
			return ErrorCode(c, http.StatusNotImplemented,
				"ERR_DRIVER_DISCOVER_UNSUPPORTED",
				"ERR_DRIVER_DISCOVER_UNSUPPORTED")
		}
		logrus.WithError(err).Error("Discover devices failed")
		return InternalError(c, "ERR_DEVICE_DISCOVER_FAILED")
	}

	if devices == nil {
		devices = []map[string]interface{}{}
	}
	return OK(c, devices)
}

// connectionTestTimeout matches the "3 second timeout" the device form advertises
// next to the button.
const connectionTestTimeout = 3 * time.Second

// tcpTestDefaults are the ports each protocol dials when the device config omits
// one; they mirror the defaults the drivers themselves use. Protocols with no
// peer socket (modbus_rtu, simulator, http_webhook, opc_da, modbus_slave) are
// absent so the pre-check reports "unsupported" instead of passing a test that
// never happened.
var tcpTestDefaults = map[string]int{
	"modbus_tcp":    502,
	"siemens_s7":    102,
	"mitsubishi_mc": 5000,
	"omron_fins":    9600,
	"allen_bradley": constants.AllenBradleyDefaultPort,
	"mqtt_client":   1883,
	"onvif":         80,
	"opc_ua":        4840,
}

// deviceTCPTestTarget resolves the socket a protocol connects to, reading the same
// config keys the drivers do. supported reports whether the protocol has a peer
// socket at all; an empty host with supported true then means the device carries no
// usable address, which callers report differently from "cannot be probed".
func deviceTCPTestTarget(protocol string, config map[string]interface{}) (string, int, bool) {
	defaultPort, supported := tcpTestDefaults[protocol]
	if !supported {
		return "", 0, false
	}
	for _, key := range []string{"endpoint", "server_url", "broker", "host", "ip", "address"} {
		value, _ := config[key].(string)
		if value == "" {
			continue
		}
		host, port, explicit, ok := parseSocketTarget(value, defaultPort)
		if !ok {
			continue
		}
		if explicit {
			// The address spelled its own port out; that is the more specific
			// statement of the two.
			return host, port, true
		}
		configured, present := configSocketPort(config)
		if !present {
			return host, port, true
		}
		if configured == 0 {
			// A port key that is present but unusable must not fall back to the
			// protocol default: the test would report on an endpoint nobody set.
			return "", 0, true
		}
		return host, configured, true
	}
	return "", 0, true
}

// configSocketPort reads the port field of a device config in whichever shape it
// arrives: JSON decodes numbers as float64, the YAML loader as int, and some forms
// send a string. present reports that a port was configured, port 0 meaning it was
// configured but cannot be a TCP port.
func configSocketPort(config map[string]interface{}) (int, bool) {
	for _, key := range []string{"port", "tcp_port", "broker_port"} {
		var text string
		switch v := config[key].(type) {
		case nil:
			continue
		case float64:
			text = strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			text = strconv.Itoa(v)
		case int64:
			text = strconv.FormatInt(v, 10)
		case string:
			text = v
		default:
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, true
		}
		if port <= 0 || port > 65535 {
			return 0, true
		}
		return port, true
	}
	return 0, false
}

// parseSocketTarget accepts a scheme URL ("opc.tcp://10.0.0.5:4840/path"), a
// "host:port" pair or a bare host name, filling in def when no port is present.
// The third result reports whether the address carried a port of its own, which is
// what decides whether the separate "port" config key applies.
func parseSocketTarget(raw string, def int) (string, int, bool, bool) {
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return "", 0, false, false
		}
		if parsed.Port() == "" {
			return parsed.Hostname(), def, false, true
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			return "", 0, false, false
		}
		return parsed.Hostname(), port, true, true
	}
	if strings.Contains(raw, ":") {
		host, portText, err := net.SplitHostPort(raw)
		if err != nil || host == "" {
			return "", 0, false, false
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return "", 0, false, false
		}
		return host, port, true, true
	}
	if strings.ContainsAny(raw, "/ \\") {
		return "", 0, false, false
	}
	return raw, def, false, true
}

// handleTestConnection performs the TCP reachability pre-check the device form
// describes. It used to answer success:true for every request, so an unreachable
// host or a closed port looked tested -- and the operator only found out once
// the device was saved and staying offline.
func handleTestConnection(c echo.Context) error {
	var req models.DiscoverRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	config := req.Config
	if config == nil {
		return BadRequest(c, "No config to test")
	}

	protocol := constants.NormalizeProtocol(req.Protocol)
	host, port, supported := deviceTCPTestTarget(protocol, config)
	if !supported {
		return OK(c, map[string]interface{}{
			"success":   false,
			"supported": false,
			"message":   "ERR_DEVICE_TEST_CONNECTION_UNSUPPORTED",
		})
	}
	if host == "" {
		return OK(c, map[string]interface{}{
			"success":   false,
			"supported": true,
			"message":   "ERR_DEVICE_TEST_NO_ADDRESS",
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), connectionTestTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: connectionTestTimeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	started := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		logrus.WithError(err).
			WithField("address", addr).
			WithField("protocol", protocol).
			Warn("Device connection test failed")
		return OK(c, map[string]interface{}{
			"success":   false,
			"supported": true,
			"host":      host,
			"port":      port,
			"message":   "ERR_DEVICE_TEST_CONNECTION_FAILED",
		})
	}
	_ = conn.Close()

	return OK(c, map[string]interface{}{
		"success":    true,
		"supported":  true,
		"host":       host,
		"port":       port,
		"latency_ms": float64(time.Since(started).Microseconds()) / 1000.0,
	})
}

// handleListAllDeviceHealth returns health status for all devices.
func handleListAllDeviceHealth(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	limit := 100
	if l, err := strconv.Atoi(c.QueryParam("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}

	devices, _, err := cont.DeviceService.List(1, limit)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_LIST_FAILED")
	}

	items := make([]map[string]interface{}, 0, len(devices))
	// The dashboard's "N devices degraded" panel reads these entries, so they carry
	// the measured counters: with only a status here it had nothing to compute a
	// score from and every device looked healthy.
	var statsByID map[string]*drivers.DriverHealthStats
	if mgr := drivers.GetHealthStatsManager(); mgr != nil {
		statsByID = mgr.GetAllHealthStats()
	}
	for _, d := range devices {
		entry := map[string]interface{}{
			"device_id":                d.DeviceID,
			"name":                     d.Name,
			"status":                   d.Status,
			"protocol":                 d.Protocol,
			"online_rate":              nil,
			"connection_quality_score": nil,
			"consecutive_failures":     int64(0),
			"total_reads":              int64(0),
			"failed_reads":             int64(0),
			"has_samples":              false,
			"last_error":               "",
		}
		if st := statsByID[d.DeviceID]; st != nil {
			ctr := st.Counters()
			total := ctr.TotalReads + ctr.TotalWrites
			failed := ctr.FailedReads + ctr.FailedWrites
			entry["has_samples"] = total > 0
			entry["consecutive_failures"] = ctr.ConsecutiveFailures
			entry["total_reads"] = ctr.TotalReads
			entry["failed_reads"] = ctr.FailedReads
			if total > 0 {
				entry["online_rate"] = float64(total-failed) / float64(total) * 100.0
				entry["connection_quality_score"] = ctr.ConnectionQualityScore
			}
		}
		if cont.Scheduler != nil {
			if status, ok := cont.Scheduler.GetDeviceStatus(d.DeviceID); ok {
				if v, ok := status["last_error"].(string); ok && v != "" {
					entry["last_error"] = v
				}
			}
		}
		items = append(items, entry)
	}

	return OK(c, map[string]interface{}{
		"items":  items,
		"total":  len(items),
		"limit":  limit,
		"offset": 0,
	})
}

// maxBatchSize limits the number of items in a single batch operation
// to prevent abuse and resource exhaustion.
const maxBatchSize = 500

// handleBatchDeleteDevices deletes multiple devices.
func handleBatchDeleteDevices(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.BatchDeviceIDs
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.DeviceIDs) == 0 {
		return BadRequest(c, "device_ids required")
	}
	if len(req.DeviceIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	successCount := 0
	failed := make(map[string]string)
	for _, id := range req.DeviceIDs {
		// Check ownership for non-admin
		if user.Role != security.RoleAdmin {
			device, _ := cont.DeviceService.Get(id)
			if device != nil && device.CreatedBy != user.UserID {
				failed[id] = "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED"
				continue
			}
		}
		if err := cont.DeviceService.Delete(id); err != nil {
			logrus.WithError(err).WithField("device_id", id).Warn("Batch delete device failed")
			failed[id] = "ERR_DEVICE_DELETE_FAILED"
		} else {
			successCount++
		}
	}

	if successCount > 0 {
		recordAudit(c, "device_delete", "device", "", "success", map[string]interface{}{"batch": true, "deleted": successCount, "failed": len(failed)})
	}

	return OK(c, map[string]interface{}{
		"success_count": successCount,
		"failed":        failed,
	})
}

// handleBatchStartCollect starts collection for multiple devices.
func handleBatchStartCollect(c echo.Context) error {
	var req models.BatchDeviceIDs
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.DeviceIDs) == 0 {
		return BadRequest(c, "device_ids required")
	}
	if len(req.DeviceIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	successCount := 0
	failed := make(map[string]string)

	for _, id := range req.DeviceIDs {
		if cont.Scheduler == nil {
			failed[id] = "scheduler not available"
			continue
		}
		cont.Scheduler.StartCollector(id)
		// StartCollector is a no-op when the collector entry was removed by a
		// previous stop — rebuild the driver + collector so start actually works.
		if !cont.Scheduler.IsCollecting(id) {
			if cont.DeviceService == nil {
				failed[id] = "device service not available"
				continue
			}
			dev, err := cont.DeviceService.Get(id)
			if err != nil || dev == nil {
				failed[id] = "device not found"
				continue
			}
			if err := cont.DeviceService.SetupDriver(dev); err != nil {
				failed[id] = err.Error()
				continue
			}
		}
		successCount++
	}

	return OK(c, map[string]interface{}{
		"success_count": successCount,
		"failed":        failed,
	})
}

// handleBatchStopCollect stops collection for multiple devices.
func handleBatchStopCollect(c echo.Context) error {
	var req models.BatchDeviceIDs
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.DeviceIDs) == 0 {
		return BadRequest(c, "device_ids required")
	}
	if len(req.DeviceIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	successCount := 0
	failed := make(map[string]string)

	for _, id := range req.DeviceIDs {
		if cont.Scheduler != nil {
			cont.Scheduler.UnregisterCollector(id)
			successCount++
		} else {
			failed[id] = "scheduler not available"
		}
	}

	return OK(c, map[string]interface{}{
		"success_count": successCount,
		"failed":        failed,
	})
}

// handleBatchDeployConfig deploys config from a template device to target devices.
func handleBatchDeployConfig(c echo.Context) error {
	type BatchDeployRequest struct {
		TemplateDeviceID string                 `json:"template_device_id"`
		TargetDeviceIDs  []string               `json:"target_device_ids"`
		OverrideConfig   map[string]interface{} `json:"override_config,omitempty"`
	}

	var req BatchDeployRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.TemplateDeviceID == "" || len(req.TargetDeviceIDs) == 0 {
		return BadRequest(c, "template_device_id and target_device_ids required")
	}

	cont := GetContainer()
	template, err := cont.DeviceService.Get(req.TemplateDeviceID)
	if err != nil || template == nil {
		return NotFound(c, "ERR_DEVICE_TEMPLATE_NOT_FOUND")
	}

	success := []string{}
	failed := []map[string]string{}
	for _, targetID := range req.TargetDeviceIDs {
		updateData := &models.DeviceUpdate{
			Points:          &template.Points,
			CollectInterval: &template.CollectInterval,
		}
		if req.OverrideConfig != nil {
			updateData.Config = req.OverrideConfig
		}
		_, err := cont.DeviceService.Update(targetID, updateData)
		if err != nil {
			logrus.WithError(err).WithField("device_id", targetID).Warn("Batch apply template failed")
			failed = append(failed, map[string]string{"device_id": targetID, "error": "ERR_DEVICE_UPDATE_FAILED"})
		} else {
			success = append(success, targetID)
		}
	}

	return OK(c, map[string]interface{}{
		"success": success,
		"failed":  failed,
	})
}

// handleExportDevices exports device configurations.
func handleExportDevices(c echo.Context) error {
	var req models.ExportDevicesRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	data, err := cont.DeviceService.ExportDevices(&req)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_EXPORT_FAILED")
	}

	if data == nil {
		data = []map[string]interface{}{}
	}
	return OK(c, data)
}

// handleImportDevices imports device configurations.
func handleImportDevices(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.ImportDevicesRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	imported, failed, err := cont.DeviceService.ImportDevices(&req, user.UserID)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_IMPORT_FAILED")
	}

	return OK(c, map[string]interface{}{
		"imported": imported,
		"failed":   failed,
	})
}

// handleCreateTemplate creates a device template.
func handleCreateTemplate(c echo.Context) error {
	var req models.TemplateCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	template, err := cont.DeviceService.CreateTemplate(&req)
	if err != nil {
		return Conflict(c, "ERR_DEVICE_TEMPLATE_CREATE_FAILED")
	}

	return Created(c, template)
}

// handleListTemplates returns all device templates.
func handleListTemplates(c echo.Context) error {
	cont := GetContainer()
	templates, err := cont.DeviceService.ListTemplates()
	if err != nil {
		return InternalError(c, "ERR_DEVICE_TEMPLATE_LIST_FAILED")
	}
	if templates == nil {
		templates = []models.TemplateResponse{}
	}
	return OK(c, templates)
}

// handleCreateFromTemplate creates a device from a template.
func handleCreateFromTemplate(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.CreateFromTemplateRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	device, err := cont.DeviceService.CreateFromTemplate(&req, user.UserID)
	if err != nil {
		return Conflict(c, "ERR_DEVICE_FROM_TEMPLATE_FAILED")
	}

	return Created(c, device)
}

// handleDeleteTemplate deletes a device template.
func handleDeleteTemplate(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return BadRequest(c, "Template name required")
	}

	cont := GetContainer()
	if err := cont.DeviceService.DeleteTemplate(name); err != nil {
		return NotFound(c, "ERR_DEVICE_TEMPLATE_NOT_FOUND")
	}

	return OK(c, nil)
}

// handleGetCollectStats aggregates the collect scheduler's own view of every
// registered collector. It used to answer {}, which made "no devices" and
// "nobody implemented this" the same response.
func handleGetCollectStats(c echo.Context) error {
	cont := GetContainer()
	if cont.Scheduler == nil {
		return ServiceUnavailable(c, "ERR_COMMON_SERVICE_NOT_READY: collect scheduler is not running")
	}

	statuses := cont.Scheduler.GetAllStatus()
	collecting := 0
	erroring := 0
	for _, st := range statuses {
		if enabled, _ := st["enabled"].(bool); enabled {
			collecting++
		}
		if errs, _ := st["consecutive_errors"].(int); errs > 0 {
			erroring++
		}
	}

	return OK(c, map[string]interface{}{
		"collectors": len(statuses),
		"collecting": collecting,
		"erroring":   erroring,
		"devices":    statuses,
	})
}

// handleGetDevice returns a single device by ID.
func handleGetDevice(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, err := cont.DeviceService.Get(deviceID)
	if err != nil || device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	// Check ownership
	if user.Role != security.RoleAdmin && device.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	if cont.Scheduler != nil {
		device.Collecting = cont.Scheduler.IsCollecting(device.DeviceID)
	}

	return OK(c, device)
}

// handleUpdateDevice updates a device.
func handleUpdateDevice(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req models.DeviceUpdate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()

	// Check ownership
	existing, _ := cont.DeviceService.Get(deviceID)
	if existing == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	// The write governance settings live in device.config, so a plain device
	// update can change them. Only the ADMIN-only write-policy route is allowed
	// to, otherwise a role with device:update clears write_whitelist or turns off
	// write_verify without ever touching the gated endpoint.
	if req.Config != nil && writePolicyChanged(existing.Config, req.Config) {
		if !security.HasPermission(user.Role, security.PermDeviceWritePolicyEdit) {
			recordAudit(c, "device_write_policy_denied", "device", deviceID, "failure", nil)
			return ErrorCode(c, http.StatusForbidden, "ERR_WRITE_POLICY_FORBIDDEN",
				"Changing write_verify, write_rate_limit, write_audit or write_whitelist requires the device:write_policy_edit permission")
		}
	}

	// Save the config as it stands before this edit replaces it. Snapshotting the
	// incoming config instead would record the state the device is already in, and
	// the device's original config would never get a version — so the first edit
	// could not be undone.
	if req.Config != nil && cont.ConfigVersionMgr != nil {
		cont.ConfigVersionMgr.SnapshotDeviceConfig(deviceID, existing.Config,
			configChangedKeys(existing.Config, req.Config), user.Username)
	}

	device, err := cont.DeviceService.Update(deviceID, &req)
	if err != nil {
		logrus.WithError(err).Error("Update device failed")
		return InternalError(c, "ERR_DEVICE_UPDATE_FAILED")
	}

	recordAudit(c, "device_update", "device", deviceID, "success", nil)

	return OK(c, device)
}

// writePolicyConfigKeys are the config entries DeviceService.WritePoint enforces
// on every write path.
var writePolicyConfigKeys = []string{"write_verify", "write_rate_limit", "write_audit", "write_whitelist"}

// writePolicyChanged reports whether the incoming config moves any enforced
// write-policy value. A device update replaces the whole config map, so a key the
// request drops counts as a change even though no value was set.
func writePolicyChanged(existing, incoming map[string]interface{}) bool {
	for _, key := range writePolicyConfigKeys {
		if writePolicyValue(existing[key]) != writePolicyValue(incoming[key]) {
			return true
		}
	}
	return false
}

// writePolicyValue renders an enforced config entry the same way however it
// arrived: numbers come back from the database as float64, and the whitelist may
// be a JSON list or the comma-separated string a hand-authored config.yaml uses.
// Comparing raw values would report an unchanged policy as a change and lock
// every device edit. A missing whitelist and an empty one both read as "", which
// is right: neither restricts anyone.
func writePolicyValue(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case []string:
		return strings.Join(t, ",")
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, writePolicyValue(item))
		}
		return strings.Join(parts, ",")
	case string:
		fields := strings.Split(t, ",")
		parts := make([]string, 0, len(fields))
		for _, f := range fields {
			parts = append(parts, strings.TrimSpace(f))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprintf("%v", t)
	}
}

// handleDeleteDevice deletes a device.
func handleDeleteDevice(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()

	// Check ownership
	existing, _ := cont.DeviceService.Get(deviceID)
	if existing == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	if err := cont.DeviceService.Delete(deviceID); err != nil {
		return Conflict(c, "ERR_DEVICE_DELETE_FAILED")
	}

	recordAudit(c, "device_delete", "device", deviceID, "success", nil)

	return OK(c, nil)
}

// handleGetDevicePoints reads current point values from a device.
func handleGetDevicePoints(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()

	// Return cached data if available
	if cont.Cache != nil {
		data := cont.Cache.GetAll()
		result := make(map[string]interface{})
		for _, pd := range data {
			if pd.DeviceID == deviceID {
				result[pd.PointName] = pd.Value
			}
		}
		return OK(c, result)
	}

	return OK(c, map[string]interface{}{})
}

// auditActionDeviceWrite is the audit-trail action every device point write
// leaves behind, and the filter GET /:device_id/write-audit reads back. Nothing
// used to write these rows, which is why that endpoint could only answer with an
// empty list.
const auditActionDeviceWrite = "device_write"

// recordDeviceWrite appends one write to the tamper-chained audit trail, with
// the outcome the operator asked for rather than only successes: a rejected write
// to a read-only point is exactly what an auditor wants to see.
//
// config.write_audit=false on the device is the only way to opt out, so the check
// reads the stored device instead of trusting the request.
func recordDeviceWrite(c echo.Context, deviceID, point string, value interface{}, status string, failure error) {
	cont := GetContainer()
	if cont.DeviceRepo != nil {
		if dev, err := cont.DeviceRepo.Get(deviceID); err == nil && dev != nil {
			if enabled, ok := dev.Config["write_audit"].(bool); ok && !enabled {
				return
			}
		}
	}
	details := map[string]interface{}{"point": point, "value": value}
	if prev := lastCachedValue(cont, deviceID, point); prev != nil {
		details["old_value"] = prev
	}
	if failure != nil {
		details["error"] = failure.Error()
	}
	recordAudit(c, auditActionDeviceWrite, "device", deviceID, status, details)
}

// writeActorContext tags the request context with the user performing the write
// so the device write whitelist, enforced in the service where every write path
// converges, can see who is asking.
func writeActorContext(c echo.Context) context.Context {
	ctx := c.Request().Context()
	user := getUserFromContext(c)
	if user == nil {
		return ctx
	}
	return services.WithWriteActor(ctx, services.WriteActor{
		UserID:   user.UserID,
		Username: user.Username,
		Role:     user.Role,
	})
}

// lastCachedValue returns the most recently collected value of a point, which is
// what the operator was looking at when they typed the new one. Best effort: a
// device that has never been polled simply has no previous value to report.
func lastCachedValue(cont *ServiceContainer, deviceID, point string) interface{} {
	if cont.Cache == nil {
		return nil
	}
	for _, pd := range cont.Cache.GetAll() {
		if pd.DeviceID == deviceID && pd.PointName == point {
			return pd.Value
		}
	}
	return nil
}

// handleWriteDevicePoint writes a value to a device point.
func handleWriteDevicePoint(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req models.WritePointRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Point == "" {
		return BadRequest(c, "Point name required")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	if err := cont.DeviceService.WritePoint(writeActorContext(c), deviceID, &req); err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"device_id": deviceID,
			"point":     req.Point,
			"user":      user.Username,
		}).Error("Write point failed")
		recordDeviceWrite(c, deviceID, req.Point, req.Value, "failed", err)
		// A read-only point is a client-correctable mistake, not a device fault;
		// one code for both made every write failure indistinguishable in the UI.
		switch {
		case errors.Is(err, services.ErrReadOnlyPoint):
			return BadRequest(c, "ERR_POINT_READ_ONLY")
		case errors.Is(err, services.ErrWriteNotAllowed):
			return Forbidden(c, "ERR_WRITE_NOT_WHITELISTED")
		case errors.Is(err, services.ErrWriteRateLimited):
			return ErrorCode(c, http.StatusTooManyRequests, "ERR_WRITE_RATE_LIMITED", err.Error())
		case errors.Is(err, services.ErrWriteVerifyFailed):
			return ErrorCode(c, http.StatusBadGateway, "ERR_WRITE_VERIFY_FAILED", err.Error())
		}
		// The driver's own reason is the only thing that makes this actionable: a
		// server that answers StatusBadUserAccessDenied and one that is simply
		// offline look identical without it, and the operator has no way to tell
		// whether to fix the node, the credentials or the cable.
		return ErrorCode(c, http.StatusBadRequest, "ERR_DEVICE_WRITE_FAILED", err.Error())
	}
	recordDeviceWrite(c, deviceID, req.Point, req.Value, "success", nil)

	return OK(c, nil)
}

// handlePushDeviceData pushes external data to a device.
func handlePushDeviceData(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req models.PushDeviceDataRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.Data) == 0 {
		return BadRequest(c, "ERR_DEVICE_PUSH_EMPTY")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	if err := cont.DeviceService.PushData(deviceID, &req); err != nil {
		return BadRequest(c, "ERR_DEVICE_PUSH_FAILED")
	}

	return OK(c, nil)
}

// handleGetDeviceHealth returns device health status.
// 采集调度器中有该设备的采集记录时返回真实统计（最近采集时间/成败计数/延迟/错误），
// 否则退回设备表状态。
func handleGetDeviceHealth(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	resp := map[string]interface{}{
		"device_id":            device.DeviceID,
		"status":               device.Status,
		"protocol":             device.Protocol,
		"last_check":           "",
		"consecutive_failures": 0,
		"success_count":        int64(0),
		"fail_count":           int64(0),
		"latency_ms":           int64(0),
		"message":              "",
	}

	if cont.Scheduler != nil {
		if status, ok := cont.Scheduler.GetDeviceStatus(deviceID); ok {
			if v, ok := status["last_collect_at"].(string); ok && v != "" {
				resp["last_check"] = v
			}
			if v, ok := status["consecutive_errors"].(int); ok {
				resp["consecutive_failures"] = v
			}
			if v, ok := status["success_count"].(int64); ok {
				resp["success_count"] = v
			}
			if v, ok := status["fail_count"].(int64); ok {
				resp["fail_count"] = v
			}
			if v, ok := status["latency_ms"].(int64); ok {
				resp["latency_ms"] = v
			}
			if v, ok := status["last_error"].(string); ok {
				resp["message"] = v
			}
		}
	}

	return OK(c, resp)
}

// handleResetDeviceHealth resets device health counters.
func handleResetDeviceHealth(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	if cont.Scheduler != nil {
		cont.Scheduler.ResetDeviceStats(deviceID)
	}
	if cont.CBRegistry != nil {
		// Reset circuit breaker
		cb := cont.CBRegistry.Get(deviceID)
		cb.Reset()
	}

	return OK(c, nil)
}

// handleGetDeviceMetrics returns the driver's own health counters for a device.
// It used to answer fixed zeros with a 100.0 quality score, so every device --
// including one that had never connected -- looked perfect to whatever read it.
func handleGetDeviceMetrics(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	resp := map[string]interface{}{
		"device_id":                deviceID,
		"protocol":                 device.Protocol,
		"read_error_rate":          0.0,
		"write_error_rate":         0.0,
		"consecutive_failures":     int64(0),
		"connection_quality_score": nil,
		"total_downtime_seconds":   0.0,
		"last_online_at":           nil,
		"last_offline_at":          nil,
		"avg_latency_ms":           nil,
		"p95_latency_ms":           nil,
		"reconnect_count":          int64(0),
		"total_reads":              int64(0),
		"failed_reads":             int64(0),
		"total_writes":             int64(0),
		"failed_writes":            int64(0),
		"healthy":                  false,
		// No samples means the driver has never completed a collection cycle for
		// this device; report it instead of implying an untested link is fine.
		"has_samples": false,
	}

	mgr := drivers.GetHealthStatsManager()
	if mgr == nil {
		return OK(c, resp)
	}
	if st := mgr.GetHealthStats(deviceID); st != nil {
		ctr := st.Counters()
		resp["read_error_rate"] = st.ReadErrorRate()
		resp["write_error_rate"] = st.WriteErrorRate()
		resp["consecutive_failures"] = ctr.ConsecutiveFailures
		resp["total_downtime_seconds"] = ctr.TotalDowntimeSeconds
		resp["last_online_at"] = ctr.LastOnlineAt
		resp["last_offline_at"] = ctr.LastOfflineAt
		resp["reconnect_count"] = ctr.TotalReconnects
		resp["total_reads"] = ctr.TotalReads
		resp["failed_reads"] = ctr.FailedReads
		resp["total_writes"] = ctr.TotalWrites
		resp["failed_writes"] = ctr.FailedWrites
		resp["healthy"] = st.IsHealthy()
		resp["has_samples"] = ctr.TotalReads+ctr.TotalWrites > 0
		// Same rule as /ops: quality and latency are null until measured, so the
		// score's initial 100.0 cannot be read as a healthy untested link.
		if ctr.TotalReads+ctr.TotalWrites > 0 {
			resp["connection_quality_score"] = ctr.ConnectionQualityScore
		}
		if ctr.HasLatencySample {
			resp["avg_latency_ms"] = ctr.AvgLatencyMs
			resp["p95_latency_ms"] = st.P95LatencyMs()
		}
	}
	if status := mgr.GetConnectionStatus(deviceID); status != nil {
		resp["connection_state"] = string(status.State)
		resp["state_reason"] = status.Reason
		resp["last_error"] = status.LastError
	}
	return OK(c, resp)
}

// handleGetDeviceCollectStatus returns the collection scheduler status for a specific device.
func handleGetDeviceCollectStatus(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont.Scheduler == nil {
		return ServiceUnavailable(c, "Scheduler not available")
	}
	status, ok := cont.Scheduler.GetDeviceStatus(deviceID)
	if !ok {
		return NotFound(c, "Device collector not found")
	}
	return OK(c, status)
}

// Helper functions

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOfContains(s, substr) >= 0)
}

func indexOfContains(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func containsInsensitive(s, substr string) bool {
	return containsStr(toLowerStr(s), toLowerStr(substr))
}

func toLowerStr(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result[i] = c
	}
	return string(result)
}

// handleListDeviceHealthByIDs returns health status for devices by IDs.
func handleListDeviceHealthByIDs(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	ids := c.QueryParams()["ids"]
	result := make(map[string]interface{})
	mgr := drivers.GetHealthStatsManager()
	for _, id := range ids {
		if id == "" {
			continue
		}
		device, _ := cont.DeviceService.Get(id)
		if device == nil {
			continue
		}
		entry := map[string]interface{}{
			"device_id":            device.DeviceID,
			"status":               device.Status,
			"protocol":             device.Protocol,
			"consecutive_failures": int64(0),
			"last_check":           nil,
		}
		// The counters used to be a hard-coded 0 and an empty last_check for every
		// device, which is a report that was never read from anything.
		if mgr != nil {
			if st := mgr.GetHealthStats(id); st != nil {
				ctr := st.Counters()
				entry["consecutive_failures"] = ctr.ConsecutiveFailures
				if ctr.LastOnlineAt != nil {
					entry["last_check"] = ctr.LastOnlineAt
				}
			}
		}
		result[id] = entry
	}
	return OK(c, result)
}

// handleGetDeviceQualityStats returns device quality statistics.
func handleGetDeviceQualityStats(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	devices, _, err := cont.DeviceService.List(1, 100)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_LIST_FAILED")
	}

	result := make(map[string]interface{})
	mgr := drivers.GetHealthStatsManager()
	for _, d := range devices {
		row := map[string]interface{}{
			"device_id":     d.DeviceID,
			"success_count": int64(0),
			"error_count":   int64(0),
			"total_count":   int64(0),
			"error_rate":    0.0,
			// Without a sample the error rate above is "nothing measured yet", and
			// a reader has to be able to tell that apart from a clean 0%.
			"has_samples": false,
		}
		if mgr != nil {
			if st := mgr.GetHealthStats(d.DeviceID); st != nil {
				ctr := st.Counters()
				total := ctr.TotalReads + ctr.TotalWrites
				failed := ctr.FailedReads + ctr.FailedWrites
				row["total_count"] = total
				row["error_count"] = failed
				row["success_count"] = total - failed
				row["has_samples"] = total > 0
				if total > 0 {
					row["error_rate"] = float64(failed) / float64(total)
				}
			}
		}
		result[d.DeviceID] = row
	}
	return OK(c, result)
}

// handleUpdateWritePolicy updates device write protection policy.
func handleUpdateWritePolicy(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req models.DeviceWritePolicyUpdate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()

	// Check ownership
	existing, _ := cont.DeviceService.Get(deviceID)
	if existing == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	// Merge write policy fields into config
	config := existing.Config
	if config == nil {
		config = map[string]interface{}{}
	}
	if req.WriteVerify != nil {
		config["write_verify"] = *req.WriteVerify
	}
	if req.WriteRateLimit != nil {
		config["write_rate_limit"] = *req.WriteRateLimit
	}
	if req.WriteAudit != nil {
		config["write_audit"] = *req.WriteAudit
	}
	if req.WriteWhitelist != nil {
		config["write_whitelist"] = *req.WriteWhitelist
	}

	updateData := &models.DeviceUpdate{
		Config: config,
	}

	device, err := cont.DeviceService.Update(deviceID, updateData)
	if err != nil {
		logrus.WithError(err).Error("Update write policy failed")
		return InternalError(c, "ERR_DEVICE_UPDATE_FAILED")
	}

	return OK(c, device)
}

// handleGetDeviceOps reports what the gateway measured about a device's link:
// the driver's own counters plus the collector's last error. It used to answer
// online_rate 100.0 with every counter zeroed, so a device that had never
// connected looked healthier than one that was working.
func handleGetDeviceOps(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	resp := map[string]interface{}{
		"device_id":    device.DeviceID,
		"protocol":     device.Protocol,
		"state":        device.Status,
		"is_connected": device.Status == "online",
		"last_error":   "",
		// The measured metrics are null until they are measured: a 0 here would
		// read as "no latency at all" and a 100 (the score's initial value) as a
		// perfect link on a device nobody has polled.
		"online_rate":              nil,
		"avg_latency_ms":           nil,
		"p95_latency_ms":           nil,
		"latency_history":          []float64{},
		"total_reads":              int64(0),
		"failed_reads":             int64(0),
		"total_writes":             int64(0),
		"failed_writes":            int64(0),
		"total_reconnects":         int64(0),
		"consecutive_failures":     int64(0),
		"connection_quality_score": nil,
		"has_samples":              false,
	}

	if cont.Scheduler != nil {
		if status, ok := cont.Scheduler.GetDeviceStatus(deviceID); ok {
			if v, ok := status["last_error"].(string); ok && v != "" {
				resp["last_error"] = v
			}
		}
	}

	if mgr := drivers.GetHealthStatsManager(); mgr != nil {
		if st := mgr.GetHealthStats(deviceID); st != nil {
			ctr := st.Counters()
			total := ctr.TotalReads + ctr.TotalWrites
			failed := ctr.FailedReads + ctr.FailedWrites
			resp["has_samples"] = total > 0
			// Success rate of the operations actually attempted, not uptime: with
			// no operations yet online_rate stays null instead of claiming 0% or 100%.
			if total > 0 {
				resp["online_rate"] = float64(total-failed) / float64(total) * 100.0
				resp["connection_quality_score"] = ctr.ConnectionQualityScore
			}
			// Latency is gated on latency samples, not on operations: a device whose
			// every read failed has counters but no timing at all.
			if ctr.HasLatencySample {
				resp["latency_history"] = st.LatencySamples()
				resp["avg_latency_ms"] = ctr.AvgLatencyMs
				resp["p95_latency_ms"] = st.P95LatencyMs()
			}
			resp["total_reads"] = ctr.TotalReads
			resp["failed_reads"] = ctr.FailedReads
			resp["total_writes"] = ctr.TotalWrites
			resp["failed_writes"] = ctr.FailedWrites
			resp["total_reconnects"] = ctr.TotalReconnects
			resp["consecutive_failures"] = ctr.ConsecutiveFailures
			if ctr.DegradationReason != "" {
				resp["degradation_reason"] = ctr.DegradationReason
			}
		}
	}

	return OK(c, resp)
}

// handleProbePrimaryLink dials the peer socket the driver connects to and
// reports what happened. It used to answer "reachable" from the device's stored
// status, which reported the result of the last collection cycle as if this
// request had measured the link.
func handleProbePrimaryLink(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	host, port, supported := deviceTCPTestTarget(constants.NormalizeProtocol(device.Protocol), device.Config)
	if !supported {
		return ErrorCode(c, http.StatusNotImplemented, "ERR_DEVICE_PROBE_UNSUPPORTED",
			"protocol "+device.Protocol+" has no peer socket to probe")
	}
	if host == "" {
		return BadRequest(c, "ERR_DEVICE_PROBE_NO_ADDRESS")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), connectionTestTimeout)
	defer cancel()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	started := time.Now()
	dialer := net.Dialer{Timeout: connectionTestTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		logrus.WithError(err).WithField("address", addr).Debug("Primary link probe failed")
		return OK(c, map[string]interface{}{
			"reachable":  false,
			"address":    addr,
			"latency_ms": probeLatencyMs(started),
			"error":      "ERR_DEVICE_PROBE_UNREACHABLE",
		})
	}
	_ = conn.Close()

	return OK(c, map[string]interface{}{
		"reachable":  true,
		"address":    addr,
		"latency_ms": probeLatencyMs(started),
	})
}

// probeLatencyMs reports sub-millisecond precision: a dial inside a LAN or to
// localhost takes tens of microseconds, and whole milliseconds showed every probe
// as latency_ms 0 -- indistinguishable from a measurement that never happened.
func probeLatencyMs(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000.0
}

// handleGetPointHealth reports the per-point state the gateway actually holds:
// the quality and age of the value the collector last produced. It used to
// answer success_rate 100.0 with zero counts for every point, so a point that
// had never been read looked healthy.
func handleGetPointHealth(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	// The cache is a ring of the most recent values gateway-wide, ordered oldest
	// to newest, so walking it forward and overwriting leaves the newest entry
	// per point.
	type lastSeen struct {
		value   interface{}
		quality string
		at      time.Time
	}
	latest := map[string]lastSeen{}
	if cont.Cache != nil {
		for _, pd := range cont.Cache.GetAll() {
			if pd.DeviceID != deviceID {
				continue
			}
			latest[pd.PointName] = lastSeen{value: pd.Value, quality: pd.Quality, at: pd.Timestamp}
		}
	}

	result := make([]map[string]interface{}, 0, len(device.Points))
	for _, p := range device.Points {
		row := map[string]interface{}{
			"point_name": p.Name,
			// "never_collected" is a distinct answer from a good-quality zero.
			"current_quality": "never_collected",
			"last_value":      nil,
			"last_seen_at":    nil,
		}
		if seen, ok := latest[p.Name]; ok {
			row["current_quality"] = seen.quality
			row["last_value"] = seen.value
			if !seen.at.IsZero() {
				row["last_seen_at"] = seen.at.Format(time.RFC3339)
			}
		}
		result = append(result, row)
	}
	return OK(c, result)
}

// handleGetWriteAudit returns write audit logs for a device, newest first. The
// rows come from the same tamper-chained trail as /audit/logs, filtered to this
// device's device_write entries.
func handleGetWriteAudit(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	limit := 100
	if l, err := strconv.Atoi(c.QueryParam("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}

	cont := GetContainer()
	if cont.AuditService == nil {
		// An empty list here would read as "this device has never been written
		// to", which is a different claim from "there is no audit service".
		return ServiceUnavailable(c, "ERR_WRITE_AUDIT_UNAVAILABLE: audit service not ready")
	}
	entries, _ := cont.AuditService.List(services.AuditFilter{
		Action:       auditActionDeviceWrite,
		ResourceType: "device",
		ResourceID:   deviceID,
		Status:       c.QueryParam("result"),
		StartTime:    normalizeRFC3339(c.QueryParam("start_time")),
		EndTime:      normalizeRFC3339(c.QueryParam("end_time")),
	}, 1, limit)

	result := make([]map[string]interface{}, 0, len(entries))
	for _, e := range entries {
		row := map[string]interface{}{
			"id":         e.ID,
			"device_id":  deviceID,
			"timestamp":  e.CreatedAt.Format(time.RFC3339),
			"user_id":    e.UserID,
			"username":   e.Username,
			"ip_address": e.IPAddress,
			"result":     e.Status,
		}
		for k, v := range e.Details {
			row[k] = v
		}
		result = append(result, row)
	}
	return OK(c, result)
}

// --- Device Config Version Management ---

// handleListDeviceConfigVersions returns config versions for a device.
func handleListDeviceConfigVersions(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}
	cont := GetContainer()
	if cont.ConfigVersionMgr == nil {
		return OK(c, []interface{}{})
	}
	versions := cont.ConfigVersionMgr.ListVersions(deviceID, 20, 0)
	result := make([]map[string]interface{}, 0, len(versions))
	for _, v := range versions {
		result = append(result, map[string]interface{}{
			"version":      v.Version,
			"device_id":    v.DeviceID,
			"timestamp":    v.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			"changed_keys": v.ChangedKeys,
			"user":         v.User,
		})
	}
	return OK(c, result)
}

// handleGetDeviceConfigCurrent returns the current config for a device.
func handleGetDeviceConfigCurrent(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	cont := GetContainer()
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	return OK(c, map[string]interface{}{
		"device_id": device.DeviceID,
		"config":    device.Config,
		"version":   device.Version,
	})
}

// handleGetDeviceConfigVersion returns a specific config version.
func handleGetDeviceConfigVersion(c echo.Context) error {
	deviceID := c.Param("device_id")
	versionStr := c.Param("version")
	if deviceID == "" || versionStr == "" {
		return BadRequest(c, "Device ID and version required")
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		return BadRequest(c, "Invalid version number")
	}
	cont := GetContainer()
	if cont.ConfigVersionMgr == nil {
		return NotFound(c, "ERR_CONFIG_VERSION_NOT_FOUND")
	}
	snapshot, err := cont.ConfigVersionMgr.GetVersion(deviceID, version)
	if err != nil {
		return NotFound(c, "ERR_CONFIG_VERSION_NOT_FOUND")
	}
	return OK(c, map[string]interface{}{
		"version":      snapshot.Version,
		"device_id":    snapshot.DeviceID,
		"timestamp":    snapshot.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
		"config":       snapshot.Config,
		"changed_keys": snapshot.ChangedKeys,
		"user":         snapshot.User,
	})
}

// handleSaveDeviceConfigVersion saves a new config version.
func handleSaveDeviceConfigVersion(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req struct {
		Config      map[string]interface{} `json:"config"`
		ChangedKeys []string               `json:"changed_keys"`
		Operator    string                 `json:"operator"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont.ConfigVersionMgr == nil {
		return BadRequest(c, "Config versioning not available")
	}
	snapshot := cont.ConfigVersionMgr.SnapshotDeviceConfig(deviceID, req.Config, req.ChangedKeys, req.Operator)
	return OK(c, map[string]interface{}{
		"device_id": snapshot.DeviceID,
		"version":   snapshot.Version,
		"timestamp": snapshot.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// handleRollbackDeviceConfig restores the config a device had at a saved version.
// It used to answer with the version numbers a caller asked for without reading
// or writing anything, so a "rollback" before a maintenance window left the
// device on the config it was supposed to leave behind and reported success.
func handleRollbackDeviceConfig(c echo.Context) error {
	user := getUserFromContext(c)
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	var req struct {
		TargetVersion int    `json:"target_version"`
		Operator      string `json:"operator"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.TargetVersion <= 0 {
		return ErrorCode(c, http.StatusBadRequest, "ERR_CONFIG_VERSION_INVALID", "target_version must be a saved version number")
	}

	cont := GetContainer()
	if cont.ConfigVersionMgr == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_CONFIG_VERSIONING_UNAVAILABLE", "Config versioning is not enabled on this gateway")
	}
	device, _ := cont.DeviceService.Get(deviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && device.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	snapshot, err := cont.ConfigVersionMgr.GetVersion(deviceID, req.TargetVersion)
	if err != nil {
		recordAudit(c, "device_config_rollback", "device", deviceID, "failure", map[string]interface{}{"target_version": req.TargetVersion})
		return NotFound(c, "ERR_CONFIG_VERSION_NOT_FOUND")
	}
	// The saved version can carry a weaker write policy than the one in force
	// now, and restoring it is a policy change even though it goes through the
	// rollback route rather than a device edit.
	if writePolicyChanged(device.Config, snapshot.Config) && !security.HasPermission(user.Role, security.PermDeviceWritePolicyEdit) {
		recordAudit(c, "device_write_policy_denied", "device", deviceID, "failure", nil)
		return ErrorCode(c, http.StatusForbidden, "ERR_WRITE_POLICY_FORBIDDEN",
			"Changing write_verify, write_rate_limit, write_audit or write_whitelist requires the device:write_policy_edit permission")
	}

	// Snapshot what is about to be replaced, so the rollback itself can be rolled
	// back instead of being a one-way door.
	if len(device.Config) > 0 {
		cont.ConfigVersionMgr.SnapshotDeviceConfig(deviceID, device.Config,
			configChangedKeys(device.Config, snapshot.Config), user.Username)
	}

	updated, err := cont.DeviceService.Update(deviceID, &models.DeviceUpdate{Config: snapshot.Config})
	if err != nil {
		logrus.WithField("device_id", deviceID).
			WithError(err).
			Error("Device config rollback failed")
		recordAudit(c, "device_config_rollback", "device", deviceID, "failure", map[string]interface{}{"target_version": req.TargetVersion})
		return InternalError(c, "ERR_DEVICE_UPDATE_FAILED")
	}
	recordAudit(c, "device_config_rollback", "device", deviceID, "success", map[string]interface{}{"target_version": req.TargetVersion})

	return OK(c, map[string]interface{}{
		"device_id":      deviceID,
		"rolled_back_to": snapshot.Version,
		"version":        updated.Version,
		"config":         updated.Config,
	})
}

// configChangedKeys lists the top-level config entries a rollback or an edit
// moves, so a saved version records what changed instead of claiming "config".
func configChangedKeys(old, new map[string]interface{}) []string {
	keys := make([]string, 0, len(old)+len(new))
	for k, ov := range old {
		nv, ok := new[k]
		if !ok || fmt.Sprintf("%v", ov) != fmt.Sprintf("%v", nv) {
			keys = append(keys, k)
		}
	}
	for k := range new {
		if _, ok := old[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// handleGetDeviceConfigAuditTrail returns the recorded audit entries for one
// device. Every change to it — create, update, point write, rollback — lands in
// the audit log, so the trail reads from there instead of being reported empty.
func handleGetDeviceConfigAuditTrail(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}
	cont := GetContainer()
	if cont.AuditService == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_AUDIT_UNAVAILABLE", "Audit service not ready")
	}
	page, size := parsePagination(c)
	entries, total := cont.AuditService.List(services.AuditFilter{
		ResourceType: "device",
		ResourceID:   deviceID,
	}, page, size)
	return OKPaged(c, entries, total, page, size)
}

// handleDiffDeviceConfigVersions returns diff between two config versions.
func handleDiffDeviceConfigVersions(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}

	versionA, errA := strconv.Atoi(c.QueryParam("version_a"))
	versionB, errB := strconv.Atoi(c.QueryParam("version_b"))
	if errA != nil || errB != nil {
		return ErrorCode(c, http.StatusBadRequest, "ERR_CONFIG_VERSION_INVALID", "version_a and version_b must be saved version numbers")
	}

	cont := GetContainer()
	if cont.ConfigVersionMgr == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_CONFIG_VERSIONING_UNAVAILABLE", "Config versioning is not enabled on this gateway")
	}
	diff, err := cont.ConfigVersionMgr.DiffVersions(deviceID, versionA, versionB)
	if err != nil {
		return NotFound(c, "ERR_CONFIG_VERSION_NOT_FOUND")
	}
	diff["device_id"] = deviceID
	return OK(c, diff)
}

// Suppress unused import error
var _ = http.StatusOK

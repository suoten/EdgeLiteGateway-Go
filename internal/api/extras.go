package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/constants"
	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/northbound"
	"edgelite/internal/platform"
	"edgelite/internal/security"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

// RegisterNotifyRoutes registers notification configuration API routes.
func RegisterNotifyRoutes(g *echo.Group) {
	g.GET("/config", handleGetNotifyConfig, requirePermission(security.PermNotifyConfig))
	g.PUT("/config", handleUpdateNotifyConfig, requirePermission(security.PermNotifyConfig))
	g.POST("/test", handleSendTestNotification, requirePermission(security.PermNotifyConfig))
	g.GET("/channels", handleListNotifyChannels, requirePermission(security.PermNotifyConfig))
	g.GET("/history", handleListNotifyHistory, requirePermission(security.PermNotifyConfig))
}

func handleGetNotifyConfig(c echo.Context) error {
	// Security: return sanitized config to avoid leaking sensitive fields
	return OK(c, config.GetSanitizedConfig())
}

func handleUpdateNotifyConfig(c echo.Context) error {
	// Read the body as a map first: the page round-trips the masked values it got
	// from GET /config, and binding straight into NotifyConfig would store
	// "a***z" as the real secret.
	var body map[string]interface{}
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil || body == nil {
		return BadRequest(c, "Invalid request body")
	}
	config.RestoreMaskedSecrets(map[string]interface{}{"notify": body})
	raw, err := json.Marshal(body)
	if err != nil {
		return BadRequest(c, "Invalid request body")
	}
	var req config.NotifyConfig
	if err := json.Unmarshal(raw, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cfg := config.GetConfig()
	cfg.Notify = req
	if err := config.SaveConfig(cfg, ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	return OK(c, map[string]string{"message": "Notification config updated"})
}

// notifyService returns the container's NotifyService, falling back to an
// on-demand one when the service layer was never wired, so a partially
// initialised container cannot leave the "test channel" button answering a
// bare 503 with no way to diagnose the setup.
func notifyService() *services.NotifyService {
	if cont := GetContainer(); cont != nil && cont.NotifyService != nil {
		return cont.NotifyService
	}
	if v := unsetNotifyService.Load(); v != nil {
		return v.(*services.NotifyService)
	}
	svc := services.NewNotifyService()
	unsetNotifyService.Store(svc)
	return svc
}

var unsetNotifyService atomic.Value

// configuredNotifyChannels lists the channels that could actually deliver.
func configuredNotifyChannels() []string {
	notify := &config.GetConfig().Notify
	var out []string
	for _, ch := range []string{"dingtalk", "email", "webhook", "wecom"} {
		if services.ChannelConfigured(notify, ch) {
			out = append(out, ch)
		}
	}
	return out
}

func handleSendTestNotification(c echo.Context) error {
	type TestRequest struct {
		Channel string `json:"channel"`
		Message string `json:"message"`
	}
	var req TestRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Message == "" {
		req.Message = "Test notification from EdgeLite Gateway"
	}

	channels := []string{req.Channel}
	if req.Channel == "" {
		channels = configuredNotifyChannels()
	}
	if len(channels) == 0 || !notifyChannelsConfigured(channels) {
		return OK(c, map[string]interface{}{
			"channel": req.Channel,
			"success": false,
			"message": "ERR_NOTIFY_CHANNEL_NOT_CONFIGURED",
		})
	}
	if err := notifyService().SendNotification(channels, "Test Notification", req.Message, "info"); err != nil {
		return OK(c, map[string]interface{}{
			"channel":  req.Channel,
			"channels": channels,
			"success":  false,
			"message":  err.Error(),
		})
	}

	return OK(c, map[string]interface{}{
		"channel":  req.Channel,
		"channels": channels,
		"success":  true,
		"message":  "Test notification sent",
	})
}

// notifyChannelsConfigured reports whether every named channel can deliver.
func notifyChannelsConfigured(channels []string) bool {
	notify := &config.GetConfig().Notify
	for _, ch := range channels {
		if !services.ChannelConfigured(notify, ch) {
			return false
		}
	}
	return true
}

// handleListNotifyChannels reports each channel together with its stored
// settings, so the page can show real status and pre-fill the forms instead of
// an empty one. Credentials are masked exactly as GET /config masks them.
func handleListNotifyChannels(c echo.Context) error {
	sanitized, _ := config.GetSanitizedConfig()["notify"].(map[string]interface{})
	notify := &config.GetConfig().Notify

	channels := []map[string]interface{}{}
	for _, ch := range []struct{ uiType, section string }{
		{"dingtalk", "dingtalk"},
		{"wecom", "wechat"},
		{"email", "email"},
		{"webhook", "webhook"},
	} {
		cfg, _ := sanitized[ch.section].(map[string]interface{})
		if cfg == nil {
			cfg = map[string]interface{}{}
		}
		configured := notifyChannelConfigured(notify, ch.uiType)
		name, _ := cfg["name"].(string)
		if name == "" {
			name = ch.uiType
		}
		status := "not_configured"
		if configured {
			status = "configured"
		}
		channels = append(channels, map[string]interface{}{
			"id":        ch.uiType,
			"type":      ch.uiType,
			"name":      name,
			"enabled":   configured,
			"status":    status,
			"last_test": nil,
			"config":    applyNotifyKeys(ch.uiType, cfg, false),
		})
	}
	return OK(c, map[string]interface{}{"channels": channels})
}

func handleListNotifyHistory(c echo.Context) error {
	return OK(c, []interface{}{})
}

// --- Video API ---

// RegisterVideoRoutes registers video management API routes.
func RegisterVideoRoutes(g *echo.Group) {
	g.GET("/devices", handleListVideoDevices, requirePermission(security.PermVideoRead))
	g.GET("/devices/:device_id/streams", handleGetVideoStreams, requirePermission(security.PermVideoRead))
	g.POST("/devices/:device_id/ptz", handleControlPTZ, requirePermission(security.PermVideoRead))
	g.GET("/config", handleGetVideoConfig, requirePermission(security.PermVideoRead))
}

// videoProtocols are the driver protocols that stand for a camera rather than a
// field sensor. A point type may also carry them in a mixed device.
var videoProtocols = map[string]bool{
	"onvif": true, "rtsp": true, "gb28181": true,
}

// isVideoDevice reports whether a device should show up under /video/devices.
// The ONVIF driver carries its point type in the point address (video_status,
// ptz_status), so a mixed device that polls camera data counts too.
func isVideoDevice(d models.DeviceResponse) bool {
	if videoProtocols[strings.ToLower(d.Protocol)] {
		return true
	}
	for _, p := range d.Points {
		key := strings.ToLower(p.Address + "." + p.Name)
		if strings.Contains(key, "video") || strings.Contains(key, "ptz") || strings.Contains(key, "camera") {
			return true
		}
	}
	return false
}

// handleListVideoDevices answers from the device registry. It used to return a
// literal empty list, so a gateway with cameras configured showed "no devices"
// and the page could not tell an empty installation apart from a broken listing.
func handleListVideoDevices(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceRepo == nil {
		return ServiceUnavailable(c, "Device repository not ready")
	}
	devs, _, err := cont.DeviceRepo.List(1, 1000)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_DEVICE_LIST_FAILED", "Cannot read the device registry: "+err.Error())
	}
	out := make([]map[string]interface{}, 0, len(devs))
	for _, d := range devs {
		if !isVideoDevice(d) {
			continue
		}
		host, _ := d.Config["host"].(string)
		streaming := false
		if cont.VideoService != nil {
			if st := cont.VideoService.GetStream(d.DeviceID); st != nil && st.Status == "started" {
				streaming = true
			}
		}
		out = append(out, map[string]interface{}{
			"device_id": d.DeviceID,
			"name":      d.Name,
			"protocol":  d.Protocol,
			"status":    d.Status,
			"host":      host,
			"port":      d.Config["port"],
			// Nothing in this build can start a stream: VideoService.StartStream has
			// no caller, so this is false for every device, and it has to be said.
			"streaming": streaming,
		})
	}
	return OK(c, out)
}

// handleGetVideoStreams reports the streams actually running for one camera. An
// empty list means "no stream is playing", not "this camera has no streams", so
// the response carries streaming_implemented to make that distinction.
func handleGetVideoStreams(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}
	cont := GetContainer()
	if cont.DeviceRepo == nil {
		return ServiceUnavailable(c, "Device repository not ready")
	}
	dev, err := cont.DeviceRepo.Get(deviceID)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_DEVICE_READ_FAILED", "Cannot read the device registry: "+err.Error())
	}
	// A missing row comes back as (nil, nil) from this repository, so the nil
	// check is the 404 and the error is a real storage failure.
	if dev == nil {
		return NotFound(c, "Device not found: "+deviceID)
	}
	streams := make([]map[string]interface{}, 0, 1)
	if cont.VideoService != nil {
		if st := cont.VideoService.GetStream(deviceID); st != nil {
			streams = append(streams, map[string]interface{}{
				"url":        st.URL,
				"protocol":   st.Protocol,
				"status":     st.Status,
				"started_at": st.StartedAt.Format(time.RFC3339),
			})
		}
	}
	return OK(c, map[string]interface{}{
		"device_id":             deviceID,
		"protocol":              dev.Protocol,
		"streams":               streams,
		"streaming_implemented": false,
		"message":               "No media pipeline is wired in this build: the gateway lists cameras and reads their status, but it neither opens nor proxies a stream.",
	})
}

// ptzCommands are the actions the video API accepts. The list is checked before
// anything reaches a device so a typo cannot be reported as a successful move.
var ptzCommands = map[string]bool{
	"up": true, "down": true, "left": true, "right": true,
	"zoom_in": true, "zoom_out": true, "stop": true,
}

// handleControlPTZ sends the command through the same write path as a point
// write, so the device whitelist, rate limit, read-only check and audit apply,
// and the operator sees whatever the driver actually answers. It used to log
// "PTZ control requested" and return status ok without touching a driver, so the
// UI reported a camera moving that had received nothing. Every camera driver in
// this build is read-only (see drivers.ErrONVIFWriteUnsupported), which now
// surfaces as an error instead of a success.
func handleControlPTZ(c echo.Context) error {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		return BadRequest(c, "Device ID required")
	}
	// The action has reached us as a query param (the shipped client) and as a
	// JSON body (every other write endpoint); accept both, never ignore one.
	var req struct {
		Action string `json:"action"`
		Speed  int    `json:"speed"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if q := c.QueryParam("action"); q != "" {
		req.Action = q
	}
	if q := c.QueryParam("speed"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			req.Speed = n
		}
	}
	if !ptzCommands[req.Action] {
		return BadRequest(c, "Unknown PTZ action, expected one of up/down/left/right/zoom_in/zoom_out/stop")
	}
	// A movement with no speed would be written as 0, which most cameras execute as
	// "do not move" - the same class of lie as the old handler, just further down.
	// stop carries no speed by definition.
	if req.Action != "stop" && (req.Speed < 1 || req.Speed > 100) {
		return BadRequest(c, "speed must be between 1 and 100 for movement actions")
	}
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	value := map[string]interface{}{"action": req.Action, "speed": req.Speed}
	err := cont.DeviceService.WritePoint(writeActorContext(c), deviceID, &models.WritePointRequest{
		Point: "ptz",
		Value: value,
	})
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"device_id": deviceID,
			"action":    req.Action,
		}).Warn("PTZ command failed")
		recordDeviceWrite(c, deviceID, "ptz", value, "failed", err)
		switch {
		case errors.Is(err, services.ErrReadOnlyPoint):
			return BadRequest(c, "This camera exposes PTZ as a read-only point, so it cannot be commanded")
		case errors.Is(err, services.ErrWriteNotAllowed):
			return Forbidden(c, "PTZ writes are not on this device's write whitelist")
		case errors.Is(err, services.ErrWriteRateLimited):
			return ErrorCode(c, http.StatusTooManyRequests, "ERR_WRITE_RATE_LIMITED", err.Error())
		}
		return ErrorCode(c, http.StatusBadGateway, "ERR_PTZ_WRITE_FAILED", "PTZ command failed: "+err.Error())
	}
	recordDeviceWrite(c, deviceID, "ptz", value, "success", nil)
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"action":    req.Action,
		"speed":     req.Speed,
		"status":    "sent",
	})
}

func handleGetVideoConfig(c echo.Context) error {
	cfg := config.GetConfig()
	return OK(c, cfg.Video)
}

// --- Drivers API ---

// RegisterDriverRoutes registers driver management API routes.
// GET /drivers/list is served by RegisterDriverExtendedRoutes; it is not
// repeated here because Echo lets the last registration win, which made the
// two handlers' payloads interchangeable-but-different depending on order.
func RegisterDriverRoutes(g *echo.Group) {
	g.GET("", handleListDrivers, requirePermission(security.PermDriverConfig))
	g.GET("/protocols", handleListProtocols, requirePermission(security.PermDriverConfig))
	g.POST("/reload", handleReloadDrivers, requirePermission(security.PermDriverConfig))
	g.GET("/:protocol/info", handleGetDriverInfo, requirePermission(security.PermDriverConfig))
}

// registeredDrivers lists the protocols the driver registry can actually
// instantiate, in stable order, annotated with the display names
// drivers.RegisterAll is paired with. Every driver endpoint derives from this
// so the API cannot advertise a driver the gateway does not have.
func registeredDrivers() []string {
	names := drivers.GetRegistry().SupportedProtocols()
	sort.Strings(names)
	return names
}

func handleListDrivers(c echo.Context) error {
	names := registeredDrivers()
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]interface{}{
			"name":        name,
			"description": drivers.GetDriverDisplayName(name, "en"),
			"protocols":   []string{name},
			"enabled":     true,
		})
	}
	return OK(c, map[string]interface{}{"drivers": out, "total": len(out)})
}

// handleListProtocols reports every protocol key a device creation request may
// name: the canonical driver names plus their aliases. The Python edition's
// get_all_protocol_keys() included aliases, and ProtoForge's integration client
// filters its protocol map against this exact list before pushing — omitting
// the aliases made it skip protocols the gateway actually accepts (e.g. opcua).
func handleListProtocols(c echo.Context) error {
	protocols := registeredDrivers()
	seen := make(map[string]bool, len(protocols)+len(constants.ProtocolAliases))
	for _, name := range protocols {
		seen[name] = true
	}
	for alias := range constants.ProtocolAliases {
		if !seen[alias] {
			protocols = append(protocols, alias)
			seen[alias] = true
		}
	}
	sort.Strings(protocols)
	return OK(c, map[string]interface{}{"protocols": protocols})
}

// handleReloadDrivers used to answer "Drivers reload initiated" without doing
// anything. Driver factories are compiled in and registered once at startup
// (cmd/edgelite calls drivers.RegisterAll), and the dynamic plugin manager is
// not wired to any driver source, so there is nothing to reload.
func handleReloadDrivers(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_DRIVERS_RELOAD_UNSUPPORTED",
		"ERR_DRIVERS_RELOAD_UNSUPPORTED")
}

func handleGetDriverInfo(c echo.Context) error {
	protocol := c.Param("protocol")
	if !drivers.GetRegistry().IsSupported(protocol) {
		return NotFound(c, "ERR_DRIVER_NOT_FOUND")
	}
	return OK(c, map[string]interface{}{
		"protocol":    protocol,
		"description": drivers.GetDriverDisplayName(protocol, "en"),
		"enabled":     true,
	})
}

// --- Platforms API ---

// RegisterPlatformRoutes registers northbound platform API routes.
func RegisterPlatformRoutes(g *echo.Group) {
	g.GET("", handleListPlatforms, requirePermission(security.PermPlatformManage))
	g.POST("", handleCreatePlatform, requirePermission(security.PermPlatformManage))
	g.GET("/:name", handleGetPlatform, requirePermission(security.PermPlatformManage))
	g.PUT("/:name", handleUpdatePlatform, requirePermission(security.PermPlatformManage))
	g.DELETE("/:name", handleDeletePlatform, requirePermission(security.PermPlatformManage))
	g.POST("/:name/test", handleTestPlatform, requirePermission(security.PermPlatformManage))
	g.GET("/:name/status", handleGetPlatformStatus, requirePermission(security.PermPlatformManage))
}

// getPlatformManager returns the northbound platform manager (may be nil
// before the engine has started).
func getPlatformManager() *northbound.Manager {
	return GetContainer().PlatformMgr
}

// platformConnectionState returns the runtime connection state label for a
// configured platform: "connected" / "disconnected" / "unknown" (disabled).
func platformConnectionState(name string, enabled bool) (string, bool) {
	mgr := getPlatformManager()
	if mgr != nil && mgr.IsConnected(name) {
		return "connected", true
	}
	if enabled {
		return "disconnected", false
	}
	return "unknown", false
}

// buildPlatformListEntries builds the platform list from the persisted config
// with live connection state from the manager.
func buildPlatformListEntries() []map[string]interface{} {
	cfg := config.GetConfig()
	names := make([]string, 0, len(cfg.Platforms))
	for name := range cfg.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)

	platforms := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		entry := map[string]interface{}{
			"name":    name,
			"version": "1.0",
		}
		enabled := false
		if m, ok := cfg.Platforms[name].(map[string]interface{}); ok {
			entry["config"] = platform.MaskConfig(m)
			if t, ok := m["type"].(string); ok && t != "" {
				entry["type"] = t
			}
			if b, ok := m["enabled"].(bool); ok {
				enabled = b
				entry["enabled"] = b
			}
			if l, ok := m["label"].(string); ok && l != "" {
				entry["label"] = l
			}
		}
		state, connected := platformConnectionState(name, enabled)
		entry["state"] = state
		entry["connected"] = connected
		platforms = append(platforms, entry)
	}
	return platforms
}

// persistPlatformConfig saves cfg.Platforms mutations to the config file.
func persistPlatformConfig() error {
	return config.SaveConfig(config.GetConfig(), "")
}

// mergePlatformConfig merges incoming over base while preserving existing
// values for secret fields whose incoming value is the export mask "***".
// The frontend edit modal and export/import round-trips carry masked secrets,
// and writing the mask back would destroy the real credential.
func mergePlatformConfig(base, incoming map[string]interface{}) map[string]interface{} {
	merged := make(map[string]interface{}, len(base)+len(incoming))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok && s == "***" && platform.IsSecretField(k) {
			if bv, exists := base[k]; exists && bv != nil {
				merged[k] = bv // keep the existing real secret
			} else {
				delete(merged, k) // mask without a real value — drop the key
			}
			continue
		}
		merged[k] = v
	}
	return merged
}

// resolvePlatformType determines the platform handler type for an instance:
// an explicit valid type in the incoming config wins, then the persisted type
// (for custom-named instances), otherwise the instance name itself.
func resolvePlatformType(name string, incoming map[string]interface{}) string {
	if incoming != nil {
		if t, ok := incoming["type"].(string); ok && t != "" {
			if _, known := platform.GetPlatformMeta(t); known {
				return t
			}
		}
	}
	if stored, ok := config.GetConfig().Platforms[name].(map[string]interface{}); ok {
		if t, ok := stored["type"].(string); ok && t != "" {
			if _, known := platform.GetPlatformMeta(t); known {
				return t
			}
		}
	}
	return name
}

func handleListPlatforms(c echo.Context) error {
	return OK(c, buildPlatformListEntries())
}

func handleCreatePlatform(c echo.Context) error {
	type PlatformCreate struct {
		Name    string                 `json:"name"`
		Type    string                 `json:"type"`
		Enabled bool                   `json:"enabled"`
		Config  map[string]interface{} `json:"config"`
	}
	var req PlatformCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Name == "" {
		return BadRequest(c, "Platform name required")
	}
	typeName := req.Type
	if typeName == "" {
		typeName = req.Name
	}
	if _, ok := platform.GetPlatformMeta(typeName); !ok {
		return BadRequest(c, "Unknown platform type: "+typeName)
	}

	stored := make(map[string]interface{}, len(req.Config)+2)
	for k, v := range req.Config {
		stored[k] = v
	}
	stored["type"] = typeName
	stored["enabled"] = req.Enabled

	cfg := config.GetConfig()
	if _, exists := cfg.Platforms[req.Name]; exists {
		return ErrorCode(c, http.StatusConflict, "ERR_PLATFORM_EXISTS", "Platform already exists: "+req.Name)
	}
	cfg.Platforms[req.Name] = stored
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", req.Name).Warn("Failed to persist platform config")
	}

	if req.Enabled {
		if mgr := getPlatformManager(); mgr != nil {
			if err := mgr.Connect(req.Name, stored); err != nil {
				logrus.WithError(err).WithField("platform", req.Name).Warn("Platform auto-connect after create failed")
			}
		}
	}

	logrus.WithField("platform", req.Name).Info("Platform created")
	return Created(c, req)
}

func handleGetPlatform(c echo.Context) error {
	name := c.Param("name")
	cfg := config.GetConfig()
	p, ok := cfg.Platforms[name]
	if !ok {
		return NotFound(c, "Platform not found")
	}
	entry := map[string]interface{}{
		"name":   name,
		"config": p,
	}
	enabled := false
	if m, ok := p.(map[string]interface{}); ok {
		if b, ok := m["enabled"].(bool); ok {
			enabled = b
		}
	}
	state, connected := platformConnectionState(name, enabled)
	entry["state"] = state
	entry["connected"] = connected
	return OK(c, entry)
}

func handleUpdatePlatform(c echo.Context) error {
	name := c.Param("name")
	var req struct {
		Enabled *bool                  `json:"enabled"`
		Config  map[string]interface{} `json:"config"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cfg := config.GetConfig()
	existing, ok := cfg.Platforms[name].(map[string]interface{})
	if !ok {
		return NotFound(c, "Platform not found")
	}
	if req.Config != nil {
		merged := mergePlatformConfig(existing, req.Config)
		cfg.Platforms[name] = merged
		existing = merged
	}
	if req.Enabled != nil {
		existing["enabled"] = *req.Enabled
		if !*req.Enabled {
			// Disabling a platform also tears down its runtime connection so
			// the manager watchdog does not keep reconnecting it.
			if mgr := getPlatformManager(); mgr != nil {
				_ = mgr.Disconnect(name)
			}
		}
	}
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", name).Warn("Failed to persist platform config")
	}

	// Reconnect with the new configuration if currently connected and still
	// enabled (explicit disable already tore the connection down above).
	disabled := req.Enabled != nil && !*req.Enabled
	mgr := getPlatformManager()
	if mgr != nil && !disabled && mgr.IsConnected(name) {
		if err := mgr.Connect(name, existing); err != nil {
			logrus.WithError(err).WithField("platform", name).Warn("Platform reconnect after update failed")
		}
	}
	logrus.WithField("platform", name).Info("Platform updated")
	return OK(c, map[string]string{"message": "Platform updated", "name": name})
}

func handleDeletePlatform(c echo.Context) error {
	name := c.Param("name")
	if mgr := getPlatformManager(); mgr != nil {
		_ = mgr.Disconnect(name)
	}
	cfg := config.GetConfig()
	delete(cfg.Platforms, name)
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", name).Warn("Failed to persist platform config")
	}
	logrus.WithField("platform", name).Info("Platform deleted")
	return OK(c, nil)
}

func handleTestPlatform(c echo.Context) error {
	name := c.Param("name")
	var req struct {
		Config map[string]interface{} `json:"config"`
	}
	_ = c.Bind(&req)
	return OK(c, runPlatformTest(name, req.Config))
}

// runPlatformTest dials a platform with the throwaway handler the manager
// builds, falling back to the stored config when the caller sends none. It
// answers with what the dial did — measured latency, or the real error — and is
// shared with POST /integration/test so the two cannot drift apart.
func runPlatformTest(name string, incoming map[string]interface{}) map[string]interface{} {
	cfg := incoming
	if cfg == nil {
		if stored, ok := config.GetConfig().Platforms[name].(map[string]interface{}); ok {
			cfg = stored
		}
	}
	if cfg == nil {
		cfg = map[string]interface{}{"type": name}
	}
	cfg["type"] = resolvePlatformType(name, cfg)
	mgr := getPlatformManager()
	if mgr == nil {
		return map[string]interface{}{"name": name, "success": false, "message": "Platform manager not ready"}
	}
	result, err := mgr.TestConnection(name, cfg)
	if err != nil {
		return map[string]interface{}{"name": name, "success": false, "message": err.Error()}
	}
	result["name"] = name
	return result
}

func handleGetPlatformStatus(c echo.Context) error {
	name := c.Param("name")
	state, connected := platformConnectionState(name, true)
	var stats northbound.PlatformStats
	if mgr := getPlatformManager(); mgr != nil {
		stats = mgr.GetStats(name)
	}
	lastActive := ""
	if !stats.LastActive.IsZero() {
		lastActive = stats.LastActive.Format(time.RFC3339)
	}
	return OK(c, map[string]interface{}{
		"name":            name,
		"status":          state,
		"connected":       connected,
		"last_active":     lastActive,
		"messages_sent":   stats.MessagesSent,
		"messages_failed": stats.MessagesFailed,
	})
}

// --- WebSocket API ---

// RegisterWSRoute registers the WebSocket endpoint.
func RegisterWSRoute(g *echo.Group) {
	g.GET("/ws", handleWebSocket)
}

func handleWebSocket(c echo.Context) error {
	cont := GetContainer()
	if cont.WSManager == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_WS_NOT_READY", "WebSocket manager not ready")
	}
	cont.WSManager.HandleWS(c.Response(), c.Request())
	return nil
}

// --- Audit API ---

// RegisterAuditRoutes registers audit log API routes.
func RegisterAuditRoutes(g *echo.Group) {
	g.GET("", handleListAuditLogs, requirePermission(security.PermAuditRead))
	g.GET("/logs", handleListAuditLogs, requirePermission(security.PermAuditRead))
}

// handleListAuditLogs 返回前端期望的 {logs, total, page, size} 信封（非分页数组），
// 支持 user_id（匹配 user_id 或 username）、action、resource_type、start_time/end_time 过滤。
func handleListAuditLogs(c echo.Context) error {
	cont := GetContainer()
	if cont.AuditService == nil {
		return OK(c, map[string]interface{}{"logs": []interface{}{}, "total": 0, "page": 1, "size": 20})
	}
	page, size := parsePagination(c)
	filter := services.AuditFilter{
		UserID:       c.QueryParam("user_id"),
		Action:       c.QueryParam("action"),
		ResourceType: c.QueryParam("resource_type"),
		StartTime:    normalizeRFC3339(c.QueryParam("start_time")),
		EndTime:      normalizeRFC3339(c.QueryParam("end_time")),
	}
	entries, total := cont.AuditService.List(filter, page, size)
	return OK(c, map[string]interface{}{"logs": entries, "total": total, "page": page, "size": size})
}

// handleExportAuditLogs 导出审计 CSV。前端经 /audit/export/csv 调用并期望
// 标准信封 {content}；支持与列表一致的过滤参数。
func handleExportAuditLogs(c echo.Context) error {
	cont := GetContainer()
	filter := services.AuditFilter{
		UserID:    c.QueryParam("user_id"),
		Action:    c.QueryParam("action"),
		StartTime: normalizeRFC3339(c.QueryParam("start_time")),
		EndTime:   normalizeRFC3339(c.QueryParam("end_time")),
	}
	var sb strings.Builder
	sb.WriteString("log_id,created_at,user_id,username,action,resource_type,resource_id,ip_address,status,details\n")
	if cont.AuditService != nil {
		entries, _ := cont.AuditService.List(filter, 1, 50000)
		for _, e := range entries {
			detailsJSON, _ := json.Marshal(e.Details)
			sb.WriteString(strings.Join([]string{
				csvEscape(e.ID),
				csvEscape(e.CreatedAt.Format(time.RFC3339)),
				csvEscape(e.UserID),
				csvEscape(e.Username),
				csvEscape(e.Action),
				csvEscape(e.ResourceType),
				csvEscape(e.ResourceID),
				csvEscape(e.IPAddress),
				csvEscape(e.Status),
				csvEscape(string(detailsJSON)),
			}, ",") + "\n")
		}
	}
	return OK(c, map[string]interface{}{"content": sb.String()})
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// --- SCADA API ---

// RegisterSCADARoutes registers SCADA screen API routes.
func RegisterSCADARoutes(g *echo.Group) {
	g.GET("/screens", handleListSCADAScreens, requirePermission(security.PermSCADAEdit))
	g.POST("/screens", handleCreateSCADAScreen, requirePermission(security.PermSCADAEdit))
	g.GET("/screens/:id", handleGetSCADAScreen, requirePermission(security.PermSCADAEdit))
	g.PUT("/screens/:id", handleUpdateSCADAScreen, requirePermission(security.PermSCADAEdit))
	g.DELETE("/screens/:id", handleDeleteSCADAScreen, requirePermission(security.PermSCADAEdit))
}

func handleListSCADAScreens(c echo.Context) error {
	return OK(c, []interface{}{})
}

func handleCreateSCADAScreen(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return Created(c, req)
}

func handleGetSCADAScreen(c echo.Context) error {
	id := c.Param("id")
	return OK(c, map[string]interface{}{
		"id":         id,
		"name":       "Screen " + id,
		"components": []interface{}{},
	})
}

func handleUpdateSCADAScreen(c echo.Context) error {
	id := c.Param("id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"id": id, "updated": true})
}

func handleDeleteSCADAScreen(c echo.Context) error {
	id := c.Param("id")
	logrus.WithField("screen_id", id).Info("SCADA screen deleted")
	return OK(c, nil)
}

// --- Shadow API ---

// shadowJSON converts a DeviceShadow into the frontend ShadowDetail shape
// (DeviceShadow has no json tags and its Go field names don't match).
func shadowJSON(shadow *services.DeviceShadow, delta map[string]interface{}) map[string]interface{} {
	if shadow == nil {
		return nil
	}
	reported := shadow.ReportedState
	if reported == nil {
		reported = map[string]interface{}{}
	}
	desired := shadow.DesiredState
	if desired == nil {
		desired = map[string]interface{}{}
	}
	if delta == nil {
		delta = map[string]interface{}{}
	}
	return map[string]interface{}{
		"device_id":    shadow.DeviceID,
		"reported":     reported,
		"desired":      desired,
		"metadata":     map[string]interface{}{},
		"version":      shadow.Version,
		"last_updated": shadow.LastUpdated.UnixMilli(),
		"delta":        delta,
	}
}

// RegisterShadowRoutes registers device shadow API routes.
func RegisterShadowRoutes(g *echo.Group) {
	g.GET("/:device_id", handleGetShadow, requirePermission(security.PermDeviceRead))
	g.PUT("/:device_id/desired", handleUpdateShadowDesired, requirePermission(security.PermDeviceUpdate))
	g.DELETE("/:device_id/desired", handleClearShadowDesired, requirePermission(security.PermDeviceUpdate))
}

func handleGetShadow(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont.ShadowService != nil {
		if shadow := cont.ShadowService.GetShadow(deviceID); shadow != nil {
			return OK(c, shadowJSON(shadow, cont.ShadowService.GetDelta(deviceID)))
		}
	}
	return OK(c, map[string]interface{}{
		"device_id":    deviceID,
		"reported":     map[string]interface{}{},
		"desired":      map[string]interface{}{},
		"metadata":     map[string]interface{}{},
		"version":      1,
		"last_updated": 0,
		"delta":        map[string]interface{}{},
	})
}

func handleUpdateShadowDesired(c echo.Context) error {
	deviceID := c.Param("device_id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// Frontend sends {desired: {...}}; accept a flat map as well.
	desired, ok := req["desired"].(map[string]interface{})
	if !ok {
		desired = req
	}
	cont := GetContainer()
	if cont.ShadowService != nil {
		cont.ShadowService.UpdateDesired(deviceID, desired)
		if shadow := cont.ShadowService.GetShadow(deviceID); shadow != nil {
			return OK(c, shadowJSON(shadow, cont.ShadowService.GetDelta(deviceID)))
		}
	}
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"desired":   desired,
		"version":   1,
	})
}

func handleClearShadowDesired(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont.ShadowService != nil {
		cont.ShadowService.UpdateDesired(deviceID, map[string]interface{}{})
	}
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"desired":   map[string]interface{}{},
		"message":   "Desired state cleared",
	})
}

// --- Preprocess API ---

// RegisterPreprocessRoutes registers data preprocessing API routes.
func RegisterPreprocessRoutes(g *echo.Group) {
	g.GET("/rules", handleListPreprocessRules, requirePermission(security.PermPreprocessConfig))
	g.POST("/rules", handleCreatePreprocessRule, requirePermission(security.PermPreprocessConfig))
	g.PUT("/rules/:id", handleUpdatePreprocessRule, requirePermission(security.PermPreprocessConfig))
	g.DELETE("/rules/:id", handleDeletePreprocessRule, requirePermission(security.PermPreprocessConfig))
}

func handleListPreprocessRules(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}
	rules, err := cont.PreprocessRules.List()
	if err != nil {
		logrus.WithError(err).Error("Failed to read preprocessing rules")
		return InternalError(c, "ERR_COMMON_READ_FAILED: preprocessing rules could not be read")
	}
	// The device id is optional here because the page that owns these rules edits
	// them per point through PUT /preprocess/config; this listing is the audit
	// view, so it answers with everything unless a device is named.
	deviceID := c.QueryParam("device_id")
	out := make([]models.PreprocessRule, 0, len(rules))
	for _, rule := range rules {
		if deviceID != "" && rule.DeviceID != deviceID {
			continue
		}
		out = append(out, rule)
	}
	return OK(c, out)
}

func handleCreatePreprocessRule(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}
	var rule models.PreprocessRule
	if err := c.Bind(&rule); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// A rule with no device would land in the same bucket the preprocessing page
	// replaces wholesale, so a later save from that page would delete it without
	// ever having shown it. Name the device or use the page.
	if strings.TrimSpace(rule.DeviceID) == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: device_id is required; a gateway-wide per-point filter is set through PUT /preprocess/config")
	}
	if err := validatePreprocessRule(&rule); err != nil {
		return BadRequest(c, err.Error())
	}
	rule.ID = newPreprocessRuleID()
	if err := cont.PreprocessRules.Save(rule); err != nil {
		logrus.WithError(err).Error("Failed to store preprocessing rule")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the preprocessing rule could not be stored")
	}
	if err := applyPreprocessorFromStore(cont); err != nil {
		logrus.WithError(err).Error("Preprocessing rule was stored but could not be applied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the rule could not be applied to the running engine")
	}
	return Created(c, rule)
}

func handleUpdatePreprocessRule(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}
	if id == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: the rule id is required")
	}
	stored, err := cont.PreprocessRules.Get(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return NotFound(c, "ERR_COMMON_NOT_FOUND: no preprocessing rule is stored under "+id)
		}
		return InternalError(c, "ERR_COMMON_READ_FAILED: "+err.Error())
	}
	var raw map[string]interface{}
	if err := c.Bind(&raw); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	// Overlay only the keys the body carries onto the stored rule, so a page that
	// edits one rule's window cannot move it to another point or switch a
	// disabled rule back on by leaving the field out. Echo reads the body once,
	// which is also why this does not bind the struct as well.
	updated := stored
	if value, ok := raw["point_name"].(string); ok {
		updated.PointName = value
	}
	if value, ok := raw["operation"].(string); ok {
		updated.Operation = value
	}
	if value, ok := raw["params"].(map[string]interface{}); ok {
		updated.Params = value
	}
	if value, ok := raw["enabled"].(bool); ok {
		updated.Enabled = value
	}
	if value, ok := raw["device_id"].(string); ok && strings.TrimSpace(value) != stored.DeviceID {
		return BadRequest(c, "ERR_COMMON_VALIDATION: device_id cannot be changed by an update; create the rule for the other device and delete this one")
	}
	if err := validatePreprocessRule(&updated); err != nil {
		return BadRequest(c, err.Error())
	}
	if err := cont.PreprocessRules.Save(updated); err != nil {
		logrus.WithError(err).Error("Failed to store preprocessing rule")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the preprocessing rule could not be stored")
	}
	if err := applyPreprocessorFromStore(cont); err != nil {
		logrus.WithError(err).Error("Preprocessing rule was stored but could not be applied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the rule could not be applied to the running engine")
	}
	return OK(c, updated)
}

func handleDeletePreprocessRule(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}
	removed, err := cont.PreprocessRules.Delete(id)
	if err != nil {
		logrus.WithError(err).Error("Failed to delete preprocessing rule")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the preprocessing rule could not be deleted")
	}
	if !removed {
		return NotFound(c, "ERR_COMMON_NOT_FOUND: no preprocessing rule is stored under "+id)
	}
	if err := applyPreprocessorFromStore(cont); err != nil {
		logrus.WithError(err).Error("Preprocessing rule was deleted but could not be reapplied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the running engine could not be updated")
	}
	return OK(c, map[string]interface{}{"id": id, "deleted": true})
}

// --- Expression API ---

// RegisterExpressionRoutes registers expression engine API routes.
func RegisterExpressionRoutes(g *echo.Group) {
	g.GET("/configs", handleListExpressionConfigs, requirePermission(security.PermExpressionConfig))
	g.POST("/configs", handleCreateExpressionConfig, requirePermission(security.PermExpressionConfig))
	g.PUT("/configs/:id", handleUpdateExpressionConfig, requirePermission(security.PermExpressionConfig))
	g.DELETE("/configs/:id", handleDeleteExpressionConfig, requirePermission(security.PermExpressionConfig))
	g.POST("/test", handleTestExpression, requirePermission(security.PermExpressionConfig))
}

func handleListExpressionConfigs(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.ExpressionConfigs == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the expression store is not ready")
	}
	expressions, err := cont.ExpressionConfigs.List()
	if err != nil {
		logrus.WithError(err).Error("Failed to read derived point expressions")
		return InternalError(c, "ERR_COMMON_READ_FAILED: expressions could not be read")
	}
	if expressions == nil {
		expressions = []models.ExpressionConfig{}
	}
	return OK(c, expressions)
}

func handleCreateExpressionConfig(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.ExpressionConfigs == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the expression store is not ready")
	}
	var expr models.ExpressionConfig
	if err := c.Bind(&expr); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	expr.Name = strings.TrimSpace(expr.Name)
	expr.OutputPoint = strings.TrimSpace(expr.OutputPoint)
	if expr.OutputPoint == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: output_point is required, it is the point name the derived value is published under")
	}
	if len(expr.OutputPoint) > 200 {
		return BadRequest(c, "ERR_COMMON_VALIDATION: output_point is longer than 200 characters")
	}
	if err := validateDerivedExpression(expr.Expression); err != nil {
		return BadRequest(c, err.Error())
	}
	expr.ID = newExpressionConfigID()
	if err := cont.ExpressionConfigs.Save(expr); err != nil {
		logrus.WithError(err).Error("Failed to store derived point expression")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the expression could not be stored")
	}
	if err := applyExpressionsFromStore(cont); err != nil {
		logrus.WithError(err).Error("Expression was stored but could not be applied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the expression could not be applied to the running engine")
	}
	return Created(c, expr)
}

func handleUpdateExpressionConfig(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	cont := GetContainer()
	if cont == nil || cont.ExpressionConfigs == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the expression store is not ready")
	}
	if id == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: the expression id is required")
	}
	stored, err := cont.ExpressionConfigs.Get(id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return NotFound(c, "ERR_COMMON_NOT_FOUND: no expression is stored under "+id)
		}
		return InternalError(c, "ERR_COMMON_READ_FAILED: "+err.Error())
	}
	var raw map[string]interface{}
	if err := c.Bind(&raw); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	updated := stored
	for key, target := range map[string]*string{
		"name":         &updated.Name,
		"expression":   &updated.Expression,
		"output_point": &updated.OutputPoint,
		"device_id":    &updated.DeviceID,
	} {
		if value, ok := raw[key].(string); ok {
			*target = strings.TrimSpace(value)
		}
	}
	if value, ok := raw["enabled"].(bool); ok {
		updated.Enabled = value
	}
	if updated.OutputPoint == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: output_point is required")
	}
	if err := validateDerivedExpression(updated.Expression); err != nil {
		return BadRequest(c, err.Error())
	}
	if err := cont.ExpressionConfigs.Save(updated); err != nil {
		logrus.WithError(err).Error("Failed to store derived point expression")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the expression could not be stored")
	}
	if err := applyExpressionsFromStore(cont); err != nil {
		logrus.WithError(err).Error("Expression was stored but could not be applied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the expression could not be applied to the running engine")
	}
	return OK(c, updated)
}

func handleDeleteExpressionConfig(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	cont := GetContainer()
	if cont == nil || cont.ExpressionConfigs == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the expression store is not ready")
	}
	removed, err := cont.ExpressionConfigs.Delete(id)
	if err != nil {
		logrus.WithError(err).Error("Failed to delete derived point expression")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the expression could not be deleted")
	}
	if !removed {
		return NotFound(c, "ERR_COMMON_NOT_FOUND: no expression is stored under "+id)
	}
	if err := applyExpressionsFromStore(cont); err != nil {
		logrus.WithError(err).Error("Expression was deleted but could not be reapplied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: the running engine could not be updated")
	}
	return OK(c, map[string]interface{}{"id": id, "deleted": true})
}

// handleTestExpression computes the expression instead of answering null. It is
// the /expressions/configs sibling of the workbench's evaluate route and takes
// either spelling of the variable map.
func handleTestExpression(c echo.Context) error {
	var req expressionRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	req.Expression = strings.TrimSpace(req.Expression)
	if req.Expression == "" {
		return BadRequest(c, "ERR_COMMON_VALIDATION: expression is required")
	}
	result, err := expressionEvaluator.EvaluateDetailed(req.Expression, req.vars())
	if err != nil {
		return OK(c, map[string]interface{}{
			"expression": req.Expression,
			"result":     nil,
			"error":      err.Error(),
		})
	}
	return OK(c, map[string]interface{}{
		"expression": req.Expression,
		"result":     result,
		"error":      "",
	})
}

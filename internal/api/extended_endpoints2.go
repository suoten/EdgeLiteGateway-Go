package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
)

// ============================================================================
// Notify Extended Routes — 补齐 Python notify.py 中缺失的端点
// ============================================================================

func RegisterNotifyExtendedRoutes(g *echo.Group) {
	g.POST("/channels/dingtalk", handleCreateDingTalkChannel, requirePermission(security.PermNotifyConfig))
	g.POST("/channels/wecom", handleCreateWeComChannel, requirePermission(security.PermNotifyConfig))
	g.POST("/channels/email", handleCreateEmailChannel, requirePermission(security.PermNotifyConfig))
	g.POST("/channels/webhook", handleCreateWebhookChannel, requirePermission(security.PermNotifyConfig))
	g.POST("/channels/:channel_id/test", handleTestChannel, requirePermission(security.PermNotifyConfig))
	g.POST("/channels/:channel_id/enable", handleEnableChannel, requirePermission(security.PermNotifyConfig))
	g.DELETE("/channels/:channel_id", handleDeleteChannel, requirePermission(security.PermNotifyConfig))
}

// notifySectionIn points at the config section for a channel inside a given
// notify config.
func notifySectionIn(notify *config.NotifyConfig, channel string) (interface{}, bool) {
	switch notifyCanonicalKey(channel) {
	case "dingtalk":
		return &notify.Dingtalk, true
	case "email":
		return &notify.Email, true
	case "wechat":
		return &notify.Wechat, true
	case "webhook":
		return &notify.Webhook, true
	}
	return nil, false
}

// notifySection points at the live config section for a channel name, so that
// writing through it also updates every reader of config.GetConfig().
func notifySection(channel string) (interface{}, bool) {
	return notifySectionIn(&config.GetConfig().Notify, channel)
}

// notifyChannelConfigured mirrors the guard every sender applies, so the test
// endpoint can answer "not configured" before attempting delivery.
func notifyChannelConfigured(notify *config.NotifyConfig, channel string) bool {
	return services.ChannelConfigured(notify, notifyCanonicalKey(channel))
}

// normalizeNotifyBody folds a request body onto the config's key names and
// replaces masked credentials with the stored ones, so a page that round-trips
// "a***z" cannot wipe a real secret.
func normalizeNotifyBody(channel string, section map[string]interface{}) map[string]interface{} {
	if section == nil {
		section = map[string]interface{}{}
	}
	applyNotifyKeys(channel, section, true)
	config.RestoreMaskedSecrets(map[string]interface{}{
		"notify": map[string]interface{}{notifyCanonicalKey(channel): section},
	})
	return section
}

// notifyTestConfig applies unsaved form values onto a copy of the live notify
// config, giving the test endpoint the settings the operator is looking at.
func notifyTestConfig(channel string, section map[string]interface{}) (*config.NotifyConfig, bool) {
	snapshot := config.GetConfig().Notify
	target, ok := notifySectionIn(&snapshot, channel)
	if !ok {
		return nil, false
	}
	body, err := json.Marshal(section)
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, false
	}
	return &snapshot, true
}

// notifyUIKeys maps channel config keys onto the names the notification page
// posts and reads back: the Go port kept the Python config names (from_addr,
// to_addrs) while the UI sends from_address/to_addresses, so those two values
// were dropped on every save.
var notifyUIKeys = map[string]map[string]string{
	"email": {"from_addr": "from_address", "to_addrs": "to_addresses"},
}

// notifyCanonicalKey folds the UI's channel id onto the config section name.
func notifyCanonicalKey(channel string) string {
	if channel == "wecom" {
		return "wechat"
	}
	return channel
}

// applyNotifyKeys renames section keys between the config and the UI naming in
// place, returning the same map for chaining.
func applyNotifyKeys(channel string, section map[string]interface{}, toConfig bool) map[string]interface{} {
	aliases, ok := notifyUIKeys[notifyCanonicalKey(channel)]
	if !ok {
		return section
	}
	for configKey, uiKey := range aliases {
		from, to := configKey, uiKey
		if toConfig {
			from, to = uiKey, configKey
		}
		if v, ok := section[from]; ok {
			section[to] = v
			delete(section, from)
		}
	}
	return section
}

// saveNotifyChannel merges a request body into one notify section and persists
// the config. The body is unmarshalled onto the existing value because the UI
// omits fields such as enabled/name — binding onto a fresh struct would reset
// them on every save.
func saveNotifyChannel(channel string, c echo.Context) error {
	target, ok := notifySection(channel)
	if !ok {
		return BadRequest(c, "unknown notify channel: "+channel)
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil || len(body) == 0 {
		return BadRequest(c, "Invalid request body")
	}
	var section map[string]interface{}
	if err := json.Unmarshal(body, &section); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	merged, err := json.Marshal(normalizeNotifyBody(channel, section))
	if err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if err := json.Unmarshal(merged, target); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if err := config.SaveConfig(config.GetConfig(), ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	logrus.WithField("channel", channel).Info("Notify channel saved")
	return Created(c, map[string]interface{}{"channel": channel, "saved": true})
}

func handleCreateDingTalkChannel(c echo.Context) error {
	return saveNotifyChannel("dingtalk", c)
}

func handleCreateWeComChannel(c echo.Context) error {
	return saveNotifyChannel("wechat", c)
}

func handleCreateEmailChannel(c echo.Context) error {
	return saveNotifyChannel("email", c)
}

func handleCreateWebhookChannel(c echo.Context) error {
	return saveNotifyChannel("webhook", c)
}

// handleTestChannel sends a test message through one channel. The request body
// may carry unsaved form values, which are applied onto a copy of the live
// config so the button tests what the operator is looking at.
func handleTestChannel(c echo.Context) error {
	channelID := c.Param("channel_id")
	if _, ok := notifySectionIn(&config.GetConfig().Notify, channelID); !ok {
		return BadRequest(c, "unknown notify channel: "+channelID)
	}

	notifyCfg := &config.GetConfig().Notify
	body, err := io.ReadAll(c.Request().Body)
	if err == nil && len(body) > 0 {
		var section map[string]interface{}
		if err := json.Unmarshal(body, &section); err == nil && section != nil {
			applied, ok := notifyTestConfig(channelID, normalizeNotifyBody(channelID, section))
			if !ok {
				return BadRequest(c, "unknown notify channel: "+channelID)
			}
			notifyCfg = applied
		}
	}
	if !notifyChannelConfigured(notifyCfg, channelID) {
		return BadRequest(c, "ERR_NOTIFY_CHANNEL_NOT_CONFIGURED")
	}

	if err := notifyService().SendNotificationWith(notifyCfg, []string{channelID}, "Test Notification", "Test from EdgeLite Gateway", "info"); err != nil {
		return OK(c, map[string]interface{}{"channel_id": channelID, "success": false, "sent": false, "message": err.Error()})
	}
	return OK(c, map[string]interface{}{"channel_id": channelID, "success": true, "sent": true})
}

// notifyChannelHasSwitch reports whether a channel carries its own enabled flag.
func notifyChannelHasSwitch(channel string) bool {
	return channel == "dingtalk" || channel == "webhook"
}

func handleEnableChannel(c echo.Context) error {
	channelID := c.Param("channel_id")
	enabled := c.QueryParam("enabled") != "false"
	target, ok := notifySection(channelID)
	if !ok {
		return BadRequest(c, "unknown notify channel: "+channelID)
	}
	if !notifyChannelHasSwitch(channelID) {
		// Email and WeCom have no enable switch: they send as soon as they are
		// configured, so there is nothing to persist here.
		return OK(c, map[string]interface{}{"channel_id": channelID, "enabled": true})
	}
	switch section := target.(type) {
	case *config.NotifyDingtalkConfig:
		section.Enabled = enabled
	case *config.NotifyWebhookConfig:
		section.Enabled = enabled
	}
	if err := config.SaveConfig(config.GetConfig(), ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	return OK(c, map[string]interface{}{"channel_id": channelID, "enabled": enabled})
}

// handleDeleteChannel clears a channel's credentials/endpoint so it stops
// receiving traffic, and reports what it did.
func handleDeleteChannel(c echo.Context) error {
	channelID := c.Param("channel_id")
	target, ok := notifySection(channelID)
	if !ok {
		return BadRequest(c, "unknown notify channel: "+channelID)
	}
	switch section := target.(type) {
	case *config.NotifyDingtalkConfig:
		section.WebhookURL = ""
		section.Secret = ""
		section.Enabled = false
	case *config.NotifyEmailConfig:
		section.SMTPHost = ""
		section.SMTPUser = ""
		section.SMTPPassword = ""
		section.ToAddrs = []string{}
	case *config.NotifyWechatConfig:
		section.WebhookURL = ""
	case *config.NotifyWebhookConfig:
		section.URL = ""
		section.AuthToken = ""
		section.AuthPassword = ""
		section.Enabled = false
	}
	if err := config.SaveConfig(config.GetConfig(), ""); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	logrus.WithField("channel_id", channelID).Info("Notify channel deleted")
	return OK(c, map[string]interface{}{"deleted": channelID})
}

// ============================================================================
// Drivers Extended Routes — 补齐 Python drivers.py 中缺失的端点
// ============================================================================

func RegisterDriverExtendedRoutes(g *echo.Group) {
	g.GET("/list", handleGetDriverList, requirePermission(security.PermDriverConfig))
	g.GET("/:driver_name/config-schema", handleGetDriverConfigSchema, requirePermission(security.PermDriverConfig))
	g.POST("/:driver_name/discover", handleDriverDiscover, requirePermission(security.PermDriverConfig))
	g.GET("/load-status", handleGetDriverLoadStatus, requirePermission(security.PermDriverConfig))
	g.GET("/meta", handleGetDriverMeta, requirePermission(security.PermDriverConfig))
	g.GET("/:driver_name/environment-check", handleDriverEnvCheck, requirePermission(security.PermDriverConfig))
	g.POST("/opcua/browse", handleOPCUABrowse, requirePermission(security.PermDriverConfig))
	g.GET("/opcua/certificate-status", handleOPCUACertStatus, requirePermission(security.PermDriverConfig))
	g.GET("/opc-da/servers", handleOPCDAServers, requirePermission(security.PermDriverConfig))
	g.GET("/health", handleGetDriverHealth, requirePermission(security.PermDriverConfig))
}

func handleGetDriverList(c echo.Context) error {
	names := registeredDrivers()
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]interface{}{
			"name":        name,
			"description": drivers.GetDriverDisplayName(name, "en"),
			"protocols":   []string{name},
			"enabled":     true,
			"version":     "1.0",
		})
	}
	return OK(c, map[string]interface{}{
		"drivers": out,
		"total":   len(out),
	})
}

// handleDriverDiscover runs the protocol's own discovery through the same
// service the device page uses (POST /devices/discover). It used to return
// "0 devices" without calling a driver at all, which the UI rendered as a
// successful-but-empty scan.
func handleDriverDiscover(c echo.Context) error {
	driverName := c.Param("driver_name")
	var body struct {
		Config map[string]interface{} `json:"config"`
	}
	if err := c.Bind(&body); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	if !discoverProtocolAvailable(driverName) {
		return ErrorCode(c, http.StatusBadRequest, "ERR_DRIVER_NOT_AVAILABLE",
			fmt.Sprintf("No driver named %q is installed in this gateway, so it cannot be scanned for devices", driverName))
	}
	devices, err := cont.DeviceService.Discover(c.Request().Context(), &models.DiscoverRequest{
		Protocol: driverName,
		Config:   body.Config,
	})
	if err != nil {
		if errors.Is(err, drivers.ErrDiscoveryUnsupported) || errors.Is(err, drivers.ErrOPCDAUnsupported) {
			return ErrorCode(c, http.StatusNotImplemented,
				"ERR_DRIVER_DISCOVER_UNSUPPORTED",
				"ERR_DRIVER_DISCOVER_UNSUPPORTED")
		}
		logrus.WithError(err).WithField("driver", driverName).Error("Driver discover failed")
		return InternalError(c, "ERR_DEVICE_DISCOVER_FAILED")
	}
	if devices == nil {
		devices = []map[string]interface{}{}
	}
	return OK(c, map[string]interface{}{
		"driver":     driverName,
		"devices":    devices,
		"discovered": len(devices),
	})
}

// handleGetDriverLoadStatus reports the protocols the driver registry actually
// holds. The dynamic tracker it used to imitate (ExtendedRegistry load status)
// has no producer, so the registry itself is the only honest source.
func handleGetDriverLoadStatus(c echo.Context) error {
	names := registeredDrivers()
	entries := make(map[string]interface{}, len(names))
	for _, name := range names {
		entries[name] = map[string]interface{}{"loaded": true, "error": nil}
	}
	return OK(c, map[string]interface{}{
		"drivers":       entries,
		"loaded_count":  len(names),
		"skipped_count": 0,
	})
}

// handleDriverEnvCheck used to answer environment_ok:true for every driver.
// Driver factories are compiled into the binary, so there is no runtime
// dependency resolution to report; the check would only ever repeat what the
// registry already says in /drivers/list.
func handleDriverEnvCheck(c echo.Context) error {
	driverName := constants.NormalizeProtocol(c.Param("driver_name"))
	if !drivers.GetRegistry().IsSupported(driverName) {
		return NotFound(c, "ERR_DRIVER_NOT_FOUND")
	}
	return ErrorCode(c, http.StatusNotImplemented, "ERR_DRIVER_ENV_CHECK_UNSUPPORTED", "ERR_DRIVER_ENV_CHECK_UNSUPPORTED")
}

// handleOPCUABrowse runs the Browse service of an OPC UA server and returns its
// real address-space children. It used to answer [] for every request, which the
// UI displayed as "this server exposes no nodes".
//
// Two callers ask different questions: the device editor browses a device it has
// already saved, and the point editor browses the endpoint that is still only in
// the form. The second one is when an operator actually needs the node list —
// they are typing the address right now — so a config-only request opens a
// throwaway session instead of failing with "device not found".
func handleOPCUABrowse(c echo.Context) error {
	var req struct {
		DeviceID string                 `json:"device_id"`
		NodeID   string                 `json:"node_id"`
		Config   map[string]interface{} `json:"config"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.DeviceID == "" && len(req.Config) == 0 {
		return BadRequest(c, "ERR_DEVICE_ID_REQUIRED")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), driverBrowseTimeout)
	defer cancel()

	var entries []drivers.BrowseEntry
	var err error
	if req.DeviceID != "" {
		device, derr := cont.DeviceService.Get(req.DeviceID)
		if derr != nil || device == nil {
			return NotFound(c, "ERR_DEVICE_NOT_FOUND")
		}
		entries, err = cont.DeviceService.BrowseNodes(ctx, req.DeviceID, req.NodeID)
	} else {
		entries, err = browseUnsavedEndpoint(ctx, req.Config, req.NodeID)
	}
	if err != nil {
		if errors.Is(err, services.ErrBrowseUnsupported) {
			return ErrorCode(c, http.StatusNotImplemented, "ERR_DRIVER_BROWSE_UNSUPPORTED", "ERR_DRIVER_BROWSE_UNSUPPORTED")
		}
		logrus.WithError(err).
			WithField("device_id", req.DeviceID).
			Warn("OPC UA browse failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_DRIVER_BROWSE_FAILED", err.Error())
	}
	if entries == nil {
		entries = []drivers.BrowseEntry{}
	}
	return OK(c, entries)
}

// browseUnsavedEndpoint connects an OPC UA client built from a config that has
// not been saved as a device, browses one node, and disconnects. It is the same
// temporary-driver pattern /devices/discover already uses, and it inherits that
// route's permission gate: this is an operator-directed connection to an address
// the operator typed, not a caller-reachable forward.
func browseUnsavedEndpoint(ctx context.Context, config map[string]interface{}, nodeID string) ([]drivers.BrowseEntry, error) {
	drv, err := drivers.GetRegistry().CreateDriver("opc_ua", "browse-unsaved", config)
	if err != nil {
		return nil, err
	}
	browser, canBrowse := drv.(drivers.NodeBrowser)
	if !canBrowse {
		return nil, fmt.Errorf("%w: %s", services.ErrBrowseUnsupported, drv.Name())
	}
	if err := drv.Connect(ctx); err != nil {
		return nil, err
	}
	defer func() { _ = drv.Disconnect() }()
	return browser.BrowseChildren(ctx, nodeID)
}

// driverBrowseTimeout bounds a browse so a hung server cannot pin a request
// goroutine; the address space listing is interactive, so it stays short.
const driverBrowseTimeout = 20 * time.Second

// handleOPCUACertStatus reports the client certificates OPC UA devices actually
// have configured, read from those files. It used to return fixed constants
// ("no certificate", no expiry), so a certificate about to lapse looked absent
// and a valid one looked missing.
func handleOPCUACertStatus(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	devices, _, err := cont.DeviceService.List(1, constants.MaxQuerySize)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_LIST_FAILED")
	}

	certificates := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		if d.Protocol != "opc_ua" {
			continue
		}
		// The device form writes client_cert_path; certificate_file is the key
		// documented for the driver. Both are accepted by OPCUADriver.
		path := ""
		for _, key := range []string{"client_cert_path", "certificate_file"} {
			if v, _ := d.Config[key].(string); v != "" {
				path = v
				break
			}
		}
		if path == "" {
			continue
		}
		entry := map[string]interface{}{
			"device_id":        d.DeviceID,
			"certificate_file": path,
			"readable":         false,
		}
		if info, statErr := os.Stat(path); statErr != nil {
			entry["error"] = "ERR_OPCUA_CERT_FILE_NOT_FOUND"
		} else {
			entry["readable"] = true
			entry["size_bytes"] = info.Size()
			if cert, parseErr := parseX509CertificateFile(path); parseErr != nil {
				entry["error"] = parseErr.Error()
			} else {
				entry["subject"] = cert.Subject.String()
				entry["issuer"] = cert.Issuer.String()
				entry["not_before"] = cert.NotBefore.Format(time.RFC3339)
				entry["expires_at"] = cert.NotAfter.Format(time.RFC3339)
				entry["expired"] = time.Now().After(cert.NotAfter)
			}
		}
		certificates = append(certificates, entry)
	}

	return OK(c, map[string]interface{}{
		"has_certificate": len(certificates) > 0,
		"certificates":    certificates,
		// trusted_hosts has no producer in this build: gopcua does not pin or
		// validate server certificates against a trust list here.
		"trusted_hosts_supported": false,
	})
}

func parseX509CertificateFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ERR_OPCUA_CERT_FILE_UNREADABLE: %s", err)
	}
	if block, _ := pem.Decode(raw); block != nil {
		raw = block.Bytes
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		return nil, fmt.Errorf("ERR_OPCUA_CERT_PARSE_FAILED: %s", err)
	}
	return cert, nil
}

// handleOPCDAServers enumerating OPC DA servers requires the Windows COM
// registry, which this build does not link. An empty list made the dialog read
// as "no servers on that host" instead of "this gateway cannot ask".
func handleOPCDAServers(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented,
		"ERR_OPCDA_DISCOVERY_UNSUPPORTED",
		"ERR_OPCDA_DISCOVERY_UNSUPPORTED")
}

// handleGetDriverHealth aggregates the counters the collectors really record,
// grouped by protocol. It used to return {healthy:true, drivers:{}} for every
// request, which reported an outage as a healthy fleet.
func handleGetDriverHealth(c echo.Context) error {
	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	devices, _, err := cont.DeviceService.List(1, constants.MaxQuerySize)
	if err != nil {
		return InternalError(c, "ERR_DEVICE_LIST_FAILED")
	}

	type protocolHealth struct {
		DeviceCount    int     `json:"device_count"`
		OnlineCount    int     `json:"online_devices"`
		HealthyCount   int     `json:"healthy_devices"`
		TotalReads     int64   `json:"total_reads"`
		FailedReads    int64   `json:"failed_reads"`
		TotalWrites    int64   `json:"total_writes"`
		FailedWrites   int64   `json:"failed_writes"`
		AvgHealthScore float64 `json:"avg_health_score"`
		scored         int
	}

	groups := make(map[string]*protocolHealth)
	deviceHealth := make(map[string]interface{}, len(devices))
	unhealthy := 0
	scoredDevices := 0

	mgr := drivers.GetHealthStatsManager()
	for _, d := range devices {
		group := groups[d.Protocol]
		if group == nil {
			group = &protocolHealth{}
			groups[d.Protocol] = group
		}
		group.DeviceCount++
		if d.Status == "online" {
			group.OnlineCount++
		}

		entry := map[string]interface{}{
			"protocol": d.Protocol,
			"status":   d.Status,
			"healthy":  false,
			// has_samples distinguishes "the link is fine" from "nothing has
			// been tried yet", which a zeroed score cannot.
			"has_samples": false,
		}
		if mgr != nil {
			if st := mgr.GetHealthStats(d.DeviceID); st != nil {
				ctr := st.Counters()
				healthy := st.IsHealthy()
				score := st.HealthScore()
				entry["healthy"] = healthy
				entry["health_score"] = score
				entry["consecutive_failures"] = ctr.ConsecutiveFailures
				entry["read_error_rate"] = st.ReadErrorRate()
				entry["write_error_rate"] = st.WriteErrorRate()
				entry["avg_latency_ms"] = ctr.AvgLatencyMs
				entry["total_reads"] = ctr.TotalReads
				entry["failed_reads"] = ctr.FailedReads
				entry["has_samples"] = ctr.TotalReads+ctr.TotalWrites > 0
				group.TotalReads += ctr.TotalReads
				group.FailedReads += ctr.FailedReads
				group.TotalWrites += ctr.TotalWrites
				group.FailedWrites += ctr.FailedWrites
				group.AvgHealthScore += score
				group.scored++
				scoredDevices++
				if healthy {
					group.HealthyCount++
				} else {
					unhealthy++
				}
			}
			if status := mgr.GetConnectionStatus(d.DeviceID); status != nil {
				entry["connection_state"] = string(status.State)
				entry["state_reason"] = status.Reason
			}
		}
		deviceHealth[d.DeviceID] = entry
	}

	protocols := make(map[string]interface{}, len(groups))
	for name, group := range groups {
		if group.scored > 0 {
			group.AvgHealthScore /= float64(group.scored)
		}
		protocols[name] = group
	}

	return OK(c, map[string]interface{}{
		"healthy":      unhealthy == 0,
		"device_count": len(devices),
		"unhealthy":    unhealthy,
		"has_stats":    mgr != nil && scoredDevices > 0,
		"drivers":      protocols,
		"devices":      deviceHealth,
	})
}

// ============================================================================
// Platforms Extended Routes — 补齐 Python platforms.py 中缺失的端点
// ============================================================================

func RegisterPlatformExtendedRoutes(g *echo.Group) {
	g.GET("/list", handleGetPlatformList, requirePermission(security.PermPlatformManage))
	g.GET("/config-schema/:platform_name", handleGetPlatformConfigSchema, requirePermission(security.PermPlatformManage))
	g.POST("/connect/:platform_name", handleConnectPlatform, requirePermission(security.PermPlatformManage))
	g.POST("/disconnect/:platform_name", handleDisconnectPlatform, requirePermission(security.PermPlatformManage))
	g.POST("/test-connection/:platform_name", handleTestPlatformConnection, requirePermission(security.PermPlatformManage))
	g.GET("/status/:platform_name", handleGetPlatformStatusByName, requirePermission(security.PermPlatformManage))
	g.GET("/dashboard", handleGetPlatformDashboard, requirePermission(security.PermPlatformManage))
	g.GET("/metrics", handleGetPlatformMetrics, requirePermission(security.PermPlatformManage))
	g.POST("/reload/:platform_name", handleReloadPlatform, requirePermission(security.PermPlatformManage))
	g.GET("/message-preview/:platform_name", handleGetMessagePreview, requirePermission(security.PermPlatformManage))
	g.GET("/broker-quality/:platform_name", handleGetBrokerQuality, requirePermission(security.PermPlatformManage))
	g.POST("/validate-topic", handleValidateTopic, requirePermission(security.PermPlatformManage))
	g.GET("/tb/devices/:platform_name", handleGetTBDevices, requirePermission(security.PermPlatformManage))
	g.GET("/tb/rpc-logs/:platform_name", handleGetTBRpcLogs, requirePermission(security.PermPlatformManage))
	g.GET("/tb/alarms/:platform_name", handleGetTBAlarms, requirePermission(security.PermPlatformManage))
	g.GET("/tb/sync-status/:platform_name", handleGetTBSyncStatus, requirePermission(security.PermPlatformManage))
	g.GET("/shadow/:platform_name", handleGetPlatformShadow, requirePermission(security.PermPlatformManage))
	g.GET("/command-logs/:platform_name", handleGetCommandLogs, requirePermission(security.PermPlatformManage))
	g.GET("/alarm-records/:platform_name", handleGetAlarmRecords, requirePermission(security.PermPlatformManage))
	g.GET("/device-mapping/:platform_name", handleGetDeviceMapping, requirePermission(security.PermPlatformManage))
	g.GET("/export/:platform_name", handleExportPlatform, requirePermission(security.PermPlatformManage))
	g.POST("/import/:platform_name", handleImportPlatform, requirePermission(security.PermPlatformManage))
	g.GET("/broker-status/:platform_name", handleGetBrokerStatus, requirePermission(security.PermPlatformManage))
	g.POST("/validate-advanced-template", handleValidateAdvancedTemplate, requirePermission(security.PermPlatformManage))
	g.POST("/preview-template", handlePreviewTemplate, requirePermission(security.PermPlatformManage))
	g.POST("/validate-script", handleValidateScript, requirePermission(security.PermPlatformManage))
	g.POST("/test-script", handleTestScriptPlatform, requirePermission(security.PermPlatformManage))
	g.POST("/mqtt-test-publish/:platform_name", handleMqttTestPublish, requirePermission(security.PermPlatformManage))
}

func handleGetPlatformList(c echo.Context) error {
	// Supported platform catalog for the frontend card picker.
	supported := make([]map[string]interface{}, 0, len(platform.PlatformCatalog))
	for _, meta := range platform.PlatformCatalog {
		supported = append(supported, map[string]interface{}{
			"name":        meta.Name,
			"label":       meta.Label,
			"description": meta.Description,
			"version":     meta.Version,
		})
	}
	return OK(c, map[string]interface{}{
		"platforms": buildPlatformListEntries(),
		"supported": supported,
	})
}

func handleGetPlatformConfigSchema(c echo.Context) error {
	platformName := c.Param("platform_name")
	meta, ok := platform.GetPlatformMeta(platformName)
	if !ok {
		return NotFound(c, "Unknown platform: "+platformName)
	}
	return OK(c, map[string]interface{}{
		"platform":      platformName,
		"config_schema": platform.GetPlatformSchema(platformName),
		"label":         meta.Label,
		"description":   meta.Description,
	})
}

func handleConnectPlatform(c echo.Context) error {
	platformName := c.Param("platform_name")
	var req struct {
		Config map[string]interface{} `json:"config"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cfg := req.Config
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	// Preserve real secrets when the caller re-sends masked values (e.g. a
	// config loaded from the export endpoint), and keep the persisted type
	// for custom-named instances (instance name != platform type).
	if stored, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
		cfg = mergePlatformConfig(stored, cfg)
	}
	cfg["type"] = resolvePlatformType(platformName, cfg)

	mgr := getPlatformManager()
	if mgr == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_PLATFORM_NOT_READY", "Platform manager not ready")
	}
	if err := mgr.Connect(platformName, cfg); err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_PLATFORM_CONNECT_FAILED", err.Error())
	}

	// Persist the connection config so the platform auto-connects on restart.
	stored := make(map[string]interface{}, len(cfg)+1)
	for k, v := range cfg {
		stored[k] = v
	}
	stored["enabled"] = true
	appCfg := config.GetConfig()
	appCfg.Platforms[platformName] = stored
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", platformName).Warn("Failed to persist platform config")
	}
	return OK(c, map[string]interface{}{
		"platform":  platformName,
		"status":    "connected",
		"connected": true,
	})
}

func handleDisconnectPlatform(c echo.Context) error {
	platformName := c.Param("platform_name")
	mgr := getPlatformManager()
	if mgr == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_PLATFORM_NOT_READY", "Platform manager not ready")
	}
	if err := mgr.Disconnect(platformName); err != nil {
		return ErrorCode(c, http.StatusInternalServerError, "ERR_PLATFORM_DISCONNECT_FAILED", err.Error())
	}
	return OK(c, map[string]interface{}{
		"platform":     platformName,
		"status":       "disconnected",
		"disconnected": true,
	})
}

func handleTestPlatformConnection(c echo.Context) error {
	platformName := c.Param("platform_name")
	var req struct {
		Config map[string]interface{} `json:"config"`
	}
	_ = c.Bind(&req)
	cfg := req.Config
	if cfg == nil {
		if stored, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
			cfg = stored
		}
	}
	if cfg == nil {
		cfg = map[string]interface{}{"type": platformName}
	}
	cfg["type"] = resolvePlatformType(platformName, cfg)
	mgr := getPlatformManager()
	if mgr == nil {
		return OK(c, map[string]interface{}{"platform": platformName, "success": false, "message": "Platform manager not ready"})
	}
	result, err := mgr.TestConnection(platformName, cfg)
	if err != nil {
		return OK(c, map[string]interface{}{"platform": platformName, "success": false, "message": err.Error()})
	}
	return OK(c, result)
}

func handleGetPlatformStatusByName(c echo.Context) error {
	platformName := c.Param("platform_name")
	enabled := true
	if m, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
		if b, ok := m["enabled"].(bool); ok {
			enabled = b
		}
	}
	state, connected := platformConnectionState(platformName, enabled)
	var stats northbound.PlatformStats
	if mgr := getPlatformManager(); mgr != nil {
		stats = mgr.GetStats(platformName)
	}
	lastActive := ""
	if !stats.LastActive.IsZero() {
		lastActive = stats.LastActive.Format(time.RFC3339)
	}
	return OK(c, map[string]interface{}{
		"platform":          platformName,
		"name":              platformName,
		"version":           "1.0",
		"connected":         connected,
		"status":            state,
		"last_active":       lastActive,
		"messages_sent":     stats.MessagesSent,
		"messages_failed":   stats.MessagesFailed,
		"messages_received": 0,
	})
}

// handleGetPlatformDashboard returns northbound platform status entries
// (schema parity with Python GET /platforms/dashboard). Connection state and
// message statistics come from the northbound platform manager.
func handleGetPlatformDashboard(c echo.Context) error {
	cfg := config.GetConfig()
	mgr := getPlatformManager()

	names := make([]string, 0, len(cfg.Platforms))
	for name := range cfg.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)

	dashboard := []map[string]interface{}{}
	for _, name := range names {
		p := cfg.Platforms[name]
		entry := map[string]interface{}{
			"platform_name":  name,
			"label":          name,
			"state":          "disconnected",
			"connected":      false,
			"messages_today": 0,
			"error_rate":     0.0,
			"queue_backlog":  0,
			"last_heartbeat": nil,
			"latency_ms":     0.0,
		}
		enabled := false
		if m, ok := p.(map[string]interface{}); ok {
			if b, ok := m["enabled"].(bool); ok {
				enabled = b
			}
			if s, ok := m["type"].(string); ok {
				if meta, ok := platform.GetPlatformMeta(s); ok {
					entry["label"] = meta.Label
				} else {
					entry["label"] = s
				}
			}
			if l, ok := m["label"].(string); ok && l != "" {
				entry["label"] = l
			}
		}
		stats := northbound.PlatformStats{}
		if mgr != nil {
			stats = mgr.GetStats(name)
		}
		connected := mgr != nil && mgr.IsConnected(name)
		entry["connected"] = connected
		if connected {
			entry["state"] = "connected"
			entry["messages_today"] = stats.MessagesSent
			if !stats.LastActive.IsZero() {
				entry["last_heartbeat"] = stats.LastActive.Format(time.RFC3339)
			}
			if total := stats.MessagesSent + stats.MessagesFailed; total > 0 {
				entry["error_rate"] = round2(float64(stats.MessagesFailed) / float64(total))
			}
		} else if !enabled {
			entry["state"] = "unknown"
		}
		dashboard = append(dashboard, entry)
	}
	return OK(c, dashboard)
}

func handleGetPlatformMetrics(c echo.Context) error {
	cfg := config.GetConfig()
	mgr := getPlatformManager()
	var b strings.Builder
	b.WriteString("# HELP edgelite_platforms_total Total configured northbound platforms.\n")
	b.WriteString("# TYPE edgelite_platforms_total gauge\n")
	fmt.Fprintf(&b, "edgelite_platforms_total %d\n", len(cfg.Platforms))
	b.WriteString("# HELP edgelite_platform_messages_sent Messages published to the platform.\n")
	b.WriteString("# TYPE edgelite_platform_messages_sent counter\n")
	for _, name := range sortedPlatformNames() {
		var stats northbound.PlatformStats
		connected := 0
		if mgr != nil {
			stats = mgr.GetStats(name)
			if mgr.IsConnected(name) {
				connected = 1
			}
		}
		fmt.Fprintf(&b, "edgelite_platform_connected{name=%q} %d\n", name, connected)
		fmt.Fprintf(&b, "edgelite_platform_messages_sent{name=%q} %d\n", name, stats.MessagesSent)
		fmt.Fprintf(&b, "edgelite_platform_messages_failed{name=%q} %d\n", name, stats.MessagesFailed)
	}
	return c.String(http.StatusOK, b.String())
}

// sortedPlatformNames returns configured platform names in stable order.
func sortedPlatformNames() []string {
	cfg := config.GetConfig()
	names := make([]string, 0, len(cfg.Platforms))
	for name := range cfg.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func handleReloadPlatform(c echo.Context) error {
	platformName := c.Param("platform_name")
	var req struct {
		Config map[string]interface{} `json:"config"`
	}
	_ = c.Bind(&req)
	cfg := req.Config
	if cfg == nil {
		if stored, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
			cfg = stored
		}
	}
	if cfg == nil {
		return NotFound(c, "Platform not found: "+platformName)
	}
	// The edit modal loads the exported (masked) config into its form and
	// submits it here; preserve the real secrets behind "***" placeholders.
	if stored, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
		cfg = mergePlatformConfig(stored, cfg)
	}
	cfg["type"] = resolvePlatformType(platformName, cfg)
	mgr := getPlatformManager()
	if mgr == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_PLATFORM_NOT_READY", "Platform manager not ready")
	}
	if err := mgr.Connect(platformName, cfg); err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_PLATFORM_CONNECT_FAILED", err.Error())
	}
	// Persist any config changes supplied with the reload.
	stored := make(map[string]interface{}, len(cfg)+1)
	for k, v := range cfg {
		stored[k] = v
	}
	stored["enabled"] = true
	appCfg := config.GetConfig()
	appCfg.Platforms[platformName] = stored
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", platformName).Warn("Failed to persist platform config")
	}
	logrus.WithField("platform", platformName).Info("Platform reloaded")
	return OK(c, map[string]interface{}{
		"platform":  platformName,
		"reloaded":  true,
		"connected": true,
		"status":    "connected",
	})
}

func handleGetMessagePreview(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"messages": samplePlatformMessages(platformName),
	})
}

// samplePlatformMessages builds one sample topic/payload pair per platform so
// users can inspect what will be published without a live connection.
func samplePlatformMessages(platformName string) []map[string]interface{} {
	now := time.Now()
	sampleValues := map[string]interface{}{"temperature": 25.6, "humidity": 60.2}
	deviceID := "demo-device"
	var topic string
	var payload interface{}
	switch platformName {
	case "thingsboard":
		topic = "v1/gateway/telemetry"
		payload = map[string]interface{}{
			deviceID: []map[string]interface{}{{"ts": now.UnixMilli(), "values": sampleValues}},
		}
	case "huawei_iotda":
		topic = fmt.Sprintf("$oc/devices/%s/sys/properties/report", deviceID)
		payload = map[string]interface{}{
			"services": []map[string]interface{}{{
				"service_id": "edgelite",
				"properties": sampleValues,
				"event_time": now.Format(time.RFC3339),
			}},
		}
	case "thingspanel":
		topic = fmt.Sprintf("device/%s/telemetry", deviceID)
		payload = sampleValues
	case "thingscloud":
		topic = fmt.Sprintf("thingscloud/%s/telemetry", deviceID)
		payload = sampleValues
	case "iotsharp":
		topic = fmt.Sprintf("devices/telemetry/%s", deviceID)
		payload = sampleValues
	default: // custom_mqtt
		topic = fmt.Sprintf("edgelite/telemetry/%s", deviceID)
		payload = sampleValues
	}
	return []map[string]interface{}{{
		"topic":     topic,
		"payload":   payload,
		"qos":       1,
		"timestamp": now.Format(time.RFC3339),
	}}
}

func handleGetBrokerQuality(c echo.Context) error {
	platformName := c.Param("platform_name")
	mgr := getPlatformManager()
	var stats northbound.PlatformStats
	connected := false
	if mgr != nil {
		stats = mgr.GetStats(platformName)
		connected = mgr.IsConnected(platformName)
	}
	quality := "unknown"
	if connected {
		quality = "good"
		if stats.MessagesFailed > stats.MessagesSent {
			quality = "poor"
		}
	}
	latencyMs := stats.LastTestLatency
	return OK(c, map[string]interface{}{
		"platform":          platformName,
		"connected":         connected,
		"quality":           quality,
		"latency_ms":        latencyMs,
		"avg_latency_ms":    latencyMs,
		"max_latency_ms":    latencyMs,
		"min_latency_ms":    latencyMs,
		"packet_loss_count": stats.MessagesFailed,
		"loss_rate":         0.0,
		"samples":           stats.MessagesSent,
	})
}

func handleValidateTopic(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	topic, _ := req["topic"].(string)
	valid := len(topic) > 0
	return OK(c, map[string]interface{}{"topic": topic, "valid": valid})
}

func handleGetTBDevices(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"devices":  []interface{}{},
		"total":    0,
	})
}

func handleGetTBRpcLogs(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"logs":     []interface{}{},
	})
}

func handleGetTBAlarms(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"alarms":   []interface{}{},
	})
}

func handleGetTBSyncStatus(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform":    platformName,
		"last_sync":   "",
		"sync_status": "idle",
	})
}

func handleGetPlatformShadow(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"shadow":   map[string]interface{}{},
	})
}

func handleGetCommandLogs(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"logs":     []interface{}{},
	})
}

func handleGetAlarmRecords(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"records":  []interface{}{},
	})
}

func handleGetDeviceMapping(c echo.Context) error {
	platformName := c.Param("platform_name")
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"mappings": []interface{}{},
	})
}

func handleExportPlatform(c echo.Context) error {
	platformName := c.Param("platform_name")
	// Security: sanitize platform name for Content-Disposition header
	safeName := sanitizeForHeader(platformName)
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s_export.json", safeName))
	stored, ok := config.GetConfig().Platforms[platformName].(map[string]interface{})
	if !ok {
		return NotFound(c, "Platform not found: "+platformName)
	}
	masked := platform.MaskConfig(stored)
	return OK(c, map[string]interface{}{
		"platform": platformName,
		"config":   masked,
		"data":     masked,
	})
}

func handleImportPlatform(c echo.Context) error {
	platformName := c.Param("platform_name")
	var req struct {
		ConfigData map[string]interface{} `json:"config_data"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.ConfigData == nil {
		return BadRequest(c, "config_data is required")
	}
	cfg := config.GetConfig()
	existing, _ := cfg.Platforms[platformName].(map[string]interface{})
	// Masked secrets in an imported export file must not overwrite the real
	// stored credentials; mergePlatformConfig keeps existing values for "***".
	merged := mergePlatformConfig(existing, req.ConfigData)
	if existing != nil {
		// Preserve runtime flags not present in the imported file.
		for _, key := range []string{"type", "enabled"} {
			if v, ok := existing[key]; ok {
				if _, exists := merged[key]; !exists || merged[key] == nil {
					merged[key] = v
				}
			}
		}
	}
	if _, ok := merged["type"]; !ok {
		merged["type"] = platformName
	}
	if _, ok := merged["enabled"]; !ok {
		merged["enabled"] = false
	}
	cfg.Platforms[platformName] = merged
	if err := persistPlatformConfig(); err != nil {
		logrus.WithError(err).WithField("platform", platformName).Warn("Failed to persist imported platform config")
	}
	return OK(c, map[string]interface{}{"platform": platformName, "imported": true})
}

func handleGetBrokerStatus(c echo.Context) error {
	platformName := c.Param("platform_name")
	enabled := true
	if m, ok := config.GetConfig().Platforms[platformName].(map[string]interface{}); ok {
		if b, ok := m["enabled"].(bool); ok {
			enabled = b
		}
	}
	state, connected := platformConnectionState(platformName, enabled)
	return OK(c, map[string]interface{}{
		"platform":  platformName,
		"status":    state,
		"connected": connected,
	})
}

func handleValidateAdvancedTemplate(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"valid": true, "errors": []interface{}{}})
}

func handlePreviewTemplate(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"preview": req})
}

func handleValidateScript(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"valid": true, "errors": []interface{}{}})
}

func handleTestScriptPlatform(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"output": "", "error": ""})
}

func handleMqttTestPublish(c echo.Context) error {
	platformName := c.Param("platform_name")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	mgr := getPlatformManager()
	if mgr == nil || !mgr.IsConnected(platformName) {
		return OK(c, map[string]interface{}{
			"platform":  platformName,
			"published": false,
			"success":   false,
			"error":     "platform not connected",
		})
	}
	return OK(c, map[string]interface{}{
		"platform":  platformName,
		"published": true,
		"success":   true,
	})
}

// ============================================================================
// Audit Extended Routes — 补齐 Python audit.py 中缺失的端点
// ============================================================================

func RegisterAuditExtendedRoutes(g *echo.Group) {
	g.GET("/integrity", handleGetAuditIntegrity, requirePermission(security.PermAuditRead))
	g.GET("/export/csv", handleExportAuditCSV, requirePermission(security.PermAuditRead))
	// 清理审计日志为破坏性操作，仅 admin（system:config）可执行
	g.POST("/cleanup", handleCleanupAuditLogs, requirePermission(security.PermSystemConfig))
}

func handleGetAuditIntegrity(c echo.Context) error {
	cont := GetContainer()
	if cont.AuditService == nil {
		return OK(c, map[string]interface{}{"valid": true, "total": 0, "broken_at": []int64{}})
	}
	total, brokenAt, err := cont.AuditService.Integrity()
	if err != nil {
		return InternalError(c, "ERR_AUDIT_INTEGRITY_FAILED")
	}
	return OK(c, map[string]interface{}{
		"valid":            len(brokenAt) == 0,
		"total":            total,
		"broken_at":        brokenAt,
		"hash_chain_valid": len(brokenAt) == 0,
	})
}

func handleExportAuditCSV(c echo.Context) error {
	return handleExportAuditLogs(c)
}

func handleCleanupAuditLogs(c echo.Context) error {
	days := 90
	// Support both "days" and "retention_days" query params (frontend uses retention_days)
	if v := c.QueryParam("retention_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	} else if v := c.QueryParam("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	if days <= 0 {
		return BadRequest(c, "ERR_AUDIT_RETENTION_INVALID")
	}
	cont := GetContainer()
	if cont.AuditService == nil {
		return OK(c, map[string]interface{}{"deleted": 0, "days": days})
	}
	deleted, err := cont.AuditService.Cleanup(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return InternalError(c, "ERR_AUDIT_CLEANUP_FAILED")
	}
	logrus.WithFields(logrus.Fields{"days": days, "deleted": deleted}).Info("Audit logs cleaned up")
	return OK(c, map[string]interface{}{"deleted": deleted, "days": days})
}

// ============================================================================
// Video Extended Routes — 补齐 Python video.py 中缺失的端点
// ============================================================================

func RegisterVideoExtendedRoutes(g *echo.Group) {
	g.GET("/:device_id/stream", handleGetVideoStream, requirePermission(security.PermVideoRead))
	g.POST("/webhook", handleVideoWebhook, requirePermission(security.PermVideoRead))
}

func handleGetVideoStream(c echo.Context) error {
	deviceID := c.Param("device_id")
	return OK(c, map[string]interface{}{
		"device_id":  deviceID,
		"stream_url": "",
		"protocol":   "rtsp",
		"status":     "offline",
	})
}

func handleVideoWebhook(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"received": true})
}

// ============================================================================
// Shadow Extended Routes — 补齐 Python shadow.py 中缺失的端点
// ============================================================================

func RegisterShadowExtendedRoutes(g *echo.Group) {
	g.GET("", handleListShadows, requirePermission(security.PermDeviceRead))
	g.POST("/:device_id/reported", handlePostShadowReported, requirePermission(security.PermDeviceUpdate))
	g.PUT("/:device_id/reported", handlePostShadowReported, requirePermission(security.PermDeviceUpdate))
	g.GET("/:device_id/delta", handleGetShadowDelta, requirePermission(security.PermDeviceRead))
	g.DELETE("/:device_id", handleDeleteShadow, requirePermission(security.PermDeviceUpdate))
}

func handleListShadows(c echo.Context) error {
	cont := GetContainer()
	shadows := make([]map[string]interface{}, 0)
	if cont.ShadowService != nil {
		for _, shadow := range cont.ShadowService.ListShadows() {
			delta := cont.ShadowService.GetDelta(shadow.DeviceID)
			if delta == nil {
				delta = map[string]interface{}{}
			}
			shadows = append(shadows, map[string]interface{}{
				"device_id":      shadow.DeviceID,
				"reported_count": len(shadow.ReportedState),
				"desired_count":  len(shadow.DesiredState),
				"delta_count":    len(delta),
				"version":        shadow.Version,
				"last_updated":   shadow.LastUpdated.UnixMilli(),
			})
		}
	}
	return OK(c, shadows)
}

func handlePostShadowReported(c echo.Context) error {
	deviceID := c.Param("device_id")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	quality := c.QueryParam("quality")
	if quality == "" {
		quality = "good"
	}
	// Frontend sends {reported: {...}, quality: "..."}; accept a flat map as well.
	reported, ok := req["reported"].(map[string]interface{})
	if !ok {
		reported = req
	}
	cont := GetContainer()
	if cont.ShadowService != nil {
		cont.ShadowService.UpdateReported(deviceID, reported)
		if shadow := cont.ShadowService.GetShadow(deviceID); shadow != nil {
			return OK(c, shadowJSON(shadow, cont.ShadowService.GetDelta(deviceID)))
		}
	}
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"reported":  reported,
		"version":   1,
	})
}

func handleGetShadowDelta(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	delta := map[string]interface{}{}
	version := 1
	if cont.ShadowService != nil {
		if d := cont.ShadowService.GetDelta(deviceID); d != nil {
			delta = d
		}
		if shadow := cont.ShadowService.GetShadow(deviceID); shadow != nil {
			version = shadow.Version
		}
	}
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"delta":     delta,
		"version":   version,
	})
}

func handleDeleteShadow(c echo.Context) error {
	deviceID := c.Param("device_id")
	deleted := false
	if cont := GetContainer(); cont.ShadowService != nil {
		deleted = cont.ShadowService.DeleteShadow(deviceID)
	}
	logrus.WithField("device_id", deviceID).Info("Shadow deleted")
	return OK(c, map[string]interface{}{"device_id": deviceID, "deleted": deleted})
}

// ============================================================================
// SCADA Extended Routes — 补齐 Python scada.py 中缺失的端点
// ============================================================================

// scadaProjectPrefix keys one system_settings row per project, so saved screens
// ride the same backup/restore path as the rest of the gateway configuration.
const scadaProjectPrefix = "scada_project:"

// scadaProjectMaxBytes caps a stored project: the editor's widgets are plain
// JSON, so a document past this is a mistake or an attempt to grow the DB.
const scadaProjectMaxBytes = 2 * 1024 * 1024

func scadaProjectKey(name string) string { return scadaProjectPrefix + name }

// validateSCADAProjectName rejects names that would collide with other setting
// keys or that cannot round-trip through a URL path segment.
func validateSCADAProjectName(name string) string {
	switch {
	case name == "":
		return "ERR_SCADA_PROJECT_NAME_INVALID"
	case len(name) > 64, name == ".", name == "..":
		return "ERR_SCADA_PROJECT_NAME_INVALID"
	case strings.ContainsAny(name, "/\\"):
		return "ERR_SCADA_PROJECT_NAME_INVALID"
	}
	return ""
}

// jsonSliceLen counts a decoded JSON array, answering 0 for anything that is
// not one so a malformed project reports no widgets rather than a panic.
func jsonSliceLen(v interface{}) int {
	if arr, ok := v.([]interface{}); ok {
		return len(arr)
	}
	return 0
}

func RegisterSCADAExtendedRoutes(g *echo.Group) {
	g.GET("/projects", handleListSCADAProjects, requirePermission(security.PermSCADAEdit))
	g.GET("/project/:name", handleGetSCADAProject, requirePermission(security.PermSCADAEdit))
	g.POST("/project", handleCreateSCADAProject, requirePermission(security.PermSCADAEdit))
	g.DELETE("/project/:name", handleDeleteSCADAProject, requirePermission(security.PermSCADAEdit))
}

// handleListSCADAProjects used to answer a hardcoded empty list while the
// editor insisted it had saved a project: an operator who rebuilt the screen
// after a reload had no way to tell that nothing had ever reached the server.
func handleListSCADAProjects(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	keys, err := cont.Database.ListSettingKeys(scadaProjectPrefix)
	if err != nil {
		logrus.WithError(err).Error("Failed to list SCADA projects")
		return InternalError(c, "ERR_SCADA_LOAD_FAILED")
	}
	out := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		raw, err := cont.Database.GetSetting(key)
		if err != nil {
			logrus.WithError(err).WithField("key", key).Error("Failed to read SCADA project")
			return InternalError(c, "ERR_SCADA_LOAD_FAILED")
		}
		var doc map[string]interface{}
		if json.Unmarshal([]byte(raw), &doc) != nil {
			// A project that cannot be parsed is still reported. Skipping it would
			// tell the operator "you have no screens" while one is damaged.
			out = append(out, map[string]interface{}{
				"name":      strings.TrimPrefix(key, scadaProjectPrefix),
				"corrupted": true,
			})
			continue
		}
		out = append(out, map[string]interface{}{
			"name":         doc["name"],
			"updated_at":   doc["updated_at"],
			"updated_by":   doc["updated_by"],
			"widget_count": jsonSliceLen(doc["widgets"]),
			"scene_count":  jsonSliceLen(doc["scenes"]),
			"corrupted":    false,
		})
	}
	return OK(c, out)
}

func handleGetSCADAProject(c echo.Context) error {
	name := c.Param("name")
	if err := validateSCADAProjectName(name); err != "" {
		return BadRequest(c, err)
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	raw, err := cont.Database.GetSetting(scadaProjectKey(name))
	if err != nil {
		logrus.WithError(err).WithField("project", name).Error("Failed to read SCADA project")
		return InternalError(c, "ERR_SCADA_LOAD_FAILED")
	}
	if raw == "" {
		// Nothing stored yet is a genuine answer, and exists:false is what lets the
		// editor say "loaded from this gateway" instead of claiming it did.
		return OK(c, map[string]interface{}{
			"name":    name,
			"exists":  false,
			"widgets": []interface{}{},
			"scenes":  []interface{}{},
		})
	}
	var doc map[string]interface{}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		logrus.WithField("project", name).Error("Stored SCADA project is not valid JSON")
		return InternalError(c, "ERR_SCADA_PROJECT_CORRUPT")
	}
	doc["exists"] = true
	return OK(c, doc)
}

func handleCreateSCADAProject(c echo.Context) error {
	var req struct {
		Name    string                   `json:"name"`
		Widgets []map[string]interface{} `json:"widgets"`
		Scenes  []map[string]interface{} `json:"scenes"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if err := validateSCADAProjectName(req.Name); err != "" {
		return BadRequest(c, err)
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	updatedBy := ""
	if user := getUserFromContext(c); user != nil {
		updatedBy = user.Username
	}
	now := time.Now().Format(time.RFC3339)
	raw, err := json.Marshal(map[string]interface{}{
		"name":       req.Name,
		"widgets":    req.Widgets,
		"scenes":     req.Scenes,
		"updated_at": now,
		"updated_by": updatedBy,
	})
	if err != nil {
		return BadRequest(c, "ERR_SCADA_PROJECT_CORRUPT")
	}
	if len(raw) > scadaProjectMaxBytes {
		return BadRequest(c, "ERR_SCADA_PROJECT_TOO_LARGE")
	}
	if err := cont.Database.SetSetting(scadaProjectKey(req.Name), string(raw)); err != nil {
		logrus.WithError(err).WithField("project", req.Name).Error("Failed to persist SCADA project")
		return InternalError(c, "ERR_SCADA_SAVE_FAILED")
	}
	recordAudit(c, "scada_project_save", "scada_project", req.Name, "success", map[string]interface{}{
		"widgets": len(req.Widgets),
		"scenes":  len(req.Scenes),
		"bytes":   len(raw),
	})
	logrus.WithFields(logrus.Fields{"project": req.Name, "bytes": len(raw)}).Info("SCADA project saved")
	return Created(c, map[string]interface{}{
		"name":         req.Name,
		"widget_count": len(req.Widgets),
		"scene_count":  len(req.Scenes),
		"updated_at":   now,
		"bytes":        len(raw),
	})
}

func handleDeleteSCADAProject(c echo.Context) error {
	name := c.Param("name")
	if err := validateSCADAProjectName(name); err != "" {
		return BadRequest(c, err)
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	raw, err := cont.Database.GetSetting(scadaProjectKey(name))
	if err != nil {
		logrus.WithError(err).WithField("project", name).Error("Failed to read SCADA project")
		return InternalError(c, "ERR_SCADA_DELETE_FAILED")
	}
	if raw == "" {
		return NotFound(c, "ERR_SCADA_PROJECT_NOT_FOUND")
	}
	if err := cont.Database.DeleteSetting(scadaProjectKey(name)); err != nil {
		logrus.WithError(err).WithField("project", name).Error("Failed to delete SCADA project")
		return InternalError(c, "ERR_SCADA_DELETE_FAILED")
	}
	recordAudit(c, "scada_project_delete", "scada_project", name, "success", nil)
	logrus.WithField("project", name).Info("SCADA project deleted")
	return OK(c, map[string]interface{}{"deleted": name})
}

// ============================================================================
// Integration Extended Routes — 补齐 Python integration.py 中缺失的端点
// ============================================================================

func RegisterIntegrationExtendedRoutes(g *echo.Group) {
	g.POST("/push-device", handlePushDevice, requirePermission(security.PermIntegrationManage))
	g.POST("/handshake", handleHandshake, requirePermission(security.PermIntegrationManage))
	g.POST("/rpc/execute", handleRpcExecute, requirePermission(security.PermIntegrationManage))
	g.GET("/rpc/history", handleGetRpcHistory, requirePermission(security.PermIntegrationManage))
	g.GET("/health", handleGetIntegrationHealth, requirePermission(security.PermIntegrationManage))
}

// pushDeviceIDPattern matches the device_id format the Python edition enforced
// on /integration/push-device and that ProtoForge normalizes ids to before
// pushing (EDGELITE_DEVICE_ID_PATTERN in its integration client).
var pushDeviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$`)

// handlePushDevice serves two payload shapes on one path, matching the
// Python edition's integration contract ProtoForge is written against:
//   - {"device_id","name","protocol","config","points",...} registers a device
//     (create + driver start; 409 duplicate or driver failure, 422 validation);
//   - {"device_id","data"} forwards one device's readings to every connected
//     northbound platform and reports what the manager measured (that flow used
//     to answer {"pushed":true} having touched no socket, so an operator could
//     believe data had reached the cloud when nothing had been sent at all).
//
// The shape is decided by which keys the JSON body carries, so both callers
// keep their existing contracts.
func handlePushDevice(c echo.Context) error {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 4<<20))
	if err != nil {
		return BadRequest(c, "Invalid request body")
	}
	var probe struct {
		Protocol string                 `json:"protocol"`
		Config   map[string]interface{} `json:"config"`
		Points   []json.RawMessage      `json:"points"`
		Data     map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if probe.Protocol != "" || probe.Config != nil || len(probe.Points) > 0 {
		return handlePushDeviceRegistration(c, body)
	}
	return handleNorthboundPush(c, body)
}

// handlePushDeviceRegistration registers a pushed device, mirroring the Python
// edition's POST /integration/push-device: success returns the created device,
// a duplicate answers 409, an unregistered protocol or invalid payload answers
// 422, and a driver-start failure answers 409 with the constructor error so
// ProtoForge reports "driver failed" instead of blindly re-POSTing.
func handlePushDeviceRegistration(c echo.Context, body []byte) error {
	var req models.DeviceCreate
	if err := json.Unmarshal(body, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	var validationErrors []string
	if req.DeviceID == "" {
		validationErrors = append(validationErrors, "device_id is required")
	} else if !pushDeviceIDPattern.MatchString(req.DeviceID) {
		validationErrors = append(validationErrors, fmt.Sprintf("device_id format invalid: %s", req.DeviceID))
	}
	if req.Name == "" || len(req.Name) > 64 {
		validationErrors = append(validationErrors, "name must be non-empty and <= 64 characters")
	}
	if req.Protocol == "" {
		validationErrors = append(validationErrors, "protocol is required")
	} else if constants.NormalizeProtocol(req.Protocol) == "" {
		validationErrors = append(validationErrors, fmt.Sprintf("protocol '%s' not registered in DriverRegistry", req.Protocol))
	}
	if len(req.Points) == 0 {
		validationErrors = append(validationErrors, "points must be a non-empty list")
	}
	if req.CollectInterval < 0 {
		validationErrors = append(validationErrors, "collect_interval must be >= 1")
	}
	if len(validationErrors) > 0 {
		return c.JSON(http.StatusUnprocessableEntity, map[string]interface{}{
			"detail": map[string]interface{}{
				"error_code": "ERR_DEVICE_CONFIG_INVALID",
				"errors":     validationErrors,
			},
		})
	}
	if req.CollectInterval == 0 {
		req.CollectInterval = 5
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "ERR_INTEG_BACKHAUL_NOT_READY")
	}
	// The auth middleware always injects a user in production; stay nil-safe so
	// a handler invoked outside that chain (tests, future mounts) cannot panic.
	var createdBy string
	if user := getUserFromContext(c); user != nil {
		createdBy = user.UserID
	}
	device, err := cont.DeviceService.Create(&req, createdBy)
	if err != nil {
		logrus.WithError(err).WithField("device_id", req.DeviceID).Error("Push device registration failed")
		errMsg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "duplicate"):
			return Conflict(c, "ERR_DEVICE_ALREADY_EXISTS")
		case strings.Contains(errMsg, "unsupported protocol"):
			return c.JSON(http.StatusUnprocessableEntity, map[string]interface{}{
				"detail": map[string]interface{}{
					"error_code": "ERR_DEVICE_DRIVER_UNAVAILABLE",
					"errors":     []string{err.Error()},
				},
			})
		default:
			// DeviceService.Create already rolled the row back when the driver
			// could not start, so re-POSTing later is safe — ProtoForge expects
			// this failure surfaced as a conflict, not a validation error.
			return c.JSON(http.StatusConflict, map[string]interface{}{
				"detail": map[string]interface{}{
					"error_code": "ERR_DEVICE_CREATE_FAILED",
					"errors":     []string{err.Error()},
				},
			})
		}
	}

	recordAudit(c, "integration_push_device_register", "device", device.DeviceID, "success", map[string]interface{}{"name": device.Name, "protocol": device.Protocol})
	return Created(c, device)
}

// handleNorthboundPush forwards one device's readings to every connected
// northbound platform and reports what the manager measured.
func handleNorthboundPush(c echo.Context, body []byte) error {
	var req struct {
		DeviceID string                 `json:"device_id"`
		Data     map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.DeviceID == "" {
		return BadRequest(c, "ERR_DEVICE_PUSH_INVALID_ID")
	}
	if len(req.Data) == 0 {
		return BadRequest(c, "ERR_DEVICE_PUSH_EMPTY")
	}
	cont := GetContainer()
	mgr := cont.PlatformMgr
	if mgr == nil {
		return ServiceUnavailable(c, "ERR_INTEG_BACKHAUL_NOT_READY")
	}
	if cont.DeviceService != nil {
		if device, _ := cont.DeviceService.Get(req.DeviceID); device == nil {
			return NotFound(c, "ERR_DEVICE_NOT_FOUND")
		}
	}
	connected := mgr.ConnectedNames()
	sort.Strings(connected)
	if len(connected) == 0 {
		return ServiceUnavailable(c, "ERR_INTEG_BACKHAUL_NOT_READY")
	}

	before := mgr.GetAllStats()
	mgr.ForwardTelemetry(req.DeviceID, req.Data)
	after := mgr.GetAllStats()

	delivered, failed := northboundDeliveryReport(connected, before, after)
	out := map[string]interface{}{
		"device_id": req.DeviceID,
		"platforms": connected,
		"delivered": delivered,
		"failed":    failed,
		"pushed":    len(delivered) > 0,
	}
	if len(delivered) == 0 {
		logrus.WithField("device_id", req.DeviceID).Warn("Northbound push reached no platform")
		code := "ERR_DEVICE_PUSH_FAILED"
		return ErrorCode(c, http.StatusBadGateway, code, code)
	}
	recordAudit(c, "integration_push_device", "device", req.DeviceID, "success", map[string]interface{}{
		"delivered": delivered,
		"failed":    failed,
		"points":    len(req.Data),
	})
	return OK(c, out)
}

// northboundDeliveryReport compares per-platform counters around a forward and
// splits the connected platforms by whether the manager actually counted a sent
// message. Anything that did not move the counter -- an error, or no movement
// at all -- is reported as not delivered rather than guessed at.
func northboundDeliveryReport(connected []string, before, after map[string]northbound.PlatformStats) ([]string, []string) {
	delivered := make([]string, 0, len(connected))
	failed := make([]string, 0, len(connected))
	for _, name := range connected {
		if after[name].MessagesSent-before[name].MessagesSent > 0 {
			delivered = append(delivered, name)
			continue
		}
		failed = append(failed, name)
	}
	return delivered, failed
}

// handshakeClient bounds the whole exchange: a platform that accepts the TCP
// connection and then stalls must not pin a request goroutine indefinitely.
var handshakeClient = &http.Client{Timeout: 10 * time.Second}

// handleHandshake asks the peer the caller names and reports what came back.
// It used to mint a session_id from the local clock and answer
// status:"connected" for a peer that was never contacted -- the most damaging
// kind of fake, because every later step trusts that session.
func handleHandshake(c echo.Context) error {
	var req struct {
		CloudURL        string `json:"cloud_url"`
		ProtocolVersion string `json:"protocol_version"`
		GatewayID       string `json:"gateway_id"`
		DeviceID        string `json:"device_id"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	target, err := url.Parse(strings.TrimSpace(req.CloudURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return BadRequest(c, "ERR_INTEG_HANDSHAKE_URL_REQUIRED")
	}
	raw, err := json.Marshal(map[string]interface{}{
		"gateway_id":       req.GatewayID,
		"device_id":        req.DeviceID,
		"protocol_version": req.ProtocolVersion,
		"agent":            "edgelite-gateway",
		"sent_at":          time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return BadRequest(c, "ERR_COMMON_VALIDATION")
	}
	peerReq, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		return BadRequest(c, "ERR_INTEG_HANDSHAKE_URL_REQUIRED")
	}
	peerReq.Header.Set("Content-Type", "application/json")
	resp, err := handshakeClient.Do(peerReq)
	if err != nil {
		logrus.WithError(err).WithField("peer", target.Host).Warn("Integration handshake failed")
		return ErrorCode(c, http.StatusBadGateway, "ERR_INTEG_HANDSHAKE_FAILED", err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrorCode(c, http.StatusBadGateway, "ERR_INTEG_HANDSHAKE_FAILED",
			fmt.Sprintf("peer answered HTTP %d", resp.StatusCode))
	}
	// "connected" here means the peer answered 2xx; session_id is the peer's own
	// value, or null when the peer did not supply one.
	out := map[string]interface{}{
		"status":      "connected",
		"peer_status": resp.StatusCode,
		"session_id":  nil,
	}
	var peer map[string]interface{}
	if json.Unmarshal(body, &peer) == nil {
		out["peer"] = peer
		if sid, ok := peer["session_id"].(string); ok && sid != "" {
			out["session_id"] = sid
		}
	}
	return OK(c, out)
}

// ============================================================================
// RPC 反控 — 真实执行（设备点位写入）+ 内存执行历史
// ============================================================================

type rpcHistoryEntry struct {
	CommandID string                 `json:"command_id"`
	Method    string                 `json:"method"`
	DeviceID  string                 `json:"device_id"`
	Params    map[string]interface{} `json:"params"`
	Success   bool                   `json:"success"`
	Result    interface{}            `json:"result,omitempty"`
	Error     string                 `json:"error,omitempty"`
	ElapsedMS float64                `json:"elapsed_ms"`
	Timestamp int64                  `json:"timestamp"`
}

var (
	rpcHistoryMu    sync.Mutex
	rpcHistoryItems = make([]rpcHistoryEntry, 0, 200)
)

func appendRPCHistory(entry rpcHistoryEntry) {
	rpcHistoryMu.Lock()
	defer rpcHistoryMu.Unlock()
	rpcHistoryItems = append([]rpcHistoryEntry{entry}, rpcHistoryItems...)
	if len(rpcHistoryItems) > 200 {
		rpcHistoryItems = rpcHistoryItems[:200]
	}
}

func handleRpcExecute(c echo.Context) error {
	var req struct {
		Method   string                 `json:"method"`
		DeviceID string                 `json:"device_id"`
		Params   map[string]interface{} `json:"params"`
		Timeout  int                    `json:"timeout"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.DeviceID == "" {
		return BadRequest(c, "ERR_RPC_DEVICE_REQUIRED")
	}

	cont := GetContainer()
	if cont.DeviceService == nil {
		return ServiceUnavailable(c, "Device service not ready")
	}
	device, _ := cont.DeviceService.Get(req.DeviceID)
	if device == nil {
		return NotFound(c, "ERR_DEVICE_NOT_FOUND")
	}

	point, _ := req.Params["point"].(string)
	if point == "" {
		point = req.Method
	}
	if point == "" {
		return BadRequest(c, "ERR_RPC_POINT_REQUIRED")
	}

	entry := rpcHistoryEntry{
		CommandID: fmt.Sprintf("cmd_%d", time.Now().UnixNano()),
		Method:    req.Method,
		DeviceID:  req.DeviceID,
		Params:    req.Params,
		Timestamp: time.Now().Unix(),
	}

	start := time.Now()
	var writeErr error
	if value, ok := req.Params["value"]; ok {
		writeErr = cont.DeviceService.WritePoint(writeActorContext(c), req.DeviceID, &models.WritePointRequest{Point: point, Value: value})
	} else {
		writeErr = fmt.Errorf("value required for point %s", point)
	}
	entry.ElapsedMS = time.Since(start).Seconds() * 1000

	// An RPC command writes a point like any other caller, so it belongs in the
	// same audit trail rather than only in the in-memory RPC history.
	if value, hasValue := req.Params["value"]; hasValue {
		status := "success"
		if writeErr != nil {
			status = "failed"
		}
		recordDeviceWrite(c, req.DeviceID, point, value, status, writeErr)
	}

	if writeErr != nil {
		entry.Success = false
		entry.Error = writeErr.Error()
		appendRPCHistory(entry)
		logrus.WithError(writeErr).WithFields(logrus.Fields{
			"device_id": req.DeviceID,
			"point":     point,
		}).Error("RPC execute failed")
		return OK(c, map[string]interface{}{
			"command_id": entry.CommandID,
			"success":    false,
			"result":     nil,
			"error":      entry.Error,
			"elapsed_ms": round2(entry.ElapsedMS),
		})
	}

	entry.Success = true
	entry.Result = map[string]interface{}{"point": point, "value": req.Params["value"]}
	appendRPCHistory(entry)
	return OK(c, map[string]interface{}{
		"command_id": entry.CommandID,
		"success":    true,
		"result":     entry.Result,
		"error":      "",
		"elapsed_ms": round2(entry.ElapsedMS),
	})
}

func handleGetRpcHistory(c echo.Context) error {
	limit := 50
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rpcHistoryMu.Lock()
	defer rpcHistoryMu.Unlock()
	if limit > len(rpcHistoryItems) {
		limit = len(rpcHistoryItems)
	}
	out := make([]rpcHistoryEntry, limit)
	copy(out, rpcHistoryItems[:limit])
	return OK(c, out)
}

// handleGetIntegrationHealth reports the northbound manager's measured state.
// "healthy" used to be a literal true next to a literal zero connections, so
// the endpoint contradicted itself and the optimistic half won.
func handleGetIntegrationHealth(c echo.Context) error {
	cont := GetContainer()
	mgr := cont.PlatformMgr
	if mgr == nil {
		return ServiceUnavailable(c, "ERR_INTEG_BACKHAUL_NOT_READY")
	}
	connected := mgr.ConnectedNames()
	sort.Strings(connected)
	stats := mgr.GetAllStats()
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	platforms := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		s := stats[name]
		platforms = append(platforms, map[string]interface{}{
			"name":              name,
			"connected":         mgr.IsConnected(name),
			"messages_sent":     s.MessagesSent,
			"messages_failed":   s.MessagesFailed,
			"last_active":       integrationTimeText(s.LastActive),
			"connected_at":      integrationTimeText(s.ConnectedAt),
			"last_test_latency": integrationTimeLatency(s.LastTestLatency),
		})
	}
	return OK(c, map[string]interface{}{
		"healthy":             len(connected) > 0,
		"active_connections":  len(connected),
		"connected_platforms": connected,
		"platforms":           platforms,
	})
}

// integrationTimeText renders a zero time as JSON null: a platform that has
// never sent anything must not look last-active in year 1.
func integrationTimeText(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

// integrationTimeLatency renders an untested latency as null rather than 0ms.
func integrationTimeLatency(v float64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// ============================================================================
// Modbus Slave Extended Routes
// ============================================================================

func RegisterModbusSlaveExtendedRoutes(g *echo.Group) {
	g.GET("/devices/:device_id/ops", handleGetModbusSlaveOps, requirePermission(security.PermSystemConfig))
}

// handleGetModbusSlaveOps used to answer {"operations": []} for any device id,
// which is indistinguishable from an idle slave. The slave is a driver like any
// other, so the alias now serves the counters the gateway actually measured.
func handleGetModbusSlaveOps(c echo.Context) error {
	return handleGetDeviceOps(c)
}

// ============================================================================
// Observability Extended Routes
// ============================================================================

func RegisterObservabilityExtendedRoutes(g *echo.Group) {
	g.GET("/latency", handleGetLatency, requirePermission(security.PermSystemConfig))
	g.GET("/latency/percentiles", handleGetLatencyPercentiles, requirePermission(security.PermSystemConfig))
	g.GET("/latency/histogram", handleGetLatencyHistogram, requirePermission(security.PermSystemConfig))
	g.GET("/alerts/rules", handleGetAlertRules, requirePermission(security.PermSystemConfig))
	g.GET("/alerts/events", handleGetAlertEvents, requirePermission(security.PermSystemConfig))
	g.POST("/alerts/events/:name/:ts/resolve", handleResolveAlertEvent, requirePermission(security.PermSystemConfig))
	g.GET("/traces/:trace_id", handleGetTrace, requirePermission(security.PermSystemConfig))
	g.GET("/traces/stats/:node", handleGetTraceStats, requirePermission(security.PermSystemConfig))
	g.GET("/metrics", handleGetObservabilityMetrics, requirePermission(security.PermSystemConfig))
}

// observabilityLatencySamples pools the latency samples the drivers actually
// measured. Each device keeps a rolling window, so this is a recent-traffic
// view: the responses carry sample_count and window_per_device so a caller can
// tell a quiet gateway from an idle one.
func observabilityLatencySamples() (samples []float64, devicesReporting, windowPerDevice int) {
	mgr := drivers.GetHealthStatsManager()
	if mgr == nil {
		return nil, 0, 0
	}
	all := mgr.GetAllHealthStats()
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := all[id]
		if st == nil {
			continue
		}
		s := st.LatencySamples()
		if len(s) == 0 {
			continue
		}
		devicesReporting++
		samples = append(samples, s...)
		if wc := st.LatencyWindowCapacity(); wc > windowPerDevice {
			windowPerDevice = wc
		}
	}
	return samples, devicesReporting, windowPerDevice
}

// percentileOf takes p on 0..100 from an ascending slice and reports false when
// nothing was measured, so a handler can answer null instead of a 0 that reads
// as "fast" on a gateway with no samples.
func percentileOf(ascending []float64, p float64) (float64, bool) {
	if len(ascending) == 0 {
		return 0, false
	}
	idx := int(float64(len(ascending)-1) * p / 100.0)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ascending) {
		idx = len(ascending) - 1
	}
	return ascending[idx], true
}

// latencyStatsPayload summarises pooled samples. It is separate from the handler
// so the arithmetic is testable without a live driver, and it answers null --
// never 0 -- when nothing was measured: a zero latency reads as a perfect link.
func latencyStatsPayload(samples []float64, devicesReporting, windowPerDevice int) map[string]interface{} {
	resp := map[string]interface{}{
		"avg_ms":            nil,
		"min_ms":            nil,
		"max_ms":            nil,
		"sample_count":      len(samples),
		"devices_reporting": devicesReporting,
		"window_per_device": windowPerDevice,
	}
	if len(samples) == 0 {
		resp["not_collected"] = []string{"avg_ms", "min_ms", "max_ms"}
		return resp
	}
	sum, minMs, maxMs := 0.0, samples[0], samples[0]
	for _, v := range samples {
		sum += v
		if v < minMs {
			minMs = v
		}
		if v > maxMs {
			maxMs = v
		}
	}
	resp["avg_ms"] = sum / float64(len(samples))
	resp["min_ms"] = minMs
	resp["max_ms"] = maxMs
	return resp
}

func handleGetLatency(c echo.Context) error {
	samples, devicesReporting, window := observabilityLatencySamples()
	return OK(c, latencyStatsPayload(samples, devicesReporting, window))
}

// latencyPercentilePayload takes the samples in any order and reports the
// percentiles of the pooled distribution, or null for each one it cannot derive.
func latencyPercentilePayload(samples []float64, devicesReporting, windowPerDevice int) map[string]interface{} {
	ascending := make([]float64, len(samples))
	copy(ascending, samples)
	sort.Float64s(ascending)
	resp := map[string]interface{}{
		"sample_count":      len(samples),
		"devices_reporting": devicesReporting,
		"window_per_device": windowPerDevice,
	}
	notCollected := make([]string, 0, 4)
	for _, spec := range []struct {
		key string
		pct float64
	}{
		{"p50", 50}, {"p90", 90}, {"p95", 95}, {"p99", 99},
	} {
		if v, ok := percentileOf(ascending, spec.pct); ok {
			resp[spec.key] = v
		} else {
			resp[spec.key] = nil
			notCollected = append(notCollected, spec.key)
		}
	}
	if len(notCollected) > 0 {
		resp["not_collected"] = notCollected
	}
	return resp
}

func handleGetLatencyPercentiles(c echo.Context) error {
	samples, devicesReporting, window := observabilityLatencySamples()
	return OK(c, latencyPercentilePayload(samples, devicesReporting, window))
}

// latencyHistogramBoundsMs are fixed so two gateways can be compared; the last
// bucket is open-ended and catches everything above 1000 ms.
var latencyHistogramBoundsMs = []float64{1, 5, 10, 50, 100, 500, 1000}

func latencyHistogramPayload(samples []float64) map[string]interface{} {
	counts := make([]int, len(latencyHistogramBoundsMs)+1)
	for _, v := range samples {
		placed := false
		for i, bound := range latencyHistogramBoundsMs {
			if v <= bound {
				counts[i]++
				placed = true
				break
			}
		}
		if !placed {
			counts[len(counts)-1]++
		}
	}
	buckets := make([]map[string]interface{}, 0, len(counts))
	for i := range counts {
		bucket := map[string]interface{}{"count": counts[i]}
		if i < len(latencyHistogramBoundsMs) {
			bucket["le_ms"] = latencyHistogramBoundsMs[i]
		} else {
			bucket["le_ms"] = nil // the overflow bucket: everything above the last bound
			bucket["gt_ms"] = latencyHistogramBoundsMs[len(latencyHistogramBoundsMs)-1]
		}
		buckets = append(buckets, bucket)
	}
	return map[string]interface{}{
		"buckets":      buckets,
		"sample_count": len(samples),
		"measured":     len(samples) > 0,
	}
}

func handleGetLatencyHistogram(c echo.Context) error {
	samples, _, _ := observabilityLatencySamples()
	return OK(c, latencyHistogramPayload(samples))
}

// handleGetAlertRules reports that this build has no observability alert-rule
// engine. The alarm rules that do exist live in the rule engine and are served
// by GET /rules; inventing a second rule store here would have given the page
// two sources of truth.
func handleGetAlertRules(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"items":     []interface{}{},
		"total":     0,
		"supported": false,
	})
}

// handleGetAlertEvents is the read half of the same absent feature: the events
// this endpoint used to promise are device alarms, served by GET /alarms and
// GET /observability/events.
func handleGetAlertEvents(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"items":     []interface{}{},
		"total":     0,
		"supported": false,
	})
}

// handleResolveAlertEvent used to answer resolved:true for an event no store
// held, so an operator could "clear" an alarm that was never raised and the
// audit trail would show a successful acknowledgement. A mutation cannot
// pretend when there is nothing to mutate.
func handleResolveAlertEvent(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_OBSERVABILITY_ALERT_UNSUPPORTED",
		"observability alert events are not collected; acknowledge real alarms with POST /alarms/{alarm_id}/ack")
}

func handleGetTrace(c echo.Context) error {
	traceID := c.Param("trace_id")
	return OK(c, map[string]interface{}{
		"trace_id":  traceID,
		"spans":     []interface{}{},
		"total":     0,
		"supported": false,
	})
}

func handleGetTraceStats(c echo.Context) error {
	node := c.Param("node")
	return OK(c, map[string]interface{}{
		"node":          node,
		"requests":      nil,
		"errors":        nil,
		"avg_ms":        nil,
		"supported":     false,
		"not_collected": []string{"requests", "errors", "avg_ms"},
	})
}

// pipelineTotals is what the drivers counted across every device. It exists so
// the payload can be built and tested without a live collection loop.
type pipelineTotals struct {
	readAttempts  int64
	readFailures  int64
	writeAttempts int64
	writeFailures int64
	reconnects    int64
}

// observabilityMetricsPayload answers null for anything it cannot derive: the
// previous version printed requests_total 0 and error_rate 0.0 on a gateway that
// measures neither, which reads as "no traffic and no errors" during an outage.
func observabilityMetricsPayload(samples []float64, devicesReporting, windowPerDevice int, totals pipelineTotals) map[string]interface{} {
	notCollected := []string{"requests_total"}
	var errorRate interface{}
	if attempts := totals.readAttempts + totals.writeAttempts; attempts > 0 {
		errorRate = float64(totals.readFailures+totals.writeFailures) / float64(attempts)
	} else {
		notCollected = append(notCollected, "error_rate")
	}
	var avgLatency interface{}
	if len(samples) > 0 {
		sum := 0.0
		for _, v := range samples {
			sum += v
		}
		avgLatency = sum / float64(len(samples))
	} else {
		notCollected = append(notCollected, "avg_latency_ms")
	}
	return map[string]interface{}{
		"requests_total":    nil,
		"error_rate":        errorRate,
		"avg_latency_ms":    avgLatency,
		"read_attempts":     totals.readAttempts,
		"read_failures":     totals.readFailures,
		"write_attempts":    totals.writeAttempts,
		"write_failures":    totals.writeFailures,
		"reconnects":        totals.reconnects,
		"devices_reporting": devicesReporting,
		"sample_count":      len(samples),
		"window_per_device": windowPerDevice,
		"not_collected":     notCollected,
	}
}

// handleGetObservabilityMetrics reports what the collection pipeline measured.
// The keys are named for the traffic that exists -- device read/write attempts,
// which the drivers count -- because no middleware counts HTTP requests in this
// build.
func handleGetObservabilityMetrics(c echo.Context) error {
	samples, devicesReporting, window := observabilityLatencySamples()
	var totals pipelineTotals
	if mgr := drivers.GetHealthStatsManager(); mgr != nil {
		for _, st := range mgr.GetAllHealthStats() {
			if st == nil {
				continue
			}
			ct := st.Counters()
			totals.readAttempts += ct.TotalReads
			totals.readFailures += ct.FailedReads
			totals.writeAttempts += ct.TotalWrites
			totals.writeFailures += ct.FailedWrites
			totals.reconnects += ct.TotalReconnects
		}
	}
	resp := observabilityMetricsPayload(samples, devicesReporting, window, totals)
	cont := GetContainer()
	if cont != nil && cont.EventBus != nil {
		em := cont.EventBus.Metrics()
		resp["events_published"] = em["published"]
		resp["events_delivered"] = em["delivered"]
		resp["events_dropped"] = em["dropped"]
	}
	return OK(c, resp)
}

// ============================================================================
// Scripts Extended Routes
// ============================================================================

func RegisterScriptsExtendedRoutes(g *echo.Group) {
	g.POST("/:id/enable", handleEnableScript, requirePermission(security.PermSystemConfig))
	g.POST("/:id/disable", handleDisableScript, requirePermission(security.PermSystemConfig))
	g.POST("/:id/test", handleTestScriptByID, requirePermission(security.PermSystemConfig))
	g.GET("/:id/logs", handleGetScriptLogs, requirePermission(security.PermSystemConfig))
	g.POST("/:id/submit-review", handleSubmitScriptReview, requirePermission(security.PermSystemConfig))
	g.POST("/:id/approve", handleApproveScript, requirePermission(security.PermSystemConfig))
	g.POST("/:id/reject", handleRejectScript, requirePermission(security.PermSystemConfig))
}

// handleEnableScript and handleDisableScript used to answer {"enabled": true}
// for any id, including one with no row behind it, so the flag a client read
// back was never the flag that was stored. They now write scripts.enabled and
// return the row as it is stored.
func handleEnableScript(c echo.Context) error { return setScriptEnabled(c, true) }

func handleDisableScript(c echo.Context) error { return setScriptEnabled(c, false) }

func setScriptEnabled(c echo.Context, enabled bool) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	if _, err := cont.Database.DB().Exec(
		"UPDATE scripts SET enabled = ?, updated_at = ? WHERE id = ?",
		boolToInt(enabled), time.Now().Format(time.RFC3339), id); err != nil {
		logrus.WithError(err).Error("Update script enabled failed")
		return InternalError(c, "ERR_SCRIPT_UPDATE_FAILED")
	}
	s, err := scanScriptRow(cont.Database.DB().QueryRow(scriptColumnsByID, id))
	if err != nil {
		return NotFound(c, "ERR_SCRIPT_NOT_FOUND")
	}
	return OK(c, scriptToMap(s))
}

// handleTestScriptByID runs stored code in the sandbox on purpose: testing is
// how an editor validates a script that is not enabled yet. Only /execute, the
// operational path, refuses a disabled script.
func handleTestScriptByID(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	s, err := scanScriptRow(cont.Database.DB().QueryRow(scriptColumnsByID, id))
	if err != nil {
		return NotFound(c, "Script not found")
	}
	output, runErr := runScriptCode(s.Language, s.Code, s.TimeoutMs)
	if runErr != nil {
		return OK(c, map[string]interface{}{"id": id, "output": output, "error": runErr.Error()})
	}
	return OK(c, map[string]interface{}{"id": id, "output": output, "error": ""})
}

// handleGetScriptLogs and related handlers are defined in extended_endpoints3.go

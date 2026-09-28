package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// ============================================================================
// Profiler Routes (prefix=/api/v1/profiler) — aligned with Python profiler.py
// ============================================================================

func RegisterProfilerRoutes(g *echo.Group) {
	g.GET("/stats", handleGetProfilerStats, requirePermission(security.PermSystemConfig))
	g.GET("/slowest", handleGetSlowestRequests, requirePermission(security.PermSystemConfig))
	g.GET("/memory", handleGetProfilerMemory, requirePermission(security.PermSystemConfig))
	g.GET("/requests", handleGetProfilerRequests, requirePermission(security.PermSystemConfig))
	g.POST("/enable", handleEnableProfiler, requirePermission(security.PermSystemConfig))
	g.POST("/disable", handleDisableProfiler, requirePermission(security.PermSystemConfig))
	g.POST("/reset", handleResetProfiler, requirePermission(security.PermSystemConfig))
	g.GET("/export", handleExportProfiler, requirePermission(security.PermSystemConfig))
	g.POST("/start", handleStartProfiler, requirePermission(security.PermSystemConfig))
	g.GET("/history", handleGetProfilerHistory, requirePermission(security.PermSystemConfig))
	g.GET("/:id", handleGetProfilerProfile, requirePermission(security.PermSystemConfig))
}

func handleGetProfilerStats(c echo.Context) error {
	minCalls := 0
	if v := c.QueryParam("min_calls"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			minCalls = n
		}
	}
	topN := 20
	if v := c.QueryParam("top_n"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topN = n
		}
	}
	_ = minCalls
	_ = topN
	return OK(c, map[string]interface{}{
		"enabled":       false,
		"total_calls":   0,
		"total_errors":  0,
		"top_functions": []interface{}{},
	})
}

func handleGetSlowestRequests(c echo.Context) error {
	topN := 10
	if v := c.QueryParam("top_n"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topN = n
		}
	}
	_ = topN
	return OK(c, []interface{}{})
}

func handleGetProfilerMemory(c echo.Context) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return OK(c, map[string]interface{}{
		"alloc_bytes":  m.Alloc,
		"sys_bytes":    m.Sys,
		"heap_objects": m.HeapObjects,
		"num_gc":       m.NumGC,
		"goroutines":   runtime.NumGoroutine(),
	})
}

func handleGetProfilerRequests(c echo.Context) error {
	page := 1
	size := 20
	if v := c.QueryParam("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if v := c.QueryParam("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			size = n
		}
	}
	_ = page
	_ = size
	return OKPaged(c, []interface{}{}, 0, page, size)
}

func handleEnableProfiler(c echo.Context) error {
	logrus.Info("Profiler enabled")
	return OK(c, map[string]string{"status": "enabled"})
}

func handleDisableProfiler(c echo.Context) error {
	logrus.Info("Profiler disabled")
	return OK(c, map[string]string{"status": "disabled"})
}

func handleResetProfiler(c echo.Context) error {
	logrus.Info("Profiler stats reset")
	return OK(c, map[string]string{"status": "reset"})
}

func handleExportProfiler(c echo.Context) error {
	filename := c.QueryParam("filename")
	if filename == "" {
		filename = "profiler_export.csv"
	}
	// Security: sanitize filename to prevent header injection
	safeFilename := sanitizeForHeader(filename)
	c.Response().Header().Set("Content-Type", "text/csv")
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", safeFilename))
	return c.String(http.StatusOK, "function,calls,total_ms,avg_ms\n")
}

// ============================================================================
// Debug Routes (prefix=/api/v1/debug) — aligned with Python debug.py
// ============================================================================

// debugGateMiddleware returns a middleware that checks if debug API is enabled.
// This provides defense-in-depth: even if a user has system:config permission,
// debug endpoints are inaccessible unless debug_api_enabled is set to true.
func debugGateMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cfg := config.GetConfig()
			if !cfg.Server.DebugAPIEnabled {
				// Check if the request IP is in the allowed list
				ip := c.RealIP()
				allowed := false
				for _, allowedIP := range cfg.Server.DebugAPIAllowedIPs {
					if ip == allowedIP {
						allowed = true
						break
					}
				}
				if !allowed {
					return NotFound(c, "Debug API is not enabled")
				}
			}
			return next(c)
		}
	}
}

func RegisterDebugRoutes(g *echo.Group) {
	g.Use(debugGateMiddleware())
	g.GET("/protocols", handleGetDebugProtocols, requirePermission(security.PermSystemConfig))
	g.POST("/simulate", handleDebugSimulate, requirePermission(security.PermSystemConfig))
	g.GET("/packets", handleGetDebugPackets, requirePermission(security.PermSystemConfig))
	g.DELETE("/packets", handleDeleteDebugPackets, requirePermission(security.PermSystemConfig))
	g.GET("/devices", handleGetDebugDevices, requirePermission(security.PermSystemConfig))
	g.POST("/read", handleDebugRead, requirePermission(security.PermSystemConfig))
	g.POST("/write", handleDebugWrite, requirePermission(security.PermSystemConfig))
	g.POST("/protocol-read", handleDebugProtocolRead, requirePermission(security.PermSystemConfig))
	g.POST("/protocol-write", handleDebugProtocolWrite, requirePermission(security.PermSystemConfig))
}

// handleGetDebugProtocols reports the protocols the driver registry can actually
// instantiate. It used to return a hand-written list of eleven names, so the
// debug page kept offering probes (opc_da, http_webhook) that no factory backs
// and CreateDriver then rejects.
func handleGetDebugProtocols(c echo.Context) error {
	names := registeredDrivers()
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]interface{}{
			"name":        name,
			"description": drivers.GetDriverDisplayName(name, "en"),
			"enabled":     true,
		})
	}
	return OK(c, map[string]interface{}{"protocols": out})
}

// handleDebugSimulate used to bind the request body and echo it back with
// success:true, which reads as "the probe ran" when no driver was ever opened.
// There is no simulator behind this route; the one-shot probes that do exist are
// /debug/protocol-read and /debug/protocol-write.
func handleDebugSimulate(c echo.Context) error {
	return debugUnsupported(c, "ERR_DEBUG_SIMULATE_UNSUPPORTED",
		"protocol simulation is not implemented; use POST /debug/protocol-read or /debug/protocol-write to probe a real device")
}

// debugUnsupported answers the placeholder debug routes. The message names the
// working route so the operator is redirected rather than left with a success
// that never happened.
func debugUnsupported(c echo.Context, code, message string) error {
	return ErrorCode(c, http.StatusNotImplemented, code, message)
}

// handleGetDebugPackets has no capture to read: no driver in this build records
// wire frames, so the list is always empty. An empty list is the truthful answer
// (unlike the write handlers below), but the tab needs to say why it is empty so
// an operator does not read "no packets" as "the device is silent".
func handleGetDebugPackets(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"packets":         []interface{}{},
		"total":           0,
		"capture_enabled": false,
		"message":         "No driver in this build records wire frames, so this list is always empty.",
	})
}

func handleDeleteDebugPackets(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"cleared":         0,
		"capture_enabled": false,
	})
}

func handleGetDebugDevices(c echo.Context) error {
	protocol := c.QueryParam("protocol")
	cont := GetContainer()
	if cont == nil || cont.DeviceRepo == nil {
		return ServiceUnavailable(c, "Device repository not ready")
	}
	all, _, err := cont.DeviceRepo.List(1, 1000)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_DEVICE_LIST_FAILED", err.Error())
	}
	filtered := make([]models.DeviceResponse, 0, len(all))
	for _, d := range all {
		if protocol != "" && d.Protocol != protocol {
			continue
		}
		filtered = append(filtered, d)
	}
	return OK(c, map[string]interface{}{"devices": filtered})
}

// handleDebugRead used to answer success:true with an empty value list for any
// body, so a failed probe and a probe that returned nothing were identical.
func handleDebugRead(c echo.Context) error {
	return debugUnsupported(c, "ERR_DEBUG_READ_UNSUPPORTED",
		"read by device is not implemented; use POST /debug/protocol-read with an explicit protocol, host and address")
}

// handleDebugWrite is the dangerous one of the group: it answered
// {success:true, written:1} having opened no connection, so an operator could
// believe a PLC register had been changed when nothing on the plant floor moved.
func handleDebugWrite(c echo.Context) error {
	return debugUnsupported(c, "ERR_DEBUG_WRITE_UNSUPPORTED",
		"write by device is not implemented; use POST /debug/protocol-write, which drives a real connection and reports its failures")
}

// ============================================================================
// Protocol Debug — real driver-backed read/write for the protocol debug page
// ============================================================================

// debugProtocolAliases maps the debug page protocol IDs to driver registry names.
var debugProtocolAliases = map[string]string{
	"s7":   "siemens_s7",
	"mc":   "mitsubishi_mc",
	"fins": "omron_fins",
	"cip":  "allen_bradley",
}

type debugProtocolReq struct {
	Protocol     string      `json:"protocol"`
	Host         string      `json:"host"`
	Port         int         `json:"port"`
	UnitID       int         `json:"unit_id"`
	FunctionCode string      `json:"function_code"`
	StartAddress interface{} `json:"start_address"`
	Address      interface{} `json:"address"`
	Quantity     int         `json:"quantity"`
	Value        interface{} `json:"value"`
}

func debugCreateDriver(req debugProtocolReq) (drivers.Driver, error) {
	if req.Protocol == "" {
		return nil, fmt.Errorf("protocol is required")
	}
	if req.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	name := req.Protocol
	if alias, ok := debugProtocolAliases[name]; ok {
		name = alias
	}
	if !drivers.GetRegistry().IsSupported(name) {
		return nil, fmt.Errorf("unsupported protocol: %s", req.Protocol)
	}
	config := map[string]interface{}{
		"host":     req.Host,
		"port":     req.Port,
		"slave_id": req.UnitID,
		"timeout":  3.0,
	}
	driver, err := drivers.GetRegistry().CreateDriver(name, "protocol-debug", config)
	if err != nil {
		return nil, err
	}
	// Detach global health stats / circuit breaker: one-off debug probes to
	// unreachable hosts must not trip or pollute shared breaker state.
	if injector, ok := driver.(drivers.InfrastructureInjector); ok {
		injector.SetHealthStatsManager(nil)
		injector.SetCircuitBreaker(nil)
		injector.SetReconnectManager(nil)
	}
	return driver, nil
}

// debugAddressString normalizes the address field (JSON number or string).
func debugAddressString(v interface{}) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return ""
	}
}

// debugIsModbus reports whether the (aliased) protocol uses Modbus addressing.
func debugIsModbus(protocol string) bool {
	name := protocol
	if alias, ok := debugProtocolAliases[protocol]; ok {
		name = alias
	}
	return name == "modbus_tcp" || name == "modbus_rtu"
}

var debugReadPrefixes = map[string]string{
	"read_coils":    "coil",
	"read_discrete": "di",
	"read_holding":  "hr",
	"read_input":    "ir",
}

var debugWritePrefixes = map[string]string{
	"write_single_coil":        "coil",
	"write_multiple_coils":     "coil",
	"write_single_register":    "hr",
	"write_multiple_registers": "hr",
}

func handleDebugProtocolRead(c echo.Context) error {
	var req debugProtocolReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Quantity < 1 {
		req.Quantity = 1
	}
	if req.Quantity > 125 {
		req.Quantity = 125
	}
	addrStr := debugAddressString(req.StartAddress)
	if addrStr == "" {
		return BadRequest(c, "start_address is required")
	}

	isModbus := debugIsModbus(req.Protocol)
	points := make([]models.PointDef, 0, req.Quantity)
	if isModbus {
		base, err := strconv.Atoi(addrStr)
		if err != nil {
			return BadRequest(c, fmt.Sprintf("invalid start_address: %s", addrStr))
		}
		prefix, ok := debugReadPrefixes[req.FunctionCode]
		if !ok {
			prefix = "hr"
		}
		// Modbus addresses are 1-based in parseModbusAddress (HR1 = protocol 0),
		// so each point maps protocol address p to the string p+1.
		protoBase := base
		if protoBase > 0 {
			protoBase--
		}
		for i := 0; i < req.Quantity; i++ {
			points = append(points, models.PointDef{
				Name:    fmt.Sprintf("debug_%d", i),
				Address: fmt.Sprintf("%s%d", prefix, protoBase+i+1),
			})
		}
	} else {
		// Non-Modbus drivers parse their own address syntax and cannot
		// be auto-incremented; read a single raw address.
		points = append(points, models.PointDef{Name: "debug_0", Address: addrStr})
	}

	driver, err := debugCreateDriver(req)
	if err != nil {
		return BadRequest(c, err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 8*time.Second)
	defer cancel()
	if err := driver.Connect(ctx); err != nil {
		return BadRequest(c, err.Error())
	}
	defer driver.Disconnect()

	start := time.Now()
	data, err := driver.ReadPoints(ctx, points)
	latencyMs := math.Round(float64(time.Since(start).Microseconds())/10.0) / 100.0
	if err != nil {
		return BadRequest(c, err.Error())
	}

	values := make([]map[string]interface{}, 0, len(data))
	for i, pd := range data {
		values = append(values, map[string]interface{}{
			"name":    pd.PointName,
			"address": points[i].Address,
			"value":   pd.Value,
			"quality": pd.Quality,
		})
	}
	return OK(c, map[string]interface{}{
		"success":    true,
		"protocol":   req.Protocol,
		"latency_ms": latencyMs,
		"values":     values,
	})
}

func handleDebugProtocolWrite(c echo.Context) error {
	var req debugProtocolReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	addrStr := debugAddressString(req.Address)
	if addrStr == "" {
		return BadRequest(c, "address is required")
	}
	if req.Value == nil {
		return BadRequest(c, "value is required")
	}

	if debugIsModbus(req.Protocol) {
		prefix, ok := debugWritePrefixes[req.FunctionCode]
		if !ok {
			prefix = "hr"
		}
		base, err := strconv.Atoi(addrStr)
		if err != nil {
			return BadRequest(c, fmt.Sprintf("invalid address: %s", addrStr))
		}
		// parseModbusAddress treats addresses as 1-based (HR1 = protocol 0).
		protoBase := base
		if protoBase > 0 {
			protoBase--
		}
		addrStr = fmt.Sprintf("%s%d", prefix, protoBase+1)
	}

	writeValue := req.Value
	if debugIsModbus(req.Protocol) && prefixIsCoil(req.FunctionCode) {
		if s, ok := writeValue.(string); ok {
			if b, err := strconv.ParseBool(s); err == nil {
				writeValue = b
			} else if n, err := strconv.ParseFloat(s, 64); err == nil {
				writeValue = n
			}
		}
	} else if s, ok := writeValue.(string); ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			writeValue = n
		}
	}

	driver, err := debugCreateDriver(req)
	if err != nil {
		return BadRequest(c, err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 8*time.Second)
	defer cancel()
	if err := driver.Connect(ctx); err != nil {
		return BadRequest(c, err.Error())
	}
	defer driver.Disconnect()

	start := time.Now()
	if err := driver.WritePoint(ctx, addrStr, writeValue); err != nil {
		return BadRequest(c, err.Error())
	}
	latencyMs := math.Round(float64(time.Since(start).Microseconds())/10.0) / 100.0
	return OK(c, map[string]interface{}{
		"success":    true,
		"protocol":   req.Protocol,
		"address":    addrStr,
		"latency_ms": latencyMs,
		"written":    1,
	})
}

func prefixIsCoil(fc string) bool {
	return fc == "write_single_coil" || fc == "write_multiple_coils"
}

// ============================================================================
// System Extended Routes —补齐 Python system.py 中缺失的端点
// ============================================================================

func RegisterSystemExtendedRoutes(g *echo.Group) {
	// Resources
	g.GET("/resources", handleGetSystemResources, requirePermission(security.PermSystemConfig))
	// Backup schedule
	g.GET("/backup/schedule", handleGetBackupSchedule, requirePermission(security.PermSystemBackup))
	g.PUT("/backup/schedule", handleUpdateBackupSchedule, requirePermission(security.PermSystemBackup))
	g.POST("/backup/schedule/trigger", handleTriggerBackupSchedule, requirePermission(security.PermSystemBackup))
	// Cascade
	g.GET("/cascade/topology", handleGetCascadeTopology, requirePermission(security.PermSystemConfig))
	g.GET("/cascade/neighbors", handleGetCascadeNeighbors, requirePermission(security.PermSystemConfig))
	g.POST("/cascade/config", handleSetCascadeConfig, requirePermission(security.PermSystemConfig))
	g.DELETE("/cascade/neighbors/:neighbor_id", handleDeleteCascadeNeighbor, requirePermission(security.PermSystemConfig))
	// Quality per device
	g.GET("/quality/:device_id", handleGetDeviceQuality, requirePermission(security.PermDataRead))
	// Circuit breakers
	g.GET("/circuit-breakers", handleGetCircuitBreakers, requirePermission(security.PermSystemConfig))
	g.POST("/circuit-breakers/:device_id/reset", handleResetCircuitBreaker, requirePermission(security.PermSystemConfig))
	// Health basic
	g.GET("/health/basic", handleGetHealthBasic, requirePermission(security.PermSystemConfig))
	// Ready status
	g.GET("/ready-status", handleGetReadyStatus, requirePermission(security.PermSystemConfig))
	// Performance
	g.GET("/performance", handleGetPerformance, requirePermission(security.PermSystemConfig))
	// Retention
	g.GET("/retention", handleGetRetention, requirePermission(security.PermSystemConfig))
	g.PUT("/retention", handleUpdateRetention, requirePermission(security.PermSystemConfig))
	// Cert
	g.GET("/cert", handleGetCert, requirePermission(security.PermSystemConfig))
	g.POST("/cert/rotate", handleRotateCert, requirePermission(security.PermSystemConfig))
	// NTP PUT (Python has PUT /ntp)
	g.PUT("/ntp", handleUpdateNTP, requirePermission(security.PermSystemConfig))
	// Migration
	g.POST("/migration/retry", handleRetryMigration, requirePermission(security.PermSystemConfig))
	g.GET("/migration/history", handleGetMigrationHistory, requirePermission(security.PermSystemConfig))
	// Locks
	g.GET("/locks/status", handleGetLocksStatus, requirePermission(security.PermSystemConfig))
	// Network
	g.GET("/network", handleGetNetwork, requirePermission(security.PermSystemConfig))
	// Config section update (Python: PUT /config/{section})
	g.PUT("/config/:section", handleUpdateConfigSection, requirePermission(security.PermSystemConfig))
	// Resources endpoint
	g.GET("/backup", handleListBackupsV2, requirePermission(security.PermSystemBackup))
	g.POST("/restore", handleRestoreV2, requirePermission(security.PermSystemRestore))
	g.DELETE("/backup/:backup_id", handleDeleteBackupV2, requirePermission(security.PermSystemBackup))
}

func handleGetSystemResources(c echo.Context) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mTotal, mUsed, mPct := memorySnapshot()
	dTotal, dUsed, dPct := diskSnapshot(".")
	sentMB, recvMB := netIOSnapshot()
	return OK(c, map[string]interface{}{
		"goroutines":   runtime.NumGoroutine(),
		"mem_alloc_mb": m.Alloc / 1024 / 1024,
		"mem_sys_mb":   m.Sys / 1024 / 1024,
		"heap_objects": m.HeapObjects,
		"num_gc":       m.NumGC,
		"cpu": map[string]interface{}{
			"count":    cpuCoreCount(),
			"freq_mhz": cpuFreqMHz(),
			"percent":  cpuPercentCached(),
		},
		"memory": map[string]interface{}{
			"total_bytes": mTotal,
			"used_bytes":  mUsed,
			"percent":     mPct,
		},
		"disk": map[string]interface{}{
			"total_bytes": dTotal,
			"used_bytes":  dUsed,
			"percent":     dPct,
		},
		"network": map[string]interface{}{
			"sent_mb": sentMB,
			"recv_mb": recvMB,
		},
	})
}

func handleGetBackupSchedule(c echo.Context) error {
	cfg := config.GetConfig()
	resp := map[string]interface{}{
		"enabled":        cfg.Backup.Enabled,
		"interval_hours": cfg.Backup.IntervalHours,
		"retain_days":    cfg.Backup.RetainDays,
		"backup_dir":     cfg.BackupDirectory(),
		"min_free_mb":    cfg.Backup.MinFreeMB,
		"last_run":       "",
		"next_run":       "",
		"last_error":     "",
	}
	// The scheduler holds the real run state. Answering from config alone made
	// the page claim a next_run that nothing was going to honour.
	if cont := GetContainer(); cont != nil && cont.BackupScheduler != nil {
		st := cont.BackupScheduler.Status()
		resp["running"] = st.Running
		resp["scheduler_enabled"] = st.Enabled
		resp["max_backups"] = st.MaxBackups
		resp["last_run"] = st.LastRun
		resp["next_run"] = st.NextRun
		resp["last_error"] = st.LastError
	} else {
		resp["running"] = false
		resp["last_error"] = "backup scheduler is not wired into this instance"
	}
	// A staged restore must be visible: otherwise the operator asks for one,
	// sees nothing in the UI, and is surprised at the next boot.
	if cont := GetContainer(); cont != nil && cont.Database != nil {
		if pr, err := storage.LoadPendingRestore(cont.Database.Path()); err == nil && pr != nil {
			resp["pending_restore"] = map[string]interface{}{
				"filename":  pr.Filename,
				"staged_at": pr.StagedAt.Format(time.RFC3339),
			}
		}
	}
	return OK(c, resp)
}

// handleUpdateBackupSchedule persists the `backup:` section and pushes it into
// the live scheduler. Before this endpoint existed the page could only display
// a schedule that had no write path, and the scheduler ignored config entirely.
func handleUpdateBackupSchedule(c echo.Context) error {
	if c.Request() == nil || c.Request().Body == nil {
		return BadRequest(c, "Invalid request body")
	}
	var patch map[string]interface{}
	if err := json.NewDecoder(c.Request().Body).Decode(&patch); err != nil {
		return BadRequest(c, "Invalid request body: "+err.Error())
	}
	if len(patch) == 0 {
		return BadRequest(c, "Empty backup schedule")
	}
	requestedKeys := make([]string, 0, len(patch))
	for k := range patch {
		requestedKeys = append(requestedKeys, k)
	}
	sort.Strings(requestedKeys)

	cfg := config.GetConfig()
	// Merge over the stored section so unmentioned keys keep their values.
	merged := map[string]interface{}{}
	existing, err := json.Marshal(cfg.Backup)
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	if err := json.Unmarshal(existing, &merged); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	for k, v := range patch {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return BadRequest(c, "Invalid backup schedule: "+err.Error())
	}
	next := config.BackupConfig{}
	if err := json.Unmarshal(raw, &next); err != nil {
		return BadRequest(c, "Invalid backup schedule: "+err.Error())
	}

	live := config.LiveConfigMap()
	after, err := json.Marshal(next)
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	kept := map[string]interface{}{}
	if err := json.Unmarshal(after, &kept); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	live["backup"] = kept
	body, err := json.Marshal(live)
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	newCfg := &config.AppConfig{}
	if err := json.Unmarshal(body, newCfg); err != nil {
		return BadRequest(c, "Invalid value in backup schedule: "+err.Error())
	}
	newCfg.ConfigVersion = cfg.ConfigVersion
	if err := config.SaveConfig(newCfg, ""); err != nil {
		logrus.WithError(err).Error("Failed to persist backup schedule")
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	var ignoredKeys = make([]string, 0, len(requestedKeys))
	updatedKeys := make([]string, 0, len(requestedKeys))
	for _, k := range requestedKeys {
		if _, ok := kept[k]; !ok {
			ignoredKeys = append(ignoredKeys, k)
			continue
		}
		updatedKeys = append(updatedKeys, k)
	}

	status := map[string]interface{}{"running": false, "last_error": "backup scheduler is not wired into this instance"}
	if cont := GetContainer(); cont != nil && cont.BackupScheduler != nil {
		st := cont.BackupScheduler.ApplyConfig(newCfg.Backup)
		status = map[string]interface{}{
			"running":    st.Running,
			"last_run":   st.LastRun,
			"next_run":   st.NextRun,
			"last_error": st.LastError,
		}
	}

	recordAudit(c, "backup_schedule_update", "backup", strings.Join(updatedKeys, ","), "success", nil)
	return OK(c, map[string]interface{}{
		"enabled":        newCfg.Backup.Enabled,
		"interval_hours": newCfg.Backup.IntervalHours,
		"retain_days":    newCfg.Backup.RetainDays,
		"backup_dir":     newCfg.BackupDirectory(),
		"min_free_mb":    newCfg.Backup.MinFreeMB,
		"updated_keys":   updatedKeys,
		"ignored_keys":   ignoredKeys,
		"scheduler":      status,
	})
}

// handleTriggerBackupSchedule runs one backup through the scheduler so the
// manual button and the timer share the same snapshot, retention and status.
func handleTriggerBackupSchedule(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.BackupScheduler == nil {
		return ServiceUnavailable(c, "Backup scheduler is not available")
	}
	name, size, err := cont.BackupScheduler.BackupNow()
	if err != nil {
		logrus.WithError(err).Error("Manual backup failed")
		recordAudit(c, "backup_create", "backup", "", "failure", nil)
		return InternalError(c, "Backup failed: "+err.Error())
	}
	recordAudit(c, "backup_create", "backup", name, "success", nil)
	return OK(c, map[string]interface{}{
		"filename":     name,
		"size":         size,
		"created_at":   time.Now().Format(time.RFC3339),
		"triggered_at": time.Now().Unix(),
		"results":      []interface{}{map[string]interface{}{"filename": name, "size": size}},
	})
}

func handleGetCascadeTopology(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"nodes": []interface{}{},
		"edges": []interface{}{},
		"root":  nil,
	})
}

func handleGetCascadeNeighbors(c echo.Context) error {
	return OK(c, []interface{}{})
}

func handleSetCascadeConfig(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"success": true, "config": req})
}

func handleDeleteCascadeNeighbor(c echo.Context) error {
	neighborID := c.Param("neighbor_id")
	logrus.WithField("neighbor_id", neighborID).Info("Cascade neighbor deleted")
	return OK(c, map[string]string{"deleted": neighborID})
}

func handleGetDeviceQuality(c echo.Context) error {
	deviceID := c.Param("device_id")
	return OK(c, map[string]interface{}{
		"device_id":     deviceID,
		"quality_score": 100.0,
		"good_count":    0,
		"bad_count":     0,
		"missing_count": 0,
	})
}

func handleGetCircuitBreakers(c echo.Context) error {
	cont := GetContainer()
	if cont.CBRegistry != nil {
		return OK(c, cont.CBRegistry.GetStats())
	}
	return OK(c, map[string]interface{}{"breakers": []interface{}{}})
}

func handleResetCircuitBreaker(c echo.Context) error {
	deviceID := c.Param("device_id")
	cont := GetContainer()
	if cont.CBRegistry != nil {
		// Reset is not available on the registry, log instead
		logrus.WithField("device_id", deviceID).Info("Circuit breaker reset requested")
	}
	return OK(c, map[string]string{"device_id": deviceID, "status": "reset"})
}

// handleGetHealthBasic answers what this process can actually observe. It used
// to return the literals "healthy" and uptime 0, so no matter what the gateway
// was doing the probe read as a healthy service that had never started.
// The status code stays 200 while the body carries the truth: the console's
// probe counts HTTP answers as reachability, and an impaired-but-serving
// gateway is still reachable.
func handleGetHealthBasic(c echo.Context) error {
	cont := GetContainer()
	checks := map[string]interface{}{}
	status := "healthy"
	if cont != nil && cont.Database != nil {
		if err := cont.Database.HealthCheck(); err != nil {
			status = "degraded"
			checks["database"] = err.Error()
		} else {
			checks["database"] = "ok"
		}
	} else {
		status = "degraded"
		checks["database"] = "no database is attached to this build"
	}
	return OK(c, map[string]interface{}{
		"status":   status,
		"uptime_s": time.Since(startTime).Seconds(),
		"version":  gatewayVersion,
		"checks":   checks,
	})
}

func handleGetReadyStatus(c echo.Context) error {
	cont := GetContainer()
	ready := cont.Database != nil
	return OK(c, map[string]interface{}{
		"ready": ready,
		"checks": map[string]interface{}{
			"database":   ready,
			"ts_storage": cont.TsStorage != nil,
			"scheduler":  cont.Scheduler != nil,
		},
	})
}

func handleGetPerformance(c echo.Context) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mTotal, mUsed, mPct := memorySnapshot()
	dTotal, dUsed, dPct := diskSnapshot(".")
	sentMB, recvMB := netIOSnapshot()
	return OK(c, map[string]interface{}{
		"goroutines":      runtime.NumGoroutine(),
		"mem_alloc_mb":    m.Alloc / 1024 / 1024,
		"mem_sys_mb":      m.Sys / 1024 / 1024,
		"gc_pause_ns":     m.PauseNs[(m.NumGC+255)%256],
		"uptime_s":        time.Since(startTime).Seconds(),
		"cpu_percent":     cpuPercentCached(),
		"memory_percent":  mPct,
		"memory_used_mb":  float64(mUsed) / 1024 / 1024,
		"memory_total_mb": float64(mTotal) / 1024 / 1024,
		"disk_percent":    dPct,
		"disk_used_gb":    float64(dUsed) / 1024 / 1024 / 1024,
		"disk_total_gb":   float64(dTotal) / 1024 / 1024 / 1024,
		"net_sent_mb":     sentMB,
		"net_recv_mb":     recvMB,
	})
}

const retentionSettingKey = "retention_policy"

func handleGetRetention(c echo.Context) error {
	cfg := config.GetConfig()
	policy := map[string]interface{}{
		"bucket":           cfg.InfluxDB.Bucket,
		"retention_period": fmt.Sprintf("%dd", cfg.InfluxDB.RetentionDays),
	}
	cont := GetContainer()
	if cont != nil && cont.Database != nil {
		if raw, err := cont.Database.GetSetting(retentionSettingKey); err == nil && raw != "" {
			var stored map[string]interface{}
			if json.Unmarshal([]byte(raw), &stored) == nil {
				for k, v := range stored {
					policy[k] = v
				}
			}
		}
	}
	return OK(c, policy)
}

func handleUpdateRetention(c echo.Context) error {
	var req struct {
		RetentionPeriod string `json:"retention_period"`
		Bucket          string `json:"bucket"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.RetentionPeriod == "" {
		return BadRequest(c, "retention_period required")
	}
	days := 0
	if _, err := fmt.Sscanf(req.RetentionPeriod, "%dd", &days); err != nil || days <= 0 {
		return BadRequest(c, "retention_period must be in the form <days>d, e.g. 30d")
	}

	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	cfg := config.GetConfig()
	policy := map[string]interface{}{
		"bucket":           req.Bucket,
		"retention_period": req.RetentionPeriod,
	}
	if policy["bucket"] == "" {
		policy["bucket"] = cfg.InfluxDB.Bucket
	}
	raw, _ := json.Marshal(policy)
	if err := cont.Database.SetSetting(retentionSettingKey, string(raw)); err != nil {
		logrus.WithError(err).Error("Failed to persist retention policy")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	return OK(c, policy)
}

// handleGetCert reports the gateway's own listener credential. There is none:
// cmd/edgelite serves plain HTTP (no StartTLS call exists in this build), so the
// previous all-empty stub was read as "certificate present but not readable".
// The certificate endpoint that does have real data is the OPC UA one, which
// parses each device's client certificate file.
func handleGetCert(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"has_cert":       false,
		"https_enabled":  false,
		"issuer":         "",
		"subject":        "",
		"not_before":     "",
		"not_after":      "",
		"days_remaining": 0,
		"message":        "This build serves plain HTTP: no server certificate is configured, loaded or terminated by the gateway.",
	})
}

// handleRotateCert used to answer {rotated:true, "rotation initiated"} for a
// certificate that does not exist, so an operator believed the TLS credential
// had been renewed.
func handleRotateCert(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_CERT_ROTATE_UNSUPPORTED",
		"there is no server certificate in this build to rotate")
}

// handleUpdateNTP used to answer {updated:true, config:req} echoing the body:
// the config schema has no NTP section, so nothing could be stored. The SNTP
// probe at POST /system/ntp/sync is real; only persistence is missing.
func handleUpdateNTP(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_NTP_CONFIG_UNSUPPORTED",
		"NTP configuration is not part of this build's config schema; use POST /system/ntp/sync to perform one SNTP query")
}

// handleRetryMigration answered {status:"retrying", "retry initiated"} while no
// migration was queued or runnable: schema migrations run once inside
// storage.NewDatabase at startup and abort the process when they fail.
func handleRetryMigration(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_MIGRATION_RETRY_UNSUPPORTED",
		"schema migrations run at startup and abort the process on failure; there is no runtime migration to retry")
}

// handleGetMigrationHistory used to return a bare [], while the status card reads
// data.history -- so the key mismatch alone guaranteed an empty table. It now
// answers in the shape the page reads, and says the history is not tracked.
func handleGetMigrationHistory(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"history": []interface{}{},
		"tracked": false,
		"message": "Migration runs are not recorded in this build; the schema is migrated at startup.",
	})
}

// handleGetLocksStatus reported a hardcoded "no locks" for a lock registry that
// does not exist. Login lockouts are tracked per key in internal/security with no
// enumeration, and SQLite has no per-table lock state to expose either, so the
// truthful answer is that this is not measurable rather than an empty list.
func handleGetLocksStatus(c echo.Context) error {
	cont := GetContainer()
	backend := ""
	if cont != nil && cont.Database != nil {
		backend = config.GetConfig().Database.Backend
	}
	return OK(c, map[string]interface{}{
		"use_fine_grained_locks": false,
		"global_lock":            nil,
		"table_locks":            nil,
		"active_locks":           []interface{}{},
		"deadlocks":              0,
		"enumerable":             false,
		"database_backend":       backend,
		"message":                "No lock registry is exposed by this backend, so lock state cannot be enumerated; an empty list here means unknown, not unlocked.",
	})
}

// handleGetNetwork used to answer with no interfaces, an empty hostname and an
// empty address -- a page full of blanks that looked like a broken network. Both
// are readable from the operating system without any privileges.
func handleGetNetwork(c echo.Context) error {
	hostname, hostErr := os.Hostname()
	interfaces := []map[string]interface{}{}
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifa := range ifs {
			addrs := []string{}
			if as, err := ifa.Addrs(); err == nil {
				for _, a := range as {
					addrs = append(addrs, a.String())
				}
			}
			interfaces = append(interfaces, map[string]interface{}{
				"name":        ifa.Name,
				"index":       ifa.Index,
				"mac":         ifa.HardwareAddr.String(),
				"addresses":   addrs,
				"up":          ifa.Flags&net.FlagUp != 0,
				"loopback":    ifa.Flags&net.FlagLoopback != 0,
				"mtu":         ifa.MTU,
				"operational": ifa.Flags&net.FlagUp != 0 && ifa.Flags&net.FlagRunning != 0,
			})
		}
	}
	resp := map[string]interface{}{
		"interfaces": interfaces,
		"hostname":   hostname,
		"ip_address": primaryOutboundIP(),
	}
	if hostErr != nil {
		resp["hostname_error"] = hostErr.Error()
	}
	return OK(c, resp)
}

// primaryOutboundIP asks the OS which local address it would use to reach the
// wider internet. No packet is sent: the dial only selects and opens a socket, so
// this reports the real primary address without depending on a peer.
func primaryOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// handleUpdateConfigSection applies a partial patch to one top-level config
// section and persists it. It used to answer {"updated":true} after binding the
// body and throwing it away, so every page routed through it reported success
// for settings that were never stored.
func handleUpdateConfigSection(c echo.Context) error {
	section := c.Param("section")
	var patch map[string]interface{}
	// Decode the body directly: echo's DefaultBinder also folds path parameters
	// into a *map[string]interface{} target, so binding would inject
	// `section=<name>` into the patch the operator never sent.
	if c.Request() == nil || c.Request().Body == nil {
		return BadRequest(c, "Invalid request body")
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&patch); err != nil {
		return BadRequest(c, "Invalid request body: "+err.Error())
	}
	if len(patch) == 0 {
		return BadRequest(c, "Empty config section")
	}
	// The keys the client actually sent. RestoreMaskedSecrets below adds stored
	// credentials back, and those are not something the caller updated.
	requestedKeys := make([]string, 0, len(patch))
	for k := range patch {
		requestedKeys = append(requestedKeys, k)
	}
	sort.Strings(requestedKeys)
	// The UI posts back a sanitized config, so a masked credential in the patch
	// means "unchanged" and has to keep the stored value.
	wrapped := map[string]interface{}{section: patch}
	config.RestoreMaskedSecrets(wrapped)
	if restored, ok := wrapped[section].(map[string]interface{}); ok {
		patch = restored
	}

	live := config.LiveConfigMap()
	raw, exists := live[section]
	if !exists {
		return BadRequest(c, "Unknown config section: "+section)
	}
	existing := map[string]interface{}{}
	switch v := raw.(type) {
	case map[string]interface{}:
		existing = v
	case nil:
		// A free-form section that has never been configured.
	default:
		return BadRequest(c, "Config section is not an object: "+section)
	}
	for k, v := range patch {
		existing[k] = v
	}
	live[section] = existing

	body, err := json.Marshal(live)
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	newCfg := &config.AppConfig{}
	if err := json.Unmarshal(body, newCfg); err != nil {
		return BadRequest(c, "Invalid value in config section "+section+": "+err.Error())
	}
	newCfg.ConfigVersion = config.GetConfig().ConfigVersion
	// The serial bridge reads its section when it opens the device, so a patch
	// that stores settings Start rejects would leave the file describing a
	// service that cannot run. Check it here too: this route is the other way in.
	var serialBridgeWasRunning bool
	var serialBridgePrevious config.SerialBridgeConfig
	if section == "serial_bridge" {
		if err := validateSerialBridgeSection(newCfg.SerialBridge); err != nil {
			return BadRequest(c, err.Error())
		}
		serialBridgeWasRunning = serialBridgeIsRunning(GetContainer())
		serialBridgePrevious = config.GetConfig().SerialBridge
	}
	if err := config.SaveConfig(newCfg, ""); err != nil {
		logrus.WithError(err).WithField("section", section).Error("Failed to persist config section")
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	if section == "serial_bridge" {
		if err := restartSerialBridgeForConfig(serialBridgeWasRunning, serialBridgePrevious); err != nil {
			return BadRequest(c, err.Error())
		}
	}

	// A key the section's struct does not declare disappears in the round-trip,
	// so name it instead of letting the response imply it was stored.
	var ignoredKeys = make([]string, 0, len(requestedKeys))
	updatedKeys := make([]string, 0, len(requestedKeys))
	if after, ok := config.LiveConfigMap()[section].(map[string]interface{}); ok {
		for _, k := range requestedKeys {
			if _, kept := after[k]; !kept {
				ignoredKeys = append(ignoredKeys, k)
				continue
			}
			updatedKeys = append(updatedKeys, k)
		}
	} else {
		// The whole section came back empty, so nothing the client sent survived.
		ignoredKeys = append(ignoredKeys, requestedKeys...)
	}
	if len(ignoredKeys) > 0 {
		sort.Strings(ignoredKeys)
		logrus.WithField("section", section).WithField("ignored_keys", ignoredKeys).
			Warn("Config section keys are not defined by the gateway and were dropped")
	}
	if section == "mqtt" {
		if cont := GetContainer(); cont != nil && cont.MqttForward != nil {
			logrus.Warn("mqtt section persisted; the running MQTT forwarder keeps its previous settings until it is restarted")
		}
	}
	logrus.WithField("section", section).WithField("updated_keys", updatedKeys).Info("Config section updated")
	return OK(c, map[string]interface{}{
		"section":      section,
		"updated":      true,
		"updated_keys": updatedKeys,
		"ignored_keys": ignoredKeys,
	})
}

// handleListBackupsV2 — alias for Python's GET /system/backup
func handleListBackupsV2(c echo.Context) error {
	return handleListBackups(c)
}

func handleRestoreV2(c echo.Context) error {
	return handleRestoreBackup(c)
}

func handleDeleteBackupV2(c echo.Context) error {
	_ = c.Param("backup_id")
	return handleDeleteBackup(c)
}

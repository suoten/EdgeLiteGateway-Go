package api

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// startTime records when the process started, used as a fallback for system info.
var startTime = time.Now()

// RegisterSystemRoutes registers system management API routes.
func RegisterSystemRoutes(g *echo.Group) {
	g.GET("/info", handleSystemInfo)
	g.GET("/status", handleSystemStatus, requirePermission(security.PermSystemConfig))
	g.GET("/stats", handleGetSystemStats, requirePermission(security.PermSystemConfig))
	g.GET("/ai-monitor", handleGetAIMonitor, requirePermission(security.PermSystemConfig))
	g.GET("/health", handleSystemHealth)
	g.POST("/self-test", handleSystemSelfTest, requirePermission(security.PermSystemConfig))
	g.GET("/config", handleGetConfig, requirePermission(security.PermSystemConfig))
	g.PUT("/config", handleUpdateConfig, requirePermission(security.PermSystemConfig))
	g.POST("/config/reload", handleReloadConfig, requirePermission(security.PermSystemConfig))
	g.GET("/config/sanitized", handleGetSanitizedConfig, requirePermission(security.PermSystemConfig))
	g.GET("/config/versions", handleListConfigVersions, requirePermission(security.PermSystemConfig))
	g.POST("/config/rollback", handleRollbackConfig, requirePermission(security.PermSystemConfig))

	// Backup & Restore
	g.GET("/backup/list", handleListBackups, requirePermission(security.PermSystemBackup))
	g.POST("/backup", handleCreateBackup, requirePermission(security.PermSystemBackup))
	g.POST("/backup/restore", handleRestoreBackup, requirePermission(security.PermSystemRestore))
	g.GET("/backup/:filename/download", handleDownloadBackup, requirePermission(security.PermSystemBackup))
	g.DELETE("/backup/:filename", handleDeleteBackup, requirePermission(security.PermSystemBackup))

	// NTP
	g.GET("/ntp/status", handleGetNTPStatus, requirePermission(security.PermSystemConfig))
	g.POST("/ntp/sync", handleNTPSync, requirePermission(security.PermSystemConfig))

	// Migration
	g.GET("/migration/status", handleGetMigrationStatus, requirePermission(security.PermSystemConfig))

	// Logs
	g.GET("/logs/level", handleGetLogLevel, requirePermission(security.PermSystemLogs))
	g.PUT("/logs/level", handleSetLogLevel, requirePermission(security.PermSystemLogs))

	// Services
	g.GET("/services", handleListServices, requirePermission(security.PermSystemConfig))
	g.GET("/services/list", handleListServices, requirePermission(security.PermSystemConfig))
	g.POST("/services/:name/:action", handleServiceAction, requirePermission(security.PermSystemConfig))
}

func handleSystemInfo(c echo.Context) error {
	cont := GetContainer()
	info := map[string]interface{}{
		"version":    gatewayVersion,
		"go_version": runtime.Version(),
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"cpus":       runtime.NumCPU(),
		"start_time": startTime.Format(time.RFC3339),
	}
	if cont.SystemService != nil {
		sysInfo := cont.SystemService.GetSystemInfo()
		if uptime, ok := sysInfo["uptime_s"]; ok {
			info["uptime_s"] = uptime
		}
		if st, ok := sysInfo["start_time"]; ok {
			info["start_time"] = st
		}
	} else {
		info["uptime_s"] = time.Since(startTime).Seconds()
	}
	return OK(c, info)
}

func handleSystemStatus(c echo.Context) error {
	cont := GetContainer()
	status := map[string]interface{}{
		"status":  "healthy",
		"version": gatewayVersion,
	}

	if cont.SystemService != nil {
		sysInfo := cont.SystemService.GetSystemInfo()
		status["uptime_s"] = sysInfo["uptime_s"]
		status["uptime"] = sysInfo["uptime_s"] // alias for frontend compatibility
	}

	// ── Flat fields for frontend Dashboard compatibility ──
	// The frontend SystemStatus interface expects flat fields, not nested objects.
	var deviceTotal, deviceOnline, ruleTotal, ruleEnabled, alarmFiring, collectTaskCount int

	// Device counts
	if cont.DeviceRepo != nil {
		devs, _, derr := cont.DeviceRepo.List(1, 1000)
		if derr == nil {
			deviceTotal = len(devs)
			for _, d := range devs {
				if d.Status == "online" {
					deviceOnline++
				}
			}
		}
	}
	status["device_total"] = deviceTotal
	status["device_online"] = deviceOnline

	// Rule counts
	if cont.RuleRepo != nil {
		rules, _, rerr := cont.RuleRepo.List(1, 1000, "")
		if rerr == nil {
			ruleTotal = len(rules)
			for _, r := range rules {
				if r.Enabled {
					ruleEnabled++
				}
			}
		}
	}
	status["rule_total"] = ruleTotal
	status["rule_enabled"] = ruleEnabled

	// Alarm firing count
	if cont.AlarmRepo != nil {
		alarms, _, aerr := cont.AlarmRepo.List(models.AlarmFilter{Status: "firing"}, 1, 1000)
		if aerr == nil {
			alarmFiring = len(alarms)
		}
	}
	status["alarm_firing"] = alarmFiring

	// Collect task count
	if cont.Scheduler != nil {
		schedStats := cont.Scheduler.Stats()
		if ac, ok := schedStats["active_collectors"]; ok {
			if acInt, ok := ac.(int); ok {
				collectTaskCount = acInt
			}
		}
		status["scheduler"] = schedStats
		status["collect_task_count"] = collectTaskCount
	}

	if cont.Database != nil {
		status["database"] = map[string]interface{}{
			"backend": "sqlite",
			"healthy": true,
		}
	}

	if cont.TsStorage != nil {
		status["influxdb"] = map[string]interface{}{
			"backend":  "sqlite_fallback",
			"healthy":  cont.TsStorage.CheckHealth(),
			"fallback": cont.TsStorage.IsUsingFallback(),
		}
	}

	if cont.WSManager != nil {
		status["websocket"] = cont.WSManager.GetStats()
	}

	if cont.EventBus != nil {
		status["event_bus"] = cont.EventBus.Metrics()
	}

	if cont.CBRegistry != nil {
		status["circuit_breakers"] = cont.CBRegistry.GetStats()
	}

	if cont.Evaluator != nil {
		status["rule_evaluator"] = cont.Evaluator.Stats()
	}

	// Resource usage (flat fields for frontend compatibility)
	cpuPct := cpuPercentCached()
	mTotal, mUsed, mPct := memorySnapshot()
	dTotal, dUsed, dPct := diskSnapshot(".")
	status["cpu_percent"] = cpuPct
	status["memory_total"] = mTotal
	status["memory_used"] = mUsed
	status["memory_percent"] = mPct
	status["disk_total"] = dTotal
	status["disk_used"] = dUsed
	status["disk_percent"] = dPct

	return OK(c, status)
}

// handleGetSystemStats returns flat counters for the large-screen dashboard.
func handleGetSystemStats(c echo.Context) error {
	cont := GetContainer()
	stats := map[string]interface{}{
		"devices":     0,
		"online":      0,
		"points":      0,
		"alarms":      0,
		"collections": 0,
		"uptime":      0.0,
	}

	if cont.DeviceRepo != nil {
		devs, _, err := cont.DeviceRepo.List(1, 1000)
		if err == nil {
			online := 0
			for _, d := range devs {
				if d.Status == "online" {
					online++
				}
			}
			stats["devices"] = len(devs)
			stats["online"] = online
		}
	}

	if cont.AlarmRepo != nil {
		alarms, _, err := cont.AlarmRepo.List(models.AlarmFilter{Status: "firing"}, 1, 1000)
		if err == nil {
			stats["alarms"] = len(alarms)
		}
	}

	if cont.Scheduler != nil {
		ss := cont.Scheduler.Stats()
		if tp, ok := asInt64(ss["total_points"]); ok {
			stats["points"] = tp
		}
		if tc, ok := asInt64(ss["total_collects"]); ok {
			stats["collections"] = tc
		}
	}

	if cont.SystemService != nil {
		sysInfo := cont.SystemService.GetSystemInfo()
		if us, ok := asFloat64(sysInfo["uptime_s"]); ok {
			stats["uptime"] = round2(us / 86400)
		}
	}

	return OK(c, stats)
}

func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func asFloat64(v interface{}) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

func handleSystemHealth(c echo.Context) error {
	cont := GetContainer()
	components := map[string]string{}
	allHealthy := true

	if cont.Database != nil {
		components["database"] = "healthy"
	} else {
		components["database"] = "unavailable"
		allHealthy = false
	}

	if cont.TsStorage != nil {
		if cont.TsStorage.CheckHealth() {
			components["ts_storage"] = "healthy"
		} else {
			components["ts_storage"] = "unhealthy"
			allHealthy = false
		}
	}

	if cont.Scheduler != nil {
		components["scheduler"] = "running"
	} else {
		components["scheduler"] = "stopped"
	}

	if cont.EventBus != nil && cont.EventBus.IsStarted() {
		components["event_bus"] = "running"
	} else {
		components["event_bus"] = "stopped"
	}

	if cont.WSManager != nil {
		components["websocket"] = "running"
	}

	if cont.MqttForward != nil {
		if cont.MqttForward.IsConnected() {
			components["mqtt_forwarder"] = "connected"
		} else {
			components["mqtt_forwarder"] = "disconnected"
		}
	}

	status := "healthy"
	if !allHealthy {
		status = "degraded"
	}

	return OK(c, map[string]interface{}{
		"status":     status,
		"components": components,
		"timestamp":  time.Now().Format(time.RFC3339),
	})
}

func handleGetConfig(c echo.Context) error {
	// Security: return sanitized config to avoid leaking sensitive fields
	// (passwords, secret keys, tokens) in API responses.
	return OK(c, config.GetSanitizedConfig())
}

// operationalConfigPath is the file this instance reads at boot. An instance
// started with --config must persist to that file, not to the packaged default
// it never loads, or every UI edit vanishes on the next restart.
func operationalConfigPath() string {
	if p := config.LoadedConfigPath(); p != "" {
		return p
	}
	return filepath.Join("configs", "config.yaml")
}

func handleUpdateConfig(c echo.Context) error {
	// Bind to a map first so masked/blanked sensitive values can be restored
	// from the live config before decoding into the typed struct. The UI edits
	// a sanitized config; saving it must not replace real credentials with
	// masks or wipe them.
	var bodyMap map[string]interface{}
	if err := c.Bind(&bodyMap); err != nil {
		return BadRequest(c, "Invalid config body")
	}
	if bodyMap == nil {
		bodyMap = map[string]interface{}{}
	}
	config.RestoreMaskedSecrets(bodyMap)
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return BadRequest(c, "Invalid config body")
	}
	var newCfg config.AppConfig
	if err := json.Unmarshal(body, &newCfg); err != nil {
		return BadRequest(c, "Invalid config body")
	}

	// An instance owns exactly one config file: the one it started with. The
	// packaged default is only a fallback, otherwise every UI edit on a
	// deployment that boots with --config /etc/edgelite/config.yaml is written
	// somewhere nothing reads and disappears on the next restart.
	loaded := config.LoadedConfigPath()
	configPath := c.QueryParam("path")
	if configPath == "" {
		configPath = operationalConfigPath()
	}

	// Security: prevent path traversal — writes stay inside configs/, except for
	// the file this instance actually loaded (checked below).
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return BadRequest(c, "Invalid config path")
	}
	absConfigsDir, err := filepath.Abs("configs")
	if err != nil {
		absConfigsDir, _ = filepath.Abs(".")
	}
	// Use filepath.Rel for more robust containment check instead of string prefix
	rel, err := filepath.Rel(absConfigsDir, absPath)
	outsideConfigs := err != nil || strings.HasPrefix(rel, "..") || rel == ".."
	// The file this instance already reads is legitimate even when it lives
	// outside configs/; an explicit ?path= still is not.
	onOwnConfig := false
	if loaded != "" {
		if absLoaded, lerr := filepath.Abs(loaded); lerr == nil {
			onOwnConfig = absPath == absLoaded
		}
	}
	if outsideConfigs && !onOwnConfig {
		logrus.WithField("path", configPath).Warn("Config path traversal attempt blocked")
		return BadRequest(c, "Config path must be within configs/ directory")
	}

	if err := config.SaveConfig(&newCfg, configPath); err != nil {
		logrus.WithError(err).Error("Failed to save config")
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	if cont := GetContainer(); cont != nil && cont.Database != nil {
		updatedBy := "system"
		if u := getUserFromContext(c); u != nil {
			updatedBy = u.Username
		}
		summary := "系统配置更新"
		if s, ok := bodyMap["config_version"].(string); ok && s != "" {
			summary = "系统配置更新 (v" + s + ")"
		}
		if _, err := cont.Database.DB().Exec(
			"INSERT INTO config_versions (config_json, created_at, created_by, change_summary) VALUES (?, ?, ?, ?)",
			string(body), time.Now().Format(time.RFC3339), updatedBy, summary); err != nil {
			logrus.WithError(err).Warn("Failed to record config version")
		}
	}

	return OK(c, map[string]string{"message": "Config updated successfully"})
}

func handleReloadConfig(c echo.Context) error {
	newCfg, changed, err := config.ReloadConfig("")
	if err != nil {
		logrus.WithError(err).Error("Config reload failed")
		return InternalError(c, "ERR_SYSTEM_CONFIG_RELOAD_FAILED")
	}

	return OK(c, map[string]interface{}{
		"config_version": newCfg.ConfigVersion,
		"changed_keys":   changed,
	})
}

func handleGetSanitizedConfig(c echo.Context) error {
	return OK(c, config.GetSanitizedConfig())
}

func handleListConfigVersions(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	db := cont.Database.DB()

	seedConfigVersionBaseline(db)

	rows, err := db.Query("SELECT version, config_json, created_at, created_by, change_summary FROM config_versions ORDER BY version DESC LIMIT 100")
	if err != nil {
		logrus.WithError(err).Error("List config versions failed")
		return InternalError(c, "ERR_SYSTEM_CONFIG_VERSIONS_FAILED")
	}
	defer rows.Close()

	history := []map[string]interface{}{}
	var latestVersion int
	var latestJSON, latestAt, latestBy string
	for rows.Next() {
		var version int
		var configJSON, createdAt, createdBy, changeSummary *string
		if err := rows.Scan(&version, &configJSON, &createdAt, &createdBy, &changeSummary); err != nil {
			logrus.WithError(err).Warn("Failed to scan config version row")
			continue
		}
		if len(history) == 0 {
			latestVersion = version
			if configJSON != nil {
				latestJSON = *configJSON
			}
			if createdAt != nil {
				latestAt = *createdAt
			}
			if createdBy != nil {
				latestBy = *createdBy
			}
		}
		entry := map[string]interface{}{
			"version":        version,
			"updated_at":     derefString(createdAt),
			"updated_by":     derefString(createdBy),
			"change_summary": derefString(changeSummary),
		}
		history = append(history, entry)
	}
	if err := rows.Err(); err != nil {
		logrus.WithError(err).Warn("Failed to iterate config versions")
		return InternalError(c, "ERR_SYSTEM_CONFIG_VERSIONS_ITER_FAILED")
	}

	changedKeys := []string{}
	if latestJSON != "" {
		changedKeys = diffTopLevelKeys(latestJSON, config.GetSanitizedConfig())
	}

	current := map[string]interface{}{
		"version":      latestVersion,
		"updated_at":   latestAt,
		"updated_by":   latestBy,
		"changed_keys": changedKeys,
	}
	return OK(c, map[string]interface{}{"current": current, "history": history})
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// diffTopLevelKeys compares stored config JSON with the live config map and
// returns the top-level keys that differ.
func diffTopLevelKeys(configJSON string, live interface{}) []string {
	var stored map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &stored); err != nil {
		return []string{}
	}
	liveMap, ok := live.(map[string]interface{})
	if !ok {
		data, err := json.Marshal(live)
		if err != nil {
			return []string{}
		}
		if err := json.Unmarshal(data, &liveMap); err != nil {
			return []string{}
		}
	}
	keys := []string{}
	seen := map[string]bool{}
	for k, v := range stored {
		if !reflect.DeepEqual(normalizeJSONValue(v), normalizeJSONValue(liveMap[k])) && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k := range liveMap {
		if _, ok := stored[k]; !ok && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	sort.Strings(keys)
	return keys
}

// normalizeJSONValue converts json.Number/float64 mixtures to plain strings
// so DeepEqual is stable across marshal round-trips.
func normalizeJSONValue(v interface{}) interface{} {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, val := range t {
			out[k] = normalizeJSONValue(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = normalizeJSONValue(val)
		}
		return out
	default:
		return v
	}
}

// seedConfigVersionBaseline records the current config as the initial version
// when no version history exists yet.
func seedConfigVersionBaseline(db *sql.DB) {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM config_versions").Scan(&count); err != nil || count > 0 {
		return
	}
	cfgJSON, err := json.Marshal(config.GetSanitizedConfig())
	if err != nil {
		return
	}
	_, _ = db.Exec("INSERT INTO config_versions (config_json, created_at, created_by, change_summary) VALUES (?, ?, ?, ?)",
		string(cfgJSON), time.Now().Format(time.RFC3339), "system", "初始版本")
}

// configVersionYAML renders a stored config version as YAML for the diff view.
func configVersionYAML(configJSON string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return ""
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return ""
	}
	return string(out)
}

func handleGetConfigVersionDiff(c echo.Context) error {
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil {
		return BadRequest(c, "Invalid version number")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	var configJSON string
	err = cont.Database.DB().QueryRow("SELECT config_json FROM config_versions WHERE version = ?", version).Scan(&configJSON)
	if err != nil {
		return NotFound(c, "Config version not found")
	}
	return OK(c, map[string]interface{}{
		"version":    version,
		"old_config": configVersionYAML(configJSON),
		"new_config": configVersionYAML(mustJSON(config.GetSanitizedConfig())),
	})
}

func mustJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func handleRollbackConfigVersion(c echo.Context) error {
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil {
		return BadRequest(c, "Invalid version number")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	var configJSON string
	err = cont.Database.DB().QueryRow("SELECT config_json FROM config_versions WHERE version = ?", version).Scan(&configJSON)
	if err != nil {
		return NotFound(c, "Config version not found")
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	yamlOut, err := yaml.Marshal(m)
	if err != nil {
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}
	configPath := operationalConfigPath()
	if err := os.WriteFile(configPath, yamlOut, 0o600); err != nil {
		logrus.WithError(err).Error("Config rollback write failed")
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	by := "system"
	if u := getUserFromContext(c); u != nil {
		by = u.Username
	}
	_, _ = cont.Database.DB().Exec("INSERT INTO config_versions (config_json, created_at, created_by, change_summary) VALUES (?, ?, ?, ?)",
		configJSON, time.Now().Format(time.RFC3339), by, fmt.Sprintf("回滚到 v%d", version))

	return OK(c, map[string]interface{}{
		"rolled_back":      true,
		"version":          version,
		"requires_restart": true,
	})
}

func handleRollbackConfig(c echo.Context) error {
	versionStr := c.QueryParam("version")
	if versionStr == "" {
		return BadRequest(c, "version parameter required")
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		return BadRequest(c, "Invalid version number")
	}

	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	var configJSON string
	err = cont.Database.DB().QueryRow("SELECT config_json FROM config_versions WHERE version = ?", version).Scan(&configJSON)
	if err != nil {
		return NotFound(c, "Config version not found")
	}

	return OK(c, map[string]interface{}{
		"version": version,
		"config":  configJSON,
		"message": "Config rollback retrieved (apply manually)",
	})
}

// --- Backup ---

func handleListBackups(c echo.Context) error {
	cfg := config.GetConfig()
	backupDir := cfg.BackupDirectory()

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return OK(c, []interface{}{})
		}
		return InternalError(c, "ERR_SYSTEM_BACKUP_LIST_FAILED")
	}

	backups := []map[string]interface{}{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// A snapshot's WAL/SHM sidecars can end up in this directory, and offering
		// them for restore or download is misleading: they are not databases.
		if strings.HasSuffix(entry.Name(), "-wal") || strings.HasSuffix(entry.Name(), "-shm") || strings.HasSuffix(entry.Name(), "-journal") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, map[string]interface{}{
			"backup_id":  strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			"filename":   entry.Name(),
			"size":       info.Size(),
			"created_at": info.ModTime().Format(time.RFC3339),
		})
	}

	return OK(c, backups)
}

func handleCreateBackup(c echo.Context) error {
	cfg := config.GetConfig()
	cont := GetContainer()
	if cont == nil || (cont.BackupScheduler == nil && cont.Database == nil) {
		return ServiceUnavailable(c, "Backup is unavailable: this instance has no database handle")
	}
	backupDir := cfg.BackupDirectory()
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return InternalError(c, "ERR_SYSTEM_BACKUP_DIR_FAILED")
	}

	// Prefer the scheduler so the manual button updates last_run and applies the
	// same free-space guard and retention rules as the timer.
	if cont.BackupScheduler != nil {
		name, size, err := cont.BackupScheduler.BackupNow()
		if err != nil {
			logrus.WithError(err).Error("Backup creation failed")
			recordAudit(c, "backup_create", "backup", "", "failure", nil)
			return InternalError(c, "Backup failed: "+err.Error())
		}
		logrus.WithField("backup", name).Info("Backup created")
		recordAudit(c, "backup_create", "backup", name, "success", nil)
		return OK(c, map[string]interface{}{
			"filename":   name,
			"size":       size,
			"created_at": time.Now().Format(time.RFC3339),
		})
	}

	backupName := uniqueBackupName(backupDir, time.Now())
	backupPath := filepath.Join(backupDir, backupName)
	// VACUUM INTO, not a file copy: with WAL the newest committed rows can still
	// be in `<db>-wal`, so copying the main file produced a backup that restored
	// an older database while the API reported success.
	size, err := cont.Database.BackupTo(c.Request().Context(), backupPath)
	if err != nil {
		logrus.WithError(err).Error("Backup creation failed")
		recordAudit(c, "backup_create", "backup", "", "failure", nil)
		return InternalError(c, "Backup failed: "+err.Error())
	}

	logrus.WithField("backup", backupName).Info("Backup created")
	recordAudit(c, "backup_create", "backup", backupName, "success", nil)
	return OK(c, map[string]interface{}{
		"filename":   backupName,
		"size":       size,
		"created_at": time.Now().Format(time.RFC3339),
	})
}

// uniqueBackupName keeps two backups taken in the same second from overwriting
// each other.
func uniqueBackupName(dir string, at time.Time) string {
	stamp := at.Format("20060102_150405")
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("edgelite_backup_%s.db", stamp)
		if i > 0 {
			name = fmt.Sprintf("edgelite_backup_%s-%02d.db", stamp, i)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
	}
	return fmt.Sprintf("edgelite_backup_%s-%d.db", stamp, 100)
}

func handleRestoreBackup(c echo.Context) error {
	type RestoreRequest struct {
		Filename string `json:"filename"`
		Cancel   bool   `json:"cancel"`
	}
	var req RestoreRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Cancel {
		cont := GetContainer()
		if cont == nil || cont.Database == nil {
			return ServiceUnavailable(c, "Restore is unavailable: this instance has no database handle")
		}
		if err := storage.ClearPendingRestore(cont.Database.Path()); err != nil {
			return InternalError(c, "Cancel failed: "+err.Error())
		}
		recordAudit(c, "backup_restore_cancel", "backup", "", "success", nil)
		return OK(c, map[string]interface{}{"staged": false, "cancelled": true})
	}

	if req.Filename == "" {
		return BadRequest(c, "filename required")
	}

	// Security: prevent path traversal — reject any path separators or ..
	if strings.Contains(req.Filename, "/") || strings.Contains(req.Filename, "\\") || strings.Contains(req.Filename, "..") {
		logrus.WithField("filename", req.Filename).Warn("Path traversal attempt in backup restore")
		return BadRequest(c, "Invalid filename")
	}

	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Restore is unavailable: this instance has no database handle")
	}
	dbPath := cont.Database.Path()

	cfg := config.GetConfig()
	backupDir := cfg.BackupDirectory()
	backupPath := filepath.Join(backupDir, req.Filename)

	// Security: verify the resolved path is within backupDir using filepath.Rel
	absBackupPath, err := filepath.Abs(backupPath)
	if err != nil {
		return BadRequest(c, "Invalid filename")
	}
	absBackupDir, err := filepath.Abs(backupDir)
	if err != nil {
		absBackupDir, _ = filepath.Abs("data/backups")
	}
	relPath, err := filepath.Rel(absBackupDir, absBackupPath)
	if err != nil || strings.HasPrefix(relPath, "..") || relPath == ".." {
		logrus.WithField("path", absBackupPath).Warn("Path traversal attempt in backup restore")
		return BadRequest(c, "Invalid filename")
	}

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return NotFound(c, "Backup file not found")
	}
	// A restore is applied at boot, so a bad snapshot would take the gateway
	// down before it can serve the UI that reports the problem.
	if err := storage.VerifySQLiteFile(backupPath); err != nil {
		logrus.WithError(err).WithField("filename", req.Filename).Error("Rejected restore of an unusable backup")
		return BadRequest(c, "Backup is not a usable database: "+err.Error())
	}
	checksum, err := storage.FileChecksum(backupPath)
	if err != nil {
		return InternalError(c, "Checksum failed: "+err.Error())
	}
	if err := storage.StageRestore(dbPath, storage.PendingRestore{
		Filename:   req.Filename,
		BackupPath: absBackupPath,
		Checksum:   checksum,
		StagedAt:   time.Now(),
	}); err != nil {
		return InternalError(c, "Stage restore failed: "+err.Error())
	}

	logrus.WithFields(logrus.Fields{
		"filename": req.Filename,
		"database": dbPath,
	}).Warn("Backup restore staged; it is applied on the next start")
	recordAudit(c, "backup_restore", "backup", req.Filename, "staged", nil)
	return OK(c, map[string]interface{}{
		"filename":     req.Filename,
		"staged":       true,
		"applied":      false,
		"effective_on": "restart",
		"checksum":     checksum,
		"message":      "Restore staged. The gateway applies it while starting up and snapshots the current database first. Restart to take effect.",
	})
}

func handleDeleteBackup(c echo.Context) error {
	filename := c.Param("filename")
	if filename == "" {
		// DELETE /backup/:backup_id (extended route) delegates here — accept both param names
		filename = c.Param("backup_id")
	}
	if filename == "" {
		return BadRequest(c, "filename required")
	}

	// Security: prevent path traversal — reject any path separators or ..
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || strings.Contains(filename, "..") {
		logrus.WithField("filename", filename).Warn("Path traversal attempt in backup delete")
		return BadRequest(c, "Invalid filename")
	}

	cfg := config.GetConfig()
	backupDir := cfg.BackupDirectory()
	backupPath := filepath.Join(backupDir, filename)

	// Security: verify the resolved path is within backupDir using filepath.Rel
	absBackupPath, err := filepath.Abs(backupPath)
	if err != nil {
		return BadRequest(c, "Invalid filename")
	}
	absBackupDir, err := filepath.Abs(backupDir)
	if err != nil {
		absBackupDir, _ = filepath.Abs("data/backups")
	}
	relPath, err := filepath.Rel(absBackupDir, absBackupPath)
	if err != nil || strings.HasPrefix(relPath, "..") || relPath == ".." {
		logrus.WithField("path", absBackupPath).Warn("Path traversal attempt in backup delete")
		return BadRequest(c, "Invalid filename")
	}

	if err := os.Remove(backupPath); err != nil {
		if os.IsNotExist(err) {
			return NotFound(c, "Backup file not found")
		}
		return InternalError(c, "ERR_SYSTEM_BACKUP_DELETE_FAILED")
	}

	return OK(c, map[string]string{"deleted": filename})
}

func handleDownloadBackup(c echo.Context) error {
	filename := c.Param("filename")
	if filename == "" {
		return BadRequest(c, "filename required")
	}

	// Security: prevent path traversal — reject any path separators or ..
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || strings.Contains(filename, "..") {
		logrus.WithField("filename", filename).Warn("Path traversal attempt in backup download")
		return BadRequest(c, "Invalid filename")
	}

	cfg := config.GetConfig()
	backupDir := cfg.BackupDirectory()
	backupPath := filepath.Join(backupDir, filename)

	// Security: verify the resolved path is within backupDir using filepath.Rel
	absBackupPath, err := filepath.Abs(backupPath)
	if err != nil {
		return BadRequest(c, "Invalid filename")
	}
	absBackupDir, err := filepath.Abs(backupDir)
	if err != nil {
		absBackupDir, _ = filepath.Abs("data/backups")
	}
	relPath, err := filepath.Rel(absBackupDir, absBackupPath)
	if err != nil || strings.HasPrefix(relPath, "..") || relPath == ".." {
		logrus.WithField("path", absBackupPath).Warn("Path traversal attempt in backup download")
		return BadRequest(c, "Invalid filename")
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NotFound(c, "Backup file not found")
		}
		return InternalError(c, "ERR_SYSTEM_BACKUP_LIST_FAILED")
	}

	recordAudit(c, "backup_download", "backup", filename, "success", nil)
	c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(info.Size(), 10))
	return c.Attachment(backupPath, filename)
}

// --- NTP ---

// ntpServer is the only SNTP target this build knows about; there is no NTP
// section in config.yaml, so POST /system/ntp cannot change it.
const ntpServer = "pool.ntp.org:123"

// ntpLastSync records the outcome of the most recent SNTP query run by this
// process. There is no background time daemon, so this is the whole story:
// nil until someone calls POST /system/ntp/sync, and it is lost on restart.
var ntpLastSync struct {
	sync.Mutex
	result map[string]interface{}
}

func recordNTPSync(payload map[string]interface{}) {
	ntpLastSync.Lock()
	ntpLastSync.result = payload
	ntpLastSync.Unlock()
}

func handleGetNTPStatus(c echo.Context) error {
	ntpLastSync.Lock()
	result := ntpLastSync.result
	ntpLastSync.Unlock()

	payload := map[string]interface{}{
		// No daemon, no config: the gateway never adjusts the clock on its
		// own, so this endpoint can only report the last manual query.
		"enabled":      false,
		"server":       "pool.ntp.org",
		"sync_status":  "never_queried",
		"current_time": time.Now().Format(time.RFC3339),
		"synced":       false,
		"offset_ms":    0,
		"last_sync":    "",
		"message":      "No SNTP query has been run by this process yet; POST /system/ntp/sync performs one.",
	}
	if result != nil {
		for k, v := range result {
			payload[k] = v
		}
		payload["enabled"] = false
		payload["current_time"] = time.Now().Format(time.RFC3339)
		if synced, _ := result["synced"].(bool); synced {
			payload["sync_status"] = "queried_ok"
		} else {
			payload["sync_status"] = "queried_failed"
		}
	}
	return OK(c, payload)
}

func handleNTPSync(c echo.Context) error {
	// Query NTP server via SNTP (Simple Network Time Protocol)
	conn, err := net.DialTimeout("udp", ntpServer, 5*time.Second)
	if err != nil {
		logrus.WithError(err).Warn("NTP sync failed: cannot connect to NTP server")
		payload := map[string]interface{}{
			"synced":  false,
			"server":  "pool.ntp.org",
			"message": fmt.Sprintf("NTP sync failed: %v", err),
		}
		recordNTPSync(payload)
		return OK(c, payload)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send SNTP request (48 bytes, first byte = 0x1B for client mode)
	req := make([]byte, 48)
	req[0] = 0x1B
	if _, err := conn.Write(req); err != nil {
		payload := map[string]interface{}{
			"synced":  false,
			"server":  "pool.ntp.org",
			"message": fmt.Sprintf("NTP send failed: %v", err),
		}
		recordNTPSync(payload)
		return OK(c, payload)
	}

	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		payload := map[string]interface{}{
			"synced":  false,
			"server":  "pool.ntp.org",
			"message": fmt.Sprintf("NTP read failed: %v", err),
		}
		recordNTPSync(payload)
		return OK(c, payload)
	}

	// Parse NTP timestamp from bytes 40-47
	// Seconds (4 bytes big-endian) + Fraction (4 bytes)
	secs := binary.BigEndian.Uint32(resp[40:44])
	frac := binary.BigEndian.Uint32(resp[44:48])

	// NTP epoch is 1900-01-01, Unix epoch is 1970-01-01
	// Difference: 2208988800 seconds
	const ntpEpochOffset = 2208988800
	unixSecs := int64(secs) - ntpEpochOffset
	unixNsecs := (int64(frac) * 1e9) >> 32
	ntpTime := time.Unix(unixSecs, unixNsecs)

	offset := time.Since(ntpTime).Seconds()

	logrus.WithFields(logrus.Fields{
		"server":     "pool.ntp.org",
		"offset_sec": offset,
	}).Info("NTP sync completed")

	payload := map[string]interface{}{
		"synced":    true,
		"server":    "pool.ntp.org",
		"offset_ms": offset * 1000,
		"last_sync": ntpTime.Format(time.RFC3339),
		// The query only measures the drift; nothing sets the system clock.
		"message": "SNTP query succeeded; the measured offset is reported but the system clock was not modified.",
	}
	recordNTPSync(payload)
	return OK(c, payload)
}

// --- Migration ---

func handleGetMigrationStatus(c echo.Context) error {
	return OK(c, map[string]interface{}{
		// storage.Migrate runs once at startup and keeps no history table, so
		// there is no per-run status to report after boot. "untracked" is the
		// honest answer; the previous "idle" implied a watcher that does not exist.
		"current_status": "untracked",
		"last_updated":   nil,
		"last_failure":   nil,
		"tracked":        false,
		"message":        "Schema migrations run at startup only and are not recorded, so no run status is available.",
	})
}

// --- Logs ---

func handleGetLogLevel(c echo.Context) error {
	level := logrus.GetLevel()
	return OK(c, map[string]string{
		"level": level.String(),
	})
}

func handleSetLogLevel(c echo.Context) error {
	type LevelRequest struct {
		Level string `json:"level"`
	}
	var req LevelRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	level, err := logrus.ParseLevel(req.Level)
	if err != nil {
		return BadRequest(c, "Invalid log level")
	}

	logrus.SetLevel(level)
	logrus.WithField("level", req.Level).Info("Log level changed")

	return OK(c, map[string]string{"level": req.Level})
}

// --- Services ---

func handleListServices(c echo.Context) error {
	cont := GetContainer()
	services := []map[string]interface{}{}

	if cont.Scheduler != nil {
		stats := cont.Scheduler.Stats()
		statusVal := "disabled"
		if v, ok := stats["started"].(bool); ok && v {
			statusVal = "running"
		}
		services = append(services, map[string]interface{}{
			"name":           "collect_scheduler",
			"display_name":   "collect_scheduler",
			"description":    "Data collection scheduler",
			"icon":           "power",
			"category":       "builtin",
			"state":          statusVal,
			"config_section": "scheduler",
			"dependencies":   []interface{}{},
			"use_cases":      []string{"device_data_collection"},
			"stats":          stats,
		})
	}

	if cont.EventBus != nil {
		statusVal := "disabled"
		if cont.EventBus.IsStarted() {
			statusVal = "running"
		}
		services = append(services, map[string]interface{}{
			"name":           "event_bus",
			"display_name":   "event_bus",
			"description":    "Internal event bus",
			"icon":           "swap",
			"category":       "builtin",
			"state":          statusVal,
			"config_section": "event_bus",
			"dependencies":   []interface{}{},
			"use_cases":      []string{"event_distribution"},
			"stats":          cont.EventBus.Metrics(),
		})
	}

	if cont.WSManager != nil {
		services = append(services, map[string]interface{}{
			"name":           "websocket",
			"display_name":   "websocket",
			"description":    "WebSocket real-time push",
			"icon":           "radio",
			"category":       "builtin",
			"state":          "running",
			"config_section": "websocket",
			"dependencies":   []interface{}{},
			"use_cases":      []string{"realtime_push"},
			"stats":          cont.WSManager.GetStats(),
		})
	}

	if cont.MqttForward != nil {
		statusVal := "disabled"
		if cont.MqttForward.IsConnected() {
			statusVal = "running"
		}
		services = append(services, map[string]interface{}{
			"name":           "mqtt_forwarder",
			"display_name":   "mqtt_forwarder",
			"description":    "MQTT message forwarder",
			"icon":           "swap",
			"category":       "builtin",
			"state":          statusVal,
			"config_section": "mqtt",
			"dependencies":   []interface{}{},
			"use_cases":      []string{"mqtt_forward"},
			"stats":          cont.MqttForward.GetStats(),
		})
	}

	// MCP Server
	// The same predicate the /mcp/* routes refuse on. This used to ask whether a
	// registry object existed, which is always true once the tools are
	// registered, so the service list showed MCP running while the MCP page and
	// /services/mcp_server/status showed it switched off.
	mcpState := "disabled"
	if mcpServiceEnabled() {
		mcpState = "running"
	}
	services = append(services, map[string]interface{}{
		"name":           "mcp_server",
		"display_name":   "mcp_server",
		"description":    "Model Context Protocol server",
		"icon":           "puzzle",
		"category":       "integration",
		"state":          mcpState,
		"config_section": "mcp",
		"dependencies":   []interface{}{},
		"use_cases":      []string{"ai_tool_integration"},
	})

	// Grafana monitoring
	grafanaState := "disabled"
	cont.ServiceEnabledMu.RLock()
	if enabled, ok := cont.ServiceEnabledMap["grafana"]; ok && enabled {
		grafanaState = "enabled"
	}
	cont.ServiceEnabledMu.RUnlock()
	services = append(services, map[string]interface{}{
		"name":           "grafana",
		"display_name":   "grafana",
		"description":    "Grafana dashboard monitoring",
		"icon":           "chart",
		"category":       "integration",
		"state":          grafanaState,
		"config_section": "grafana",
		"dependencies":   []interface{}{},
		"use_cases":      []string{"dashboard_monitoring"},
	})

	// Merge user-enabled state for services whose actual state is "disabled"
	// but the user has toggled them on via the UI (map first, then persisted).
	cont.ServiceEnabledMu.RLock()
	for i, svc := range services {
		name, _ := svc["name"].(string)
		enabled, ok := cont.ServiceEnabledMap[name]
		if !ok {
			enabled = serviceEnabled(cont, name)
		}
		if enabled {
			curState, _ := svc["state"].(string)
			if curState == "disabled" {
				services[i]["state"] = "enabled"
			}
		}
	}
	cont.ServiceEnabledMu.RUnlock()

	return OK(c, map[string]interface{}{"services": services})
}

func handleServiceAction(c echo.Context) error {
	name := c.Param("name")
	action := c.Param("action")
	if name == "" || action == "" {
		return BadRequest(c, "Service name and action required")
	}

	switch action {
	case "restart":
		logrus.WithField("service", name).Info("Service restart requested")
	case "start":
		logrus.WithField("service", name).Info("Service start requested")
	case "stop":
		logrus.WithField("service", name).Info("Service stop requested")
	default:
		return BadRequest(c, "Invalid action. Use start, stop, or restart.")
	}

	return OK(c, map[string]string{
		"service": name,
		"action":  action,
		"message": "Action submitted",
	})
}

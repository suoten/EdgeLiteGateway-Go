package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// Continue Scripts handlers (split from extended_endpoints2.go due to size)

// handleGetScriptLogs used to answer an empty page for every id, which read as
// "this script has never run" on a gateway that had run it a thousand times.
// Nothing records script executions at all, so the honest answer is that the
// capability is absent rather than a list that is always empty.
func handleGetScriptLogs(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_SCRIPT_LOGS_UNSUPPORTED",
		"script executions are not recorded: /scripts/:id/execute returns its output to the caller and keeps no history, so there are no logs to page through")
}

// The review endpoints used to echo back the status they were asked about
// ("pending_review", "approved", "rejected") while storing nothing, so a client
// could "approve" a script and the row would not change. scripts has no review
// column and no execution path consults one, so the whole state machine is
// reported as unavailable instead of pretending to pass.
func handleSubmitScriptReview(c echo.Context) error {
	return scriptReviewUnsupported(c)
}

func handleApproveScript(c echo.Context) error {
	return scriptReviewUnsupported(c)
}

func handleRejectScript(c echo.Context) error {
	return scriptReviewUnsupported(c)
}

func scriptReviewUnsupported(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_SCRIPT_REVIEW_UNSUPPORTED",
		"there is no script review state machine: scripts have no review column, no approver and no approval check before execution, so approving would be a label with no effect")
}

// ============================================================================
// Log Aggregation Extended Routes
// ============================================================================

func RegisterLogAggregationExtendedRoutes(g *echo.Group) {
	g.GET("/query", handleLogAggQuery, requirePermission(security.PermSystemLogs))
	g.GET("/stats", handleLogAggStats, requirePermission(security.PermSystemLogs))
	g.GET("/filters", handleLogAggFilters, requirePermission(security.PermSystemLogs))
	g.PUT("/level", handleLogAggSetLevel, requirePermission(security.PermSystemLogs))
	g.POST("/archive", handleLogAggArchive, requirePermission(security.PermSystemLogs))
	g.POST("/cleanup", handleLogAggCleanup, requirePermission(security.PermSystemLogs))
}

func handleLogAggQuery(c echo.Context) error {
	// This used to answer {logs: [], total: 0} no matter what was asked, so the
	// log page that called it always showed an empty list. It now serves the same
	// filtered buffer as GET /logs, with limit/offset paging on top -- and the same
	// 503 when there is no buffer behind it.
	rows, err := logRowsOrRefuse(c)
	if err != nil {
		return err
	}
	limit := 100
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return OK(c, map[string]interface{}{
		"logs":   rows[offset:end],
		"total":  len(rows),
		"limit":  limit,
		"offset": offset,
	})
}

func handleLogAggStats(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.LogAggregator == nil {
		return ServiceUnavailable(c, "ERR_LOG_AGGREGATOR_NOT_READY: log aggregation is not running")
	}

	// The histogram covers the in-memory ring only, not the rotated files: the
	// response says so, because the counters drop to zero on a restart and that has
	// to read as "the buffer is new" rather than "the gateway stopped logging".
	entries := cont.LogAggregator.Query("", "", time.Time{}, 0)
	counts := map[string]int64{}
	for _, e := range entries {
		counts[strings.ToUpper(e.Level)]++
	}
	order := []string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL"}
	inOrder := make(map[string]bool, len(order))
	for _, level := range order {
		inOrder[level] = true
	}
	for level := range counts {
		if !inOrder[level] {
			order = append(order, level)
		}
	}
	stats := make([]map[string]interface{}, 0, len(order))
	for _, level := range order {
		stats = append(stats, map[string]interface{}{"level": level, "count": counts[level]})
	}
	return OK(c, map[string]interface{}{
		"total_logs": len(entries),
		"stats":      stats,
		"scope":      "memory_buffer",
		"buffer":     cont.LogAggregator.GetStats(),
	})
}

// handleLogAggFilters lists the query parameters GET /logs actually honours. It
// used to advertise "module", which no filter reads, and omit device_id, which
// does -- an operator following the hint would get unfiltered results.
func handleLogAggFilters(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"fields": []string{"level", "source", "device_id", "search", "start_time", "end_time"},
	})
}

// handleLogAggSetLevel changes the runtime log level, the same effect as
// PUT /system/logs/level. It used to answer {updated: true} without touching
// logrus, so DEBUG stayed off while the UI reported success.
func handleLogAggSetLevel(c echo.Context) error {
	var req struct {
		Level string `json:"level"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	level, err := logrus.ParseLevel(strings.TrimSpace(req.Level))
	if err != nil {
		return BadRequest(c, "ERR_LOG_INVALID_LEVEL: "+strings.TrimSpace(req.Level))
	}
	logrus.SetLevel(level)
	logrus.WithField("level", level.String()).Info("Log level changed")
	return OK(c, map[string]interface{}{"updated": true, "level": strings.ToUpper(level.String())})
}

// handleLogAggArchive reports honestly: the aggregator rotates by size and there
// is no archiver, so answering {archived: true} with a generated file name
// advertised an artifact that was never written to disk.
func handleLogAggArchive(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_LOG_ARCHIVE_UNSUPPORTED",
		"log archiving is not implemented; logs rotate by size in place")
}

// handleLogAggCleanup likewise has no age-based retention to run: rotation keeps
// a fixed number of size-bounded backups.
func handleLogAggCleanup(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_LOG_CLEANUP_UNSUPPORTED",
		"age-based log cleanup is not implemented; rotation is size based")
}

// ============================================================================
// Simulation Extended Routes (Python simulation.py)
// ============================================================================

func RegisterSimulationExtendedRoutes(g *echo.Group) {
	g.GET("/types", handleGetSimTypes, requirePermission(security.PermDeviceRead))
	g.POST("/preview", handleSimPreview, requirePermission(security.PermDeviceRead))
	g.POST("/run", handleSimRun, requirePermission(security.PermDeviceCreate))
	g.POST("/assess", handleSimAssess, requirePermission(security.PermDeviceRead))
}

func handleGetSimTypes(c echo.Context) error {
	return OK(c, []string{"sine", "square", "triangle", "sawtooth", "random", "step", "ramp"})
}

func handleSimPreview(c echo.Context) error {
	var req struct {
		Type      string  `json:"type"`
		Points    int     `json:"points"`
		Amplitude float64 `json:"amplitude"`
		Frequency float64 `json:"frequency"`
		Offset    float64 `json:"offset"`
		Phase     float64 `json:"phase"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Points == 0 {
		req.Points = 100
	}
	if req.Amplitude == 0 {
		req.Amplitude = 1.0
	}
	if req.Frequency == 0 {
		req.Frequency = 1.0
	}

	preview := make([]map[string]interface{}, req.Points)
	for i := 0; i < req.Points; i++ {
		t := float64(i) / float64(req.Points)
		var val float64
		switch req.Type {
		case "sine":
			val = req.Amplitude*math.Sin(2*math.Pi*req.Frequency*t+req.Phase) + req.Offset
		case "square":
			phase := 2*math.Pi*req.Frequency*t + req.Phase
			if math.Sin(phase) >= 0 {
				val = req.Amplitude + req.Offset
			} else {
				val = -req.Amplitude + req.Offset
			}
		case "triangle":
			phase := 2*math.Pi*req.Frequency*t + req.Phase
			val = (2*req.Amplitude/math.Pi)*math.Asin(math.Sin(phase)) + req.Offset
		case "sawtooth":
			phase := req.Frequency*t + req.Phase/(2*math.Pi)
			val = 2*req.Amplitude*(phase-math.Floor(phase+0.5)) + req.Offset
		case "random":
			val = req.Amplitude*(2*rand.Float64()-1) + req.Offset
		case "step":
			if t < 0.5 {
				val = req.Offset
			} else {
				val = req.Amplitude + req.Offset
			}
		case "ramp":
			val = req.Amplitude*t + req.Offset
		default:
			val = req.Amplitude*math.Sin(2*math.Pi*req.Frequency*t) + req.Offset
		}
		preview[i] = map[string]interface{}{
			"index": i,
			"time":  t,
			"value": val,
		}
	}

	return OK(c, map[string]interface{}{
		"preview": preview,
		"points":  req.Points,
	})
}

func handleSimRun(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{
		"status": "running",
		"run_id": fmt.Sprintf("sim_%d", time.Now().UnixNano()),
	})
}

func handleSimAssess(c echo.Context) error {
	var req struct {
		Type       string  `json:"type"`
		Points     int     `json:"points"`
		Amplitude  float64 `json:"amplitude"`
		Frequency  float64 `json:"frequency"`
		Offset     float64 `json:"offset"`
		SampleRate float64 `json:"sample_rate"`
		Duration   float64 `json:"duration"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	metrics := make(map[string]interface{})
	score := 100.0

	// Validate parameters and compute quality metrics
	if req.Points < 10 {
		score -= 20
		metrics["warning_points"] = "too few points for reliable simulation"
	}
	if req.Amplitude < 0 {
		score -= 15
		metrics["error_amplitude"] = "amplitude must be non-negative"
	}
	if req.Frequency <= 0 {
		score -= 15
		metrics["error_frequency"] = "frequency must be positive"
	}

	// Nyquist check
	if req.SampleRate > 0 && req.Frequency > 0 {
		nyquist := req.SampleRate / 2
		if req.Frequency > nyquist {
			score -= 25
			metrics["warning_nyquist"] = fmt.Sprintf("frequency %.2f exceeds Nyquist limit %.2f", req.Frequency, nyquist)
		}
		metrics["nyquist_limit"] = nyquist
	}

	// Duration check
	if req.Duration > 0 && req.Points > 0 {
		effectiveRate := float64(req.Points) / req.Duration
		metrics["effective_sample_rate"] = effectiveRate
	}

	metrics["type"] = req.Type
	metrics["points"] = req.Points
	metrics["amplitude"] = req.Amplitude
	metrics["frequency"] = req.Frequency

	if score < 0 {
		score = 0
	}

	return OK(c, map[string]interface{}{
		"score":   score,
		"metrics": metrics,
	})
}

// ============================================================================
// Data Quality Extended Routes
// ============================================================================

func RegisterDataQualityExtendedRoutes(g *echo.Group) {
	g.GET("/devices", handleGetDataQualityDevices, requirePermission(security.PermDataRead))
	g.GET("/devices/:device_id/points", handleGetDataQualityPoints, requirePermission(security.PermDataRead))
	g.GET("/rules", handleGetDataQualityRules, requirePermission(security.PermDataRead))
	g.GET("/downsample/config", handleGetDownsampleConfig, requirePermission(security.PermDataRead))
	g.GET("/downsample/history", handleGetDownsampleHistory, requirePermission(security.PermDataRead))
	g.GET("/downsample/storage", handleGetDownsampleStorage, requirePermission(security.PermDataRead))
	g.POST("/downsample/execute", handleExecuteDownsample, requirePermission(security.PermDataRead))
	g.GET("/report", handleGetDataQualityReport, requirePermission(security.PermDataRead))
	// A mutating endpoint cannot sit behind the read permission: RoleViewer
	// holds data:read and would otherwise be able to POST here.
	g.POST("/reset", handleResetDataQuality, requirePermission(security.PermDataImport))
}

func handleGetDataQualityDevices(c echo.Context) error {
	hours := 24
	if h, err := strconv.Atoi(c.QueryParam("hours")); err == nil && h > 0 && h <= 24*30 {
		hours = h
	}

	out := []map[string]interface{}{}
	cont := GetContainer()
	if cont == nil || cont.TsStorage == nil || cont.DeviceRepo == nil {
		return OK(c, out)
	}

	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := cont.TsStorage.QualityByDevice(since)
	if err != nil {
		logrus.WithError(err).Error("Failed to compute device data quality")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}
	rowByDevice := make(map[string]storage.DeviceQualityRow, len(rows))
	for _, r := range rows {
		rowByDevice[r.DeviceID] = r
	}

	devices, err := cont.DeviceRepo.ListAll()
	if err != nil {
		logrus.WithError(err).Error("Failed to list devices for data quality")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}

	windowSec := hours * 3600
	seen := make(map[string]bool, len(devices))
	for _, d := range devices {
		seen[d.DeviceID] = true
		interval := d.CollectInterval
		if interval <= 0 {
			interval = 5
		}
		samples := windowSec / interval
		if samples < 1 {
			samples = 1
		}
		expected := len(d.Points) * samples

		entry := map[string]interface{}{
			"device_id":     d.DeviceID,
			"device_name":   d.Name,
			"point_count":   0,
			"valid_count":   0,
			"invalid_count": 0,
			"missing_count": expected,
			"quality_pct":   0.0,
			"last_check":    nil,
		}
		if r, ok := rowByDevice[d.DeviceID]; ok {
			missing := expected - int(r.TotalRows)
			if missing < 0 {
				missing = 0
			}
			qualityPct := 0.0
			if r.TotalRows > 0 {
				qualityPct = math.Round(float64(r.ValidRows)/float64(r.TotalRows)*1000) / 10
			}
			entry["point_count"] = r.DistinctPoints
			entry["valid_count"] = r.ValidRows
			entry["invalid_count"] = r.InvalidRows
			entry["missing_count"] = missing
			entry["quality_pct"] = qualityPct
			entry["last_check"] = r.LastTimestamp
		}
		out = append(out, entry)
	}

	// Devices with data but no definition (e.g. deleted) still deserve a row.
	for id, r := range rowByDevice {
		if seen[id] {
			continue
		}
		qualityPct := 0.0
		if r.TotalRows > 0 {
			qualityPct = math.Round(float64(r.ValidRows)/float64(r.TotalRows)*1000) / 10
		}
		out = append(out, map[string]interface{}{
			"device_id":     id,
			"device_name":   id,
			"point_count":   r.DistinctPoints,
			"valid_count":   r.ValidRows,
			"invalid_count": r.InvalidRows,
			"missing_count": 0,
			"quality_pct":   qualityPct,
			"last_check":    r.LastTimestamp,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i]["device_id"].(string) < out[j]["device_id"].(string)
	})
	return OK(c, out)
}

// handleGetDataQualityRules returns the built-in quality validation rules the
// gateway applies to collected samples.
func handleGetDataQualityRules(c echo.Context) error {
	rules := []map[string]interface{}{
		{
			"name":      "quality_flag",
			"rule_type": "driver",
			"condition": "Driver read returned quality != 'good' — sample marked invalid",
			"enabled":   true,
		},
		{
			"name":      "null_value",
			"rule_type": "value",
			"condition": "Sample value is null/empty — sample marked invalid",
			"enabled":   true,
		},
		{
			"name":      "stale_data",
			"rule_type": "timeliness",
			"condition": "No update within 2x collect interval — point marked stale",
			"enabled":   true,
		},
	}
	return OK(c, rules)
}

// handleGetDownsampleConfig returns the tiered downsample configuration.
func handleGetDownsampleConfig(c echo.Context) error {
	cfg := config.GetConfig().InfluxDB.Downsample
	return OK(c, map[string]interface{}{
		"enabled":            cfg.Enabled,
		"auto_run":           cfg.AutoRun,
		"tier1_age_days":     cfg.Tier1AgeDays,
		"tier2_age_days":     cfg.Tier2AgeDays,
		"tier3_age_days":     cfg.Tier3AgeDays,
		"run_interval_hours": cfg.RunIntervalHours,
	})
}

func handleGetDownsampleHistory(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return OK(c, []interface{}{})
	}
	runs, err := cont.Database.ListDownsampleRuns(50)
	if err != nil {
		logrus.WithError(err).Error("Failed to list downsample runs")
		return InternalError(c, "ERR_DOWNSAMPLE_FAILED")
	}
	return OK(c, runs)
}

func handleGetDownsampleStorage(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.TsStorage == nil || cont.Database == nil {
		return OK(c, map[string]interface{}{
			"raw_size_mb": 0, "downsampled_size_mb": 0, "space_saved_pct": 0.0,
		})
	}

	total, downsampled, err := cont.TsStorage.DownsampleCounts()
	if err != nil {
		logrus.WithError(err).Error("Failed to count time-series rows")
		return InternalError(c, "ERR_DOWNSAMPLE_FAILED")
	}
	netDeleted, err := cont.Database.DownsampleNetDeleted()
	if err != nil {
		netDeleted = 0
	}

	cfg := config.GetConfig()
	var fileBytes int64
	for _, p := range []string{cfg.InfluxDB.SQLiteTSPath, cfg.InfluxDB.SQLiteTSPath + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			fileBytes += info.Size()
		}
	}
	avgRowBytes := 0.0
	if total > 0 {
		avgRowBytes = float64(fileBytes) / float64(total)
	}
	const mib = 1024 * 1024
	currentSizeMB := float64(total) * avgRowBytes / mib
	rawEquivRows := float64(total) + float64(netDeleted)
	rawSizeMB := rawEquivRows * avgRowBytes / mib
	savedPct := 0.0
	if rawEquivRows > 0 {
		savedPct = float64(netDeleted) / rawEquivRows * 100
	}
	return OK(c, map[string]interface{}{
		"total_rows":          total,
		"downsampled_rows":    downsampled,
		"net_deleted_rows":    netDeleted,
		"file_size_mb":        math.Round(float64(fileBytes)/mib*100) / 100,
		"raw_size_mb":         math.Round(rawSizeMB*100) / 100,
		"downsampled_size_mb": math.Round(currentSizeMB*100) / 100,
		"space_saved_pct":     math.Round(savedPct*10) / 10,
	})
}

func handleExecuteDownsample(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.TsStorage == nil || cont.Database == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	cfg := config.GetConfig().InfluxDB.Downsample
	started := time.Now().Format(time.RFC3339)
	results, err := cont.TsStorage.RunDownsample(cfg.Tier1AgeDays, cfg.Tier2AgeDays, cfg.Tier3AgeDays)
	completed := time.Now().Format(time.RFC3339)
	status, errMsg := "success", ""
	if err != nil {
		status, errMsg = "failed", err.Error()
		logrus.WithError(err).Error("Downsample execution failed")
	}
	for _, r := range results {
		if _, ierr := cont.Database.InsertDownsampleRun(storage.DownsampleRun{
			Tier: r.Tier, RowsProcessed: r.RowsProcessed, RowsArchived: r.RowsArchived,
			Status: status, Error: errMsg, StartedAt: started, CompletedAt: completed,
		}); ierr != nil {
			logrus.WithError(ierr).Warn("Failed to record downsample run")
		}
	}
	if err != nil {
		return InternalError(c, "ERR_DOWNSAMPLE_FAILED")
	}
	totalProcessed, totalArchived := int64(0), int64(0)
	for _, r := range results {
		totalProcessed += r.RowsProcessed
		totalArchived += r.RowsArchived
	}
	return OK(c, map[string]interface{}{
		"tiers":          results,
		"rows_processed": totalProcessed,
		"rows_archived":  totalArchived,
		"completed_at":   completed,
	})
}

func handleGetDataQualityPoints(c echo.Context) error {
	deviceID := c.Param("device_id")
	return OK(c, map[string]interface{}{
		"device_id": deviceID,
		"points":    []interface{}{},
	})
}

// handleGetDataQualityReport summarizes the quality the collection pipeline
// measured. It used to answer {"summary":{}, "by_device":[]} for every request,
// which reads as "no quality problems anywhere" -- the opposite of what the
// same store reports on /data-quality/devices.
func handleGetDataQualityReport(c echo.Context) error {
	hours := 24
	if h, err := strconv.Atoi(c.QueryParam("hours")); err == nil && h > 0 && h <= 24*30 {
		hours = h
	}
	cont := GetContainer()
	if cont.TsStorage == nil {
		return ServiceUnavailable(c, "Time-series storage is not ready")
	}
	rows, err := cont.TsStorage.QualityByDevice(time.Now().Add(-time.Duration(hours) * time.Hour))
	if err != nil {
		logrus.WithError(err).Error("Failed to compute the data quality report")
		return InternalError(c, "ERR_DATA_QUALITY_FAILED")
	}

	var total, valid, invalid int64
	byDevice := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		total += r.TotalRows
		valid += r.ValidRows
		invalid += r.InvalidRows
		var pct interface{}
		if r.TotalRows > 0 {
			pct = math.Round(float64(r.ValidRows)/float64(r.TotalRows)*1000) / 10
		}
		byDevice = append(byDevice, map[string]interface{}{
			"device_id":      r.DeviceID,
			"point_count":    r.DistinctPoints,
			"total_rows":     r.TotalRows,
			"valid_rows":     r.ValidRows,
			"invalid_rows":   r.InvalidRows,
			"quality_pct":    pct,
			"last_timestamp": r.LastTimestamp,
		})
	}
	sort.Slice(byDevice, func(i, j int) bool {
		return byDevice[i]["device_id"].(string) < byDevice[j]["device_id"].(string)
	})

	summary := map[string]interface{}{
		"window_hours": hours,
		"device_count": len(rows),
		"total_rows":   total,
		"valid_rows":   valid,
		"invalid_rows": invalid,
		// No samples in the window is not a perfect score: nothing was measured.
		"quality_pct": nil,
		"measured":    false,
	}
	if total > 0 {
		summary["quality_pct"] = math.Round(float64(valid)/float64(total)*1000) / 10
		summary["measured"] = true
	}
	return OK(c, map[string]interface{}{
		"summary":      summary,
		"by_device":    byDevice,
		"generated_at": time.Now().Format(time.RFC3339),
	})
}

// handleResetDataQuality refuses a request for a capability the gateway does not
// have: quality is derived from the stored samples, so there is no counter to
// clear. It used to answer {"reset":true} having changed nothing.
func handleResetDataQuality(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_DATA_QUALITY_RESET_UNSUPPORTED",
		"Data quality is derived from stored samples, so it cannot be reset; delete the samples instead")
}

// ============================================================================
// DB Monitor Extended Routes
// ============================================================================

func RegisterDBMonitorExtendedRoutes(g *echo.Group) {
	g.GET("/pool-stats", handleGetDBPoolStats, requirePermission(security.PermSystemConfig))
}

// handleGetDBPoolStats reports the connection pool as the driver sees it. The
// numbers used to be a literal written into the source, so an operator reading a
// saturated pool was shown one idle connection either way.
func handleGetDBPoolStats(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_DB_UNAVAILABLE", "Database is not ready")
	}
	// Take a read first: without one a freshly started gateway has never opened a
	// connection and reports an empty pool that says nothing about the load.
	if err := cont.Database.HealthCheck(); err != nil {
		logrus.WithError(err).Warn("Database health check failed while reading pool stats")
	}
	st := cont.Database.DB().Stats()
	return OK(c, map[string]interface{}{
		"max_open":         st.MaxOpenConnections,
		"open":             st.OpenConnections,
		"in_use":           st.InUse,
		"idle":             st.Idle,
		"wait_count":       st.WaitCount,
		"wait_duration_ns": st.WaitDuration.Nanoseconds(),
	})
}

// unmeasuredDBStats is the counterpart of GetStats' own failure shape: the keys
// the page binds are present so it renders, but every value is null (not a
// fabricated 0) and the payload says why it was not measured.
func unmeasuredDBStats(reason string) map[string]interface{} {
	return map[string]interface{}{
		"db_size":         nil,
		"table_count":     nil,
		"total_rows":      nil,
		"wal_size":        nil,
		"page_size":       nil,
		"free_node_pages": nil,
		"measured":        false,
		"measure_error":   reason,
	}
}

// handleGetDBStats returns database statistics for the DbMonitor dashboard.
//
// The fallback used to answer a full set of zeros with no marker, which is
// indistinguishable from an empty database, and it dropped GetStats' error on
// the floor. It now reports measured:false with the reason, and the numbers
// stay null so "cannot measure" never reads as "measured nothing".
func handleGetDBStats(c echo.Context) error {
	cont := GetContainer()
	if cont.DBMonitor == nil {
		return OK(c, unmeasuredDBStats("database monitor service is not running"))
	}
	stats, err := cont.DBMonitor.GetStats()
	if err != nil {
		logrus.WithError(err).Warn("DB monitor failed to gather statistics")
		return OK(c, unmeasuredDBStats(err.Error()))
	}
	return OK(c, stats)
}

// handleDBVacuum executes VACUUM on the database. Without a monitor it used to
// answer "vacuum_completed", reporting work no code had performed.
func handleDBVacuum(c echo.Context) error {
	cont := GetContainer()
	if cont.DBMonitor == nil {
		return ServiceUnavailable(c, "Database monitor is not ready, VACUUM cannot run")
	}
	if err := cont.DBMonitor.Vacuum(); err != nil {
		logrus.WithError(err).Warn("VACUUM failed")
		// VACUUM failing on a busy database is expected, so this stays a 200 --
		// but the reason is the error SQLite gave, not a hardcoded guess.
		return OK(c, map[string]interface{}{"status": "vacuum_skipped", "reason": err.Error()})
	}
	return OK(c, map[string]interface{}{"status": "vacuum_completed"})
}

// handleDBReindex executes REINDEX on the database.
func handleDBReindex(c echo.Context) error {
	cont := GetContainer()
	if cont.DBMonitor == nil {
		return ServiceUnavailable(c, "Database monitor is not ready, REINDEX cannot run")
	}
	if err := cont.DBMonitor.Reindex(); err != nil {
		logrus.WithError(err).Warn("REINDEX failed")
		return OK(c, map[string]interface{}{"status": "reindex_skipped", "reason": err.Error()})
	}
	return OK(c, map[string]interface{}{"status": "reindex_completed"})
}

// ============================================================================
// Protocol Bridge Extended Routes (Python prefix=/api/v1/bridge)
// ============================================================================

func RegisterProtocolBridgeExtendedRoutes(g *echo.Group) {
	g.GET("/list", handleBridgeList, requirePermission(security.PermSystemConfig))
	g.GET("/:name", handleBridgeGet, requirePermission(security.PermSystemConfig))
	g.POST("/create", handleBridgeCreate, requirePermission(security.PermSystemConfig))
	g.PUT("/:name", handleBridgeUpdate, requirePermission(security.PermSystemConfig))
	g.POST("/:name/enable", handleBridgeEnable, requirePermission(security.PermSystemConfig))
	g.POST("/:name/disable", handleBridgeDisable, requirePermission(security.PermSystemConfig))
}

func handleBridgeList(c echo.Context) error {
	return OKPaged(c, []interface{}{}, 0, 1, 20)
}

func handleBridgeGet(c echo.Context) error {
	name := c.Param("name")
	return OK(c, map[string]interface{}{
		"name":    name,
		"enabled": false,
		"config":  map[string]interface{}{},
	})
}

func handleBridgeCreate(c echo.Context) error {
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return Created(c, req)
}

func handleBridgeUpdate(c echo.Context) error {
	name := c.Param("name")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"name": name, "updated": true})
}

func handleBridgeEnable(c echo.Context) error {
	name := c.Param("name")
	return OK(c, map[string]interface{}{"name": name, "enabled": true})
}

func handleBridgeDisable(c echo.Context) error {
	name := c.Param("name")
	return OK(c, map[string]interface{}{"name": name, "disabled": true})
}

// ============================================================================
// Resource Shares Extended Routes
// ============================================================================

func RegisterResourceShareExtendedRoutes(g *echo.Group) {
	g.POST("", handleCreateResourceShare, requirePermission(security.PermSystemConfig))
	g.DELETE("", handleUnshareResource, requirePermission(security.PermSystemConfig))
	g.DELETE("/:id", handleDeleteResourceShare, requirePermission(security.PermSystemConfig))
	g.POST("/check", handleCheckResourceShare, requirePermission(security.PermSystemConfig))
}

type resourceShare struct {
	ID              string   `json:"id"`
	ResourceType    string   `json:"resource_type"`
	ResourceID      string   `json:"resource_id"`
	TargetGatewayID string   `json:"target_gateway_id"`
	ExpiresHours    int      `json:"expires_hours"`
	Permissions     []string `json:"permissions"`
	CreatedAt       string   `json:"created_at"`
	ExpiresAt       string   `json:"expires_at"`
	Status          string   `json:"status"`
}

func loadResourceShares(cont *ServiceContainer) ([]resourceShare, error) {
	if cont.Database == nil {
		return nil, fmt.Errorf("ERR_COMMON_DB_NOT_READY")
	}
	raw, err := cont.Database.GetSetting("resource_shares")
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return []resourceShare{}, nil
	}
	var shares []resourceShare
	if err := json.Unmarshal([]byte(raw), &shares); err != nil {
		// Answering "no shares" for a store that cannot be parsed would hide every
		// grant at once and look like a clean slate.
		return nil, fmt.Errorf("ERR_RESOURCE_SHARE_STORE_CORRUPT")
	}
	return shares, nil
}

func saveResourceShares(cont *ServiceContainer, shares []resourceShare) error {
	if cont.Database == nil {
		return fmt.Errorf("ERR_COMMON_DB_NOT_READY")
	}
	raw, err := json.Marshal(shares)
	if err != nil {
		return err
	}
	return cont.Database.SetSetting("resource_shares", string(raw))
}

func resourceSharesWithStatus() ([]resourceShare, error) {
	shares, err := loadResourceShares(GetContainer())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range shares {
		shares[i].Status = "active"
		if exp, err := time.Parse(time.RFC3339, shares[i].ExpiresAt); err == nil && now.After(exp) {
			shares[i].Status = "expired"
		}
	}
	return shares, nil
}

func handleCreateResourceShare(c echo.Context) error {
	var req struct {
		ResourceType    string   `json:"resource_type"`
		ResourceID      string   `json:"resource_id"`
		TargetGatewayID string   `json:"target_gateway_id"`
		ExpiresHours    int      `json:"expires_hours"`
		Permissions     []string `json:"permissions"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.ResourceType == "" || req.ResourceID == "" || req.TargetGatewayID == "" {
		return BadRequest(c, "resource_type, resource_id and target_gateway_id are required")
	}
	if req.ExpiresHours <= 0 {
		req.ExpiresHours = 24
	}
	if len(req.Permissions) == 0 {
		req.Permissions = []string{"read"}
	}
	cont := GetContainer()
	now := time.Now()
	share := resourceShare{
		ID:              fmt.Sprintf("share-%d", now.UnixNano()),
		ResourceType:    req.ResourceType,
		ResourceID:      req.ResourceID,
		TargetGatewayID: req.TargetGatewayID,
		ExpiresHours:    req.ExpiresHours,
		Permissions:     req.Permissions,
		CreatedAt:       now.Format(time.RFC3339),
		ExpiresAt:       now.Add(time.Duration(req.ExpiresHours) * time.Hour).Format(time.RFC3339),
		Status:          "active",
	}
	shares, err := loadResourceShares(cont)
	if err != nil {
		return shareStoreError(c, err)
	}
	shares = append(shares, share)
	if err := saveResourceShares(cont, shares); err != nil {
		return shareStoreError(c, err)
	}
	logrus.WithFields(logrus.Fields{"share_id": share.ID, "resource_type": share.ResourceType}).Info("Resource share created")
	return Created(c, share)
}

func handleDeleteResourceShare(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	shares, err := loadResourceShares(cont)
	if err != nil {
		return shareStoreError(c, err)
	}
	remaining := make([]resourceShare, 0, len(shares))
	found := false
	for _, s := range shares {
		if s.ID == id {
			found = true
			continue
		}
		remaining = append(remaining, s)
	}
	if !found {
		return NotFound(c, "ERR_RESOURCE_SHARE_NOT_FOUND")
	}
	if err := saveResourceShares(cont, remaining); err != nil {
		return shareStoreError(c, err)
	}
	logrus.WithField("share_id", id).Info("Resource share revoked")
	return OK(c, map[string]interface{}{"deleted": id})
}

func handleUnshareResource(c echo.Context) error {
	var req struct {
		ResourceType    string `json:"resource_type"`
		ResourceID      string `json:"resource_id"`
		TargetGatewayID string `json:"target_gateway_id"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.ResourceType == "" || req.ResourceID == "" {
		return BadRequest(c, "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED")
	}
	cont := GetContainer()
	shares, err := loadResourceShares(cont)
	if err != nil {
		return shareStoreError(c, err)
	}
	remaining := make([]resourceShare, 0, len(shares))
	removed := 0
	for _, s := range shares {
		if s.ResourceType == req.ResourceType && s.ResourceID == req.ResourceID &&
			(req.TargetGatewayID == "" || s.TargetGatewayID == req.TargetGatewayID) {
			removed++
			continue
		}
		remaining = append(remaining, s)
	}
	if removed > 0 {
		if err := saveResourceShares(cont, remaining); err != nil {
			return shareStoreError(c, err)
		}
	}
	return OK(c, map[string]interface{}{"deleted": removed > 0, "count": removed})
}

// handleCheckResourceShare answers whether a stored, unexpired share really
// grants the asking gateway access. It used to return has_access:true for every
// question, which turns an authorization check into a rubber stamp.
func handleCheckResourceShare(c echo.Context) error {
	var req struct {
		ResourceType    string `json:"resource_type"`
		ResourceID      string `json:"resource_id"`
		TargetGatewayID string `json:"target_gateway_id"`
		Permission      string `json:"permission"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.ResourceType == "" || req.ResourceID == "" {
		return BadRequest(c, "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED")
	}
	if req.Permission == "" {
		req.Permission = "read"
	}
	shares, err := resourceSharesWithStatus()
	if err != nil {
		return shareStoreError(c, err)
	}

	var matched *resourceShare
	expiredHit := false
	targetMismatch := false
	permMismatch := false
	for i := range shares {
		s := shares[i]
		if s.ResourceType != req.ResourceType || s.ResourceID != req.ResourceID {
			continue
		}
		if req.TargetGatewayID != "" && s.TargetGatewayID != req.TargetGatewayID {
			targetMismatch = true
			continue
		}
		if s.Status == "expired" {
			expiredHit = true
			continue
		}
		if !shareGrants(s.Permissions, req.Permission) {
			permMismatch = true
			continue
		}
		matched = &shares[i]
		break
	}

	out := map[string]interface{}{
		"has_access": matched != nil,
		"permission": req.Permission,
		"reason":     "no_share",
	}
	switch {
	case matched != nil:
		out["reason"] = "share_active"
		out["share_id"] = matched.ID
		out["expires_at"] = matched.ExpiresAt
		out["granted"] = matched.Permissions
	case expiredHit:
		out["reason"] = "share_expired"
	case targetMismatch:
		out["reason"] = "target_gateway_mismatch"
	case permMismatch:
		out["reason"] = "permission_denied"
	}
	return OK(c, out)
}

// shareGrants reports whether a share's permission list covers what is asked
// for. A write grant implies read; nothing else implies anything.
func shareGrants(perms []string, want string) bool {
	for _, p := range perms {
		if p == want || (want == "read" && p == "write") {
			return true
		}
	}
	return false
}

// shareStoreError turns a share-store failure into the answer an operator can
// act on, instead of a 200 that looks like an empty share list.
func shareStoreError(c echo.Context, err error) error {
	switch err.Error() {
	case "ERR_COMMON_DB_NOT_READY":
		return ServiceUnavailable(c, err.Error())
	case "ERR_RESOURCE_SHARE_STORE_CORRUPT":
		return InternalError(c, err.Error())
	}
	logrus.WithError(err).Error("Resource share store failure")
	return InternalError(c, "ERR_COMMON_INTERNAL")
}

// ============================================================================
// Services Extended Routes
// ============================================================================

func RegisterServiceExtendedRoutes(g *echo.Group) {
	g.POST("/:name/enable", handleEnableService, requirePermission(security.PermSystemConfig))
	g.POST("/:name/disable", handleDisableService, requirePermission(security.PermSystemConfig))
	g.POST("/:name/install-deps", handleInstallDeps, requirePermission(security.PermSystemConfig))
	g.PUT("/:name/config", handleUpdateServiceConfig, requirePermission(security.PermSystemConfig))
}

func handleEnableService(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()

	// Persist the enabled flag so handleListServices reports the correct state.
	cont.ServiceEnabledMu.Lock()
	cont.ServiceEnabledMap[name] = true
	cont.ServiceEnabledMu.Unlock()
	if cont.Database != nil {
		_ = cont.Database.SetSetting("service_enabled_"+name, "1")
	}

	switch name {
	case "mcp_server":
		logrus.WithField("service", name).Info("MCP Server enabled")
		return OK(c, map[string]interface{}{
			"name":    name,
			"enabled": true,
			"message": "MCP Server enabled successfully",
		})
	case "grafana":
		logrus.WithField("service", name).Info("Grafana monitoring enabled")
		return OK(c, map[string]interface{}{
			"name":    name,
			"enabled": true,
			"warning": "Grafana requires external Grafana instance. Configure URL in system settings.",
		})
	case "mqtt_forwarder":
		// Try to start the MQTT forwarder if it exists (non-blocking)
		if cont.MqttForward != nil {
			go func() {
				if err := cont.MqttForward.Start(context.Background()); err != nil {
					logrus.WithError(err).WithField("service", name).Warn("MQTT forwarder start failed (may need broker config)")
				}
			}()
		}
		logrus.WithField("service", name).Info("MQTT forwarder enabled")
		return OK(c, map[string]interface{}{
			"name":    name,
			"enabled": true,
			"warning": "MQTT forwarder enabled. Connection to broker will be attempted in background.",
		})
	case "mqtt_server", "modbus_slave", "serial_bridge":
		// Embedded services: enabling persists the config flag AND starts the
		// real listener, so "已启用" always means the service is serving.
		if err := setEmbeddedServiceEnabled(name, true); err != nil {
			logrus.WithError(err).WithField("service", name).Warn("Failed to persist service enabled state")
		}
		var startErr error
		switch name {
		case "mqtt_server":
			startErr = startEmbeddedMqttServer(cont)
		case "modbus_slave":
			startErr = startEmbeddedModbusSlave(cont)
		case "serial_bridge":
			startErr = startEmbeddedSerialBridge(cont)
		}
		if startErr != nil {
			logrus.WithError(startErr).WithField("service", name).Error("Service start failed")
			return BadRequest(c, startErr.Error())
		}
		logrus.WithField("service", name).Info("Service enabled")
		return OK(c, map[string]interface{}{
			"name":    name,
			"enabled": true,
		})
	default:
		return OK(c, map[string]interface{}{
			"name":    name,
			"enabled": true,
		})
	}
}

func handleDisableService(c echo.Context) error {
	name := c.Param("name")
	cont := GetContainer()

	cont.ServiceEnabledMu.Lock()
	cont.ServiceEnabledMap[name] = false
	cont.ServiceEnabledMu.Unlock()
	if cont.Database != nil {
		_ = cont.Database.SetSetting("service_enabled_"+name, "0")
	}

	switch name {
	case "mqtt_forwarder":
		if cont.MqttForward != nil {
			cont.MqttForward.Stop()
			logrus.WithField("service", name).Info("MQTT forwarder stopped")
		}
	case "mqtt_server", "modbus_slave", "serial_bridge":
		if err := setEmbeddedServiceEnabled(name, false); err != nil {
			logrus.WithError(err).WithField("service", name).Warn("Failed to persist service disabled state")
		}
		switch name {
		case "mqtt_server":
			stopEmbeddedMqttServer(cont)
		case "modbus_slave":
			stopEmbeddedModbusSlave(cont)
		case "serial_bridge":
			stopEmbeddedSerialBridge(cont)
		}
	}
	logrus.WithField("service", name).Info("Service disabled")
	return OK(c, map[string]interface{}{"name": name, "disabled": true})
}

func handleInstallDeps(c echo.Context) error {
	name := c.Param("name")
	logrus.WithField("service", name).Info("Install dependencies requested")
	return OK(c, map[string]interface{}{"name": name, "installed": true})
}

func handleUpdateServiceConfig(c echo.Context) error {
	name := c.Param("name")
	var req map[string]interface{}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	return OK(c, map[string]interface{}{"name": name, "updated": true})
}

// ============================================================================
// Grafana Extended Routes
// ============================================================================

func RegisterGrafanaExtendedRoutes(g *echo.Group) {
	g.GET("/embed-url", handleGetGrafanaEmbedURL, requirePermission(security.PermSystemConfig))
}

func handleGetGrafanaEmbedURL(c echo.Context) error {
	dashboardID := c.QueryParam("dashboard_id")
	panelID := c.QueryParam("panel_id")
	return OK(c, map[string]interface{}{
		"embed_url":    fmt.Sprintf("/grafana/d-solo/%s?panelId=%s", dashboardID, panelID),
		"dashboard_id": dashboardID,
		"panel_id":     panelID,
	})
}

// ============================================================================
// MQTT Forwarder Extended Routes (Python mqtt_forwarder.py)
// ============================================================================

func RegisterMQTTForwarderExtendedRoutes(g *echo.Group) {
	g.GET("/offline-queue/status", handleGetMQTTOfflineQueueStatus, requirePermission(security.PermSystemConfig))
}

func handleGetMQTTOfflineQueueStatus(c echo.Context) error {
	var pending, sent int64
	var connected, brokerConfigured bool
	if cont := GetContainer(); cont != nil && cont.MqttForward != nil {
		p, s, err := cont.MqttForward.OfflineQueueStats()
		if err != nil {
			logrus.WithError(err).Error("Failed to read offline queue stats")
			return InternalError(c, "ERR_INTERNAL_ERROR")
		}
		pending, sent = p, s
		connected = cont.MqttForward.IsConnected()
		brokerConfigured = cont.MqttForward.IsConfigured()
	}

	maxSize := 10000
	if cfg := config.GetConfig(); cfg != nil && cfg.MQTT.MaxQueueSize > 0 {
		maxSize = cfg.MQTT.MaxQueueSize
	}

	// queue_size is the actual pending backlog (not pending+sent lifetime total),
	// so it is directly comparable with max_size for the watermark.
	ratio := float64(pending) / float64(maxSize)
	watermark := "normal"
	switch {
	case ratio >= 1.0:
		watermark = "critical"
	case ratio >= 0.8:
		watermark = "high"
	case ratio >= 0.5:
		watermark = "warning"
	}

	return OK(c, map[string]interface{}{
		"queue_size":        pending,
		"pending":           pending,
		"sent":              sent,
		"max_size":          maxSize,
		"usage_ratio":       math.Min(ratio, 1),
		"watermark":         watermark,
		"connected":         connected,
		"broker_configured": brokerConfigured,
	})
}

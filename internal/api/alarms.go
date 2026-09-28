package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// RegisterAlarmRoutes registers alarm management API routes.
func RegisterAlarmRoutes(g *echo.Group) {
	// Static routes first (before dynamic :alarm_id)
	g.GET("", handleListAlarms, requirePermission(security.PermAlarmRead))
	g.GET("/statistics", handleGetAlarmStatistics, requirePermission(security.PermAlarmRead))
	g.GET("/trend", handleGetAlarmTrend, requirePermission(security.PermAlarmRead))
	g.GET("/silence", handleListAlarmSilence, requirePermission(security.PermAlarmRead))
	g.POST("/silence", handleCreateAlarmSilence, requirePermission(security.PermAlarmAck))
	g.GET("/correlation", handleGetAlarmCorrelation, requirePermission(security.PermAlarmRead))
	g.GET("/correlation/stats", handleGetAlarmCorrelationStats, requirePermission(security.PermAlarmRead))
	g.GET("/correlation/groups", handleGetAlarmCorrelationGroups, requirePermission(security.PermAlarmRead))
	g.GET("/correlation/suppression", handleGetAlarmSuppressionRules, requirePermission(security.PermAlarmRead))
	g.POST("/batch-ack", handleBatchAckAlarms, requirePermission(security.PermAlarmAck))

	// Dynamic routes
	g.GET("/:alarm_id", handleGetAlarm, requirePermission(security.PermAlarmRead))
	g.PUT("/:alarm_id/ack", handleAckAlarm, requirePermission(security.PermAlarmAck))
	g.PUT("/:alarm_id/recover", handleRecoverAlarm, requirePermission(security.PermAlarmAck))
	g.DELETE("/:alarm_id", handleDeleteAlarm, requirePermission(security.PermAlarmAck))
	g.POST("/:alarm_id/suppress", handleSuppressAlarm, requirePermission(security.PermAlarmAck))
	g.GET("/silence/:silence_id", handleGetAlarmSilence, requirePermission(security.PermAlarmRead))
	g.DELETE("/silence/:silence_id", handleCancelAlarmSilence, requirePermission(security.PermAlarmAck))
	g.GET("/history/:rule_id", handleGetAlarmHistory, requirePermission(security.PermAlarmRead))
}

func handleListAlarms(c echo.Context) error {
	page, size := parsePagination(c)

	filter := models.AlarmFilter{
		Status:    c.QueryParam("status"),
		Severity:  c.QueryParam("severity"),
		DeviceID:  c.QueryParam("device_id"),
		RuleID:    c.QueryParam("rule_id"),
		RuleType:  c.QueryParam("rule_type"),
		Search:    c.QueryParam("search"),
		StartTime: normalizeRFC3339(c.QueryParam("start_time")),
		EndTime:   normalizeRFC3339(c.QueryParam("end_time")),
		SortBy:    c.QueryParam("sort_by"),
		SortOrder: c.QueryParam("sort_order"),
	}
	if v := c.QueryParam("unack_overtime_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.UnackOvertimeMinutes = n
		}
	}
	if v := c.QueryParam("since"); v != "" {
		// The alarm list used to accept `since` and ignore it, so a client that
		// asked for "what changed since I disconnected" received the newest page
		// of everything instead and could not tell the difference.
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return ErrorCode(c, http.StatusBadRequest, "ERR_ALARM_SINCE_INVALID",
				"since must be an RFC3339 timestamp")
		}
		filter.Since = ts.Local().Format(time.RFC3339)
	}

	// A status the store never uses can only match zero rows, so an operator who
	// asked for ?status=active would be told "you have no alarms" by a query that
	// could not have found any.
	switch filter.Status {
	case "", "firing", "acknowledged", "recovered":
	default:
		return ErrorCode(c, http.StatusBadRequest, "ERR_ALARM_INVALID_STATUS",
			"status must be one of firing, acknowledged, recovered")
	}

	cont := GetContainer()
	if cont.AlarmService == nil {
		return ServiceUnavailable(c, "Alarm service not ready")
	}

	alarms, total, err := cont.AlarmService.List(filter, page, size)
	if err != nil {
		logrus.WithError(err).Error("List alarms failed")
		return InternalError(c, "ERR_ALARM_LIST_FAILED")
	}

	if alarms == nil {
		alarms = []models.AlarmResponse{}
	}

	return OKPaged(c, alarms, total, page, size)
}

func handleGetAlarmStatistics(c echo.Context) error {
	days, _ := strconv.Atoi(c.QueryParam("days"))
	if days <= 0 {
		days = 7
	}
	if days > 365 {
		days = 365
	}

	cont := GetContainer()
	if cont.AlarmRepo == nil || cont.Database == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_ALARM_STORE_UNAVAILABLE", "Alarm store is not ready")
	}

	db := cont.Database.DB()

	// Summary counts
	statusCounts := map[string]int{"firing": 0, "acknowledged": 0, "recovered": 0}
	rows, err := db.Query("SELECT status, COUNT(*) FROM alarms GROUP BY status")
	if err == nil {
		for rows.Next() {
			var status string
			var count int
			if err := rows.Scan(&status, &count); err == nil {
				statusCounts[status] += count
			}
		}
		rows.Close()
	}

	// The window bound is compared through datetime(): fired_at is stored as
	// local-offset RFC3339 ("2026-09-21T21:32:10+08:00"), and datetime()
	// normalises those strings to UTC so the comparison is chronological
	// instead of textual.
	cutoffUTC := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")

	// MTTR is the mean of (recovered_at - fired_at) over the alarms that
	// actually recovered in the window. Rows whose recovery timestamp precedes
	// the firing (clock skew, hand-edited rows) are excluded instead of being
	// averaged in as a negative duration.
	var recovered int
	var mttrTotal float64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM((julianday(recovered_at) - julianday(fired_at)) * 86400.0), 0)
		FROM alarms
		WHERE datetime(fired_at) >= datetime(?)
		  AND recovered_at IS NOT NULL AND recovered_at <> ''
		  AND julianday(recovered_at) >= julianday(fired_at)`, cutoffUTC).Scan(&recovered, &mttrTotal); err != nil {
		logrus.WithError(err).Warn("alarm MTTR query failed")
	}

	// MTBF is the mean spacing between consecutive firings in the window,
	// (last - first) / (count - 1). Below two firings there is no interval to
	// average, so it stays unknown: reporting 0 h would read as a plant
	// failing continuously.
	var fired int
	var aiCount int
	var span float64
	if err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN rule_type = 'ai_inference' THEN 1 ELSE 0 END), 0),
		COALESCE((julianday(MAX(fired_at)) - julianday(MIN(fired_at))) * 86400.0, 0)
		FROM alarms WHERE datetime(fired_at) >= datetime(?)`, cutoffUTC).
		Scan(&fired, &aiCount, &span); err != nil {
		logrus.WithError(err).Warn("alarm window query failed")
	}

	// Pointer fields marshal to JSON null when the metric cannot be derived, so
	// the UI renders "no data" instead of a plausible-looking zero.
	var mttrSeconds, mtbfSeconds, aiRatio *float64
	if recovered > 0 {
		v := mttrTotal / float64(recovered)
		mttrSeconds = &v
	}
	if fired > 1 {
		v := span / float64(fired-1)
		mtbfSeconds = &v
	}
	if fired > 0 {
		v := float64(aiCount) / float64(fired)
		aiRatio = &v
	}

	summary := make(map[string]interface{}, len(statusCounts)+5)
	for status, count := range statusCounts {
		summary[status] = count
	}
	summary["window_count"] = fired
	summary["ai_count"] = aiCount
	summary["ai_ratio"] = aiRatio
	summary["mttr_seconds"] = mttrSeconds
	summary["mtbf_seconds"] = mtbfSeconds

	// Trend: daily alarm counts for the past N days
	type trendItem struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	trend := []trendItem{}
	// 按北京时间（UTC+8）分桶日期，与前端显示时区一致
	bj := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	cutoff := bj.AddDate(0, 0, -days).Format("2006-01-02")
	rows, err = db.Query("SELECT DATE(fired_at, '+8 hours') as d, COUNT(*) as c FROM alarms WHERE fired_at >= ? GROUP BY DATE(fired_at, '+8 hours') ORDER BY d", cutoff)
	if err == nil {
		for rows.Next() {
			var d string
			var c int
			if err := rows.Scan(&d, &c); err == nil {
				trend = append(trend, trendItem{Date: d, Count: c})
			}
		}
		rows.Close()
	}

	// Top 10 devices by alarm count
	type topItem struct {
		DeviceID string `json:"device_id"`
		RuleID   string `json:"rule_id"`
		Count    int    `json:"count"`
	}
	topDevices := []topItem{}
	rows, err = db.Query("SELECT device_id, COUNT(*) as c FROM alarms WHERE fired_at >= ? GROUP BY device_id ORDER BY c DESC LIMIT 10", cutoff)
	if err == nil {
		for rows.Next() {
			var devID string
			var c int
			if err := rows.Scan(&devID, &c); err == nil {
				topDevices = append(topDevices, topItem{DeviceID: devID, Count: c})
			}
		}
		rows.Close()
	}

	// Top 10 rules by alarm count
	topRules := []topItem{}
	rows, err = db.Query("SELECT rule_id, COUNT(*) as c FROM alarms WHERE fired_at >= ? GROUP BY rule_id ORDER BY c DESC LIMIT 10", cutoff)
	if err == nil {
		for rows.Next() {
			var rID string
			var c int
			if err := rows.Scan(&rID, &c); err == nil {
				topRules = append(topRules, topItem{RuleID: rID, Count: c})
			}
		}
		rows.Close()
	}

	// Convert to []interface{} for JSON marshalling
	trendIface := make([]interface{}, len(trend))
	for i, t := range trend {
		trendIface[i] = t
	}
	topDevIface := make([]interface{}, len(topDevices))
	for i, t := range topDevices {
		topDevIface[i] = t
	}
	topRuleIface := make([]interface{}, len(topRules))
	for i, t := range topRules {
		topRuleIface[i] = t
	}

	return OK(c, map[string]interface{}{
		"summary":     summary,
		"trend":       trendIface,
		"top_devices": topDevIface,
		"top_rules":   topRuleIface,
	})
}

func handleGetAlarmTrend(c echo.Context) error {
	days, _ := strconv.Atoi(c.QueryParam("days"))
	if days <= 0 {
		days = 7
	}
	if days > 365 {
		days = 365
	}

	cont := GetContainer()
	if cont.Database == nil {
		return OK(c, []interface{}{})
	}
	db := cont.Database.DB()

	// 按北京时间（UTC+8）分桶日期，与前端显示时区一致
	bj := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	cutoff := bj.AddDate(0, 0, -days).Format("2006-01-02")
	type trendItem struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	trend := []trendItem{}
	rows, err := db.Query("SELECT DATE(fired_at, '+8 hours') as d, COUNT(*) as c FROM alarms WHERE fired_at >= ? GROUP BY DATE(fired_at, '+8 hours') ORDER BY d", cutoff)
	if err != nil {
		return OK(c, []interface{}{})
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var c int
		if err := rows.Scan(&d, &c); err == nil {
			trend = append(trend, trendItem{Date: d, Count: c})
		}
	}

	trendIface := make([]interface{}, len(trend))
	for i, t := range trend {
		trendIface[i] = t
	}
	return OK(c, trendIface)
}

func handleBatchAckAlarms(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.AlarmBatchAckRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.AlarmIDs) == 0 {
		return BadRequest(c, "Alarm IDs required")
	}
	if len(req.AlarmIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	acked, failed, err := cont.AlarmService.BatchAcknowledge(&req, user.UserID)
	if err != nil {
		return InternalError(c, "ERR_ALARM_ACK_FAILED")
	}

	return OK(c, map[string]interface{}{
		"succeeded":       acked,
		"failed":          failed,
		"total":           len(req.AlarmIDs),
		"succeeded_count": acked,
		"failed_count":    failed,
	})
}

func handleGetAlarm(c echo.Context) error {
	alarmID := c.Param("alarm_id")
	if alarmID == "" {
		return BadRequest(c, "Alarm ID required")
	}

	cont := GetContainer()
	alarm, err := cont.AlarmService.Get(alarmID)
	if err != nil || alarm == nil {
		return NotFound(c, "ERR_ALARM_NOT_FOUND")
	}

	return OK(c, alarm)
}

func handleAckAlarm(c echo.Context) error {
	user := getUserFromContext(c)
	alarmID := c.Param("alarm_id")
	if alarmID == "" {
		return BadRequest(c, "Alarm ID required")
	}

	cont := GetContainer()
	if err := cont.AlarmService.Acknowledge(alarmID, user.Username); err != nil {
		return NotFound(c, "ERR_ALARM_NOT_FOUND")
	}

	alarm, _ := cont.AlarmService.Get(alarmID)
	return OK(c, alarm)
}

func handleRecoverAlarm(c echo.Context) error {
	alarmID := c.Param("alarm_id")
	if alarmID == "" {
		return BadRequest(c, "Alarm ID required")
	}

	cont := GetContainer()
	if err := cont.AlarmService.Recover(alarmID); err != nil {
		return NotFound(c, "ERR_ALARM_NOT_FOUND")
	}

	alarm, _ := cont.AlarmService.Get(alarmID)
	return OK(c, alarm)
}

func handleDeleteAlarm(c echo.Context) error {
	alarmID := c.Param("alarm_id")
	if alarmID == "" {
		return BadRequest(c, "Alarm ID required")
	}

	cont := GetContainer()
	if err := cont.AlarmService.Delete(alarmID); err != nil {
		return NotFound(c, "ERR_ALARM_NOT_FOUND")
	}

	return OK(c, map[string]string{"alarm_id": alarmID, "deleted": "true"})
}

func handleSuppressAlarm(c echo.Context) error {
	alarmID := c.Param("alarm_id")
	if alarmID == "" {
		return BadRequest(c, "Alarm ID required")
	}

	type SuppressRequest struct {
		DurationSeconds int    `json:"duration_seconds"`
		Reason          string `json:"reason"`
	}
	var req SuppressRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.DurationSeconds <= 0 {
		req.DurationSeconds = 3600
	}

	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	alarm, err := cont.AlarmRepo.Get(alarmID)
	if err != nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if alarm == nil {
		return NotFound(c, "Alarm not found")
	}

	now := time.Now()
	silence := &storage.SilenceRecord{
		ID:        "silence_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		AlarmID:   alarmID,
		DeviceID:  alarm.DeviceID,
		RuleID:    alarm.RuleID,
		StartTime: now.Format(time.RFC3339),
		EndTime:   now.Add(time.Duration(req.DurationSeconds) * time.Second).Format(time.RFC3339),
		Reason:    req.Reason,
	}
	if user := getUserFromContext(c); user != nil {
		silence.Operator = user.Username
	}
	if err := cont.AlarmRepo.CreateSilence(silence); err != nil {
		logrus.WithError(err).Error("Failed to persist alarm silence")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}

	logrus.WithFields(logrus.Fields{
		"alarm_id":         alarmID,
		"duration_seconds": req.DurationSeconds,
		"reason":           req.Reason,
	}).Info("Alarm suppressed")

	return OK(c, map[string]interface{}{
		"alarm_id":         alarmID,
		"suppressed":       true,
		"duration_seconds": req.DurationSeconds,
		"silence_id":       silence.ID,
		"end_time":         silence.EndTime,
	})
}

// handleListAlarmSilence lists alarm silence periods.
func handleListAlarmSilence(c echo.Context) error {
	page, size := parsePagination(c)
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return OKPaged(c, []interface{}{}, 0, page, size)
	}
	silences, err := cont.AlarmRepo.ListSilences(c.QueryParam("status") == "active")
	if err != nil {
		logrus.WithError(err).Error("List alarm silences failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	items := make([]interface{}, len(silences))
	for i, s := range silences {
		items[i] = s
	}
	return OKPaged(c, items, len(items), page, size)
}

// normalizeRFC3339 converts any RFC3339 input (UTC "Z" or other offsets) to the
// server-local RFC3339 form, so silence windows compare correctly against
// time.Now().Format(time.RFC3339) in SQL string comparisons.
func normalizeRFC3339(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format(time.RFC3339)
	}
	return s
}

// handleCreateAlarmSilence creates an alarm silence period.
func handleCreateAlarmSilence(c echo.Context) error {
	user := getUserFromContext(c)
	var req struct {
		DeviceID  string `json:"device_id"`
		RuleID    string `json:"rule_id"`
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
		Reason    string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.StartTime == "" || req.EndTime == "" {
		return BadRequest(c, "start_time and end_time required")
	}
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	silence := &storage.SilenceRecord{
		ID:        "silence_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		DeviceID:  req.DeviceID,
		RuleID:    req.RuleID,
		StartTime: normalizeRFC3339(req.StartTime),
		EndTime:   normalizeRFC3339(req.EndTime),
		Reason:    req.Reason,
	}
	if user != nil {
		silence.Operator = user.Username
	}
	if err := cont.AlarmRepo.CreateSilence(silence); err != nil {
		logrus.WithError(err).Error("Failed to create alarm silence")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	return Created(c, silence)
}

// handleGetAlarmSilence gets a specific alarm silence period.
func handleGetAlarmSilence(c echo.Context) error {
	silenceID := c.Param("silence_id")
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	s, err := cont.AlarmRepo.GetSilence(silenceID)
	if err != nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if s == nil {
		return NotFound(c, "Silence not found")
	}
	return OK(c, s)
}

// handleCancelAlarmSilence cancels an alarm silence period.
func handleCancelAlarmSilence(c echo.Context) error {
	silenceID := c.Param("silence_id")
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if err := cont.AlarmRepo.CancelSilence(silenceID); err != nil {
		logrus.WithError(err).Error("Failed to cancel alarm silence")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	logrus.WithField("silence_id", silenceID).Info("Alarm silence cancelled")
	return OK(c, map[string]interface{}{
		"id":      silenceID,
		"deleted": true,
	})
}

// handleGetAlarmCorrelation returns correlated alarm groups.
func handleGetAlarmCorrelation(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	groups, _, err := buildAlarmCorrelationGroups(c)
	if err != nil {
		logrus.WithError(err).Error("Alarm correlation failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if offset > len(groups) {
		offset = len(groups)
	}
	end := offset + limit
	if end > len(groups) {
		end = len(groups)
	}
	return OK(c, map[string]interface{}{
		"groups": groups[offset:end],
		"limit":  limit,
		"offset": offset,
	})
}

// handleGetAlarmCorrelationStats returns correlation summary counts for the alarm correlation page.
func handleGetAlarmCorrelationStats(c echo.Context) error {
	groups, totalAlarms, err := buildAlarmCorrelationGroups(c)
	if err != nil {
		logrus.WithError(err).Error("Alarm correlation stats failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	suppressed := 0
	if cont := GetContainer(); cont != nil && cont.AlarmRepo != nil {
		if silences, err := cont.AlarmRepo.ListSilences(true); err == nil {
			suppressed = len(silences)
		}
	}
	return OK(c, map[string]interface{}{
		"total_alarms":       totalAlarms,
		"correlation_groups": len(groups),
		"root_causes":        len(groups),
		"suppressed":         suppressed,
	})
}

// handleGetAlarmCorrelationGroups returns correlated alarm groups within the requested time range.
func handleGetAlarmCorrelationGroups(c echo.Context) error {
	groups, _, err := buildAlarmCorrelationGroups(c)
	if err != nil {
		logrus.WithError(err).Error("Alarm correlation groups failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	return OK(c, map[string]interface{}{"items": groups})
}

// handleGetAlarmSuppressionRules lists suppression windows (silences) for the alarm correlation page.
func handleGetAlarmSuppressionRules(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.AlarmRepo == nil {
		return OK(c, map[string]interface{}{"items": []interface{}{}})
	}
	silences, err := cont.AlarmRepo.ListSilences(false)
	if err != nil {
		logrus.WithError(err).Error("List suppression rules failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	now := time.Now().Format(time.RFC3339)
	items := make([]interface{}, 0, len(silences))
	for _, s := range silences {
		var start, end time.Time
		if t, err := time.Parse(time.RFC3339, s.StartTime); err == nil {
			start = t
		}
		if t, err := time.Parse(time.RFC3339, s.EndTime); err == nil {
			end = t
		}
		patternParts := make([]string, 0, 3)
		if s.DeviceID != "" {
			patternParts = append(patternParts, "device:"+s.DeviceID)
		}
		if s.RuleID != "" {
			patternParts = append(patternParts, "rule:"+s.RuleID)
		}
		if s.AlarmID != "" {
			patternParts = append(patternParts, "alarm:"+s.AlarmID)
		}
		if len(patternParts) == 0 {
			patternParts = append(patternParts, "*")
		}
		name := s.Reason
		if name == "" {
			name = s.ID
		}
		items = append(items, map[string]interface{}{
			"id":           s.ID,
			"name":         name,
			"pattern":      strings.Join(patternParts, " "),
			"duration_sec": int(end.Sub(start).Seconds()),
			"enabled":      !s.Cancelled && s.EndTime > now,
			"start_time":   s.StartTime,
			"end_time":     s.EndTime,
			"operator":     s.Operator,
		})
	}
	return OK(c, map[string]interface{}{"items": items})
}

// correlationWindow is the max gap between two alarms on the same device to be
// considered part of one correlation group (5 minutes).
const correlationWindow = 5 * time.Minute

// buildAlarmCorrelationGroups clusters alarms per device: alarms on the same
// device whose fired_at gaps are within correlationWindow form one group; the
// earliest alarm is the root cause. Returns groups (cluster with >=2 alarms)
// and the total number of alarms in range.
func buildAlarmCorrelationGroups(c echo.Context) ([]map[string]interface{}, int, error) {
	cont := GetContainer()
	if cont == nil || cont.AlarmService == nil {
		return nil, 0, nil
	}
	filter := models.AlarmFilter{
		DeviceID:  c.QueryParam("device_id"),
		StartTime: normalizeRFC3339(c.QueryParam("start_time")),
		EndTime:   normalizeRFC3339(c.QueryParam("end_time")),
	}
	if filter.StartTime == "" && filter.EndTime == "" {
		filter.StartTime = time.Now().AddDate(0, 0, -7).Format(time.RFC3339)
	}
	alarms, _, err := cont.AlarmService.List(filter, 1, 1000)
	if err != nil {
		return nil, 0, err
	}

	deviceNames := map[string]string{}
	if cont.DeviceRepo != nil {
		devs, _, _ := cont.DeviceRepo.List(1, 1000)
		for _, d := range devs {
			deviceNames[d.DeviceID] = d.Name
		}
	}

	// Group alarms by device, keeping them sorted by fired_at.
	byDevice := map[string][]models.AlarmResponse{}
	for _, a := range alarms {
		byDevice[a.DeviceID] = append(byDevice[a.DeviceID], a)
	}

	groups := make([]map[string]interface{}, 0, len(byDevice))
	for deviceID, list := range byDevice {
		sort.Slice(list, func(i, j int) bool {
			return list[i].FiredAt < list[j].FiredAt
		})
		clusterStart := 0
		for i := 1; i <= len(list); i++ {
			split := i == len(list)
			if !split {
				if prev, err := time.Parse(time.RFC3339, list[i-1].FiredAt); err == nil {
					if cur, err2 := time.Parse(time.RFC3339, list[i].FiredAt); err2 == nil {
						if cur.Sub(prev) <= correlationWindow {
							continue
						}
					}
				}
				split = true
			}
			cluster := list[clusterStart:i]
			clusterStart = i
			if len(cluster) < 2 {
				continue
			}
			first, last := cluster[0], cluster[len(cluster)-1]
			firstUnix := int64(0)
			if t, err := time.Parse(time.RFC3339, first.FiredAt); err == nil {
				firstUnix = t.Unix()
			}
			alarmsOut := make([]map[string]interface{}, 0, len(cluster))
			for _, a := range cluster {
				name := deviceNames[a.DeviceID]
				if name == "" {
					name = a.DeviceID
				}
				alarmsOut = append(alarmsOut, map[string]interface{}{
					"alarm_id":    a.AlarmID,
					"device_id":   a.DeviceID,
					"device_name": name,
					"title":       a.Message,
					"severity":    a.Severity,
					"timestamp":   a.FiredAt,
					"status":      a.Status,
				})
			}
			groups = append(groups, map[string]interface{}{
				"id":               "corr_" + deviceID + "_" + strconv.FormatInt(firstUnix, 10),
				"device_id":        deviceID,
				"alarm_count":      len(cluster),
				"root_cause":       first.Message,
				"correlation_type": "device_window",
				"first_alarm_time": first.FiredAt,
				"last_alarm_time":  last.FiredAt,
				"alarms":           alarmsOut,
			})
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		fi, _ := groups[i]["first_alarm_time"].(string)
		fj, _ := groups[j]["first_alarm_time"].(string)
		return fi > fj
	})
	return groups, len(alarms), nil
}

// handleGetAlarmHistory returns alarm history for a rule.
func handleGetAlarmHistory(c echo.Context) error {
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}
	days, _ := strconv.Atoi(c.QueryParam("days"))
	if days <= 0 {
		days = 7
	}
	if days > 365 {
		days = 365
	}
	cont := GetContainer()
	if cont.AlarmService == nil {
		return ServiceUnavailable(c, "Alarm service not ready")
	}
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	alarms, total, err := cont.AlarmService.List(models.AlarmFilter{RuleID: ruleID, StartTime: since}, 1, 1000)
	if err != nil {
		logrus.WithError(err).Error("Get alarm history failed")
		return InternalError(c, "ERR_ALARM_LIST_FAILED")
	}
	if alarms == nil {
		alarms = []models.AlarmResponse{}
	}
	return OKPaged(c, alarms, total, 1, 1000)
}

// Data API routes

// RegisterDataRoutes registers data query API routes.
func RegisterDataRoutes(g *echo.Group) {
	g.GET("/query", handleQueryTimeseries, requirePermission(security.PermDataRead))
	g.GET("/stats", handleGetDataStats, requirePermission(security.PermDataRead))
	g.GET("/trend", handleQueryTrend, requirePermission(security.PermDataRead))
	g.GET("/correlation", handleGetDataCorrelation, requirePermission(security.PermDataRead))
	g.GET("/statistics", handleGetDataStatistics, requirePermission(security.PermDataRead))
	g.GET("/multi-point", handleQueryMultiPoint, requirePermission(security.PermDataRead))
	g.GET("/export", handleExportData, requirePermission(security.PermDataExport))
	g.GET("/export/history", handleExportHistory, requirePermission(security.PermDataExport))
	g.GET("/export/:id/download", handleDownloadExport, requirePermission(security.PermDataExport))
	g.POST("/export", handleExportConfig, requirePermission(security.PermDataExport))
	g.POST("/import", handleImportData, requirePermission(security.PermDataImport))
	g.GET("/import/template", handleImportTemplate, requirePermission(security.PermDataImport))
	g.POST("/downsample", handleDownsampleData, requirePermission(security.PermDataRead))
}

func handleQueryTimeseries(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	pointName := c.QueryParam("point_name")
	start := c.QueryParam("start")
	stop := c.QueryParam("stop")

	if deviceID == "" || pointName == "" || start == "" {
		return BadRequest(c, "device_id, point_name, and start are required")
	}

	startTime, err := parseTimeString(start)
	if err != nil {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}

	endTime := time.Now()
	if stop != "" {
		if t, err := parseTimeString(stop); err == nil {
			endTime = t
		}
	}

	cont := GetContainer()
	if cont.DataService == nil {
		return ServiceUnavailable(c, "Data service not ready")
	}

	data, err := cont.DataService.QueryHistory(deviceID, pointName, startTime, endTime)
	if err != nil {
		logrus.WithError(err).Error("Timeseries query failed")
		return InternalError(c, "ERR_DATA_QUERY_FAILED")
	}

	return OK(c, data)
}

func handleGetDataStats(c echo.Context) error {
	cont := GetContainer()

	totalPointsToday := int64(0)
	deviceStats := map[string]int{
		"total":   0,
		"online":  0,
		"offline": 0,
		"error":   0,
	}

	// Get total points today from scheduler stats
	if cont.Scheduler != nil {
		schedStats := cont.Scheduler.Stats()
		if tp, ok := schedStats["total_points"]; ok {
			if tpInt, ok := tp.(int64); ok {
				totalPointsToday = tpInt
			} else if tpInt, ok := tp.(int); ok {
				totalPointsToday = int64(tpInt)
			}
		}
	}

	// Get device stats
	if cont.DeviceRepo != nil {
		devs, _, _ := cont.DeviceRepo.List(1, 1000)
		deviceStats["total"] = len(devs)
		for _, d := range devs {
			if d.Status == "online" {
				deviceStats["online"]++
			} else if d.Status == "error" {
				deviceStats["error"]++
			} else {
				deviceStats["offline"]++
			}
		}
	}

	// Calculate success rate from scheduler
	successRate := 0.0
	if cont.Scheduler != nil {
		schedStats := cont.Scheduler.Stats()
		var totalCollects, totalErrors int64
		if tc, ok := schedStats["total_collects"]; ok {
			if v, ok := tc.(int64); ok {
				totalCollects = v
			} else if v, ok := tc.(int); ok {
				totalCollects = int64(v)
			}
		}
		if te, ok := schedStats["total_errors"]; ok {
			if v, ok := te.(int64); ok {
				totalErrors = v
			} else if v, ok := te.(int); ok {
				totalErrors = int64(v)
			}
		}
		if totalCollects > 0 {
			successRate = float64(totalCollects-totalErrors) / float64(totalCollects) * 100
		}
	}

	return OK(c, map[string]interface{}{
		"total_points_today": totalPointsToday,
		"device_stats":       deviceStats,
		"success_rate":       successRate,
	})
}

// maxChartSamples bounds the series returned by the comparison/correlation
// endpoints: the browser charts cannot render more, and an unbounded query on a
// busy tag would serialise hundreds of megabytes into one response.
const maxChartSamples = 500

// sampleValue converts a stored sample into a float, reporting whether the value
// is numeric. Booleans map to 1/0 and numeric strings are parsed, because tags
// of those types are stored as written by the driver.
func sampleValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// resolveQueryWindow reads the start/stop query parameters the data pages send,
// defaulting the end of the window to now.
func resolveQueryWindow(c echo.Context) (time.Time, time.Time, error) {
	startParam := c.QueryParam("start")
	if startParam == "" {
		startParam = c.QueryParam("start_time")
	}
	stopParam := c.QueryParam("stop")
	if stopParam == "" {
		stopParam = c.QueryParam("end_time")
	}
	start, err := parseTimeString(startParam)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end := time.Now()
	if stopParam != "" {
		if t, err := parseTimeString(stopParam); err == nil {
			end = t
		}
	}
	return start, end, nil
}

// queryDevicePoint fetches one tag's samples for a device inside a window.
func queryDevicePoint(deviceID, pointName string, start, end time.Time) ([]storage.PointData, error) {
	cont := GetContainer()
	if cont.TsStorage == nil {
		return nil, fmt.Errorf("time-series storage not ready")
	}
	return cont.TsStorage.QueryPoints(deviceID, pointName, start, end)
}

// strideSample keeps at most maxChartSamples entries, evenly spaced and always
// including the newest one, so a long window still renders as a trend line.
func strideSample[T any](items []T) []T {
	if len(items) <= maxChartSamples {
		return items
	}
	step := float64(len(items)) / float64(maxChartSamples)
	out := make([]T, 0, maxChartSamples)
	for i := 0; i < maxChartSamples; i++ {
		out = append(out, items[int(float64(i)*step)])
	}
	return out
}

// alignToTimeline projects samples onto grid timestamps with last-known-value
// semantics. Grid slots earlier than the first sample keep a nil Value.
func alignToTimeline(grid []time.Time, samples []storage.PointData) []storage.PointData {
	out := make([]storage.PointData, len(grid))
	cursor := -1
	for i, ts := range grid {
		for cursor+1 < len(samples) && !samples[cursor+1].Timestamp.After(ts) {
			cursor++
		}
		if cursor >= 0 {
			out[i] = samples[cursor]
		}
		out[i].Timestamp = ts
	}
	return out
}

func handleGetDataCorrelation(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	point1 := c.QueryParam("point1")
	point2 := c.QueryParam("point2")
	if deviceID == "" || point1 == "" || point2 == "" {
		return BadRequest(c, "device_id, point1, and point2 are required")
	}
	start, end, err := resolveQueryWindow(c)
	if err != nil {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}
	a, err := queryDevicePoint(deviceID, point1, start, end)
	if err != nil {
		logrus.WithError(err).Error("Correlation: query point1 failed")
		return InternalError(c, "ERR_DATA_QUERY_FAILED")
	}
	b, err := queryDevicePoint(deviceID, point2, start, end)
	if err != nil {
		logrus.WithError(err).Error("Correlation: query point2 failed")
		return InternalError(c, "ERR_DATA_QUERY_FAILED")
	}

	grid := make([]time.Time, 0, len(a))
	for _, s := range a {
		grid = append(grid, s.Timestamp)
	}
	aligned := alignToTimeline(grid, b)

	// Only pairs where both tags actually hold a number contribute; a boolean or
	// text tag aligned onto a numeric one would otherwise poison the sum.
	var xs, ys []float64
	for i, s := range a {
		x, okX := sampleValue(s.Value)
		y, okY := sampleValue(aligned[i].Value)
		if !okX || !okY || aligned[i].Value == nil {
			continue
		}
		xs = append(xs, x)
		ys = append(ys, y)
	}

	r := pearson(xs, ys)
	return OK(c, map[string]interface{}{
		"device_id":    deviceID,
		"point1":       point1,
		"point2":       point2,
		"correlation":  r,
		"sample_count": len(xs),
		"point_names":  []string{point1, point2},
		"correlation_matrix": [][]float64{
			{1, r},
			{r, 1},
		},
	})
}

// pearson returns the Pearson correlation coefficient, or 0 when fewer than two
// pairs or a constant series makes it undefined.
func pearson(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || n != len(ys) {
		return 0
	}
	var mx, my float64
	for i := 0; i < n; i++ {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(n)
	my /= float64(n)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dx := xs[i] - mx
		dy := ys[i] - my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	r := sxy / math.Sqrt(sxx*syy)
	if r > 1 {
		return 1
	}
	if r < -1 {
		return -1
	}
	return r
}

// maxTrendBuckets caps how many buckets one trend answer carries; the bucket
// width grows instead of the response when the requested window is finer.
const maxTrendBuckets = 500

// numericSamples returns the values a set of stored samples actually holds.
// Non-numeric tags (text, unparseable) are dropped rather than counted as 0.
func numericSamples(recs []storage.PointData) []float64 {
	out := make([]float64, 0, len(recs))
	for _, r := range recs {
		if v, ok := sampleValue(r.Value); ok {
			out = append(out, v)
		}
	}
	return out
}

// trendBucket summarises the samples that fell into one slice of the window.
// Buckets with no samples are omitted, so an outage reads as a gap on the
// chart rather than as a line that held its previous value.
type trendBucket struct {
	Start string  `json:"start"`
	End   string  `json:"end"`
	Count int     `json:"count"`
	Avg   float64 `json:"avg"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

func handleQueryTrend(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	pointName := c.QueryParam("point_name")
	if deviceID == "" || pointName == "" {
		return BadRequest(c, "device_id and point_name are required")
	}
	start, end, err := resolveQueryWindow(c)
	if err != nil {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}
	if !end.After(start) {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}
	// This endpoint answered a hardcoded empty trend, which looked identical to
	// "no samples in this window"; it now summarises the stored history.
	if GetContainer().TsStorage == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_TS_STORAGE_UNAVAILABLE", "time-series storage is not ready")
	}
	recs, err := queryDevicePoint(deviceID, pointName, start, end)
	if err != nil {
		logrus.WithError(err).Error("Trend: query failed")
		return InternalError(c, "ERR_DATA_QUERY_FAILED")
	}

	window := end.Sub(start)
	bucket := window / time.Duration(maxTrendBuckets)
	if b := c.QueryParam("bucket_size"); b != "" {
		if d, derr := time.ParseDuration(b); derr == nil && d > 0 {
			bucket = d
		}
	}
	if bucket < time.Second {
		bucket = time.Second
	}

	type acc struct {
		n        int
		sum      float64
		min, max float64
	}
	buckets := map[int]*acc{}
	sampled := 0
	for _, r := range recs {
		v, ok := sampleValue(r.Value)
		if !ok {
			continue
		}
		idx := int(r.Timestamp.Sub(start) / bucket)
		b := buckets[idx]
		if b == nil {
			b = &acc{min: v, max: v}
			buckets[idx] = b
		}
		b.n++
		b.sum += v
		if v < b.min {
			b.min = v
		}
		if v > b.max {
			b.max = v
		}
		sampled++
	}

	// Every non-empty bucket is reported, in window order: stopping at the first
	// empty index would truncate the trend at the oldest collection gap.
	indexes := make([]int, 0, len(buckets))
	for idx := range buckets {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	trend := make([]trendBucket, 0, len(indexes))
	for _, idx := range indexes {
		b := buckets[idx]
		trend = append(trend, trendBucket{
			Start: start.Add(time.Duration(idx) * bucket).Format(time.RFC3339),
			End:   start.Add(time.Duration(idx+1) * bucket).Format(time.RFC3339),
			Count: b.n,
			Avg:   b.sum / float64(b.n),
			Min:   b.min,
			Max:   b.max,
		})
	}
	return OK(c, map[string]interface{}{
		"device_id":      deviceID,
		"point_name":     pointName,
		"start":          start.Format(time.RFC3339),
		"end":            end.Format(time.RFC3339),
		"bucket_seconds": bucket.Seconds(),
		"sample_count":   sampled,
		"trend":          trend,
	})
}

func handleGetDataStatistics(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	pointName := c.QueryParam("point_name")
	if deviceID == "" || pointName == "" {
		return BadRequest(c, "device_id and point_name are required")
	}
	start, end, err := resolveQueryWindow(c)
	if err != nil || !end.After(start) {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}
	// The handler used to answer count/mean/stddev/min/max all 0 whatever was
	// asked, so an empty window and a real measurement were indistinguishable.
	if GetContainer().TsStorage == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_TS_STORAGE_UNAVAILABLE", "time-series storage is not ready")
	}
	recs, err := queryDevicePoint(deviceID, pointName, start, end)
	if err != nil {
		logrus.WithError(err).Error("Statistics: query failed")
		return InternalError(c, "ERR_DATA_QUERY_FAILED")
	}
	vals := numericSamples(recs)

	resp := map[string]interface{}{
		"device_id":  deviceID,
		"point_name": pointName,
		"start":      start.Format(time.RFC3339),
		"end":        end.Format(time.RFC3339),
		"count":      len(vals),
		// The aggregates stay absent until there are samples to average: mean 0
		// over no data is a number nobody measured.
		"mean":                nil,
		"stddev":              nil,
		"min":                 nil,
		"max":                 nil,
		"q1":                  nil,
		"median":              nil,
		"q3":                  nil,
		"p95":                 nil,
		"non_numeric_samples": len(recs) - len(vals),
	}
	if len(vals) > 0 {
		sorted := append([]float64(nil), vals...)
		sort.Float64s(sorted)
		var sum float64
		for _, v := range vals {
			sum += v
		}
		mean := sum / float64(len(vals))
		var varSum float64
		for _, v := range vals {
			varSum += (v - mean) * (v - mean)
		}
		resp["mean"] = mean
		resp["min"] = sorted[0]
		resp["max"] = sorted[len(sorted)-1]
		resp["median"] = percentileSorted(sorted, 0.5)
		resp["q1"] = percentileSorted(sorted, 0.25)
		resp["q3"] = percentileSorted(sorted, 0.75)
		resp["p95"] = percentileSorted(sorted, 0.95)
		if len(vals) > 1 {
			resp["stddev"] = math.Sqrt(varSum / float64(len(vals)-1))
		}
		// With one sample stddev stays null: 0 would claim the value is constant,
		// which a single reading cannot show.
	}
	return OK(c, resp)
}

// percentileSorted reads a quantile off an ascending sample set with
// nearest-rank indexing, so a single sample answers with its own value.
func percentileSorted(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func handleQueryMultiPoint(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	pointNames := c.QueryParam("point_names")
	if deviceID == "" || pointNames == "" {
		return BadRequest(c, "device_id and point_names are required")
	}
	start, end, err := resolveQueryWindow(c)
	if err != nil {
		return BadRequest(c, "ERR_DATA_INVALID_TIME_RANGE")
	}

	var names []string
	seen := map[string]bool{}
	for _, n := range strings.Split(pointNames, ",") {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return BadRequest(c, "device_id and point_names are required")
	}

	series := make(map[string][]storage.PointData, len(names))
	for _, n := range names {
		recs, err := queryDevicePoint(deviceID, n, start, end)
		if err != nil {
			logrus.WithError(err).WithField("point_name", n).Error("Multi-point query failed")
			return InternalError(c, "ERR_DATA_QUERY_FAILED")
		}
		series[n] = recs
	}

	// The comparison chart plots one value per index and labels the X axis from
	// the first requested tag, so every tag is resampled onto that timeline
	// (last-known-value) instead of being returned as an independent series.
	grid := make([]time.Time, 0, len(series[names[0]]))
	for _, s := range series[names[0]] {
		grid = append(grid, s.Timestamp)
	}
	if len(grid) == 0 {
		for _, n := range names {
			if len(series[n]) > len(grid) {
				grid = grid[:0]
				for _, s := range series[n] {
					grid = append(grid, s.Timestamp)
				}
			}
		}
	}
	if len(grid) > maxChartSamples {
		step := len(grid) / maxChartSamples
		thinned := make([]time.Time, 0, maxChartSamples)
		for i := 0; i < len(grid); i += step {
			thinned = append(thinned, grid[i])
		}
		thinned = append(thinned, grid[len(grid)-1])
		grid = thinned
	}

	out := make([]storage.PointData, 0, len(names)*len(grid))
	for _, n := range names {
		for _, s := range alignToTimeline(grid, series[n]) {
			if s.Value == nil {
				continue
			}
			s.DeviceID = deviceID
			s.PointName = n
			out = append(out, s)
		}
	}
	return OK(c, out)
}

// exportDir returns the directory where export files are persisted.
func exportDir() string {
	return filepath.Join("data", "exports")
}

// safeFilenamePart reduces a device/point identifier to filename-safe characters.
func safeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "unknown"
	}
	return out
}

func handleExportData(c echo.Context) error {
	deviceID := c.QueryParam("device_id")
	pointName := c.QueryParam("point_name")
	format := c.QueryParam("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		return BadRequest(c, "unsupported format: "+format)
	}
	if deviceID == "" {
		return BadRequest(c, "device_id is required")
	}

	startTime := time.Now().Add(-24 * time.Hour)
	endTime := time.Now()
	if s := c.QueryParam("start_time"); s != "" {
		t, err := parseTimeString(s)
		if err != nil {
			return BadRequest(c, "invalid start_time")
		}
		startTime = t
	}
	if s := c.QueryParam("end_time"); s != "" {
		t, err := parseTimeString(s)
		if err != nil {
			return BadRequest(c, "invalid end_time")
		}
		endTime = t
	}
	if endTime.Before(startTime) {
		return BadRequest(c, "end_time must be after start_time")
	}

	cont := GetContainer()
	if cont.TsStorage == nil {
		return ServiceUnavailable(c, "Time-series storage not ready")
	}

	records, err := cont.TsStorage.QueryPoints(deviceID, pointName, startTime, endTime)
	if err != nil {
		logrus.WithError(err).Error("Export: query failed")
		return InternalError(c, "export query failed")
	}
	if records == nil {
		records = []storage.PointData{}
	}

	var (
		contentType string
		body        []byte
	)
	switch format {
	case "json":
		contentType = "application/json"
		body, err = json.Marshal(records)
		if err != nil {
			return InternalError(c, "export marshal failed")
		}
	default: // csv
		contentType = "text/csv"
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		if err := w.Write([]string{"device_id", "point_name", "value", "quality", "timestamp"}); err != nil {
			return InternalError(c, "export write failed")
		}
		for _, r := range records {
			if err := w.Write([]string{
				r.DeviceID,
				r.PointName,
				fmt.Sprintf("%v", r.Value),
				r.Quality,
				r.Timestamp.Format(time.RFC3339),
			}); err != nil {
				return InternalError(c, "export write failed")
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return InternalError(c, "export write failed")
		}
		body = buf.Bytes()
	}

	// Persist the file so it can be re-downloaded from the export history.
	now := time.Now()
	filename := fmt.Sprintf("export_%s_%s.%s", safeFilenamePart(deviceID), now.Format("20060102_150405"), format)
	if err := os.MkdirAll(exportDir(), 0755); err != nil {
		return InternalError(c, "export dir create failed")
	}
	if err := os.WriteFile(filepath.Join(exportDir(), filename), body, 0644); err != nil {
		logrus.WithError(err).Error("Export: failed to write export file")
		return InternalError(c, "export write failed")
	}

	if cont.Database != nil {
		if _, err := cont.Database.InsertExportRecord(storage.ExportRecord{
			Filename:  filename,
			DeviceID:  deviceID,
			PointName: pointName,
			StartTime: startTime.Format(time.RFC3339),
			EndTime:   endTime.Format(time.RFC3339),
			Format:    format,
			RowCount:  int64(len(records)),
			SizeBytes: int64(len(body)),
			Status:    "success",
			CreatedAt: now.Format(time.RFC3339),
		}); err != nil {
			logrus.WithError(err).Warn("Export: failed to record export history")
		}
		// Cap on-disk export files so they don't accumulate unbounded.
		if removed, err := cont.Database.PruneExportRecords(50); err == nil {
			for _, r := range removed {
				if r.Filename == "" || strings.ContainsAny(r.Filename, `/\\`) || strings.Contains(r.Filename, "..") {
					continue
				}
				os.Remove(filepath.Join(exportDir(), r.Filename))
			}
		}
	}

	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="`+sanitizeForHeader(filename)+`"`)
	return c.Blob(200, contentType, body)
}

func handleExportHistory(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return OK(c, []storage.ExportRecord{})
	}
	records, err := cont.Database.ListExportRecords(100)
	if err != nil {
		logrus.WithError(err).Error("Export history: list failed")
		return InternalError(c, "list export history failed")
	}
	if records == nil {
		records = []storage.ExportRecord{}
	}
	return OK(c, records)
}

func handleDownloadExport(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return BadRequest(c, "invalid export id")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return InternalError(c, "database not ready")
	}
	rec, err := cont.Database.GetExportRecord(id)
	if err != nil {
		return NotFound(c, "export record not found")
	}
	// Filenames are generated server-side; refuse anything that could escape the export dir.
	if rec.Filename == "" || strings.ContainsAny(rec.Filename, `/\\`) || strings.Contains(rec.Filename, "..") {
		return BadRequest(c, "invalid filename")
	}
	fullPath := filepath.Join(exportDir(), rec.Filename)
	if _, err := os.Stat(fullPath); err != nil {
		return NotFound(c, "export file not found")
	}
	return c.Attachment(fullPath, rec.Filename)
}

func handleExportConfig(c echo.Context) error {
	type ConfigExportRequest struct {
		Scope  string   `json:"scope"`
		IDs    []string `json:"ids"`
		Format string   `json:"format"`
	}
	var req ConfigExportRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.Scope == "" {
		req.Scope = "all"
	}
	if req.Format == "" {
		req.Format = "json"
	}

	// No config-export implementation exists in this build; answering 200 with
	// "{}" content made the caller believe a real export had been produced.
	// The timeseries export lives on GET /data/export (handleExportData).
	return ErrorCode(c, http.StatusNotImplemented, "ERR_CONFIG_EXPORT_UNSUPPORTED",
		"ERR_CONFIG_EXPORT_UNSUPPORTED")
}

// handleImportData imports time-series rows from an uploaded CSV or JSON file.
// CSV header: device_id,point_name,value,quality,timestamp (same as handleExportData).
func handleImportData(c echo.Context) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return BadRequest(c, "file is required")
	}
	const maxImportSize = 50 * 1024 * 1024
	if fileHeader.Size > maxImportSize {
		return BadRequest(c, "file too large (max 50MB)")
	}
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if ext != ".csv" && ext != ".json" {
		return BadRequest(c, "unsupported file type: "+ext)
	}
	src, err := fileHeader.Open()
	if err != nil {
		return BadRequest(c, "cannot read uploaded file")
	}
	defer src.Close()
	content, err := io.ReadAll(io.LimitReader(src, maxImportSize))
	if err != nil {
		return BadRequest(c, "cannot read uploaded file")
	}

	cont := GetContainer()
	if cont.TsStorage == nil {
		return ServiceUnavailable(c, "Time-series storage not ready")
	}

	var (
		records   []storage.PointData
		rowErrors []map[string]interface{}
		total     int
		addErr    = func(row int, msg string) {
			if len(rowErrors) < 100 {
				rowErrors = append(rowErrors, map[string]interface{}{"row": row, "message": msg})
			}
		}
	)

	switch ext {
	case ".csv":
		reader := csv.NewReader(bytes.NewReader(content))
		reader.FieldsPerRecord = -1
		rows, err := reader.ReadAll()
		if err != nil {
			return BadRequest(c, "invalid CSV: "+err.Error())
		}
		if len(rows) == 0 {
			return BadRequest(c, "empty CSV file")
		}
		header := rows[0]
		col := func(name string) int {
			for i, h := range header {
				if strings.EqualFold(strings.TrimSpace(h), name) {
					return i
				}
			}
			return -1
		}
		iDev, iPoint, iValue, iQuality, iTs := col("device_id"), col("point_name"), col("value"), col("quality"), col("timestamp")
		if iDev < 0 || iPoint < 0 || iValue < 0 || iTs < 0 {
			return BadRequest(c, "CSV must contain columns: device_id, point_name, value, timestamp")
		}
		for i, row := range rows[1:] {
			total++
			rowNum := i + 2 // 1-based, +1 for header
			dev := ""
			point := ""
			val := ""
			quality := ""
			ts := ""
			if iDev < len(row) {
				dev = strings.TrimSpace(row[iDev])
			}
			if iPoint < len(row) {
				point = strings.TrimSpace(row[iPoint])
			}
			if iValue < len(row) {
				val = strings.TrimSpace(row[iValue])
			}
			if iQuality < len(row) {
				quality = strings.TrimSpace(row[iQuality])
			}
			if iTs < len(row) {
				ts = strings.TrimSpace(row[iTs])
			}
			p, perr := parseImportRow(dev, point, val, quality, ts)
			if perr != "" {
				addErr(rowNum, perr)
				continue
			}
			records = append(records, p)
		}
	default: // .json
		var raw []map[string]interface{}
		if err := json.Unmarshal(content, &raw); err != nil {
			return BadRequest(c, "invalid JSON: expected an array of row objects")
		}
		total = len(raw)
		for i, item := range raw {
			rowNum := i + 1
			get := func(k string) string {
				if v, ok := item[k]; ok {
					return fmt.Sprintf("%v", v)
				}
				return ""
			}
			p, perr := parseImportRow(get("device_id"), get("point_name"), get("value"), get("quality"), get("timestamp"))
			if perr != "" {
				addErr(rowNum, perr)
				continue
			}
			records = append(records, p)
		}
	}

	if len(records) > 0 {
		if err := cont.TsStorage.WritePoints(records); err != nil {
			logrus.WithError(err).Error("Import: write points failed")
			return InternalError(c, "import write failed")
		}
	}

	// device_id label: the single common device, "mixed" when rows span several.
	devSet := map[string]bool{}
	for _, p := range records {
		devSet[p.DeviceID] = true
	}
	deviceLabel := ""
	switch len(devSet) {
	case 1:
		for k := range devSet {
			deviceLabel = k
		}
	case 0:
		deviceLabel = "-"
	default:
		deviceLabel = fmt.Sprintf("%d devices", len(devSet))
	}

	return OK(c, map[string]interface{}{
		"total":     total,
		"success":   len(records),
		"failed":    total - len(records),
		"device_id": deviceLabel,
		"errors":    rowErrors,
	})
}

// parseImportRow validates one import row; returns "" on success.
func parseImportRow(deviceID, pointName, value, quality, ts string) (storage.PointData, string) {
	if deviceID == "" {
		return storage.PointData{}, "device_id is required"
	}
	if pointName == "" {
		return storage.PointData{}, "point_name is required"
	}
	if ts == "" {
		return storage.PointData{}, "timestamp is required"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return storage.PointData{}, "invalid timestamp (want RFC3339, e.g. 2026-01-02T15:04:05+08:00): " + ts
	}
	switch quality {
	case "":
		quality = "good"
	case "good", "bad", "uncertain":
	default:
		return storage.PointData{}, "invalid quality (want good/bad/uncertain): " + quality
	}
	p := storage.PointData{
		DeviceID:  deviceID,
		PointName: pointName,
		Quality:   quality,
		Timestamp: t,
	}
	if v, err := strconv.ParseFloat(value, 64); err == nil {
		p.Value = v
	} else if value != "" {
		p.Value = value
	}
	return p, ""
}

// handleImportTemplate serves a sample CSV/JSON file showing the expected import format.
func handleImportTemplate(c echo.Context) error {
	format := c.QueryParam("format")
	if format == "" {
		format = "csv"
	}
	now := time.Now().Add(-time.Hour).Format(time.RFC3339)
	var contentType string
	var filename string
	var body []byte
	switch format {
	case "csv":
		contentType = "text/csv"
		filename = "import_template.csv"
		body = []byte("device_id,point_name,value,quality,timestamp\n" +
			"dev-example,temp,23.5,good," + now + "\n" +
			"dev-example,status,normal,bad," + now + "\n")
	case "json":
		contentType = "application/json"
		filename = "import_template.json"
		tpl, _ := json.MarshalIndent([]map[string]interface{}{
			{"device_id": "dev-example", "point_name": "temp", "value": 23.5, "quality": "good", "timestamp": now},
			{"device_id": "dev-example", "point_name": "status", "value": "normal", "quality": "bad", "timestamp": now},
		}, "", "  ")
		body = append(tpl, '\n')
	default:
		return BadRequest(c, "unsupported template format: "+format+" (use csv or json)")
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="`+filename+`"`)
	return c.Blob(200, contentType, body)
}

func handleDownsampleData(c echo.Context) error {
	type DownsampleRequest struct {
		DeviceID  string `json:"device_id"`
		PointName string `json:"point_name"`
		Interval  string `json:"interval"`
		AggFn     string `json:"agg_fn"`
	}
	var req DownsampleRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	return OK(c, map[string]interface{}{
		"device_id":  req.DeviceID,
		"point_name": req.PointName,
		"interval":   req.Interval,
		"agg_fn":     req.AggFn,
		"buckets":    []interface{}{},
	})
}

// parseTimeString parses time strings in RFC3339 or relative (-1h) format.
func parseTimeString(s string) (time.Time, error) {
	// Try RFC3339
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Try relative time (e.g., -1h, -30m, -2d)
	if len(s) > 2 && s[0] == '-' {
		unit := s[len(s)-1]
		numStr := s[1 : len(s)-1]
		var num int
		for _, c := range numStr {
			if c < '0' || c > '9' {
				return time.Time{}, &echo.HTTPError{Code: 400, Message: "invalid time"}
			}
			num = num*10 + int(c-'0')
		}
		now := time.Now()
		switch unit {
		case 's':
			return now.Add(-time.Duration(num) * time.Second), nil
		case 'm':
			return now.Add(-time.Duration(num) * time.Minute), nil
		case 'h':
			return now.Add(-time.Duration(num) * time.Hour), nil
		case 'd':
			return now.AddDate(0, 0, -num), nil
		case 'w':
			return now.AddDate(0, 0, -num*7), nil
		}
	}
	return time.Time{}, &echo.HTTPError{Code: 400, Message: "invalid time format"}
}

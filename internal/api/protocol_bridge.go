package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/engine"
	"edgelite/internal/models"
)

// Protocol bridges are configured through /api/v1/bridge/bridges and stored in
// the system_settings table, then replayed into engine.ProtocolBridgeManager so
// live collection traffic is forwarded to the target device. Before this file the
// four handlers answered with hardcoded JSON and echoed the request body, so a
// "created" bridge never existed and a "deleted" one never went away.

const bridgeSettingsKey = "protocol_bridges"

// bridgeFeedBuffer bounds the queue between the event bus and the bridge worker.
// The worker performs device writes, which can take seconds; the event bus
// dispatch loop must never wait for them.
const bridgeFeedBuffer = 256

var (
	bridgeFeedCh      chan bridgeFeedItem
	bridgeFeedDropped atomic.Int64
)

type bridgeFeedItem struct {
	deviceID string
	point    string
	value    interface{}
	at       time.Time
}

// bridgeRuleRecord is one source-point -> target-point mapping.
type bridgeRuleRecord struct {
	RuleID         string  `json:"rule_id"`
	SourcePoint    string  `json:"source_point"`
	TargetPoint    string  `json:"target_point"`
	ConversionType string  `json:"conversion_type"`
	Scale          float64 `json:"scale"`
	Offset         float64 `json:"offset"`
	Enabled        bool    `json:"enabled"`
}

// bridgeRecord is a persisted bridge definition.
type bridgeRecord struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	SourceDevice string             `json:"source_device"`
	TargetDevice string             `json:"target_device"`
	Enabled      bool               `json:"enabled"`
	Rules        []bridgeRuleRecord `json:"rules"`
	CreatedAt    string             `json:"created_at"`
	UpdatedAt    string             `json:"updated_at"`
}

func loadBridgeRecords(cont *ServiceContainer) ([]bridgeRecord, error) {
	if cont.Database == nil {
		return nil, fmt.Errorf("database not ready")
	}
	raw, err := cont.Database.GetSetting(bridgeSettingsKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var list []bridgeRecord
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("ERR_PROTOCOL_BRIDGE_STORE_CORRUPT: %w", err)
	}
	return list, nil
}

func saveBridgeRecords(cont *ServiceContainer, list []bridgeRecord) error {
	if cont.Database == nil {
		return fmt.Errorf("database not ready")
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return cont.Database.SetSetting(bridgeSettingsKey, string(encoded))
}

var bridgeConversionTypes = map[string]bool{
	"":            true,
	"passthrough": true,
	"linear":      true,
	"scale":       true,
	"bool_to_int": true,
	"int_to_bool": true,
}

// validateBridgeRecord normalises and checks a definition. Errors are ERR_* codes
// so the UI can translate them instead of showing a bare 400.
func validateBridgeRecord(cont *ServiceContainer, rec *bridgeRecord) error {
	rec.Name = strings.TrimSpace(rec.Name)
	rec.SourceDevice = strings.TrimSpace(rec.SourceDevice)
	rec.TargetDevice = strings.TrimSpace(rec.TargetDevice)
	if rec.Name == "" {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_NAME_REQUIRED")
	}
	if rec.SourceDevice == "" || rec.TargetDevice == "" {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_DEVICES_REQUIRED")
	}
	if rec.SourceDevice == rec.TargetDevice {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_SAME_DEVICE")
	}
	if cont.DeviceRepo == nil {
		return fmt.Errorf("ERR_COMMON_DB_NOT_READY")
	}
	source, err := cont.DeviceRepo.Get(rec.SourceDevice)
	if err != nil || source == nil {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_SOURCE_DEVICE_UNKNOWN: %s", rec.SourceDevice)
	}
	target, err := cont.DeviceRepo.Get(rec.TargetDevice)
	if err != nil || target == nil {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_TARGET_DEVICE_UNKNOWN: %s", rec.TargetDevice)
	}
	if len(rec.Rules) == 0 {
		return fmt.Errorf("ERR_PROTOCOL_BRIDGE_RULES_REQUIRED")
	}
	seen := map[string]bool{}
	for i := range rec.Rules {
		rule := &rec.Rules[i]
		rule.SourcePoint = strings.TrimSpace(rule.SourcePoint)
		rule.TargetPoint = strings.TrimSpace(rule.TargetPoint)
		rule.ConversionType = strings.TrimSpace(rule.ConversionType)
		if rule.SourcePoint == "" || rule.TargetPoint == "" {
			return fmt.Errorf("ERR_PROTOCOL_BRIDGE_RULE_POINT_REQUIRED")
		}
		if !bridgeConversionTypes[rule.ConversionType] {
			return fmt.Errorf("ERR_PROTOCOL_BRIDGE_CONVERSION_UNSUPPORTED: %s", rule.ConversionType)
		}
		// scale==0 can only come from an omitted field; zeroing every value would
		// silently destroy the signal, so treat it as "no scaling".
		if rule.Scale == 0 && (rule.ConversionType == "linear" || rule.ConversionType == "scale") {
			rule.Scale = 1
		}
		key := rule.SourcePoint + "\x00" + rule.TargetPoint
		if seen[key] {
			return fmt.Errorf("ERR_PROTOCOL_BRIDGE_RULE_DUPLICATE: %s -> %s", rule.SourcePoint, rule.TargetPoint)
		}
		seen[key] = true
		// Only validate against declared points when the device publishes a point
		// list; a typo in a mapping is otherwise invisible until it never fires.
		if len(source.Points) > 0 && pointDef(source.Points, rule.SourcePoint) == nil {
			return fmt.Errorf("ERR_PROTOCOL_BRIDGE_SOURCE_POINT_UNKNOWN: %s.%s", rec.SourceDevice, rule.SourcePoint)
		}
		if len(target.Points) > 0 {
			if pointDef(target.Points, rule.TargetPoint) == nil {
				return fmt.Errorf("ERR_PROTOCOL_BRIDGE_TARGET_POINT_UNKNOWN: %s.%s", rec.TargetDevice, rule.TargetPoint)
			}
			if readOnlyPointName(target.Points, rule.TargetPoint) {
				return fmt.Errorf("ERR_PROTOCOL_BRIDGE_TARGET_READ_ONLY: %s.%s", rec.TargetDevice, rule.TargetPoint)
			}
		}
		if rule.RuleID == "" {
			rule.RuleID = fmt.Sprintf("r%d", i+1)
		}
	}
	return nil
}

func pointDef(points []models.PointDef, name string) *models.PointDef {
	for i := range points {
		if points[i].Name == name {
			return &points[i]
		}
	}
	return nil
}

func readOnlyPointName(points []models.PointDef, name string) bool {
	p := pointDef(points, name)
	if p == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.AccessMode)) {
	case "read", "readonly", "read_only":
		return true
	}
	return false
}

// bridgeGraphHasCycle reports whether adding rec to others closes a device-level
// loop. A -> B and B -> A would forward each other's writes forever, so the
// second bridge is rejected at creation time instead of oscillating at runtime.
func bridgeGraphHasCycle(others []bridgeRecord, rec bridgeRecord) bool {
	edges := map[string][]string{}
	add := func(from, to string) { edges[from] = append(edges[from], to) }
	for _, o := range others {
		if o.ID == rec.ID || !o.Enabled {
			continue
		}
		add(o.SourceDevice, o.TargetDevice)
	}
	if rec.Enabled {
		add(rec.SourceDevice, rec.TargetDevice)
	}
	visited := map[string]int{}
	var walk func(node string) bool
	walk = func(node string) bool {
		visited[node]++
		if visited[node] > 1 {
			return true
		}
		for _, next := range edges[node] {
			if walk(next) {
				return true
			}
		}
		visited[node]--
		return false
	}
	for node := range edges {
		if walk(node) {
			return true
		}
	}
	return false
}

func bridgeToManagerBridge(rec bridgeRecord) *engine.ProtocolBridge {
	bridge := engine.NewProtocolBridge(rec.ID, rec.Name, rec.SourceDevice, rec.TargetDevice)
	bridge.SetEnabled(rec.Enabled)
	for _, rule := range rec.Rules {
		bridge.AddRule(&engine.MappingRule{
			RuleID:         rule.RuleID,
			SourceDeviceID: rec.SourceDevice,
			SourcePoint:    rule.SourcePoint,
			TargetDeviceID: rec.TargetDevice,
			TargetPoint:    rule.TargetPoint,
			ConversionType: rule.ConversionType,
			Scale:          rule.Scale,
			Offset:         rule.Offset,
			Enabled:        rule.Enabled,
		})
	}
	return bridge
}

// applyBridgesToManager makes the live manager match the persisted definitions.
func applyBridgesToManager(cont *ServiceContainer, list []bridgeRecord) {
	if cont.ProtocolBridge == nil {
		return
	}
	next := make([]*engine.ProtocolBridge, 0, len(list))
	for _, rec := range list {
		next = append(next, bridgeToManagerBridge(rec))
	}
	cont.ProtocolBridge.SetBridges(next)
}

// bridgeView merges a persisted definition with live manager stats.
func bridgeView(cont *ServiceContainer, rec bridgeRecord) map[string]interface{} {
	stats := map[string]interface{}{}
	if cont.ProtocolBridge != nil {
		if s := cont.ProtocolBridge.GetBridgeStats(rec.ID); s != nil {
			stats = s
		}
	}
	rules := make([]map[string]interface{}, 0, len(rec.Rules))
	for _, r := range rec.Rules {
		rules = append(rules, map[string]interface{}{
			"rule_id":        r.RuleID,
			"source_point":   r.SourcePoint,
			"target_point":   r.TargetPoint,
			"conversion_type": r.ConversionType,
			"scale":          r.Scale,
			"offset":         r.Offset,
			"enabled":        r.Enabled,
		})
	}
	return map[string]interface{}{
		"id":            rec.ID,
		"name":          rec.Name,
		"source_device": rec.SourceDevice,
		"target_device": rec.TargetDevice,
		"enabled":       rec.Enabled,
		"rules":         rules,
		"rule_count":    len(rec.Rules),
		"created_at":    rec.CreatedAt,
		"updated_at":    rec.UpdatedAt,
		"stats":         stats,
	}
}

// WireProtocolBridge installs the write sink, loads persisted bridges and feeds
// collected data into the manager. Called once from main after the manager starts.
func WireProtocolBridge(cont *ServiceContainer) {
	if cont == nil || cont.ProtocolBridge == nil {
		logrus.Warn("Protocol bridge manager unavailable, /bridge routes will report disabled")
		return
	}
	cont.ProtocolBridge.SetWriteSink(func(ctx context.Context, deviceID, point string, value interface{}) error {
		if cont.DeviceService == nil {
			return fmt.Errorf("device service not ready")
		}
		writeCtx, cancel := context.WithTimeout(ctx, bridgeWriteTimeout)
		defer cancel()
		return cont.DeviceService.WritePoint(writeCtx, deviceID, &models.WritePointRequest{Point: point, Value: value})
	})

	list, err := loadBridgeRecords(cont)
	if err != nil {
		logrus.WithError(err).Error("Protocol bridges could not be loaded, no bridge will forward data")
	} else {
		applyBridgesToManager(cont, list)
		logrus.WithField("bridges", len(list)).Info("Protocol bridges loaded from database")
	}

	if cont.EventBus == nil {
		logrus.Warn("Event bus unavailable, protocol bridges will not receive collected data")
		return
	}
	bridgeFeedCh = make(chan bridgeFeedItem, bridgeFeedBuffer)
	cont.EventBus.Subscribe(engine.EventTypeDataCollected, func(event engine.Event) {
		payload, ok := event.Data.(engine.DataCollectedEvent)
		if !ok {
			return
		}
		for _, p := range payload.Points {
			select {
			case bridgeFeedCh <- bridgeFeedItem{deviceID: payload.DeviceID, point: p.PointName, value: p.Value, at: time.Now()}:
			default:
				// Counting the drop is the point: without it the bridge stats look
				// like a working link that simply never saw any data.
				bridgeFeedDropped.Add(1)
			}
		}
	})
	manager := cont.ProtocolBridge
	go func() {
		// Each write is device I/O; doing it on the event-bus dispatch goroutine
		// would stall alarm and websocket delivery for every other subscriber.
		for item := range bridgeFeedCh {
			if !manager.IsStarted() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), bridgeWriteTimeout)
			if err := manager.UpdateSourceData(ctx, item.deviceID, item.point, item.value); err != nil {
				logrus.WithError(err).Warn("Protocol bridge feed failed")
			}
			cancel()
		}
	}()
}

const bridgeWriteTimeout = 5 * time.Second

func bridgeStoreError(c echo.Context, err error) error {
	if strings.Contains(err.Error(), "ERR_PROTOCOL_BRIDGE_STORE_CORRUPT") {
		return ErrorCode(c, http.StatusConflict, "ERR_PROTOCOL_BRIDGE_STORE_CORRUPT", err.Error())
	}
	return InternalError(c, "ERR_PROTOCOL_BRIDGE_STORE_FAILED")
}

func handleGetProtocolBridgeStatus(c echo.Context) error {
	cont := GetContainer()
	if cont.ProtocolBridge == nil {
		return OK(c, map[string]interface{}{
			"enabled": false,
			"reason":  "ERR_PROTOCOL_BRIDGE_MANAGER_UNAVAILABLE",
		})
	}
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	active := 0
	for _, rec := range records {
		if rec.Enabled {
			active++
		}
	}
	transferred, errs := cont.ProtocolBridge.Totals()
	return OK(c, map[string]interface{}{
		"enabled":               cont.ProtocolBridge.IsStarted(),
		"configured_bridges":    len(records),
		"active_bridges":        active,
		"sink_wired":            cont.ProtocolBridge.SinkWired(),
		"total_transferred":     transferred,
		"total_errors":          errs,
		"dropped_samples":       bridgeFeedDropped.Load(),
		"feed_configured":       bridgeFeedCh != nil,
		"persistence":           "database:system_settings",
		"supported_conversions": []string{"passthrough", "linear", "scale", "bool_to_int", "int_to_bool"},
	})
}

func handleListProtocolBridges(c echo.Context) error {
	cont := GetContainer()
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	views := make([]map[string]interface{}, 0, len(records))
	for _, rec := range records {
		views = append(views, bridgeView(cont, rec))
	}
	sort.Slice(views, func(i, j int) bool {
		si, _ := views[i]["created_at"].(string)
		sj, _ := views[j]["created_at"].(string)
		return si > sj
	})
	return OK(c, map[string]interface{}{"bridges": views, "total": len(views)})
}

func findBridgeRecord(list []bridgeRecord, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func handleGetProtocolBridge(c echo.Context) error {
	cont := GetContainer()
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	idx := findBridgeRecord(records, c.Param("id"))
	if idx < 0 {
		return NotFound(c, "ERR_PROTOCOL_BRIDGE_NOT_FOUND")
	}
	return OK(c, bridgeView(cont, records[idx]))
}

func handleCreateProtocolBridge(c echo.Context) error {
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	var rec bridgeRecord
	if err := c.Bind(&rec); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	rec.ID = ""
	rec.CreatedAt = ""
	if err := validateBridgeRecord(cont, &rec); err != nil {
		return badRequestErr(c, err)
	}
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	if bridgeGraphHasCycle(records, rec) {
		return Conflict(c, "ERR_PROTOCOL_BRIDGE_CYCLE")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec.ID = "pb-" + uuid.New().String()[:8]
	rec.CreatedAt = now
	rec.UpdatedAt = now
	records = append(records, rec)
	if err := saveBridgeRecords(cont, records); err != nil {
		return bridgeStoreError(c, err)
	}
	applyBridgesToManager(cont, records)
	logrus.WithFields(logrus.Fields{
		"bridge_id":     rec.ID,
		"source_device": rec.SourceDevice,
		"target_device": rec.TargetDevice,
		"rules":         len(rec.Rules),
	}).Info("Protocol bridge created")
	return Created(c, bridgeView(cont, rec))
}

func handleUpdateProtocolBridge(c echo.Context) error {
	cont := GetContainer()
	id := c.Param("id")
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	idx := findBridgeRecord(records, id)
	if idx < 0 {
		return NotFound(c, "ERR_PROTOCOL_BRIDGE_NOT_FOUND")
	}
	var rec bridgeRecord
	if err := c.Bind(&rec); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	rec.ID = id
	rec.CreatedAt = records[idx].CreatedAt
	if err := validateBridgeRecord(cont, &rec); err != nil {
		return badRequestErr(c, err)
	}
	others := make([]bridgeRecord, 0, len(records)-1)
	for i, o := range records {
		if i != idx {
			others = append(others, o)
		}
	}
	if bridgeGraphHasCycle(others, rec) {
		return Conflict(c, "ERR_PROTOCOL_BRIDGE_CYCLE")
	}
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	records[idx] = rec
	if err := saveBridgeRecords(cont, records); err != nil {
		return bridgeStoreError(c, err)
	}
	applyBridgesToManager(cont, records)
	return OK(c, bridgeView(cont, rec))
}

func handleSetProtocolBridgeEnabled(enabled bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		cont := GetContainer()
		id := c.Param("id")
		records, err := loadBridgeRecords(cont)
		if err != nil {
			return bridgeStoreError(c, err)
		}
		idx := findBridgeRecord(records, id)
		if idx < 0 {
			return NotFound(c, "ERR_PROTOCOL_BRIDGE_NOT_FOUND")
		}
		if enabled && bridgeGraphHasCycle(withoutRecord(records, idx), records[idx]) {
			return Conflict(c, "ERR_PROTOCOL_BRIDGE_CYCLE")
		}
		records[idx].Enabled = enabled
		records[idx].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := saveBridgeRecords(cont, records); err != nil {
			return bridgeStoreError(c, err)
		}
		applyBridgesToManager(cont, records)
		logrus.WithField("bridge_id", id).WithField("enabled", enabled).Info("Protocol bridge enabled state changed")
		return OK(c, bridgeView(cont, records[idx]))
	}
}

func withoutRecord(list []bridgeRecord, skip int) []bridgeRecord {
	out := make([]bridgeRecord, 0, len(list)-1)
	for i, rec := range list {
		if i != skip {
			out = append(out, rec)
		}
	}
	return out
}

func handleDeleteProtocolBridge(c echo.Context) error {
	cont := GetContainer()
	id := c.Param("id")
	records, err := loadBridgeRecords(cont)
	if err != nil {
		return bridgeStoreError(c, err)
	}
	idx := findBridgeRecord(records, id)
	if idx < 0 {
		return NotFound(c, "ERR_PROTOCOL_BRIDGE_NOT_FOUND")
	}
	records = append(records[:idx], records[idx+1:]...)
	if err := saveBridgeRecords(cont, records); err != nil {
		return bridgeStoreError(c, err)
	}
	wasLive := cont.ProtocolBridge != nil && cont.ProtocolBridge.GetBridge(id) != nil
	applyBridgesToManager(cont, records)
	stillLive := cont.ProtocolBridge != nil && cont.ProtocolBridge.GetBridge(id) != nil
	removed := wasLive && !stillLive
	logrus.WithFields(logrus.Fields{
		"bridge_id":             id,
		"was_live":              wasLive,
		"removed_from_manager":  removed,
	}).Info("Protocol bridge deleted")
	return OK(c, map[string]interface{}{"deleted": id, "was_live": wasLive, "removed_from_manager": removed})
}

// badRequestErr maps a validation error onto the ERR_* code the message already
// carries. The code keeps its ":detail" suffix because the client splits on the
// colon to interpolate the offending device/point into the translated text.
func badRequestErr(c echo.Context, err error) error {
	msg := strings.TrimSpace(err.Error())
	if strings.HasPrefix(msg, "ERR_") {
		return ErrorCode(c, http.StatusBadRequest, msg, msg)
	}
	return BadRequest(c, "ERR_PROTOCOL_BRIDGE_INVALID")
}

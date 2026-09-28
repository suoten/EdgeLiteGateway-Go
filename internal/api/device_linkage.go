package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// Device linkage was a CRUD-only feature: rules were stored, listed and toggled,
// but nothing at runtime ever read the table, so a rule could never fire and
// trigger_count stayed 0 forever. These helpers connect the table to the
// collection pipeline: rules are loaded into an evaluator, samples are fed from
// the event bus, a satisfied rule writes through the same guarded WritePoint path
// the UI uses, and the trigger counter is written back.

const (
	linkageFeedBuffer   = 256
	linkageWriteTimeout = 5 * time.Second
)

var (
	linkageFeedCh      chan linkageFeedItem
	linkageFeedDropped atomic.Int64
)

type linkageFeedItem struct {
	deviceID string
	point    string
	value    interface{}
}

// linkageConditionOps is what the evaluator understands; anything else is a typo
// that would silently never trigger.
var linkageConditionOps = map[string]bool{
	">": true, "<": true, ">=": true, "<=": true, "==": true, "!=": true,
}

// linkageTargetValue converts the stored target_value string into the Go type
// matching the target point's declared data type. A digital point written with
// the string "1" would otherwise be rejected (or worse, truncated) by the driver.
func linkageTargetValue(dataType, raw string) interface{} {
	dt := strings.ToLower(strings.TrimSpace(dataType))
	trimmed := strings.TrimSpace(raw)
	switch dt {
	case "bool", "boolean":
		b, err := strconv.ParseBool(trimmed)
		if err != nil {
			return trimmed
		}
		return b
	case "int", "int32", "int64", "integer", "long":
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return int64(f)
		}
		return trimmed
	case "float", "float32", "float64", "double", "number":
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
		return trimmed
	}
	if trimmed == "" {
		return raw
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f
	}
	return raw
}

// buildLinkageRules turns persisted rows into evaluator rules, resolving each
// target value against the target point's declared data type.
func buildLinkageRules(cont *ServiceContainer, records []storage.LinkageRuleRecord) []engine.LinkageRule {
	rules := make([]engine.LinkageRule, 0, len(records))
	topic := map[string]string{}
	for _, r := range records {
		dt := ""
		key := r.TargetDeviceID + "\x00" + r.TargetPoint
		if t, ok := topic[key]; ok {
			dt = t
		} else if cont.DeviceRepo != nil {
			if dev, err := cont.DeviceRepo.Get(r.TargetDeviceID); err == nil && dev != nil {
				if p := pointDef(dev.Points, r.TargetPoint); p != nil {
					dt = p.DataType
				}
			}
			topic[key] = dt
		}
		rules = append(rules, engine.LinkageRule{
			ID:             r.ID,
			Name:           r.Name,
			SourceDeviceID: r.SourceDeviceID,
			SourcePoint:    r.SourcePoint,
			ConditionOp:    r.ConditionOp,
			Threshold:      r.Threshold,
			TargetDeviceID: r.TargetDeviceID,
			TargetPoint:    r.TargetPoint,
			TargetValue:    linkageTargetValue(dt, r.TargetValue),
			Enabled:        r.Enabled,
		})
	}
	return rules
}

// applyLinkageRules reloads the persisted rules into the live evaluator. Called
// after every create / enable / disable / delete so the UI cannot look enabled
// while the runtime still holds the old row.
func applyLinkageRules(cont *ServiceContainer) error {
	if cont == nil || cont.LinkageEvaluator == nil || cont.AlarmRepo == nil {
		return nil
	}
	records, err := cont.AlarmRepo.ListDeviceLinkages()
	if err != nil {
		return err
	}
	cont.LinkageEvaluator.SetRules(buildLinkageRules(cont, records))
	return nil
}

// newLinkageRuleID builds a collision-free id. The previous "linkage_" + nanos
// in base36 collided for two rules created in the same tick on Windows clocks,
// and the UNIQUE constraint turned that into a 500.
func newLinkageRuleID() string {
	return "linkage_" + uuid.New().String()[:12]
}

// validateDeviceLinkage rejects a rule that could never actuate: unknown devices,
// points the source device does not publish, a read-only target, or an operator
// the evaluator does not implement.
func validateDeviceLinkage(cont *ServiceContainer, rule *storage.LinkageRuleRecord) error {
	rule.SourceDeviceID = strings.TrimSpace(rule.SourceDeviceID)
	rule.SourcePoint = strings.TrimSpace(rule.SourcePoint)
	rule.TargetDeviceID = strings.TrimSpace(rule.TargetDeviceID)
	rule.TargetPoint = strings.TrimSpace(rule.TargetPoint)
	if rule.SourceDeviceID == "" || rule.SourcePoint == "" || rule.TargetDeviceID == "" || rule.TargetPoint == "" {
		return fmt.Errorf("ERR_LINKAGE_DEVICES_REQUIRED")
	}
	if cont.DeviceRepo == nil {
		return fmt.Errorf("ERR_COMMON_DB_NOT_READY")
	}
	if !linkageConditionOps[rule.ConditionOp] {
		return fmt.Errorf("ERR_LINKAGE_CONDITION_UNSUPPORTED: %s", rule.ConditionOp)
	}
	source, err := cont.DeviceRepo.Get(rule.SourceDeviceID)
	if err != nil || source == nil {
		return fmt.Errorf("ERR_LINKAGE_SOURCE_DEVICE_UNKNOWN: %s", rule.SourceDeviceID)
	}
	target, err := cont.DeviceRepo.Get(rule.TargetDeviceID)
	if err != nil || target == nil {
		return fmt.Errorf("ERR_LINKAGE_TARGET_DEVICE_UNKNOWN: %s", rule.TargetDeviceID)
	}
	if len(source.Points) > 0 && pointDef(source.Points, rule.SourcePoint) == nil {
		return fmt.Errorf("ERR_LINKAGE_SOURCE_POINT_UNKNOWN: %s.%s", rule.SourceDeviceID, rule.SourcePoint)
	}
	if len(target.Points) > 0 {
		if pointDef(target.Points, rule.TargetPoint) == nil {
			return fmt.Errorf("ERR_LINKAGE_TARGET_POINT_UNKNOWN: %s.%s", rule.TargetDeviceID, rule.TargetPoint)
		}
		if readOnlyPointName(target.Points, rule.TargetPoint) {
			return fmt.Errorf("ERR_LINKAGE_TARGET_READ_ONLY: %s.%s", rule.TargetDeviceID, rule.TargetPoint)
		}
	}
	if strings.TrimSpace(rule.TargetValue) == "" {
		return fmt.Errorf("ERR_LINKAGE_TARGET_VALUE_REQUIRED")
	}
	return nil
}

// linkageErrResponse maps an ERR_* validation error onto 400 with the code kept
// as "CODE:detail" so the client can interpolate the detail.
func linkageErrResponse(c echo.Context, err error) error {
	msg := strings.TrimSpace(err.Error())
	if strings.HasPrefix(msg, "ERR_") {
		return ErrorCode(c, http.StatusBadRequest, msg, msg)
	}
	return BadRequest(c, "ERR_LINKAGE_INVALID")
}

// WireDeviceLinkage installs the write sink, loads the persisted rules and feeds
// collected samples to the evaluator. Called once from main after services start.
func WireDeviceLinkage(cont *ServiceContainer) {
	if cont == nil || cont.LinkageEvaluator == nil {
		logrus.Warn("Linkage evaluator unavailable, /linkage routes will report not running")
		return
	}
	ev := cont.LinkageEvaluator
	ev.SetWriteSink(func(ctx context.Context, deviceID, point string, value interface{}) error {
		if cont.DeviceService == nil {
			return fmt.Errorf("device service not ready")
		}
		writeCtx, cancel := context.WithTimeout(ctx, linkageWriteTimeout)
		defer cancel()
		return cont.DeviceService.WritePoint(writeCtx, deviceID, &models.WritePointRequest{Point: point, Value: value})
	})
	ev.SetTriggerRecorder(func(ruleID string, at time.Time) error {
		if cont.AlarmRepo == nil {
			return fmt.Errorf("alarm repository not ready")
		}
		return cont.AlarmRepo.RecordLinkageTrigger(ruleID, at)
	})

	if err := applyLinkageRules(cont); err != nil {
		logrus.WithError(err).Error("Device linkage rules could not be loaded, no rule will fire")
	} else {
		logrus.WithField("rules", ev.RuleCount()).Info("Device linkage rules loaded from database")
	}

	if cont.EventBus == nil {
		logrus.Warn("Event bus unavailable, device linkage rules will never be evaluated")
		return
	}
	linkageFeedCh = make(chan linkageFeedItem, linkageFeedBuffer)
	cont.EventBus.Subscribe(engine.EventTypeDataCollected, func(event engine.Event) {
		payload, ok := event.Data.(engine.DataCollectedEvent)
		if !ok {
			return
		}
		for _, p := range payload.Points {
			// A sample the collector marked bad must not actuate a device; the
			// evaluator has no quality field to judge by, so drop it here.
			if q := strings.ToLower(strings.TrimSpace(p.Quality)); q != "" && q != "good" {
				linkageFeedDropped.Add(1)
				continue
			}
			select {
			case linkageFeedCh <- linkageFeedItem{deviceID: payload.DeviceID, point: p.PointName, value: p.Value}:
			default:
				linkageFeedDropped.Add(1)
			}
		}
	})
	go func() {
		// Writes are device I/O; running them on the event-bus dispatch goroutine
		// would stall alarm and websocket delivery for every other subscriber.
		for item := range linkageFeedCh {
			ctx, cancel := context.WithTimeout(context.Background(), linkageWriteTimeout)
			ev.Evaluate(ctx, item.deviceID, item.point, item.value)
			cancel()
		}
	}()
	ev.SetStarted(true)
	logrus.Info("Device linkage evaluator started")
}

// handleGetLinkageStatus reports what the evaluator really is doing. A link the
// operator cannot observe is how a stuck rule gets mistaken for a working one.
func handleGetLinkageStatus(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.LinkageEvaluator == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_LINKAGE_EVALUATOR_UNAVAILABLE",
			"Linkage evaluator is not available in this process; rules are stored but nothing can fire them")
	}
	totals := cont.LinkageEvaluator.Totals()
	return OK(c, map[string]interface{}{
		"running":         cont.LinkageEvaluator.Started(),
		"sink_wired":      cont.LinkageEvaluator.SinkWired(),
		"rules":           totals.Rules,
		"armed":           totals.Armed,
		"total_triggers":  totals.Transfers,
		"total_errors":    totals.Errors,
		"dropped_samples": totals.SkippedSamples + linkageFeedDropped.Load(),
		"feed_configured": linkageFeedCh != nil,
		"persistence":     cont.AlarmRepo != nil,
	})
}

// handleGetLinkageRuleStats returns the live per-rule counters for one rule.
func handleGetLinkageRuleStats(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.LinkageEvaluator == nil {
		return ServiceUnavailable(c, "ERR_LINKAGE_EVALUATOR_UNAVAILABLE")
	}
	id := c.Param("id")
	stats := cont.LinkageEvaluator.RuleStats(id)
	if stats == nil {
		return NotFound(c, "ERR_LINKAGE_RULE_NOT_FOUND")
	}
	transferAt := ""
	if !stats.LastTransferAt.IsZero() {
		// A zero time.Time serialises as 0001-01-01T00:00:00Z, which the UI would
		// render as a date instead of "never".
		transferAt = stats.LastTransferAt.Format(time.RFC3339)
	}
	return OK(c, map[string]interface{}{
		"rule_id":          stats.RuleID,
		"transfers":        stats.Transfers,
		"errors":           stats.Errors,
		"skipped_samples":  stats.SkippedSamples,
		"last_error":       stats.LastError,
		"last_value":       stats.LastValue,
		"has_value":        stats.HasValue,
		"satisfied":        stats.Satisfied,
		"retry_pending":    stats.RetryPending,
		"last_transfer_at": transferAt,
	})
}

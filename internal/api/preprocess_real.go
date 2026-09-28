package api

// The preprocessing page talks to /preprocess/config and the expression page to
// /expressions/evaluate|validate|functions. Every one of those used to answer
// from a literal: GET returned an empty object, PUT echoed the body back with
// updated:true, evaluate answered result:null for any input. An operator could
// add a deadband, get a green toast, and keep collecting raw values, because the
// Preprocessor the collect path runs through was never built or fed at all.
//
// These are the real routes. What they persist is split the way the behaviour
// is: the switches (enabled, default windows) live in the preprocess section of
// the config file, the per-point filters live as rules in system_settings, and
// each write is pushed into the running engine so it takes effect on the next
// collected batch rather than on the next restart.

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// RegisterPreprocessConfigRoutes mounts the routes the preprocessing page uses.
// It replaces RegisterPreprocessExtendedRoutes, which was mounted in its place
// and wrote nothing.
func RegisterPreprocessConfigRoutes(g *echo.Group) {
	g.GET("/config", handleGetPreprocessGlobalSettings, requirePermission(security.PermPreprocessConfig))
	g.PUT("/config", handleUpdatePreprocessGlobalSettings, requirePermission(security.PermPreprocessConfig))
}

// RegisterExpressionTestRoutes mounts the expression workbench endpoints. It
// replaces RegisterExpressionExtendedRoutes, whose handlers answered result
// null for every expression and valid true for every syntax.
func RegisterExpressionTestRoutes(g *echo.Group) {
	g.POST("/evaluate", handleEvaluateExpressionReal, requirePermission(security.PermExpressionConfig))
	g.POST("/evaluate-batch", handleEvaluateBatchExpressionReal, requirePermission(security.PermExpressionConfig))
	g.POST("/validate", handleValidateExpressionReal, requirePermission(security.PermExpressionConfig))
	g.GET("/functions", handleGetExpressionFunctionsReal, requirePermission(security.PermExpressionConfig))
}

// globalScopeDeviceID is the device id a rule carries when it applies to every
// device. The page configures points and names no device, so its rows become
// rules in this bucket; rules bound to a device are a separate set and the
// page's replace-all save never touches them.
const globalScopeDeviceID = ""

// preprocessPointConfig is one row of the page's per-point table. The numeric
// fields are pointers so a negative deadband can be rejected instead of being
// read back as "not set".
type preprocessPointConfig struct {
	Deadband           *float64 `json:"deadband,omitempty"`
	DeadbandPercent    *float64 `json:"deadband_percent,omitempty"`
	Filter             string   `json:"filter,omitempty"`
	FilterWindow       *int     `json:"filter_window,omitempty"`
	EmaAlpha           *float64 `json:"ema_alpha,omitempty"`
	KalmanProcessNoise *float64 `json:"kalman_process_noise,omitempty"`
	KalmanMeasNoise    *float64 `json:"kalman_measurement_noise,omitempty"`
	Aggregate          string   `json:"aggregate,omitempty"`
	AggregateWindowSec *float64 `json:"aggregate_window_sec,omitempty"`
}

// rules expands a table row into the operations the engine runs. One row can
// mean up to three rules because a rule carries a single Operation; they are
// emitted filter, aggregate, deadband, so the change detection compares the
// cleaned-up value instead of the raw one.
func (p *preprocessPointConfig) rules(pointKey string, defaults config.PreprocessGlobalConfig) ([]*models.PreprocessRule, error) {
	var out []*models.PreprocessRule

	if p.Filter != "" {
		window, ok := preprocessFilterWindow(p.Filter)
		if !ok {
			return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q asks for filter %q, which this build cannot apply (median_<N>, moving_avg, ema, kalman)", pointKey, p.Filter)
		}
		if p.FilterWindow != nil && (*p.FilterWindow < 1 || *p.FilterWindow > 99) {
			return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has filter_window %d, which must be between 1 and 99", pointKey, *p.FilterWindow)
		}
		if window <= 0 {
			switch {
			case p.FilterWindow != nil:
				window = *p.FilterWindow
			default:
				window = defaults.DefaultFilterWindow
			}
		}
		if window <= 0 {
			window = 3
		}
		params := map[string]interface{}{
			"filter":        p.Filter,
			"filter_window": float64(window),
		}
		// ema and kalman ignore a sample window, so storing one would show the
		// operator a number that filters nothing.
		if p.Filter == "ema" || p.Filter == "kalman" {
			delete(params, "filter_window")
		}
		if p.EmaAlpha != nil {
			if p.Filter != "ema" {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q sets ema_alpha but asks for filter %q; alpha only applies to ema", pointKey, p.Filter)
			}
			if *p.EmaAlpha <= 0 || *p.EmaAlpha > 1 {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has ema_alpha %v, which must be greater than 0 and at most 1", pointKey, *p.EmaAlpha)
			}
			params["ema_alpha"] = *p.EmaAlpha
		}
		if p.KalmanProcessNoise != nil || p.KalmanMeasNoise != nil {
			if p.Filter != "kalman" {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q sets kalman noise but asks for filter %q; the noise settings only apply to kalman", pointKey, p.Filter)
			}
			if p.KalmanProcessNoise != nil {
				if *p.KalmanProcessNoise <= 0 {
					return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has kalman_process_noise %v, which must be greater than 0", pointKey, *p.KalmanProcessNoise)
				}
				params["kalman_process_noise"] = *p.KalmanProcessNoise
			}
			if p.KalmanMeasNoise != nil {
				if *p.KalmanMeasNoise <= 0 {
					return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has kalman_measurement_noise %v, which must be greater than 0", pointKey, *p.KalmanMeasNoise)
				}
				params["kalman_measurement_noise"] = *p.KalmanMeasNoise
			}
		}
		out = append(out, &models.PreprocessRule{
			ID:        globalRuleID(pointKey, "filter"),
			DeviceID:  globalScopeDeviceID,
			PointName: pointKey,
			Operation: "filter",
			Params:    params,
			Enabled:   true,
		})
	}

	if p.Aggregate != "" {
		if !supportedAggregate(p.Aggregate) {
			return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q asks for aggregate %q, which this build cannot apply (avg, max, min, sum, last)", pointKey, p.Aggregate)
		}
		window := 0.0
		if p.AggregateWindowSec != nil {
			if *p.AggregateWindowSec < 0 {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has aggregate_window_sec %v, which must be 0 or more", pointKey, *p.AggregateWindowSec)
			}
			window = *p.AggregateWindowSec
		}
		if window <= 0 {
			window = float64(defaults.DefaultAggregateWindowSec)
		}
		if window <= 0 {
			// The engine passes a point straight through when the window is 0, so
			// storing this row would show an aggregation in the table that filters
			// nothing. Say so instead.
			return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q aggregates with %q but has no window; set aggregate_window_sec or the global default_aggregate_window_sec", pointKey, p.Aggregate)
		}
		out = append(out, &models.PreprocessRule{
			ID:        globalRuleID(pointKey, "aggregate"),
			DeviceID:  globalScopeDeviceID,
			PointName: pointKey,
			Operation: "aggregate",
			Params: map[string]interface{}{
				"aggregate":            p.Aggregate,
				"aggregate_window_sec": window,
			},
			Enabled: true,
		})
	}

	if p.Deadband != nil || p.DeadbandPercent != nil {
		deadband, pct := 0.0, 0.0
		if p.Deadband != nil {
			if *p.Deadband < 0 {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has deadband %v, which must be 0 or more", pointKey, *p.Deadband)
			}
			deadband = *p.Deadband
		}
		if p.DeadbandPercent != nil {
			if *p.DeadbandPercent < 0 || *p.DeadbandPercent > 100 {
				return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q has deadband_percent %v, which must be between 0 and 100", pointKey, *p.DeadbandPercent)
			}
			pct = *p.DeadbandPercent
		}
		if deadband <= 0 && pct <= 0 {
			return nil, fmt.Errorf("ERR_COMMON_VALIDATION: point %q sets a deadband of 0, which filters nothing; remove the row to use the global default", pointKey)
		}
		out = append(out, &models.PreprocessRule{
			ID:        globalRuleID(pointKey, "deadband"),
			DeviceID:  globalScopeDeviceID,
			PointName: pointKey,
			Operation: "deadband",
			Params: map[string]interface{}{
				"deadband":         deadband,
				"deadband_percent": pct,
			},
			Enabled: true,
		})
	}

	return out, nil
}

// pointConfigFromRules folds the rules of one point back into a table row.
func pointConfigFromRules(rules []*models.PreprocessRule) preprocessPointConfig {
	cfg := preprocessPointConfig{}
	for _, rule := range rules {
		switch rule.Operation {
		case "filter":
			if f, ok := rule.Params["filter"].(string); ok && f != "" {
				cfg.Filter = f
			}
			if w, ok := rule.Params["filter_window"].(float64); ok {
				window := int(w)
				cfg.FilterWindow = &window
			}
			if a, ok := rule.Params["ema_alpha"].(float64); ok && a > 0 {
				alpha := a
				cfg.EmaAlpha = &alpha
			}
			if q, ok := rule.Params["kalman_process_noise"].(float64); ok && q > 0 {
				noise := q
				cfg.KalmanProcessNoise = &noise
			}
			if r, ok := rule.Params["kalman_measurement_noise"].(float64); ok && r > 0 {
				noise := r
				cfg.KalmanMeasNoise = &noise
			}
		case "aggregate":
			if a, ok := rule.Params["aggregate"].(string); ok && a != "" {
				cfg.Aggregate = a
			}
			if w, ok := rule.Params["aggregate_window_sec"].(float64); ok {
				window := w
				cfg.AggregateWindowSec = &window
			}
		case "deadband":
			if d, ok := rule.Params["deadband"].(float64); ok && d > 0 {
				deadband := d
				cfg.Deadband = &deadband
			}
			if d, ok := rule.Params["deadband_percent"].(float64); ok && d > 0 {
				pct := d
				cfg.DeadbandPercent = &pct
			}
		}
	}
	return cfg
}

func supportedAggregate(agg string) bool {
	for _, prefix := range []string{"avg", "max", "min", "sum", "last"} {
		if strings.HasPrefix(agg, prefix) {
			return true
		}
	}
	return false
}

// preprocessFilterWindow reports whether this build can apply the filter type,
// and the sample count it runs at. A median carries its own width in its name
// (median_7), which the engine honours, so the returned window overrides any
// number sent alongside it; the other types report 0 and use the configured
// window.
func preprocessFilterWindow(filterType string) (int, bool) {
	switch {
	case filterType == "kalman", filterType == "ema", filterType == "moving_avg":
		return 0, true
	case strings.HasPrefix(filterType, "median_"):
		n, err := strconv.Atoi(strings.TrimPrefix(filterType, "median_"))
		if err != nil || n < 1 || n > 99 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// globalRuleID keeps the id stable for a point and operation, so saving the same
// row twice updates one rule instead of stacking the same filter twice.
func globalRuleID(pointKey, operation string) string {
	return fmt.Sprintf("pp-g%08x-%s", crc32.ChecksumIEEE([]byte(pointKey)), operation)
}

// preprocessGlobalFields are the switches the top card of the page edits.
type preprocessGlobalFields struct {
	Enabled                   *bool    `json:"enabled"`
	DefaultDeadband           *float64 `json:"default_deadband"`
	DefaultFilterWindow       *int     `json:"default_filter_window"`
	DefaultAggregateWindowSec *int     `json:"default_aggregate_window_sec"`
}

// jsonSubObject reads a nested object out of a bound request body, so the
// presence of the section can be told apart from an empty one.
func jsonSubObject(raw map[string]interface{}, key string) (map[string]interface{}, bool) {
	value, ok := raw[key]
	if !ok || value == nil {
		return nil, false
	}
	object, ok := value.(map[string]interface{})
	return object, ok
}

func decodeInto(object map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return errors.New("ERR_COMMON_VALIDATION: " + err.Error())
	}
	return nil
}

// collectPreprocessConfig reads the switches from the live config and the rows
// from the rule store, so the page shows what the running gateway applies.
func collectPreprocessConfig(cont *ServiceContainer) (map[string]interface{}, error) {
	cfg := config.GetConfig()
	view := map[string]interface{}{
		"enabled":                      cfg.Preprocess.Enabled,
		"default_deadband":             cfg.Preprocess.DefaultDeadband,
		"default_filter_window":        cfg.Preprocess.DefaultFilterWindow,
		"default_aggregate_window_sec": cfg.Preprocess.DefaultAggregateWindowSec,
	}

	all, err := cont.PreprocessRules.List()
	if err != nil {
		return nil, err
	}

	byPoint := make(map[string][]*models.PreprocessRule)
	deviceRules := []models.PreprocessRule{}
	for i := range all {
		rule := &all[i]
		if rule.DeviceID == globalScopeDeviceID {
			byPoint[rule.PointName] = append(byPoint[rule.PointName], rule)
			continue
		}
		deviceRules = append(deviceRules, *rule)
	}

	pointConfigs := make(map[string]preprocessPointConfig, len(byPoint))
	for point, rules := range byPoint {
		pointConfigs[point] = pointConfigFromRules(rules)
	}
	view["point_configs"] = pointConfigs
	view["device_rules"] = deviceRules
	return view, nil
}

func handleGetPreprocessGlobalSettings(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}
	view, err := collectPreprocessConfig(cont)
	if err != nil {
		logrus.WithError(err).Error("Failed to read preprocessing rules")
		return InternalError(c, "ERR_COMMON_READ_FAILED: preprocessing rules could not be read")
	}
	return OK(c, view)
}

func handleUpdatePreprocessGlobalSettings(c echo.Context) error {
	var raw map[string]interface{}
	if err := c.Bind(&raw); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont == nil || cont.PreprocessRules == nil {
		return ErrorCode(c, http.StatusServiceUnavailable, "ERR_COMMON_UNAVAILABLE",
			"ERR_COMMON_UNAVAILABLE: the preprocessing rule store is not ready")
	}

	// The page nests the switches under global and the rows under points; the
	// flat spelling GET answers with is accepted too.
	globalObject, hasGlobal := jsonSubObject(raw, "global")
	if !hasGlobal {
		globalObject, hasGlobal = raw, true
	}
	var fields preprocessGlobalFields
	if err := decodeInto(globalObject, &fields); err != nil {
		return BadRequest(c, err.Error())
	}

	rowsObject, hasRows := jsonSubObject(raw, "points")
	if !hasRows {
		rowsObject, hasRows = jsonSubObject(raw, "point_configs")
	}
	var rows map[string]preprocessPointConfig
	if hasRows {
		if err := decodeInto(rowsObject, &rows); err != nil {
			return BadRequest(c, err.Error())
		}
	}

	cfg := config.GetConfig()
	defaults, err := mergedPreprocessGlobals(cfg.Preprocess, &fields)
	if err != nil {
		return BadRequest(c, err.Error())
	}

	// Build the whole desired rule set before anything is written, so a rejected
	// row cannot leave the stored set half applied.
	var desired []*models.PreprocessRule
	for pointKey, row := range rows {
		key := strings.TrimSpace(pointKey)
		if key == "" {
			return BadRequest(c, "ERR_COMMON_VALIDATION: a point name cannot be empty")
		}
		if len(key) > 200 {
			return BadRequest(c, fmt.Sprintf("ERR_COMMON_VALIDATION: point name %q is longer than 200 characters", key))
		}
		rules, err := row.rules(key, defaults)
		if err != nil {
			return BadRequest(c, err.Error())
		}
		if len(rules) == 0 {
			// A row with no deadband, filter or aggregation stores nothing, so the
			// page would report success and the row would be gone on the next
			// refresh.
			return BadRequest(c, fmt.Sprintf("ERR_COMMON_VALIDATION: point %q sets no deadband, filter or aggregation, so there is nothing to store", key))
		}
		desired = append(desired, rules...)
	}

	previousGlobal := cfg.Preprocess
	cfg.Preprocess = defaults
	if err := config.SaveConfig(cfg, ""); err != nil {
		cfg.Preprocess = previousGlobal
		return InternalError(c, "ERR_SYSTEM_CONFIG_SAVE_FAILED")
	}

	// An absent points section means "leave the rows alone"; a present one is the
	// complete set, because a row the operator deleted is simply not in it.
	if hasRows {
		if err := replaceGlobalPreprocessRules(cont.PreprocessRules, desired); err != nil {
			cfg.Preprocess = previousGlobal
			if rbErr := config.SaveConfig(cfg, ""); rbErr != nil {
				logrus.WithError(rbErr).Error("Failed to roll back the preprocess section after the rules were rejected")
			}
			logrus.WithError(err).Error("Failed to store preprocessing rules")
			return InternalError(c, "ERR_COMMON_WRITE_FAILED: preprocessing rules could not be stored")
		}
	}

	if err := applyPreprocessorFromStore(cont); err != nil {
		logrus.WithError(err).Error("Preprocessing rules were stored but could not be applied")
		return InternalError(c, "ERR_COMMON_WRITE_FAILED: preprocessing rules could not be applied to the running engine")
	}

	view, err := collectPreprocessConfig(cont)
	if err != nil {
		return InternalError(c, "ERR_COMMON_READ_FAILED: preprocessing rules could not be read back")
	}
	return OK(c, view)
}

// mergedPreprocessGlobals applies only the switches the body carries, so a page
// that posts its point table cannot switch preprocessing off as a side effect.
func mergedPreprocessGlobals(current config.PreprocessGlobalConfig, fields *preprocessGlobalFields) (config.PreprocessGlobalConfig, error) {
	next := current
	if fields == nil {
		return next, nil
	}
	if fields.Enabled != nil {
		next.Enabled = *fields.Enabled
	}
	if fields.DefaultDeadband != nil {
		if *fields.DefaultDeadband < 0 {
			return next, errors.New("ERR_COMMON_VALIDATION: default_deadband must be 0 or more")
		}
		next.DefaultDeadband = *fields.DefaultDeadband
	}
	if fields.DefaultFilterWindow != nil {
		if *fields.DefaultFilterWindow < 1 || *fields.DefaultFilterWindow > 99 {
			return next, errors.New("ERR_COMMON_VALIDATION: default_filter_window must be between 1 and 99")
		}
		next.DefaultFilterWindow = *fields.DefaultFilterWindow
	}
	if fields.DefaultAggregateWindowSec != nil {
		if *fields.DefaultAggregateWindowSec < 0 {
			return next, errors.New("ERR_COMMON_VALIDATION: default_aggregate_window_sec must be 0 or more")
		}
		next.DefaultAggregateWindowSec = *fields.DefaultAggregateWindowSec
	}
	return next, nil
}

// replaceGlobalPreprocessRules makes the device-independent rules equal the
// desired set, leaving rules bound to a device alone.
func replaceGlobalPreprocessRules(store *storage.PreprocessRuleStore, desired []*models.PreprocessRule) error {
	stored, err := store.List()
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(desired))
	for _, rule := range desired {
		wanted[rule.ID] = true
	}
	for i := range stored {
		rule := stored[i]
		if rule.DeviceID != globalScopeDeviceID || wanted[rule.ID] {
			continue
		}
		if _, err := store.Delete(rule.ID); err != nil {
			return err
		}
	}
	for _, rule := range desired {
		if err := store.Save(*rule); err != nil {
			return err
		}
	}
	return nil
}

// applyPreprocessorFromStore pushes what the store holds into the engine the
// collect path runs through. Without it a save changed only what the next GET
// listed, which is how this page stayed green while filtering nothing.
func applyPreprocessorFromStore(cont *ServiceContainer) error {
	if cont == nil || cont.Preprocessor == nil || cont.PreprocessRules == nil {
		return nil
	}
	rules, err := cont.PreprocessRules.List()
	if err != nil {
		return err
	}
	ptrs := make([]*models.PreprocessRule, 0, len(rules))
	for i := range rules {
		ptrs = append(ptrs, &rules[i])
	}
	cont.Preprocessor.ReplaceRules(ptrs)
	cont.Preprocessor.ApplyGlobalConfig(&config.GetConfig().Preprocess)
	return nil
}

// applyExpressionsFromStore pushes the stored derived-point expressions into the
// engine that appends them to each collected batch.
func applyExpressionsFromStore(cont *ServiceContainer) error {
	if cont == nil || cont.ExpressionEngine == nil || cont.ExpressionConfigs == nil {
		return nil
	}
	stored, err := cont.ExpressionConfigs.List()
	if err != nil {
		return err
	}
	ptrs := make([]*models.ExpressionConfig, 0, len(stored))
	for i := range stored {
		ptrs = append(ptrs, &stored[i])
	}
	cont.ExpressionEngine.ReplaceExpressions(ptrs)
	return nil
}

// WirePreprocessing loads what the operator saved into the engines the collect
// path runs through. cmd/edgelite calls it during bootstrap, before the
// scheduler starts, and a failure here is reported rather than left as a quiet
// empty rule set: the gateway then collects raw values and says so in the log.
func WirePreprocessing(cont *ServiceContainer) {
	if cont == nil {
		return
	}
	if err := applyPreprocessorFromStore(cont); err != nil {
		logrus.WithError(err).Error("Preprocessing rules could not be loaded; collection will run without them")
		return
	}
	if cont.Preprocessor != nil {
		logrus.WithFields(logrus.Fields{
			"rules":   len(cont.Preprocessor.AllRules()),
			"enabled": cont.Preprocessor.Enabled(),
		}).Info("Edge preprocessing loaded")
	}
	if err := applyExpressionsFromStore(cont); err != nil {
		logrus.WithError(err).Error("Derived point expressions could not be loaded; collection will run without them")
		return
	}
	if cont.ExpressionEngine != nil {
		logrus.WithField("expressions", len(cont.ExpressionEngine.GetExpressions())).
			Info("Derived point expressions loaded")
	}
}

// expressionEvaluator is the same safe parser the collect path uses, so what the
// workbench computes is what a stored derived point computes later.
var expressionEvaluator = engine.NewExpressionEngine()

// expressionRequest is the page's body. It posts the variables under "variables";
// the replaced handler bound "context", so even a real implementation would have
// evaluated every ${...} against an empty map.
type expressionRequest struct {
	Expression string                 `json:"expression"`
	Variables  map[string]interface{} `json:"variables"`
	Context    map[string]interface{} `json:"context"`
}

func (r *expressionRequest) vars() map[string]interface{} {
	if len(r.Variables) > 0 {
		return r.Variables
	}
	return r.Context
}

// respondExpressionError answers a failed evaluation with the parser's own
// reason, which is the difference between a workbench that teaches the operator
// what is wrong and one that says "null".
func respondExpressionError(c echo.Context, reason string) error {
	message := "ERR_EXPRESSION_INVALID: " + reason
	return ErrorCode(c, http.StatusBadRequest, message, message)
}

func handleEvaluateExpressionReal(c echo.Context) error {
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
		return respondExpressionError(c, err.Error())
	}
	return OK(c, map[string]interface{}{
		"expression": req.Expression,
		"result":     result,
	})
}

func handleEvaluateBatchExpressionReal(c echo.Context) error {
	var req struct {
		Expressions map[string]string      `json:"expressions"`
		Variables   map[string]interface{} `json:"variables"`
		Context     map[string]interface{} `json:"context"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if len(req.Expressions) == 0 {
		return BadRequest(c, "ERR_COMMON_VALIDATION: expressions must be a non-empty object of name to expression")
	}
	vars := req.Variables
	if len(vars) == 0 {
		vars = req.Context
	}
	results := make(map[string]interface{}, len(req.Expressions))
	failures := make(map[string]string, len(req.Expressions))
	for name, expr := range req.Expressions {
		value, err := expressionEvaluator.EvaluateDetailed(expr, vars)
		if err != nil {
			// A failed entry is reported next to the ones that worked; answering
			// null for it alone is how a typo hides in a batch.
			results[name] = nil
			failures[name] = err.Error()
			continue
		}
		results[name] = value
	}
	out := map[string]interface{}{"results": results}
	if len(failures) > 0 {
		out["errors"] = failures
	}
	return OK(c, out)
}

func handleValidateExpressionReal(c echo.Context) error {
	var req expressionRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	expression := strings.TrimSpace(req.Expression)
	if expression == "" {
		return OK(c, map[string]interface{}{"valid": false, "expression": expression, "error": "expression is empty"})
	}
	// Validate runs the same resolve-then-parse path evaluate does, so "valid"
	// means this build can compute it with the variables the caller named.
	_, err := expressionEvaluator.EvaluateDetailed(expression, req.vars())
	response := map[string]interface{}{"valid": err == nil, "expression": expression}
	if err != nil {
		response["error"] = err.Error()
	} else {
		response["error"] = ""
	}
	return OK(c, response)
}

// expressionFunctionDoc describes one parser function or operator for the
// workbench's reference tables.
type expressionFunctionDoc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

type expressionOperatorDoc struct {
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
}

// expressionCatalog returns the functions and operators this build's parser
// accepts. The handler and the test share it, so the reference table the page
// shows cannot drift away from what the engine can compute.
func expressionCatalog() ([]expressionFunctionDoc, []expressionOperatorDoc) {
	functions := []expressionFunctionDoc{
		{"abs", "Absolute value", "abs(${demo.value})"},
		{"round", "Round to N decimal places", "round(${demo.value}, 2)"},
		{"min", "Minimum value", "min(${demo.value}, 2)"},
		{"max", "Maximum value", "max(${demo.value}, 2)"},
		{"pow", "Power operation", "pow(${demo.value}, 2)"},
		{"sqrt", "Square root", "sqrt(${demo.value})"},
		{"ceil", "Ceiling (round up)", "ceil(${demo.value})"},
		{"floor", "Floor (round down)", "floor(${demo.value})"},
		{"log", "Natural logarithm", "log(${demo.value})"},
		{"log10", "Base-10 logarithm", "log10(${demo.value})"},
		{"int", "Convert to integer", "int(${demo.value})"},
		{"float", "Convert to float", "float(${demo.value})"},
	}
	operators := []expressionOperatorDoc{
		{"+", "Addition"},
		{"-", "Subtraction"},
		{"*", "Multiplication"},
		{"/", "Division"},
		{"%", "Modulo"},
		{"**", "Exponentiation"},
		{"==", "Equal"},
		{"!=", "Not equal"},
		{"<", "Less than"},
		{"<=", "Less or equal"},
		{">", "Greater than"},
		{">=", "Greater or equal"},
		{"and", "Logical AND"},
		{"or", "Logical OR"},
		{"not", "Logical NOT"},
	}
	return functions, operators
}

// handleGetExpressionFunctionsReal lists what the parser in this build accepts.
// It is not a copy of the Python engine's catalogue: an entry here is a promise
// the workbench can compute that example, and the test evaluates each of them to
// keep the promise checked.
func handleGetExpressionFunctionsReal(c echo.Context) error {
	functions, operators := expressionCatalog()
	return OK(c, map[string]interface{}{"functions": functions, "operators": operators})
}

// preprocessOperations are the rule kinds applyOperation understands. A rule with
// any other value is stored, listed in the UI and changes nothing, which is the
// exact failure this round is about, so the API refuses it at the door. The
// Preprocessor only ever receives rules through these routes - the config file
// carries switches, not rules - so validating here covers every way in.
var preprocessOperations = map[string]func(params map[string]interface{}) error{
	"scale":     numericParams("scale"),
	"offset":    numericParams("offset"),
	"transform": numericParams("scale", "offset", "round"),
	"deadband":  deadbandParams,
	"clamp":     clampParams,
	"filter":    filterParams,
	"aggregate": aggregateParams,
	"sqrt":      noParamsRequired,
	"abs":       noParamsRequired,
	"negate":    noParamsRequired,
	"percent":   percentParams,
}

func numericParams(names ...string) func(map[string]interface{}) error {
	return func(params map[string]interface{}) error {
		for _, name := range names {
			value, ok := params[name]
			if !ok || value == nil {
				continue
			}
			if _, isNumber := value.(float64); !isNumber {
				return fmt.Errorf("ERR_COMMON_VALIDATION: %s must be a number, got %T", name, value)
			}
		}
		return nil
	}
}

func noParamsRequired(map[string]interface{}) error { return nil }

func deadbandParams(params map[string]interface{}) error {
	if err := numericParams("deadband", "deadband_percent")(params); err != nil {
		return err
	}
	if d, ok := params["deadband"].(float64); ok && d < 0 {
		return errors.New("ERR_COMMON_VALIDATION: deadband must be 0 or more")
	}
	if d, ok := params["deadband_percent"].(float64); ok && (d < 0 || d > 100) {
		return errors.New("ERR_COMMON_VALIDATION: deadband_percent must be between 0 and 100")
	}
	if d, _ := params["deadband"].(float64); d <= 0 {
		if p, _ := params["deadband_percent"].(float64); p <= 0 {
			return errors.New("ERR_COMMON_VALIDATION: a deadband rule needs deadband or deadband_percent above 0, or it filters nothing")
		}
	}
	return nil
}

func clampParams(params map[string]interface{}) error {
	if err := numericParams("min", "max")(params); err != nil {
		return err
	}
	minValue, hasMin := params["min"].(float64)
	maxValue, hasMax := params["max"].(float64)
	if hasMin && hasMax && minValue > maxValue {
		return fmt.Errorf("ERR_COMMON_VALIDATION: clamp min %v is above max %v, so every value would be replaced", minValue, maxValue)
	}
	if !hasMin && !hasMax {
		return errors.New("ERR_COMMON_VALIDATION: a clamp rule needs min, max or both, or it changes nothing")
	}
	return nil
}

func filterParams(params map[string]interface{}) error {
	filterType, _ := params["filter"].(string)
	if filterType == "" {
		return errors.New(`ERR_COMMON_VALIDATION: a filter rule needs params.filter (median_<N>, moving_avg, ema, kalman)`)
	}
	if _, ok := preprocessFilterWindow(filterType); !ok {
		return fmt.Errorf("ERR_COMMON_VALIDATION: filter %q is not a filter this build can apply", filterType)
	}
	if err := numericParams("filter_window", "ema_alpha", "kalman_process_noise", "kalman_measurement_noise")(params); err != nil {
		return err
	}
	if w, ok := params["filter_window"].(float64); ok && (w < 1 || w > 99) {
		return fmt.Errorf("ERR_COMMON_VALIDATION: filter_window must be between 1 and 99, got %v", w)
	}
	if a, ok := params["ema_alpha"].(float64); ok {
		if filterType != "ema" {
			return fmt.Errorf("ERR_COMMON_VALIDATION: ema_alpha only applies to filter ema, got filter %q", filterType)
		}
		if a <= 0 || a > 1 {
			return fmt.Errorf("ERR_COMMON_VALIDATION: ema_alpha must be greater than 0 and at most 1, got %v", a)
		}
	}
	for _, name := range []string{"kalman_process_noise", "kalman_measurement_noise"} {
		value, ok := params[name].(float64)
		if !ok {
			continue
		}
		if filterType != "kalman" {
			return fmt.Errorf("ERR_COMMON_VALIDATION: %s only applies to filter kalman, got filter %q", name, filterType)
		}
		if value <= 0 {
			return fmt.Errorf("ERR_COMMON_VALIDATION: %s must be greater than 0, got %v", name, value)
		}
	}
	return nil
}

func aggregateParams(params map[string]interface{}) error {
	aggType, _ := params["aggregate"].(string)
	if aggType == "" {
		return errors.New("ERR_COMMON_VALIDATION: an aggregate rule needs params.aggregate (avg, max, min, sum, last)")
	}
	if !supportedAggregate(aggType) {
		return fmt.Errorf("ERR_COMMON_VALIDATION: aggregate %q is not one this build can apply", aggType)
	}
	if err := numericParams("aggregate_window_sec")(params); err != nil {
		return err
	}
	window, _ := params["aggregate_window_sec"].(float64)
	if window <= 0 {
		window = float64(config.GetConfig().Preprocess.DefaultAggregateWindowSec)
	}
	if window <= 0 {
		return fmt.Errorf("ERR_COMMON_VALIDATION: aggregate %q needs aggregate_window_sec above 0; with no window the engine passes the point through unaggregated", aggType)
	}
	return nil
}

func percentParams(params map[string]interface{}) error {
	if err := numericParams("base")(params); err != nil {
		return err
	}
	base, ok := params["base"].(float64)
	if !ok {
		return errors.New("ERR_COMMON_VALIDATION: a percent rule needs params.base, the value 100% is taken against")
	}
	if base == 0 {
		return errors.New("ERR_COMMON_VALIDATION: percent base cannot be 0")
	}
	return nil
}

// validatePreprocessRule checks the rule is one the engine can act on, and says
// which point it is about so a batch edit names the offender.
func validatePreprocessRule(rule *models.PreprocessRule) error {
	rule.PointName = strings.TrimSpace(rule.PointName)
	rule.Operation = strings.TrimSpace(rule.Operation)
	if rule.PointName == "" {
		return errors.New("ERR_COMMON_VALIDATION: a preprocessing rule needs a point_name")
	}
	if len(rule.PointName) > 200 {
		return fmt.Errorf("ERR_COMMON_VALIDATION: point_name %q is longer than 200 characters", rule.PointName)
	}
	check, ok := preprocessOperations[rule.Operation]
	if !ok {
		supported := make([]string, 0, len(preprocessOperations))
		for name := range preprocessOperations {
			supported = append(supported, name)
		}
		sort.Strings(supported)
		return fmt.Errorf("ERR_COMMON_VALIDATION: operation %q is not one this build applies (supported: %s)", rule.Operation, strings.Join(supported, ", "))
	}
	if rule.Params == nil {
		rule.Params = map[string]interface{}{}
	}
	if err := check(rule.Params); err != nil {
		return fmt.Errorf("ERR_COMMON_VALIDATION: point %q: %s", rule.PointName, strings.TrimPrefix(err.Error(), "ERR_COMMON_VALIDATION: "))
	}
	return nil
}

// newPreprocessRuleID and newExpressionConfigID give generated records the same
// id shape the rest of this gateway uses for them.
func newPreprocessRuleID() string {
	return "pp-" + uuid.New().String()[:12]
}

func newExpressionConfigID() string {
	return "expr-" + uuid.New().String()[:12]
}

// expressionVariableReference matches the ${device.point} syntax the parser
// resolves at evaluation time.
var expressionVariableReference = regexp.MustCompile(`\$\{[^{}]*\}`)

// validateDerivedExpression checks a stored derived-point expression the way the
// collect path will use it: every ${...} is replaced with a number before the
// parser runs, because a stored expression names live points that no caller has
// supplied here. Degenerate arithmetic on those stand-ins is not a reason to
// refuse the expression; an unknown function or a broken parse is.
func validateDerivedExpression(expression string) error {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return errors.New("ERR_COMMON_VALIDATION: expression is required")
	}
	counter := 0
	standIns := map[string]string{}
	resolved := expressionVariableReference.ReplaceAllStringFunc(trimmed, func(match string) string {
		name := match[2 : len(match)-2]
		if value, ok := standIns[name]; ok {
			return value
		}
		counter++
		value := strconv.Itoa(counter + 1)
		standIns[name] = value
		return value
	})
	if err := expressionEvaluator.ValidateExpression(resolved); err != nil {
		message := err.Error()
		if strings.Contains(message, "by zero") {
			return nil
		}
		return errors.New("ERR_COMMON_VALIDATION: " + message)
	}
	return nil
}

package engine

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// Preprocessor applies edge data preprocessing rules to collected data points.
// Supported operations:
//   - transform: scale + offset + round
//   - filter: median_N, moving_avg, ema, kalman
//   - deadband: absolute + percent
//   - aggregate: avg, max, min, sum, last (time-windowed)
//   - clamp: min/max limiting
//   - sqrt, abs, negate, percent
type Preprocessor struct {
	mu                       sync.RWMutex
	rules                    map[string][]*models.PreprocessRule // key: deviceID
	enabled                  bool
	defaultDeadband          float64
	defaultFilterWindow      int
	defaultAggregateWindowSec int

	// State windows for filtering and aggregation (key: deviceID:pointName:ruleID)
	filterWindows    map[string][]float64 // ring buffer for median/moving_avg
	emaState         map[string]float64   // EMA last value
	kalmanState      map[string][2]float64 // [estimate, variance]
	lastValues       map[string]float64   // deadband last values
	aggregateWindows map[string][]aggEntry // time-windowed aggregation
}

type aggEntry struct {
	ts    float64
	value float64
}

// NewPreprocessor creates a new Preprocessor from config.
func NewPreprocessor(cfg *config.PreprocessGlobalConfig) *Preprocessor {
	return &Preprocessor{
		rules:                    make(map[string][]*models.PreprocessRule),
		enabled:                  cfg.Enabled,
		defaultDeadband:          cfg.DefaultDeadband,
		defaultFilterWindow:      cfg.DefaultFilterWindow,
		defaultAggregateWindowSec: cfg.DefaultAggregateWindowSec,
		filterWindows:            make(map[string][]float64),
		emaState:                 make(map[string]float64),
		kalmanState:              make(map[string][2]float64),
		lastValues:               make(map[string]float64),
		aggregateWindows:         make(map[string][]aggEntry),
	}
}

// SetEnabled enables or disables the preprocessor.
func (p *Preprocessor) SetEnabled(enabled bool) {
	p.mu.Lock()
	p.enabled = enabled
	p.mu.Unlock()
}

// AddRule adds a preprocessing rule.
func (p *Preprocessor) AddRule(rule *models.PreprocessRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rules := p.rules[rule.DeviceID]
	rules = append(rules, rule)
	p.rules[rule.DeviceID] = rules
}

// RemoveRule removes a preprocessing rule by ID.
func (p *Preprocessor) RemoveRule(deviceID, ruleID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rules := p.rules[deviceID]
	for i, r := range rules {
		if r.ID == ruleID {
			p.rules[deviceID] = append(rules[:i], rules[i+1:]...)
			break
		}
	}
}

// ClearRules removes all preprocessing rules for a device.
func (p *Preprocessor) ClearRules(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.rules, deviceID)
}

// GetRules returns all preprocessing rules for a device.
func (p *Preprocessor) GetRules(deviceID string) []*models.PreprocessRule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rules := p.rules[deviceID]
	result := make([]*models.PreprocessRule, len(rules))
	copy(result, rules)
	return result
}

// AllRules returns every loaded rule, keyed by nothing, for the caller that has
// to show or replace the whole set.
func (p *Preprocessor) AllRules() []*models.PreprocessRule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var all []*models.PreprocessRule
	for _, rules := range p.rules {
		all = append(all, rules...)
	}
	return all
}

// ReplaceRules swaps the live rule set for the given one. Editing a rule through
// the UI used to change only what the next GET listed, because nothing ever
// pushed the stored rules into the object that actually filters readings.
func (p *Preprocessor) ReplaceRules(rules []*models.PreprocessRule) {
	byDevice := make(map[string][]*models.PreprocessRule)
	for _, rule := range rules {
		byDevice[rule.DeviceID] = append(byDevice[rule.DeviceID], rule)
	}
	p.mu.Lock()
	p.rules = byDevice
	p.mu.Unlock()
	// A deadband or filter keeps the last value per rule; once the rule is gone
	// or edited that history describes a different series, so carrying it over
	// would make the first reading after the edit compare against a stale one.
	p.ResetState()
}

// ResetState clears the filter and aggregation windows.
func (p *Preprocessor) ResetState() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filterWindows = make(map[string][]float64)
	p.emaState = make(map[string]float64)
	p.kalmanState = make(map[string][2]float64)
	p.lastValues = make(map[string]float64)
	p.aggregateWindows = make(map[string][]aggEntry)
}

// Enabled reports whether preprocessing is applied to collected data at all.
func (p *Preprocessor) Enabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

// ApplyGlobalConfig updates the switches the preprocess section of the config
// file owns, so saving that section reaches the running preprocessor instead of
// waiting for a restart.
func (p *Preprocessor) ApplyGlobalConfig(cfg *config.PreprocessGlobalConfig) {
	p.mu.Lock()
	p.enabled = cfg.Enabled
	p.defaultDeadband = cfg.DefaultDeadband
	p.defaultFilterWindow = cfg.DefaultFilterWindow
	p.defaultAggregateWindowSec = cfg.DefaultAggregateWindowSec
	p.mu.Unlock()
}

// Process applies preprocessing rules to a set of data points.
// Returns the processed points. Points filtered by deadband are excluded.
func (p *Preprocessor) Process(deviceID string, points []storage.PointData) []storage.PointData {
	p.mu.RLock()
	if !p.enabled {
		p.mu.RUnlock()
		return points
	}
	// The rules stored under the empty device id apply to every device, which is
	// what the preprocessing page's per-point table configures: it names a point
	// and no device. Device rules run last so a per-device setting has the final
	// say over the gateway-wide one.
	global := p.rules[""]
	rules := p.rules[deviceID]
	// Take a copy of rules to avoid holding lock during processing
	rulesCopy := make([]*models.PreprocessRule, 0, len(global)+len(rules))
	rulesCopy = append(rulesCopy, global...)
	rulesCopy = append(rulesCopy, rules...)
	p.mu.RUnlock()

	if len(rulesCopy) == 0 {
		return points
	}

	// Build a map of point name -> rules
	pointRules := make(map[string][]*models.PreprocessRule)
	for _, r := range rulesCopy {
		if !r.Enabled {
			continue
		}
		pointRules[r.PointName] = append(pointRules[r.PointName], r)
	}

	result := make([]storage.PointData, 0, len(points))
	for _, pt := range points {
		prs, ok := pointRules[pt.PointName]
		if !ok {
			result = append(result, pt)
			continue
		}

		val := pt.Value
		ts := float64(pt.Timestamp.Unix())
		skip := false

		for _, pr := range prs {
			val, skip = p.applyOperation(deviceID, pt.PointName, pr.ID, val, ts, pr)
			if skip {
				break // deadband filtered, skip this point
			}
		}
		if !skip {
			pt.Value = val
			result = append(result, pt)
		}
	}

	return result
}

// applyOperation applies a single preprocessing operation to a value.
// Returns (processedValue, shouldSkip).
func (p *Preprocessor) applyOperation(deviceID, pointName, ruleID string, val interface{}, ts float64, rule *models.PreprocessRule) (interface{}, bool) {
	floatVal, err := toFloat64(val)
	if err != nil {
		return val, false
	}

	stateKey := deviceID + ":" + pointName + ":" + ruleID

	p.mu.Lock()
	defer p.mu.Unlock()

	switch rule.Operation {
	case "scale":
		if scale, ok := rule.Params["scale"].(float64); ok {
			floatVal *= scale
		}
	case "offset":
		if offset, ok := rule.Params["offset"].(float64); ok {
			floatVal += offset
		}
	case "transform":
		scale := 1.0
		offset := 0.0
		if v, ok := rule.Params["scale"].(float64); ok {
			scale = v
		}
		if v, ok := rule.Params["offset"].(float64); ok {
			offset = v
		}
		floatVal = floatVal*scale + offset
		if rounded, ok := rule.Params["round"].(float64); ok && rounded > 0 {
			pow := math.Pow(10, rounded)
			floatVal = math.Round(floatVal*pow) / pow
		}

	case "deadband":
		deadband := p.defaultDeadband
		if d, ok := rule.Params["deadband"].(float64); ok {
			deadband = d
		}
		deadbandPct := 0.0
		if d, ok := rule.Params["deadband_percent"].(float64); ok {
			deadbandPct = d
		}

		last, hasLast := p.lastValues[stateKey]
		if !hasLast {
			p.lastValues[stateKey] = floatVal
			return floatVal, false
		}

		// Absolute deadband
		if deadband > 0 && math.Abs(floatVal-last) < deadband {
			return last, true // skip
		}

		// Percent deadband
		if deadbandPct > 0 {
			if math.Abs(last) < 1e-6 {
				if math.Abs(floatVal-last) < deadbandPct {
					return last, true
				}
			} else {
				if math.Abs(floatVal-last)/math.Abs(last)*100 < deadbandPct {
					return last, true
				}
			}
		}

		p.lastValues[stateKey] = floatVal
		return floatVal, false

	case "clamp":
		minVal := math.Inf(-1)
		maxVal := math.Inf(1)
		if v, ok := rule.Params["min"].(float64); ok {
			minVal = v
		}
		if v, ok := rule.Params["max"].(float64); ok {
			maxVal = v
		}
		if floatVal < minVal {
			floatVal = minVal
		}
		if floatVal > maxVal {
			floatVal = maxVal
		}

	case "filter":
		filterType, _ := rule.Params["filter"].(string)
		windowSize := p.defaultFilterWindow
		if w, ok := rule.Params["filter_window"].(float64); ok && w > 0 {
			windowSize = int(w)
		}
		if windowSize < 1 {
			windowSize = 5
		}

		switch {
		case filterType == "kalman":
			floatVal = p.applyKalman(stateKey, floatVal, rule.Params)
		case filterType == "ema":
			alpha := 0.3
			if a, ok := rule.Params["ema_alpha"].(float64); ok && a > 0 && a <= 1 {
				alpha = a
			}
			floatVal = p.applyEMA(stateKey, floatVal, alpha)
		case filterType == "moving_avg":
			floatVal = p.applyMovingAverage(stateKey, floatVal, windowSize)
		case len(filterType) >= 7 && filterType[:7] == "median_":
			// The width a median runs at is part of the type's own name
			// (median_5), so pairing median_5 with filter_window 3 must not
			// quietly become a 3-wide median. The operator chose the type.
			floatVal = p.applyMedianFilter(stateKey, floatVal, medianWindow(filterType, windowSize))
		}

	case "aggregate":
		aggType, _ := rule.Params["aggregate"].(string)
		aggWindow := float64(p.defaultAggregateWindowSec)
		if w, ok := rule.Params["aggregate_window_sec"].(float64); ok && w > 0 {
			aggWindow = w
		}
		if aggType == "" || aggWindow <= 0 {
			return floatVal, false
		}
		aggregated := p.applyAggregation(stateKey, floatVal, ts, aggType, aggWindow)
		if aggregated == nil {
			return floatVal, true // not enough data yet, skip reporting
		}
		floatVal = *aggregated

	case "sqrt":
		if floatVal >= 0 {
			floatVal = math.Sqrt(floatVal)
		}
	case "abs":
		floatVal = math.Abs(floatVal)
	case "negate":
		floatVal = -floatVal
	case "percent":
		if base, ok := rule.Params["base"].(float64); ok && base != 0 {
			floatVal = (floatVal / base) * 100
		}
	}

	return floatVal, false
}

// medianWindow reports how many samples a median_<N> filter type asks for,
// falling back to the configured window when the type carries no number.
func medianWindow(filterType string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimPrefix(filterType, "median_")); err == nil && n >= 1 {
		return n
	}
	return fallback
}

// applyKalman applies a 1D Kalman filter.
func (p *Preprocessor) applyKalman(key string, value float64, params map[string]interface{}) float64 {
	q := 0.001 // process noise
	r := 0.01  // measurement noise
	if v, ok := params["kalman_process_noise"].(float64); ok {
		q = v
	}
	if v, ok := params["kalman_measurement_noise"].(float64); ok {
		r = v
	}

	state, exists := p.kalmanState[key]
	if !exists {
		p.kalmanState[key] = [2]float64{value, 1.0}
		return value
	}

	estimate, variance := state[0], state[1]

	// Prediction
	predEstimate := estimate
	predVariance := variance + q

	// Update
	k := predVariance / (predVariance + r) // Kalman gain
	estimate = predEstimate + k*(value-predEstimate)
	variance = (1 - k) * predVariance

	p.kalmanState[key] = [2]float64{estimate, variance}
	return estimate
}

// applyEMA applies exponential moving average.
func (p *Preprocessor) applyEMA(key string, value, alpha float64) float64 {
	last, exists := p.emaState[key]
	if !exists {
		p.emaState[key] = value
		return value
	}
	ema := alpha*value + (1-alpha)*last
	p.emaState[key] = ema
	return ema
}

// applyMovingAverage applies moving average filter.
func (p *Preprocessor) applyMovingAverage(key string, value float64, windowSize int) float64 {
	window := p.filterWindows[key]
	window = append(window, value)
	if len(window) > windowSize {
		window = window[len(window)-windowSize:]
	}
	p.filterWindows[key] = window

	sum := 0.0
	for _, v := range window {
		sum += v
	}
	return sum / float64(len(window))
}

// applyMedianFilter applies median filter.
func (p *Preprocessor) applyMedianFilter(key string, value float64, windowSize int) float64 {
	window := p.filterWindows[key]
	window = append(window, value)
	if len(window) > windowSize {
		window = window[len(window)-windowSize:]
	}
	p.filterWindows[key] = window

	if len(window) < 3 {
		return value
	}

	sorted := make([]float64, len(window))
	copy(sorted, window)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// applyAggregation applies time-windowed aggregation.
// Returns nil if not enough data in the window.
func (p *Preprocessor) applyAggregation(key string, value, ts float64, aggType string, aggWindow float64) *float64 {
	const maxPoints = 10000

	window := p.aggregateWindows[key]
	window = append(window, aggEntry{ts: ts, value: value})
	if len(window) > maxPoints {
		window = window[len(window)-maxPoints:]
	}

	// Remove old entries outside the time window
	cutoff := ts - aggWindow
	idx := 0
	for idx < len(window) && window[idx].ts < cutoff {
		idx++
	}
	if idx > 0 {
		window = window[idx:]
	}
	p.aggregateWindows[key] = window

	if len(window) < 2 {
		return nil
	}

	values := make([]float64, len(window))
	for i, e := range window {
		values[i] = e.value
	}

	var result float64
	switch {
	case len(aggType) >= 3 && aggType[:3] == "avg":
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		result = sum / float64(len(values))
	case len(aggType) >= 3 && aggType[:3] == "max":
		result = values[0]
		for _, v := range values[1:] {
			if v > result {
				result = v
			}
		}
	case len(aggType) >= 3 && aggType[:3] == "min":
		result = values[0]
		for _, v := range values[1:] {
			if v < result {
				result = v
			}
		}
	case len(aggType) >= 3 && aggType[:3] == "sum":
		for _, v := range values {
			result += v
		}
	case len(aggType) >= 4 && aggType[:4] == "last":
		result = values[len(values)-1]
	default:
		return nil
	}
	return &result
}

// Stats returns preprocessor statistics.
func (p *Preprocessor) Stats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	totalRules := 0
	for _, rules := range p.rules {
		totalRules += len(rules)
	}
	return map[string]interface{}{
		"enabled":      p.enabled,
		"device_count": len(p.rules),
		"total_rules":  totalRules,
	}
}

// PreprocessorExpressionEngine manages derived point expressions and evaluates them
// using the ExpressionEngine for safe expression evaluation.
type PreprocessorExpressionEngine struct {
	mu          sync.RWMutex
	expressions map[string]*models.ExpressionConfig // key: expression ID
	engine      *ExpressionEngine                   // for safe evaluation
}

// NewPreprocessorExpressionEngine creates a new PreprocessorExpressionEngine.
func NewPreprocessorExpressionEngine() *PreprocessorExpressionEngine {
	return &PreprocessorExpressionEngine{
		expressions: make(map[string]*models.ExpressionConfig),
		engine:      NewExpressionEngine(),
	}
}

// AddExpression adds a derived point expression.
func (e *PreprocessorExpressionEngine) AddExpression(expr *models.ExpressionConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expressions[expr.ID] = expr
}

// RemoveExpression removes an expression.
func (e *PreprocessorExpressionEngine) RemoveExpression(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.expressions, id)
}

// ReplaceExpressions swaps the whole live set, mirroring
// Preprocessor.ReplaceRules, so what the API stored is what the collect path
// evaluates without a restart.
func (e *PreprocessorExpressionEngine) ReplaceExpressions(exprs []*models.ExpressionConfig) {
	next := make(map[string]*models.ExpressionConfig, len(exprs))
	for _, expr := range exprs {
		next[expr.ID] = expr
	}
	e.mu.Lock()
	e.expressions = next
	e.mu.Unlock()
}

// Evaluate evaluates all expressions for a device and returns derived points.
func (e *PreprocessorExpressionEngine) Evaluate(deviceID string, pointValues map[string]interface{}) []storage.PointData {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var result []storage.PointData
	for _, expr := range e.expressions {
		if !expr.Enabled {
			continue
		}
		if expr.DeviceID != "" && expr.DeviceID != deviceID {
			continue
		}
		val := e.engine.Evaluate(expr.Expression, pointValues)
		if val == nil {
			logrus.WithField("expression_id", expr.ID).
				Debug("Expression evaluation returned nil")
			continue
		}
		result = append(result, storage.PointData{
			DeviceID:  deviceID,
			PointName: expr.OutputPoint,
			Value:     val,
			Quality:   "good",
			Timestamp: time.Now(),
		})
	}
	return result
}

// GetExpressions returns all expressions.
func (e *PreprocessorExpressionEngine) GetExpressions() []*models.ExpressionConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]*models.ExpressionConfig, 0, len(e.expressions))
	for _, expr := range e.expressions {
		result = append(result, expr)
	}
	return result
}

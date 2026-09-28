package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"edgelite/internal/constants"
)

// linkageDefaultRetryCooldown is how long an evaluator waits before
// re-attempting a write that failed while the condition still holds. Retrying on
// every sample would put a load on the bus and on the device's own rate limit.
const linkageDefaultRetryCooldown = 5 * time.Second

// linkageEqualTolerance exists because device values are floats: "temp == 1" is
// never literally true for an analog reading, and an exact compare makes the
// operator's == and != rules silently dead.
const linkageEqualTolerance = 1e-9

// LinkageRule is one device-linkage rule normalised for evaluation. TargetValue
// is resolved by the caller (the API layer knows the point's declared data type)
// so the write goes out with the same Go type the UI would send.
type LinkageRule struct {
	ID             string
	Name           string
	SourceDeviceID string
	SourcePoint    string
	ConditionOp    string
	Threshold      float64
	TargetDeviceID string
	TargetPoint    string
	TargetValue    interface{}
	Enabled        bool
}

// LinkageRuleStats is what the evaluator knows about one rule at runtime.
type LinkageRuleStats struct {
	RuleID         string    `json:"rule_id"`
	Transfers      int64     `json:"transfers"`
	Errors         int64     `json:"errors"`
	SkippedSamples int64     `json:"skipped_samples"`
	LastError      string    `json:"last_error"`
	LastValue      float64   `json:"last_value"`
	HasValue       bool      `json:"has_value"`
	Satisfied      bool      `json:"satisfied"`
	RetryPending   bool      `json:"retry_pending"`
	LastTransferAt time.Time `json:"last_transfer_at"`
}

// LinkageTotals aggregates every rule.
type LinkageTotals struct {
	Rules          int   `json:"rules"`
	Armed          int   `json:"armed"`
	Transfers      int64 `json:"transfers"`
	Errors         int64 `json:"errors"`
	SkippedSamples int64 `json:"skipped_samples"`
}

type linkageState struct {
	rule LinkageRule
	// satisfied is the edge-detection memory: the rule is evaluated on every
	// sample but only writes when the comparison flips from false to true.
	satisfied bool
	// A write that failed while the condition held would otherwise be lost until
	// the signal drops and crosses again, so the rule stays eligible for a retry
	// gated by retryNotBefore (rate limiting downstream bounds the cost).
	pendingRetry   bool
	retryNotBefore time.Time
	stats          LinkageRuleStats
}

// LinkageEvaluator turns collected samples into device writes. It is deliberately
// independent of the alarm engine: an alarm informs, a linkage actuates.
type LinkageEvaluator struct {
	mu      sync.Mutex
	states  map[string]*linkageState
	order   []string
	running bool
	writeFn BridgeWriteSink
	// recordFn persists the trigger counter; a nil recorder still writes to the
	// device but the counter stays at zero, so the status reports it.
	recordFn func(ruleID string, at time.Time) error
	// retryCooldown bounds how often a refused write is re-attempted while the
	// condition keeps holding; SetRetryCooldown shortens it in tests.
	retryCooldown time.Duration
}

// NewLinkageEvaluator returns an evaluator with no rules.
func NewLinkageEvaluator() *LinkageEvaluator {
	return &LinkageEvaluator{states: map[string]*linkageState{}, retryCooldown: linkageDefaultRetryCooldown}
}

// SetWriteSink installs the function that performs the device write.
func (e *LinkageEvaluator) SetWriteSink(fn BridgeWriteSink) {
	e.mu.Lock()
	e.writeFn = fn
	e.mu.Unlock()
}

// SinkWired reports whether a write sink has been installed. Without one every
// trigger is counted as an error, so the status endpoint can name the reason.
func (e *LinkageEvaluator) SinkWired() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writeFn != nil
}

// SetTriggerRecorder installs the persistence callback for trigger counters.
func (e *LinkageEvaluator) SetTriggerRecorder(fn func(ruleID string, at time.Time) error) {
	e.mu.Lock()
	e.recordFn = fn
	e.mu.Unlock()
}

// SetRetryCooldown overrides the gap between retries of a refused write.
func (e *LinkageEvaluator) SetRetryCooldown(d time.Duration) {
	if d <= 0 {
		d = linkageDefaultRetryCooldown
	}
	e.mu.Lock()
	e.retryCooldown = d
	e.mu.Unlock()
}

// SetStarted records whether collected data is being fed in. Start/Stop is owned
// by the caller that wires the event bus.
func (e *LinkageEvaluator) SetStarted(started bool) {
	e.mu.Lock()
	e.running = started
	e.mu.Unlock()
}

// Started reports whether the evaluator is being fed.
func (e *LinkageEvaluator) Started() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// SetRules replaces the rule set, carrying over the stats of rules that survive
// so editing a rule does not reset its counters.
func (e *LinkageEvaluator) SetRules(rules []LinkageRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.states
	next := make(map[string]*linkageState, len(rules))
	order := make([]string, 0, len(rules))
	for _, r := range rules {
		st := &linkageState{rule: r, stats: LinkageRuleStats{RuleID: r.ID}}
		if prev, ok := old[r.ID]; ok {
			st.stats = prev.stats
			st.stats.RuleID = r.ID
			// Keep the edge memory only while the rule is still enabled; disabling
			// and re-enabling must allow a fresh trigger.
			st.satisfied = prev.satisfied && r.Enabled
			// An action the device refused stays owed across an edit, otherwise
			// fixing a threshold would silently drop the pending retry.
			st.pendingRetry = prev.pendingRetry
			st.retryNotBefore = prev.retryNotBefore
		}
		next[r.ID] = st
		order = append(order, r.ID)
	}
	e.states = next
	e.order = order
}

// RuleCount returns how many rules are loaded.
func (e *LinkageEvaluator) RuleCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.states)
}

// RuleStats returns live stats for one rule, or nil when the rule is not loaded.
func (e *LinkageEvaluator) RuleStats(id string) *LinkageRuleStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.states[id]
	if !ok {
		return nil
	}
	cp := st.stats
	cp.Satisfied = st.satisfied
	cp.RetryPending = st.pendingRetry
	return &cp
}

// Totals returns aggregate counters across every rule.
func (e *LinkageEvaluator) Totals() LinkageTotals {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := LinkageTotals{Rules: len(e.states)}
	for _, st := range e.states {
		t.Transfers += st.stats.Transfers
		t.Errors += st.stats.Errors
		t.SkippedSamples += st.stats.SkippedSamples
		if st.satisfied && st.rule.Enabled {
			t.Armed++
		}
	}
	return t
}

// Evaluate feeds one collected sample to the rules watching that point. A rule
// whose source sample is not numeric is skipped and counted: silently treating a
// string as zero would actuate devices on garbage.
func (e *LinkageEvaluator) Evaluate(ctx context.Context, deviceID, point string, value interface{}) {
	now := time.Now()
	num, ok := linkageAsFloat(value)
	e.mu.Lock()
	var due []*linkageState
	for _, id := range e.order {
		st := e.states[id]
		r := st.rule
		if !r.Enabled || r.SourceDeviceID != deviceID || r.SourcePoint != point {
			continue
		}
		if !ok {
			st.stats.SkippedSamples++
			st.stats.LastError = fmt.Sprintf("source value %v is not numeric", value)
			continue
		}
		st.stats.LastValue = num
		st.stats.HasValue = true
		sat := linkageCompare(r.ConditionOp, num, r.Threshold)
		// A fresh crossing always fires. A rule that is still satisfied fires
		// again only if the previous attempt failed and its cooldown elapsed;
		// without that, a device that was offline at the crossing moment would
		// lose the action until the signal dropped and rose again.
		fire := false
		if sat {
			switch {
			case !st.satisfied:
				fire = true
			case st.pendingRetry && !now.Before(st.retryNotBefore):
				fire = true
			}
		}
		st.satisfied = sat
		if fire {
			due = append(due, st)
		}
	}
	writeFn := e.writeFn
	recordFn := e.recordFn
	cooldown := e.retryCooldown
	e.mu.Unlock()

	for _, st := range due {
		r := st.rule
		if writeFn == nil {
			e.mu.Lock()
			st.stats.Errors++
			st.stats.LastError = "no write sink configured"
			st.pendingRetry = true
			st.retryNotBefore = now.Add(cooldown)
			e.mu.Unlock()
			continue
		}
		if err := writeFn(ctx, r.TargetDeviceID, r.TargetPoint, r.TargetValue); err != nil {
			e.mu.Lock()
			st.stats.Errors++
			st.stats.LastError = fmt.Sprintf("write %s.%s: %s", r.TargetDeviceID, r.TargetPoint, err.Error())
			st.pendingRetry = true
			st.retryNotBefore = now.Add(cooldown)
			e.mu.Unlock()
			continue
		}
		attemptedAt := now
		e.mu.Lock()
		st.stats.Transfers++
		st.stats.LastError = ""
		st.stats.LastTransferAt = attemptedAt
		st.pendingRetry = false
		st.retryNotBefore = time.Time{}
		e.mu.Unlock()
		if recordFn != nil {
			if err := recordFn(r.ID, attemptedAt); err != nil {
				e.mu.Lock()
				st.stats.Errors++
				st.stats.LastError = "trigger counter not persisted: " + err.Error()
				e.mu.Unlock()
			}
		}
	}
}

func linkageAsFloat(v interface{}) (float64, bool) {
	return constants.NumericAsFloat(v)
}

func linkageCompare(op string, v, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case "<":
		return v < threshold
	case ">=":
		return v >= threshold
	case "<=":
		return v <= threshold
	case "==":
		// A device reading is a float64 that went through JSON, so an exact match
		// with the configured threshold is representation luck. 1e-9 absorbs that
		// noise without letting a genuinely different value trigger.
		return math.Abs(v-threshold) <= linkageEqualTolerance
	case "!=":
		return math.Abs(v-threshold) > linkageEqualTolerance
	}
	return false
}

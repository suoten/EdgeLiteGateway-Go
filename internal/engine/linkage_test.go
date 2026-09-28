package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Device linkage rows used to be written by the API and read by nobody, so a rule
// could never fire. These tests pin the evaluator down: it fires on the crossing,
// not on every sample, and its counters only move for writes that actually landed.

func linkageRule() LinkageRule {
	return LinkageRule{
		ID: "l1", Name: "hot -> fan on",
		SourceDeviceID: "src", SourcePoint: "temp",
		ConditionOp: ">", Threshold: 30,
		TargetDeviceID: "dst", TargetPoint: "fan", TargetValue: true,
		Enabled: true,
	}
}

func TestLinkageFiresOncePerCrossing(t *testing.T) {
	ev := NewLinkageEvaluator()
	var mu sync.Mutex
	var writes []interface{}
	ev.SetWriteSink(func(_ context.Context, _, _ string, value interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		writes = append(writes, value)
		return nil
	})
	ev.SetRules([]LinkageRule{linkageRule()})

	ctx := context.Background()
	// 25, 26, 27 stay below the threshold: nothing must be written.
	for _, v := range []float64{25, 26, 27} {
		ev.Evaluate(ctx, "src", "temp", v)
	}
	// The crossing writes exactly once even though the condition keeps holding.
	for _, v := range []float64{31, 32, 33} {
		ev.Evaluate(ctx, "src", "temp", v)
	}
	mu.Lock()
	first := len(writes)
	mu.Unlock()
	if first != 1 {
		t.Fatalf("writes after one crossing = %d, want 1", first)
	}

	// Falling back below the threshold re-arms the rule; crossing again writes again.
	ev.Evaluate(ctx, "src", "temp", 10)
	ev.Evaluate(ctx, "src", "temp", 40)
	mu.Lock()
	second := len(writes)
	mu.Unlock()
	if second != 2 {
		t.Fatalf("writes after a second crossing = %d, want 2", second)
	}
	if s := ev.RuleStats("l1"); s == nil || s.Transfers != 2 || s.Errors != 0 {
		t.Fatalf("stats = %#v, want 2 transfers 0 errors", s)
	}
}

func TestLinkageIgnoresUnrelatedSamples(t *testing.T) {
	ev := NewLinkageEvaluator()
	var called int
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { called++; return nil })
	ev.SetRules([]LinkageRule{linkageRule()})
	ev.Evaluate(context.Background(), "other", "temp", 999)
	ev.Evaluate(context.Background(), "src", "humidity", 999)
	if called != 0 {
		t.Fatalf("sink called %d times for samples the rule does not watch", called)
	}
}

func TestLinkageSkipsNonNumericSource(t *testing.T) {
	ev := NewLinkageEvaluator()
	var called int
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { called++; return nil })
	ev.SetRules([]LinkageRule{linkageRule()})
	// Treating "abc" as 0 would flip ">" into "<" style behaviour on garbage data.
	ev.Evaluate(context.Background(), "src", "temp", "abc")
	if called != 0 {
		t.Fatalf("sink called for a non-numeric sample")
	}
	s := ev.RuleStats("l1")
	if s == nil || s.SkippedSamples != 1 || s.LastError == "" {
		t.Fatalf("skipped sample not reported: %#v", s)
	}
}

func TestLinkageRecordsWriteFailure(t *testing.T) {
	ev := NewLinkageEvaluator()
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error {
		return errors.New("device offline")
	})
	ev.SetRules([]LinkageRule{linkageRule()})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	s := ev.RuleStats("l1")
	if s == nil || s.Transfers != 0 || s.Errors != 1 {
		t.Fatalf("failed write counted as %#v", s)
	}
	if s.LastError == "" {
		t.Fatal("failed write must leave a reason for the UI")
	}
}

func TestLinkageWithoutSinkReportsWhy(t *testing.T) {
	ev := NewLinkageEvaluator()
	ev.SetRules([]LinkageRule{linkageRule()})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	s := ev.RuleStats("l1")
	if s == nil || s.Errors != 1 {
		t.Fatalf("no-sink case: %#v", s)
	}
	if s.LastError != "no write sink configured" {
		t.Fatalf("last_error = %q", s.LastError)
	}
}

func TestLinkageDisabledRuleNeverFires(t *testing.T) {
	ev := NewLinkageEvaluator()
	var called int
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { called++; return nil })
	r := linkageRule()
	r.Enabled = false
	ev.SetRules([]LinkageRule{r})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	if called != 0 {
		t.Fatal("disabled rule wrote to the device")
	}
}

func TestLinkageSetRulesKeepsStatsAndReArms(t *testing.T) {
	ev := NewLinkageEvaluator()
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { return nil })
	ev.SetRules([]LinkageRule{linkageRule()})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	before := ev.RuleStats("l1")
	if before == nil || before.Transfers != 1 {
		t.Fatalf("setup: %#v", before)
	}

	// Editing the rule (same id) must not reset the counter an operator can see.
	edited := linkageRule()
	edited.Threshold = 40
	ev.SetRules([]LinkageRule{edited})
	after := ev.RuleStats("l1")
	if after == nil || after.Transfers != 1 {
		t.Fatalf("counters lost on edit: %#v", after)
	}
	// The rule is still above its old threshold, so it must not re-fire until it
	// drops and crosses again.
	ev.Evaluate(context.Background(), "src", "temp", 50)
	if s := ev.RuleStats("l1"); s.Transfers != 1 {
		t.Fatalf("rule re-fired without a new crossing: %#v", s)
	}

	// Disabling then re-enabling re-arms the edge memory on purpose.
	off := linkageRule()
	off.Enabled = false
	ev.SetRules([]LinkageRule{off})
	on := linkageRule()
	ev.SetRules([]LinkageRule{on})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	if s := ev.RuleStats("l1"); s.Transfers != 2 {
		t.Fatalf("re-enabled rule did not get a fresh trigger: %#v", s)
	}
}

func TestLinkageTriggerRecorderPersists(t *testing.T) {
	ev := NewLinkageEvaluator()
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { return nil })
	var mu sync.Mutex
	var recorded []string
	ev.SetTriggerRecorder(func(id string, at time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, id)
		return nil
	})
	ev.SetRules([]LinkageRule{linkageRule()})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	mu.Lock()
	n := len(recorded)
	mu.Unlock()
	if n != 1 || recorded[0] != "l1" {
		t.Fatalf("recorded = %v, want one record for l1", recorded)
	}
}

func TestLinkageRetriesARefusedWrite(t *testing.T) {
	ev := NewLinkageEvaluator()
	ev.SetRetryCooldown(30 * time.Millisecond)
	var mu sync.Mutex
	fail := true
	var writes int
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return errors.New("device offline")
		}
		writes++
		return nil
	})
	ev.SetRules([]LinkageRule{linkageRule()})
	ctx := context.Background()

	// The crossing attempt fails; the condition then keeps holding, which is
	// exactly the case an edge-only evaluator used to lose.
	ev.Evaluate(ctx, "src", "temp", 50)
	mu.Lock()
	sawFail := writes == 0
	mu.Unlock()
	if !sawFail {
		t.Fatal("first attempt should have failed")
	}
	if s := ev.RuleStats("l1"); s == nil || !s.RetryPending || s.Errors != 1 {
		t.Fatalf("refused write not marked for retry: %#v", s)
	}
	// Inside the cooldown nothing is re-attempted.
	ev.Evaluate(ctx, "src", "temp", 51)
	time.Sleep(40 * time.Millisecond)
	// After it, the still-satisfied condition is acted on.
	ev.Evaluate(ctx, "src", "temp", 52)
	mu.Lock()
	got := writes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("retry did not happen: writes=%d stats=%#v", got, ev.RuleStats("l1"))
	}
	s := ev.RuleStats("l1")
	if s.RetryPending || s.Transfers != 1 {
		t.Fatalf("successful retry not cleared: %#v", s)
	}
}

func TestLinkageEqualUsesTolerance(t *testing.T) {
	ev := NewLinkageEvaluator()
	var mu sync.Mutex
	var writes int
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		writes++
		return nil
	})
	r := linkageRule()
	r.ConditionOp, r.Threshold = "==", 20.0
	ev.SetRules([]LinkageRule{r})
	// A reading that only differs in the last bits must still match, otherwise an
	// == rule is silently dead for anything but the identical float64.
	ev.Evaluate(context.Background(), "src", "temp", 20.0+1e-12)
	mu.Lock()
	got := writes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("== with float noise did not trigger: writes=%d", got)
	}
}

func TestLinkageTotalsAndStarted(t *testing.T) {
	ev := NewLinkageEvaluator()
	if ev.Started() {
		t.Fatal("a fresh evaluator must not claim to be running")
	}
	ev.SetStarted(true)
	if !ev.Started() {
		t.Fatal("SetStarted(true) not reflected")
	}
	ev.SetWriteSink(func(context.Context, string, string, interface{}) error { return nil })
	r1 := linkageRule()
	r2 := linkageRule()
	r2.ID, r2.Name = "l2", "cold -> heater on"
	r2.ConditionOp, r2.Threshold = "<", 5
	ev.SetRules([]LinkageRule{r1, r2})
	ev.Evaluate(context.Background(), "src", "temp", 50)
	tot := ev.Totals()
	if tot.Rules != 2 || tot.Transfers != 1 || tot.Armed != 1 {
		t.Fatalf("totals = %#v, want 2 rules, 1 transfer, 1 armed", tot)
	}
	if ev.RuleCount() != 2 {
		t.Fatalf("RuleCount = %d", ev.RuleCount())
	}
}

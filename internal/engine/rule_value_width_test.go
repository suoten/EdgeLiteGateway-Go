package engine

import (
	"testing"
	"time"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// A threshold rule on a Modbus / S7 / FINS tag never fired: those drivers return
// uint16, int16 and uint32, and the evaluator's own numeric conversion listed
// only float64/float32/int/int32/int64/bool. A value it could not read was
// treated as "condition false" and skipped without a word, so the alarm chain
// looked healthy while it was deaf to every integer sample.

func TestThresholdRuleFiresForEveryDriverNumericKind(t *testing.T) {
	rule := &models.RuleResponse{
		RuleID:   "rule-width",
		Name:     "over five",
		DeviceID: "dev-1",
		Severity: "warning",
		Enabled:  true,
		RuleType: "threshold",
		Logic:    "AND",
		Conditions: []models.RuleCondition{
			{Point: "reg", Operator: ">", Threshold: 5},
		},
	}
	evaluator := NewRuleEvaluator(nil, nil, nil)
	now := time.Now()
	for _, tc := range []struct {
		name  string
		value interface{}
	}{
		{"uint16", uint16(7)},
		{"int16", int16(7)},
		{"uint32", uint32(7)},
		{"uint8", uint8(7)},
		{"int", int(7)},
		{"float64", float64(7)},
		{"numeric text", "7"},
	} {
		points := []storage.PointData{
			{DeviceID: "dev-1", PointName: "reg", Value: tc.value, Quality: "good", Timestamp: now},
		}
		triggered, values, err := evaluator.evaluateRule(rule, points)
		if err != nil {
			t.Fatalf("%s: evaluateRule: %v", tc.name, err)
		}
		if !triggered {
			t.Fatalf("%s: value %v did not trigger a > 5 rule, want it to (conditions are silently skipped when the value cannot be read)", tc.name, tc.value)
		}
		if _, ok := values["reg"]; !ok {
			t.Fatalf("%s: trigger values = %#v, want the sampled value reported", tc.name, values)
		}
	}
}

// A value that is genuinely not a number must still not trigger, rather than
// being coerced into zero and alarming on that.
func TestThresholdRuleStaysSilentOnNonNumericValue(t *testing.T) {
	rule := &models.RuleResponse{
		RuleID:     "rule-text",
		Name:       "over five",
		DeviceID:   "dev-1",
		Severity:   "warning",
		Enabled:    true,
		RuleType:   "threshold",
		Logic:      "AND",
		Conditions: []models.RuleCondition{{Point: "reg", Operator: ">", Threshold: 5}},
	}
	evaluator := NewRuleEvaluator(nil, nil, nil)
	points := []storage.PointData{
		{DeviceID: "dev-1", PointName: "reg", Value: "setpoint-a", Quality: "good", Timestamp: time.Now()},
	}
	triggered, values, err := evaluator.evaluateRule(rule, points)
	if err != nil {
		t.Fatalf("evaluateRule: %v", err)
	}
	if triggered || len(values) != 0 {
		t.Fatalf("triggered=%v values=%#v, want no alarm for a tag that holds text", triggered, values)
	}
}

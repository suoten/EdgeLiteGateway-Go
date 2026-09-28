package engine

// The Preprocessor filters what every subscriber sees, and until this round
// nothing built one - so 536 lines of filtering had no test either, which is how
// it stayed invisible for as long as it did. These cover the behaviour the
// preprocessing page promises: a gateway-wide per-point rule reaching every
// device, a deadband dropping a reading, and a median filter running at the
// width its own name states.

import (
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

func point(deviceID, name string, value float64, ts time.Time) storage.PointData {
	return storage.PointData{DeviceID: deviceID, PointName: name, Value: value, Quality: "good", Timestamp: ts}
}

func newTestPreprocessor(rules ...*models.PreprocessRule) *Preprocessor {
	p := NewPreprocessor(&config.PreprocessGlobalConfig{Enabled: true, DefaultFilterWindow: 3})
	p.ReplaceRules(rules)
	return p
}

func TestGlobalRuleAppliesToEveryDeviceAndDeviceRuleWins(t *testing.T) {
	start := time.Now()
	rules := []*models.PreprocessRule{
		// A gateway-wide row from the preprocessing page: no device, negate the value.
		{ID: "pp-global", DeviceID: "", PointName: "temp", Operation: "negate", Enabled: true},
		// The same point on one device is also clamped, which must run afterwards.
		{ID: "pp-dev", DeviceID: "dev-1", PointName: "temp", Operation: "clamp",
			Params: map[string]interface{}{"min": -5.0}, Enabled: true},
	}
	p := newTestPreprocessor(rules...)

	globalOnly := p.Process("dev-2", []storage.PointData{point("dev-2", "temp", 7, start)})
	if len(globalOnly) != 1 {
		t.Fatalf("global rule must reach a device with no rules of its own, got %d points", len(globalOnly))
	}
	if globalOnly[0].Value != float64(-7) {
		t.Fatalf("dev-2 temp = %v, want -7 from the gateway-wide negate rule", globalOnly[0].Value)
	}

	scoped := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 7, start)})
	if len(scoped) != 1 || scoped[0].Value != float64(-5) {
		t.Fatalf("dev-1 temp = %+v, want -5 so the device clamp runs after the global negate", scoped)
	}
}

func TestDisabledRuleDoesNotFilter(t *testing.T) {
	start := time.Now()
	p := newTestPreprocessor(&models.PreprocessRule{
		ID: "pp-1", DeviceID: "dev-1", PointName: "temp", Operation: "deadband",
		Params: map[string]interface{}{"deadband": 10.0}, Enabled: false,
	})
	out := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 1, start), point("dev-1", "temp", 2, start)})
	if len(out) != 2 {
		t.Fatalf("an off rule dropped points: %+v", out)
	}
}

func TestDeadbandDropsAReadingThatBarelyMoved(t *testing.T) {
	start := time.Now()
	p := newTestPreprocessor(&models.PreprocessRule{
		ID: "pp-1", DeviceID: "dev-1", PointName: "temp", Operation: "deadband",
		Params: map[string]interface{}{"deadband": 2.0}, Enabled: true,
	})
	out := p.Process("dev-1", []storage.PointData{
		point("dev-1", "temp", 20, start),
		point("dev-1", "temp", 21, start.Add(time.Second)),
		point("dev-1", "temp", 30, start.Add(2*time.Second)),
	})
	if len(out) != 2 {
		t.Fatalf("want the 21 reading dropped, got %+v", out)
	}
	if valueOf(t, out[0].Value) != 20 || valueOf(t, out[1].Value) != 30 {
		t.Fatalf("surviving values = %v, %v, want 20 and 30", out[0].Value, out[1].Value)
	}
	// The dropped 21 must not have become the deadband reference: compared to 21
	// this reading is 10 away and would pass, compared to the last accepted 30 it
	// is 1 away and has to be dropped.
	dropped := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 31, start.Add(3*time.Second))})
	if len(dropped) != 0 {
		t.Fatalf("31 is within the deadband of the last accepted 30 and must be dropped, got %+v", dropped)
	}
}

func TestMedianFilterRunsAtTheWidthItsNameStates(t *testing.T) {
	// The page offers median_3/5/7 and sends filter_window 3 with all of them.
	// A rule pairing median_7 with window 3 has to be a 7-wide median, because
	// that is what the operator selected.
	start := time.Now()
	p := newTestPreprocessor(&models.PreprocessRule{
		ID: "pp-1", DeviceID: "dev-1", PointName: "temp", Operation: "filter",
		Params: map[string]interface{}{"filter": "median_5", "filter_window": float64(3)}, Enabled: true,
	})
	values := []float64{10, 11, 100, 12, 13}
	var last interface{}
	for i, v := range values {
		out := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", v, start.Add(time.Duration(i)*time.Second))})
		if len(out) != 1 {
			t.Fatalf("value %v: want one point out, got %+v", v, out)
		}
		last = out[0].Value
	}
	// A 3-wide median would have dropped the 100 spike out of its window by the
	// fifth sample and answered 13; 12 is what a 5-wide median answers, which is
	// the type the rule names.
	if last != 12.0 {
		t.Fatalf("median of 10,11,100,12,13 after five samples = %v, want 12 (a 5-wide median)", last)
	}
}

func TestAggregateHoldsPointsUntilTheWindowHasEnough(t *testing.T) {
	start := time.Now()
	p := newTestPreprocessor(&models.PreprocessRule{
		ID: "pp-1", DeviceID: "dev-1", PointName: "temp", Operation: "aggregate",
		Params: map[string]interface{}{"aggregate": "avg", "aggregate_window_sec": float64(60)}, Enabled: true,
	})
	if out := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 10, start)}); len(out) != 0 {
		t.Fatalf("one sample cannot answer an average; got %+v", out)
	}
	out := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 20, start.Add(time.Second))})
	if len(out) != 1 || valueOf(t, out[0].Value) != 15 {
		t.Fatalf("avg of 10 and 20 = %+v, want one point at 15", out)
	}
}

// valueOf reads a processed point back as a number. PointData.Value is an
// interface{}, so comparing it to an untyped integer constant in a test compares
// float64 against int and fails on values that look identical.
func valueOf(t *testing.T, value interface{}) float64 {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("processed value is %T (%v), want a float64", value, value)
	}
	return number
}

func TestResetStateForgetsTheFilterWindows(t *testing.T) {
	start := time.Now()
	p := newTestPreprocessor(&models.PreprocessRule{
		ID: "pp-1", DeviceID: "dev-1", PointName: "temp", Operation: "deadband",
		Params: map[string]interface{}{"deadband": 2.0}, Enabled: true,
	})
	p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 20, start)})
	p.ResetState()
	// After a reset the first reading is accepted again rather than compared to a
	// value the operator can no longer see.
	if out := p.Process("dev-1", []storage.PointData{point("dev-1", "temp", 20.5, start.Add(time.Second))}); len(out) != 1 {
		t.Fatalf("after ResetState the first reading must pass, got %+v", out)
	}
}

func TestExpressionEngineReportsWhyItFailed(t *testing.T) {
	e := NewExpressionEngine()

	if _, err := e.EvaluateDetailed("${boiler.temp} * 2", map[string]interface{}{}); err == nil {
		t.Fatal("an unresolvable variable reference must be reported, not answered as null")
	} else if got := err.Error(); got == "" {
		t.Fatal("the failure must carry a reason")
	}

	result, err := e.EvaluateDetailed("${boiler.temp} * 2", map[string]interface{}{"boiler.temp": 3.0})
	if err != nil {
		t.Fatalf("a resolvable expression failed: %v", err)
	}
	if result != 6.0 {
		t.Fatalf("result = %v, want 6", result)
	}

	if _, err := e.EvaluateDetailed("sin(1)", nil); err == nil {
		t.Fatal("a function this parser does not have must be an error")
	}

	if _, err := e.EvaluateDetailed("1 / (2 - 2)", nil); err == nil {
		t.Fatal("a division by zero must be an error")
	}

	if _, err := e.EvaluateDetailed("   ", nil); err == nil {
		t.Fatal("an empty expression must be an error, not a nil result")
	}
}

// The filters the preprocessing page offers are tuned by different parameters: a
// median and a moving average by a sample window, EMA by alpha, Kalman by its two
// noise settings. These pin that the parameters the rule carries are the ones the
// engine uses, rather than the constants the code falls back to.
func TestEmaUsesTheAlphaTheRuleCarries(t *testing.T) {
	start := time.Now()
	step := []storage.PointData{
		point("dev-1", "temp", 0, start),
		point("dev-1", "temp", 0, start.Add(time.Second)),
		point("dev-1", "temp", 100, start.Add(2*time.Second)),
	}
	newEma := func(alpha float64) *Preprocessor {
		return newTestPreprocessor(&models.PreprocessRule{
			ID: "pp-ema", DeviceID: "dev-1", PointName: "temp", Operation: "filter",
			Params: map[string]interface{}{"filter": "ema", "ema_alpha": alpha}, Enabled: true,
		})
	}

	tracking := newEma(1.0).Process("dev-1", step)
	if got := valueOf(t, tracking[len(tracking)-1].Value); got != 100 {
		t.Fatalf("ema at alpha 1 = %v, want it to follow the raw step exactly", got)
	}

	lagging := newEma(0.1).Process("dev-1", step)
	if got := valueOf(t, lagging[len(lagging)-1].Value); got > 20 {
		t.Fatalf("ema at alpha 0.1 = %v, want it to stay near 10 after one step", got)
	}
}

func TestKalmanUsesTheNoiseSettingsTheRuleCarries(t *testing.T) {
	start := time.Now()
	step := []storage.PointData{
		point("dev-1", "temp", 0, start),
		point("dev-1", "temp", 0, start.Add(time.Second)),
		point("dev-1", "temp", 100, start.Add(2*time.Second)),
	}
	newKalman := func(q, r float64) *Preprocessor {
		return newTestPreprocessor(&models.PreprocessRule{
			ID: "pp-kalman", DeviceID: "dev-1", PointName: "temp", Operation: "filter",
			Params: map[string]interface{}{
				"filter":                   "kalman",
				"kalman_process_noise":     q,
				"kalman_measurement_noise": r,
			},
			Enabled: true,
		})
	}

	// A measurement the filter distrusts must not be allowed to move the estimate.
	distrustful := newKalman(0.001, 1000).Process("dev-1", step)
	if got := valueOf(t, distrustful[len(distrustful)-1].Value); got > 20 {
		t.Fatalf("kalman with measurement noise 1000 = %v, want it to keep doubting the step", got)
	}

	// The opposite settings must let the reading through almost unchanged.
	trusting := newKalman(1000, 0.001).Process("dev-1", step)
	if got := valueOf(t, trusting[len(trusting)-1].Value); got < 90 {
		t.Fatalf("kalman with process noise 1000 = %v, want it to accept the step", got)
	}
}

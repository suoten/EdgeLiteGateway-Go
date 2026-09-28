package api

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/drivers"
	"edgelite/internal/models"
)

// These handlers used to answer literal zeros and empty lists for measurements
// this build does not take, so an operator looking at the observability page
// could not tell "everything is fast" from "nobody measured anything".
// The tests below pin the two directions of that contract: a value that is
// reported must equal the samples that were taken, and a value that is not
// measured must be null and named in not_collected.

func decodeData(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data is not an object: %s (%v)", raw, err)
	}
	return m
}

// notCollectedList reads the marker from either shape: the payload builders are
// tested on their Go values, the handlers on their JSON encoding, where the same
// list arrives as []interface{}.
func notCollectedList(t *testing.T, m map[string]interface{}) []string {
	t.Helper()
	v, present := m["not_collected"]
	if !present {
		return nil
	}
	if strs, ok := v.([]string); ok {
		return strs
	}
	arr, ok := v.([]interface{})
	if !ok {
		t.Fatalf("not_collected is %T, want a list", v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("not_collected holds %T (%v), want strings", e, e)
		}
		out = append(out, s)
	}
	return out
}

func hasKey(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestPercentileOfRanksThePooledSamples(t *testing.T) {
	// 100 samples of 1..100 ms: nearest-rank on a 0-based index.
	ascending := make([]float64, 100)
	for i := range ascending {
		ascending[i] = float64(i + 1)
	}
	for _, tc := range []struct {
		pct  float64
		want float64
	}{
		// Nearest-rank: p of n samples is the value at rank ceil(p/100*n), so for
		// 1..100 ms the percentile equals the sample that many ms out.
		{50, 50},
		{90, 90},
		{95, 95},
		{99, 99},
		{0, 1},
		{100, 100},
	} {
		got, ok := percentileOf(ascending, tc.pct)
		if !ok {
			t.Fatalf("percentileOf(1..100, %v) reported no measurement", tc.pct)
		}
		if got != tc.want {
			t.Errorf("p%v of 1..100 = %v, want %v", tc.pct, got, tc.want)
		}
	}
	if _, ok := percentileOf(nil, 95); ok {
		t.Error("percentileOf on no samples must report nothing measured, not a 0")
	}
}

func TestLatencyStatsPayloadMatchesTheSamples(t *testing.T) {
	samples := []float64{4, 8, 15, 16, 23, 42}
	got := latencyStatsPayload(samples, 2, 100)
	if hasKey(notCollectedList(t, got), "avg_ms") {
		t.Fatalf("a measured gateway still reported avg_ms uncollected: %v", got)
	}
	// avg = 108/6 = 18
	if avg, ok := got["avg_ms"].(float64); !ok || math.Abs(avg-18) > 1e-9 {
		t.Errorf("avg_ms = %v, want 18", got["avg_ms"])
	}
	if mn, ok := got["min_ms"].(float64); !ok || mn != 4 {
		t.Errorf("min_ms = %v, want 4", got["min_ms"])
	}
	if mx, ok := got["max_ms"].(float64); !ok || mx != 42 {
		t.Errorf("max_ms = %v, want 42", got["max_ms"])
	}
	if n, ok := got["sample_count"].(int); !ok || n != len(samples) {
		t.Errorf("sample_count = %v, want %d", got["sample_count"], len(samples))
	}
	if d, ok := got["devices_reporting"].(int); !ok || d != 2 {
		t.Errorf("devices_reporting = %v, want 2", got["devices_reporting"])
	}
	if w, ok := got["window_per_device"].(int); !ok || w != 100 {
		t.Errorf("window_per_device = %v, want 100", got["window_per_device"])
	}
}

func TestLatencyStatsPayloadNullsWhatWasNotMeasured(t *testing.T) {
	got := latencyStatsPayload(nil, 0, 0)
	nc := notCollectedList(t, got)
	for _, key := range []string{"avg_ms", "min_ms", "max_ms"} {
		if v, present := got[key]; !present || v != nil {
			t.Errorf("%s = %#v, want null with no samples", key, v)
		}
		if !hasKey(nc, key) {
			t.Errorf("%s is null but not named in not_collected %v", key, nc)
		}
	}
}

func TestLatencyPercentilePayload(t *testing.T) {
	ascending := make([]float64, 100)
	for i := range ascending {
		ascending[i] = float64(i + 1)
	}
	got := latencyPercentilePayload(ascending, 1, 100)
	if p95, ok := got["p95"].(float64); !ok || p95 != 95 {
		t.Errorf("p95 = %v, want 95", got["p95"])
	}
	if nc := notCollectedList(t, got); len(nc) != 0 {
		t.Errorf("a fully measured distribution named %v uncollected", nc)
	}
	// The payload must not reorder the caller's slice: the latency handler pools
	// samples that another panel reports oldest-first.
	shuffled := []float64{42, 4, 23, 8, 16, 15}
	before := append([]float64(nil), shuffled...)
	if _, ok := latencyPercentilePayload(shuffled, 1, 100)["p50"]; !ok {
		t.Fatal("p50 reported unmeasured for 6 samples")
	}
	for i := range before {
		if shuffled[i] != before[i] {
			t.Fatalf("latencyPercentilePayload mutated its input: %v (was %v)", shuffled, before)
		}
	}
}

func TestLatencyPercentilePayloadNullsWithoutSamples(t *testing.T) {
	got := latencyPercentilePayload(nil, 0, 0)
	nc := notCollectedList(t, got)
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		if v, present := got[key]; !present || v != nil {
			t.Errorf("%s = %#v, want null with no samples", key, v)
		}
		if !hasKey(nc, key) {
			t.Errorf("%s is null but not named in not_collected %v", key, nc)
		}
	}
}

func TestLatencyHistogramPayloadCountsEverySampleOnce(t *testing.T) {
	// One sample exactly on each bound -- an inclusive/exclusive slip at the
	// bucket edge has to move one of these -- plus two above the last bound.
	samples := []float64{0.5, 5, 10, 50, 100, 500, 1000, 1500, 9999}
	got := latencyHistogramPayload(samples)
	rawBuckets, ok := got["buckets"].([]map[string]interface{})
	if !ok {
		t.Fatalf("buckets is %T", got["buckets"])
	}
	if len(rawBuckets) != len(latencyHistogramBoundsMs)+1 {
		t.Fatalf("got %d buckets, want %d", len(rawBuckets), len(latencyHistogramBoundsMs)+1)
	}
	total := 0
	for i, b := range rawBuckets {
		n, ok := b["count"].(int)
		if !ok {
			t.Fatalf("bucket %d count is %T", i, b["count"])
		}
		total += n
		if i == len(rawBuckets)-1 {
			if n != 2 {
				t.Errorf("overflow bucket = %d, want 2 (samples above %v ms)", n, latencyHistogramBoundsMs[len(latencyHistogramBoundsMs)-1])
			}
			if le, present := b["le_ms"]; !present || le != nil {
				t.Errorf("overflow bucket must not claim a finite bound, le_ms=%v", le)
			}
			continue
		}
		if n != 1 {
			t.Errorf("bucket %d (le_ms=%v) = %d, want 1", i, b["le_ms"], n)
		}
	}
	if total != len(samples) {
		t.Errorf("buckets hold %d of %d samples - a sample was dropped or double counted", total, len(samples))
	}
	if m, ok := got["measured"].(bool); !ok || !m {
		t.Errorf("measured = %v, want true with samples", got["measured"])
	}
	empty := latencyHistogramPayload(nil)
	if m, ok := empty["measured"].(bool); !ok || m {
		t.Errorf("measured = %v with no samples, want false", empty["measured"])
	}
}

func TestObservabilityUnsupportedPanelsSaySo(t *testing.T) {
	for _, tc := range []struct {
		path string
		h    func(echo.Context) error
	}{
		{"/api/v1/observability/alerts/rules", handleGetAlertRules},
		{"/api/v1/observability/alerts/events", handleGetAlertEvents},
	} {
		status, _, raw, _ := callHandler(t, http.MethodGet, tc.path, "", tc.h)
		if status != http.StatusOK {
			t.Fatalf("%s -> %d, want 200", tc.path, status)
		}
		m := decodeData(t, raw)
		if supported, ok := m["supported"].(bool); !ok || supported {
			t.Errorf("%s: supported = %#v, want false - this build has no observability alert engine", tc.path, m["supported"])
		}
		items, ok := m["items"].([]interface{})
		if !ok {
			t.Fatalf("%s: items is %T, want a list", tc.path, m["items"])
		}
		if len(items) != 0 {
			t.Errorf("%s: items = %v, want empty while unsupported", tc.path, items)
		}
	}
}

func TestObservabilityTracePanelsAreUnsupported(t *testing.T) {
	status, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/observability/traces/msg-1", "", handleGetTrace, "trace_id", "msg-1")
	if status != http.StatusOK {
		t.Fatalf("GET trace -> %d", status)
	}
	m := decodeData(t, raw)
	if m["trace_id"] != "msg-1" {
		t.Errorf("trace_id = %v, want the requested msg-1", m["trace_id"])
	}
	if supported, ok := m["supported"].(bool); !ok || supported {
		t.Errorf("supported = %#v, want false: no collector is wired", m["supported"])
	}

	status, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/observability/traces/stats/northbound", "", handleGetTraceStats, "node", "northbound")
	if status != http.StatusOK {
		t.Fatalf("GET trace stats -> %d", status)
	}
	m = decodeData(t, raw)
	nc := notCollectedList(t, m)
	for _, key := range []string{"requests", "errors", "avg_ms"} {
		if v, present := m[key]; !present || v != nil {
			t.Errorf("%s = %#v, want null - no request counter exists", key, v)
		}
		if !hasKey(nc, key) {
			t.Errorf("%s is null but not named in not_collected %v", key, nc)
		}
	}
}

// handleResolveAlertEvent used to answer resolved:true for an event no store
// held. A write that changes nothing must not report success: that is how an
// operator ends up believing an alarm was acknowledged.
func TestResolveAlertEventRefusesInsteadOfPretending(t *testing.T) {
	status, code, raw, msg := callHandler(t, http.MethodPost,
		"/api/v1/observability/alerts/events/disk_full/1700000000/resolve", "",
		handleResolveAlertEvent, "name", "disk_full", "ts", "1700000000")
	if status != http.StatusNotImplemented {
		t.Fatalf("resolve -> %d (code %s, msg %q), want 501", status, code, msg)
	}
	if code != "ERR_OBSERVABILITY_ALERT_UNSUPPORTED" {
		t.Errorf("error_code = %q, want ERR_OBSERVABILITY_ALERT_UNSUPPORTED", code)
	}
	if len(raw) > 0 && string(raw) != "null" {
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) == nil {
			if v, present := m["resolved"]; present {
				t.Errorf("a refused resolve still reports resolved=%v", v)
			}
		}
	}
	if msg == "" {
		t.Error("501 without a message leaves the caller no hint of the real endpoint")
	}
}

// The metrics panel must agree with the counters the drivers keep, in both
// directions: real totals when there is traffic, null for the HTTP request
// count no middleware takes.
func TestObservabilityMetricsReportsWhatIsCounted(t *testing.T) {
	// Two attempts on a device of its own, so the pooled totals have to move:
	// an assertion against an idle gateway would pass even if the handler summed
	// nothing at all.
	const id = "dev-obs-metrics"
	mgr := drivers.GetHealthStatsManager()
	mgr.ResetHealthStats(id)
	mgr.RecordReadSuccess(id, 20)
	mgr.RecordReadFailure(id)
	mgr.RecordWriteSuccess(id)
	defer mgr.ResetHealthStats(id)

	status, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/observability/metrics", "", handleGetObservabilityMetrics)
	if status != http.StatusOK {
		t.Fatalf("GET metrics -> %d", status)
	}
	m := decodeData(t, raw)
	if v, present := m["requests_total"]; !present || v != nil {
		t.Errorf("requests_total = %#v, want null: nothing counts HTTP requests here", v)
	}
	nc := notCollectedList(t, m)
	if !hasKey(nc, "requests_total") {
		t.Errorf("requests_total is null but not named in not_collected %v", nc)
	}
	for _, key := range []string{"read_attempts", "read_failures", "write_attempts", "write_failures", "reconnects"} {
		if _, ok := m[key].(float64); !ok {
			t.Errorf("%s = %#v, want a number from the driver counters", key, m[key])
		}
	}
	if got := m["read_attempts"].(float64); got < 2 {
		t.Errorf("read_attempts = %v, want at least the 2 attempts just recorded", got)
	}
	if got := m["read_failures"].(float64); got < 1 {
		t.Errorf("read_failures = %v, want at least the 1 failure just recorded", got)
	}
	if got := m["write_attempts"].(float64); got < 1 {
		t.Errorf("write_attempts = %v, want at least the 1 write just recorded", got)
	}
	if got := m["sample_count"].(float64); got < 1 {
		t.Errorf("sample_count = %v, want at least the latency sample from the successful read", got)
	}
	if got, ok := m["avg_latency_ms"].(float64); !ok || got <= 0 {
		t.Errorf("avg_latency_ms = %#v, want the mean of the samples taken", m["avg_latency_ms"])
	}
	// error_rate is either a real ratio or null; a 0 with no attempts is a lie.
	switch v := m["error_rate"].(type) {
	case nil:
		if !hasKey(nc, "error_rate") {
			t.Error("error_rate is null but not named in not_collected")
		}
	case float64:
		if v < 0 || v > 1 {
			t.Errorf("error_rate = %v, want a 0..1 ratio", v)
		}
		if attempts, ok := m["read_attempts"].(float64); ok {
			writes := m["write_attempts"].(float64)
			if attempts+writes == 0 {
				t.Errorf("error_rate = %v with zero attempts", v)
			}
		}
	default:
		t.Errorf("error_rate = %#v, want null or a number", v)
	}
}

// rowFor pulls one device row out of the decoded /metrics/summary payload.
func rowFor(t *testing.T, raw json.RawMessage, deviceID string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("summary is not an object: %s (%v)", raw, err)
	}
	rows, ok := m["drivers"].([]interface{})
	if !ok {
		t.Fatalf("drivers is %T, want a list", m["drivers"])
	}
	for _, r := range rows {
		row, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if row["device_id"] == deviceID {
			return row
		}
	}
	t.Fatalf("device %s missing from the driver rows: %s", deviceID, raw)
	return nil
}

// The driver table used to seed every unmeasured device with read_count 0,
// error_rate 0.0 and circuit_state "closed", so a device that had never
// completed a round trip was drawn as a healthy link.
func TestMetricsSummaryLeavesUnmeasuredDevicesUnreported(t *testing.T) {
	cont := withDeviceService(t)
	const id = "dev-obs-unmeasured"
	if err := cont.DeviceRepo.Create(&models.DeviceResponse{
		DeviceID: id, Name: "unmeasured", Protocol: "simulator", Status: "offline",
	}, "tester"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	drivers.GetHealthStatsManager().ResetHealthStats(id)

	status, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/metrics/summary", "", handleGetMetricsSummary)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics/summary -> %d, want 200", status)
	}
	row := rowFor(t, raw, id)
	for _, key := range []string{"read_count", "error_count", "error_rate", "avg_latency_ms", "circuit_state"} {
		if v, present := row[key]; !present || v != nil {
			t.Errorf("%s = %#v, want null for a device nothing has measured", key, v)
		}
	}
}

func TestMetricsSummaryReportsTheCountersTheDriverKept(t *testing.T) {
	cont := withDeviceService(t)
	const id = "dev-obs-measured"
	if err := cont.DeviceRepo.Create(&models.DeviceResponse{
		DeviceID: id, Name: "measured", Protocol: "simulator", Status: "online",
	}, "tester"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	mgr := drivers.GetHealthStatsManager()
	mgr.ResetHealthStats(id)
	mgr.RecordReadSuccess(id, 12)
	mgr.RecordReadFailure(id)
	defer mgr.ResetHealthStats(id)

	_, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/metrics/summary", "", handleGetMetricsSummary)
	row := rowFor(t, raw, id)
	if got := row["read_count"]; got != float64(2) {
		t.Errorf("read_count = %#v, want 2 attempts", got)
	}
	if got := row["error_count"]; got != float64(1) {
		t.Errorf("error_count = %#v, want 1 failure", got)
	}
	if got, ok := row["error_rate"].(float64); !ok || math.Abs(got-0.5) > 1e-9 {
		t.Errorf("error_rate = %#v, want 0.5", row["error_rate"])
	}
	// One 12 ms sample: the column promises an average, so it must not be the
	// scheduler's last-latency placeholder or a zero.
	if got, ok := row["avg_latency_ms"].(float64); !ok || math.Abs(got-12) > 1e-9 {
		t.Errorf("avg_latency_ms = %#v, want the measured mean of 12", row["avg_latency_ms"])
	}
}

// An idle gateway must not print error_rate 0.0: that is the difference between
// "nothing has gone wrong" and "nothing has happened", and the panel used to
// claim the first one on a fresh install.
func TestObservabilityMetricsPayloadNullsWhatAnIdleGatewayCannotDerive(t *testing.T) {
	got := observabilityMetricsPayload(nil, 0, 0, pipelineTotals{})
	nc := notCollectedList(t, got)
	for _, key := range []string{"requests_total", "error_rate", "avg_latency_ms"} {
		if v, present := got[key]; !present || v != nil {
			t.Errorf("%s = %#v on an idle gateway, want null", key, v)
		}
		if !hasKey(nc, key) {
			t.Errorf("%s is null but not named in not_collected %v", key, nc)
		}
	}
}

func TestObservabilityMetricsPayloadDerivesTheRealRatios(t *testing.T) {
	totals := pipelineTotals{readAttempts: 8, readFailures: 2, writeAttempts: 2, writeFailures: 1, reconnects: 1}
	got := observabilityMetricsPayload([]float64{10, 20}, 2, 100, totals)
	// Three failures out of ten attempts: reads and writes both count on both
	// sides of the ratio, so dropping either one changes the answer.
	if er, ok := got["error_rate"].(float64); !ok || math.Abs(er-0.3) > 1e-9 {
		t.Errorf("error_rate = %#v, want 0.3 (3 failures / 10 attempts)", got["error_rate"])
	}
	if avg, ok := got["avg_latency_ms"].(float64); !ok || avg != 15 {
		t.Errorf("avg_latency_ms = %#v, want the mean 15", got["avg_latency_ms"])
	}
	if nc := notCollectedList(t, got); !hasKey(nc, "requests_total") || hasKey(nc, "error_rate") || hasKey(nc, "avg_latency_ms") {
		t.Errorf("not_collected = %v, want only requests_total", nc)
	}
	if v := got["read_attempts"]; v != int64(8) {
		t.Errorf("read_attempts = %#v, want the 8 counted attempts", v)
	}
	if v := got["reconnects"]; v != int64(1) {
		t.Errorf("reconnects = %#v, want 1", v)
	}
}

// The pooled view is the only source these panels have, so it has to count the
// devices that measured something and skip the ones that did not: a device that
// only ever failed adds no latency sample, and counting it would report a
// "device reporting" figure the numbers do not back up.
func TestObservabilityLatencyPoolIgnoresDevicesWithoutSamples(t *testing.T) {
	mgr := drivers.GetHealthStatsManager()
	const withSample, withoutSample = "dev-obs-pool-a", "dev-obs-pool-b"
	mgr.ResetHealthStats(withSample)
	mgr.ResetHealthStats(withoutSample)
	defer mgr.ResetHealthStats(withSample)
	defer mgr.ResetHealthStats(withoutSample)

	mgr.RecordReadSuccess(withSample, 33)
	samples, devices, window := observabilityLatencySamples()
	found := false
	for _, v := range samples {
		if v == 33 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the 33 ms sample is missing from the pooled %v", samples)
	}
	if window <= 0 {
		t.Errorf("window_per_device = %d, want the rolling-window capacity", window)
	}

	mgr.RecordReadFailure(withoutSample)
	samples2, devices2, window2 := observabilityLatencySamples()
	if devices2 != devices {
		t.Errorf("devices_reporting went %d -> %d for a device with no latency samples", devices, devices2)
	}
	if len(samples2) != len(samples) {
		t.Errorf("sample_count went %d -> %d for a device with no latency samples", len(samples), len(samples2))
	}
	if window2 != window {
		t.Errorf("window_per_device went %d -> %d without any new sample", window, window2)
	}
}

// gopsutil reports 0 for an unreadable host as well as for an idle one, so the
// percentage has to disappear rather than render as an all-clear bar.
func TestPercentOrUnmeasuredHidesAFailedHostSnapshot(t *testing.T) {
	if got := percentOrUnmeasured(0, 0); got != nil {
		t.Errorf("percentOrUnmeasured(0, 0) = %v, want nil when the snapshot read nothing", got)
	}
	if got := percentOrUnmeasured(1<<30, 42.5); got != 42.5 {
		t.Errorf("percentOrUnmeasured(1GiB, 42.5) = %v, want the measured percentage", got)
	}
}

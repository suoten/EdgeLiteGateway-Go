package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// The device health panels read the counters the drivers write, and only the
// drivers write them. The two push-style protocols -- MQTT and HTTP webhook --
// recorded nothing at all, so a device that had been streaming for days still
// answered /devices/:id/ops with "no samples" and blanked its health tab. These
// tests pin what each mode is allowed to claim: a polled HTTP endpoint has a
// round trip to measure, a pushed one has a link state and no latency, and a
// driver that cannot reach its peer must record a failure.

func readOnePoint(t *testing.T, d Driver) ([]storage.PointData, error) {
	t.Helper()
	return d.ReadPoints(context.Background(), []models.PointDef{
		{Name: "temperature", DataType: "float32", Address: "temperature"},
	})
}

func statsFor(t *testing.T, deviceID string) HealthCounters {
	t.Helper()
	st := GetHealthStatsManager().GetHealthStats(deviceID)
	if st == nil {
		t.Fatalf("device %s recorded no health stats", deviceID)
	}
	return st.Counters()
}

func TestHTTPPollReadRecordsMeasuredLatency(t *testing.T) {
	RegisterAll()
	const deviceID = "http-poll-health"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"temperature":21.5}`))
	}))
	t.Cleanup(srv.Close)

	d, err := GetRegistry().CreateDriver("http_webhook", deviceID, map[string]interface{}{"poll_url": srv.URL})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	values, err := readOnePoint(t, d)
	if err != nil {
		t.Fatalf("ReadPoints returned an error: %v", err)
	}
	if len(values) != 1 || values[0].Value != 21.5 {
		t.Fatalf("ReadPoints = %+v, want the polled 21.5", values)
	}

	ctr := statsFor(t, deviceID)
	if ctr.TotalReads != 1 || ctr.FailedReads != 0 {
		t.Fatalf("counters = %d reads / %d failed, want 1 / 0", ctr.TotalReads, ctr.FailedReads)
	}
	if !ctr.HasLatencySample {
		t.Fatal("a polled read must carry a measured latency sample")
	}
	if ctr.AvgLatencyMs <= 0 {
		t.Errorf("avg_latency_ms = %v, want a measured duration above zero", ctr.AvgLatencyMs)
	}
}

func TestHTTPPollReadRecordsFailure(t *testing.T) {
	RegisterAll()
	const deviceID = "http-dead-health"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // the endpoint is gone before the first collection cycle

	d, err := GetRegistry().CreateDriver("http_webhook", deviceID, map[string]interface{}{"poll_url": url})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	if _, err := readOnePoint(t, d); err == nil {
		t.Fatal("ReadPoints succeeded against a closed endpoint")
	}

	ctr := statsFor(t, deviceID)
	if ctr.TotalReads != 1 || ctr.FailedReads != 1 {
		t.Fatalf("counters = %d reads / %d failed, want 1 / 1", ctr.TotalReads, ctr.FailedReads)
	}
	if ctr.HasLatencySample {
		t.Error("a failed read must not contribute a latency sample")
	}
}

func TestHTTPPushOnlyReadRecordsNoLatencySample(t *testing.T) {
	RegisterAll()
	const deviceID = "http-push-health"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	// No url: the device is fed by pushed webhooks, so a collection cycle has no
	// round trip to time.
	d, err := GetRegistry().CreateDriver("http_webhook", deviceID, map[string]interface{}{})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	if _, err := readOnePoint(t, d); err != nil {
		t.Fatalf("ReadPoints returned an error: %v", err)
	}

	ctr := statsFor(t, deviceID)
	if ctr.TotalReads != 1 {
		t.Fatalf("total_reads = %d, want 1", ctr.TotalReads)
	}
	if ctr.HasLatencySample || ctr.AvgLatencyMs != 0 {
		t.Errorf("push-only read reported latency %v (sample=%v), want none", ctr.AvgLatencyMs, ctr.HasLatencySample)
	}
}

func TestMQTTDisconnectedReadRecordsFailure(t *testing.T) {
	RegisterAll()
	const deviceID = "mqtt-down-health"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	// Never connected: there is no broker to dial, and the read must say so in
	// the counters the panels show instead of staying silent.
	d, err := GetRegistry().CreateDriver("mqtt_client", deviceID, map[string]interface{}{})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	if _, err := readOnePoint(t, d); err == nil {
		t.Fatal("ReadPoints succeeded while disconnected from the broker")
	}

	ctr := statsFor(t, deviceID)
	if ctr.TotalReads != 1 || ctr.FailedReads != 1 {
		t.Fatalf("counters = %d reads / %d failed, want 1 / 1", ctr.TotalReads, ctr.FailedReads)
	}
	if ctr.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", ctr.ConsecutiveFailures)
	}
}

func TestMQTTConnectFailureRecordsHealth(t *testing.T) {
	RegisterAll()
	const deviceID = "mqtt-connect-health"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	// The collect loop connects before it reads, so a broker that refuses the
	// socket never reaches ReadPoints. Connect is where that has to be recorded,
	// otherwise the device's health history stays empty while it is offline.
	d, err := GetRegistry().CreateDriver("mqtt_client", deviceID, map[string]interface{}{
		"broker": "127.0.0.1",
		"port":   1,
	})
	if err != nil {
		t.Fatalf("CreateDriver returned an error: %v", err)
	}
	if err := d.Connect(context.Background()); err == nil {
		t.Fatal("Connect succeeded against a closed port")
	}

	ctr := statsFor(t, deviceID)
	if ctr.TotalReads != 1 || ctr.FailedReads != 1 {
		t.Fatalf("counters = %d reads / %d failed, want 1 / 1", ctr.TotalReads, ctr.FailedReads)
	}
	if ctr.HasLatencySample {
		t.Error("a refused connection must not contribute a latency sample")
	}
}

func TestNotMeasuredLatencyStoresNoSample(t *testing.T) {
	const deviceID = "latency-sentinel"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	hsm.RecordReadSuccess(deviceID, LatencyNotMeasured)
	st := hsm.GetHealthStats(deviceID)
	if st == nil {
		t.Fatal("no health stats were created")
	}
	ctr := st.Counters()
	if ctr.TotalReads != 1 {
		t.Fatalf("total_reads = %d, want the operation to be counted", ctr.TotalReads)
	}
	if ctr.HasLatencySample {
		t.Errorf("latency = %v, want LatencyNotMeasured to store no sample", ctr.AvgLatencyMs)
	}
	if got := len(st.LatencySamples()); got != 0 {
		t.Errorf("latency_history has %d entries, want 0", got)
	}
}

// Nothing ever wrote LastOnlineAt, so last_online_at and the health list's
// last_check stayed null on devices that had been talking for days.
func TestRoundTripsStampLastTimeTheDeviceWasSeenOnline(t *testing.T) {
	const deviceID = "last-online"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	hsm.RecordReadSuccess(deviceID, 12.0)
	afterRead := statsFor(t, deviceID).LastOnlineAt
	if afterRead == nil {
		t.Fatal("a successful read left last_online_at null")
	}

	time.Sleep(2 * time.Millisecond)
	hsm.RecordWriteSuccess(deviceID)
	afterWrite := statsFor(t, deviceID).LastOnlineAt
	if afterWrite == nil || !afterWrite.After(*afterRead) {
		t.Errorf("last_online_at = %v, want a completed write to advance it past %v", afterWrite, afterRead)
	}

	got := hsm.GetObservabilityMetrics(deviceID)["last_online_at"]
	if got == nil {
		t.Fatal("GetObservabilityMetrics reported no last_online_at for an online device")
	}
	if _, ok := got.(string); !ok {
		t.Errorf("last_online_at = %v (%T), want an RFC3339 string", got, got)
	}
}

func TestFailedReadDoesNotClaimTheDeviceIsOnline(t *testing.T) {
	const deviceID = "last-online-fail"
	hsm := GetHealthStatsManager()
	t.Cleanup(func() { hsm.ResetHealthStats(deviceID) })

	hsm.RecordReadFailure(deviceID)
	ctr := statsFor(t, deviceID)
	if ctr.LastOnlineAt != nil {
		t.Errorf("last_online_at = %v, want null when no round trip ever succeeded", ctr.LastOnlineAt)
	}
	if ctr.LastOfflineAt == nil {
		t.Error("last_offline_at = nil, want the failure to open the offline window")
	}
}

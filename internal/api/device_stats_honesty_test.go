package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/constants"
	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// The statistics endpoints under /devices used to answer with fabricated numbers:
// online_rate 100.0 over zero samples, point health "success_rate 100", a probe
// that reported the stored status as if it had dialled the device. Each test here
// pins one of them to a measurement the gateway actually took -- and, just as
// importantly, to the "nothing was measured yet" answer when nothing was.

func callForDevice(t *testing.T, h func(echo.Context) error, method, deviceID, body string) (int, string, []byte) {
	t.Helper()
	c, rec := setupEcho(method, "/api/v1/devices/"+deviceID, body)
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.ErrorCode, rec.Body.Bytes()
}

func decodeObject(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", body, err)
	}
	return env.Data
}

func decodeArray(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var env struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", body, err)
	}
	return env.Data
}

func statsDevice(t *testing.T, cont *ServiceContainer, deviceID string) {
	t.Helper()
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: deviceID,
		Name:     deviceID,
		Protocol: "simulator",
		Config:   map[string]interface{}{},
		Points: []models.PointDef{
			{Name: "measured", DataType: "float32", Address: "0", AccessMode: "rw"},
			{Name: "unmeasured", DataType: "float32", Address: "1", AccessMode: "rw"},
		},
	})
}

func TestDeviceOpsReportsMeasuredCounters(t *testing.T) {
	cont := withDeviceService(t)
	statsDevice(t, cont, "ops-loud")
	statsDevice(t, cont, "ops-quiet")

	mgr := drivers.GetHealthStatsManager()
	if mgr == nil {
		t.Fatal("health stats manager is not available")
	}
	t.Cleanup(func() {
		mgr.ResetHealthStats("ops-loud")
		mgr.ResetHealthStats("ops-quiet")
	})
	mgr.RecordReadSuccess("ops-loud", 10)
	mgr.RecordReadSuccess("ops-loud", 30)
	mgr.RecordReadFailure("ops-loud")
	mgr.RecordWriteSuccess("ops-loud")

	_, _, body := callForDevice(t, handleGetDeviceOps, http.MethodGet, "ops-loud", "")
	data := decodeObject(t, body)
	if data["has_samples"] != true {
		t.Fatalf("has_samples = %v, want true after four operations", data["has_samples"])
	}
	if got := data["total_reads"]; got != float64(3) {
		t.Fatalf("total_reads = %v, want 3", got)
	}
	if got := data["failed_reads"]; got != float64(1) {
		t.Fatalf("failed_reads = %v, want 1", got)
	}
	if got := data["total_writes"]; got != float64(1) {
		t.Fatalf("total_writes = %v, want 1", got)
	}
	if got := data["online_rate"]; got != float64(75) {
		t.Fatalf("online_rate = %v, want 75 (3 of 4 operations succeeded)", got)
	}
	if got := data["avg_latency_ms"]; got.(float64) <= 0 {
		t.Fatalf("avg_latency_ms = %v, want a measured latency", got)
	}
	history, ok := data["latency_history"].([]interface{})
	if !ok || len(history) == 0 {
		t.Fatalf("latency_history = %v, want the samples that were measured", data["latency_history"])
	}

	// A device nobody polled must not report a success rate at all.
	_, _, quietBody := callForDevice(t, handleGetDeviceOps, http.MethodGet, "ops-quiet", "")
	quiet := decodeObject(t, quietBody)
	if quiet["has_samples"] != false {
		t.Fatalf("has_samples = %v for a device with no operations", quiet["has_samples"])
	}
	if quiet["online_rate"] != nil {
		t.Fatalf("online_rate = %v, want null so the UI cannot read it as 0%% or 100%%", quiet["online_rate"])
	}
	// The quality score starts at 100 inside the driver stats and the latency
	// average at 0, so both have to be withheld until they are measured: the UI
	// would otherwise plot an untested link as perfect.
	for _, key := range []string{"connection_quality_score", "avg_latency_ms", "p95_latency_ms"} {
		if quiet[key] != nil {
			t.Fatalf("%s = %v, want null with no samples", key, quiet[key])
		}
	}
	if h, ok := quiet["latency_history"].([]interface{}); !ok || len(h) != 0 {
		t.Fatalf("latency_history = %v, want an empty series", quiet["latency_history"])
	}
}

// TestDeviceOpsReportsFailedReadsHaveNoLatency covers the middle case: a device
// with operations but no successful one has counters and a 0% success rate, yet
// timing was never measured, so the latency fields stay null.
func TestDeviceOpsReportsFailedReadsHaveNoLatency(t *testing.T) {
	cont := withDeviceService(t)
	statsDevice(t, cont, "ops-all-fail")

	mgr := drivers.GetHealthStatsManager()
	t.Cleanup(func() { mgr.ResetHealthStats("ops-all-fail") })
	mgr.RecordReadFailure("ops-all-fail")
	mgr.RecordReadFailure("ops-all-fail")

	_, _, body := callForDevice(t, handleGetDeviceOps, http.MethodGet, "ops-all-fail", "")
	data := decodeObject(t, body)
	if data["has_samples"] != true {
		t.Fatalf("has_samples = %v, want true: two reads were attempted", data["has_samples"])
	}
	if data["total_reads"] != float64(2) || data["failed_reads"] != float64(2) {
		t.Fatalf("counters = %v/%v, want 2/2", data["total_reads"], data["failed_reads"])
	}
	if data["online_rate"] != float64(0) {
		t.Fatalf("online_rate = %v, want 0 for a link that only failed", data["online_rate"])
	}
	for _, key := range []string{"avg_latency_ms", "p95_latency_ms"} {
		if data[key] != nil {
			t.Fatalf("%s = %v, want null: a failed read records no timing", key, data[key])
		}
	}
}

func TestDeviceQualityStatsCountsMeasuredOperations(t *testing.T) {
	cont := withDeviceService(t)
	statsDevice(t, cont, "quality-loud")

	mgr := drivers.GetHealthStatsManager()
	t.Cleanup(func() { mgr.ResetHealthStats("quality-loud") })
	mgr.RecordReadSuccess("quality-loud", 5)
	mgr.RecordReadFailure("quality-loud")
	mgr.RecordReadFailure("quality-loud")

	c, rec := setupWithAdmin(http.MethodGet, "/api/v1/devices/device-quality-stats", "")
	if err := handleGetDeviceQualityStats(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	all := decodeObject(t, rec.Body.Bytes())
	row, ok := all["quality-loud"].(map[string]interface{})
	if !ok {
		t.Fatalf("quality stats missing the seeded device: %v", all)
	}
	if row["total_count"] != float64(3) || row["error_count"] != float64(2) || row["success_count"] != float64(1) {
		t.Fatalf("counters = %v, want total 3 / error 2 / success 1", row)
	}
	if rate, _ := row["error_rate"].(float64); rate < 0.66 || rate > 0.67 {
		t.Fatalf("error_rate = %v, want 2/3", row["error_rate"])
	}
	if row["has_samples"] != true {
		t.Fatalf("has_samples = %v, want true", row["has_samples"])
	}
}

func TestCollectStatsRefusesWithoutScheduler(t *testing.T) {
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	t.Cleanup(func() { SetContainer(prev) })

	code, errCode, body := callForDevice(t, handleGetCollectStats, http.MethodGet, "ignored", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with %q, want 503: an empty 200 reads as \"nothing is collecting\"", code, body)
	}
	// The derived-code convention (response.go) puts "CODE: reason" in error_code,
	// so the client translates the code and still learns which service is missing.
	if !strings.HasPrefix(errCode, "ERR_COMMON_SERVICE_NOT_READY") {
		t.Fatalf("error_code = %q, want ERR_COMMON_SERVICE_NOT_READY", errCode)
	}
	if !strings.Contains(string(body), "collect scheduler") {
		t.Fatalf("body = %s, want the reason the answer refused", body)
	}
}

func TestPointHealthSeparatesNeverCollected(t *testing.T) {
	cont := withDeviceService(t)
	cont.Cache = storage.NewCacheManager(100)
	statsDevice(t, cont, "health-points")

	collected := time.Date(2026, 9, 20, 8, 30, 0, 0, time.Local)
	cont.Cache.Push(storage.PointData{DeviceID: "health-points", PointName: "measured", Value: 21.5, Quality: "good", Timestamp: collected})
	// An older entry must lose to the newer one below.
	cont.Cache.Push(storage.PointData{DeviceID: "health-points", PointName: "measured", Value: 22, Quality: "uncertain", Timestamp: collected.Add(time.Minute)})
	cont.Cache.Push(storage.PointData{DeviceID: "other-device", PointName: "unmeasured", Value: 999, Quality: "good", Timestamp: collected})

	_, _, body := callForDevice(t, handleGetPointHealth, http.MethodGet, "health-points", "")
	rows := decodeArray(t, body)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per configured point: %v", len(rows), rows)
	}
	byName := map[string]map[string]interface{}{}
	for _, r := range rows {
		byName[r["point_name"].(string)] = r
	}
	got := byName["measured"]
	if got["current_quality"] != "uncertain" {
		t.Fatalf("measured quality = %v, want the newest cached sample", got["current_quality"])
	}
	if got["last_value"] != float64(22) {
		t.Fatalf("measured value = %v, want 22", got["last_value"])
	}
	if seen, _ := got["last_seen_at"].(string); !strings.HasPrefix(seen, "2026-09-20T08:31") {
		t.Fatalf("last_seen_at = %v, want the newer sample timestamp", got["last_seen_at"])
	}
	missing := byName["unmeasured"]
	if missing["current_quality"] != "never_collected" {
		t.Fatalf("unmeasured quality = %v, want never_collected, not a healthy default", missing["current_quality"])
	}
	if missing["last_value"] != nil || missing["last_seen_at"] != nil {
		t.Fatalf("unmeasured point carries invented data: %v", missing)
	}
}

func TestProbePrimaryLinkDialsThePeer(t *testing.T) {
	cont := withDeviceService(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a local listener: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "probe-open",
		Name:     "probe-open",
		Protocol: "modbus_tcp",
		Config:   map[string]interface{}{"host": listener.Addr().String()},
	})
	// Port 1 on 127.0.0.1 is not served: the answer must say unreachable.
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "probe-closed",
		Name:     "probe-closed",
		Protocol: "modbus_tcp",
		Config:   map[string]interface{}{"host": "127.0.0.1:1"},
	})
	// A serial protocol has no peer socket to dial at all.
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "probe-serial",
		Name:     "probe-serial",
		Protocol: "modbus_rtu",
		Config:   map[string]interface{}{"serial_port": "COM1"},
	})

	code, _, body := callForDevice(t, handleProbePrimaryLink, http.MethodPost, "probe-open", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	open := decodeObject(t, body)
	if open["reachable"] != true {
		t.Fatalf("reachable = %v for an open socket, want true: %s", open["reachable"], body)
	}
	if open["address"] != listener.Addr().String() {
		t.Fatalf("address = %v, want the socket that was dialled", open["address"])
	}

	code, _, body = callForDevice(t, handleProbePrimaryLink, http.MethodPost, "probe-closed", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with reachable=false: %s", code, body)
	}
	closed := decodeObject(t, body)
	if closed["reachable"] != false {
		t.Fatalf("reachable = %v for a closed port, want false", closed["reachable"])
	}
	if closed["error"] != "ERR_DEVICE_PROBE_UNREACHABLE" {
		t.Fatalf("error = %v, want ERR_DEVICE_PROBE_UNREACHABLE", closed["error"])
	}

	code, errCode, _ := callForDevice(t, handleProbePrimaryLink, http.MethodPost, "probe-serial", "")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for a protocol with no peer socket", code)
	}
	if errCode != "ERR_DEVICE_PROBE_UNSUPPORTED" {
		t.Fatalf("error_code = %q, want ERR_DEVICE_PROBE_UNSUPPORTED", errCode)
	}
}

// TestProbeUsesTheSeparatePortKey covers the shape the protocol forms actually
// write: host and port as two config entries. The probe resolved the host, saw no
// port embedded in it and dialled the protocol default, so a device configured for
// 127.0.0.1:1 was reported against 127.0.0.1:502.
func TestProbeUsesTheSeparatePortKey(t *testing.T) {
	cont := withDeviceService(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a local listener: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address %v: %v", listener.Addr(), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("listener port %q: %v", portText, err)
	}

	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "probe-split",
		Name:     "probe-split",
		Protocol: "modbus_tcp",
		// float64 because that is what a config arriving over JSON decodes to.
		Config: map[string]interface{}{"host": "127.0.0.1", "port": float64(port)},
	})
	// An out-of-range port must not silently fall back to 502 either: it is
	// refused before the dial so the operator sees the address that was tested.
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "probe-badport",
		Name:     "probe-badport",
		Protocol: "modbus_tcp",
		Config:   map[string]interface{}{"host": "127.0.0.1", "port": "70000"},
	})

	_, _, body := callForDevice(t, handleProbePrimaryLink, http.MethodPost, "probe-split", "")
	data := decodeObject(t, body)
	if data["reachable"] != true {
		t.Fatalf("reachable = %v, want the configured port to be dialled: %s", data["reachable"], body)
	}
	if data["address"] != listener.Addr().String() {
		t.Fatalf("address = %v, want %v", data["address"], listener.Addr())
	}

	code, _, body := callForDevice(t, handleProbePrimaryLink, http.MethodPost, "probe-badport", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d with %s, want 400 for a port that cannot be dialled", code, body)
	}
	if !strings.Contains(string(body), "ERR_DEVICE_PROBE_NO_ADDRESS") {
		t.Fatalf("body = %s, want ERR_DEVICE_PROBE_NO_ADDRESS", body)
	}
}

// TestHealthAllCarriesMeasuredCounters pins the feed the dashboard's degraded
// device panel reads. The endpoint used to return device_id/name/status/protocol
// only, so a client looking for a health score found none and every device read as
// healthy -- including the offline ones that were failing every collection cycle.
func TestHealthAllCarriesMeasuredCounters(t *testing.T) {
	cont := withDeviceService(t)
	statsDevice(t, cont, "all-loud")
	statsDevice(t, cont, "all-quiet")

	mgr := drivers.GetHealthStatsManager()
	t.Cleanup(func() {
		mgr.ResetHealthStats("all-loud")
		mgr.ResetHealthStats("all-quiet")
	})
	mgr.RecordReadSuccess("all-loud", 12)
	mgr.RecordReadFailure("all-loud")

	c, rec := setupWithAdmin(http.MethodGet, "/api/v1/devices/health/all", "")
	if err := handleListAllDeviceHealth(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	data := decodeObject(t, rec.Body.Bytes())
	rows, ok := data["items"].([]interface{})
	if !ok {
		t.Fatalf("items = %v, want the device rows the dashboard iterates", data["items"])
	}
	byID := map[string]map[string]interface{}{}
	for _, r := range rows {
		row, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		byID[row["device_id"].(string)] = row
	}

	loud := byID["all-loud"]
	if loud == nil {
		t.Fatalf("the seeded device is missing from health/all: %v", byID)
	}
	if loud["has_samples"] != true || loud["online_rate"] != float64(50) {
		t.Fatalf("loud row = %v, want measured 1/2 success", loud)
	}
	if score, ok := loud["connection_quality_score"].(float64); !ok || score >= 100 {
		t.Fatalf("connection_quality_score = %v, want the degraded score a failed read earns", loud["connection_quality_score"])
	}
	if loud["consecutive_failures"] != float64(1) || loud["total_reads"] != float64(2) || loud["failed_reads"] != float64(1) {
		t.Fatalf("counters = %v, want 1 consecutive failure over 2 reads with 1 failed", loud)
	}

	quiet := byID["all-quiet"]
	if quiet == nil {
		t.Fatalf("the unpolled device is missing from health/all: %v", byID)
	}
	if quiet["has_samples"] != false {
		t.Fatalf("has_samples = %v for a device with no operations", quiet["has_samples"])
	}
	// A never-polled device has no score: 0 would call it broken, 100 (the value
	// the driver stats start at) would call it healthy.
	for _, key := range []string{"online_rate", "connection_quality_score"} {
		if quiet[key] != nil {
			t.Fatalf("%s = %v, want null so the dashboard treats it as unknown", key, quiet[key])
		}
	}
}

// TestConnectionTestReportsTheDial pins the form's pre-check to what it actually
// measured. It used to answer success:true for every request, so an unreachable
// host looked tested; and it resolves the socket through the same resolver the
// probe uses, so the host:port it reports is the one the operator configured.
func TestConnectionTestReportsTheDial(t *testing.T) {
	withDeviceService(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a local listener: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address %v: %v", listener.Addr(), err)
	}
	livePort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("listener port %q: %v", portText, err)
	}

	for _, tc := range []struct {
		name     string
		protocol string
		config   map[string]interface{}
		want     map[string]interface{}
	}{
		{
			name:     "serial protocol has no socket",
			protocol: "modbus_rtu",
			config:   map[string]interface{}{"serial_port": "COM1"},
			want:     map[string]interface{}{"success": false, "supported": false, "message": "ERR_DEVICE_TEST_CONNECTION_UNSUPPORTED"},
		},
		{
			name:     "tcp protocol with no address",
			protocol: "modbus_tcp",
			config:   map[string]interface{}{"unit_id": 3},
			want:     map[string]interface{}{"success": false, "supported": true, "message": "ERR_DEVICE_TEST_NO_ADDRESS"},
		},
		{
			// The address is known but the port key cannot be a port; answering with
			// the default 502 here would test an endpoint nobody configured.
			name:     "unusable port is not replaced by the default",
			protocol: "modbus_tcp",
			config:   map[string]interface{}{"host": "10.0.0.5", "port": "not-a-port"},
			want:     map[string]interface{}{"success": false, "supported": true, "message": "ERR_DEVICE_TEST_NO_ADDRESS"},
		},
		{
			name:     "open socket",
			protocol: "modbus_tcp",
			config:   map[string]interface{}{"host": "127.0.0.1", "port": livePort},
			want:     map[string]interface{}{"success": true, "supported": true, "host": "127.0.0.1", "port": livePort},
		},
		{
			name:     "closed socket",
			protocol: "modbus_tcp",
			config:   map[string]interface{}{"host": "127.0.0.1", "port": 1},
			want:     map[string]interface{}{"success": false, "supported": true, "host": "127.0.0.1", "port": 1, "message": "ERR_DEVICE_TEST_CONNECTION_FAILED"},
		},
	} {
		body, err := json.Marshal(map[string]interface{}{"protocol": tc.protocol, "config": tc.config})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c, rec := setupEcho(http.MethodPost, "/api/v1/devices/test-connection", string(body))
		if err := handleTestConnection(c); err != nil {
			t.Fatalf("%s: handler returned an error: %v", tc.name, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 with the verdict in the payload: %s", tc.name, rec.Code, rec.Body)
		}
		data := decodeObject(t, rec.Body.Bytes())
		for key, want := range tc.want {
			switch expected := want.(type) {
			case int:
				if got, _ := data[key].(float64); int(got) != expected {
					t.Fatalf("%s: %v = %v, want %d (%s)", tc.name, key, data[key], expected, rec.Body)
				}
			default:
				if data[key] != expected {
					t.Fatalf("%s: %v = %v, want %v (%s)", tc.name, key, data[key], expected, rec.Body)
				}
			}
		}
		// A refused dial must not report a latency it never measured, and an
		// accepted one must report the time it did take.
		// No latency was measured for that device, so the key is absent: both 0 and
		// null mean "not measured", and the UI renders a missing key as '-'.
		latency, hasLatency := data["latency_ms"]
		if wantOK, _ := tc.want["success"].(bool); wantOK != hasLatency {
			t.Fatalf("%s: latency_ms present = %v (%v), want it only for a dial that happened: %s", tc.name, hasLatency, latency, rec.Body)
		}
	}
}

func TestDeviceTCPTestTargetResolution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		config   map[string]interface{}
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"host and port keys", "modbus_tcp", map[string]interface{}{"host": "10.0.0.5", "port": 1502}, "10.0.0.5", 1502, true},
		{"json number port", "modbus_tcp", map[string]interface{}{"host": "10.0.0.5", "port": float64(1502)}, "10.0.0.5", 1502, true},
		{"string port", "omron_fins", map[string]interface{}{"host": "10.0.0.5", "port": "9601"}, "10.0.0.5", 9601, true},
		{"embedded pair wins over the key", "modbus_tcp", map[string]interface{}{"host": "10.0.0.5:1502", "port": 502}, "10.0.0.5", 1502, true},
		{"url with port", "opc_ua", map[string]interface{}{"endpoint": "opc.tcp://10.0.0.5:4841/path"}, "10.0.0.5", 4841, true},
		// A URL that carries its own port must not be overridden by a stale "port"
		// key: the endpoint string is the more specific statement.
		{"url port wins", "opc_ua", map[string]interface{}{"endpoint": "opc.tcp://10.0.0.5:4841", "port": 4840}, "10.0.0.5", 4841, true},
		{"url without port takes the key", "opc_ua", map[string]interface{}{"endpoint": "opc.tcp://10.0.0.5", "port": 4845}, "10.0.0.5", 4845, true},
		{"no port anywhere uses the default", "siemens_s7", map[string]interface{}{"host": "10.0.0.9"}, "10.0.0.9", 102, true},
		// An unusable port is not the protocol default: the resolver reports no
		// address so the caller refuses rather than testing 502.
		{"out of range port is refused", "modbus_tcp", map[string]interface{}{"host": "10.0.0.5", "port": 70000}, "", 0, true},
		{"serial protocol unsupported", "modbus_rtu", map[string]interface{}{"serial_port": "COM1"}, "", 0, false},
		// A TCP protocol with nothing configured is "no address", not
		// "unsupported": the two answer differently in the form.
		{"no address at all", "modbus_tcp", map[string]interface{}{"unit_id": 3}, "", 0, true},
	} {
		host, port, ok := deviceTCPTestTarget(constants.NormalizeProtocol(tc.protocol), tc.config)
		if ok != tc.wantOK {
			t.Fatalf("%s: supported = %v, want %v", tc.name, ok, tc.wantOK)
		}
		if !ok {
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Fatalf("%s: resolved %s:%d, want %s:%d", tc.name, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

// TestDeleteDeviceClearsInfrastructureState pins the cleanup that deleting a
// device used to skip: the health stats manager, the circuit breaker and the
// reconnect manager are package-level and keyed by device ID, so a deleted
// device kept its counters (and an open breaker) forever. Deleting and then
// re-creating under the same ID -- an import, or a retry -- inherited the old
// device's failure history, which is how a device nobody had polled could
// report a quality score of 20.
func TestDeleteDeviceClearsInfrastructureState(t *testing.T) {
	const deviceID = "del-leak"
	cont := withDeviceService(t)
	statsDevice(t, cont, deviceID)

	mgr := drivers.GetHealthStatsManager()
	breaker := drivers.GetCircuitBreaker()
	reconnect := drivers.GetReconnectManager()
	t.Cleanup(func() {
		mgr.ResetHealthStats(deviceID)
		breaker.Reset(deviceID)
		reconnect.ResetReconnectState(deviceID)
	})

	mgr.RecordReadFailure(deviceID)
	mgr.RecordReadFailure(deviceID)
	if !mgr.SetConnectionState(deviceID, drivers.StateConnected, "test fixture") {
		t.Fatal("fixture could not set the connection state")
	}
	breaker.RecordFailure(deviceID, 5)
	// The backoff sleep is skipped by handing the manager an already cancelled
	// context, which it reports back as an error; the attempt counter it records
	// on the way out is the entry that leaks.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = reconnect.ReconnectWithBackoff(ctx, deviceID, func() (bool, error) { return false, nil })

	if mgr.GetHealthStats(deviceID) == nil {
		t.Fatal("fixture did not produce health stats")
	}
	if got := breaker.GetState(deviceID); got != drivers.CircuitOpen {
		t.Fatalf("breaker state = %v, want open before delete", got)
	}
	if got := reconnect.GetReconnectAttempts(deviceID); got == 0 {
		t.Fatal("reconnect attempts = 0, want a recorded attempt before delete")
	}

	if err := cont.DeviceService.Delete(deviceID); err != nil {
		t.Fatalf("Delete returned an error: %v", err)
	}

	if st := mgr.GetHealthStats(deviceID); st != nil {
		t.Errorf("health stats survived delete: %+v", st.Counters())
	}
	if got := mgr.GetConnectionStatus(deviceID).State; got != drivers.StateDisconnected {
		t.Errorf("connection state after delete = %v, want disconnected", got)
	}
	if got := breaker.GetState(deviceID); got != drivers.CircuitClosed {
		t.Errorf("breaker state after delete = %v, want closed", got)
	}
	if got := reconnect.GetReconnectAttempts(deviceID); got != 0 {
		t.Errorf("reconnect attempts after delete = %d, want 0", got)
	}
}

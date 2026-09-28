package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// /data-quality/devices/:id answered quality_score 100 for every device id,
// including ones that do not exist, and /data-quality/trend answered []. Both
// now read the time-series store, and the slow-query endpoint that has no data
// source says so instead of returning an empty 200.

func useQualityStore(t *testing.T) (*storage.TimeSeriesStorage, *storage.DeviceRepo) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(dir, "main.db")
	cfg.InfluxDB.SQLiteTSPath = filepath.Join(dir, "ts.db")

	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	tsDB, err := storage.NewTimeSeriesStorage(cfg)
	if err != nil {
		db.Close()
		t.Fatalf("NewTimeSeriesStorage: %v", err)
	}
	cont := GetContainer()
	prevDB, prevRepo, prevTS := cont.Database, cont.DeviceRepo, cont.TsStorage
	cont.Database, cont.DeviceRepo, cont.TsStorage = db, storage.NewDeviceRepo(db), tsDB
	t.Cleanup(func() {
		cont.Database, cont.DeviceRepo, cont.TsStorage = prevDB, prevRepo, prevTS
		_ = tsDB.Close()
		_ = db.Close()
	})
	return tsDB, cont.DeviceRepo
}

func addQualityDevice(t *testing.T, repo *storage.DeviceRepo, id string) {
	t.Helper()
	d := &models.DeviceResponse{DeviceID: id, Name: id + " name", Protocol: "simulator", Status: "online", CollectInterval: 5}
	if err := repo.Create(d, "test"); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
}

func callQualityData(t *testing.T, h func(echo.Context) error, path, query string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, path+query, "")
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

// callDeviceQuality keeps the query string out of the request body, which is
// where callWithParam would have put it.
func callDeviceQuality(t *testing.T, deviceID, query string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, "/api/v1/data-quality/devices/"+deviceID+query, "")
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := handleGetDeviceDataQuality(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

func callQualityList(t *testing.T, path string) (int, []map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, path, "")
	if err := handleGetDataQualityTrend(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable trend payload %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data
}

func TestDeviceDataQualityUnknownDeviceIs404(t *testing.T) {
	_, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")

	code, ec, data := callDeviceQuality(t, "dev-missing", "")
	if code != http.StatusNotFound || ec != "ERR_DEVICE_NOT_FOUND" {
		t.Fatalf("status = %d error_code = %q, want 404/ERR_DEVICE_NOT_FOUND", code, ec)
	}
	if data != nil {
		t.Fatalf("404 must not carry a score, got %#v", data)
	}
}

func TestDeviceDataQualityWithoutStoreIs503(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	cont := GetContainer()
	prev := cont.TsStorage
	cont.TsStorage = nil
	t.Cleanup(func() { cont.TsStorage = prev })
	_ = tsDB

	code, ec, _ := callDeviceQuality(t, "dev-dq", "")
	if code != http.StatusServiceUnavailable || ec != "ERR_TS_STORAGE_UNAVAILABLE" {
		t.Fatalf("status = %d error_code = %q, want 503/ERR_TS_STORAGE_UNAVAILABLE", code, ec)
	}
}

// A device that exists but has no samples in the window has not been measured;
// it must not inherit the 100 the old handler hardcoded.
func TestDeviceDataQualityNullScoreWithoutSamples(t *testing.T) {
	_, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")

	code, _, data := callDeviceQuality(t, "dev-dq", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a device with no samples", code)
	}
	if v, ok := data["quality_score"]; !ok || v != nil {
		t.Fatalf("quality_score = %#v (present=%v), want null", v, ok)
	}
	if v, ok := data["last_timestamp"]; !ok || v != nil {
		t.Fatalf("last_timestamp = %#v (present=%v), want null", v, ok)
	}
	if pts, ok := data["points"].([]interface{}); !ok || len(pts) != 0 {
		t.Fatalf("points = %#v, want an empty array", data["points"])
	}
}

func TestDeviceDataQualityReportsMeasuredScore(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	base := time.Now().Add(-20 * time.Minute)
	if err := tsDB.WritePoints([]storage.PointData{
		{DeviceID: "dev-dq", PointName: "temp", Value: 1, Quality: "good", Timestamp: base},
		{DeviceID: "dev-dq", PointName: "temp", Value: 2, Quality: "good", Timestamp: base.Add(time.Minute)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 3, Quality: "good", Timestamp: base.Add(2 * time.Minute)},
		{DeviceID: "dev-dq", PointName: "press", Value: 4, Quality: "bad", Timestamp: base.Add(3 * time.Minute)},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	code, _, data := callDeviceQuality(t, "dev-dq", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got, _ := data["quality_score"].(float64); got != 75 {
		t.Fatalf("quality_score = %#v, want 75 (3 good of 4 rows)", data["quality_score"])
	}
	if got, _ := data["total_rows"].(float64); got != 4 {
		t.Fatalf("total_rows = %#v, want 4", data["total_rows"])
	}
	if got, _ := data["point_count"].(float64); got != 2 {
		t.Fatalf("point_count = %#v, want 2", data["point_count"])
	}
	pts, _ := data["points"].([]interface{})
	if len(pts) != 2 {
		t.Fatalf("points = %#v, want one row per point", data["points"])
	}
	first, _ := pts[0].(map[string]interface{})
	// press sorts before temp
	if first["point_name"] != "press" {
		t.Fatalf("first point row = %#v, want press", first)
	}
	if q, _ := first["quality_pct"].(float64); q != 0 {
		t.Fatalf("press quality_pct = %#v, want 0 for an all-invalid point", first["quality_pct"])
	}
}

// The window parameter has to actually narrow the aggregate: an operator
// reading "last 1 hour" must not be shown 24 hours of history.
func TestDeviceDataQualityHonorsWindow(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	now := time.Now()
	if err := tsDB.WritePoints([]storage.PointData{
		{DeviceID: "dev-dq", PointName: "temp", Value: 1, Quality: "good", Timestamp: now.Add(-10 * time.Minute)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 2, Quality: "bad", Timestamp: now.Add(-48 * time.Hour)},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	_, _, wide := callDeviceQuality(t, "dev-dq", "?hours=72")
	if got, _ := wide["total_rows"].(float64); got != 2 {
		t.Fatalf("hours=72 total_rows = %#v, want both rows", wide["total_rows"])
	}
	if got, _ := wide["quality_score"].(float64); got != 50 {
		t.Fatalf("hours=72 quality_score = %#v, want 50", wide["quality_score"])
	}

	_, _, narrow := callDeviceQuality(t, "dev-dq", "?hours=1")
	if got, _ := narrow["total_rows"].(float64); got != 1 {
		t.Fatalf("hours=1 total_rows = %#v, want only the recent row", narrow["total_rows"])
	}
	if got, _ := narrow["quality_score"].(float64); got != 100 {
		t.Fatalf("hours=1 quality_score = %#v, want 100 for the one good row", narrow["quality_score"])
	}
}

func TestQualityTrendReturnsBuckets(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	latest := time.Now().Truncate(time.Hour)
	if err := tsDB.WritePoints([]storage.PointData{
		{DeviceID: "dev-dq", PointName: "temp", Value: 1, Quality: "good", Timestamp: latest.Add(-time.Hour)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 2, Quality: "bad", Timestamp: latest.Add(-time.Hour)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 3, Quality: "good", Timestamp: latest},
		// a different device must not show up when the trend is filtered
		{DeviceID: "dev-other", PointName: "temp", Value: 4, Quality: "bad", Timestamp: latest},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	code, rows := callQualityList(t, "/api/v1/data-quality/trend?device_id=dev-dq&hours=6")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per hour: %#v", len(rows), rows)
	}
	if rows[0]["total_rows"].(float64) != 2 || rows[0]["quality_pct"].(float64) != 50 {
		t.Fatalf("first bucket = %#v, want 2 rows at 50%%", rows[0])
	}
	if rows[1]["valid_rows"].(float64) != 1 {
		t.Fatalf("second bucket = %#v, want the device's own good row only", rows[1])
	}

	_, all := callQualityList(t, "/api/v1/data-quality/trend?hours=6")
	if len(all) != 2 || all[1]["total_rows"].(float64) != 2 {
		t.Fatalf("unfiltered buckets = %#v, want the latest hour to sum both devices", all)
	}
}

func TestQualityTrendWithoutStoreIs503(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	cont := GetContainer()
	prev := cont.TsStorage
	cont.TsStorage = nil
	t.Cleanup(func() { cont.TsStorage = prev })
	_ = tsDB

	code, rows := callQualityList(t, "/api/v1/data-quality/trend")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with %#v, want 503 instead of an empty trend", code, rows)
	}
}

func TestSlowQueriesUnsupportedIs501(t *testing.T) {
	code, ec, data := callQualityData(t, handleGetSlowQueries, "/api/v1/db-monitor/slow-queries", "")
	if code != http.StatusNotImplemented || ec != "ERR_DB_SLOW_QUERIES_UNSUPPORTED" {
		t.Fatalf("status = %d error_code = %q, want 501/ERR_DB_SLOW_QUERIES_UNSUPPORTED", code, ec)
	}
	if data != nil {
		t.Fatalf("501 must not carry an empty query list, got %#v", data)
	}
}

// callReport reads the /data-quality/report envelope, whose data is an object.
func callReport(t *testing.T, query string) (int, string, map[string]interface{}) {
	t.Helper()
	return callQualityData(t, handleGetDataQualityReport, "/api/v1/data-quality/report", query)
}

func reportSummary(t *testing.T, data map[string]interface{}) map[string]interface{} {
	t.Helper()
	s, ok := data["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("report has no summary object: %#v", data)
	}
	return s
}

func TestQualityReportAggregatesWhatWasMeasured(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	base := time.Now().Add(-20 * time.Minute)
	if err := tsDB.WritePoints([]storage.PointData{
		{DeviceID: "dev-dq", PointName: "temp", Value: 1, Quality: "good", Timestamp: base},
		{DeviceID: "dev-dq", PointName: "temp", Value: 2, Quality: "good", Timestamp: base.Add(time.Minute)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 3, Quality: "good", Timestamp: base.Add(2 * time.Minute)},
		{DeviceID: "dev-dq", PointName: "press", Value: 4, Quality: "bad", Timestamp: base.Add(3 * time.Minute)},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}

	code, ec, data := callReport(t, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d error_code = %q, want 200", code, ec)
	}
	sum := reportSummary(t, data)
	if sum["total_rows"] != float64(4) || sum["valid_rows"] != float64(3) || sum["invalid_rows"] != float64(1) {
		t.Fatalf("summary = %#v, want the 4 rows (3 good, 1 bad) that were written", sum)
	}
	if sum["quality_pct"] != float64(75) {
		t.Fatalf("quality_pct = %#v, want 75", sum["quality_pct"])
	}
	if sum["measured"] != true {
		t.Fatalf("measured = %#v with four samples in the window", sum["measured"])
	}
	if sum["device_count"] != float64(1) {
		t.Fatalf("device_count = %#v, want the one device that has samples", sum["device_count"])
	}
	raw, _ := json.Marshal(data["by_device"])
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("by_device = %s, want one row per device with samples", raw)
	}
	if rows[0]["device_id"] != "dev-dq" || rows[0]["quality_pct"] != float64(75) {
		t.Fatalf("by_device row = %#v, want dev-dq at 75%%", rows[0])
	}
	if gen, _ := data["generated_at"].(string); gen == "" {
		t.Fatal("generated_at is empty; the report has to say when it was computed")
	}
}

// An empty window used to be indistinguishable from a flawless one because the
// handler returned a literal empty summary.
func TestQualityReportWithoutSamplesSaysNothingWasMeasured(t *testing.T) {
	useQualityStore(t)
	code, _, data := callReport(t, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an explicit unmeasured summary", code)
	}
	sum := reportSummary(t, data)
	if sum["measured"] != false {
		t.Fatalf("measured = %#v with no samples at all", sum["measured"])
	}
	if v, ok := sum["quality_pct"]; !ok || v != nil {
		t.Fatalf("quality_pct = %#v (present=%v), want null: no samples is not a score", v, ok)
	}
	if rows, ok := data["by_device"].([]interface{}); !ok || len(rows) != 0 {
		t.Fatalf("by_device = %#v, want an empty list", data["by_device"])
	}
}

func TestQualityReportHonorsTheWindow(t *testing.T) {
	tsDB, repo := useQualityStore(t)
	addQualityDevice(t, repo, "dev-dq")
	now := time.Now()
	if err := tsDB.WritePoints([]storage.PointData{
		{DeviceID: "dev-dq", PointName: "temp", Value: 1, Quality: "good", Timestamp: now.Add(-10 * time.Minute)},
		{DeviceID: "dev-dq", PointName: "temp", Value: 2, Quality: "bad", Timestamp: now.Add(-48 * time.Hour)},
	}); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}
	_, _, wide := callReport(t, "?hours=72")
	if got := reportSummary(t, wide)["total_rows"]; got != float64(2) {
		t.Fatalf("hours=72 total_rows = %#v, want both rows", got)
	}
	_, _, narrow := callReport(t, "?hours=1")
	narrowSum := reportSummary(t, narrow)
	if narrowSum["total_rows"] != float64(1) {
		t.Fatalf("hours=1 total_rows = %#v, want only the recent row", narrowSum["total_rows"])
	}
	if narrowSum["quality_pct"] != float64(100) {
		t.Fatalf("hours=1 quality_pct = %#v, want 100 for the single good row in the window", narrowSum["quality_pct"])
	}
	if narrowSum["window_hours"] != float64(1) {
		t.Fatalf("window_hours = %#v, want the window the caller asked for", narrowSum["window_hours"])
	}
}

func TestQualityReportWithoutStoreIs503(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	cont := GetContainer()
	prev := cont.TsStorage
	cont.TsStorage = nil
	t.Cleanup(func() { cont.TsStorage = prev })
	_ = tsDB

	code, ec, data := callReport(t, "")
	if code != http.StatusServiceUnavailable || ec != "ERR_COMMON_SERVICE_NOT_READY" {
		t.Fatalf("status = %d error_code = %q, want 503/ERR_COMMON_SERVICE_NOT_READY instead of an empty report", code, ec)
	}
	if data != nil {
		t.Fatalf("503 must not carry a summary, got %#v", data)
	}
}

func TestQualityResetRefusesInsteadOfClaimingSuccess(t *testing.T) {
	c, rec := setupEcho(http.MethodPost, "/api/v1/data-quality/reset", "")
	if err := handleResetDataQuality(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Code      int                    `json:"code"`
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope: %v", err)
	}
	if rec.Code != http.StatusNotImplemented || env.ErrorCode != "ERR_DATA_QUALITY_RESET_UNSUPPORTED" {
		t.Fatalf("status = %d error_code = %q, want 501/ERR_DATA_QUALITY_RESET_UNSUPPORTED", rec.Code, env.ErrorCode)
	}
	if env.Data["reset"] != nil {
		t.Fatalf("payload reports reset = %#v for work that did not happen", env.Data["reset"])
	}
}

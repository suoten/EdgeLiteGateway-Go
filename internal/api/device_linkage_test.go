package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// /linkage used to be a table with a CRUD face and no runtime: nothing read
// device_linkages, so trigger_count stayed 0 forever while the UI displayed it.
// These tests pin the connected version: rules are validated against real devices
// and points, loaded into the live evaluator, and a write that lands bumps the
// persisted counter.

func useLinkageStore(t *testing.T) (*ServiceContainer, *storage.DeviceRepo) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(dir, "main.db")
	cfg.InfluxDB.SQLiteTSPath = filepath.Join(dir, "ts.db")

	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	cont := GetContainer()
	prevDB, prevDev, prevAlarm, prevLink := cont.Database, cont.DeviceRepo, cont.AlarmRepo, cont.LinkageEvaluator
	cont.Database = db
	cont.DeviceRepo = storage.NewDeviceRepo(db)
	cont.AlarmRepo = storage.NewAlarmRepo(db)
	cont.LinkageEvaluator = engine.NewLinkageEvaluator()
	t.Cleanup(func() {
		cont.Database, cont.DeviceRepo, cont.AlarmRepo, cont.LinkageEvaluator = prevDB, prevDev, prevAlarm, prevLink
		_ = db.Close()
	})
	return cont, cont.DeviceRepo
}

func callLinkage(t *testing.T, h func(echo.Context) error, method, path, body string, params []string, values []string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if len(params) > 0 {
		c.SetParamNames(params...)
		c.SetParamValues(values...)
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Code      int                    `json:"code"`
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

const linkageBody = `{"name":"hot fan","source_device_id":"src","source_point":"temp",` +
	`"condition_op":">","threshold":30,"target_device_id":"dst","target_point":"fan","target_value":"true"}`

func TestCreateDeviceLinkageValidatesAgainstRealDevices(t *testing.T) {
	cont, repo := useLinkageStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", DataType: "float", AccessMode: "ReadOnly"})
	addBridgeDevice(t, repo, "dst",
		models.PointDef{Name: "fan", DataType: "bool", AccessMode: "ReadWrite"},
		models.PointDef{Name: "locked", DataType: "bool", AccessMode: "ReadOnly"})

	cases := []struct {
		name, body, wantCode string
	}{
		{"unknown source device", `{"source_device_id":"ghost","source_point":"temp","condition_op":">","target_device_id":"dst","target_point":"fan","target_value":"1"}`, "ERR_LINKAGE_SOURCE_DEVICE_UNKNOWN"},
		{"unknown target device", `{"source_device_id":"src","source_point":"temp","condition_op":">","target_device_id":"ghost","target_point":"fan","target_value":"1"}`, "ERR_LINKAGE_TARGET_DEVICE_UNKNOWN"},
		{"unknown source point", `{"source_device_id":"src","source_point":"ghost","condition_op":">","target_device_id":"dst","target_point":"fan","target_value":"1"}`, "ERR_LINKAGE_SOURCE_POINT_UNKNOWN"},
		{"unknown target point", `{"source_device_id":"src","source_point":"temp","condition_op":">","target_device_id":"dst","target_point":"ghost","target_value":"1"}`, "ERR_LINKAGE_TARGET_POINT_UNKNOWN"},
		{"read-only target", `{"source_device_id":"src","source_point":"temp","condition_op":">","target_device_id":"dst","target_point":"locked","target_value":"1"}`, "ERR_LINKAGE_TARGET_READ_ONLY"},
		{"unsupported operator", `{"source_device_id":"src","source_point":"temp","condition_op":"~","target_device_id":"dst","target_point":"fan","target_value":"1"}`, "ERR_LINKAGE_CONDITION_UNSUPPORTED"},
		{"empty target value", `{"source_device_id":"src","source_point":"temp","condition_op":">","target_device_id":"dst","target_point":"fan","target_value":""}`, "ERR_LINKAGE_TARGET_VALUE_REQUIRED"},
	}
	for _, tc := range cases {
		code, errCode, _ := callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", tc.body, nil, nil)
		if code != http.StatusBadRequest || !strings.HasPrefix(errCode, tc.wantCode) {
			t.Fatalf("%s: got %d %s, want 400 %s", tc.name, code, errCode, tc.wantCode)
		}
	}
	if n := cont.LinkageEvaluator.RuleCount(); n != 0 {
		t.Fatalf("rejected rules reached the evaluator: %d", n)
	}

	code, _, data := callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", linkageBody, nil, nil)
	if code != http.StatusCreated {
		t.Fatalf("valid rule rejected: %d %#v", code, data)
	}
	if cont.LinkageEvaluator.RuleCount() != 1 {
		t.Fatalf("created rule not loaded into the evaluator: %d", cont.LinkageEvaluator.RuleCount())
	}
}

func TestCreateDeviceLinkageCoercesTargetValueByDataType(t *testing.T) {
	cont, repo := useLinkageStore(t)
	addBridgeDevice(t, repo, "src",
		models.PointDef{Name: "t0", DataType: "float"},
		models.PointDef{Name: "t1", DataType: "float"},
		models.PointDef{Name: "t2", DataType: "float"})
	addBridgeDevice(t, repo, "dst",
		models.PointDef{Name: "fan", DataType: "bool", AccessMode: "ReadWrite"},
		models.PointDef{Name: "setpoint", DataType: "float", AccessMode: "ReadWrite"},
		models.PointDef{Name: "mode", DataType: "int", AccessMode: "ReadWrite"})

	cases := []struct {
		target, value string
		want          interface{}
	}{
		{"fan", "true", true},
		{"setpoint", "22.5", 22.5},
		{"mode", "3", int64(3)},
	}
	for i, tc := range cases {
		// Each rule watches its own source point so exactly one rule can fire.
		body := `{"name":"c` + strconv.Itoa(i) + `","source_device_id":"src","source_point":"t` + strconv.Itoa(i) + `",` +
			`"condition_op":">","threshold":-1,"target_device_id":"dst","target_point":"` + tc.target +
			`","target_value":"` + tc.value + `"}`
		code, errCode, _ := callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", body, nil, nil)
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", tc.target, code, errCode)
		}
		var fired int
		var got interface{}
		cont.LinkageEvaluator.SetWriteSink(func(_ context.Context, _, _ string, value interface{}) error {
			fired++
			got = value
			return nil
		})
		// threshold -1 with a 0 sample is a crossing on the first evaluation.
		cont.LinkageEvaluator.Evaluate(context.Background(), "src", "t"+strconv.Itoa(i), 0.0)
		if fired != 1 {
			t.Fatalf("target %s: %d writes, want exactly 1", tc.target, fired)
		}
		if got != tc.want {
			t.Fatalf("target %s got %#v (%T), want %#v (%T)", tc.target, got, got, tc.want, tc.want)
		}
	}
}

func TestDeviceLinkageTriggerPersistsCounter(t *testing.T) {
	cont, repo := useLinkageStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", DataType: "float"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "fan", DataType: "bool", AccessMode: "ReadWrite"})
	_, _, data := callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", linkageBody, nil, nil)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("no rule id in %#v", data)
	}

	var written []string
	cont.LinkageEvaluator.SetWriteSink(func(_ context.Context, deviceID, point string, value interface{}) error {
		written = append(written, deviceID+"."+point+"="+fmt.Sprintf("%v", value))
		return nil
	})
	cont.LinkageEvaluator.SetTriggerRecorder(func(ruleID string, at time.Time) error {
		return cont.AlarmRepo.RecordLinkageTrigger(ruleID, at)
	})

	// Below the threshold, then above: one crossing, one write, one persisted trigger.
	cont.LinkageEvaluator.Evaluate(context.Background(), "src", "temp", 10)
	cont.LinkageEvaluator.Evaluate(context.Background(), "src", "temp", 50)
	cont.LinkageEvaluator.Evaluate(context.Background(), "src", "temp", 60)
	if len(written) != 1 || written[0] != "dst.fan=true" {
		t.Fatalf("sink saw %v, want one dst.fan=true", written)
	}

	records, err := cont.AlarmRepo.ListDeviceLinkages()
	if err != nil || len(records) != 1 {
		t.Fatalf("ListDeviceLinkages: %#v err %v", records, err)
	}
	if records[0].TriggerCount != 1 || records[0].LastTriggeredAt == "" {
		t.Fatalf("trigger counter not persisted: count=%d at=%q", records[0].TriggerCount, records[0].LastTriggeredAt)
	}

	// The list handler merges the live stats in so the UI can explain a rule that
	// has not fired yet.
	code, _, listData := callLinkage(t, handleListDeviceLinkages, http.MethodGet, "/api/v1/linkage", "", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	items, _ := listData["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("list items = %#v", listData)
	}
	row := items[0].(map[string]interface{})
	if row["trigger_count"] != float64(1) {
		t.Fatalf("list trigger_count = %v, want 1", row["trigger_count"])
	}
	rt, _ := row["runtime"].(map[string]interface{})
	if rt == nil || rt["live_transfers"] != float64(1) || rt["satisfied"] != true {
		t.Fatalf("list runtime stats = %#v", row["runtime"])
	}
}

func TestDeviceLinkageStatusIsHonest(t *testing.T) {
	_, repo := useLinkageStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", DataType: "float"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "fan", DataType: "bool", AccessMode: "ReadWrite"})
	callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", linkageBody, nil, nil)

	// Nothing was wired: no sink, no event feed, so the status must say so.
	code, _, data := callLinkage(t, handleGetLinkageStatus, http.MethodGet, "/api/v1/linkage/status", "", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if data["running"] != false || data["sink_wired"] != false || data["feed_configured"] != false {
		t.Fatalf("unwired evaluator reported itself live: %#v", data)
	}
	if data["rules"] != float64(1) || data["persistence"] != true {
		t.Fatalf("status counters = %#v", data)
	}
}

func TestDeviceLinkageRuleStatsNotFound(t *testing.T) {
	useLinkageStore(t)
	code, errCode, _ := callLinkage(t, handleGetLinkageRuleStats, http.MethodGet, "/api/v1/linkage/ghost/stats", "",
		[]string{"id"}, []string{"ghost"})
	if code != http.StatusNotFound || errCode != "ERR_LINKAGE_RULE_NOT_FOUND" {
		t.Fatalf("unknown rule stats = %d %s", code, errCode)
	}
}

func TestDeleteDeviceLinkageRemovesFromEvaluator(t *testing.T) {
	cont, repo := useLinkageStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", DataType: "float"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "fan", DataType: "bool", AccessMode: "ReadWrite"})
	_, _, data := callLinkage(t, handleCreateDeviceLinkage, http.MethodPost, "/api/v1/linkage", linkageBody, nil, nil)
	id, _ := data["id"].(string)

	code, _, _ := callLinkage(t, handleDeleteDeviceLinkage, http.MethodDelete, "/api/v1/linkage/"+id, "", []string{"id"}, []string{id})
	if code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if n := cont.LinkageEvaluator.RuleCount(); n != 0 {
		t.Fatalf("deleted rule still loaded: %d", n)
	}
	code, errCode, _ := callLinkage(t, handleDeleteDeviceLinkage, http.MethodDelete, "/api/v1/linkage/"+id, "", []string{"id"}, []string{id})
	if code != http.StatusNotFound || !strings.Contains(errCode, "ERR_") {
		t.Fatalf("re-delete = %d %s, want 404 with a code", code, errCode)
	}
}

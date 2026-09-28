package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// /bridge/status answered {enabled:false, active_bridges:0}, /bridge/bridges
// answered [], POST echoed the request body and DELETE logged a deletion that
// never happened. These tests pin the replacement: definitions are validated,
// persisted, registered with the live manager and removed again.

func useBridgeStore(t *testing.T) (*ServiceContainer, *storage.DeviceRepo) {
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
	prevDB, prevRepo, prevBridge := cont.Database, cont.DeviceRepo, cont.ProtocolBridge
	manager := engine.NewProtocolBridgeManager()
	cont.Database, cont.DeviceRepo, cont.ProtocolBridge = db, storage.NewDeviceRepo(db), manager
	t.Cleanup(func() {
		cont.Database, cont.DeviceRepo, cont.ProtocolBridge = prevDB, prevRepo, prevBridge
		_ = db.Close()
	})
	return cont, cont.DeviceRepo
}

func addBridgeDevice(t *testing.T, repo *storage.DeviceRepo, id string, points ...models.PointDef) {
	t.Helper()
	d := &models.DeviceResponse{DeviceID: id, Name: id + " name", Protocol: "simulator", Status: "online", CollectInterval: 5, Points: points}
	if err := repo.Create(d, "test"); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
}

func callBridge(t *testing.T, h func(echo.Context) error, method, path, body string, params []string, values []string) (int, string, map[string]interface{}) {
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

const bridgeBody = `{"name":"line-1","source_device":"src","target_device":"dst","enabled":true,` +
	`"rules":[{"source_point":"temp","target_point":"setpoint","conversion_type":"linear","scale":2,"offset":1,"enabled":true}]}`

func TestCreateProtocolBridgePersistsAndRegisters(t *testing.T) {
	cont, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", AccessMode: "ReadWrite"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"})

	code, _, data := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", bridgeBody, nil, nil)
	if code != http.StatusCreated {
		t.Fatalf("create returned %d, want 201", code)
	}
	id, _ := data["id"].(string)
	if !strings.HasPrefix(id, "pb-") {
		t.Fatalf("created bridge has no server-assigned id: %#v", data)
	}

	// The definition must survive into the store, not just the HTTP response.
	records, err := loadBridgeRecords(cont)
	if err != nil || len(records) != 1 {
		t.Fatalf("persisted records = %#v err %v, want one bridge", records, err)
	}
	if records[0].Rules[0].ConversionType != "linear" || records[0].Rules[0].Scale != 2 {
		t.Fatalf("rule was not persisted verbatim: %#v", records[0].Rules[0])
	}

	live := cont.ProtocolBridge.GetBridge(id)
	if live == nil {
		t.Fatalf("created bridge is not registered with the manager")
	}
	if !live.Enabled() || live.TargetDevice() != "dst" || len(live.Info()["rules"].([]*engine.MappingRule)) != 1 {
		t.Fatalf("live bridge does not match the request: %#v", live.Info())
	}

	_, _, listed := callBridge(t, handleListProtocolBridges, http.MethodGet, "/api/v1/bridge/bridges", "", nil, nil)
	if bridges, _ := listed["bridges"].([]interface{}); len(bridges) != 1 {
		t.Fatalf("list returned %#v, want one bridge", listed)
	}
}

func TestProtocolBridgeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing name", `{"name":"  ","source_device":"src","target_device":"dst","rules":[{"source_point":"temp","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_NAME_REQUIRED"},
		{"same device", `{"name":"x","source_device":"src","target_device":"src","rules":[{"source_point":"temp","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_SAME_DEVICE"},
		{"unknown source", `{"name":"x","source_device":"nope","target_device":"dst","rules":[{"source_point":"temp","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_SOURCE_DEVICE_UNKNOWN"},
		{"no rules", `{"name":"x","source_device":"src","target_device":"dst","rules":[]}`, "ERR_PROTOCOL_BRIDGE_RULES_REQUIRED"},
		{"empty point", `{"name":"x","source_device":"src","target_device":"dst","rules":[{"source_point":"","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_RULE_POINT_REQUIRED"},
		{"unknown conversion", `{"name":"x","source_device":"src","target_device":"dst","rules":[{"source_point":"temp","target_point":"setpoint","conversion_type":"quadratic"}]}`, "ERR_PROTOCOL_BRIDGE_CONVERSION_UNSUPPORTED"},
		{"unknown source point", `{"name":"x","source_device":"src","target_device":"dst","rules":[{"source_point":"ghost","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_SOURCE_POINT_UNKNOWN"},
		{"read-only target", `{"name":"x","source_device":"src","target_device":"dst","rules":[{"source_point":"temp","target_point":"readonly_point"}]}`, "ERR_PROTOCOL_BRIDGE_TARGET_READ_ONLY"},
		{"duplicate rule", `{"name":"x","source_device":"src","target_device":"dst","rules":[{"source_point":"temp","target_point":"setpoint"},{"source_point":"temp","target_point":"setpoint"}]}`, "ERR_PROTOCOL_BRIDGE_RULE_DUPLICATE"},
	}
	_, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", AccessMode: "ReadWrite"})
	addBridgeDevice(t, repo, "dst",
		models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"},
		models.PointDef{Name: "readonly_point", AccessMode: "Read"})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, errCode, _ := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", tc.body, nil, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", code, tc.body)
			}
			if !strings.HasPrefix(errCode, tc.want) {
				t.Fatalf("error_code = %q, want %q", errCode, tc.want)
			}
		})
	}
}

func TestCreateProtocolBridgeRejectsCycle(t *testing.T) {
	_, repo := useBridgeStore(t)
	for _, id := range []string{"a", "b"} {
		addBridgeDevice(t, repo, id, models.PointDef{Name: "p", AccessMode: "ReadWrite"})
	}
	first := `{"name":"a-to-b","source_device":"a","target_device":"b","enabled":true,"rules":[{"source_point":"p","target_point":"p"}]}`
	if code, errCode, _ := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", first, nil, nil); code != http.StatusCreated {
		t.Fatalf("first bridge rejected: %d %s", code, errCode)
	}
	second := `{"name":"b-to-a","source_device":"b","target_device":"a","enabled":true,"rules":[{"source_point":"p","target_point":"p"}]}`
	code, errCode, _ := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", second, nil, nil)
	if code != http.StatusConflict || errCode != "ERR_PROTOCOL_BRIDGE_CYCLE" {
		t.Fatalf("reverse bridge accepted: %d %s, want 409 ERR_PROTOCOL_BRIDGE_CYCLE", code, errCode)
	}
}

func TestDeleteProtocolBridgeRemovesIt(t *testing.T) {
	cont, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"})
	_, _, created := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", bridgeBody, nil, nil)
	id, _ := created["id"].(string)

	code, _, data := callBridge(t, handleDeleteProtocolBridge, http.MethodDelete, "/api/v1/bridge/bridges/"+id, "",
		[]string{"id"}, []string{id})
	if code != http.StatusOK {
		t.Fatalf("delete returned %d, want 200", code)
	}
	if removed, _ := data["removed_from_manager"].(bool); !removed {
		t.Fatalf("manager still holds the bridge: %#v", data)
	}
	if records, err := loadBridgeRecords(cont); err != nil || len(records) != 0 {
		t.Fatalf("bridge still persisted: %#v err %v", records, err)
	}

	code, errCode, _ := callBridge(t, handleDeleteProtocolBridge, http.MethodDelete, "/api/v1/bridge/bridges/nope", "",
		[]string{"id"}, []string{"nope"})
	if code != http.StatusNotFound || errCode != "ERR_PROTOCOL_BRIDGE_NOT_FOUND" {
		t.Fatalf("unknown bridge delete = %d %s, want 404 ERR_PROTOCOL_BRIDGE_NOT_FOUND", code, errCode)
	}
}

func TestSetProtocolBridgeEnabledPersists(t *testing.T) {
	cont, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"})
	_, _, created := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", bridgeBody, nil, nil)
	id, _ := created["id"].(string)

	callBridge(t, handleSetProtocolBridgeEnabled(false), http.MethodPost, "/api/v1/bridge/bridges/"+id+"/disable", "",
		[]string{"id"}, []string{id})
	records, err := loadBridgeRecords(cont)
	if err != nil || len(records) != 1 || records[0].Enabled {
		t.Fatalf("disable did not persist: %#v err %v", records, err)
	}
	if live := cont.ProtocolBridge.GetBridge(id); live == nil || live.Enabled() {
		t.Fatalf("live bridge still enabled: %#v", live)
	}
}

// Editing or pausing a bridge rebuilds it inside the manager, so the counters it
// already earned must be carried over; otherwise the operator loses the evidence
// that the link ever worked.
func TestProtocolBridgeStatsSurviveEdits(t *testing.T) {
	cont, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp", AccessMode: "ReadWrite"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"})
	if err := cont.ProtocolBridge.Start(context.Background()); err != nil {
		t.Fatalf("manager Start: %v", err)
	}
	cont.ProtocolBridge.SetWriteSink(func(context.Context, string, string, interface{}) error { return nil })

	_, _, created := callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", bridgeBody, nil, nil)
	id, _ := created["id"].(string)
	if err := cont.ProtocolBridge.UpdateSourceData(context.Background(), "src", "temp", 10.0); err != nil {
		t.Fatalf("UpdateSourceData: %v", err)
	}
	if got := cont.ProtocolBridge.GetBridgeStats(id)["total_transferred"]; got != int64(1) {
		t.Fatalf("baseline counter = %v, want 1", got)
	}

	callBridge(t, handleSetProtocolBridgeEnabled(false), http.MethodPost, "/api/v1/bridge/bridges/"+id+"/disable", "",
		[]string{"id"}, []string{id})
	if got := cont.ProtocolBridge.GetBridgeStats(id)["total_transferred"]; got != int64(1) {
		t.Fatalf("disable wiped the transfer counter: %v", got)
	}

	_, _, updated := callBridge(t, handleUpdateProtocolBridge, http.MethodPut, "/api/v1/bridge/bridges/"+id,
		`{"name":"line-1 edited","source_device":"src","target_device":"dst","enabled":true,`+
			`"rules":[{"source_point":"temp","target_point":"setpoint","conversion_type":"linear","scale":3,"offset":0,"enabled":true}]}`,
		[]string{"id"}, []string{id})
	if rules, _ := updated["rules"].([]interface{}); len(rules) != 1 {
		t.Fatalf("update response lost the rules: %#v", updated)
	}
	if got := cont.ProtocolBridge.GetBridgeStats(id)["total_transferred"]; got != int64(1) {
		t.Fatalf("edit wiped the transfer counter: %v", got)
	}
	// The edited rule must be the live one: scale 3 with offset 0 on input 4.
	if err := cont.ProtocolBridge.UpdateSourceData(context.Background(), "src", "temp", 4.0); err != nil {
		t.Fatalf("UpdateSourceData after edit: %v", err)
	}
	if got := cont.ProtocolBridge.GetBridgeStats(id)["total_transferred"]; got != int64(2) {
		t.Fatalf("edited bridge did not forward: stats %#v", cont.ProtocolBridge.GetBridgeStats(id))
	}
}

func TestProtocolBridgeStatusIsHonest(t *testing.T) {
	_, repo := useBridgeStore(t)
	addBridgeDevice(t, repo, "src", models.PointDef{Name: "temp"})
	addBridgeDevice(t, repo, "dst", models.PointDef{Name: "setpoint", AccessMode: "ReadWrite"})
	callBridge(t, handleCreateProtocolBridge, http.MethodPost, "/api/v1/bridge/bridges", bridgeBody, nil, nil)

	// The manager was never started and no write sink was installed, so the
	// status must say so instead of implying the bridge is running.
	_, _, data := callBridge(t, handleGetProtocolBridgeStatus, http.MethodGet, "/api/v1/bridge/status", "", nil, nil)
	if enabled, _ := data["enabled"].(bool); enabled {
		t.Fatalf("status claims the manager is running: %#v", data)
	}
	if wired, _ := data["sink_wired"].(bool); wired {
		t.Fatalf("status claims a write sink that was never installed: %#v", data)
	}
	if n, _ := data["configured_bridges"].(float64); n != 1 {
		t.Fatalf("configured_bridges = %v, want 1", data["configured_bridges"])
	}
}

func TestSaveConfigVersionWritesAndReadsBack(t *testing.T) {
	cont, _ := useBridgeStore(t)
	body := `{"config":{"mqtt":{"broker":"tcp://127.0.0.1:1883"}},"change_summary":"pre-upgrade"}`
	code, _, created := callBridge(t, handleSaveConfigVersion, http.MethodPost, "/api/v1/config", body, nil, nil)
	if code != http.StatusCreated {
		t.Fatalf("save returned %d, want 201", code)
	}
	version, _ := created["version"].(float64)
	if version == 0 {
		t.Fatalf("save reported no version id: %#v", created)
	}

	var count int
	if err := cont.Database.DB().QueryRow("SELECT COUNT(*) FROM config_versions WHERE version = ?", int(version)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("row for version %v missing: count=%d err=%v", version, count, err)
	}

	fetchedCode, _, fetched := callBridge(t, func(cc echo.Context) error {
		cc.SetParamNames("version")
		cc.SetParamValues(strconv.Itoa(int(version)))
		return handleGetConfigVersion(cc)
	}, http.MethodGet, "/api/v1/config/"+strconv.Itoa(int(version)), "", nil, nil)
	if fetchedCode != http.StatusOK {
		t.Fatalf("get returned %d, want 200", fetchedCode)
	}
	cfg, _ := fetched["config"].(map[string]interface{})
	mqtt, _ := cfg["mqtt"].(map[string]interface{})
	if mqtt["broker"] != "tcp://127.0.0.1:1883" {
		t.Fatalf("stored snapshot came back altered: %#v", fetched["config"])
	}
	if summary, _ := fetched["change_summary"].(string); summary != "pre-upgrade" {
		t.Fatalf("change_summary = %q, want pre-upgrade", summary)
	}

	// A version that was never written must not answer 200 with an empty object.
	code, errCode, _ := callBridge(t, func(cc echo.Context) error {
		cc.SetParamNames("version")
		cc.SetParamValues("999999")
		return handleGetConfigVersion(cc)
	}, http.MethodGet, "/api/v1/config/999999", "", nil, nil)
	if code != http.StatusNotFound || errCode != "ERR_CONFIG_VERSION_NOT_FOUND" {
		t.Fatalf("missing version = %d %s, want 404 ERR_CONFIG_VERSION_NOT_FOUND", code, errCode)
	}

	code, errCode, _ = callBridge(t, func(cc echo.Context) error {
		cc.SetParamNames("version")
		cc.SetParamValues("not-a-number")
		return handleGetConfigVersion(cc)
	}, http.MethodGet, "/api/v1/config/not-a-number", "", nil, nil)
	if code != http.StatusBadRequest || errCode != "ERR_CONFIG_VERSION_INVALID" {
		t.Fatalf("bad version = %d %s, want 400 ERR_CONFIG_VERSION_INVALID", code, errCode)
	}
}

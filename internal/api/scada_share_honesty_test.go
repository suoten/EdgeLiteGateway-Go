package api

// Four handler groups used to answer "it worked" while doing nothing:
//
//   POST /scada/project          echoed the request body, so every saved screen
//     disappeared on reload while the editor said "已保存到服务器".
//   GET  /scada/project/:name    returned {name, screens:[]}, indistinguishable
//     from "the gateway has your project and it is empty".
//   POST /resource-shares/transfer  returned {"success":true} and left created_by
//     alone, so the device stayed in the old account.
//   POST /integration/handshake  minted a session_id from the local clock and
//     claimed status:"connected" for a peer it never contacted; /push-device
//     claimed pushed:true without a socket; /health claimed healthy:true next to
//     active_connections:0.
//
// These tests pin the replacements: real persistence, real ownership change, real
// peer answers, and a distinct honest refusal when nothing can be measured.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/northbound"
	"edgelite/internal/platform"
	"edgelite/internal/storage"
)

type shareStore struct {
	db      *storage.Database
	devices *storage.DeviceRepo
	rules   *storage.RuleRepo
	users   *storage.UserRepo
}

func useShareStore(t *testing.T) *shareStore {
	t.Helper()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = t.TempDir() + "/main.db"
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	cont := GetContainer()
	prev := cont
	prevDB, prevDev, prevRule, prevUser := cont.Database, cont.DeviceRepo, cont.RuleRepo, cont.UserRepo
	cont.Database, cont.DeviceRepo, cont.RuleRepo, cont.UserRepo =
		db, storage.NewDeviceRepo(db), storage.NewRuleRepo(db), storage.NewUserRepo(db)
	t.Cleanup(func() {
		prev.Database, prev.DeviceRepo, prev.RuleRepo, prev.UserRepo = prevDB, prevDev, prevRule, prevUser
		_ = db.Close()
	})
	return &shareStore{db: db, devices: cont.DeviceRepo, rules: cont.RuleRepo, users: cont.UserRepo}
}

func (s *shareStore) device(t *testing.T, id, owner string) {
	t.Helper()
	d := &models.DeviceResponse{DeviceID: id, Name: id, Protocol: "simulator", Status: "online", CollectInterval: 5}
	if err := s.devices.Create(d, owner); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
}

func (s *shareStore) rule(t *testing.T, id, owner string) {
	t.Helper()
	r := &models.RuleResponse{RuleID: id, Name: id, Logic: "and", Severity: "warning"}
	if err := s.rules.Create(r, owner); err != nil {
		t.Fatalf("create rule %s: %v", id, err)
	}
}

func (s *shareStore) user(t *testing.T, id, username string) {
	t.Helper()
	if err := s.users.Create(id, username, "hash-not-a-password", "operator"); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// callHandler runs h against a fresh request. params alternates name/value so
// path-param handlers can be driven without a router.
func callHandler(t *testing.T, method, path, body string, h func(echo.Context) error, params ...string) (int, string, json.RawMessage, string) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if len(params) > 0 {
		names := make([]string, 0, len(params)/2)
		values := make([]string, 0, len(params)/2)
		for i := 0; i+1 < len(params); i += 2 {
			names = append(names, params[i])
			values = append(values, params[i+1])
		}
		c.SetParamNames(names...)
		c.SetParamValues(values...)
	}
	if err := h(c); err != nil {
		t.Fatalf("%s %s returned a transport error: %v", method, path, err)
	}
	var env struct {
		Message   string          `json:"message"`
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: unparseable envelope %s: %v", method, path, rec.Body, err)
	}
	return rec.Code, env.ErrorCode, env.Data, env.Message
}

func asMap(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data is not a JSON object: %s (%v)", raw, err)
	}
	return m
}

func asSlice(t *testing.T, raw json.RawMessage) []interface{} {
	t.Helper()
	var out []interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("data is not a JSON array: %s (%v)", raw, err)
	}
	return out
}

func useEmptyContainerDB(t *testing.T) {
	t.Helper()
	cont := GetContainer()
	prev := cont.Database
	cont.Database = nil
	t.Cleanup(func() { cont.Database = prev })
}

// --- SCADA projects ------------------------------------------------------

func TestSCADAProjectWithoutDatabaseRefuses(t *testing.T) {
	useEmptyContainerDB(t)

	status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/scada/project", `{"name":"default","widgets":[]}`, handleCreateSCADAProject)
	if status != http.StatusServiceUnavailable || code != "ERR_COMMON_DB_NOT_READY" {
		t.Fatalf("save without a database answered %d/%s, want 503/ERR_COMMON_DB_NOT_READY", status, code)
	}
	// An echo of the request with 201 is what used to happen here; nothing was
	// stored, so the answer has to be a refusal.
	if status == http.StatusCreated {
		t.Fatal("save reported Created for a gateway that cannot store anything")
	}

	for _, call := range []struct {
		name   string
		path   string
		method string
		h      func(echo.Context) error
	}{
		{"get", "/api/v1/scada/project/default", http.MethodGet, handleGetSCADAProject},
		{"list", "/api/v1/scada/projects", http.MethodGet, handleListSCADAProjects},
		{"delete", "/api/v1/scada/project/default", http.MethodDelete, handleDeleteSCADAProject},
	} {
		status, code, _, _ := callHandler(t, call.method, call.path, "", call.h, "name", "default")
		if status != http.StatusServiceUnavailable || code != "ERR_COMMON_DB_NOT_READY" {
			t.Fatalf("%s without a database answered %d/%s, want 503/ERR_COMMON_DB_NOT_READY", call.name, status, code)
		}
	}
}

func TestSCADAProjectSurvivesRoundTrip(t *testing.T) {
	useShareStore(t)

	// Before anything is stored the answer is "no project yet", not a fabricated
	// empty-but-successful document.
	_, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/scada/project/default", "", handleGetSCADAProject, "name", "default")
	before := asMap(t, raw)
	if before["exists"] != false {
		t.Fatalf("untouched project reported exists=%v, want false", before["exists"])
	}

	body := `{"name":"default","widgets":[{"id":1,"type":"gauge"},{"id":2,"type":"chart"}],"scenes":[{"id":"s1","name":"车间","widgets":[{"id":1}]}]}`
	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/scada/project", body, handleCreateSCADAProject)
	if status != http.StatusCreated {
		t.Fatalf("save answered %d, want 201", status)
	}
	saved := asMap(t, raw)
	if saved["widget_count"] != float64(2) || saved["scene_count"] != float64(1) {
		t.Fatalf("save reported %+v, want widget_count 2 and scene_count 1", saved)
	}

	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/scada/project/default", "", handleGetSCADAProject, "name", "default")
	loaded := asMap(t, raw)
	if loaded["exists"] != true {
		t.Fatalf("saved project reported exists=%v, want true", loaded["exists"])
	}
	widgets, _ := loaded["widgets"].([]interface{})
	if len(widgets) != 2 {
		t.Fatalf("loaded project has %d widgets, want the 2 that were saved (%v)", len(widgets), loaded["widgets"])
	}
	scenes, _ := loaded["scenes"].([]interface{})
	if len(scenes) != 1 {
		t.Fatalf("loaded project has %d scenes, want 1", len(scenes))
	}
	if loaded["updated_by"] != "" {
		// An anonymous save must attribute nothing; a fabricated "admin" here
		// would make the operator believe a real account edited the screen.
		t.Fatalf("anonymous save recorded updated_by=%v, want the empty string", loaded["updated_by"])
	}

	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/scada/projects", "", handleListSCADAProjects)
	rows := asSlice(t, raw)
	if len(rows) != 1 {
		t.Fatalf("project list has %d rows, want 1: %v", len(rows), rows)
	}
	row, _ := rows[0].(map[string]interface{})
	if row["name"] != "default" || row["widget_count"] != float64(2) {
		t.Fatalf("project list row is %+v, want name default and widget_count 2", row)
	}

	status, _, _, _ = callHandler(t, http.MethodDelete, "/api/v1/scada/project/default", "", handleDeleteSCADAProject, "name", "default")
	if status != http.StatusOK {
		t.Fatalf("delete answered %d, want 200", status)
	}
	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/scada/project/default", "", handleGetSCADAProject, "name", "default")
	if again := asMap(t, raw); again["exists"] != false {
		t.Fatalf("deleted project still reports exists=%v", again["exists"])
	}
	status, code, _, _ := callHandler(t, http.MethodDelete, "/api/v1/scada/project/default", "", handleDeleteSCADAProject, "name", "default")
	if status != http.StatusNotFound || code != "ERR_SCADA_PROJECT_NOT_FOUND" {
		t.Fatalf("deleting a missing project answered %d/%s, want 404/ERR_SCADA_PROJECT_NOT_FOUND", status, code)
	}
}

func TestSCADAProjectSaveAttributesTheOperator(t *testing.T) {
	useShareStore(t)
	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/scada/project", `{"name":"line3","widgets":[{"id":9}]}`)
	if err := handleCreateSCADAProject(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("save answered %d, want 201", rec.Code)
	}
	c, rec = setupWithAdmin(http.MethodGet, "/api/v1/scada/project/line3", "")
	c.SetParamNames("name")
	c.SetParamValues("line3")
	if err := handleGetSCADAProject(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Data["updated_by"] != "admin" {
		t.Fatalf("stored project has updated_by=%v, want admin", env.Data["updated_by"])
	}
}

func TestSCADAProjectNameAndSizeLimits(t *testing.T) {
	useShareStore(t)

	for _, name := range []string{"", "a/b", "..", strings.Repeat("n", 65)} {
		body := fmt.Sprintf(`{"name":%q,"widgets":[]}`, name)
		status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/scada/project", body, handleCreateSCADAProject)
		if status != http.StatusBadRequest || code != "ERR_SCADA_PROJECT_NAME_INVALID" {
			t.Fatalf("name %q answered %d/%s, want 400/ERR_SCADA_PROJECT_NAME_INVALID", name, status, code)
		}
	}
	// A path parameter has to be refused the same way, or a crafted URL could
	// read another setting key.
	status, code, _, _ := callHandler(t, http.MethodGet, "/api/v1/scada/project/..%2f..", "", handleGetSCADAProject, "name", "../etc")
	if status != http.StatusBadRequest || code != "ERR_SCADA_PROJECT_NAME_INVALID" {
		t.Fatalf("path-traversal name answered %d/%s, want 400/ERR_SCADA_PROJECT_NAME_INVALID", status, code)
	}

	pad := `{"id":1,"blob":"` + strings.Repeat("x", scadaProjectMaxBytes+16) + `"}`
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/scada/project", `{"name":"big","widgets":[`+pad+`]}`, handleCreateSCADAProject)
	if status != http.StatusBadRequest || code != "ERR_SCADA_PROJECT_TOO_LARGE" {
		t.Fatalf("oversized project answered %d/%s, want 400/ERR_SCADA_PROJECT_TOO_LARGE", status, code)
	}
}

func TestSCADAProjectCorruptionIsNotHidden(t *testing.T) {
	store := useShareStore(t)
	if err := store.db.SetSetting(scadaProjectKey("broken"), `{not json`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	status, code, _, _ := callHandler(t, http.MethodGet, "/api/v1/scada/project/broken", "", handleGetSCADAProject, "name", "broken")
	if status != http.StatusInternalServerError || code != "ERR_SCADA_PROJECT_CORRUPT" {
		t.Fatalf("corrupt project answered %d/%s, want 500/ERR_SCADA_PROJECT_CORRUPT", status, code)
	}

	_, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/scada/projects", "", handleListSCADAProjects)
	rows := asSlice(t, raw)
	if len(rows) != 1 {
		t.Fatalf("project list hid the corrupt row: %v", rows)
	}
	row, _ := rows[0].(map[string]interface{})
	if row["corrupted"] != true || row["name"] != "broken" {
		t.Fatalf("corrupt project row is %+v, want name broken and corrupted true", row)
	}
}

// --- Resource share transfer --------------------------------------------

func TestTransferDeviceChangesStoredOwner(t *testing.T) {
	store := useShareStore(t)
	store.device(t, "dev-a", "u1")
	store.device(t, "dev-b", "u1")
	store.user(t, "u2", "operator2")

	body := `{"resource_type":"device","resource_ids":["dev-a","dev-b","dev-missing"],"target_user_id":"u2"}`
	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/transfer", body, handleTransferResource)
	if status != http.StatusOK {
		t.Fatalf("transfer answered %d, want 200 (%s)", status, raw)
	}
	out := asMap(t, raw)
	moved, _ := out["transferred"].([]interface{})
	failed, _ := out["failed"].([]interface{})
	if len(moved) != 2 || len(failed) != 1 {
		t.Fatalf("transfer reported %d moved / %d failed, want 2/1: %+v", len(moved), len(failed), out)
	}
	if out["count"] != float64(2) {
		t.Fatalf("transfer count is %v, want 2", out["count"])
	}
	if out["new_owner_username"] != "operator2" {
		t.Fatalf("transfer echoed new_owner_username=%v, want operator2", out["new_owner_username"])
	}
	first, _ := moved[0].(map[string]interface{})
	if first["previous_owner"] != "u1" {
		t.Fatalf("transferred row lost the previous owner: %+v", first)
	}
	failure, _ := failed[0].(map[string]interface{})
	if failure["resource_id"] != "dev-missing" || failure["error"] != "ERR_DEVICE_NOT_FOUND" {
		t.Fatalf("missing device reported as %+v, want resource_id dev-missing and ERR_DEVICE_NOT_FOUND", failure)
	}

	// The claim is only honest if the row really moved.
	for _, id := range []string{"dev-a", "dev-b"} {
		dev, err := store.devices.Get(id)
		if err != nil || dev == nil {
			t.Fatalf("Get(%s): %v/%v", id, err, dev)
		}
		if dev.CreatedBy != "u2" {
			t.Fatalf("device %s still owned by %q after a successful transfer", id, dev.CreatedBy)
		}
	}
}

func TestTransferAcceptsTheTypedClientShape(t *testing.T) {
	store := useShareStore(t)
	store.device(t, "dev-solo", "u1")
	store.user(t, "u9", "night-shift")

	// resourceShareApi.transfer sends resource_id/new_owner_id.
	body := `{"resource_type":"device","resource_id":"dev-solo","new_owner_id":"u9"}`
	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/transfer", body, handleTransferResource)
	if status != http.StatusOK {
		t.Fatalf("singular transfer answered %d, want 200 (%s)", status, raw)
	}
	dev, _ := store.devices.Get("dev-solo")
	if dev == nil || dev.CreatedBy != "u9" {
		t.Fatalf("singular transfer left ownership at %+v", dev)
	}
}

func TestTransferValidationAndUnsupportedKind(t *testing.T) {
	store := useShareStore(t)
	store.device(t, "dev-v", "u1")
	store.user(t, "u2", "operator2")

	cases := []struct {
		name           string
		body           string
		status         int
		code           string
		ownerUntouched bool
	}{
		{"no ids", `{"resource_type":"device","target_user_id":"u2"}`, http.StatusBadRequest, "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED", true},
		{"blank ids", `{"resource_type":"device","resource_ids":[""],"target_user_id":"u2"}`, http.StatusBadRequest, "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED", true},
		{"no target", `{"resource_type":"device","resource_ids":["dev-v"]}`, http.StatusBadRequest, "ERR_RESOURCE_SHARE_TARGET_REQUIRED", true},
		{"unknown target", `{"resource_type":"device","resource_ids":["dev-v"],"target_user_id":"ghost"}`, http.StatusNotFound, "ERR_USER_NOT_FOUND", true},
		{"template has no owner", `{"resource_type":"template","resource_ids":["dev-v"],"target_user_id":"u2"}`, http.StatusNotImplemented, "ERR_RESOURCE_SHARE_TRANSFER_UNSUPPORTED", true},
	}
	for _, tc := range cases {
		status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/transfer", tc.body, handleTransferResource)
		if status != tc.status || code != tc.code {
			t.Fatalf("%s: answered %d/%s, want %d/%s", tc.name, status, code, tc.status, tc.code)
		}
		if tc.ownerUntouched {
			dev, _ := store.devices.Get("dev-v")
			if dev == nil || dev.CreatedBy != "u1" {
				t.Fatalf("%s: rejected transfer still changed ownership (%+v)", tc.name, dev)
			}
		}
	}
}

func TestTransferRuleChangesStoredOwner(t *testing.T) {
	store := useShareStore(t)
	store.rule(t, "rule-1", "u1")
	store.user(t, "u2", "operator2")

	body := `{"resource_type":"rule","resource_ids":["rule-1"],"target_user_id":"u2"}`
	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/transfer", body, handleTransferResource)
	if status != http.StatusOK {
		t.Fatalf("rule transfer answered %d, want 200 (%s)", status, raw)
	}
	rule, err := store.rules.Get("rule-1")
	if err != nil || rule == nil {
		t.Fatalf("rules.Get: %v/%v", err, rule)
	}
	if rule.CreatedBy != "u2" {
		t.Fatalf("rule still owned by %q after transfer", rule.CreatedBy)
	}
}

// --- Share lookup and access checks --------------------------------------

func createShare(t *testing.T, resourceID, gateway string, hours int, perms ...string) string {
	t.Helper()
	permission := `["read"]`
	if len(perms) > 0 {
		raw, _ := json.Marshal(perms)
		permission = string(raw)
	}
	body := fmt.Sprintf(`{"resource_type":"device","resource_id":%q,"target_gateway_id":%q,"expires_hours":%d,"permissions":%s}`,
		resourceID, gateway, hours, permission)
	_, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares", body, handleCreateResourceShare)
	share := asMap(t, raw)
	id, _ := share["id"].(string)
	if id == "" {
		t.Fatalf("share create returned no id: %v", share)
	}
	return id
}

func TestResourceSharesRequireAStore(t *testing.T) {
	useEmptyContainerDB(t)
	// loadResourceShares used to answer "no shares" when the database was nil,
	// which reads identically to "nothing is shared".
	status, code, _, _ := callHandler(t, http.MethodGet, "/api/v1/resource-shares", "", handleListResourceShares)
	if status != http.StatusServiceUnavailable || code != "ERR_COMMON_DB_NOT_READY" {
		t.Fatalf("list without a store answered %d/%s, want 503/ERR_COMMON_DB_NOT_READY", status, code)
	}
	status, code, _, _ = callHandler(t, http.MethodGet, "/api/v1/resource-shares/resource/device/d1", "", handleGetResourceShare, "type", "device", "id", "d1")
	if status != http.StatusServiceUnavailable || code != "ERR_COMMON_DB_NOT_READY" {
		t.Fatalf("get-by-resource without a store answered %d/%s", status, code)
	}
}

func TestGetResourceShareReflectsTheStore(t *testing.T) {
	store := useShareStore(t)

	_, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/resource-shares/resource/device/d1", "", handleGetResourceShare, "type", "device", "id", "d1")
	empty := asMap(t, raw)
	if empty["shared"] != false {
		t.Fatalf("unknown resource reported shared=%v", empty["shared"])
	}
	if rows, _ := empty["shares"].([]interface{}); len(rows) != 0 {
		t.Fatalf("unknown resource returned %d share rows, want 0", len(rows))
	}
	if empty["resource_type"] != "device" || empty["resource_id"] != "d1" {
		t.Fatalf("lookup response dropped its own key: %+v", empty)
	}

	id := createShare(t, "d1", "gw-2", 24)
	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/resource-shares/resource/device/d1", "", handleGetResourceShare, "type", "device", "id", "d1")
	found := asMap(t, raw)
	if found["shared"] != true {
		t.Fatalf("stored share reported shared=%v, want true", found["shared"])
	}
	rows, _ := found["shares"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("share lookup returned %d rows, want 1: %v", len(rows), rows)
	}
	row, _ := rows[0].(map[string]interface{})
	if row["id"] != id || row["status"] != "active" {
		t.Fatalf("share row is %+v, want id %s and status active", row, id)
	}

	// An expired grant must not be reported as shared.
	shares := []resourceShare{{
		ID: "share-old", ResourceType: "device", ResourceID: "d1", TargetGatewayID: "gw-2",
		Permissions: []string{"read"}, ExpiresHours: 1,
		CreatedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		ExpiresAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	rawShares, _ := json.Marshal(shares)
	if err := store.db.SetSetting("resource_shares", string(rawShares)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/resource-shares/resource/device/d1", "", handleGetResourceShare, "type", "device", "id", "d1")
	expired := asMap(t, raw)
	if expired["shared"] != false {
		t.Fatalf("expired share reported shared=%v, want false", expired["shared"])
	}
	if rows, _ := expired["shares"].([]interface{}); len(rows) != 1 {
		t.Fatalf("expired share hid the record: %v", rows)
	}
}

func TestCheckResourceShareAnswersFromRecords(t *testing.T) {
	store := useShareStore(t)

	status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/check", `{"resource_type":"device"}`, handleCheckResourceShare)
	if status != http.StatusBadRequest || code != "ERR_RESOURCE_SHARE_RESOURCE_REQUIRED" {
		t.Fatalf("incomplete check answered %d/%s, want 400/ERR_RESOURCE_SHARE_RESOURCE_REQUIRED", status, code)
	}

	// The old handler said has_access:true for this. Nothing is shared yet.
	_, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/resource-shares/check", `{"resource_type":"device","resource_id":"d1"}`, handleCheckResourceShare)
	none := asMap(t, raw)
	if none["has_access"] != false || none["reason"] != "no_share" {
		t.Fatalf("check with no shares answered %+v, want has_access false / no_share", none)
	}

	createShare(t, "d1", "gw-2", 24)
	_, _, raw, _ = callHandler(t, http.MethodPost, "/api/v1/resource-shares/check",
		`{"resource_type":"device","resource_id":"d1","target_gateway_id":"gw-2"}`, handleCheckResourceShare)
	granted := asMap(t, raw)
	if granted["has_access"] != true || granted["reason"] != "share_active" {
		t.Fatalf("active share answered %+v, want has_access true / share_active", granted)
	}
	if granted["share_id"] == nil || granted["share_id"] == "" {
		t.Fatalf("grant did not name the share it used: %+v", granted)
	}

	_, _, raw, _ = callHandler(t, http.MethodPost, "/api/v1/resource-shares/check",
		`{"resource_type":"device","resource_id":"d1","target_gateway_id":"gw-other"}`, handleCheckResourceShare)
	wrong := asMap(t, raw)
	if wrong["has_access"] != false || wrong["reason"] != "target_gateway_mismatch" {
		t.Fatalf("other gateway answered %+v, want target_gateway_mismatch", wrong)
	}

	_, _, raw, _ = callHandler(t, http.MethodPost, "/api/v1/resource-shares/check",
		`{"resource_type":"device","resource_id":"d1","target_gateway_id":"gw-2","permission":"write"}`, handleCheckResourceShare)
	denied := asMap(t, raw)
	if denied["has_access"] != false || denied["reason"] != "permission_denied" {
		t.Fatalf("read-only share answered a write request with %+v, want permission_denied", denied)
	}

	shares := []resourceShare{{
		ID: "share-expired", ResourceType: "device", ResourceID: "d1", TargetGatewayID: "gw-2",
		Permissions: []string{"read"}, ExpiresHours: 1,
		CreatedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}}
	rawShares, _ := json.Marshal(shares)
	if err := store.db.SetSetting("resource_shares", string(rawShares)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	_, _, raw, _ = callHandler(t, http.MethodPost, "/api/v1/resource-shares/check",
		`{"resource_type":"device","resource_id":"d1","target_gateway_id":"gw-2"}`, handleCheckResourceShare)
	expired := asMap(t, raw)
	if expired["has_access"] != false || expired["reason"] != "share_expired" {
		t.Fatalf("expired share answered %+v, want share_expired", expired)
	}
}

func TestShareStoreCorruptionIsNotAnEmptyList(t *testing.T) {
	store := useShareStore(t)
	if err := store.db.SetSetting("resource_shares", `{"not":"a list"}`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	status, code, _, _ := callHandler(t, http.MethodGet, "/api/v1/resource-shares", "", handleListResourceShares)
	if status != http.StatusInternalServerError || code != "ERR_RESOURCE_SHARE_STORE_CORRUPT" {
		t.Fatalf("corrupt share store answered %d/%s, want 500/ERR_RESOURCE_SHARE_STORE_CORRUPT", status, code)
	}
}

func TestUnshareAndDeleteReportTheirRealOutcome(t *testing.T) {
	useShareStore(t)
	id := createShare(t, "d9", "gw-9", 24)

	status, code, _, _ := callHandler(t, http.MethodDelete, "/api/v1/resource-shares/ghost", "", handleDeleteResourceShare, "id", "ghost")
	if status != http.StatusNotFound || code != "ERR_RESOURCE_SHARE_NOT_FOUND" {
		t.Fatalf("delete of a missing share answered %d/%s", status, code)
	}

	status, _, raw, _ := callHandler(t, http.MethodDelete, "/api/v1/resource-shares", `{"resource_type":"device","resource_id":"none"}`, handleUnshareResource)
	if status != http.StatusOK {
		t.Fatalf("unshare answered %d (%s)", status, raw)
	}
	if out := asMap(t, raw); out["deleted"] != false || out["count"] != float64(0) {
		t.Fatalf("unshare of a missing resource reported %+v, want deleted false / count 0", out)
	}

	_, _, raw, _ = callHandler(t, http.MethodDelete, "/api/v1/resource-shares", `{"resource_type":"device","resource_id":"d9"}`, handleUnshareResource)
	if out := asMap(t, raw); out["deleted"] != true || out["count"] != float64(1) {
		t.Fatalf("unshare of an existing resource reported %+v, want deleted true / count 1", out)
	}
	_, _, raw, _ = callHandler(t, http.MethodGet, "/api/v1/resource-shares", "", handleListResourceShares)
	if rows := asSlice(t, raw); len(rows) != 0 {
		t.Fatalf("shares survived unshare: %v", rows)
	}
	_ = id
}

// --- Integration status / health / test / endpoints ----------------------

func usePlatformManager(t *testing.T, mgr *northbound.Manager, platforms map[string]interface{}) {
	t.Helper()
	cont := GetContainer()
	prevMgr, prevPlatforms := cont.PlatformMgr, config.GetConfig().Platforms
	cont.PlatformMgr = mgr
	config.GetConfig().Platforms = platforms
	t.Cleanup(func() {
		cont.PlatformMgr = prevMgr
		config.GetConfig().Platforms = prevPlatforms
	})
}

func TestIntegrationStatusCountsWhatExists(t *testing.T) {
	usePlatformManager(t, northbound.NewManager(nil), map[string]interface{}{
		"iotda": map[string]interface{}{"type": "huawei_iotda", "enabled": true},
		"tb":    map[string]interface{}{"type": "thingsboard", "enabled": false},
	})

	status, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/integration/status", "", handleGetIntegrationStatus)
	if status != http.StatusOK {
		t.Fatalf("status answered %d (%s)", status, raw)
	}
	out := asMap(t, raw)
	if out["endpoints"] != float64(2) || out["enabled_endpoints"] != float64(1) {
		t.Fatalf("status counted %+v, want 2 endpoints and 1 enabled", out)
	}
	if out["enabled"] != true {
		t.Fatalf("status reported enabled=%v while a platform is enabled", out["enabled"])
	}
	// Nothing is connected in this test, and the answer has to say so.
	if out["active_connections"] != float64(0) {
		t.Fatalf("status reported active_connections=%v with no connection", out["active_connections"])
	}
	if rows, _ := out["connected_platforms"].([]interface{}); rows == nil || len(rows) != 0 {
		t.Fatalf("status omitted connected_platforms: %+v", out)
	}
}

func TestIntegrationHealthIsMeasuredNotAsserted(t *testing.T) {
	useEmptyContainerDB(t)
	cont := GetContainer()
	prevMgr := cont.PlatformMgr
	cont.PlatformMgr = nil
	t.Cleanup(func() { cont.PlatformMgr = prevMgr })

	status, code, _, _ := callHandler(t, http.MethodGet, "/api/v1/integration/health", "", handleGetIntegrationHealth)
	if status != http.StatusServiceUnavailable || code != "ERR_INTEG_BACKHAUL_NOT_READY" {
		t.Fatalf("health without a manager answered %d/%s", status, code)
	}

	usePlatformManager(t, northbound.NewManager(nil), map[string]interface{}{})
	status, _, raw, _ := callHandler(t, http.MethodGet, "/api/v1/integration/health", "", handleGetIntegrationHealth)
	if status != http.StatusOK {
		t.Fatalf("health answered %d (%s)", status, raw)
	}
	out := asMap(t, raw)
	// The old handler paired a literal healthy:true with a literal
	// active_connections:0. With nothing connected, health is not true.
	if out["healthy"] != false {
		t.Fatalf("health reported healthy=%v with %v connections", out["healthy"], out["active_connections"])
	}
	if out["active_connections"] != float64(0) {
		t.Fatalf("health active_connections=%v, want 0", out["active_connections"])
	}
	if rows, _ := out["platforms"].([]interface{}); rows == nil {
		t.Fatalf("health omitted the platforms array: %+v", out)
	}
}

func TestIntegrationTestActuallyDials(t *testing.T) {
	usePlatformManager(t, northbound.NewManager(nil), map[string]interface{}{})

	status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/integration/test", `{}`, handleTestIntegration)
	if status != http.StatusBadRequest || code != "ERR_INTEG_TEST_NAME_REQUIRED" {
		t.Fatalf("nameless test answered %d/%s, want 400/ERR_INTEG_TEST_NAME_REQUIRED", status, code)
	}

	// An unknown platform type cannot be dialled: the answer is a measured
	// failure, never the old unconditional success:true.
	_, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/integration/test", `{"name":"no-such-platform"}`, handleTestIntegration)
	out := asMap(t, raw)
	if out["success"] != false {
		t.Fatalf("test of an unknown platform reported %+v, want success false", out)
	}
	if msg, _ := out["message"].(string); msg == "" {
		t.Fatalf("failed test carried no reason: %+v", out)
	}
}

func TestIntegrationEndpointRoutesRefuse(t *testing.T) {
	useShareStore(t)
	calls := []struct {
		method string
		path   string
		h      func(echo.Context) error
	}{
		{http.MethodGet, "/api/v1/integration/endpoints", handleListIntegrationEndpoints},
		{http.MethodPost, "/api/v1/integration/endpoints", handleCreateIntegrationEndpoint},
		{http.MethodPut, "/api/v1/integration/endpoints/e1", handleUpdateIntegrationEndpoint},
		{http.MethodDelete, "/api/v1/integration/endpoints/e1", handleDeleteIntegrationEndpoint},
	}
	for _, call := range calls {
		status, code, _, _ := callHandler(t, call.method, call.path, `{"url":"http://example.invalid"}`, call.h, "id", "e1")
		if status != http.StatusNotImplemented || code != "ERR_INTEG_ENDPOINTS_UNSUPPORTED_USE_PLATFORMS" {
			t.Fatalf("%s %s answered %d/%s, want 501/ERR_INTEG_ENDPOINTS_UNSUPPORTED_USE_PLATFORMS",
				call.method, call.path, status, code)
		}
	}
}

// --- Push device + handshake --------------------------------------------

func TestPushDeviceRefusesWithoutAPlatform(t *testing.T) {
	usePlatformManager(t, nil, map[string]interface{}{})

	status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/integration/push-device", `{"data":{"t":1}}`, handlePushDevice)
	if status != http.StatusBadRequest || code != "ERR_DEVICE_PUSH_INVALID_ID" {
		t.Fatalf("push without device_id answered %d/%s", status, code)
	}
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/integration/push-device", `{"device_id":"d1"}`, handlePushDevice)
	if status != http.StatusBadRequest || code != "ERR_DEVICE_PUSH_EMPTY" {
		t.Fatalf("push without data answered %d/%s", status, code)
	}
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/integration/push-device", `{"device_id":"d1","data":{"t":1}}`, handlePushDevice)
	if status != http.StatusServiceUnavailable || code != "ERR_INTEG_BACKHAUL_NOT_READY" {
		t.Fatalf("push without a manager answered %d/%s, want 503 (pushed:true is the bug)", status, code)
	}

	usePlatformManager(t, northbound.NewManager(nil), map[string]interface{}{})
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/integration/push-device", `{"device_id":"d1","data":{"t":1}}`, handlePushDevice)
	if status != http.StatusServiceUnavailable || code != "ERR_INTEG_BACKHAUL_NOT_READY" {
		t.Fatalf("push with no connected platform answered %d/%s, want 503", status, code)
	}
}

// pushStub is a northbound platform that answers on demand. It is registered in
// the real platform registry under its own type name, so the push handler is
// driven through the genuine Manager path -- Connect, IsConnected,
// ForwardTelemetry, per-platform counters -- rather than a mocked manager. That
// is what pins handlePushDevice to the counters instead of to ConnectedNames.
type pushStub struct {
	platform.BasePlatform
	name string
	fail bool
	seen *pushObservation
}

type pushObservation struct {
	deviceID string
	points   int
}

func (s *pushStub) Name() string { return s.name }

func (s *pushStub) Connect(_ context.Context, _ map[string]interface{}) error {
	s.SetConnected(true)
	return nil
}

func (s *pushStub) Disconnect() error {
	s.SetConnected(false)
	return nil
}

func (s *pushStub) PublishTelemetry(_ context.Context, deviceID string, data map[string]interface{}) error {
	if s.seen != nil {
		s.seen.deviceID = deviceID
		s.seen.points = len(data)
	}
	if s.fail {
		return fmt.Errorf("stub platform rejected the batch")
	}
	return nil
}

func (s *pushStub) PublishAttributes(_ context.Context, _ string, _ map[string]interface{}) error {
	return nil
}

func (s *pushStub) PublishDeviceStatus(_ context.Context, _ string, _ bool) error { return nil }
func (s *pushStub) OnRPCRequest(_ platform.RPCCallback)                           {}

var (
	pushStubOnce sync.Once
	pushStubFail bool // read by the factory at Create time
	pushStubSeen []*pushObservation
)

func registerPushStub() {
	pushStubOnce.Do(func() {
		platform.GetPlatformRegistry().Register("apitest_push_stub", func() platform.Handler {
			obs := &pushObservation{}
			pushStubSeen = append(pushStubSeen, obs)
			return &pushStub{BasePlatform: platform.NewBasePlatform(), fail: pushStubFail, seen: obs}
		})
	})
}

// connectPushStub brings up a stub platform instance under the given name and
// returns the observation slot that records what the platform actually received.
func connectPushStub(t *testing.T, mgr *northbound.Manager, name string, fail bool) *pushObservation {
	t.Helper()
	registerPushStub()
	pushStubFail = fail
	before := len(pushStubSeen)
	if err := mgr.Connect(name, map[string]interface{}{"type": "apitest_push_stub"}); err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	if len(pushStubSeen) != before+1 {
		t.Fatalf("connecting %s created %d handlers, want exactly 1", name, len(pushStubSeen)-before)
	}
	t.Cleanup(func() { _ = mgr.Disconnect(name) })
	return pushStubSeen[before]
}

func TestPushDeviceReportsMeasuredDelivery(t *testing.T) {
	usePlatformManager(t, northbound.NewManager(nil), map[string]interface{}{})
	mgr := GetContainer().PlatformMgr

	okSeen := connectPushStub(t, mgr, "stub-ok", false)
	_ = connectPushStub(t, mgr, "stub-broken", true)

	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"plc-1","data":{"temp":21.5,"pressure":3}}`, handlePushDevice)
	if status != http.StatusOK {
		t.Fatalf("push to a partially healthy backhaul answered %d (%s), want 200 with the failure itemized", status, raw)
	}
	out := asMap(t, raw)
	if out["pushed"] != true {
		t.Fatalf("push result is %+v, want pushed true because one platform took the batch", out)
	}
	delivered, _ := out["delivered"].([]interface{})
	if len(delivered) != 1 || delivered[0] != "stub-ok" {
		t.Fatalf("delivered=%v, want only the platform whose sent counter moved", delivered)
	}
	failed, _ := out["failed"].([]interface{})
	if len(failed) != 1 || failed[0] != "stub-broken" {
		t.Fatalf("failed=%v, want the platform that rejected the batch", failed)
	}
	if okSeen.deviceID != "plc-1" || okSeen.points != 2 {
		t.Fatalf("the healthy platform received device=%q points=%d, want plc-1 and 2", okSeen.deviceID, okSeen.points)
	}

	// With no platform able to take the batch, the old handler still said
	// pushed:true. Nothing arrived, so the answer has to be a measured failure.
	only := northbound.NewManager(nil)
	_ = connectPushStub(t, only, "stub-broken-only", true)
	usePlatformManager(t, only, map[string]interface{}{})
	status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"plc-1","data":{"temp":21.5}}`, handlePushDevice)
	if status != http.StatusBadGateway || code != "ERR_DEVICE_PUSH_FAILED" {
		t.Fatalf("push that reached no platform answered %d/%s, want 502/ERR_DEVICE_PUSH_FAILED", status, code)
	}
}

func TestNorthboundDeliveryReportCountsOnlyRealSends(t *testing.T) {
	connected := []string{"ok", "failed", "silent"}
	before := map[string]northbound.PlatformStats{
		"ok":     {MessagesSent: 5},
		"failed": {MessagesFailed: 2},
		"silent": {},
	}
	after := map[string]northbound.PlatformStats{
		"ok":     {MessagesSent: 6},
		"failed": {MessagesSent: 0, MessagesFailed: 3},
		"silent": {MessagesSent: 0, MessagesFailed: 0},
	}
	delivered, failed := northboundDeliveryReport(connected, before, after)
	if len(delivered) != 1 || delivered[0] != "ok" {
		t.Fatalf("delivered=%v, want only the platform whose sent counter moved", delivered)
	}
	if len(failed) != 2 || failed[0] != "failed" || failed[1] != "silent" {
		t.Fatalf("failed=%v, want the erroring and the silent platform", failed)
	}

	// A platform missing from the counter map entirely must not count as a
	// delivery either.
	delivered, failed = northboundDeliveryReport([]string{"new"}, nil, nil)
	if len(delivered) != 0 || len(failed) != 1 {
		t.Fatalf("untracked platform reported delivered=%v failed=%v", delivered, failed)
	}
}

func TestHandshakeReportsWhatThePeerSaid(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotPath == "/reject" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if gotPath == "/text" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("no session here"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"peer-77","ack":true}`))
	}))
	defer peer.Close()

	for _, bad := range []string{"", "not-a-url", "ftp://host/x", "http://"} {
		body := fmt.Sprintf(`{"cloud_url":%q}`, bad)
		status, code, _, _ := callHandler(t, http.MethodPost, "/api/v1/integration/handshake", body, handleHandshake)
		if status != http.StatusBadRequest || code != "ERR_INTEG_HANDSHAKE_URL_REQUIRED" {
			t.Fatalf("cloud_url %q answered %d/%s, want 400/ERR_INTEG_HANDSHAKE_URL_REQUIRED", bad, status, code)
		}
	}

	body := fmt.Sprintf(`{"cloud_url":%q,"gateway_id":"gw-1","protocol_version":"1.2"}`, peer.URL+"/handshake")
	status, _, raw, _ := callHandler(t, http.MethodPost, "/api/v1/integration/handshake", body, handleHandshake)
	if status != http.StatusOK {
		t.Fatalf("handshake answered %d (%s)", status, raw)
	}
	out := asMap(t, raw)
	if out["session_id"] != "peer-77" {
		t.Fatalf("handshake returned session_id=%v, want the peer's peer-77", out["session_id"])
	}
	if out["status"] != "connected" || out["peer_status"] != float64(200) {
		t.Fatalf("handshake result is %+v, want connected/200", out)
	}
	if gotBody == nil || gotBody["gateway_id"] != "gw-1" || gotBody["protocol_version"] != "1.2" {
		t.Fatalf("peer received %+v, want the gateway id and protocol version", gotBody)
	}

	reject := fmt.Sprintf(`{"cloud_url":%q}`, peer.URL+"/reject")
	status, code, _, msg := callHandler(t, http.MethodPost, "/api/v1/integration/handshake", reject, handleHandshake)
	if status != http.StatusBadGateway || code != "ERR_INTEG_HANDSHAKE_FAILED" {
		t.Fatalf("refused handshake answered %d/%s, want 502/ERR_INTEG_HANDSHAKE_FAILED", status, code)
	}
	if !strings.Contains(msg, "401") {
		t.Fatalf("refused handshake hid the peer status: %q", msg)
	}

	silent := fmt.Sprintf(`{"cloud_url":%q}`, peer.URL+"/text")
	_, _, raw, _ = callHandler(t, http.MethodPost, "/api/v1/integration/handshake", silent, handleHandshake)
	noSession := asMap(t, raw)
	sid, present := noSession["session_id"]
	if !present || sid != nil {
		t.Fatalf("peer without a session field produced session_id=%v present=%v, want JSON null", sid, present)
	}

	// A peer that is simply not there must never produce status:"connected".
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/integration/handshake", fmt.Sprintf(`{"cloud_url":%q}`, deadURL), handleHandshake)
	if status != http.StatusBadGateway || code != "ERR_INTEG_HANDSHAKE_FAILED" {
		t.Fatalf("unreachable peer answered %d/%s, want 502/ERR_INTEG_HANDSHAKE_FAILED", status, code)
	}
}

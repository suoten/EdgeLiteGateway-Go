package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/services"
)

// callDeviceHandler runs a device handler as an admin with the given path
// params and returns the status plus the decoded envelope.
func callDeviceHandler(t *testing.T, h func(echo.Context) error, method, path, body string, params, values []string) (int, map[string]interface{}) {
	t.Helper()
	c, rec := setupWithAdmin(method, path, body)
	if len(params) > 0 {
		c.SetParamNames(params...)
		c.SetParamValues(values...)
	}
	if err := h(c); err != nil {
		t.Fatalf("%s %s returned an error: %v", method, path, err)
	}
	var env map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body.String(), err)
	}
	return rec.Code, env
}

func useConfigVersionStore(t *testing.T) *ServiceContainer {
	t.Helper()
	cont := withDeviceService(t)
	cont.ConfigVersionMgr = drivers.NewConfigVersionManager("")
	cont.AuditService = services.NewAuditService(cont.Database)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID:        "rb-1",
		Name:            "versioned device",
		Protocol:        "simulator",
		Status:          "online",
		CollectInterval: 1000,
		Config:          map[string]interface{}{"amplitude": 10},
		CreatedBy:       "u1",
	})
	return cont
}

// The config-version endpoints used to answer from nothing: rollback reported
// the version numbers the caller asked for without touching the device, diff
// returned an empty object and the audit trail was hardcoded to []. Nothing
// called SnapshotDeviceConfig either, so no device ever had a version at all.
func TestDeviceConfigEditSavesAVersionThatRollbackReallyRestores(t *testing.T) {
	cont := useConfigVersionStore(t)

	code, env := callDeviceHandler(t, handleUpdateDevice, http.MethodPut, "/api/v1/devices/rb-1",
		`{"config":{"amplitude":99}}`, []string{"device_id"}, []string{"rb-1"})
	if code != http.StatusOK {
		t.Fatalf("device update returned %d (%v)", code, env)
	}

	code, env = callDeviceHandler(t, handleListDeviceConfigVersions, http.MethodGet,
		"/api/v1/devices/rb-1/config-versions", "", []string{"device_id"}, []string{"rb-1"})
	if code != http.StatusOK {
		t.Fatalf("list versions returned %d (%v)", code, env)
	}
	versions, _ := env["data"].([]interface{})
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want the edit to have saved one", env["data"])
	}
	v1, _ := versions[0].(map[string]interface{})
	if v1["version"] != float64(1) {
		t.Fatalf("first version = %v, want 1", v1["version"])
	}
	changed, _ := v1["changed_keys"].([]interface{})
	if len(changed) != 1 || changed[0] != "amplitude" {
		t.Fatalf("changed_keys = %v, want the edited key only", v1["changed_keys"])
	}

	// Roll it back: the device must end up on the config version 1 replaced.
	code, env = callDeviceHandler(t, handleRollbackDeviceConfig, http.MethodPost,
		"/api/v1/devices/rb-1/config-versions/rollback", `{"target_version":1}`,
		[]string{"device_id"}, []string{"rb-1"})
	if code != http.StatusOK {
		t.Fatalf("rollback returned %d (%v)", code, env)
	}
	data, _ := env["data"].(map[string]interface{})
	if data["rolled_back_to"] != float64(1) {
		t.Fatalf("rollback response = %v, want rolled_back_to 1", data)
	}

	device, err := cont.DeviceService.Get("rb-1")
	if err != nil || device == nil {
		t.Fatalf("Get: %v (%v)", err, device)
	}
	if got := device.Config["amplitude"]; got != float64(10) {
		t.Fatalf("config after rollback = %v, want the saved version 1 value 10 (the row was never written before)", got)
	}

	// The rollback snapshotted what it replaced, so it is itself undoable.
	versionsNow := cont.ConfigVersionMgr.ListVersions("rb-1", 20, 0)
	if len(versionsNow) != 2 {
		t.Fatalf("versions after rollback = %d, want the replaced config kept as a second version", len(versionsNow))
	}

	// A version that was never saved cannot be rolled back to.
	code, env = callDeviceHandler(t, handleRollbackDeviceConfig, http.MethodPost,
		"/api/v1/devices/rb-1/config-versions/rollback", `{"target_version":77}`,
		[]string{"device_id"}, []string{"rb-1"})
	if code != http.StatusNotFound {
		t.Fatalf("rollback to an unsaved version returned %d (%v), want 404", code, env)
	}
	if env["error_code"] != "ERR_CONFIG_VERSION_NOT_FOUND" {
		t.Fatalf("error_code = %v, want ERR_CONFIG_VERSION_NOT_FOUND", env["error_code"])
	}
	if device, _ = cont.DeviceService.Get("rb-1"); device.Config["amplitude"] != float64(10) {
		t.Fatalf("the rejected rollback changed the config to %v", device.Config["amplitude"])
	}

	// diff reports the keys that actually differ between two saved versions.
	code, env = callDeviceHandler(t, handleDiffDeviceConfigVersions, http.MethodGet,
		"/api/v1/devices/rb-1/config-versions/diff?version_a=1&version_b=2", "",
		[]string{"device_id"}, []string{"rb-1"})
	if code != http.StatusOK {
		t.Fatalf("diff returned %d (%v)", code, env)
	}
	diff, _ := env["data"].(map[string]interface{})
	if changes, _ := diff["changes"].([]interface{}); len(changes) != 1 || changes[0] != "amplitude" {
		t.Fatalf("diff changes = %v, want amplitude", diff["changes"])
	}

	// A missing version is not an empty diff.
	code, env = callDeviceHandler(t, handleDiffDeviceConfigVersions, http.MethodGet,
		"/api/v1/devices/rb-1/config-versions/diff?version_a=1&version_b=9", "",
		[]string{"device_id"}, []string{"rb-1"})
	if code != http.StatusNotFound {
		t.Fatalf("diff of an unsaved version returned %d (%v), want 404", code, env)
	}

	// The audit trail reads the recorded entries for this device, so an operator
	// can see who changed the config and who rolled it back.
	code, env = callDeviceHandler(t, handleGetDeviceConfigAuditTrail, http.MethodGet,
		"/api/v1/devices/rb-1/config-versions/audit-trail", "",
		[]string{"device_id"}, []string{"rb-1"})
	if code != http.StatusOK {
		t.Fatalf("audit trail returned %d (%v)", code, env)
	}
	entries, _ := env["data"].([]interface{})
	seen := map[string]bool{}
	for _, e := range entries {
		entry, _ := e.(map[string]interface{})
		if entry["resource_id"] != "rb-1" {
			t.Fatalf("audit trail leaked another resource: %v", entry)
		}
		seen[entry["action"].(string)] = true
	}
	if !seen["device_update"] || !seen["device_config_rollback"] {
		t.Fatalf("audit trail actions = %v, want the update and the rollback recorded", seen)
	}
}

// Versioning is not wired on every deployment, and a caller that asked for a
// rollback had to be told so rather than being handed version numbers.
func TestDeviceConfigRollbackWithoutVersioningIsHonest(t *testing.T) {
	cont := withDeviceService(t)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "rb-2",
		Name:     "device",
		Protocol: "simulator",
		Status:   "online",
		Config:   map[string]interface{}{"amplitude": 1},
	})

	code, env := callDeviceHandler(t, handleRollbackDeviceConfig, http.MethodPost,
		"/api/v1/devices/rb-2/config-versions/rollback", `{"target_version":1}`,
		[]string{"device_id"}, []string{"rb-2"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("rollback without a version manager returned %d (%v), want 503", code, env)
	}
	if env["error_code"] != "ERR_CONFIG_VERSIONING_UNAVAILABLE" {
		t.Fatalf("error_code = %v, want ERR_CONFIG_VERSIONING_UNAVAILABLE", env["error_code"])
	}
}

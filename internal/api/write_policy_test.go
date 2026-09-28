package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"edgelite/internal/models"
	"edgelite/internal/services"
)

// PUT /devices/:id/write-policy stores write_verify, write_rate_limit and
// write_whitelist, and the device editor exposes the first two as switches and
// numbers. Until the service enforced them, an operator who throttled or asked
// for read-back verification got a saved setting and a device that was neither
// throttled nor verified. These tests pin the enforcement to the HTTP write path.

func policyTestEnv(t *testing.T, deviceID string, config map[string]interface{}) *ServiceContainer {
	t.Helper()
	cont := withDeviceService(t)
	cont.AuditService = services.NewAuditService(cont.Database)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: deviceID,
		Name:     deviceID,
		Protocol: "simulator",
		Config:   config,
		Points: []models.PointDef{
			{Name: "setpoint", DataType: "float32", Address: "0", AccessMode: "rw"},
			{Name: "other", DataType: "float32", Address: "1", AccessMode: "rw"},
		},
	})
	return cont
}

// writePointAs performs the write as the given user and returns the status code
// and the error envelope, so the tests assert what the operator sees.
func writePointAs(t *testing.T, deviceID, body string, user *UserContext) (int, string, string) {
	t.Helper()
	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/devices/"+deviceID+"/points", body)
	if user != nil {
		c.Set("user", user)
	}
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := handleWriteDevicePoint(c); err != nil {
		t.Fatalf("handleWriteDevicePoint returned an error: %v", err)
	}
	var env struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env.ErrorCode, env.Message
}

var operator = &UserContext{UserID: "u2", Username: "plant-ops", Role: "operator"}

func TestWriteVerifyAcceptsEchoedValue(t *testing.T) {
	policyTestEnv(t, "verify-ok", map[string]interface{}{
		"write_verify":       true,
		"write_hold_seconds": 60,
	})

	code, errCode, msg := writePointAs(t, "verify-ok", `{"point":"setpoint","value":42.5}`, operator)
	if code != http.StatusOK {
		t.Fatalf("verified write returned %d (%s %s), want 200", code, errCode, msg)
	}
	_, rows, _ := callWriteAudit(t, "verify-ok", "")
	if len(rows) != 1 || rows[0]["result"] != "success" {
		t.Fatalf("audit rows = %v, want one success", rows)
	}
}

func TestWriteVerifyReportsUnconfirmedWrite(t *testing.T) {
	// fault_simulation=timeout makes every read fail, so the device can never
	// confirm the write. The answer has to be a failure, not a success with the
	// write silently unverified.
	policyTestEnv(t, "verify-bad", map[string]interface{}{
		"write_verify":     true,
		"fault_simulation": "timeout",
		"fault_rate":       100,
		"timeout":          0.01,
	})

	code, errCode, _ := writePointAs(t, "verify-bad", `{"point":"setpoint","value":7}`, operator)
	if code != http.StatusBadGateway {
		t.Fatalf("unconfirmed write returned %d (%s), want 502", code, errCode)
	}
	if errCode != "ERR_WRITE_VERIFY_FAILED" {
		t.Errorf("error_code = %q, want ERR_WRITE_VERIFY_FAILED", errCode)
	}
	_, rows, _ := callWriteAudit(t, "verify-bad", "")
	if len(rows) != 1 || rows[0]["result"] != "failed" {
		t.Fatalf("audit rows = %v, want one failed row", rows)
	}
	if e, _ := rows[0]["error"].(string); e == "" {
		t.Error("failed verification left no reason in the audit trail")
	}
}

// Without write_verify the gateway reports what it did (the write was sent) and
// does not spend a read on confirmation.
func TestWriteVerifyOffSkipsReadBack(t *testing.T) {
	policyTestEnv(t, "verify-off", map[string]interface{}{
		"fault_simulation": "timeout",
		"fault_rate":       100,
		"timeout":          0.01,
	})

	code, errCode, msg := writePointAs(t, "verify-off", `{"point":"setpoint","value":7}`, operator)
	if code != http.StatusOK {
		t.Fatalf("write returned %d (%s %s), want 200 without verification", code, errCode, msg)
	}
}

func TestWriteRateLimitThrottlesSamePointOnly(t *testing.T) {
	policyTestEnv(t, "throttled", map[string]interface{}{"write_rate_limit": 60000})

	if code, errCode, _ := writePointAs(t, "throttled", `{"point":"setpoint","value":1}`, operator); code != http.StatusOK {
		t.Fatalf("first write returned %d (%s), want 200", code, errCode)
	}
	code, errCode, msg := writePointAs(t, "throttled", `{"point":"setpoint","value":2}`, operator)
	if code != http.StatusTooManyRequests {
		t.Fatalf("second write returned %d (%s), want 429", code, errCode)
	}
	if errCode != "ERR_WRITE_RATE_LIMITED" {
		t.Errorf("error_code = %q, want ERR_WRITE_RATE_LIMITED", errCode)
	}
	// The message carries the wait, which is the difference between a retryable
	// hint and a dead end for the operator.
	if msg == "" || msg == "ERR_WRITE_RATE_LIMITED" {
		t.Errorf("message = %q, want the remaining interval", msg)
	}
	// A different point has its own budget.
	if code, errCode, _ := writePointAs(t, "throttled", `{"point":"other","value":3}`, operator); code != http.StatusOK {
		t.Fatalf("write to another point returned %d (%s), want 200", code, errCode)
	}

	_, rows, _ := callWriteAudit(t, "throttled", "")
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want 3 (two accepted writes and the throttled one)", len(rows))
	}
	statuses := make([]string, 0, len(rows))
	for _, r := range rows {
		s, _ := r["result"].(string)
		statuses = append(statuses, s)
	}
	// Newest first, so the throttled attempt sits between the two successes.
	if statuses[0] != "success" || statuses[1] != "failed" || statuses[2] != "success" {
		t.Errorf("audit results = %v, want success/failed/success", statuses)
	}
}

func TestWriteRateLimitDisabledByDefault(t *testing.T) {
	policyTestEnv(t, "unthrottled", map[string]interface{}{"write_rate_limit": 0})

	for i := 0; i < 3; i++ {
		if code, errCode, _ := writePointAs(t, "unthrottled", `{"point":"setpoint","value":1}`, operator); code != http.StatusOK {
			t.Fatalf("write %d returned %d (%s), want 200 with the throttle off", i+1, code, errCode)
		}
	}
}

func TestWriteWhitelistFiltersByUser(t *testing.T) {
	policyTestEnv(t, "walled", map[string]interface{}{"write_whitelist": []interface{}{"plant-ops"}})

	if code, errCode, _ := writePointAs(t, "walled", `{"point":"setpoint","value":1}`, nil); code != http.StatusOK {
		t.Fatalf("admin write returned %d (%s), want 200: admins are not gated by the list", code, errCode)
	}
	if code, _, _ := writePointAs(t, "walled", `{"point":"setpoint","value":2}`, operator); code != http.StatusOK {
		t.Fatalf("whitelisted operator write returned %d, want 200", code)
	}
	code, errCode, _ := writePointAs(t, "walled", `{"point":"setpoint","value":3}`,
		&UserContext{UserID: "u3", Username: "contractor", Role: "operator"})
	if code != http.StatusForbidden {
		t.Fatalf("non-whitelisted write returned %d, want 403", code)
	}
	if errCode != "ERR_WRITE_NOT_WHITELISTED" {
		t.Errorf("error_code = %q, want ERR_WRITE_NOT_WHITELISTED", errCode)
	}
}

func TestWriteWhitelistAcceptsCommaSeparatedString(t *testing.T) {
	// A hand-authored config.yaml stores the list as a string, and a whitelist
	// that silently failed to parse would deny every write.
	policyTestEnv(t, "walled-str", map[string]interface{}{"write_whitelist": "contractor, plant-ops"})

	if code, _, _ := writePointAs(t, "walled-str", `{"point":"setpoint","value":1}`, operator); code != http.StatusOK {
		t.Fatalf("write returned %d, want 200 for a name in the comma separated list", code)
	}
}

func TestWriteWhitelistEmptyAllowsEveryone(t *testing.T) {
	policyTestEnv(t, "open", map[string]interface{}{"write_whitelist": []interface{}{}})

	if code, errCode, _ := writePointAs(t, "open", `{"point":"setpoint","value":1}`,
		&UserContext{UserID: "u9", Username: "anyone", Role: "viewer"}); code != http.StatusOK {
		t.Fatalf("write returned %d (%s), want 200 with an empty whitelist", code, errCode)
	}
}

// The RPC command endpoint writes a point through the same service, so the
// policy has to hold there too rather than only on the point-write route.
func TestRPCCommandHonoursRateLimit(t *testing.T) {
	policyTestEnv(t, "rpc-throttled", map[string]interface{}{"write_rate_limit": 60000})

	if code, _, _ := writePointAs(t, "rpc-throttled", `{"point":"setpoint","value":1}`, operator); code != http.StatusOK {
		t.Fatalf("setup write returned %d, want 200", code)
	}

	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/integration/rpc/execute",
		`{"device_id":"rpc-throttled","method":"setpoint","params":{"value":2}}`)
	if err := handleRpcExecute(c); err != nil {
		t.Fatalf("handleRpcExecute returned an error: %v", err)
	}
	var env struct {
		Data struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable RPC response %q: %v", rec.Body.String(), err)
	}
	if env.Data.Success {
		t.Fatalf("RPC write succeeded inside the rate limit window: %v", env.Data)
	}
	if env.Data.Error == "" {
		t.Error("RPC reported a failed write with no reason")
	}
}

// updateDeviceAs performs PUT /devices/:id as the given user and returns the
// status code plus the error envelope, mirroring what the device editor sends.
func updateDeviceAs(t *testing.T, deviceID, body string, user *UserContext) (int, string) {
	t.Helper()
	c, rec := setupWithAdmin(http.MethodPut, "/api/v1/devices/"+deviceID, body)
	c.Set("user", user)
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := handleUpdateDevice(c); err != nil {
		t.Fatalf("handleUpdateDevice returned an error: %v", err)
	}
	var env struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env.ErrorCode
}

// deviceOwnerOperator is an operator that created the seeded devices, so the
// resource-ownership check passes and only the write-policy permission is left
// under test.
var deviceOwnerOperator = &UserContext{UserID: "tester", Username: "plant-ops", Role: "operator"}

// The write policy lives inside device.config, and the device editor saves the
// whole config, so the ADMIN-only write-policy route was only half a gate: a
// role with device:update could clear write_whitelist by editing the device.
func TestDeviceUpdateRejectsWritePolicyChangeByNonAdmin(t *testing.T) {
	const owner = "walled-by-update"
	policyTestEnv(t, owner, map[string]interface{}{"write_whitelist": []interface{}{"plant-ops"}})

	code, errCode := updateDeviceAs(t, owner, `{"config":{"write_whitelist":[]}}`, deviceOwnerOperator)
	if code != http.StatusForbidden {
		t.Fatalf("config-only whitelist clear returned %d (%s), want 403", code, errCode)
	}
	if errCode != "ERR_WRITE_POLICY_FORBIDDEN" {
		t.Errorf("error_code = %q, want ERR_WRITE_POLICY_FORBIDDEN", errCode)
	}

	stored, err := GetContainer().DeviceRepo.Get(owner)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
	if got := writePolicyValue(stored.Config["write_whitelist"]); got != "plant-ops" {
		t.Fatalf("stored write_whitelist = %s, want the rejected update to leave it intact", got)
	}
	// The policy still bites: the operator who was refused is still allowed, an
	// outsider is still not.
	if code, errCode, _ := writePointAs(t, owner, `{"point":"setpoint","value":1}`, deviceOwnerOperator); code != http.StatusOK {
		t.Fatalf("whitelisted write returned %d (%s), want 200", code, errCode)
	}
	if code, errCode, _ := writePointAs(t, owner, `{"point":"setpoint","value":2}`,
		&UserContext{UserID: "u7", Username: "outsider", Role: "operator"}); code != http.StatusForbidden {
		t.Fatalf("non-whitelisted write returned %d (%s), want 403", code, errCode)
	}
}

// A normal device edit has to keep working for operators: the editor sends the
// whole config back, so echoing the stored policy must not look like a change.
func TestDeviceUpdateAllowsNonPolicyChangesByOperator(t *testing.T) {
	const dev = "edited-by-operator"
	policyTestEnv(t, dev, map[string]interface{}{
		"write_verify":     true,
		"write_rate_limit": 500,
		"write_whitelist":  "plant-ops, contractor",
	})

	// Same values, different spellings: a list for the comma-separated string and
	// a float for the stored int, because JSON gives back float64.
	code, errCode := updateDeviceAs(t, dev, `{"name":"renamed","config":{"write_verify":true,"write_rate_limit":500,"write_whitelist":["plant-ops","contractor"]}}`, deviceOwnerOperator)
	if code != http.StatusOK {
		t.Fatalf("policy-echoing update returned %d (%s), want 200", code, errCode)
	}
	stored, err := GetContainer().DeviceRepo.Get(dev)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
	if stored.Name != "renamed" {
		t.Errorf("name = %q, want the update to apply", stored.Name)
	}
	if enabled, _ := stored.Config["write_verify"].(bool); !enabled {
		t.Error("write_verify was dropped by an update that did not change it")
	}
}

func TestDeviceUpdateAllowsWritePolicyForAdmin(t *testing.T) {
	const dev = "admin-policy"
	policyTestEnv(t, dev, map[string]interface{}{"write_rate_limit": 60000})

	code, errCode := updateDeviceAs(t, dev, `{"config":{"write_rate_limit":0}}`,
		&UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	if code != http.StatusOK {
		t.Fatalf("admin policy update returned %d (%s), want 200", code, errCode)
	}
	stored, err := GetContainer().DeviceRepo.Get(dev)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
	if limit, _ := stored.Config["write_rate_limit"].(float64); limit != 0 {
		t.Errorf("stored write_rate_limit = %v, want 0", stored.Config["write_rate_limit"])
	}
	// The throttle is gone, so two writes to the same point both pass.
	if code, errCode, _ := writePointAs(t, dev, `{"point":"setpoint","value":1}`, operator); code != http.StatusOK {
		t.Fatalf("write returned %d (%s), want 200 after the throttle was lifted", code, errCode)
	}
	if code, errCode, _ := writePointAs(t, dev, `{"point":"setpoint","value":2}`, operator); code != http.StatusOK {
		t.Fatalf("second write returned %d (%s), want 200", code, errCode)
	}
}

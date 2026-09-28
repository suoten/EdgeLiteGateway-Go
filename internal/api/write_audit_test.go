package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"edgelite/internal/models"
	"edgelite/internal/services"
)

// GET /devices/:id/write-audit used to answer with a hardcoded empty list, which
// is indistinguishable from "nobody has ever written to this device". These tests
// pin the real chain: a write leaves an audit row, and the endpoint reports it.

func auditTestEnv(t *testing.T) *ServiceContainer {
	t.Helper()
	cont := withDeviceService(t)
	// withDeviceService builds the container cmd/edgelite wires, and main.go
	// always installs an AuditService over the same database.
	cont.AuditService = services.NewAuditService(cont.Database)

	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "audit-dev",
		Name:     "Audit Device",
		Protocol: "simulator",
		Config:   map[string]interface{}{"timeout": 5},
		Points: []models.PointDef{
			{Name: "setpoint", DataType: "float32", Address: "0", AccessMode: "rw"},
			{Name: "locked", DataType: "float32", Address: "1", AccessMode: "read"},
		},
	})
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "audit-dev-quiet",
		Name:     "Audit Device With Auditing Off",
		Protocol: "simulator",
		Config:   map[string]interface{}{"timeout": 5, "write_audit": false},
		Points:   []models.PointDef{{Name: "setpoint", DataType: "float32", Address: "0", AccessMode: "rw"}},
	})
	return cont
}

func callWriteDevicePoint(t *testing.T, deviceID, body string) int {
	t.Helper()
	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/devices/"+deviceID+"/points", body)
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := handleWriteDevicePoint(c); err != nil {
		t.Fatalf("handleWriteDevicePoint returned an error: %v", err)
	}
	return rec.Code
}

func callWriteAudit(t *testing.T, deviceID, query string) (int, []map[string]interface{}, string) {
	t.Helper()
	c, rec := setupWithAdmin(http.MethodGet, "/api/v1/devices/"+deviceID+"/write-audit"+query, "")
	c.SetParamNames("device_id")
	c.SetParamValues(deviceID)
	if err := handleGetWriteAudit(c); err != nil {
		t.Fatalf("handleGetWriteAudit returned an error: %v", err)
	}
	var env struct {
		Data       []map[string]interface{} `json:"data"`
		ErrorCode  string                   `json:"error_code"`
		Error      string                   `json:"error"`
		ErrorMsg   string                   `json:"message"`
	}
	body := rec.Body.String()
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unparseable write-audit response %q: %v", body, err)
	}
	code := env.ErrorCode
	if code == "" {
		code = env.Error
	}
	if code == "" {
		code = env.ErrorMsg
	}
	return rec.Code, env.Data, code
}

func TestDeviceWritePointIsAudited(t *testing.T) {
	auditTestEnv(t)

	if code := callWriteDevicePoint(t, "audit-dev", `{"point":"setpoint","value":42.5}`); code != http.StatusOK {
		t.Fatalf("writable point returned %d, want 200", code)
	}
	_, rows, _ := callWriteAudit(t, "audit-dev", "")
	if len(rows) != 1 {
		t.Fatalf("write-audit returned %d rows, want 1: %v", len(rows), rows)
	}
	row := rows[0]
	if row["point"] != "setpoint" {
		t.Errorf("row point = %v, want setpoint", row["point"])
	}
	if v, ok := row["value"].(float64); !ok || v != 42.5 {
		t.Errorf("row value = %v, want 42.5", row["value"])
	}
	if row["result"] != "success" {
		t.Errorf("row result = %v, want success", row["result"])
	}
	if row["username"] != "admin" {
		t.Errorf("row username = %v, want the acting user admin", row["username"])
	}
	if ts, _ := row["timestamp"].(string); ts == "" {
		t.Errorf("row has no timestamp: %v", row)
	}
}

// A rejected write is the row an auditor cares about most, so it has to be in the
// trail too, with the reason attached.
func TestFailedDeviceWriteIsAuditedWithReason(t *testing.T) {
	auditTestEnv(t)

	code := callWriteDevicePoint(t, "audit-dev", `{"point":"locked","value":1.5}`)
	if code != http.StatusBadRequest {
		t.Fatalf("read-only point write returned %d, want 400", code)
	}
	_, rows, _ := callWriteAudit(t, "audit-dev", "")
	if len(rows) != 1 {
		t.Fatalf("failed write left %d audit rows, want 1", len(rows))
	}
	if rows[0]["result"] != "failed" {
		t.Fatalf("row result = %v, want failed", rows[0]["result"])
	}
	if e, _ := rows[0]["error"].(string); e == "" {
		t.Fatal("failed write recorded no reason")
	}

	// The result filter must not invent rows.
	_, ok, _ := callWriteAudit(t, "audit-dev", "?result=success")
	if len(ok) != 0 {
		t.Fatalf("result=success matched %d rows, want none", len(ok))
	}
	_, bad, _ := callWriteAudit(t, "audit-dev", "?result=failed")
	if len(bad) != 1 {
		t.Fatalf("result=failed matched %d rows, want 1", len(bad))
	}
}

// config.write_audit=false is the documented opt-out, so honouring it is what
// keeps the field from being decoration.
func TestWriteAuditFalseSuppressesRows(t *testing.T) {
	auditTestEnv(t)

	if code := callWriteDevicePoint(t, "audit-dev-quiet", `{"point":"setpoint","value":7}`); code != http.StatusOK {
		t.Fatalf("write returned %d, want 200", code)
	}
	_, rows, _ := callWriteAudit(t, "audit-dev-quiet", "")
	if len(rows) != 0 {
		t.Fatalf("device with write_audit=false recorded %d rows: %v", len(rows), rows)
	}
	// The other device still records: the opt-out is per device, not global.
	if code := callWriteDevicePoint(t, "audit-dev", `{"point":"setpoint","value":7}`); code != http.StatusOK {
		t.Fatalf("write returned %d, want 200", code)
	}
	_, rows2, _ := callWriteAudit(t, "audit-dev", "")
	if len(rows2) != 1 {
		t.Fatalf("audit-dev recorded %d rows, want 1", len(rows2))
	}
}

// The audit trail is per device: a write to one must never show up in another's
// history, which is what the resource_id filter in SQL guarantees.
func TestWriteAuditIsScopedToDevice(t *testing.T) {
	auditTestEnv(t)

	callWriteDevicePoint(t, "audit-dev", `{"point":"setpoint","value":1}`)
	callWriteDevicePoint(t, "audit-dev", `{"point":"setpoint","value":2}`)

	_, rows, _ := callWriteAudit(t, "audit-dev", "")
	if len(rows) != 2 {
		t.Fatalf("audit-dev has %d rows, want 2", len(rows))
	}
	// Newest first, so an operator scanning the top sees the last write.
	if a, _ := rows[0]["value"].(float64); a != 2 {
		t.Errorf("first row value = %v, want the newest write (2)", rows[0]["value"])
	}
	if b, _ := rows[1]["value"].(float64); b != 1 {
		t.Errorf("second row value = %v, want 1", rows[1]["value"])
	}
}

// Without an audit service the endpoint must say so instead of claiming the
// device has never been written to.
func TestWriteAuditWithoutServiceIsNotSilentlyEmpty(t *testing.T) {
	cont := auditTestEnv(t)
	if code := callWriteDevicePoint(t, "audit-dev", `{"point":"setpoint","value":3}`); code != http.StatusOK {
		t.Fatalf("write returned %d, want 200", code)
	}
	prev := cont.AuditService
	cont.AuditService = nil
	defer func() { cont.AuditService = prev }()

	code, rows, errCode := callWriteAudit(t, "audit-dev", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with %d rows, want 503 not an empty list", code, len(rows))
	}
	if errCode != "ERR_WRITE_AUDIT_UNAVAILABLE: audit service not ready" {
		t.Errorf("error = %q, want the audit-unavailable code", errCode)
	}
}

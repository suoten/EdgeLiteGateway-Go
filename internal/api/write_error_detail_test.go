package api

import (
	"net/http"
	"strings"
	"testing"

	"edgelite/internal/models"
	"edgelite/internal/services"
)

// When the device itself refuses a write, the response has to carry that reason.
// A bare code is not enough to act on: "the server denied the operation", "the
// address is out of range" and "the PLC is unreachable" all arrive here, and an
// operator who is shown only "写入设备测点失败" has to go read the gateway log to
// find out which one it was.

func TestDeviceWriteFailureCarriesTheDriverReason(t *testing.T) {
	cont := withDeviceService(t)
	cont.AuditService = services.NewAuditService(cont.Database)
	const dev = "write-refused"
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: dev,
		Name:     dev,
		Protocol: "modbus_tcp",
		// Nothing listens on port 1, so the driver's own connect error is what
		// comes back; the test looks for the endpoint in it, which only the
		// driver-side reason can contain.
		Config: map[string]interface{}{"host": "127.0.0.1", "port": 1, "timeout": 2},
		Points: []models.PointDef{
			{Name: "setpoint", DataType: "float32", Address: "40001", AccessMode: "rw"},
		},
	})

	code, errCode, msg := writePointAs(t, dev, `{"point":"setpoint","value":12.5}`, operator)
	if code != http.StatusBadRequest {
		t.Fatalf("rejected write returned %d (%s %s), want 400", code, errCode, msg)
	}
	if errCode != "ERR_DEVICE_WRITE_FAILED" {
		t.Errorf("error_code = %q, want ERR_DEVICE_WRITE_FAILED", errCode)
	}
	if msg == "" || msg == "ERR_DEVICE_WRITE_FAILED" || !strings.Contains(msg, "127.0.0.1") {
		t.Errorf("message = %q, want the driver's reason naming the endpoint it could not reach", msg)
	}

	_, rows, _ := callWriteAudit(t, dev, "")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %v, want the rejected write recorded once", rows)
	}
	if e, _ := rows[0]["error"].(string); e == "" {
		t.Error("the audit trail stored a failed write with no reason")
	}
}

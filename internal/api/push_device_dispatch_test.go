package api

import (
	"net/http"
	"strings"
	"testing"
)

// ProtoForge's integration client registers devices by POSTing the Python
// edition's contract {"device_id","name","protocol","config","points"} to
// /integration/push-device, while this build's northbound push posts
// {"device_id","data"} on the same path. handlePushDevice dispatches on the
// payload shape; these tests pin both halves of that contract.

func TestPushDeviceDispatchesRegistrationPayload(t *testing.T) {
	withDeviceService(t)

	status, code, _, body := callHandler(t, http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"joint-reg-dev","name":"Joint Reg","protocol":"modbus_tcp","collect_interval":3,
		  "config":{"host":"127.0.0.1","port":5020,"slave_id":1},
		  "points":[{"name":"temp","address":"100","data_type":"float32","access_mode":"rw"}]}`,
		handlePushDevice)
	if status != http.StatusCreated {
		t.Fatalf("registration answered %d/%s (%s)", status, code, string(body))
	}

	// A duplicate push must answer 409 so the client switches to its PUT path.
	status, code, _, _ = callHandler(t, http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"joint-reg-dev","name":"Joint Reg","protocol":"modbus_tcp","collect_interval":3,
		  "config":{"host":"127.0.0.1","port":5020,"slave_id":1},
		  "points":[{"name":"temp","address":"100","data_type":"float32","access_mode":"rw"}]}`,
		handlePushDevice)
	if status != http.StatusConflict || code != "ERR_DEVICE_ALREADY_EXISTS" {
		t.Fatalf("duplicate answered %d/%s, want 409 ERR_DEVICE_ALREADY_EXISTS", status, code)
	}

	// Validation failures answer 422 with per-field errors in "detail.errors".
	// The 422 carries its errors under "detail" (ProtoForge reads detail.errors),
	// which the callHandler envelope does not capture — read the raw body.
	c422, rec422 := setupEcho(http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"BAD ID","name":"","protocol":"modbus_tcp","points":[]}`)
	if err := handlePushDevice(c422); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec422.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid payload answered %d, want 422", rec422.Code)
	}
	bodyText := rec422.Body.String()
	for _, want := range []string{"device_id format invalid", "name must be", "points must be"} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("422 body missing %q: %s", want, bodyText)
		}
	}
}

func TestPushDeviceDispatchesNorthboundPayload(t *testing.T) {
	withDeviceService(t)

	// A body carrying only device_id+data must NOT create a device: it belongs
	// to the northbound forwarding half, which reports the platform manager's
	// own verdict (503 when nothing is connected).
	status, _, rawBody, _ := callHandler(t, http.MethodPost, "/api/v1/integration/push-device",
		`{"device_id":"joint-reg-dev","data":{"temperature":1}}`, handlePushDevice)
	if status == http.StatusCreated {
		t.Fatalf("northbound payload must not register a device: %s", string(rawBody))
	}
	devices, _, err := GetContainer().DeviceService.List(1, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, d := range devices {
		if d.DeviceID == "joint-reg-dev" {
			t.Fatalf("northbound payload created device %s", d.DeviceID)
		}
	}
}

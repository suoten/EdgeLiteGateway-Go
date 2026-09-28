package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Discovering on a protocol that has no discovery must answer 501 with a code the UI
// can translate. It used to answer 200 with an empty list (the device form then read
// as "scanned, nothing there") or 500 (it read as "the network is broken"), and
// neither of those is the truth.

func discoverAs(t *testing.T, body string) (int, string) {
	t.Helper()
	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/devices/discover", body)
	if err := handleDiscoverDevices(c); err != nil {
		t.Fatalf("handleDiscoverDevices returned an error: %v", err)
	}
	var env struct {
		Code      int    `json:"code"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env.ErrorCode
}

func TestDiscoverUnsupportedProtocolAnswers501(t *testing.T) {
	withDeviceService(t)

	cases := map[string]string{
		// No network to scan: the driver says so.
		"simulator":     `{"protocol":"simulator","config":{}}`,
		"siemens_s7":    `{"protocol":"siemens_s7","config":{"host":"127.0.0.1"}}`,
		"allen_bradley": `{"protocol":"allen_bradley","config":{"host":"127.0.0.1"}}`,
		// Requires the Windows COM registry this build does not link.
		"opc_da": `{"protocol":"opc_da","config":{"host":"127.0.0.1"}}`,
	}
	for protocol, body := range cases {
		code, errCode := discoverAs(t, body)
		if code != http.StatusNotImplemented {
			t.Fatalf("%s: status = %d (%s), want 501", protocol, code, errCode)
		}
		if errCode != "ERR_DRIVER_DISCOVER_UNSUPPORTED" {
			t.Fatalf("%s: error_code = %q, want ERR_DRIVER_DISCOVER_UNSUPPORTED", protocol, errCode)
		}
	}
}

func TestDriverDiscoverRouteAnswers501ForUnsupportedProtocol(t *testing.T) {
	withDeviceService(t)

	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/drivers/modbus_rtu/discover", `{"config":{}}`)
	c.SetParamNames("driver_name")
	c.SetParamValues("modbus_rtu")
	if err := handleDriverDiscover(c); err != nil {
		t.Fatalf("handleDriverDiscover returned an error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %q: %v", rec.Body.String(), err)
	}
	if env.ErrorCode != "ERR_DRIVER_DISCOVER_UNSUPPORTED" {
		t.Fatalf("error_code = %q, want ERR_DRIVER_DISCOVER_UNSUPPORTED", env.ErrorCode)
	}
}

// A protocol name no driver answers to is a mistake in the request, not a gateway
// fault. Both routes used to fail the lookup with 500 ERR_DEVICE_DISCOVER_FAILED,
// which is what an operator debugging a typo or an old bookmark gets.
func TestDiscoverWithAnUnknownProtocolAnswers400(t *testing.T) {
	withDeviceService(t)

	for _, body := range []string{`{"protocol":"totally_made_up","config":{}}`, `{"config":{}}`} {
		code, errCode := discoverAs(t, body)
		if code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, code)
		}
		if errCode != "ERR_DRIVER_NOT_AVAILABLE" {
			t.Fatalf("body %s: error_code = %q, want ERR_DRIVER_NOT_AVAILABLE", body, errCode)
		}
	}

	c, rec := setupWithAdmin(http.MethodPost, "/api/v1/drivers/totally_made_up/discover", `{"config":{}}`)
	c.SetParamNames("driver_name")
	c.SetParamValues("totally_made_up")
	if err := handleDriverDiscover(c); err != nil {
		t.Fatalf("handleDriverDiscover returned an error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("driver route: status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

// TestDiscoverProtocolWithoutDiscoveryStillReachesTheDriver pins the line between
// the two answers above: a protocol that exists but cannot scan must still get its
// 501, so the 400 branch cannot swallow every failure by rejecting too early.
func TestDiscoverProtocolWithoutDiscoveryStillReachesTheDriver(t *testing.T) {
	withDeviceService(t)

	if code, errCode := discoverAs(t, `{"protocol":"simulator","config":{}}`); code != http.StatusNotImplemented ||
		errCode != "ERR_DRIVER_DISCOVER_UNSUPPORTED" {
		t.Fatalf("simulator: status = %d code = %q, want 501 ERR_DRIVER_DISCOVER_UNSUPPORTED", code, errCode)
	}
}

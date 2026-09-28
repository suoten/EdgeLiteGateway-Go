package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"edgelite/internal/drivers"
)

// /drivers/meta is what the driver table and the device form render their
// "supported" badges and warning notes from. Two rounds of honesty fixes went
// into the tables behind it (capabilities derived from the driver methods,
// constraints and the experimental flag actually populated), so the response is
// pinned here: an over-claim in this payload reappears as a green badge in the UI.

type metaEntry struct {
	Name         string              `json:"name"`
	Capabilities map[string]bool     `json:"capabilities"`
	Experimental bool                `json:"experimental"`
	Constraints  []map[string]string `json:"constraints"`
}

func driverMeta(t *testing.T) map[string]metaEntry {
	t.Helper()
	drivers.RegisterAll() // cmd/edgelite does the same at startup
	c, rec := setupWithAdmin(http.MethodGet, "/api/v1/drivers/meta", "")
	if err := handleGetDriverMeta(c); err != nil {
		t.Fatalf("handleGetDriverMeta returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int         `json:"code"`
		Data []metaEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %q: %v", rec.Body.String(), err)
	}
	if len(env.Data) == 0 {
		t.Fatalf("no driver metadata returned: %s", rec.Body.String())
	}
	out := make(map[string]metaEntry, len(env.Data))
	for _, m := range env.Data {
		out[m.Name] = m
	}
	return out
}

func TestDriverMetaCapabilitiesMatchDrivers(t *testing.T) {
	meta := driverMeta(t)

	// A driver whose ReadPoints issues one request per point has no batch read,
	// and the UI used to badge batch_read/batch_write on six protocols anyway.
	for _, name := range []string{"modbus_tcp", "modbus_rtu", "siemens_s7", "mitsubishi_mc", "omron_fins", "allen_bradley"} {
		m, ok := meta[name]
		if !ok {
			t.Fatalf("%s missing from /drivers/meta", name)
		}
		if m.Capabilities["batch_read"] || m.Capabilities["batch_write"] {
			t.Errorf("%s capabilities = %v, must not claim batch read/write", name, m.Capabilities)
		}
		if !m.Capabilities["read"] || !m.Capabilities["write"] {
			t.Errorf("%s capabilities = %v, want read and write", name, m.Capabilities)
		}
	}

	// ONVIF implements no control command, so a write badge there advertises a
	// button that only ever produces an error toast.
	if got := meta["onvif"].Capabilities; got["write"] {
		t.Errorf("onvif capabilities = %v, want write=false (ErrONVIFWriteUnsupported)", got)
	}
	if got := meta["onvif"].Capabilities; !got["read"] || !got["discover"] {
		t.Errorf("onvif capabilities = %v, want read and discover", got)
	}

	// OPC DA links nothing: every operation is an explicit error, so it must not
	// be reported as able to read, write, subscribe or discover.
	if got := meta["opc_da"].Capabilities; len(got) > 0 {
		for k, v := range got {
			if v {
				t.Errorf("opc_da claims %s, but every operation returns ErrOPCDAUnsupported", k)
			}
		}
	}

	// Discovery is only reported where the driver does a real protocol probe.
	for _, name := range []string{"modbus_tcp", "opc_ua", "onvif"} {
		if !meta[name].Capabilities["discover"] {
			t.Errorf("%s must report discover", name)
		}
	}
	for _, name := range []string{"simulator", "siemens_s7", "allen_bradley", "modbus_rtu", "modbus_slave", "mqtt_client"} {
		if meta[name].Capabilities["discover"] {
			t.Errorf("%s must not report discover: its driver returns ErrDiscoveryUnsupported", name)
		}
	}
	// Subscriptions need MonitoredItems, which only the MQTT client has.
	if meta["opc_ua"].Capabilities["subscribe"] {
		t.Error("opc_ua must not report subscribe: points are polled per collect interval")
	}
	if !meta["mqtt_client"].Capabilities["subscribe"] {
		t.Error("mqtt_client subscribes and must report subscribe")
	}
}

func TestDriverMetaReportsConstraintsAndExperimental(t *testing.T) {
	meta := driverMeta(t)

	// The tables these come from used to be hardcoded false/[], so the schema
	// dialog could never show a warning for a driver that cannot do what its
	// fields imply.
	constraintCodes := func(name string) map[string]bool {
		got := map[string]bool{}
		for _, c := range meta[name].Constraints {
			got[c["code"]] = true
			if c["message"] == "" {
				t.Errorf("%s constraint %q has no fallback message", name, c["code"])
			}
		}
		return got
	}

	if codes := constraintCodes("onvif"); !codes["onvif_read_only"] {
		t.Errorf("onvif constraints = %v, want onvif_read_only", meta["onvif"].Constraints)
	}
	if codes := constraintCodes("opc_da"); !codes["opc_da_unsupported"] {
		t.Errorf("opc_da constraints = %v, want opc_da_unsupported", meta["opc_da"].Constraints)
	}
	if codes := constraintCodes("opc_ua"); !codes["opc_ua_polled"] {
		t.Errorf("opc_ua constraints = %v, want opc_ua_polled", meta["opc_ua"].Constraints)
	}

	if !meta["opc_da"].Experimental {
		t.Error("opc_da implements no operation in this build and must be marked experimental")
	}
	if meta["modbus_tcp"].Experimental {
		t.Error("modbus_tcp is a fully implemented driver and must not be marked experimental")
	}

	// A driver with no constraint must still answer with an array: the UI reads
	// constraints.length, and null turns that into a runtime type error.
	if got := meta["simulator"].Constraints; got == nil {
		t.Error("simulator constraints = null, want an empty array")
	}
}

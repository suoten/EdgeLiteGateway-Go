package api

// main.Version is stamped at link time and handed to SetGatewayVersion during
// startup, which is what /ota/check compares a release feed against. The other
// endpoints that name the running build carried their own copy of the default
// string, so a binary linked with -X main.Version=1.5.0 answered /ota/check with
// 1.5.0 and /system/status with 1.0.0-go from the same process - an operator
// could not tell which build was running, and an operator reading
// /system/health/basic got status "healthy" and uptime 0 no matter what the
// gateway was doing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/storage"
)

// withDatabaseOnly installs the slimmest container that can answer a health
// probe: a store and nothing else. The shared containers in this package wire a
// full repository stack, which these two tests do not need.
func withDatabaseOnly(t *testing.T) {
	t.Helper()
	restore := gatewayVersion
	t.Cleanup(func() { SetGatewayVersion(restore) })

	prev := GetContainer()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "version.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	cont := NewServiceContainer()
	cont.Database = db
	SetContainer(cont)
	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
}

// envelopeData unwraps the response envelope these three handlers write.
func envelopeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var env struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	if env.Code != 0 || env.Data == nil {
		t.Fatalf("response carries no data object: %s", rec.Body)
	}
	return env.Data
}

func TestInjectedVersionReachesEveryEndpoint(t *testing.T) {
	const stamped = "7.7.7-stamped"
	withDatabaseOnly(t)
	SetGatewayVersion(stamped)

	for _, probe := range []struct {
		label   string
		handler func(echo.Context) error
	}{
		{"/system/info", handleSystemInfo},
		{"/system/status", handleSystemStatus},
		{"/system/health/basic", handleGetHealthBasic},
	} {
		c, rec := setupWithAdmin("GET", "/api/v1"+probe.label, "")
		if err := probe.handler(c); err != nil {
			t.Fatalf("%s: %v", probe.label, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d, want 200", probe.label, rec.Code)
		}
		data := envelopeData(t, rec)
		if data["version"] != stamped {
			t.Errorf("%s reports version %#v, want the linked %s", probe.label, data["version"], stamped)
		}
	}
}

func TestHealthBasicReportsWhatItChecked(t *testing.T) {
	withDatabaseOnly(t)

	c, rec := setupWithAdmin("GET", "/api/v1/system/health/basic", "")
	if err := handleGetHealthBasic(c); err != nil {
		t.Fatalf("handleGetHealthBasic: %v", err)
	}
	data := envelopeData(t, rec)
	// The store is attached and reachable here, so the endpoint may say healthy -
	// and has to show the check it actually ran rather than the word alone.
	if data["status"] != "healthy" {
		t.Errorf("status = %#v with a working database, want healthy: %v", data["status"], data["checks"])
	}
	if checks, _ := data["checks"].(map[string]interface{}); checks["database"] != "ok" {
		t.Errorf("checks = %#v, want a database entry that says what was probed", data["checks"])
	}
	if uptime, _ := data["uptime_s"].(float64); uptime <= 0 {
		t.Errorf("uptime_s = %v, want the seconds this process has actually been up", data["uptime_s"])
	}

	// Without a store the same call must not keep claiming health.
	SetContainer(NewServiceContainer())
	c, rec = setupWithAdmin("GET", "/api/v1/system/health/basic", "")
	if err := handleGetHealthBasic(c); err != nil {
		t.Fatalf("handleGetHealthBasic without a database: %v", err)
	}
	data = envelopeData(t, rec)
	if data["status"] != "degraded" {
		t.Errorf("status = %#v with no database attached, want degraded", data["status"])
	}
}

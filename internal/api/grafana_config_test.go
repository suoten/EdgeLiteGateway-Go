package api

// PUT /grafana/config used to bind the body straight into GrafanaConfig and save
// it, and GET handed the api key out in the clear for the page to send back.
// Together those two meant: show the config, press save, and the stored
// credential became whatever the UI echoed - while the response still said
// success. The dedicated route is also the only one that persists Grafana
// settings at all; PUT /services/grafana/config (which the page used to call)
// writes nothing.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
)

func TestGrafanaConfigMasksKeyAndKeepsItOnRoundTrip(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.Grafana = config.GrafanaConfig{
		Enabled: true, URL: "http://grafana:3001", APIKey: "graf-real-key-1", Datasource: "InfluxDB",
	}

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/config", "")
	if err := handleGetGrafanaConfig(c); err != nil {
		t.Fatalf("GET handler error: %v", err)
	}
	if strings.Contains(rec.Body.String(), "graf-real-key-1") {
		t.Fatalf("GET /grafana/config leaked the plaintext api key: %s", rec.Body)
	}
	var got struct {
		Data struct {
			APIKey     string `json:"api_key"`
			Datasource string `json:"datasource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	if !strings.Contains(got.Data.APIKey, "***") {
		t.Errorf("api_key = %q, want a mask the page can send back unchanged", got.Data.APIKey)
	}

	// What the page posts after only changing the datasource: the mask it was
	// shown comes back as the credential.
	c, rec = setupWithAdmin(echo.PUT, "/api/v1/grafana/config",
		`{"url":"http://grafana:3001","api_key":"g***1","datasource":"EdgeLite"}`)
	if err := handleUpdateGrafanaConfig(c); err != nil {
		t.Fatalf("PUT handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "graf-real-key-1") {
		t.Errorf("PUT response leaked the plaintext api key: %s", rec.Body)
	}
	saved := persistedConfig(t)
	if saved.Grafana.APIKey != "graf-real-key-1" {
		t.Fatalf("the mask was stored as the api key: %q", saved.Grafana.APIKey)
	}
	if saved.Grafana.Datasource != "EdgeLite" {
		t.Fatalf("the edit that was made did not land: %+v", saved.Grafana)
	}
	if !saved.Grafana.Enabled {
		t.Error("a body without the enabled flag switched the service off in the file")
	}

	// A key the operator did type has to replace the stored one.
	c, rec = setupWithAdmin(echo.PUT, "/api/v1/grafana/config",
		`{"url":"http://grafana:3001","api_key":"brand-new-key","datasource":"EdgeLite"}`)
	if err := handleUpdateGrafanaConfig(c); err != nil {
		t.Fatalf("PUT with a new key: %v", err)
	}
	if saved = persistedConfig(t); saved.Grafana.APIKey != "brand-new-key" {
		t.Fatalf("new api key not stored: %q", saved.Grafana.APIKey)
	}
}

// withGrafanaUpstream points grafana.url at a server this test owns and answers
// with whatever fn writes, so the three states that used to all look like "no
// dashboards" - working, key rejected, nothing listening - can each be checked.
func withGrafanaUpstream(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	withIsolatedConfig(t)
	server := httptest.NewServer(fn)
	cfg := config.GetConfig()
	cfg.Grafana = config.GrafanaConfig{
		Enabled: true, URL: server.URL, APIKey: "upstream-key-9", Datasource: "InfluxDB",
	}
	prevClient := grafanaUpstreamClient
	grafanaUpstreamClient = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { grafanaUpstreamClient = prevClient; server.Close() })
	return server
}

// decodeEnvelope returns the status, the data object and the error code.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (int, map[string]interface{}, string) {
	t.Helper()
	var env struct {
		Code      int                    `json:"code"`
		Data      map[string]interface{} `json:"data"`
		ErrorCode string                 `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data, env.ErrorCode
}

func TestGrafanaDashboardsAreReadFromGrafana(t *testing.T) {
	var gotPath, gotAuth string
	withGrafanaUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.String(), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		// Extra fields on purpose: only the ones the table uses may be forwarded.
		_, _ = w.Write([]byte(`[
			{"uid":"a1","title":"Link stats","type":"dash-db","uri":"db/link-stats","url":"/d/a1/link-stats","isStarred":true},
			{"uid":"b2","title":"DB pool","type":"dash-db","uri":"db/db-pool","url":"/d/b2/db-pool"}
		]`))
	})

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/dashboards", "")
	if err := handleListGrafanaDashboards(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || !strings.Contains(gotAuth, "upstream-key-9") {
		t.Errorf("Grafana was asked without the configured key: %q", gotAuth)
	}
	if !strings.Contains(gotPath, "/api/search") {
		t.Errorf("Grafana was asked for %q, not the dashboard search", gotPath)
	}
	code, data, errCode := decodeEnvelope(t, rec)
	if code != 200 {
		t.Fatalf("status = %d (%s), want 200", code, rec.Body)
	}
	// The page reads data.dashboards; a bare array made a healthy Grafana render
	// an empty table even once the handler started answering with real rows.
	raw, ok := data["dashboards"].([]interface{})
	if !ok || len(raw) != 2 {
		t.Fatalf("data.dashboards = %v, want the two rows Grafana returned (%s)", data["dashboards"], rec.Body)
	}
	first, _ := raw[0].(map[string]interface{})
	if first["title"] != "Link stats" || first["uid"] != "a1" || first["url"] != "/d/a1/link-stats" {
		t.Errorf("dashboard row lost its identity: %v", first)
	}
	if _, leaked := first["isStarred"]; leaked {
		t.Errorf("Grafana internals are passed through to the UI: %v", first)
	}
	if errCode != "" {
		t.Errorf("error_code on a success = %q", errCode)
	}
}

func TestGrafanaDashboardsReportWhatGrafanaSaid(t *testing.T) {
	withGrafanaUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid API key"}`))
	})

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/dashboards", "")
	if err := handleListGrafanaDashboards(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	code, _, errCode := decodeEnvelope(t, rec)
	if code != http.StatusBadGateway {
		t.Fatalf("a rejected key answered %d, want 502: %s", code, rec.Body)
	}
	if !strings.HasPrefix(errCode, "ERR_GRAFANA_KEY_REJECTED") {
		t.Fatalf("error_code = %q, want ERR_GRAFANA_KEY_REJECTED", errCode)
	}
	// The reason Grafana gave has to reach the operator; an empty list hid it.
	if !strings.Contains(rec.Body.String(), "Invalid API key") {
		t.Fatalf("response dropped Grafana's reason: %s", rec.Body)
	}
}

func TestGrafanaDashboardsReportAnUnreachableGrafana(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	// Reserve a port then release it, so the address is one nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a local port: %v", err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	cfg.Grafana = config.GrafanaConfig{Enabled: true, URL: "http://" + dead, APIKey: "upstream-key-9"}
	prevClient := grafanaUpstreamClient
	grafanaUpstreamClient = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { grafanaUpstreamClient = prevClient })

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/dashboards", "")
	if err := handleListGrafanaDashboards(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	code, _, errCode := decodeEnvelope(t, rec)
	if code != http.StatusBadGateway || !strings.HasPrefix(errCode, "ERR_GRAFANA_UNREACHABLE") {
		t.Fatalf("status = %d error_code = %q, want 502 ERR_GRAFANA_UNREACHABLE: %s", code, errCode, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), dead) {
		t.Fatalf("response should name the address it could not reach: %s", rec.Body)
	}
}

func TestGrafanaDashboardsSayWhenTheIntegrationIsOff(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.Grafana = config.GrafanaConfig{Enabled: false, URL: "http://grafana:3001", APIKey: "x"}

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/dashboards", "")
	if err := handleListGrafanaDashboards(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	code, _, errCode := decodeEnvelope(t, rec)
	if code != http.StatusServiceUnavailable || errCode != "ERR_GRAFANA_DISABLED" {
		t.Fatalf("status = %d error_code = %q, want 503 ERR_GRAFANA_DISABLED: %s", code, errCode, rec.Body)
	}
}

func TestGrafanaDashboardsRequireAnAddress(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.Grafana = config.GrafanaConfig{Enabled: true, URL: "   ", APIKey: "x"}

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/dashboards", "")
	if err := handleListGrafanaDashboards(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	code, _, errCode := decodeEnvelope(t, rec)
	if code != http.StatusBadGateway || !strings.HasPrefix(errCode, "ERR_GRAFANA_URL_MISSING") {
		t.Fatalf("status = %d error_code = %q, want ERR_GRAFANA_URL_MISSING: %s", code, errCode, rec.Body)
	}
}

func TestImportGrafanaDashboardForwardsAndAnswersAsGrafanaDoes(t *testing.T) {
	var forwarded map[string]interface{}
	var sawPath, sawKey string
	withGrafanaUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawKey = r.URL.Path, r.Header.Get("Authorization")
		forwarded = map[string]interface{}{}
		_ = json.NewDecoder(r.Body).Decode(&forwarded)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":41,"uid":"a1","status":"success","version":3}`))
	})

	body := `{"dashboard":{"title":"Link stats","panels":[]},"overwrite":true}`
	c, rec := setupWithAdmin(echo.POST, "/api/v1/grafana/dashboards/import", body)
	if err := handleImportGrafanaDashboard(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if sawPath != "/api/dashboards/db" {
		t.Fatalf("import went to %q, not Grafana's dashboard API", sawPath)
	}
	if !strings.Contains(sawKey, "upstream-key-9") {
		t.Fatalf("import reached Grafana unauthenticated: %q", sawKey)
	}
	sent, _ := json.Marshal(forwarded)
	if !strings.Contains(string(sent), "Link stats") || forwarded["overwrite"] != true {
		t.Fatalf("the body was not forwarded as given: %s", sent)
	}
	code, data, _ := decodeEnvelope(t, rec)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, rec.Body)
	}
	if data["uid"] != "a1" || fmt.Sprint(data["id"]) != "41" {
		t.Fatalf("Grafana's answer was not returned to the caller: %v", data)
	}

	// A body Grafana would refuse must not be answered with a success.
	c, rec = setupWithAdmin(echo.POST, "/api/v1/grafana/dashboards/import", `{"title":"no dashboard field"}`)
	if err := handleImportGrafanaDashboard(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if code, _, errCode := decodeEnvelope(t, rec); code != http.StatusBadRequest || !strings.Contains(errCode, "dashboard") {
		t.Fatalf("status = %d error_code = %q, want a 400 naming the dashboard field: %s", code, errCode, rec.Body)
	}
}

func TestGrafanaDatasourcesAreReadFromGrafana(t *testing.T) {
	withGrafanaUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"uid":"ds1","name":"EdgeLiteTSDB","type":"influxdb","url":"http://influx:8086","isDefault":true}]`))
	})

	c, rec := setupWithAdmin(echo.GET, "/api/v1/grafana/datasources", "")
	if err := handleListGrafanaDatasources(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// This route answers with an array in data, which is what it always did.
	var env struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(env.Data) != 1 || env.Data[0]["name"] != "EdgeLiteTSDB" || env.Data[0]["type"] != "influxdb" {
		t.Fatalf("datasources did not come through: %s", rec.Body)
	}
}

package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/northbound"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

func setupWithAdmin(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	c, rec := setupEcho(method, path, body)
	c.Set("user", &UserContext{
		UserID:   "u1",
		Username: "admin",
		Role:     "admin",
	})
	return c, rec
}

// withDeviceService installs a container whose DeviceService reads a fresh
// sqlite database, and restores the previous container when the test ends. The
// empty container every other test uses only reaches the handlers' "service not
// ready" branch, so endpoints that report real per-device data need this one.
// Drivers are registered here too: cmd/edgelite calls RegisterAll at startup, so
// a container without factories is not a configuration the gateway can run in.
func withDeviceService(t *testing.T) *ServiceContainer {
	t.Helper()

	prev := GetContainer()
	drivers.RegisterAll()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "devices.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}

	cont := NewServiceContainer()
	cont.Database = db
	cont.DeviceRepo = storage.NewDeviceRepo(db)
	cont.TemplateRepo = storage.NewTemplateRepo(db)
	// cmd/edgelite always wires a scheduler and a circuit-breaker registry into
	// DeviceService, and SetupDriver dereferences both, so the test container has
	// to carry them too. The scheduler is never started here, which keeps
	// RegisterCollector from spawning collect loops.
	cont.EventBus = engine.NewEventBus(10000)
	cont.Scheduler = engine.NewCollectScheduler(cont.EventBus, nil, nil, &config.SchedulerConfig{})
	cont.CBRegistry = engine.NewCircuitBreakerRegistry(cont.EventBus)
	cont.DeviceService = services.NewDeviceService(cont.DeviceRepo, cont.TemplateRepo, cont.Scheduler, cont.CBRegistry)
	SetContainer(cont)

	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
	return cont
}

// seedDevice stores a device row and instantiates its driver, which is what the
// production create path does. Without the driver the browse endpoint can only
// ever report "driver not found".
func seedDevice(t *testing.T, cont *ServiceContainer, device *models.DeviceResponse) {
	t.Helper()
	if err := cont.DeviceRepo.Create(device, "tester"); err != nil {
		t.Fatalf("failed to seed device: %v", err)
	}
	if err := cont.DeviceService.SetupDriver(device); err != nil {
		t.Fatalf("failed to set up driver for seeded device: %v", err)
	}
}

// writeSelfSignedCert writes a PEM certificate valid until notAfter so the
// certificate-status endpoint has a real file to parse.
func writeSelfSignedCert(t *testing.T, commonName string, notAfter time.Time) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		Issuer:       pkix.Name{CommonName: "edgelite-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), commonName+".pem")
	raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("failed to write certificate: %v", err)
	}
	return path
}

// --- Profiler API Tests ---

func TestHandleGetProfilerStats(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/profiler/stats", "")
	if err := handleGetProfilerStats(c); err != nil {
		t.Fatalf("handleGetProfilerStats error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetSlowestRequests(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/profiler/slowest", "")
	if err := handleGetSlowestRequests(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetProfilerMemory(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/profiler/memory", "")
	if err := handleGetProfilerMemory(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetProfilerRequests(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/profiler/requests", "")
	if err := handleGetProfilerRequests(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleEnableProfiler(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/profiler/enable", "")
	if err := handleEnableProfiler(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleDisableProfiler(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/profiler/disable", "")
	if err := handleDisableProfiler(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleResetProfiler(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/profiler/reset", "")
	if err := handleResetProfiler(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleExportProfiler(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/profiler/export", "")
	if err := handleExportProfiler(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// --- Debug API Tests ---

// TestHandleGetDebugProtocols pins the contract the hand-written list violated:
// every protocol the debug page offers must be one CreateDriver accepts.
func TestHandleGetDebugProtocols(t *testing.T) {
	// cmd/edgelite calls RegisterAll before serving, so the registry is never
	// empty in a running gateway; this test has to install it explicitly.
	drivers.RegisterAll()
	c, rec := setupWithAdmin("GET", "/api/v1/debug/protocols", "")
	if err := handleGetDebugProtocols(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var payload struct {
		Data struct {
			Protocols []struct {
				Name string `json:"name"`
			} `json:"protocols"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %s is not the envelope: %v", rec.Body.String(), err)
	}
	if len(payload.Data.Protocols) == 0 {
		t.Fatalf("registry-backed protocol list is empty: %s", rec.Body.String())
	}
	for _, p := range payload.Data.Protocols {
		if !drivers.GetRegistry().IsSupported(p.Name) {
			t.Fatalf("debug protocols advertise %q, which no driver factory backs", p.Name)
		}
	}
}

// TestDebugPlaceholderRoutesRefuse guards the safety-relevant ones: /debug/write
// used to answer {success:true, written:1} without opening a connection, so a
// failed probe was indistinguishable from a PLC register that really changed.
func TestDebugPlaceholderRoutesRefuse(t *testing.T) {
	cases := []struct {
		path string
		call echo.HandlerFunc
		code string
	}{
		{"/api/v1/debug/simulate", handleDebugSimulate, "ERR_DEBUG_SIMULATE_UNSUPPORTED"},
		{"/api/v1/debug/read", handleDebugRead, "ERR_DEBUG_READ_UNSUPPORTED"},
		{"/api/v1/debug/write", handleDebugWrite, "ERR_DEBUG_WRITE_UNSUPPORTED"},
	}
	for _, tc := range cases {
		for _, body := range []string{`{"address":"0x01","value":1}`, `badjson`} {
			c, rec := setupWithAdmin("POST", tc.path, body)
			if err := tc.call(c); err != nil {
				t.Fatalf("%s returned error: %v", tc.path, err)
			}
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("%s: expected 501, got %d (%s)", tc.path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("%s: body %s does not carry %s", tc.path, rec.Body.String(), tc.code)
			}
			if strings.Contains(rec.Body.String(), `"success":true`) {
				t.Fatalf("%s still reports a success it did not perform: %s", tc.path, rec.Body.String())
			}
		}
	}
}

// TestHandleGetDebugPacketsStatesNoCapture: the list is legitimately empty
// because nothing records frames, and the tab has to be able to say so.
func TestHandleGetDebugPacketsStatesNoCapture(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/debug/packets?device_id=d1&limit=200", "")
	if err := handleGetDebugPackets(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var payload struct {
		Data struct {
			Packets        []interface{} `json:"packets"`
			CaptureEnabled *bool         `json:"capture_enabled"`
			Message        string        `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %s is not the envelope: %v", rec.Body.String(), err)
	}
	if payload.Data.CaptureEnabled == nil || *payload.Data.CaptureEnabled {
		t.Fatalf("packets must advertise capture_enabled:false, got %s", rec.Body.String())
	}
	if payload.Data.Message == "" {
		t.Fatalf("empty packet list needs an explanation the UI can render: %s", rec.Body.String())
	}
}

func TestHandleDeleteDebugPackets(t *testing.T) {
	c, rec := setupWithAdmin("DELETE", "/api/v1/debug/packets", "")
	if err := handleDeleteDebugPackets(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"cleared":0`) {
		t.Fatalf("must report that nothing was cleared: %s", rec.Body.String())
	}
}

// TestHandleGetDebugDevicesWithoutRepo: a missing repository used to answer an
// empty device list, which looks identical to a gateway with no devices.
func TestHandleGetDebugDevicesWithoutRepo(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/debug/devices", "")
	if err := handleGetDebugDevices(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleGetDebugDevicesFiltersByProtocol: the protocol query parameter used
// to be read and thrown away, so a modbus probe page listed the OPC UA devices.
func TestHandleGetDebugDevicesFiltersByProtocol(t *testing.T) {
	cont := withDeviceService(t)
	for _, d := range []*models.DeviceResponse{
		{DeviceID: "dbg-mod-1", Name: "modbus one", Protocol: "modbus_tcp", Status: "offline", Config: map[string]interface{}{"host": "127.0.0.1", "port": 5020}},
		{DeviceID: "dbg-opc-1", Name: "opcua one", Protocol: "opc_ua", Status: "offline", Config: map[string]interface{}{"endpoint": "opc.tcp://127.0.0.1:1"}},
	} {
		if err := cont.DeviceRepo.Create(d, "tester"); err != nil {
			t.Fatalf("failed to seed device: %v", err)
		}
	}

	c, rec := setupWithAdmin("GET", "/api/v1/debug/devices?protocol=modbus_tcp", "")
	if err := handleGetDebugDevices(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var payload struct {
		Data struct {
			Devices []models.DeviceResponse `json:"devices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %s is not the envelope: %v", rec.Body.String(), err)
	}
	if len(payload.Data.Devices) != 1 || payload.Data.Devices[0].DeviceID != "dbg-mod-1" {
		t.Fatalf("protocol filter returned the wrong devices: %s", rec.Body.String())
	}
}

// --- System Extended API Tests ---

func TestHandleGetSystemResources(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/resources", "")
	if err := handleGetSystemResources(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetBackupSchedule(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/backup/schedule", "")
	if err := handleGetBackupSchedule(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetCascadeTopology(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/cascade/topology", "")
	if err := handleGetCascadeTopology(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetCascadeNeighbors(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/cascade/neighbors", "")
	if err := handleGetCascadeNeighbors(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSetCascadeConfig(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/system/cascade/config", `{"enabled":true}`)
	if err := handleSetCascadeConfig(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSetCascadeConfigInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/system/cascade/config", `badjson`)
	if err := handleSetCascadeConfig(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleDeleteCascadeNeighbor(t *testing.T) {
	c, rec := setupWithAdmin("DELETE", "/api/v1/system/cascade/neighbors/n1", "")
	c.SetParamNames("neighbor_id")
	c.SetParamValues("n1")
	if err := handleDeleteCascadeNeighbor(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDeviceQuality(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/quality/dev-1", "")
	c.SetParamNames("device_id")
	c.SetParamValues("dev-1")
	if err := handleGetDeviceQuality(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetCircuitBreakers(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/circuit-breakers", "")
	if err := handleGetCircuitBreakers(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleResetCircuitBreaker(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/system/circuit-breakers/dev-1/reset", "")
	c.SetParamNames("device_id")
	c.SetParamValues("dev-1")
	if err := handleResetCircuitBreaker(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetHealthBasic(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/health/basic", "")
	if err := handleGetHealthBasic(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetReadyStatus(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/ready-status", "")
	if err := handleGetReadyStatus(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetPerformance(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/performance", "")
	if err := handleGetPerformance(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetRetention(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/retention", "")
	if err := handleGetRetention(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateRetention(t *testing.T) {
	prev := GetContainer()
	defer SetContainer(prev)

	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "test.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	defer db.Close()

	cont := NewServiceContainer()
	cont.Database = db
	SetContainer(cont)

	c, rec := setupWithAdmin("PUT", "/api/v1/system/retention", `{"retention_period":"60d"}`)
	if err := handleUpdateRetention(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateRetentionInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/system/retention", `badjson`)
	if err := handleUpdateRetention(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleGetCert(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/cert", "")
	if err := handleGetCert(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var body struct {
		Data struct {
			HasCert      bool   `json:"has_cert"`
			HTTPSEnabled bool   `json:"https_enabled"`
			Message      string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Data.HasCert || body.Data.HTTPSEnabled {
		t.Errorf("has_cert/https_enabled = %v/%v, want false: cmd/edgelite never calls StartTLS", body.Data.HasCert, body.Data.HTTPSEnabled)
	}
	if !strings.Contains(body.Data.Message, "plain HTTP") {
		t.Errorf("message = %q, want it to state that no server certificate exists", body.Data.Message)
	}
}

// Certificate rotation, NTP persistence and migration retry have no producer in
// this build: no TLS listener, no NTP config section, no runtime migration. They
// used to echo a success the operator could never verify, so the contract is now
// 501 + a code, and never a success envelope.
func TestSystemStatusPlaceholdersRefuse(t *testing.T) {
	for _, tc := range []struct {
		method  string
		path    string
		body    string
		h       func(echo.Context) error
		errCode string
	}{
		{http.MethodPost, "/api/v1/system/cert/rotate", "", handleRotateCert, "ERR_CERT_ROTATE_UNSUPPORTED"},
		{http.MethodPut, "/api/v1/system/ntp", `{"server":"pool.ntp.org"}`, handleUpdateNTP, "ERR_NTP_CONFIG_UNSUPPORTED"},
		{http.MethodPut, "/api/v1/system/ntp", `badjson`, handleUpdateNTP, "ERR_NTP_CONFIG_UNSUPPORTED"},
		{http.MethodPost, "/api/v1/system/migration/retry", "", handleRetryMigration, "ERR_MIGRATION_RETRY_UNSUPPORTED"},
	} {
		c, rec := setupWithAdmin(tc.method, tc.path, tc.body)
		if err := tc.h(c); err != nil {
			t.Fatalf("%s %s returned error: %v", tc.method, tc.path, err)
		}
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s -> %d, want 501", tc.method, tc.path, rec.Code)
			continue
		}
		payload := rec.Body.String()
		if !strings.Contains(payload, tc.errCode) {
			t.Errorf("%s %s body %s must carry %s", tc.method, tc.path, payload, tc.errCode)
		}
		if strings.Contains(payload, `"code":0`) || strings.Contains(payload, `"success":true`) {
			t.Errorf("%s %s still claims success: %s", tc.method, tc.path, payload)
		}
	}
}

func TestHandleGetMigrationHistory(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/migration/history", "")
	if err := handleGetMigrationHistory(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetLocksStatus(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/locks/status", "")
	if err := handleGetLocksStatus(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetNetwork(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/network", "")
	if err := handleGetNetwork(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateConfigSection(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/system/config/database", `{"pool_size":10}`)
	c.SetParamNames("section")
	c.SetParamValues("database")
	if err := handleUpdateConfigSection(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateConfigSectionInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/system/config/database", `badjson`)
	c.SetParamNames("section")
	c.SetParamValues("database")
	if err := handleUpdateConfigSection(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

// --- Notify Extended API Tests ---

func TestHandleCreateDingTalkChannel(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/dingtalk", `{"webhook":"https://oapi.dingtalk.com/robot/send?access_token=xxx"}`)
	if err := handleCreateDingTalkChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", rec.Code)
	}
}

func TestHandleCreateWeComChannel(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/wecom", `{"webhook":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"}`)
	if err := handleCreateWeComChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", rec.Code)
	}
}

func TestHandleCreateEmailChannel(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/email", `{"smtp_host":"smtp.example.com"}`)
	if err := handleCreateEmailChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", rec.Code)
	}
}

func TestHandleCreateWebhookChannel(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/webhook", `{"url":"https://example.com/webhook"}`)
	if err := handleCreateWebhookChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", rec.Code)
	}
}

func TestHandleTestChannel(t *testing.T) {
	// An unconfigured channel must not report a successful test.
	restoreNotifyConfig(t, config.NotifyConfig{})
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/dingtalk/test", "")
	c.SetParamNames("channel_id")
	c.SetParamValues("dingtalk")
	if err := handleTestChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for an unconfigured channel, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_NOTIFY_CHANNEL_NOT_CONFIGURED") {
		t.Fatalf("Expected ERR_NOTIFY_CHANNEL_NOT_CONFIGURED, got %s", rec.Body.String())
	}

	// A configured webhook that answers 200 reports success.
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	restoreNotifyConfig(t, config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL},
	})
	c, rec = setupWithAdmin("POST", "/api/v1/notify/channels/webhook/test", "")
	c.SetParamNames("channel_id")
	c.SetParamValues("webhook")
	if err := handleTestChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("Expected success:true, got %s", rec.Body.String())
	}
}

// restoreNotifyConfig installs a notify config for the duration of one test.
func restoreNotifyConfig(t *testing.T, notify config.NotifyConfig) {
	t.Helper()
	cfg := config.GetConfig()
	previous := cfg.Notify
	cfg.Notify = notify
	t.Cleanup(func() { cfg.Notify = previous })
}

func TestHandleEnableChannel(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/notify/channels/dingtalk/enable", "")
	c.SetParamNames("channel_id")
	c.SetParamValues("dingtalk")
	if err := handleEnableChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleDeleteChannel(t *testing.T) {
	c, rec := setupWithAdmin("DELETE", "/api/v1/notify/channels/dingtalk", "")
	c.SetParamNames("channel_id")
	c.SetParamValues("dingtalk")
	if err := handleDeleteChannel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateNotifyConfig(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/notify/config", `{"dingtalk":{"enabled":true}}`)
	if err := handleUpdateNotifyConfig(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleUpdateNotifyConfigInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/notify/config", `badjson`)
	if err := handleUpdateNotifyConfig(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

// --- Driver Extended API Tests ---

func TestHandleGetDriverList(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/drivers/list", "")
	if err := handleGetDriverList(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDriverConfigSchema(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/drivers/modbus_tcp/config-schema", "")
	c.SetParamNames("driver_name")
	c.SetParamValues("modbus_tcp")
	if err := handleGetDriverConfigSchema(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// handleDriverDiscover now delegates to DeviceService instead of answering
// "0 devices" on its own, so without a wired container the honest outcome is
// 503 -- the old expectation of 200 was asserting the fake handler.
func TestHandleDriverDiscover(t *testing.T) {
	prev := GetContainer()
	defer SetContainer(prev)

	SetContainer(NewServiceContainer())

	c, rec := setupWithAdmin("POST", "/api/v1/drivers/modbus_tcp/discover", `{"config":{"host":"192.168.1.1"}}`)
	c.SetParamNames("driver_name")
	c.SetParamValues("modbus_tcp")
	if err := handleDriverDiscover(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 without a device service, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_COMMON_SERVICE_NOT_READY") {
		t.Fatalf("response must carry the not-ready error code, got %s", rec.Body.String())
	}

	bad, badRec := setupWithAdmin("POST", "/api/v1/drivers/modbus_tcp/discover", `not json`)
	bad.SetParamNames("driver_name")
	bad.SetParamValues("modbus_tcp")
	if err := handleDriverDiscover(bad); err != nil {
		t.Fatalf("error: %v", err)
	}
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for a malformed body, got %d (%s)", badRec.Code, badRec.Body.String())
	}
}

func TestHandleGetDriverLoadStatus(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/drivers/load-status", "")
	if err := handleGetDriverLoadStatus(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDriverMeta(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/drivers/meta", "")
	if err := handleGetDriverMeta(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// handleDriverEnvCheck used to answer environment_ok:true for every driver.
// Factories are compiled into the binary, so there is no runtime dependency
// resolution to report: a registered protocol is 501, an unknown one is 404.
func TestHandleDriverEnvCheck(t *testing.T) {
	drivers.RegisterAll() // cmd/edgelite does the same at startup

	c, rec := setupWithAdmin("GET", "/api/v1/drivers/modbus_tcp/environment-check", "")
	c.SetParamNames("driver_name")
	c.SetParamValues("modbus_tcp")
	if err := handleDriverEnvCheck(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501 for a compiled-in driver, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_DRIVER_ENV_CHECK_UNSUPPORTED") {
		t.Fatalf("response must carry the unsupported code, got %s", rec.Body.String())
	}

	unknown, unknownRec := setupWithAdmin("GET", "/api/v1/drivers/no_such_driver/environment-check", "")
	unknown.SetParamNames("driver_name")
	unknown.SetParamValues("no_such_driver")
	if err := handleDriverEnvCheck(unknown); err != nil {
		t.Fatalf("error: %v", err)
	}
	if unknownRec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for an unregistered driver, got %d (%s)", unknownRec.Code, unknownRec.Body.String())
	}
}

func TestHandleOPCUABrowse(t *testing.T) {
	// The endpoint runs Browse on a saved device's live session; it used to
	// answer [] for every request, which the UI read as "this server has no nodes".
	cont := withDeviceService(t)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "mod-1",
		Name:     "modbus device",
		Protocol: "modbus_tcp",
		Status:   "offline",
		Config:   map[string]interface{}{"host": "127.0.0.1", "port": 5020},
	})

	// A body without device_id cannot be browsed: there is no session to ask.
	noID, noIDRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `{"node_id":"i=84"}`)
	if err := handleOPCUABrowse(noID); err != nil {
		t.Fatalf("error: %v", err)
	}
	if noIDRec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 without device_id, got %d (%s)", noIDRec.Code, noIDRec.Body.String())
	}
	if !strings.Contains(noIDRec.Body.String(), "ERR_DEVICE_ID_REQUIRED") {
		t.Fatalf("response must carry ERR_DEVICE_ID_REQUIRED, got %s", noIDRec.Body.String())
	}

	missing, missingRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `{"device_id":"nope"}`)
	if err := handleOPCUABrowse(missing); err != nil {
		t.Fatalf("error: %v", err)
	}
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for an unknown device, got %d (%s)", missingRec.Code, missingRec.Body.String())
	}

	unsupported, unsupportedRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `{"device_id":"mod-1"}`)
	if err := handleOPCUABrowse(unsupported); err != nil {
		t.Fatalf("error: %v", err)
	}
	if unsupportedRec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501 for a protocol with no browse service, got %d (%s)", unsupportedRec.Code, unsupportedRec.Body.String())
	}
	if !strings.Contains(unsupportedRec.Body.String(), "ERR_DRIVER_BROWSE_UNSUPPORTED") {
		t.Fatalf("response must carry ERR_DRIVER_BROWSE_UNSUPPORTED, got %s", unsupportedRec.Body.String())
	}

	// An OPC UA device whose endpoint is not listening must fail loudly, not
	// return an empty node list.
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "opc-1",
		Name:     "unreachable opc ua",
		Protocol: "opc_ua",
		Status:   "offline",
		Config: map[string]interface{}{
			"endpoint":        "opc.tcp://127.0.0.1:1",
			"session_timeout": 2000,
		},
	})
	unreachable, unreachableRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `{"device_id":"opc-1","node_id":"i=85"}`)
	if err := handleOPCUABrowse(unreachable); err != nil {
		t.Fatalf("error: %v", err)
	}
	if unreachableRec.Code != http.StatusBadGateway {
		t.Fatalf("Expected 502 when the session cannot be opened, got %d (%s)", unreachableRec.Code, unreachableRec.Body.String())
	}
	if !strings.Contains(unreachableRec.Body.String(), "ERR_DRIVER_BROWSE_FAILED") {
		t.Fatalf("response must carry ERR_DRIVER_BROWSE_FAILED, got %s", unreachableRec.Body.String())
	}
}

func TestHandleOPCUABrowseInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `badjson`)
	if err := handleOPCUABrowse(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleOPCUACertStatus(t *testing.T) {
	// It used to answer a hardcoded "no certificate" 200 for every request, so a
	// certificate about to lapse looked absent.

	// No device service means there is nothing to read; say so instead of 200.
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	notReady, notReadyRec := setupWithAdmin("GET", "/api/v1/drivers/opcua/certificate-status", "")
	if err := handleOPCUACertStatus(notReady); err != nil {
		t.Fatalf("error: %v", err)
	}
	SetContainer(prev)
	if notReadyRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 without a device service, got %d (%s)", notReadyRec.Code, notReadyRec.Body.String())
	}

	cont := withDeviceService(t)
	validPath := writeSelfSignedCert(t, "edgelite-valid-client", time.Now().Add(30*24*time.Hour))
	expiredPath := writeSelfSignedCert(t, "edgelite-expired-client", time.Now().Add(-time.Hour))
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "opc-valid",
		Name:     "valid cert device",
		Protocol: "opc_ua",
		Status:   "online",
		Config:   map[string]interface{}{"client_cert_path": validPath},
	})
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "opc-expired",
		Name:     "expired cert device",
		Protocol: "opc_ua",
		Status:   "online",
		Config:   map[string]interface{}{"certificate_file": expiredPath},
	})
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "opc-missing",
		Name:     "missing cert file device",
		Protocol: "opc_ua",
		Status:   "offline",
		Config:   map[string]interface{}{"client_cert_path": filepath.Join(t.TempDir(), "absent.pem")},
	})
	// A non-OPC-UA device must never appear in the certificate report.
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "mod-other",
		Name:     "modbus device",
		Protocol: "modbus_tcp",
		Status:   "offline",
		Config:   map[string]interface{}{"host": "127.0.0.1"},
	})

	c, rec := setupWithAdmin("GET", "/api/v1/drivers/opcua/certificate-status", "")
	if err := handleOPCUACertStatus(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unparseable response: %v (%s)", err, rec.Body.String())
	}
	var body struct {
		HasCertificate        bool                     `json:"has_certificate"`
		TrustedHostsSupported bool                     `json:"trusted_hosts_supported"`
		Certificates          []map[string]interface{} `json:"certificates"`
	}
	if err := json.Unmarshal(envelope.Data, &body); err != nil {
		t.Fatalf("unparseable payload: %v (%s)", err, rec.Body.String())
	}
	if len(body.Certificates) != 3 {
		t.Fatalf("Expected one entry per opc_ua device with a configured certificate, got %d (%s)", len(body.Certificates), rec.Body.String())
	}
	if !body.HasCertificate {
		t.Fatalf("has_certificate must be true when certificates are configured")
	}
	if body.TrustedHostsSupported {
		t.Fatalf("gopcua in this build does not validate server certs; the flag must stay false")
	}

	byDevice := map[string]map[string]interface{}{}
	for _, entry := range body.Certificates {
		byDevice[entry["device_id"].(string)] = entry
	}
	valid := byDevice["opc-valid"]
	if valid == nil || valid["readable"] != true || valid["expired"] != false {
		t.Fatalf("valid certificate must report readable+not expired, got %v", valid)
	}
	if subject, _ := valid["subject"].(string); !strings.Contains(subject, "edgelite-valid-client") {
		t.Fatalf("subject must come from the parsed certificate, got %v", valid["subject"])
	}
	expired := byDevice["opc-expired"]
	if expired == nil || expired["expired"] != true {
		t.Fatalf("lapsed certificate must report expired:true, got %v", expired)
	}
	missing := byDevice["opc-missing"]
	if missing == nil || missing["readable"] != false {
		t.Fatalf("unreadable certificate file must report readable:false, got %v", missing)
	}
	if errCode, _ := missing["error"].(string); !strings.Contains(errCode, "ERR_OPCUA_CERT_FILE_NOT_FOUND") {
		t.Fatalf("missing file must carry ERR_OPCUA_CERT_FILE_NOT_FOUND, got %v", missing["error"])
	}
	if _, leaked := byDevice["mod-other"]; leaked {
		t.Fatalf("non-opc_ua devices must not appear in the certificate report")
	}
}

func TestHandleOPCDAServers(t *testing.T) {
	// Enumerating DA servers needs the Windows COM registry, which this build
	// does not link. An empty 200 list read as "no servers on that host".
	c, rec := setupWithAdmin("GET", "/api/v1/drivers/opc-da/servers", "")
	if err := handleOPCDAServers(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_OPCDA_DISCOVERY_UNSUPPORTED") {
		t.Fatalf("response must carry ERR_OPCDA_DISCOVERY_UNSUPPORTED, got %s", rec.Body.String())
	}
}

func TestHandleGetDriverHealth(t *testing.T) {
	// It used to answer {healthy:true, drivers:{}} for every request, reporting
	// an outage as a healthy fleet.
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	notReady, notReadyRec := setupWithAdmin("GET", "/api/v1/drivers/health", "")
	if err := handleGetDriverHealth(notReady); err != nil {
		t.Fatalf("error: %v", err)
	}
	SetContainer(prev)
	if notReadyRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 without a device service, got %d (%s)", notReadyRec.Code, notReadyRec.Body.String())
	}

	cont := withDeviceService(t)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "healthy-1",
		Name:     "simulator with samples",
		Protocol: "simulator",
		Status:   "online",
		Config:   map[string]interface{}{},
	})
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "silent-1",
		Name:     "simulator never polled",
		Protocol: "simulator",
		Status:   "offline",
		Config:   map[string]interface{}{},
	})

	mgr := drivers.GetHealthStatsManager()
	if mgr == nil {
		t.Fatalf("expected the global health stats manager")
	}
	mgr.ResetHealthStats("healthy-1")
	mgr.ResetHealthStats("silent-1")
	t.Cleanup(func() {
		mgr.ResetHealthStats("healthy-1")
		mgr.ResetHealthStats("silent-1")
	})
	mgr.RecordReadSuccess("healthy-1", 12)
	mgr.RecordReadFailure("healthy-1")

	c, rec := setupWithAdmin("GET", "/api/v1/drivers/health", "")
	if err := handleGetDriverHealth(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unparseable response: %v (%s)", err, rec.Body.String())
	}
	var body struct {
		Healthy     bool                              `json:"healthy"`
		DeviceCount int                               `json:"device_count"`
		HasStats    bool                              `json:"has_stats"`
		Drivers     map[string]interface{}            `json:"drivers"`
		Devices     map[string]map[string]interface{} `json:"devices"`
	}
	if err := json.Unmarshal(envelope.Data, &body); err != nil {
		t.Fatalf("unparseable payload: %v (%s)", err, rec.Body.String())
	}
	if body.DeviceCount != 2 {
		t.Fatalf("Expected device_count 2, got %v", body.DeviceCount)
	}
	if !body.HasStats {
		t.Fatalf("has_stats must be true once a device has samples")
	}
	if body.Healthy {
		t.Fatalf("a device with a failed read must be reported, got %s", rec.Body.String())
	}
	group, ok := body.Drivers["simulator"]
	if !ok {
		t.Fatalf("Expected a per-protocol group for simulator, got %v", body.Drivers)
	}
	encoded, _ := json.Marshal(group)
	if !strings.Contains(string(encoded), `"total_reads":2`) || !strings.Contains(string(encoded), `"failed_reads":1`) {
		t.Fatalf("protocol group must aggregate the real counters, got %s", encoded)
	}
	if entry := body.Devices["silent-1"]; entry == nil || entry["has_samples"] != false || entry["healthy"] != false {
		t.Fatalf("an unpolled device must report has_samples:false and healthy:false, got %v", entry)
	}
	if entry := body.Devices["healthy-1"]; entry == nil || entry["has_samples"] != true {
		t.Fatalf("a polled device must report has_samples:true, got %v", entry)
	}
}

// --- Platform Extended API Tests ---

func TestHandleGetPlatformList(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/platforms/list", "")
	if err := handleGetPlatformList(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetPlatformConfigSchema(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/platforms/config-schema/iotsharp", "")
	c.SetParamNames("platform_name")
	c.SetParamValues("iotsharp")
	if err := handleGetPlatformConfigSchema(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleConnectPlatform(t *testing.T) {
	// 连接需要真实 MQTT 拨号：空配置下预期连接失败（502）；
	// 管理器未就绪则为 503。任何一种都证明 handler 已接入真实链路。
	c, rec := setupWithAdmin("POST", "/api/v1/platforms/connect/iotsharp", "")
	c.SetParamNames("platform_name")
	c.SetParamValues("iotsharp")
	if err := handleConnectPlatform(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 200/502/503, got %d", rec.Code)
	}
}

func TestHandleDisconnectPlatform(t *testing.T) {
	// 管理器就绪时，断开未连接的平台应直接返回 200。
	if GetContainer().PlatformMgr == nil {
		SetContainer(NewServiceContainer())
		GetContainer().PlatformMgr = northbound.NewManager(nil)
	}
	c, rec := setupWithAdmin("POST", "/api/v1/platforms/disconnect/iotsharp", "")
	c.SetParamNames("platform_name")
	c.SetParamValues("iotsharp")
	if err := handleDisconnectPlatform(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleTestPlatformConnection(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/platforms/test-connection/iotsharp", "")
	c.SetParamNames("platform_name")
	c.SetParamValues("iotsharp")
	if err := handleTestPlatformConnection(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetPlatformStatusByName(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/platforms/status/iotsharp", "")
	c.SetParamNames("platform_name")
	c.SetParamValues("iotsharp")
	if err := handleGetPlatformStatusByName(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetPlatformDashboard(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/platforms/dashboard", "")
	if err := handleGetPlatformDashboard(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetPlatformMetrics(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/platforms/metrics", "")
	if err := handleGetPlatformMetrics(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// --- Alarm API Tests (no service dependency) ---

// The bare container has no alarm store, and the endpoint now refuses to answer
// with an invented all-zero summary; see alarm_statistics_test.go for the
// aggregation itself.
func TestHandleGetAlarmStatistics(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/alarms/statistics", "")
	if err := handleGetAlarmStatistics(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestHandleGetAlarmTrend(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/alarms/trend", "")
	if err := handleGetAlarmTrend(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetAlarmCorrelation(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/alarms/correlation", "")
	if err := handleGetAlarmCorrelation(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleListAlarmSilence(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/alarms/silence", "")
	if err := handleListAlarmSilence(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// --- Alarm API Tests (with nil service -> ServiceUnavailable) ---

func TestHandleListAlarmsNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/alarms", "")
	if err := handleListAlarms(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestHandleBatchAckAlarmsEmptyBody(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/alarms/batch-ack", `{}`)
	if err := handleBatchAckAlarms(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleBatchAckAlarmsInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/alarms/batch-ack", `badjson`)
	if err := handleBatchAckAlarms(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

// --- Device API Tests (with nil service -> ServiceUnavailable) ---

func TestHandleListDevicesNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/devices", "")
	if err := handleListDevices(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestHandleCreateDeviceInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/devices", `badjson`)
	if err := handleCreateDevice(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleGetDeviceMissingID(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/devices/", "")
	if err := handleGetDevice(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleGetCollectStats(t *testing.T) {
	// The empty container has no collect scheduler. Answering 200 with {} would
	// read as "no collector is running", which is a different claim.
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/devices/collect-stats", "")
	if err := handleGetCollectStats(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// --- Rule API Tests ---

func TestHandleListRulesNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/rules", "")
	if err := handleListRules(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestHandleCreateRuleInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/rules", `badjson`)
	if err := handleCreateRule(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleTestRuleInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/rules/test", `badjson`)
	if err := handleTestRule(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleBatchDeleteRulesInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/rules/batch/delete", `badjson`)
	if err := handleBatchDeleteRules(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleBatchEnableRulesInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/rules/batch/enable", `badjson`)
	if err := handleBatchEnableRules(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleBatchDisableRulesInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/rules/batch/disable", `badjson`)
	if err := handleBatchDisableRules(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

// --- Data API Tests ---

func TestHandleQueryTimeseriesNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/data/timeseries?device_id=dev1&start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", "")
	if err := handleQueryTimeseries(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	// Without TsStorage, returns 503; with missing params, may return 400
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 503 or 400, got %d", rec.Code)
	}
}

func TestHandleGetDataStatsNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/data/stats", "")
	if err := handleGetDataStats(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	// May return 200 with empty data or 503 if service required
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 200 or 503, got %d", rec.Code)
	}
}

func TestHandleGetDataCorrelation(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/data/correlation?device_id=dev1", "")
	if err := handleGetDataCorrelation(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 200 or 400, got %d", rec.Code)
	}
}

func TestHandleQueryTrendNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/data/trend?device_id=dev1&start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", "")
	if err := handleQueryTrend(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 503 or 400, got %d", rec.Code)
	}
}

// Statistics needs a device, a point and a window; without them there is no
// measurement to summarise, so a 200 would be an answer to an unasked question.
func TestHandleGetDataStatisticsRequiresWindow(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/data/statistics", "")
	if err := handleGetDataStatistics(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleQueryMultiPointNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("POST", "/api/v1/data/multi-point", `{"device_ids":["dev1"],"point_names":["temp"],"start":"2024-01-01T00:00:00Z","end":"2024-01-02T00:00:00Z"}`)
	if err := handleQueryMultiPoint(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 503 or 400, got %d", rec.Code)
	}
}

func TestHandleExportDataNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/data/export?device_id=dev1&start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", "")
	if err := handleExportData(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 503 or 400, got %d", rec.Code)
	}
}

func TestHandleExportConfig(t *testing.T) {
	// No config-export implementation exists in this build. The endpoint used
	// to answer 200 with "{}" content, which reads as a completed export; it
	// must refuse honestly instead (the real timeseries export is GET
	// /data/export, covered by the handler's own tests).
	c, rec := setupWithAdmin("GET", "/api/v1/data/export-config", "")
	if err := handleExportConfig(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501 ERR_CONFIG_EXPORT_UNSUPPORTED, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_CONFIG_EXPORT_UNSUPPORTED") {
		t.Fatalf("response must carry the stable error code, got %s", rec.Body.String())
	}
}

func TestHandleImportDataNoFile(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/data/import", "")
	if err := handleImportData(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleImportTemplate(t *testing.T) {
	for _, f := range []string{"csv", "json"} {
		c, rec := setupWithAdmin("GET", "/api/v1/data/import/template?format="+f, "")
		if err := handleImportTemplate(c); err != nil {
			t.Fatalf("error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("format %s: Expected 200, got %d", f, rec.Code)
		}
	}
}

func TestHandleDownsampleDataNoService(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("POST", "/api/v1/data/downsample", `{"device_id":"dev1"}`)
	if err := handleDownsampleData(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 200 or 503, got %d", rec.Code)
	}
}

// --- Helper function tests ---

func TestContainsStr(t *testing.T) {
	if !containsStr("hello world", "world") {
		t.Fatal("containsStr should find 'world' in 'hello world'")
	}
	if containsStr("hello", "world") {
		t.Fatal("containsStr should not find 'world' in 'hello'")
	}
}

func TestIndexOfContains(t *testing.T) {
	idx := indexOfContains("hello world", "world")
	if idx != 6 {
		t.Fatalf("Expected index 6, got %d", idx)
	}
	idx = indexOfContains("hello", "world")
	if idx != -1 {
		t.Fatalf("Expected -1, got %d", idx)
	}
}

func TestParseTimeString(t *testing.T) {
	// Valid time string
	_, err := parseTimeString("2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parseTimeString failed for valid input: %v", err)
	}
	// Invalid time string should return error
	_, err = parseTimeString("invalid")
	if err == nil {
		t.Fatal("parseTimeString should error for invalid input")
	}
}

// --- Extended Endpoints 3 Tests ---

func TestHandleGetSimTypes(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/simulator/types", "")
	if err := handleGetSimTypes(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSimPreview(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/simulator/preview", `{"protocol":"modbus_tcp"}`)
	if err := handleSimPreview(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSimPreviewInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/simulator/preview", `badjson`)
	if err := handleSimPreview(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleSimRun(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/simulator/run", `{"protocol":"modbus_tcp"}`)
	if err := handleSimRun(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSimAssess(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/simulator/assess", `{"protocol":"modbus_tcp","duration":60}`)
	if err := handleSimAssess(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDataQualityDevices(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/data-quality/devices", "")
	if err := handleGetDataQualityDevices(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDataQualityPoints(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/data-quality/points", "")
	if err := handleGetDataQualityPoints(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetDataQualityReport(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/data-quality/report", "")
	if err := handleGetDataQualityReport(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 200 or an honest 503, got %d", rec.Code)
	}
}

// POST /data-quality/reset used to answer {"reset":true} while clearing
// nothing; there is no counter to clear, so the endpoint now says so.
func TestHandleResetDataQuality(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/data-quality/reset", "")
	if err := handleResetDataQuality(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 instead of a reset that never happened", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `"reset":true`) {
		t.Fatalf("still reports a reset it did not perform: %s", body)
	}
}

// The handler used to answer 200 with five literals, so this test certified it
// by asking it with no database in the container at all. It now reports the live
// pool or says there is none, which is what this asserts.
func TestHandleGetDBPoolStatsNeedsADatabase(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/db-pool-stats", "")
	if err := handleGetDBPoolStats(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 without a database, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_DB_UNAVAILABLE") {
		t.Fatalf("response must carry ERR_DB_UNAVAILABLE, got %s", rec.Body.String())
	}
}

// --- Bridge Tests ---

func TestHandleBridgeList(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/bridges", "")
	if err := handleBridgeList(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleBridgeGet(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/bridges/b1", "")
	c.SetParamNames("bridge_id")
	c.SetParamValues("b1")
	if err := handleBridgeGet(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 200 or 400, got %d", rec.Code)
	}
}

func TestHandleBridgeCreateInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/bridges", `badjson`)
	if err := handleBridgeCreate(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleBridgeUpdateInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/bridges/b1", `badjson`)
	c.SetParamNames("bridge_id")
	c.SetParamValues("b1")
	if err := handleBridgeUpdate(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

// --- Resource Share Tests ---

func TestHandleCreateResourceShareInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/resource-shares", `badjson`)
	if err := handleCreateResourceShare(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleDeleteResourceShare(t *testing.T) {
	c, rec := setupWithAdmin("DELETE", "/api/v1/resource-shares/rs1", "")
	c.SetParamNames("id")
	c.SetParamValues("rs1")
	if err := handleDeleteResourceShare(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
		// 503 ERR_COMMON_DB_NOT_READY: shares persist in system_settings, so a
		// delete without a database cannot claim anything was removed.
		t.Fatalf("Expected 200, 400, 404 or 503, got %d", rec.Code)
	}
}

func TestHandleCheckResourceShare(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/resource-shares/check?resource_id=res1", "")
	if err := handleCheckResourceShare(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 200 or 400, got %d", rec.Code)
	}
}

// --- Log Aggregation Tests ---
//
// These pin the "no aggregator running" and "not implemented" answers. The
// positive cases -- real rows, real counters, a real level change -- live in
// logs_export_test.go, which installs an in-memory aggregator.

func TestHandleLogAggQuery(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/logs/query", "")
	if err := handleLogAggQuery(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	// {logs: [], total: 0} used to be returned unconditionally, so the log page
	// could not tell "nothing was logged" apart from "logging is off".
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleLogAggStats(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/logs/stats", "")
	if err := handleLogAggStats(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_LOG_AGGREGATOR_NOT_READY") {
		t.Fatalf("body = %s, want ERR_LOG_AGGREGATOR_NOT_READY", rec.Body.String())
	}
}

func TestHandleLogAggFilters(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/logs/filters", "")
	if err := handleLogAggFilters(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	// The advertised filter set must be the one collectLogRows actually applies;
	// "module" was advertised for years and never filtered anything.
	var resp struct {
		Data struct {
			Fields []string `json:"fields"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v (%s)", err, rec.Body.String())
	}
	got := map[string]bool{}
	for _, f := range resp.Data.Fields {
		got[f] = true
	}
	for _, want := range []string{"level", "source", "device_id", "search", "start_time", "end_time"} {
		if !got[want] {
			t.Fatalf("fields = %v, want %s", resp.Data.Fields, want)
		}
	}
	if got["module"] {
		t.Fatalf("module is not a supported filter: %v", resp.Data.Fields)
	}
}

func TestHandleLogAggSetLevelInvalidJSON(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/logs/level", `badjson`)
	if err := handleLogAggSetLevel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestHandleLogAggSetLevelInvalidLevel(t *testing.T) {
	c, rec := setupWithAdmin("PUT", "/api/v1/logs/level", `{"level":"verbose"}`)
	if err := handleLogAggSetLevel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_LOG_INVALID_LEVEL") {
		t.Fatalf("body = %s, want ERR_LOG_INVALID_LEVEL", rec.Body.String())
	}
}

func TestHandleLogAggArchive(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/logs/archive", "")
	if err := handleLogAggArchive(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	// Answering {archived: true, file: "..."} advertised an artifact that was
	// never written; there is no archiver behind this route.
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_LOG_ARCHIVE_UNSUPPORTED") {
		t.Fatalf("body = %s, want ERR_LOG_ARCHIVE_UNSUPPORTED", rec.Body.String())
	}
}

func TestHandleLogAggCleanup(t *testing.T) {
	c, rec := setupWithAdmin("POST", "/api/v1/logs/cleanup", "")
	if err := handleLogAggCleanup(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_LOG_CLEANUP_UNSUPPORTED") {
		t.Fatalf("body = %s, want ERR_LOG_CLEANUP_UNSUPPORTED", rec.Body.String())
	}
}

// Script logs and the review endpoints are covered in script_enable_test.go:
// both now refuse with 501, so the two assertions that used to sit here and
// expect 200 were certifying the echoes they replaced.

// --- Simulation devices, learners and system-status honesty (task #25) ---

// The simulation page used to be a facade: create echoed the body as 201 and
// delete only logged, so a "successful" device vanished on refresh. These routes
// now go through DeviceService, so the row has to exist in the device store.
func TestSimulationDeviceRoutesDriveTheRealDeviceStack(t *testing.T) {
	cont := withDeviceService(t)
	const body = `{"device_id":"qa25-sim-1","name":"qa25 sim","collect_interval":5,"points":[{"name":"temp","data_type":"float","min":20,"max":80,"mode":"sine"},{"name":"pressure","data_type":"float","min":0.1,"max":1.5,"mode":"random"}]}`

	c, rec := setupWithAdmin("POST", "/api/v1/simulation/devices", body)
	if err := handleCreateSimDevice(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("create -> %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if _, err := cont.DeviceRepo.Get("qa25-sim-1"); err != nil {
		t.Fatalf("created device is not in the device store: %v", err)
	}

	listC, listRec := setupWithAdmin("GET", "/api/v1/simulation/devices", "")
	if err := handleListSimDevices(listC); err != nil {
		t.Fatalf("error: %v", err)
	}
	var listed struct {
		Data struct {
			Items []struct {
				DeviceID   string `json:"device_id"`
				PointCount int    `json:"point_count"`
				Status     string `json:"status"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list body %s: %v", listRec.Body.String(), err)
	}
	if len(listed.Data.Items) != 1 || listed.Data.Items[0].DeviceID != "qa25-sim-1" || listed.Data.Items[0].PointCount != 2 {
		t.Fatalf("list must show the created device: %s", listRec.Body.String())
	}

	// A taken id is a conflict; SQLite's UNIQUE error used to be reported as a
	// malformed request, which told the operator their body was wrong.
	dupC, dupRec := setupWithAdmin("POST", "/api/v1/simulation/devices", body)
	if err := handleCreateSimDevice(dupC); err != nil {
		t.Fatalf("error: %v", err)
	}
	if dupRec.Code != http.StatusConflict || !strings.Contains(dupRec.Body.String(), "ERR_DEVICE_ALREADY_EXISTS") {
		t.Fatalf("duplicate create -> %d %s, want 409 ERR_DEVICE_ALREADY_EXISTS", dupRec.Code, dupRec.Body.String())
	}

	noPoints, noPointsRec := setupWithAdmin("POST", "/api/v1/simulation/devices", `{"device_id":"qa25-sim-2","name":"no points","points":[]}`)
	if err := handleCreateSimDevice(noPoints); err != nil {
		t.Fatalf("error: %v", err)
	}
	if noPointsRec.Code != http.StatusBadRequest {
		t.Fatalf("create without points -> %d %s, want 400", noPointsRec.Code, noPointsRec.Body.String())
	}

	// Deleting through this route must not touch a real protocol's device.
	if err := cont.DeviceRepo.Create(&models.DeviceResponse{
		DeviceID: "qa25-modbus-1", Name: "real plc", Protocol: "modbus_tcp", Status: "offline",
	}, "tester"); err != nil {
		t.Fatalf("failed to seed device: %v", err)
	}
	wrongC, wrongRec := setupWithAdmin("DELETE", "/api/v1/simulation/devices/qa25-modbus-1", "")
	wrongC.SetParamNames("id")
	wrongC.SetParamValues("qa25-modbus-1")
	if err := handleDeleteSimDevice(wrongC); err != nil {
		t.Fatalf("error: %v", err)
	}
	if wrongRec.Code != http.StatusBadRequest || !strings.Contains(wrongRec.Body.String(), "ERR_DEVICE_NOT_SIMULATOR") {
		t.Fatalf("delete of a modbus device -> %d %s, want 400 ERR_DEVICE_NOT_SIMULATOR", wrongRec.Code, wrongRec.Body.String())
	}
	if still, _ := cont.DeviceRepo.Get("qa25-modbus-1"); still == nil {
		t.Fatal("a non-simulator device was deleted through the simulation route")
	}

	delC, delRec := setupWithAdmin("DELETE", "/api/v1/simulation/devices/qa25-sim-1", "")
	delC.SetParamNames("id")
	delC.SetParamValues("qa25-sim-1")
	if err := handleDeleteSimDevice(delC); err != nil {
		t.Fatalf("error: %v", err)
	}
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete -> %d %s, want 200", delRec.Code, delRec.Body.String())
	}
	if gone, _ := cont.DeviceRepo.Get("qa25-sim-1"); gone != nil {
		t.Fatal("device row survived the delete")
	}

	// Start/stop are lifecycle fakes: a simulator device collects as soon as it
	// exists, so the routes must refuse instead of answering "started".
	for _, h := range []func(echo.Context) error{handleStartSimDevice, handleStopSimDevice} {
		sc, sRec := setupWithAdmin("POST", "/api/v1/simulation/devices/qa25-sim-1/start", "")
		sc.SetParamNames("id")
		sc.SetParamValues("qa25-sim-1")
		if err := h(sc); err != nil {
			t.Fatalf("error: %v", err)
		}
		if sRec.Code != http.StatusNotImplemented || strings.Contains(sRec.Body.String(), `"code":0`) {
			t.Fatalf("simulator lifecycle -> %d %s, want 501 without a success envelope", sRec.Code, sRec.Body.String())
		}
	}
}

// There is no self-learning service in internal/services, so these panels used to
// render counters that no code ever wrote.
func TestLearnerRoutesRefuseUnimplementedFeatures(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    func(echo.Context) error
	}{
		{"anomaly reset", handleResetAnomalyLearner},
		{"trend reset", handleResetTrendLearner},
		{"threshold reset", handleResetThresholdLearner},
		{"threshold set", handleSetAnomalyThreshold},
	} {
		c, rec := setupWithAdmin("POST", "/api/v1/anomaly-learner/reset", `{"threshold":0.8}`)
		if err := tc.h(c); err != nil {
			t.Fatalf("error: %v", err)
		}
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s -> %d %s, want 501", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "ERR_LEARNER_UNSUPPORTED") {
			t.Errorf("%s body %s must carry ERR_LEARNER_UNSUPPORTED", tc.name, rec.Body.String())
		}
	}

	for _, tc := range []struct {
		name string
		h    func(echo.Context) error
	}{
		{"anomaly", handleGetAnomalyLearnerStats},
		{"trend", handleGetTrendLearnerStats},
		{"threshold", handleGetThresholdLearnerStats},
	} {
		c, rec := setupWithAdmin("GET", "/api/v1/anomaly-learner/stats", "")
		if err := tc.h(c); err != nil {
			t.Fatalf("error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s stats -> %d, want 200", tc.name, rec.Code)
			continue
		}
		var payload struct {
			Data struct {
				Implemented bool   `json:"implemented"`
				Message     string `json:"message"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s stats body %s: %v", tc.name, rec.Body.String(), err)
		}
		if payload.Data.Implemented {
			t.Errorf("%s stats claim implemented=true", tc.name)
		}
		if !strings.Contains(payload.Data.Message, "structural zeros") {
			t.Errorf("%s stats must say the zeros are not measurements: %s", tc.name, payload.Data.Message)
		}
	}
}

// The migration card reads current_status/last_updated/last_failure; the old
// payload only had status/progress/message, so every field rendered as unknown.
func TestMigrationStatusReportsTheKeysThePageReads(t *testing.T) {
	c, rec := setupWithAdmin("GET", "/api/v1/system/migration/status", "")
	if err := handleGetMigrationStatus(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	var payload struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	for _, key := range []string{"current_status", "last_updated", "last_failure", "tracked"} {
		if _, ok := payload.Data[key]; !ok {
			t.Errorf("data is missing %q that the page reads: %s", key, rec.Body.String())
		}
	}
	if tracked, _ := payload.Data["tracked"].(bool); tracked {
		t.Error("tracked = true, but storage.Migrate records no run history")
	}
}

// The status endpoint used to hardcode zeros while POST /system/ntp/sync ran a
// real SNTP query and threw the answer away.
func TestNTPStatusMirrorsTheLastManualQuery(t *testing.T) {
	prev := ntpLastSync.result
	t.Cleanup(func() {
		ntpLastSync.Lock()
		ntpLastSync.result = prev
		ntpLastSync.Unlock()
	})

	ntpLastSync.Lock()
	ntpLastSync.result = nil
	ntpLastSync.Unlock()

	read := func() map[string]interface{} {
		c, rec := setupWithAdmin("GET", "/api/v1/system/ntp/status", "")
		if err := handleGetNTPStatus(c); err != nil {
			t.Fatalf("error: %v", err)
		}
		var payload struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("body %s: %v", rec.Body.String(), err)
		}
		return payload.Data
	}

	first := read()
	if status, _ := first["sync_status"].(string); status != "never_queried" {
		t.Errorf("sync_status = %v before any query, want never_queried", first["sync_status"])
	}
	if enabled, _ := first["enabled"].(bool); enabled {
		t.Error("enabled = true, but no NTP config section or daemon exists")
	}
	if ct, _ := first["current_time"].(string); ct == "" {
		t.Error("current_time must be reported even before a query")
	}

	recordNTPSync(map[string]interface{}{"synced": true, "server": "pool.ntp.org", "offset_ms": 12.5})
	second := read()
	if status, _ := second["sync_status"].(string); status != "queried_ok" {
		t.Errorf("sync_status = %v after a successful query, want queried_ok", second["sync_status"])
	}
	if offset, _ := second["offset_ms"].(float64); offset != 12.5 {
		t.Errorf("offset_ms = %v, want the measured 12.5", second["offset_ms"])
	}

	recordNTPSync(map[string]interface{}{"synced": false, "server": "pool.ntp.org", "message": "NTP read failed: i/o timeout"})
	third := read()
	if status, _ := third["sync_status"].(string); status != "queried_failed" {
		t.Errorf("sync_status = %v after a failed query, want queried_failed", third["sync_status"])
	}
	if msg, _ := third["message"].(string); !strings.Contains(msg, "i/o timeout") {
		t.Errorf("a failed query must still report its cause: %v", third["message"])
	}
}

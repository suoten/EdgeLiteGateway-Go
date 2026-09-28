// Package integration provides end-to-end integration tests for EdgeLite Gateway.
//
// These tests verify the full request lifecycle including middleware chain,
// authentication, authorization, database operations, and API response formats.
// They use a real SQLite database (in-memory or temp file) to ensure
// realistic behavior.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/api"
	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	elware "edgelite/internal/middleware"
	"edgelite/internal/security"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

// TestMain registers the built-in driver factories the same way cmd/edgelite
// does. Without it every device creation in this suite stops at
// "unsupported protocol: simulator" inside the service layer, so none of the
// driver code below the API actually runs.
func TestMain(m *testing.M) {
	drivers.RegisterAll()
	os.Exit(m.Run())
}

// testEnv holds the integration test environment.
type testEnv struct {
	echo        *echo.Echo
	db          *storage.Database
	container   *api.ServiceContainer
	cfg         *config.AppConfig
	adminToken  string
	viewerToken string
}

// setupTestEnv creates a full test environment with database, services, and API routes.
func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	tmpDir := t.TempDir()

	cfg := config.DefaultAppConfig()
	cfg.Database.SQLitePath = filepath.Join(tmpDir, "test_integration.db")
	cfg.Database.BackupDir = filepath.Join(tmpDir, "backups")
	cfg.InfluxDB.SQLiteTSPath = filepath.Join(tmpDir, "test_ts.db")
	cfg.MQTT.OfflineDBPath = filepath.Join(tmpDir, "test_offline.db")
	cfg.Security.SecretKey = "test-secret-key-for-integration-testing-32chars!"
	cfg.Security.CSRFSecret = "test-csrf-secret-key-for-integration-testing!"
	cfg.Security.AccessTokenExpireMinutes = 60
	cfg.Security.RefreshTokenExpireDays = 7
	cfg.Server.DebugAPIEnabled = true

	config.SetGlobalConfig(cfg)

	// Reset JWT and CSRF manager singletons for test isolation
	security.ResetJWTManagerForTest()
	security.ResetCSRFManagerForTest()

	// Database
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}

	deviceRepo := storage.NewDeviceRepo(db)
	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	userRepo := storage.NewUserRepo(db)
	templateRepo := storage.NewTemplateRepo(db)

	tsStorage, err := storage.NewTimeSeriesStorage(cfg)
	if err != nil {
		db.Close()
		t.Fatalf("Failed to create TS storage: %v", err)
	}

	cache := storage.NewCacheManager(10000)
	eventBus := engine.NewEventBus(1000)
	scheduler := engine.NewCollectScheduler(eventBus, tsStorage, cache, &cfg.Scheduler)
	cbRegistry := engine.NewCircuitBreakerRegistry(eventBus)
	evaluator := engine.NewRuleEvaluator(eventBus, ruleRepo, alarmRepo)

	notifyService := services.NewNotifyService()
	notifyService.SetConfig(&cfg.Notify)

	deviceService := services.NewDeviceService(deviceRepo, templateRepo, scheduler, cbRegistry)
	ruleService := services.NewRuleService(ruleRepo, evaluator)
	alarmService := services.NewAlarmService(alarmRepo, evaluator)
	dataService := services.NewDataService(tsStorage, cache)
	systemService := services.NewSystemService(cfg)
	alarmService.SetNotifyService(notifyService)

	// Create admin user
	hashedPassword, _ := security.HashPassword("Admin@123456")
	_ = userRepo.Create("admin-user-001", "admin", hashedPassword, "admin")

	// Create viewer user
	viewerHash, _ := security.HashPassword("Viewer@123456")
	_ = userRepo.Create("viewer-user-001", "viewer", viewerHash, "viewer")

	container := &api.ServiceContainer{
		Database:     db,
		DeviceRepo:   deviceRepo,
		RuleRepo:     ruleRepo,
		AlarmRepo:    alarmRepo,
		TemplateRepo: templateRepo,
		UserRepo:     userRepo,
		TsStorage:    tsStorage,
		Cache:        cache,

		DeviceService: deviceService,
		RuleService:   ruleService,
		AlarmService:  alarmService,
		DataService:   dataService,
		SystemService: systemService,
		NotifyService: notifyService,

		Scheduler:  scheduler,
		EventBus:   eventBus,
		CBRegistry: cbRegistry,
		Evaluator:  evaluator,
	}
	api.SetContainer(container)

	// Generate tokens
	jwtMgr := security.GetJWTManager()
	adminToken, _, err := jwtMgr.GenerateAccessToken("admin-user-001", "admin", "admin")
	if err != nil {
		t.Fatalf("Failed to generate admin token: %v", err)
	}
	viewerToken, _, err := jwtMgr.GenerateAccessToken("viewer-user-001", "viewer", "viewer")
	if err != nil {
		t.Fatalf("Failed to generate viewer token: %v", err)
	}

	// Create Echo instance
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Middleware
	e.Use(elware.RequestIDMiddleware())
	e.Use(elware.LoggerMiddleware())
	e.Use(elware.SecurityHeadersMiddleware())

	// CORS (dev mode)
	e.Use(elware.CORSMiddleware([]string{"*"}))

	// Rate limiting (high limit for tests)
	limiter := elware.NewRateLimiter(10000)
	e.Use(elware.RateLimitMiddleware(limiter))

	// Routes
	v1 := e.Group("/api/v1")

	authGroup := v1.Group("/auth")
	api.RegisterAuthRoutes(authGroup)

	deviceGroup := v1.Group("/devices", elware.AuthMiddleware())
	api.RegisterDeviceRoutes(deviceGroup)

	ruleGroup := v1.Group("/rules", elware.AuthMiddleware())
	api.RegisterRuleRoutes(ruleGroup)

	alarmGroup := v1.Group("/alarms", elware.AuthMiddleware())
	api.RegisterAlarmRoutes(alarmGroup)

	dataGroup := v1.Group("/data", elware.AuthMiddleware())
	api.RegisterDataRoutes(dataGroup)

	systemGroup := v1.Group("/system")
	api.RegisterSystemRoutes(systemGroup)
	api.RegisterSystemExtendedRoutes(systemGroup)

	userGroup := v1.Group("/users", elware.AuthMiddleware())
	api.RegisterUserRoutes(userGroup)

	// Metrics
	metricsGroup := e.Group("/metrics")
	api.RegisterMetricsRoutes(metricsGroup)

	// Health
	e.GET("/health/live", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "alive"})
	})
	e.GET("/health/ready", func(c echo.Context) error {
		if db.IsHealthy() {
			return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
		}
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
	})

	return &testEnv{
		echo:        e,
		db:          db,
		container:   container,
		cfg:         cfg,
		adminToken:  adminToken,
		viewerToken: viewerToken,
	}
}

// teardownTestEnv cleans up the test environment.
func teardownTestEnv(env *testEnv) {
	if env != nil {
		if env.container != nil && env.container.TsStorage != nil {
			env.container.TsStorage.Close()
		}
		if env.db != nil {
			env.db.Close()
		}
	}
}

// makeRequest creates and executes an HTTP request against the test Echo instance.
func makeRequest(env *testEnv, method, path string, body interface{}, token string) (*httptest.ResponseRecorder, map[string]interface{}) {
	var reqBody *bytes.Buffer
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(jsonBytes)
	} else {
		reqBody = bytes.NewBuffer(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)

	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

// --- Health Check Tests ---

func TestHealthLive(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, resp := makeRequest(env, "GET", "/health/live", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	if resp["status"] != "alive" {
		t.Fatalf("Expected status 'alive', got '%v'", resp["status"])
	}
}

func TestHealthReady(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, resp := makeRequest(env, "GET", "/health/ready", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	if resp["status"] != "ready" {
		t.Fatalf("Expected status 'ready', got '%v'", resp["status"])
	}
}

// --- Authentication Tests ---

func TestLoginSuccess(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	body := map[string]string{
		"username": "admin",
		"password": "Admin@123456",
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/auth/login", body, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %v", rec.Code, resp)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in response")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Fatal("Missing access_token in response")
	}
	if data["refresh_token"] == nil || data["refresh_token"] == "" {
		t.Fatal("Missing refresh_token in response")
	}
}

func TestLoginInvalidCredentials(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	body := map[string]string{
		"username": "admin",
		"password": "wrongpassword",
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/auth/login", body, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401, got %d", rec.Code)
	}
	if resp["error_code"] != "ERR_AUTH_INVALID_CREDENTIALS" {
		t.Fatalf("Expected ERR_AUTH_INVALID_CREDENTIALS, got '%v'", resp["error_code"])
	}
}

func TestLoginEmptyBody(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "POST", "/api/v1/auth/login", nil, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401, got %d", rec.Code)
	}
}

func TestGetCurrentUser(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, resp := makeRequest(env, "GET", "/api/v1/auth/me", nil, env.adminToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %v", rec.Code, resp)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in response")
	}
	if data["username"] != "admin" {
		t.Fatalf("Expected username 'admin', got '%v'", data["username"])
	}
	if data["role"] != "admin" {
		t.Fatalf("Expected role 'admin', got '%v'", data["role"])
	}
}

func TestGetCurrentUserNoAuth(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/api/v1/auth/me", nil, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401, got %d", rec.Code)
	}
}

// --- Device CRUD Integration Tests ---

func TestDeviceCreateAndList(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device
	createBody := map[string]interface{}{
		"device_id": "test-device-001",
		"name":      "Test Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{"timeout": 5.0},
		"points": []map[string]interface{}{
			{"name": "temperature", "data_type": "float", "address": "0", "access_mode": "read"},
		},
		"collect_interval": 5,
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d: %v", rec.Code, resp)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in response")
	}
	if data["device_id"] != "test-device-001" {
		t.Fatalf("Expected device_id 'test-device-001', got '%v'", data["device_id"])
	}

	// List devices
	rec, resp = makeRequest(env, "GET", "/api/v1/devices?page=1&size=20", nil, env.adminToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %v", rec.Code, resp)
	}

	dataList, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("Expected data to be array")
	}
	if len(dataList) == 0 {
		t.Fatal("Expected at least 1 device in list")
	}
}

func TestDeviceGetByID(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device first
	createBody := map[string]interface{}{
		"device_id": "test-device-get",
		"name":      "Get Test Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{"timeout": 5.0},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Get device
	rec, resp := makeRequest(env, "GET", "/api/v1/devices/test-device-get", nil, env.adminToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %v", rec.Code, resp)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in response")
	}
	if data["device_id"] != "test-device-get" {
		t.Fatalf("Expected device_id 'test-device-get', got '%v'", data["device_id"])
	}
}

func TestDeviceGetNotFound(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, resp := makeRequest(env, "GET", "/api/v1/devices/nonexistent-device", nil, env.adminToken)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404, got %d: %v", rec.Code, resp)
	}
}

func TestDeviceDelete(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device
	createBody := map[string]interface{}{
		"device_id": "test-device-delete",
		"name":      "Delete Test Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Delete device
	rec, _ := makeRequest(env, "DELETE", "/api/v1/devices/test-device-delete", nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on delete, got %d", rec.Code)
	}

	// Verify device is gone
	rec, _ = makeRequest(env, "GET", "/api/v1/devices/test-device-delete", nil, env.adminToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 after delete, got %d", rec.Code)
	}
}

func TestDeviceUpdate(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device
	createBody := map[string]interface{}{
		"device_id": "test-device-update",
		"name":      "Before Update",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Update device
	updateBody := map[string]interface{}{
		"name": "After Update",
	}
	rec, resp := makeRequest(env, "PUT", "/api/v1/devices/test-device-update", updateBody, env.adminToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on update, got %d: %v", rec.Code, resp)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in response")
	}
	if data["name"] != "After Update" {
		t.Fatalf("Expected name 'After Update', got '%v'", data["name"])
	}
}

// TestReadOnlyPointWriteRejected covers the server-side access_mode guard.
// Before it, read-only points were only hidden by the device editor's write
// tab, so POST /devices/<id>/points could write any point regardless of its
// declared access mode.
func TestReadOnlyPointWriteRejected(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	createBody := map[string]interface{}{
		"device_id": "test-device-ro-guard",
		"name":      "RO Guard Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{"timeout": 5.0},
		"points": []map[string]interface{}{
			{"name": "temp_read", "data_type": "float32", "address": "0", "access_mode": "read"},
			{"name": "temp_r", "data_type": "float32", "address": "1", "access_mode": "r"},
			{"name": "setpoint", "data_type": "float32", "address": "2", "access_mode": "rw"},
			{"name": "legacy_blank", "data_type": "float32", "address": "3"},
		},
		"collect_interval": 5,
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201 on create, got %d: %v", rec.Code, resp)
	}

	write := func(point string) (int, map[string]interface{}) {
		rec, resp := makeRequest(env, "POST", "/api/v1/devices/test-device-ro-guard/points",
			map[string]interface{}{"point": point, "value": 42.0}, env.adminToken)
		return rec.Code, resp
	}

	// Both read-only spellings must be rejected, with a code the UI can translate.
	for _, point := range []string{"temp_read", "temp_r"} {
		code, resp := write(point)
		if code != http.StatusBadRequest {
			t.Fatalf("write to read-only point %s returned %d, want 400: %v", point, code, resp)
		}
		if resp["error_code"] != "ERR_POINT_READ_ONLY" {
			t.Fatalf("write to %s returned error_code %v, want ERR_POINT_READ_ONLY", point, resp["error_code"])
		}
	}

	// Writable and undeclared points must not be caught by the guard. Whether the
	// simulator driver then accepts the value is the driver's own contract.
	for _, point := range []string{"setpoint", "legacy_blank"} {
		_, resp := write(point)
		if resp["error_code"] == "ERR_POINT_READ_ONLY" {
			t.Fatalf("write to %s was wrongly rejected as read-only", point)
		}
	}
}

// --- Authorization Tests ---

func TestViewerCannotCreateDevice(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	createBody := map[string]interface{}{
		"device_id": "viewer-device-test",
		"name":      "Viewer Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/devices", createBody, env.viewerToken)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Expected 403 for viewer creating device, got %d: %v", rec.Code, resp)
	}
}

func TestViewerCanReadDevices(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Admin creates a device
	createBody := map[string]interface{}{
		"device_id": "viewer-read-test",
		"name":      "Viewer Read Test",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Viewer should be able to list devices
	rec, _ := makeRequest(env, "GET", "/api/v1/devices", nil, env.viewerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 for viewer listing devices, got %d", rec.Code)
	}
}

func TestNoAuthCannotAccessDevices(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/api/v1/devices", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 without auth, got %d", rec.Code)
	}
}

func TestInvalidTokenRejected(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/api/v1/devices", nil, "invalid-jwt-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 with invalid token, got %d", rec.Code)
	}
}

// --- Rule CRUD Integration Tests ---

func TestRuleCreateAndList(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create a device first (rule needs device_id)
	createBody := map[string]interface{}{
		"device_id": "rule-test-device",
		"name":      "Rule Test Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Create rule
	ruleBody := map[string]interface{}{
		"rule_id":   "test-rule-001",
		"name":      "Test Rule",
		"device_id": "rule-test-device",
		"conditions": []interface{}{
			map[string]interface{}{
				"point":     "temperature",
				"operator":  ">",
				"threshold": 80,
			},
		},
		"logic":           "AND",
		"severity":        "critical",
		"enabled":         true,
		"notify_channels": []string{"dingtalk"},
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/rules", ruleBody, env.adminToken)

	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201 on rule create, got %d: %v", rec.Code, resp)
	}

	// List rules
	rec, resp = makeRequest(env, "GET", "/api/v1/rules?page=1&size=20", nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on rule list, got %d: %v", rec.Code, resp)
	}
}

func TestRuleDelete(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device
	createBody := map[string]interface{}{
		"device_id": "rule-delete-device",
		"name":      "Rule Delete Device",
		"protocol":  "simulator",
		"config":    map[string]interface{}{},
		"points":    []interface{}{},
	}
	_, _ = makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Create rule
	ruleBody := map[string]interface{}{
		"name":      "Delete Rule",
		"device_id": "rule-delete-device",
		"conditions": []interface{}{
			map[string]interface{}{"point": "temp", "operator": ">", "threshold": 50},
		},
		"logic":    "AND",
		"severity": "warning",
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/rules", ruleBody, env.adminToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201 on rule create, got %d: %v", rec.Code, resp)
	}
	ruleData, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in rule create response")
	}
	ruleID, ok := ruleData["rule_id"].(string)
	if !ok || ruleID == "" {
		t.Fatal("Missing rule_id in rule create response")
	}

	// Delete rule
	rec, _ = makeRequest(env, "DELETE", "/api/v1/rules/"+ruleID, nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on rule delete, got %d", rec.Code)
	}
}

// --- Security Headers Tests ---

func TestSecurityHeaders(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/health/live", nil, "")

	headers := rec.Header()
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("X-Content-Type-Options header not set")
	}
	if headers.Get("X-Frame-Options") != "DENY" {
		t.Fatal("X-Frame-Options header not set")
	}
	if headers.Get("X-XSS-Protection") != "1; mode=block" {
		t.Fatal("X-XSS-Protection header not set")
	}
}

func TestRequestIDHeader(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/health/live", nil, "")

	reqID := rec.Header().Get("X-Request-ID")
	if reqID == "" {
		t.Fatal("X-Request-ID header not set in response")
	}
}

// --- API Response Format Tests ---

func TestAPIResponseFormat(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Test a standard API endpoint
	rec, resp := makeRequest(env, "GET", "/api/v1/devices", nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	// Should have code field
	if _, ok := resp["code"]; !ok {
		t.Fatal("API response missing 'code' field")
	}
}

// --- Database Health Tests ---

func TestDatabaseHealthAfterOperations(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Perform multiple operations
	for i := 0; i < 5; i++ {
		createBody := map[string]interface{}{
			"device_id": fmt.Sprintf("health-test-device-%d", i),
			"name":      "Health Test Device",
			"protocol":  "simulator",
			"config":    map[string]interface{}{},
			"points":    []interface{}{},
		}
		makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)
	}

	// Database should still be healthy
	if !env.db.IsHealthy() {
		t.Fatal("Database should be healthy after operations")
	}

	// Health endpoint should return ready
	rec, resp := makeRequest(env, "GET", "/health/ready", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %v", rec.Code, resp)
	}
	if resp["status"] != "ready" {
		t.Fatalf("Expected status 'ready', got '%v'", resp["status"])
	}
}

// --- Concurrent Request Tests ---

func TestConcurrentDeviceCreation(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// SQLite has a single-writer constraint; we test sequential creation
	// to verify data integrity under rapid successive operations.
	createdCount := 0
	for i := 0; i < 10; i++ {
		createBody := map[string]interface{}{
			"device_id": fmt.Sprintf("concurrent-device-%d", i),
			"name":      "Concurrent Device",
			"protocol":  "simulator",
			"config":    map[string]interface{}{},
			"points":    []interface{}{},
		}
		rec, _ := makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)
		if rec.Code == http.StatusCreated {
			createdCount++
		}
	}

	if createdCount < 10 {
		t.Fatalf("Expected 10 devices created, got %d", createdCount)
	}

	// Verify all devices were created
	rec, resp := makeRequest(env, "GET", "/api/v1/devices?page=1&size=50", nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	dataList, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("Expected data array")
	}
	if len(dataList) < 10 {
		t.Fatalf("Expected at least 10 devices, got %d", len(dataList))
	}
}

// --- Edge Cases ---

func TestMalformedJSON(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	req := httptest.NewRequest("POST", "/api/v1/devices", strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.adminToken)

	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for malformed JSON, got %d", rec.Code)
	}
}

func TestEmptyDeviceID(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create device without device_id
	createBody := map[string]interface{}{
		"name":     "No ID Device",
		"protocol": "simulator",
		"config":   map[string]interface{}{},
		"points":   []interface{}{},
	}
	rec, _ := makeRequest(env, "POST", "/api/v1/devices", createBody, env.adminToken)

	// Should either succeed (auto-generate ID) or fail with bad request
	if rec.Code != http.StatusCreated && rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 201 or 400, got %d", rec.Code)
	}
}

func TestPasswordChangeFlow(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Change password
	changeBody := map[string]string{
		"old_password": "Admin@123456",
		"new_password": "NewPassword@123456",
	}
	rec, _ := makeRequest(env, "POST", "/api/v1/auth/change-password", changeBody, env.adminToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on password change, got %d", rec.Code)
	}

	// Login with new password
	loginBody := map[string]string{
		"username": "admin",
		"password": "NewPassword@123456",
	}
	rec, _ = makeRequest(env, "POST", "/api/v1/auth/login", loginBody, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 with new password, got %d", rec.Code)
	}

	// Old password should fail
	loginBody = map[string]string{
		"username": "admin",
		"password": "Admin@123456",
	}
	rec, _ = makeRequest(env, "POST", "/api/v1/auth/login", loginBody, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 with old password, got %d", rec.Code)
	}
}

func TestLogoutFlow(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Logout
	rec, _ := makeRequest(env, "POST", "/api/v1/auth/logout", nil, env.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on logout, got %d", rec.Code)
	}
}

// --- Token Refresh Tests ---

func TestTokenRefresh(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Login to get refresh token
	loginBody := map[string]string{
		"username": "admin",
		"password": "Admin@123456",
	}
	_, resp := makeRequest(env, "POST", "/api/v1/auth/login", loginBody, "")

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in login response")
	}
	refreshToken, ok := data["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatal("Missing refresh_token in login response")
	}

	// Refresh token
	refreshBody := map[string]string{
		"refresh": refreshToken,
	}
	rec, resp := makeRequest(env, "POST", "/api/v1/auth/refresh", refreshBody, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on refresh, got %d: %v", rec.Code, resp)
	}

	data, ok = resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in refresh response")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Fatal("Missing access_token in refresh response")
	}
}

// --- Metrics Endpoint Tests ---

func TestMetricsEndpoint(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	rec, _ := makeRequest(env, "GET", "/metrics", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on metrics, got %d", rec.Code)
	}
}

// --- Rate Limiting Tests ---

func TestRateLimiting(t *testing.T) {
	env := setupTestEnv(t)
	defer teardownTestEnv(env)

	// Create a new Echo with low rate limit
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(elware.RequestIDMiddleware())
	limiter := elware.NewRateLimiter(3) // Very low limit
	e.Use(elware.RateLimitMiddleware(limiter))
	e.GET("/test", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Make 4 requests — 4th should be rate limited
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Request %d should succeed, got %d", i+1, rec.Code)
		}
	}

	// 4th request should be blocked
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request should be rate limited, got %d", rec.Code)
	}
}

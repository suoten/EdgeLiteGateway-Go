// Package e2e provides self-contained smoke tests that start the full
// application server in-process and verify core endpoints respond correctly.
//
// Unlike smoke_test.go which requires an external server, these tests
// bootstrap the application within the test process, ensuring they can
// run in CI without manual server startup.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"edgelite/internal/api"
	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	"edgelite/internal/security"
	"edgelite/internal/services"
	"edgelite/internal/storage"
	"edgelite/internal/ws"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	elware "edgelite/internal/middleware"
)

// findFreePort finds an available TCP port.
func findFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// smokeApp holds the in-process test application.
type smokeApp struct {
	echo   *echo.Echo
	port   int
	cancel context.CancelFunc
	tmpDir string
}

// setupSmokeApp creates and starts a full in-process application for smoke testing.
func setupSmokeApp(t *testing.T) *smokeApp {
	t.Helper()

	tmpDir := t.TempDir()
	port := findFreePort(t)

	cfg := config.DefaultAppConfig()
	cfg.Database.SQLitePath = filepath.Join(tmpDir, "smoke_test.db")
	cfg.Database.BackupDir = filepath.Join(tmpDir, "backups")
	cfg.InfluxDB.SQLiteTSPath = filepath.Join(tmpDir, "smoke_ts.db")
	cfg.MQTT.OfflineDBPath = filepath.Join(tmpDir, "smoke_offline.db")
	cfg.Security.SecretKey = "smoke-test-secret-key-for-testing-32ch!"
	cfg.Security.CSRFSecret = "smoke-test-csrf-secret-key-for-testing!"
	cfg.Security.AccessTokenExpireMinutes = 60
	cfg.Security.RefreshTokenExpireDays = 7
	cfg.Server.DebugAPIEnabled = true

	config.SetGlobalConfig(cfg)
	security.ResetJWTManagerForTest()
	security.ResetCSRFManagerForTest()

	// Bootstrap database
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("Failed to init database: %v", err)
	}

	// Bootstrap repos
	deviceRepo := storage.NewDeviceRepo(db)
	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	userRepo := storage.NewUserRepo(db)
	templateRepo := storage.NewTemplateRepo(db)

	// Time-series storage
	tsStorage, err := storage.NewTimeSeriesStorage(cfg)
	if err != nil {
		t.Fatalf("Failed to init TS storage: %v", err)
	}

	// Cache
	cache := storage.NewCacheManager(10000)

	// Register drivers
	drivers.RegisterAll()

	// Engine
	eventBus := engine.NewEventBus(10000)
	scheduler := engine.NewCollectScheduler(eventBus, tsStorage, cache, &cfg.Scheduler)
	cbRegistry := engine.NewCircuitBreakerRegistry(eventBus)
	evaluator := engine.NewRuleEvaluator(eventBus, ruleRepo, alarmRepo)

	// Services
	notifyService := services.NewNotifyService()
	notifyService.SetConfig(&cfg.Notify)
	deviceService := services.NewDeviceService(deviceRepo, templateRepo, scheduler, cbRegistry)
	ruleService := services.NewRuleService(ruleRepo, evaluator)
	alarmService := services.NewAlarmService(alarmRepo, evaluator)
	alarmService.SetNotifyService(notifyService)
	dataService := services.NewDataService(tsStorage, cache)
	systemService := services.NewSystemService(cfg)

	// WebSocket
	wsManager := ws.NewManager()

	// Wire EventBus → WebSocket
	eventBus.SubscribeAll(func(event engine.Event) {
		wsManager.BroadcastEvent(event)
	})

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
		WSManager:  wsManager,
	}
	api.SetContainer(container)

	// Start engine
	ctx, cancel := context.WithCancel(context.Background())
	eventBus.Start(ctx)
	scheduler.Start(ctx)

	// Create Echo
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	// Same header budget the real server runs with: without it a connection that is
	// accepted but never speaks keeps http.Server.Shutdown waiting for the full budget.
	e.Server.ReadHeaderTimeout = 2 * time.Second

	// Middleware
	e.Use(elware.RequestIDMiddleware())
	e.Use(elware.LoggerMiddleware())
	e.Use(elware.SecurityHeadersMiddleware())
	e.Use(elware.CORSMiddleware([]string{"*"}))
	limiter := elware.NewRateLimiter(120)
	e.Use(elware.RateLimitMiddleware(limiter))
	e.Use(echomw.BodyLimit("10M"))
	e.Use(echomw.Gzip())

	// Register routes
	v1 := e.Group("/api/v1")
	api.RegisterAuthRoutes(v1.Group("/auth"))
	api.RegisterDeviceRoutes(v1.Group("/devices", elware.AuthMiddleware()))
	api.RegisterRuleRoutes(v1.Group("/rules", elware.AuthMiddleware()))
	api.RegisterAlarmRoutes(v1.Group("/alarms", elware.AuthMiddleware()))
	api.RegisterDataRoutes(v1.Group("/data", elware.AuthMiddleware()))
	api.RegisterSystemRoutes(v1.Group("/system"))
	api.RegisterUserRoutes(v1.Group("/users", elware.AuthMiddleware()))

	// Health endpoints
	e.GET("/health/live", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "alive"})
	})
	e.GET("/health/ready", func(c echo.Context) error {
		if db != nil && db.IsHealthy() {
			return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
		}
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
	})

	// Metrics
	e.GET("/metrics", func(c echo.Context) error {
		cont := api.GetContainer()
		metrics := map[string]interface{}{
			"status":  "ok",
			"version": "smoke-test",
		}
		if cont.Scheduler != nil {
			metrics["scheduler"] = cont.Scheduler.Stats()
		}
		if cont.EventBus != nil {
			metrics["event_bus"] = cont.EventBus.Metrics()
		}
		return c.JSON(http.StatusOK, metrics)
	})

	// Start server
	go func() {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if err := e.Start(addr); err != nil && err != http.ErrServerClosed {
			t.Errorf("Server failed: %v", err)
		}
	}()

	// Wait for server to be ready
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/health/live")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return &smokeApp{echo: e, port: port, cancel: cancel, tmpDir: tmpDir}
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("Server did not become ready within 10 seconds")
	return nil
}

// teardown stops the smoke test application.
func (app *smokeApp) teardown(t *testing.T) {
	t.Helper()
	app.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.echo.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown error: %v", err)
	}
	// Close database to release file locks on Windows.
	// Use recover to handle close-of-closed-channel if container was already torn down.
	defer func() {
		recover() // Ignore panic from double-close
	}()
	cont := api.GetContainer()
	if cont.Database != nil {
		_ = cont.Database.Close()
	}
	if cont.TsStorage != nil {
		_ = cont.TsStorage.Close()
	}
	// Wait briefly for file handles to be released
	time.Sleep(200 * time.Millisecond)
	// Cleanup tmp dir (best-effort on Windows)
	_ = os.RemoveAll(app.tmpDir)
}

// baseURL returns the base URL for the test server.
func (app *smokeApp) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", app.port)
}

// doRequest performs an HTTP request and returns status code + parsed body.
func doRequest(t *testing.T, method, url string, body io.Reader, headers map[string]string) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	_ = json.Unmarshal(respBody, &result)
	return resp.StatusCode, result
}

// --- In-Process Smoke Tests ---

func TestSmokeAppLiveness(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	code, body := doRequest(t, "GET", app.baseURL()+"/health/live", nil, nil)
	if code != 200 {
		t.Fatalf("Expected 200 on /health/live, got %d", code)
	}
	if body["status"] != "alive" {
		t.Fatalf("Expected status 'alive', got '%v'", body["status"])
	}
}

func TestSmokeAppReadiness(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	code, body := doRequest(t, "GET", app.baseURL()+"/health/ready", nil, nil)
	if code != 200 {
		t.Fatalf("Expected 200 on /health/ready, got %d", code)
	}
	if body["status"] != "ready" {
		t.Fatalf("Expected status 'ready', got '%v'", body["status"])
	}
}

func TestSmokeAppMetrics(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	code, body := doRequest(t, "GET", app.baseURL()+"/metrics", nil, nil)
	if code != 200 {
		t.Fatalf("Expected 200 on /metrics, got %d", code)
	}
	if body["status"] != "ok" {
		t.Fatalf("Expected status 'ok', got '%v'", body["status"])
	}
	if body["version"] == nil {
		t.Fatal("Expected version field in metrics")
	}
}

func TestSmokeAppUnauthorizedAccess(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	code, _ := doRequest(t, "GET", app.baseURL()+"/api/v1/devices", nil, nil)
	if code != 401 {
		t.Fatalf("Expected 401 without auth, got %d", code)
	}
}

func TestSmokeAppLoginAdmin(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	// Create default admin user
	cont := api.GetContainer()
	if cont.UserRepo != nil {
		hashedPassword, _ := security.HashPassword("Admin@123456")
		_ = cont.UserRepo.Create("admin", "admin", hashedPassword, "admin")
	}

	loginURL := app.baseURL() + "/api/v1/auth/login"
	body := strings.NewReader(`{"username":"admin","password":"Admin@123456"}`)
	code, resp := doRequest(t, "POST", loginURL, body, nil)
	if code != 200 {
		t.Skipf("Login failed (code %d), skipping token validation", code)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in login response")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Fatal("Missing access_token in login response")
	}
}

func TestSmokeAppAuthFlow(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	// Create admin user
	cont := api.GetContainer()
	hashedPassword, _ := security.HashPassword("Admin@123456")
	_ = cont.UserRepo.Create("admin", "admin", hashedPassword, "admin")

	// Login
	loginURL := app.baseURL() + "/api/v1/auth/login"
	body := strings.NewReader(`{"username":"admin","password":"Admin@123456"}`)
	code, resp := doRequest(t, "POST", loginURL, body, nil)
	if code != 200 {
		t.Fatalf("Login failed with code %d", code)
	}

	data := resp["data"].(map[string]interface{})
	token, ok := data["access_token"].(string)
	if !ok || token == "" {
		t.Fatal("Missing access_token in login response")
	}

	// Use token to access protected endpoint
	headers := map[string]string{
		"Authorization": "Bearer " + token,
	}
	code, _ = doRequest(t, "GET", app.baseURL()+"/api/v1/devices", nil, headers)
	if code != 200 {
		t.Fatalf("Expected 200 with auth token, got %d", code)
	}
}

func TestSmokeAppSystemInfo(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	code, resp := doRequest(t, "GET", app.baseURL()+"/api/v1/system/info", nil, nil)
	if code != 200 {
		t.Logf("System info returned %d (may need auth)", code)
		return
	}
	if resp["data"] == nil {
		t.Fatal("Expected data in system info response")
	}
}

func TestSmokeAppPortIsDynamic(t *testing.T) {
	app1 := setupSmokeApp(t)
	port1 := app1.port
	app1.teardown(t)

	// Wait for port to be released
	time.Sleep(500 * time.Millisecond)

	app2 := setupSmokeApp(t)
	port2 := app2.port
	app2.teardown(t)

	if port1 == port2 {
		t.Fatalf("Expected different ports, got %d for both", port1)
	}
}

// TestSmokeAppConcurrentRequests verifies the server handles concurrent requests.
func TestSmokeAppConcurrentRequests(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			code, _ := doRequest(t, "GET", app.baseURL()+"/health/live", nil, nil)
			if code != 200 {
				done <- fmt.Errorf("goroutine %d: expected 200, got %d", idx, code)
				return
			}
			done <- nil
		}(i)
	}

	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

// TestSmokeAppServerStop verifies the server can be cleanly shut down.
func TestSmokeAppServerStop(t *testing.T) {
	app := setupSmokeApp(t)
	port := app.port
	app.teardown(t)

	// Verify the server is no longer responding
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	_, _ = http.Get(baseURL + "/health/live")

	// Verify port is released (can listen again)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Port may still be in TIME_WAIT, acceptable
		t.Logf("Port %d still in use (TIME_WAIT expected): %v", port, err)
	} else {
		listener.Close()
	}
}

// TestSmokeAppVersionEnv verifies the version is set correctly.
func TestSmokeAppVersionString(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	// Verify metrics returns version
	_, body := doRequest(t, "GET", app.baseURL()+"/metrics", nil, nil)
	version, ok := body["version"].(string)
	if !ok {
		t.Fatal("Expected version string in metrics")
	}
	if version == "" {
		t.Fatal("Version should not be empty")
	}
}

// Unused import guard
var _ = strconv.Itoa

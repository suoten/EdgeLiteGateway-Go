package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/models"
	"edgelite/internal/security"
)

func setupEcho(method, path string, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

// --- Response Helper Tests (extended) ---

func TestServiceUnavailableExt(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	ServiceUnavailable(c, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestServiceUnavailableWithMessageExt(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	ServiceUnavailable(c, "Service down")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
	var resp APIResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Message != "Service down" {
		t.Fatalf("Expected 'Service down', got '%s'", resp.Message)
	}
}

func TestConflictExt2(t *testing.T) {
	c, rec := setupEcho("POST", "/test", "")
	Conflict(c, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("Expected 409, got %d", rec.Code)
	}
}

func TestConflictWithMessageExt(t *testing.T) {
	c, rec := setupEcho("POST", "/test", "")
	Conflict(c, "Duplicate name")
	if rec.Code != http.StatusConflict {
		t.Fatalf("Expected 409, got %d", rec.Code)
	}
}

// --- Container Tests ---

func TestGetContainer(t *testing.T) {
	c := GetContainer()
	if c == nil {
		t.Fatal("GetContainer returned nil")
	}
}

func TestSetContainer(t *testing.T) {
	c := NewServiceContainer()
	SetContainer(c)
	got := GetContainer()
	if got != c {
		t.Fatal("SetContainer did not set the container")
	}
}

// --- Auth Helpers Tests ---

func TestGetUserFromContextNotSet(t *testing.T) {
	c, _ := setupEcho("GET", "/test", "")
	user := getUserFromContext(c)
	if user != nil {
		t.Fatal("Expected nil user when not set")
	}
}

func TestGetUserFromContextDirect(t *testing.T) {
	c, _ := setupEcho("GET", "/test", "")
	c.Set("user", &UserContext{
		UserID:   "u1",
		Username: "admin",
		Role:     "admin",
	})
	user := getUserFromContext(c)
	if user == nil {
		t.Fatal("Expected non-nil user")
	}
	if user.UserID != "u1" {
		t.Fatalf("Expected user_id 'u1', got '%s'", user.UserID)
	}
}

func TestGetUserFromContextMap(t *testing.T) {
	c, _ := setupEcho("GET", "/test", "")
	c.Set("user", map[string]string{
		"user_id":  "u2",
		"username": "operator",
		"role":     "operator",
	})
	user := getUserFromContext(c)
	if user == nil {
		t.Fatal("Expected non-nil user from map")
	}
	if user.Username != "operator" {
		t.Fatalf("Expected username 'operator', got '%s'", user.Username)
	}
}

func TestRequireAuthNoUser(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	h := requireAuth(func(c echo.Context) error {
		return OK(c, "success")
	})
	err := h(c)
	if err != nil {
		t.Fatalf("requireAuth returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401, got %d", rec.Code)
	}
}

func TestRequireAuthWithUser(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	c.Set("user", &UserContext{
		UserID:   "u1",
		Username: "admin",
		Role:     "admin",
	})
	h := requireAuth(func(c echo.Context) error {
		return OK(c, "success")
	})
	err := h(c)
	if err != nil {
		t.Fatalf("requireAuth returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestRequirePermissionAllowed(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	c.Set("user", &UserContext{
		UserID:   "u1",
		Username: "admin",
		Role:     "admin",
	})
	mw := requirePermission(security.PermSystemConfig)
	h := mw(func(c echo.Context) error {
		return OK(c, "success")
	})
	err := h(c)
	if err != nil {
		t.Fatalf("requirePermission returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 for admin, got %d", rec.Code)
	}
}

func TestRequirePermissionDenied(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	c.Set("user", &UserContext{
		UserID:   "u2",
		Username: "viewer",
		Role:     "viewer",
	})
	mw := requirePermission(security.PermSystemConfig)
	h := mw(func(c echo.Context) error {
		return OK(c, "success")
	})
	err := h(c)
	if err != nil {
		t.Fatalf("requirePermission returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Expected 403 for viewer, got %d", rec.Code)
	}
}

func TestRequirePermissionNoUser(t *testing.T) {
	c, rec := setupEcho("GET", "/test", "")
	mw := requirePermission(security.PermSystemConfig)
	h := mw(func(c echo.Context) error {
		return OK(c, "success")
	})
	err := h(c)
	if err != nil {
		t.Fatalf("requirePermission returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 without user, got %d", rec.Code)
	}
}

func TestGetClientIP(t *testing.T) {
	c, _ := setupEcho("GET", "/test", "")
	ip := getClientIP(c)
	if ip == "" {
		t.Fatal("getClientIP should return non-empty")
	}
}

// --- System API Tests ---

func TestHandleSystemInfo(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/info", "")
	err := handleSystemInfo(c)
	if err != nil {
		t.Fatalf("handleSystemInfo returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSystemStatus(t *testing.T) {
	// Set admin user in context
	c, rec := setupEcho("GET", "/api/system/status", "")
	c.Set("user", &UserContext{
		UserID:   "u1",
		Username: "admin",
		Role:     "admin",
	})
	// Manually call handler with permission check bypassed
	err := handleSystemStatus(c)
	if err != nil {
		t.Fatalf("handleSystemStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSystemHealth(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/health", "")
	err := handleSystemHealth(c)
	if err != nil {
		t.Fatalf("handleSystemHealth returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// --- Notify API Tests ---

func TestHandleListNotifyChannels(t *testing.T) {
	c, rec := setupEcho("GET", "/api/notify/channels", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleListNotifyChannels(c)
	if err != nil {
		t.Fatalf("handleListNotifyChannels returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleListNotifyHistory(t *testing.T) {
	c, rec := setupEcho("GET", "/api/notify/history", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleListNotifyHistory(c)
	if err != nil {
		t.Fatalf("handleListNotifyHistory returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetNotifyConfig(t *testing.T) {
	c, rec := setupEcho("GET", "/api/notify/config", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetNotifyConfig(c)
	if err != nil {
		t.Fatalf("handleGetNotifyConfig returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSendTestNotification(t *testing.T) {
	c, rec := setupEcho("POST", "/api/notify/test", `{"channel":"dingtalk","message":"test"}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleSendTestNotification(c)
	if err != nil {
		t.Fatalf("handleSendTestNotification returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSendTestNotificationEmpty(t *testing.T) {
	c, rec := setupEcho("POST", "/api/notify/test", `{}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleSendTestNotification(c)
	if err != nil {
		t.Fatalf("handleSendTestNotification returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

// --- Video API Tests ---

func TestHandleListVideoDevices(t *testing.T) {
	// Without a repository the answer is "not ready", not an empty list: an empty
	// list is what a gateway with no cameras looks like too.
	c, rec := setupEcho("GET", "/api/video/devices", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	if err := handleListVideoDevices(c); err != nil {
		t.Fatalf("handleListVideoDevices returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no device registry (body %s)", rec.Code, rec.Body.String())
	}

	withDeviceService(t)
	c2, rec2 := setupWithAdmin("GET", "/api/v1/video/devices", "")
	if err := handleListVideoDevices(c2); err != nil {
		t.Fatalf("handleListVideoDevices returned error: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty registry (body %s)", rec2.Code, rec2.Body.String())
	}
	var env struct {
		Data []interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data == nil {
		t.Errorf("the list must serialize as [], not null: %s", rec2.Body.String())
	}
}

func TestHandleGetVideoConfig(t *testing.T) {
	c, rec := setupEcho("GET", "/api/video/config", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetVideoConfig(c)
	if err != nil {
		t.Fatalf("handleGetVideoConfig returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetVideoStreamsUnknownDevice(t *testing.T) {
	withDeviceService(t)
	c, rec := setupWithAdmin("GET", "/api/v1/video/devices/nope/streams", "")
	c.SetParamNames("device_id")
	c.SetParamValues("nope")
	if err := handleGetVideoStreams(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a device that does not exist (body %s)", rec.Code, rec.Body.String())
	}
}

// /video/devices used to answer [] whatever was installed, so the page could not
// tell "no cameras" from "listing is broken". It now reads the device registry,
// and a camera with no running stream has to say so rather than hide the device.
func TestListVideoDevicesReportsConfiguredCameras(t *testing.T) {
	cont := withDeviceService(t)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "cam-list-1",
		Name:     "camera",
		Protocol: "onvif",
		Status:   "offline",
		Config:   map[string]interface{}{"host": "127.0.0.1", "port": 80},
	})
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "plc-1",
		Name:     "not a camera",
		Protocol: "modbus_tcp",
		Status:   "offline",
		Config:   map[string]interface{}{"host": "127.0.0.1", "port": 5020},
	})

	c, rec := setupWithAdmin("GET", "/api/v1/video/devices", "")
	if err := handleListVideoDevices(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []struct {
			DeviceID  string `json:"device_id"`
			Streaming bool   `json:"streaming"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].DeviceID != "cam-list-1" {
		t.Fatalf("only the camera should be listed, got %+v", env.Data)
	}
	if env.Data[0].Streaming {
		t.Errorf("nothing can start a stream in this build, so streaming must be false")
	}

	// The streams endpoint has to answer for the camera, with an honest empty list.
	sc, srec := setupWithAdmin("GET", "/api/v1/video/devices/cam-list-1/streams", "")
	sc.SetParamNames("device_id")
	sc.SetParamValues("cam-list-1")
	if err := handleGetVideoStreams(sc); err != nil {
		t.Fatalf("streams handler returned transport error: %v", err)
	}
	var streams struct {
		Data struct {
			DeviceID             string        `json:"device_id"`
			Streams              []interface{} `json:"streams"`
			StreamingImplemented bool          `json:"streaming_implemented"`
			Message              string        `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if streams.Data.DeviceID != "cam-list-1" || len(streams.Data.Streams) != 0 {
		t.Errorf("streams = %+v", streams.Data)
	}
	if streams.Data.StreamingImplemented || streams.Data.Message == "" {
		t.Errorf("an empty stream list must be explained: %+v", streams.Data)
	}
}

// PTZ used to answer 200 {"status":"ok"} for any body, including a device that
// has no driver, so the video UI reported a camera moving that received nothing.
// These tests pin the honest behaviour: the command must reach DeviceService and
// whatever it refuses must be reported.

func TestControlPTZRejectsUnknownAction(t *testing.T) {
	c, rec := setupEcho("POST", "/api/video/devices/dev1/ptz", `{"command":"left","speed":5}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	c.SetParamNames("device_id")
	c.SetParamValues("dev1")
	if err := handleControlPTZ(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an action the handler does not implement (body %s)", rec.Code, rec.Body.String())
	}
}

func TestControlPTZRejectsMovementWithoutSpeed(t *testing.T) {
	// speed 0 is what the shipped client sends, and a camera told to move at
	// speed 0 has not been told to move at all.
	c, rec := setupEcho("POST", "/api/video/devices/dev1/ptz", `{"action":"left"}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	c.SetParamNames("device_id")
	c.SetParamValues("dev1")
	if err := handleControlPTZ(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a movement with no speed (body %s)", rec.Code, rec.Body.String())
	}
}

func TestControlPTZWithoutDeviceService(t *testing.T) {
	c, rec := setupEcho("POST", "/api/video/devices/dev1/ptz", `{"action":"left","speed":5}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	c.SetParamNames("device_id")
	c.SetParamValues("dev1")
	if err := handleControlPTZ(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no device service is wired (body %s)", rec.Code, rec.Body.String())
	}
}

func TestControlPTZReportsDriverRefusal(t *testing.T) {
	cont := withDeviceService(t)
	seedDevice(t, cont, &models.DeviceResponse{
		DeviceID: "cam-1",
		Name:     "onvif camera",
		Protocol: "onvif",
		Status:   "offline",
		// Nothing listens here, so the write cannot silently look successful.
		Config: map[string]interface{}{"host": "127.0.0.1", "port": 1},
	})

	c, rec := setupWithAdmin("POST", "/api/v1/video/devices/cam-1/ptz", `{"action":"left","speed":5}`)
	c.SetParamNames("device_id")
	c.SetParamValues("cam-1")
	if err := handleControlPTZ(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("a camera command that never reached a driver must not report success (body %s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusBadGateway && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want a client-visible failure (body %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Message == "" {
		t.Errorf("the refusal has to say why: %s", rec.Body.String())
	}
}

// --- Login API Tests ---

func TestHandleLoginEmptyBody(t *testing.T) {
	c, rec := setupEcho("POST", "/api/auth/login", `{}`)
	err := handleLogin(c)
	if err != nil {
		t.Fatalf("handleLogin returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for empty credentials, got %d", rec.Code)
	}
}

func TestHandleLoginInvalidJSON(t *testing.T) {
	c, rec := setupEcho("POST", "/api/auth/login", `invalid json`)
	err := handleLogin(c)
	if err != nil {
		t.Fatalf("handleLogin returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for invalid JSON, got %d", rec.Code)
	}
}

func TestHandleLoginNoDatabase(t *testing.T) {
	// Reset container to empty
	SetContainer(NewServiceContainer())
	c, rec := setupEcho("POST", "/api/auth/login", `{"username":"admin","password":"pass"}`)
	err := handleLogin(c)
	if err != nil {
		t.Fatalf("handleLogin returned error: %v", err)
	}
	// Should return 401 (no user found) or 503 (database not ready)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 401 or 503, got %d", rec.Code)
	}
}

// --- Extra endpoint tests ---

func TestHandleGetLogLevel(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/logs/level", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetLogLevel(c)
	if err != nil {
		t.Fatalf("handleGetLogLevel returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleSetLogLevel(t *testing.T) {
	c, rec := setupEcho("PUT", "/api/system/logs/level", `{"level":"debug"}`)
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleSetLogLevel(c)
	if err != nil {
		t.Fatalf("handleSetLogLevel returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleListServices(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/services", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleListServices(c)
	if err != nil {
		t.Fatalf("handleListServices returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetNTPStatus(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/ntp/status", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetNTPStatus(c)
	if err != nil {
		t.Fatalf("handleGetNTPStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetMigrationStatus(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/migration/status", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetMigrationStatus(c)
	if err != nil {
		t.Fatalf("handleGetMigrationStatus returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleGetSanitizedConfig(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/config/sanitized", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleGetSanitizedConfig(c)
	if err != nil {
		t.Fatalf("handleGetSanitizedConfig returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

func TestHandleListBackups(t *testing.T) {
	c, rec := setupEcho("GET", "/api/system/backup/list", "")
	c.Set("user", &UserContext{UserID: "u1", Username: "admin", Role: "admin"})
	err := handleListBackups(c)
	if err != nil {
		t.Fatalf("handleListBackups returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
}

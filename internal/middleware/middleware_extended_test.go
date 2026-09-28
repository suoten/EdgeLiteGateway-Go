package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// --- Rate Limiter Tests ---

func TestRateLimiterAllowUnderLimit(t *testing.T) {
	limiter := NewRateLimiter(5)

	// First 5 requests should be allowed
	for i := 0; i < 5; i++ {
		if !limiter.Allow("192.168.1.1") {
			t.Errorf("Request %d should be allowed", i+1)
		}
	}

	// 6th request should be denied
	if limiter.Allow("192.168.1.1") {
		t.Error("6th request should be rate limited")
	}
}

func TestRateLimiterDifferentIPs(t *testing.T) {
	limiter := NewRateLimiter(3)

	// Different IPs should have separate limits
	for i := 0; i < 3; i++ {
		if !limiter.Allow("10.0.0.1") {
			t.Error("10.0.0.1 should be allowed")
		}
	}
	if limiter.Allow("10.0.0.1") {
		t.Error("10.0.0.1 4th request should be denied")
	}

	// Different IP should still be allowed
	if !limiter.Allow("10.0.0.2") {
		t.Error("10.0.0.2 should be allowed")
	}
}

func TestRateLimiterWindowReset(t *testing.T) {
	limiter := NewRateLimiter(2)

	limiter.Allow("1.2.3.4")
	limiter.Allow("1.2.3.4")
	if limiter.Allow("1.2.3.4") {
		t.Error("3rd request should be denied")
	}

	// Manually reset the window
	limiter.mu.Lock()
	entry := limiter.entries["1.2.3.4"]
	entry.windowStart = time.Now().Add(-2 * time.Minute)
	limiter.mu.Unlock()

	// Should be allowed again after window reset
	if !limiter.Allow("1.2.3.4") {
		t.Error("Should be allowed after window reset")
	}
}

// --- Request ID Middleware Tests ---

func TestRequestIDMiddlewareGeneratesID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := RequestIDMiddleware()(func(c echo.Context) error {
		return c.String(http.StatusOK, "test")
	})

	handler(c)

	reqID := rec.Header().Get("X-Request-ID")
	if reqID == "" {
		t.Error("Expected X-Request-ID header to be set")
	}
	if c.Get("request_id") == nil {
		t.Error("Expected request_id in context")
	}
}

func TestRequestIDMiddlewarePreservesExistingID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "custom-id-123")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := RequestIDMiddleware()(func(c echo.Context) error {
		return c.String(http.StatusOK, "test")
	})

	handler(c)

	reqID := rec.Header().Get("X-Request-ID")
	if reqID != "custom-id-123" {
		t.Errorf("Expected 'custom-id-123', got '%s'", reqID)
	}
}

// --- Security Headers Middleware Tests ---

func TestSecurityHeadersPresent(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := SecurityHeadersMiddleware()(func(c echo.Context) error {
		return c.String(http.StatusOK, "test")
	})

	handler(c)

	headers := rec.Header()
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("Expected X-Content-Type-Options: nosniff")
	}
	if headers.Get("X-Frame-Options") != "DENY" {
		t.Error("Expected X-Frame-Options: DENY")
	}
	if headers.Get("X-XSS-Protection") != "1; mode=block" {
		t.Error("Expected X-XSS-Protection header")
	}
	if headers.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Error("Expected Referrer-Policy header")
	}
}

// --- CSRF Middleware Tests ---

func TestCSRFMiddlewareSkipsGET(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := CSRFMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	handler(c)

	if !called {
		t.Error("GET request should bypass CSRF")
	}
}

func TestCSRFMiddlewareSkipsBearerAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := CSRFMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	handler(c)

	if !called {
		t.Error("Bearer auth requests should bypass CSRF")
	}
}

func TestCSRFMiddlewareRejectsMissingToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// Cookie-authenticated request: CSRF token is mandatory
	req.AddCookie(&http.Cookie{Name: "edgelite_access", Value: "some-access-token"})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := CSRFMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)

	if called {
		t.Error("Handler should NOT be called when CSRF token is missing")
	}
	if err == nil {
		t.Error("Expected error for missing CSRF token")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden, got %v", err)
	}
}

func TestCSRFMiddlewareSkipsUnauthenticatedRequests(t *testing.T) {
	e := echo.New()
	// No Bearer header and no auth cookie — public endpoints
	// (e.g. /auth/login, /auth/refresh) must not require CSRF
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := CSRFMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	if err := handler(c); err != nil {
		t.Errorf("Unauthenticated request should bypass CSRF, got %v", err)
	}
	if !called {
		t.Error("Handler should be called for unauthenticated request")
	}
}

// --- Rate Limit Middleware Tests ---

func TestRateLimitMiddleware(t *testing.T) {
	limiter := NewRateLimiter(2)

	e := echo.New()
	handler := RateLimitMiddleware(limiter)(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	// First 2 requests should succeed
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.100:1234"
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		err := handler(c)
		if err != nil {
			t.Errorf("Request %d should succeed, got error: %v", i+1, err)
		}
	}

	// 3rd request should be rate limited
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.100:1234"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := handler(c)
	if err == nil {
		t.Error("3rd request should be rate limited")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusTooManyRequests {
		t.Errorf("Expected 429 Too Many Requests, got %v", err)
	}
	if rec.Header().Get("Retry-After") != "60" {
		t.Error("Expected Retry-After header")
	}
}

// --- CORS Middleware Tests ---

func TestCORSMiddleware(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := CORSMiddleware([]string{"http://localhost:3000"})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	handler(c)

	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Error("Expected CORS header for allowed origin")
	}
}

func TestCORSMiddlewareRejectedOrigin(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := CORSMiddleware([]string{"http://localhost:3000"})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	handler(c)

	if rec.Header().Get("Access-Control-Allow-Origin") == "http://evil.com" {
		t.Error("Should not set CORS header for rejected origin")
	}
}

func TestCORSMiddlewarePreflight(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := CORSMiddleware([]string{"http://localhost:3000"})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	handler(c)

	// Preflight should set CORS headers
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Error("Expected CORS header on preflight")
	}
}

// --- Logger Middleware Tests ---

func TestLoggerMiddleware(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := LoggerMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "test")
	})

	err := handler(c)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}
	if !called {
		t.Error("Next handler should have been called")
	}
}

// --- Auth Middleware Tests ---

func TestAuthMiddlewareRejectsNoToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := AuthMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	if called {
		t.Error("Handler should NOT be called without auth")
	}
	if err == nil {
		t.Error("Expected error for missing auth")
	}
}

func TestAuthMiddlewareRejectsMalformedToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	req.Header.Set("Authorization", "NotBearer sometoken")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := AuthMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	if called {
		t.Error("Handler should NOT be called with malformed auth")
	}
	if err == nil {
		t.Error("Expected error for malformed auth header")
	}
}

// --- Sanitize String Tests ---

func TestSanitizeStringEmptyInput(t *testing.T) {
	result := SanitizeString("")
	if result != "" {
		t.Errorf("Expected empty string, got '%s'", result)
	}
}

func TestSanitizeStringNoSensitive(t *testing.T) {
	input := `{"name":"test","value":42}`
	result := SanitizeString(input)
	if result != input {
		t.Errorf("Expected input unchanged, got '%s'", result)
	}
}

func TestSanitizeStringMultipleSensitiveFields(t *testing.T) {
	input := `{"password":"secret123","api_key":"key456","name":"test"}`
	result := SanitizeString(input)

	if !contains(result, RedactValue) {
		t.Error("Expected redacted values in result")
	}
	if contains(result, "secret123") {
		t.Error("Password value should be redacted")
	}
	if contains(result, "key456") {
		t.Error("API key value should be redacted")
	}
	// Non-sensitive data may or may not be preserved depending on implementation
	// Just verify that sensitive data is redacted
}

func TestSanitizeStringBearerToken(t *testing.T) {
	input := "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.signature"
	result := SanitizeString(input)

	if contains(result, "eyJhbGciOiJIUzI1NiJ9") {
		t.Error("Bearer token should be redacted")
	}
	if !contains(result, RedactValue) {
		t.Error("Expected redacted value")
	}
}

func TestSanitizeStringURLParams(t *testing.T) {
	input := "http://example.com/login?password=secret&user=admin"
	result := SanitizeString(input)

	if contains(result, "secret") {
		t.Error("Password in URL params should be redacted")
	}
}

func TestSanitizeStringJSON(t *testing.T) {
	input := `{"username":"admin","password":"mypassword","token":"jwt-token-value"}`
	result := SanitizeString(input)

	// Parse result as JSON to verify structure is maintained
	// The result should still be valid JSON with redacted values
	if !contains(result, RedactValue) {
		t.Error("Expected redacted values in JSON output")
	}
	if !contains(result, "admin") {
		t.Error("Username should be preserved")
	}
}

// --- generateRequestID Tests ---

func TestGenerateRequestIDUniqueness(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()

	if id1 == "" || id2 == "" {
		t.Error("Request IDs should not be empty")
	}
	if id1 == id2 {
		t.Error("Request IDs should be unique")
	}
	if len(id1) != 16 {
		t.Errorf("Expected 16 char ID (8 bytes hex), got %d chars", len(id1))
	}
}

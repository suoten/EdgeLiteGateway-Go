package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// --- Helper functions ---

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func newTestRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func newTestResponseRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func TestSanitizeStringPassword(t *testing.T) {
	input := `{"password":"secret123","name":"test"}`
	result := SanitizeString(input)
	if result == input {
		t.Fatal("Password was not redacted")
	}
	if !contains(result, RedactValue) {
		t.Fatal("Redacted value not found in result")
	}
}

func TestSanitizeStringToken(t *testing.T) {
	input := `{"token":"abc123def456"}`
	result := SanitizeString(input)
	if result == input {
		t.Fatal("Token was not redacted")
	}
}

func TestSanitizeStringBearer(t *testing.T) {
	input := "Authorization: Bearer abc123def456ghi"
	result := SanitizeString(input)
	if result == input {
		t.Fatal("Bearer token was not redacted")
	}
}

func TestSanitizeStringApiKey(t *testing.T) {
	input := `{"api_key":"sk-1234567890abcdef"}`
	result := SanitizeString(input)
	if result == input {
		t.Fatal("API key was not redacted")
	}
}

func TestSanitizeStringNoSensitiveData(t *testing.T) {
	input := `{"name":"test","value":42}`
	result := SanitizeString(input)
	if result != input {
		t.Fatal("Non-sensitive data was modified")
	}
}

func TestSanitizeStringEmpty(t *testing.T) {
	result := SanitizeString("")
	if result != "" {
		t.Fatal("Empty string should return empty")
	}
}

// --- SanitizeMap Tests ---

func TestSanitizeMapPassword(t *testing.T) {
	m := map[string]interface{}{
		"password": "secret123",
		"name":     "test",
	}
	result := SanitizeMap(m)
	if result["password"] != RedactValue {
		t.Fatal("Password was not redacted in map")
	}
	if result["name"] != "test" {
		t.Fatal("Non-sensitive field was modified")
	}
}

func TestSanitizeMapNested(t *testing.T) {
	m := map[string]interface{}{
		"user": map[string]interface{}{
			"password": "secret",
			"name":     "test",
		},
	}
	result := SanitizeMap(m)
	user, ok := result["user"].(map[string]interface{})
	if !ok {
		t.Fatal("Nested map not found")
	}
	if user["password"] != RedactValue {
		t.Fatal("Nested password was not redacted")
	}
}

func TestSanitizeMapArray(t *testing.T) {
	m := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"password": "p1", "name": "n1"},
			map[string]interface{}{"password": "p2", "name": "n2"},
		},
	}
	result := SanitizeMap(m)
	items, ok := result["items"].([]interface{})
	if !ok {
		t.Fatal("Array not found")
	}
	for i, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("Item %d is not a map", i)
		}
		if m["password"] != RedactValue {
			t.Fatalf("Password in item %d was not redacted", i)
		}
	}
}

// --- SanitizeJSON Tests ---

func TestSanitizeJSON(t *testing.T) {
	input := []byte(`{"password":"secret","name":"test"}`)
	result := SanitizeJSON(input)
	if len(result) == 0 {
		t.Fatal("SanitizeJSON returned empty")
	}
	if !contains(string(result), RedactValue) {
		t.Fatal("Password was not redacted in JSON")
	}
}

func TestSanitizeJSONEmpty(t *testing.T) {
	result := SanitizeJSON([]byte{})
	if len(result) != 0 {
		t.Fatal("Empty JSON should return empty")
	}
}

func TestSanitizeJSONInvalid(t *testing.T) {
	input := []byte(`password=secret123`)
	result := SanitizeJSON(input)
	// Should still sanitize even invalid JSON
	if !contains(string(result), RedactValue) {
		t.Fatal("Invalid JSON was not sanitized")
	}
}

// --- isSensitiveField Tests ---

func TestIsSensitiveField(t *testing.T) {
	sensitive := []string{"password", "token", "api_key", "secret", "Authorization"}
	for _, field := range sensitive {
		if !isSensitiveField(field) {
			t.Fatalf("Field '%s' should be sensitive", field)
		}
	}
}

func TestIsSensitiveFieldNonSensitive(t *testing.T) {
	nonSensitive := []string{"name", "value", "description", "id"}
	for _, field := range nonSensitive {
		if isSensitiveField(field) {
			t.Fatalf("Field '%s' should not be sensitive", field)
		}
	}
}

func TestIsSensitiveFieldCaseInsensitive(t *testing.T) {
	if !isSensitiveField("PASSWORD") {
		t.Fatal("Uppercase PASSWORD should be sensitive")
	}
	if !isSensitiveField("Api_Key") {
		t.Fatal("Mixed case Api_Key should be sensitive")
	}
}

// --- isSensitiveField partial match Tests ---

func TestIsSensitiveFieldPartialMatch(t *testing.T) {
	if !isSensitiveField("mqtt_password") {
		t.Fatal("mqtt_password should be sensitive (contains 'password')")
	}
	if !isSensitiveField("auth_token") {
		t.Fatal("auth_token should be sensitive (contains 'token')")
	}
}

// --- RateLimiter Tests ---

func TestNewRateLimiter(t *testing.T) {
	rl := NewRateLimiter(60)
	if rl == nil {
		t.Fatal("NewRateLimiter returned nil")
	}
}

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(5)
	for i := 0; i < 5; i++ {
		if !rl.Allow("192.168.1.1") {
			t.Fatalf("Request %d should be allowed", i+1)
		}
	}
	// 6th request should be blocked
	if rl.Allow("192.168.1.1") {
		t.Fatal("6th request should be blocked")
	}
}

func TestRateLimiterDifferentKeys(t *testing.T) {
	rl := NewRateLimiter(2)
	if !rl.Allow("ip1") {
		t.Fatal("ip1 first request should be allowed")
	}
	if !rl.Allow("ip2") {
		t.Fatal("ip2 first request should be allowed")
	}
	if !rl.Allow("ip1") {
		t.Fatal("ip1 second request should be allowed")
	}
	if rl.Allow("ip1") {
		t.Fatal("ip1 third request should be blocked")
	}
	if !rl.Allow("ip2") {
		t.Fatal("ip2 second request should be allowed")
	}
}

// --- RequestIDMiddleware Tests ---

func TestGenerateRequestID(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()
	if id1 == id2 {
		t.Fatal("Request IDs should be unique")
	}
	if len(id1) != 16 {
		t.Fatalf("Request ID should be 16 chars (8 bytes hex), got %d", len(id1))
	}
}

// --- Echo context test (basic) ---

func TestRequestIDMiddleware(t *testing.T) {
	e := echo.New()
	req := newTestRequest("GET", "/test")
	rec := newTestResponseRecorder()
	c := e.NewContext(req, rec)

	mw := RequestIDMiddleware()
	handlerCalled := false
	err := mw(func(c echo.Context) error {
		handlerCalled = true
		reqID := c.Get("request_id")
		if reqID == nil || reqID == "" {
			t.Fatal("Request ID not set in context")
		}
		return nil
	})(c)

	if err != nil {
		t.Fatalf("Middleware returned error: %v", err)
	}
	if !handlerCalled {
		t.Fatal("Handler was not called")
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	e := echo.New()
	req := newTestRequest("GET", "/test")
	rec := newTestResponseRecorder()
	c := e.NewContext(req, rec)

	mw := SecurityHeadersMiddleware()
	err := mw(func(c echo.Context) error {
		return nil
	})(c)

	if err != nil {
		t.Fatalf("Middleware returned error: %v", err)
	}

	headers := rec.Header()
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("X-Content-Type-Options header not set")
	}
	if headers.Get("X-Frame-Options") != "DENY" {
		t.Fatal("X-Frame-Options header not set")
	}
}

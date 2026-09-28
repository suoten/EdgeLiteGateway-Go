package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func setupEchoContext(method, path string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

func TestOK(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	data := map[string]string{"key": "value"}
	err := OK(c, data)
	if err != nil {
		t.Fatalf("OK returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Code != 0 {
		t.Fatalf("Expected code 0, got %d", resp.Code)
	}
	if resp.Message != "success" {
		t.Fatalf("Expected 'success', got %s", resp.Message)
	}
}

func TestOKPaged(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	err := OKPaged(c, []string{"item1"}, 100, 1, 20)
	if err != nil {
		t.Fatalf("OKPaged returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}
	var resp PagedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Total != 100 {
		t.Fatalf("Expected total 100, got %d", resp.Total)
	}
	if resp.Page != 1 {
		t.Fatalf("Expected page 1, got %d", resp.Page)
	}
	if resp.Size != 20 {
		t.Fatalf("Expected size 20, got %d", resp.Size)
	}
}

func TestCreated(t *testing.T) {
	c, rec := setupEchoContext("POST", "/test")
	err := Created(c, map[string]string{"id": "123"})
	if err != nil {
		t.Fatalf("Created returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", rec.Code)
	}
}

func TestErrorMsg(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	err := ErrorMsg(c, http.StatusBadRequest, "invalid input")
	if err != nil {
		t.Fatalf("ErrorMsg returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Message != "invalid input" {
		t.Fatalf("Expected 'invalid input', got %s", resp.Message)
	}
}

func TestErrorCode(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	err := ErrorCode(c, http.StatusBadRequest, "ERR_VALIDATION", "validation failed")
	if err != nil {
		t.Fatalf("ErrorCode returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.ErrorCode != "ERR_VALIDATION" {
		t.Fatalf("Expected 'ERR_VALIDATION', got %s", resp.ErrorCode)
	}
}

func TestNotFound(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	NotFound(c, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404, got %d", rec.Code)
	}
}

func TestNotFoundWithMessage(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	NotFound(c, "Device not found")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Expected 404, got %d", rec.Code)
	}
	var resp Response
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Message != "Device not found" {
		t.Fatalf("Expected 'Device not found', got %s", resp.Message)
	}
}

func TestBadRequest(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	BadRequest(c, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d", rec.Code)
	}
}

func TestForbidden(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	Forbidden(c, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Expected 403, got %d", rec.Code)
	}
}

func TestUnauthorized(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	Unauthorized(c, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401, got %d", rec.Code)
	}
}

func TestInternalError(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	InternalError(c, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Expected 500, got %d", rec.Code)
	}
}

func TestServiceUnavailable(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	ServiceUnavailable(c, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503, got %d", rec.Code)
	}
}

func TestConflict(t *testing.T) {
	c, rec := setupEchoContext("GET", "/test")
	Conflict(c, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("Expected 409, got %d", rec.Code)
	}
}

func TestNewServiceContainer(t *testing.T) {
	c := NewServiceContainer()
	if c == nil {
		t.Fatal("NewServiceContainer returned nil")
	}
}

func TestGetSetContainer(t *testing.T) {
	c := NewServiceContainer()
	SetContainer(c)
	got := GetContainer()
	if got != c {
		t.Fatal("GetContainer should return the same container set by SetContainer")
	}
}

func TestGetContainerUninitialized(t *testing.T) {
	// Reset the container to nil to test the auto-creation path
	container = nil
	c := GetContainer()
	if c == nil {
		t.Fatal("GetContainer should create empty container if not initialized")
	}
}

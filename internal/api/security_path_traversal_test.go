package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

// TestPathTraversalDeleteBackup verifies that path traversal attempts
// in the delete backup endpoint are properly blocked.
func TestPathTraversalDeleteBackup(t *testing.T) {
	e := echo.New()

	// Register the system routes with auth bypassed for testing
	systemGroup := e.Group("/api/v1/system")
	systemGroup.DELETE("/backup/:filename", handleDeleteBackup)

	// Test various path traversal attempts
	traversalAttempts := []string{
		"../../../etc/passwd",
		"..%2f..%2f..%2fetc%2fpasswd",
		"....//....//....//etc/passwd",
		"..\\..\\..\\windows\\system32",
	}

	for _, attempt := range traversalAttempts {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/system/backup/"+attempt, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		// Should return 400 Bad Request, not 200 or 500
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"Path traversal attempt '%s' should be blocked", attempt)
	}
}

// TestPathTraversalRestoreBackup verifies that path traversal attempts
// in the restore backup endpoint are properly blocked.
func TestPathTraversalRestoreBackup(t *testing.T) {
	e := echo.New()

	systemGroup := e.Group("/api/v1/system")
	systemGroup.POST("/backup/restore", handleRestoreBackup)

	traversalAttempts := []string{
		`{"filename":"../../../etc/passwd"}`,
		`{"filename":"..\\..\\..\\windows\\system32"}`,
		`{"filename":"../../etc/shadow"}`,
	}

	for _, body := range traversalAttempts {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/backup/restore",
			strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"Path traversal attempt should be blocked for body: %s", body)
	}
}

// TestPathTraversalUpdateConfig verifies that path traversal attempts
// in the update config endpoint are properly blocked.
func TestPathTraversalUpdateConfig(t *testing.T) {
	e := echo.New()

	systemGroup := e.Group("/api/v1/system")
	systemGroup.PUT("/config", handleUpdateConfig)

	traversalPaths := []string{
		"?path=../../../etc/passwd",
		"?path=../../etc/shadow",
		"?path=../data/edgelite.db",
	}

	for _, path := range traversalPaths {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/system/config"+path, nil)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"Path traversal attempt should be blocked for path: %s", path)
	}
}

// TestFileUploadExtensionValidation verifies that only allowed file extensions
// are accepted for AI model upload.
func TestFileUploadExtensionValidation(t *testing.T) {
	e := echo.New()

	aiGroup := e.Group("/api/v1/ai")
	aiGroup.POST("/models/upload", handleUploadAIModel)

	// Test invalid extension
	body := "--boundary\r\n" +
		"Content-Disposition: form-data; name=\"model_name\"\r\n" +
		"\r\n" +
		"test-model\r\n" +
		"--boundary\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"malicious.exe\"\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n" +
		"fake content\r\n" +
		"--boundary--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/models/upload",
		strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, "multipart/form-data; boundary=boundary")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"Malicious file extension .exe should be rejected")
}

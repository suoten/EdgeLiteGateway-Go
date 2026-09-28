// Package e2e provides end-to-end smoke tests for EdgeLite Gateway.
//
// These tests start the full application server and verify
// that core endpoints respond correctly without a test framework
// harness — mimicking real deployment behavior.
//
// Unlike inprocess_smoke_test.go which bootstraps the app in-process,
// these tests target an externally running server (default localhost:8080).
// They are automatically skipped if no EdgeLite server is detected.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// getBaseURL returns the base URL for testing.
// Uses EDGELITE_TEST_URL env var or defaults to http://localhost:8080.
func getBaseURL() string {
	url := os.Getenv("EDGELITE_TEST_URL")
	if url == "" {
		url = "http://localhost:8080"
	}
	return url
}

// isEdgeLiteServer checks whether the server at baseURL is an EdgeLite Gateway
// instance by verifying the /health/live endpoint returns the expected response.
func isEdgeLiteServer(baseURL string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(baseURL + "/health/live")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	status, ok := result["status"].(string)
	return ok && status == "alive"
}

// checkServerRunning skips the test if no EdgeLite server is running.
// It verifies not just that a port is open, but that the server responds
// as an EdgeLite Gateway instance.
func checkServerRunning(t *testing.T, baseURL string) {
	t.Helper()
	// First check if the port is even open
	u := strings.TrimPrefix(baseURL, "http://")
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		// Fallback: try direct URL check
		if !isEdgeLiteServer(baseURL) {
			t.Skip("EdgeLite server not running, skipping E2E test. Start with: go run ./cmd/edgelite")
		}
		return
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		t.Skip("EdgeLite server not running, skipping E2E test. Start with: go run ./cmd/edgelite")
	}
	conn.Close()
	// Port is open — verify it's actually an EdgeLite server
	if !isEdgeLiteServer(baseURL) {
		t.Skip("Port is open but server is not EdgeLite Gateway, skipping E2E test")
	}
}

// waitForServer waits for the server to be ready.
func waitForServer(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/health/live")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Server did not become ready within %v", timeout)
}

// makeRequest performs an HTTP request and returns status code + parsed body.
func makeRequest(t *testing.T, method, url string, body io.Reader) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

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

// --- External Server Smoke Tests ---
// These tests require an externally running EdgeLite server.
// They are automatically skipped if no server is detected.

func TestSmokeLiveness(t *testing.T) {
	baseURL := getBaseURL()
	checkServerRunning(t, baseURL)
	waitForServer(t, baseURL, 30*time.Second)

	code, body := makeRequest(t, "GET", baseURL+"/health/live", nil)
	if code != 200 {
		t.Fatalf("Expected 200 on /health/live, got %d", code)
	}
	if body["status"] != "alive" {
		t.Fatalf("Expected status 'alive', got '%v'", body["status"])
	}
}

func TestSmokeReadiness(t *testing.T) {
	baseURL := getBaseURL()
	checkServerRunning(t, baseURL)

	code, body := makeRequest(t, "GET", baseURL+"/health/ready", nil)
	if code != 200 {
		t.Fatalf("Expected 200 on /health/ready, got %d", code)
	}
	if body["status"] != "ready" {
		t.Fatalf("Expected status 'ready', got '%v'", body["status"])
	}
}

func TestSmokeMetrics(t *testing.T) {
	baseURL := getBaseURL()
	checkServerRunning(t, baseURL)

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200 on /metrics, got %d", resp.StatusCode)
	}
}

func TestSmokeLogin(t *testing.T) {
	baseURL := getBaseURL()
	checkServerRunning(t, baseURL)

	loginURL := fmt.Sprintf("%s/api/v1/auth/login", baseURL)
	body := strings.NewReader(`{"username":"admin","password":"Admin@123456"}`)

	code, resp := makeRequest(t, "POST", loginURL, body)
	if code != 200 {
		t.Skipf("Login failed (code %d) — default admin may not exist. Skipping.", code)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Missing data in login response")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Fatal("Missing access_token in login response")
	}
}

func TestSmokeUnauthorizedAccess(t *testing.T) {
	baseURL := getBaseURL()
	checkServerRunning(t, baseURL)

	code, _ := makeRequest(t, "GET", baseURL+"/api/v1/devices", nil)
	if code != 401 {
		t.Fatalf("Expected 401 without auth, got %d", code)
	}
}

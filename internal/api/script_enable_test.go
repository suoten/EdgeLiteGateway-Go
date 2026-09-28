package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"edgelite/internal/config"
	"edgelite/internal/storage"

	"github.com/labstack/echo/v4"
)

// withScriptStore installs a container backed by a freshly migrated sqlite
// database, which is the shape cmd/edgelite builds at startup. The bare test
// container only reaches the "database not ready" branch of these handlers.
func withScriptStore(t *testing.T) *storage.Database {
	t.Helper()

	prev := GetContainer()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "scripts.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	cont := NewServiceContainer()
	cont.Database = db
	// The seed logs rather than returns: runScriptCode wraps the body in a
	// function, so a bare expression produces no output at all.
	if _, err := db.DB().Exec(`INSERT INTO scripts (id, name, language, code, timeout_ms, enabled, created_at, updated_at)
		VALUES ('s1', 'demo', 'javascript', 'console.log(40 + 2)', 5000, 1, '2026-01-01T00:00:00+08:00', '2026-01-01T00:00:00+08:00')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	SetContainer(cont)

	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
	return db
}

// scriptEnvelope is the {code,message,data} shape every one of these handlers
// answers with; data is kept raw because the honest refusals carry none.
type scriptEnvelope struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// callScriptRoute runs one mounted handler with an echo url param, so the test
// exercises the same c.Param("id") path the router feeds it.
func callScriptRoute(t *testing.T, h echo.HandlerFunc, method, path, id, body string) scriptEnvelope {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h(c); err != nil {
		t.Fatalf("%s %s: handler returned %v", method, path, err)
	}
	var out scriptEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	out.Code = rec.Code
	return out
}

func storedScriptEnabled(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := GetContainer().Database.DB().QueryRow("SELECT enabled FROM scripts WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("read back script %s: %v", id, err)
	}
	return n != 0
}

// TestScriptEnableDisableWritesTheStoredFlag pins /enable and /disable: they
// used to answer {"enabled": true} for any id, including one with no row, so
// the flag a client read back was never the flag that was stored.
func TestScriptEnableDisableWritesTheStoredFlag(t *testing.T) {
	withScriptStore(t)

	res := callScriptRoute(t, handleDisableScript, http.MethodPost, "/api/v1/scripts/s1/disable", "s1", "")
	if res.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200 (%s)", res.Code, res.Message)
	}
	if enabled, ok := res.Data["enabled"].(bool); !ok || enabled {
		t.Fatalf("disable response enabled = %v, want false", res.Data["enabled"])
	}
	if storedScriptEnabled(t, "s1") {
		t.Fatal("stored enabled = true after disable")
	}

	res = callScriptRoute(t, handleEnableScript, http.MethodPost, "/api/v1/scripts/s1/enable", "s1", "")
	if res.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200 (%s)", res.Code, res.Message)
	}
	if enabled, ok := res.Data["enabled"].(bool); !ok || !enabled {
		t.Fatalf("enable response enabled = %v, want true", res.Data["enabled"])
	}
	if !storedScriptEnabled(t, "s1") {
		t.Fatal("stored enabled = false after enable")
	}

	// An id with no row must not report success.
	if res = callScriptRoute(t, handleEnableScript, http.MethodPost, "/api/v1/scripts/nope/enable", "nope", ""); res.Code != http.StatusNotFound {
		t.Fatalf("enable unknown id status = %d, want 404", res.Code)
	}
}

// TestExecuteScriptRefusesDisabledScripts makes "disabled" mean something: the
// only path that runs a stored script used to run it whatever the flag said.
func TestExecuteScriptRefusesDisabledScripts(t *testing.T) {
	withScriptStore(t)

	res := callScriptRoute(t, handleExecuteScript, http.MethodPost, "/api/v1/scripts/s1/execute", "s1", "")
	if res.Code != http.StatusOK {
		t.Fatalf("execute enabled status = %d, want 200 (%s)", res.Code, res.Message)
	}
	if out, _ := res.Data["output"].(string); !strings.Contains(out, "2") {
		t.Fatalf("execute output = %q, want the script result 2", out)
	}

	if res = callScriptRoute(t, handleDisableScript, http.MethodPost, "/scripts/s1/disable", "s1", ""); res.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200 (%s)", res.Code, res.Message)
	}
	if res = callScriptRoute(t, handleExecuteScript, http.MethodPost, "/api/v1/scripts/s1/execute", "s1", ""); res.Code != http.StatusConflict {
		t.Fatalf("execute disabled status = %d, want 409", res.Code)
	}

	// /test stays the editor path: it runs disabled code on purpose so a script
	// can be validated before it is switched on.
	if res = callScriptRoute(t, handleTestScriptByID, http.MethodPost, "/scripts/s1/test", "s1", ""); res.Code != http.StatusOK {
		t.Fatalf("test disabled status = %d, want 200 (%s)", res.Code, res.Message)
	}
}

// TestScriptUpdatePreservesEnabledUnlessSent covers the PUT path: a client that
// omits "enabled" must not have a disabled script silently re-enabled, because
// the update handler used to rewrite every column but that one.
func TestScriptUpdatePreservesEnabledUnlessSent(t *testing.T) {
	withScriptStore(t)

	if res := callScriptRoute(t, handleDisableScript, http.MethodPost, "/scripts/s1/disable", "s1", ""); res.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200 (%s)", res.Code, res.Message)
	}

	res := callScriptRoute(t, handleUpdateScript, http.MethodPut, "/api/v1/scripts/s1", "s1",
		`{"name":"demo2","language":"javascript","code":"console.log(40 + 2)","timeout_ms":5000}`)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (%s)", res.Code, res.Message)
	}
	if enabled, ok := res.Data["enabled"].(bool); !ok || enabled {
		t.Fatalf("PUT without enabled returned %v, want the stored false", res.Data["enabled"])
	}
	if storedScriptEnabled(t, "s1") {
		t.Fatal("PUT without enabled re-enabled the script")
	}

	res = callScriptRoute(t, handleUpdateScript, http.MethodPut, "/api/v1/scripts/s1", "s1",
		`{"name":"demo2","language":"javascript","code":"console.log(40 + 2)","timeout_ms":5000,"enabled":true}`)
	if enabled, ok := res.Data["enabled"].(bool); !ok || !enabled {
		t.Fatalf("PUT with enabled=true returned %v, want true", res.Data["enabled"])
	}
	if !storedScriptEnabled(t, "s1") {
		t.Fatal("PUT with enabled=true did not store it")
	}

	if res = callScriptRoute(t, handleUpdateScript, http.MethodPut, "/api/v1/scripts/nope", "nope",
		`{"name":"x","language":"javascript","code":"1","timeout_ms":5000}`); res.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown id status = %d, want 404", res.Code)
	}
}

// TestDeleteScriptReportsMissingIDs: DELETE used to answer success for an id no
// row held, so a client reporting "deleted" could be wrong.
func TestDeleteScriptReportsMissingIDs(t *testing.T) {
	withScriptStore(t)

	if res := callScriptRoute(t, handleDeleteScript, http.MethodDelete, "/api/v1/scripts/nope", "nope", ""); res.Code != http.StatusNotFound {
		t.Fatalf("delete unknown id status = %d, want 404", res.Code)
	}
	if res := callScriptRoute(t, handleDeleteScript, http.MethodDelete, "/api/v1/scripts/s1", "s1", ""); res.Code != http.StatusOK {
		t.Fatalf("delete stored id status = %d, want 200 (%s)", res.Code, res.Message)
	}
}

// TestScriptLogsAndReviewAreHonest: /logs answered an empty page for every id
// and submit-review/approve/reject echoed a status they never stored. Both now
// refuse with 501 instead of claiming a capability this build does not have.
func TestScriptLogsAndReviewAreHonest(t *testing.T) {
	withScriptStore(t)

	cases := map[string]echo.HandlerFunc{
		"logs":          handleGetScriptLogs,
		"submit-review": handleSubmitScriptReview,
		"approve":       handleApproveScript,
		"reject":        handleRejectScript,
	}
	for name, h := range cases {
		if res := callScriptRoute(t, h, http.MethodGet, "/api/v1/scripts/s1/"+name, "s1", ""); res.Code != http.StatusNotImplemented {
			t.Fatalf("%s status = %d, want 501", name, res.Code)
		}
	}
}

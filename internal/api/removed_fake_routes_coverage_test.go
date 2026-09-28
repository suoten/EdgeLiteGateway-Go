package api

// cmd/edgelite mounts the *_real.go routers for /ota, /preprocess and
// /expressions; the fabricated blocks that used to sit in extras.go and
// extended_endpoints2.go (POST /ota/tasks echoing the request back, GET
// /ota/tasks/:id answering "pending" for any id, /check printing two hardcoded
// versions, preprocess /config answering {rules:[]} with PUT replying
// updated:true, expression /evaluate answering result:null and /validate
// answering valid:true) were never reachable and have been deleted.
//
// Deleting unreachable handlers is only safe while the live router still
// answers every path they owned, so the first test pins that whole path set --
// drop a line from a Register* function and it fails. The second pins the
// behaviour the deleted GET /ota/tasks/:task_id stood in for, because that one
// answered 200 "pending" for an id nobody had ever created.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func realOtaPreprocessExpressionRoutes() map[string]bool {
	e := echo.New()
	RegisterOTARoutesReal(e.Group("/api/v1/ota"))
	RegisterPreprocessConfigRoutes(e.Group("/api/v1/preprocess"))
	RegisterExpressionTestRoutes(e.Group("/api/v1/expressions"))
	RegisterExpressionRoutes(e.Group("/api/v1/expressions"))
	out := map[string]bool{}
	for _, r := range e.Routes() {
		out[r.Method+" "+r.Path] = true
	}
	return out
}

func TestRemovedFakeHandlersLeftNoHole(t *testing.T) {
	routes := realOtaPreprocessExpressionRoutes()
	for _, want := range []string{
		// OTA task paths, formerly the echoes in extras.go.
		"GET /api/v1/ota/tasks",
		"POST /api/v1/ota/tasks",
		"GET /api/v1/ota/tasks/:task_id",
		"DELETE /api/v1/ota/tasks/:task_id",
		// OTA self-update paths, formerly the hardcoded versions and the
		// "applying" / "rolled_back" answers in extended_endpoints2.go.
		"GET /api/v1/ota/check",
		"POST /api/v1/ota/apply",
		"POST /api/v1/ota/rollback",
		"GET /api/v1/ota/backups",
		"GET /api/v1/ota/status",
		"POST /api/v1/ota/cancel",
		// Global preprocess config, formerly the empty read plus updated:true.
		"GET /api/v1/preprocess/config",
		"PUT /api/v1/preprocess/config",
		// Expression workbench, formerly result:null / valid:true.
		"POST /api/v1/expressions/evaluate",
		"POST /api/v1/expressions/evaluate-batch",
		"POST /api/v1/expressions/validate",
		"GET /api/v1/expressions/functions",
	} {
		if !routes[want] {
			t.Errorf("nothing answers %s after the fake handlers were deleted", want)
		}
	}
}

func TestOTATaskGetNeverInventsAPendingTask(t *testing.T) {
	withIsolatedConfig(t)

	// With no OTA manager behind it the handler says so instead of printing a
	// task that does not exist.
	code, data, errCode := callOTA(t, handleOTAGetTaskReal, http.MethodGet,
		"/api/v1/ota/tasks/ota-missing", "", "task_id", "ota-missing")
	if code != http.StatusServiceUnavailable || !strings.HasPrefix(errCode, "ERR_OTA_UNSUPPORTED") {
		t.Fatalf("task get without an engine returned %d/%q, want 503 ERR_OTA_UNSUPPORTED", code, errCode)
	}
	if data != nil {
		t.Fatalf("task get answered a body the engine never produced: %#v", data)
	}

	withOTAEngine(t)
	code, data, errCode = callOTA(t, handleOTAGetTaskReal, http.MethodGet,
		"/api/v1/ota/tasks/ota-missing", "", "task_id", "ota-missing")
	if code != http.StatusNotFound || !strings.Contains(errCode, "ota-missing") {
		t.Fatalf("an id nobody created was answered %d/%q, want 404 naming it", code, errCode)
	}
	if data != nil {
		t.Fatalf("a 404 still carried a fabricated task: %#v", data)
	}
}

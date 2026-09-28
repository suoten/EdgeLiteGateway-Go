package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"edgelite/internal/services"
	"github.com/labstack/echo/v4"
)

// The database-monitor endpoints were the last ones that answered a working
// gateway and a broken one with the same body: /stats returned a page of zeros,
// /tables returned [], and /vacuum and /reindex said "completed" without a
// monitor to run them.

func withDBMonitor(t *testing.T, m *services.DBMonitorService) {
	t.Helper()
	prev := GetContainer()
	cont := NewServiceContainer()
	cont.DBMonitor = m
	SetContainer(cont)
	t.Cleanup(func() { SetContainer(prev) })
}

// monitorOnDatabase points a monitor at a database it can really open, so the
// success path has something to measure.
func monitorOnDatabase(t *testing.T, rows int) *services.DBMonitorService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE monitor_probe (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec("INSERT INTO monitor_probe (v) VALUES (?)", fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return services.NewDBMonitorService(path)
}

// monitorOnGarbage points a monitor at a file SQLite refuses to open.
func monitorOnGarbage(t *testing.T) *services.DBMonitorService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-database.db")
	if err := os.WriteFile(path, []byte("not an sqlite file at all\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return services.NewDBMonitorService(path)
}

func callMonitorHandler(t *testing.T, h func(echo.Context) error, method, path string) (int, map[string]interface{}) {
	t.Helper()
	return callDeviceHandler(t, h, method, path, "", nil, nil)
}

func TestDBStatsWithoutAMonitorSaysItWasNotMeasured(t *testing.T) {
	withDBMonitor(t, nil)
	code, env := callMonitorHandler(t, handleGetDBStats, http.MethodGet, "/api/v1/db-monitor/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an explicit unmeasured payload (a 503 blanks the whole page)", code)
	}
	data, _ := env["data"].(map[string]interface{})
	if data["measured"] != false {
		t.Fatalf("measured = %v, want false: nothing was measured", data["measured"])
	}
	if reason, _ := data["measure_error"].(string); reason == "" {
		t.Fatalf("measure_error = %v, want the reason it could not measure", data["measure_error"])
	}
	// A fabricated 0 for each of these is what made an unmonitored database
	// look like an empty one; the page renders null as "-".
	for _, key := range []string{"db_size", "table_count", "total_rows", "wal_size", "page_size", "free_node_pages"} {
		if v, ok := data[key]; !ok || v != nil {
			t.Fatalf("%s = %#v, want the key present and null (not measured, not zero)", key, v)
		}
	}
}

func TestDBStatsReportsWhatTheMonitorMeasured(t *testing.T) {
	withDBMonitor(t, monitorOnDatabase(t, 3))
	code, env := callMonitorHandler(t, handleGetDBStats, http.MethodGet, "/api/v1/db-monitor/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, env)
	}
	data, _ := env["data"].(map[string]interface{})
	if data["measured"] != true {
		t.Fatalf("measured = %v for an opening database: %+v", data["measured"], data)
	}
	if data["table_count"] != float64(1) {
		t.Fatalf("table_count = %v, want the one table the database holds", data["table_count"])
	}
	if data["total_rows"] != float64(3) {
		t.Fatalf("total_rows = %v, want the three rows that were written", data["total_rows"])
	}
	if size, _ := data["db_size"].(float64); size <= 0 {
		t.Fatalf("db_size = %v, want a real byte count", data["db_size"])
	}
}

func TestVacuumWithoutAMonitorIsNotReportedAsComplete(t *testing.T) {
	withDBMonitor(t, nil)
	for name, h := range map[string]func(echo.Context) error{
		"VACUUM":  handleDBVacuum,
		"REINDEX": handleDBReindex,
	} {
		code, env := callMonitorHandler(t, h, http.MethodPost, "/api/v1/db-monitor/"+strings.ToLower(name))
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s with no monitor returned %d (%v), want 503", name, code, env)
		}
		if env["error_code"] != "ERR_COMMON_SERVICE_NOT_READY" {
			t.Fatalf("%s error_code = %v, want ERR_COMMON_SERVICE_NOT_READY", name, env["error_code"])
		}
		// The envelope must not carry a completed marker anywhere: the page
		// keys its success toast off that word.
		if body, _ := env["data"].(map[string]interface{}); body["status"] != nil {
			t.Fatalf("%s with no monitor reported status %v", name, body["status"])
		}
	}
}

func TestVacuumAndReindexCarryTheRealFailureReason(t *testing.T) {
	withDBMonitor(t, monitorOnGarbage(t))
	for _, tc := range []struct {
		name          string
		handler       func(echo.Context) error
		path          string
		skippedStatus string
	}{
		{"vacuum", handleDBVacuum, "/api/v1/db-monitor/vacuum", "vacuum_skipped"},
		{"reindex", handleDBReindex, "/api/v1/db-monitor/reindex", "reindex_skipped"},
	} {
		code, env := callMonitorHandler(t, tc.handler, http.MethodPost, tc.path)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d (%v)", tc.name, code, env)
		}
		data, _ := env["data"].(map[string]interface{})
		if data["status"] != tc.skippedStatus {
			t.Fatalf("%s: status = %v, want %s", tc.name, data["status"], tc.skippedStatus)
		}
		reason, _ := data["reason"].(string)
		if reason == "" {
			t.Fatalf("%s: skipped without naming a reason: %+v", tc.name, data)
		}
		// "database_in_use" was written into the source; whatever SQLite
		// actually said has to reach the operator instead.
		if reason == "database_in_use" {
			t.Fatalf("%s: reason is the hardcoded guess, not %s' error", tc.name, strings.ToUpper(tc.name))
		}
	}
}

func TestVacuumAndReindexOnAHealthyDatabaseReportCompletion(t *testing.T) {
	withDBMonitor(t, monitorOnDatabase(t, 2))
	for _, tc := range []struct {
		name            string
		handler         func(echo.Context) error
		path            string
		completedStatus string
		skippedStatus   string
	}{
		{"vacuum", handleDBVacuum, "/api/v1/db-monitor/vacuum", "vacuum_completed", "vacuum_skipped"},
		{"reindex", handleDBReindex, "/api/v1/db-monitor/reindex", "reindex_completed", "reindex_skipped"},
	} {
		code, env := callMonitorHandler(t, tc.handler, http.MethodPost, tc.path)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d (%v)", tc.name, code, env)
		}
		data, _ := env["data"].(map[string]interface{})
		if data["status"] != tc.completedStatus {
			t.Fatalf("%s: status = %v, want %s for a database it can open (skipped would be %v)",
				tc.name, data["status"], tc.completedStatus, tc.skippedStatus)
		}
	}
}

func TestDBTablesWithoutAMonitorRefuseInsteadOfAnsweringEmpty(t *testing.T) {
	withDBMonitor(t, nil)
	code, env := callMonitorHandler(t, handleGetDBTables, http.MethodGet, "/api/v1/db-monitor/tables")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want 503 instead of an empty list", code, env)
	}
	if env["error_code"] != "ERR_COMMON_SERVICE_NOT_READY" {
		t.Fatalf("error_code = %v, want ERR_COMMON_SERVICE_NOT_READY", env["error_code"])
	}
}

func TestDBTablesOfAnUnreadableDatabaseRefuseInsteadOfAnsweringEmpty(t *testing.T) {
	withDBMonitor(t, monitorOnGarbage(t))
	code, env := callMonitorHandler(t, handleGetDBTables, http.MethodGet, "/api/v1/db-monitor/tables")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want 503: [] would read as a database with no tables", code, env)
	}
	if env["error_code"] != "ERR_DB_NOT_CONNECTED" {
		t.Fatalf("error_code = %v, want ERR_DB_NOT_CONNECTED", env["error_code"])
	}
}

func TestDBTablesListsWhatTheDatabaseHolds(t *testing.T) {
	withDBMonitor(t, monitorOnDatabase(t, 3))
	code, env := callMonitorHandler(t, handleGetDBTables, http.MethodGet, "/api/v1/db-monitor/tables")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, env)
	}
	raw, _ := json.Marshal(env["data"])
	var tables []map[string]interface{}
	if err := json.Unmarshal(raw, &tables); err != nil {
		t.Fatalf("data is not a table list: %s", raw)
	}
	found := false
	for _, tbl := range tables {
		if tbl["name"] == "monitor_probe" {
			found = true
			if tbl["row_count"] != float64(3) {
				t.Fatalf("monitor_probe row_count = %v, want 3", tbl["row_count"])
			}
		}
	}
	if !found {
		t.Fatalf("the table that was created is not listed: %s", raw)
	}
}

func TestDBMonitorStatusReportsOnlyWhatItChecked(t *testing.T) {
	withDBMonitor(t, nil)
	code, env := callMonitorHandler(t, handleGetDBMonitorStatus, http.MethodGet, "/api/v1/db-monitor/status")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want 503", code, env)
	}

	withDBMonitor(t, monitorOnDatabase(t, 1))
	code, env = callMonitorHandler(t, handleGetDBMonitorStatus, http.MethodGet, "/api/v1/db-monitor/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, env)
	}
	data, _ := env["data"].(map[string]interface{})
	if ts, _ := data["timestamp"].(string); ts == "" {
		t.Fatalf("status carries no timestamp of the check: %+v", data)
	}
	// The old fallback invented connections:1, tables:0 and size_bytes:0 under
	// the same names the page never reads; none of them may come back.
	for _, invented := range []string{"connections", "tables", "size_bytes"} {
		if v, ok := data[invented]; ok {
			t.Fatalf("status carries %s = %v, which nothing measured", invented, v)
		}
	}
}

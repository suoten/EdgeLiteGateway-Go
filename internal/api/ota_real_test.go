package api

// The /ota/* surface these tests cover used to answer 200 to everything: /check
// printed two hardcoded versions, /apply said "applying" and /rollback said
// "rolled_back" without writing a single byte, and the UI followed up with
// "upgrade applied, restarting shortly". The replacements either do real work
// (a live feed, the real backup directory, the real OTA manager) or refuse with a
// code, so every assertion below is about not lying.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// callOTA drives one handler directly. setupEcho cannot express a path
// parameter, and half of /ota/* keys off :task_id, so params takes name/value
// pairs (echo.Context.SetParamNames/SetParamValues).
func callOTA(t *testing.T, h func(echo.Context) error, method, path, body string, params ...string) (int, map[string]interface{}, string) {
	t.Helper()
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
	if len(params)%2 != 0 {
		t.Fatalf("params must come in name/value pairs: %#v", params)
	}
	if len(params) > 0 {
		names := make([]string, 0, len(params)/2)
		values := make([]string, 0, len(params)/2)
		for i := 0; i < len(params); i += 2 {
			names = append(names, params[i])
			values = append(values, params[i+1])
		}
		c.SetParamNames(names...)
		c.SetParamValues(values...)
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data, env.ErrorCode
}

func withOTAFeed(t *testing.T, url string) {
	t.Helper()
	config.GetConfig().OTAUpdateURL = url
}

func TestOTACheckSaysItCheckedNothingWithoutAFeed(t *testing.T) {
	withIsolatedConfig(t)
	withOTAFeed(t, "")

	code, data, errCode := callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if code != http.StatusOK || errCode != "" {
		t.Fatalf("check returned %d/%q, want 200 with no error code", code, errCode)
	}
	if data["checked"] != false {
		t.Fatalf("an unconfigured feed was reported as checked: %#v", data)
	}
	if data["update_available"] != false || data["has_update"] != false {
		t.Fatalf("nothing was asked of anywhere yet an update was offered: %#v", data)
	}
	// self_update has to stay false: "no update available" and "an update could not
	// be applied even if there were one" are different facts.
	if data["self_update"] != false {
		t.Fatalf("the gateway claimed it can self-update: %#v", data)
	}
	if data["current_version"] != gatewayVersion {
		t.Fatalf("check compared against %v, not the running %s", data["current_version"], gatewayVersion)
	}
	if !strings.Contains(fmt.Sprint(data["reason"]), "ota_update_url") {
		t.Fatalf("the empty answer did not explain itself: %#v", data["reason"])
	}
}

func TestOTACheckReadsARealFeed(t *testing.T) {
	withIsolatedConfig(t)

	feed := func(body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	// A GitHub-style payload, which is what an operator is most likely to paste.
	withOTAFeed(t, feed(`{"tag_name":"v99.0.0","body":"release notes","assets":[{"browser_download_url":"http://example.invalid/gw.bin"}]}`).URL)
	_, data, _ := callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if data["checked"] != true || data["has_update"] != true || data["update_available"] != true {
		t.Fatalf("a newer release was not reported: %#v", data)
	}
	if data["latest_version"] != "99.0.0" || data["release_notes"] != "release notes" || data["download_url"] != "http://example.invalid/gw.bin" {
		t.Fatalf("the feed answer lost fields: %#v", data)
	}

	// The bespoke shape, and an older release which must not offer a downgrade.
	withOTAFeed(t, feed(`{"version":"0.9.9","release_notes":"ancient"}`).URL)
	_, data, _ = callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if data["has_update"] != false || data["latest_version"] != "0.9.9" {
		t.Fatalf("an older release was offered as an update: %#v", data)
	}
	if !strings.Contains(fmt.Sprint(data["reason"]), "not newer") {
		t.Fatalf("the refusal did not say why: %#v", data["reason"])
	}

	// A pre-release qualifier is not "newer": there is no ranking for it here.
	withOTAFeed(t, feed(`{"version":"`+gatewayVersion+`-rc1"}`).URL)
	_, data, _ = callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if data["has_update"] != false {
		t.Fatalf("a release candidate of the running version was offered: %#v", data)
	}
}

func TestOTACheckFailsHonestlyOnABrokenFeed(t *testing.T) {
	withIsolatedConfig(t)

	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>coming soon</html>")
	}))
	defer notJSON.Close()
	withOTAFeed(t, notJSON.URL)
	code, _, errCode := callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if code != http.StatusBadGateway || !strings.HasPrefix(errCode, "ERR_OTA_CHECK_FAILED") {
		t.Fatalf("a non-JSON feed returned %d/%q, want 502 ERR_OTA_CHECK_FAILED", code, errCode)
	}

	noVersion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"notes":"no version key here"}`)
	}))
	defer noVersion.Close()
	withOTAFeed(t, noVersion.URL)
	code, _, errCode = callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if code != http.StatusBadGateway || !strings.Contains(errCode, "no version") {
		t.Fatalf("a versionless feed returned %d/%q", code, errCode)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close() // nothing is listening any more
	withOTAFeed(t, down.URL)
	code, _, errCode = callOTA(t, handleOTACheckReal, http.MethodGet, "/api/v1/ota/check", "")
	if code != http.StatusBadGateway || !strings.HasPrefix(errCode, "ERR_OTA_CHECK_FAILED") {
		t.Fatalf("an unreachable feed returned %d/%q, want 502", code, errCode)
	}
}

func TestOTAApplyAndRollbackRefuse(t *testing.T) {
	withIsolatedConfig(t)
	for name, h := range map[string]func(echo.Context) error{
		"apply":    handleOTAApplyReal,
		"rollback": handleOTARollbackReal,
	} {
		code, _, errCode := callOTA(t, h, http.MethodPost, "/api/v1/ota/"+name, `{"version":"9.9.9"}`)
		if code != http.StatusConflict || !strings.HasPrefix(errCode, "ERR_OTA_UNSUPPORTED") {
			t.Fatalf("%s returned %d/%q, want 409 ERR_OTA_UNSUPPORTED", name, code, errCode)
		}
	}
	// Creating a task is refused too: the engine can track a task but no driver can
	// install firmware, so the queue would never drain.
	code, _, errCode := callOTA(t, handleOTACreateTaskReal, http.MethodPost, "/api/v1/ota/tasks", `{"device_id":"d1"}`)
	if code != http.StatusConflict || !strings.HasPrefix(errCode, "ERR_OTA_UNSUPPORTED") {
		t.Fatalf("task create returned %d/%q, want 409", code, errCode)
	}
}

func TestOTABackupsReportsTheRealDirectory(t *testing.T) {
	withIsolatedConfig(t)
	dir := t.TempDir()
	old := filepath.Join(dir, "edgelite-20260101.db")
	fresh := filepath.Join(dir, "edgelite-20260920.db")
	for i, name := range []string{old, fresh} {
		if err := os.WriteFile(name, []byte(strings.Repeat("x", 10+i)), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	ago := time.Hour
	if err := os.Chtimes(old, time.Now().Add(-ago), time.Now().Add(-ago)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-directory"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config.GetConfig().Database.BackupDir = dir

	code, data, errCode := callOTA(t, handleOTABackupsReal, http.MethodGet, "/api/v1/ota/backups", "")
	if code != http.StatusOK || errCode != "" {
		t.Fatalf("backups returned %d/%q", code, errCode)
	}
	if data["configured"] != true || data["directory"] != dir || data["kind"] != "database backups" {
		t.Fatalf("backups did not describe where it looked: %#v", data)
	}
	items, ok := data["backups"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("backups = %#v, want the two files only", data["backups"])
	}
	if items[0].(map[string]interface{})["filename"] != filepath.Base(fresh) {
		t.Fatalf("backups are not newest-first: %#v", items)
	}
	first := items[0].(map[string]interface{})
	if first["size"] != float64(11) || first["version"] != filepath.Base(fresh) {
		t.Fatalf("backup row does not match the file on disk: %#v", first)
	}

	// A directory that has never been written to is not an error, but it must not
	// be reported as an empty list of backups without saying why.
	config.GetConfig().Database.BackupDir = filepath.Join(dir, "not-created-yet")
	_, data, _ = callOTA(t, handleOTABackupsReal, http.MethodGet, "/api/v1/ota/backups", "")
	if items, _ := data["backups"].([]interface{}); len(items) != 0 || data["reason"] == nil {
		t.Fatalf("a missing backup directory was reported as merely empty: %#v", data)
	}
}

// withOTAEngine puts a live engine.OTAManager behind the handlers, which is the
// only way /ota/status and /ota/cancel can be tested for telling the truth.
func withOTAEngine(t *testing.T) *engine.OTAManager {
	t.Helper()
	cont := GetContainer()
	if cont == nil {
		t.Fatal("no container to attach the OTA manager to")
	}
	mgr := engine.NewOTAManager(t.TempDir())
	prev := cont.OTAEngine
	cont.OTAEngine = mgr
	t.Cleanup(func() { cont.OTAEngine = prev })
	return mgr
}

func TestOTAStatusAndCancelFollowTheLiveEngine(t *testing.T) {
	withIsolatedConfig(t)
	mgr := withOTAEngine(t)

	_, data, _ := callOTA(t, handleOTAStatusReal, http.MethodGet, "/api/v1/ota/status", "")
	if data["status"] != "idle" || data["task_count"] != float64(0) {
		t.Fatalf("an empty manager reported %#v", data)
	}
	if data["self_update"] != false {
		t.Fatalf("status implied this build can self-update: %#v", data)
	}

	task := mgr.CreateTask("dev-1", "http://example.invalid/fw.bin", "2.0")
	_, data, _ = callOTA(t, handleOTAStatusReal, http.MethodGet, "/api/v1/ota/status", "")
	if data["status"] != "busy" || data["task_count"] != float64(1) {
		t.Fatalf("a pending task was not visible in status: %#v", data)
	}
	if counts, _ := data["status_counts"].(map[string]interface{}); counts["pending"] != float64(1) {
		t.Fatalf("status_counts wrong: %#v", data["status_counts"])
	}

	_, list, _ := callOTA(t, handleOTAListTasksReal, http.MethodGet, "/api/v1/ota/tasks", "")
	if list["total"] != float64(1) {
		t.Fatalf("tasks list wrong: %#v", list)
	}
	row := list["items"].([]interface{})[0].(map[string]interface{})
	if row["task_id"] != task.TaskID || row["device_id"] != "dev-1" || row["status"] != "pending" {
		t.Fatalf("task row lost the engine state: %#v", row)
	}
	code, got, _ := callOTA(t, handleOTAGetTaskReal, http.MethodGet, "/api/v1/ota/tasks/"+task.TaskID, "", "task_id", task.TaskID)
	if code != http.StatusOK || got["firmware_version"] != "2.0" {
		t.Fatalf("task get wrong: %d %#v", code, got)
	}

	// Cancel needs an id it actually knows.
	code, _, errCode := callOTA(t, handleOTACancelReal, http.MethodPost, "/api/v1/ota/cancel", `{}`)
	if code != http.StatusBadRequest || !strings.HasPrefix(errCode, "ERR_OTA_TASK_ID_REQUIRED") {
		t.Fatalf("a cancel with no task_id returned %d/%q", code, errCode)
	}
	code, _, errCode = callOTA(t, handleOTACancelReal, http.MethodPost, "/api/v1/ota/cancel", `{"task_id":"ota-nope"}`)
	if code != http.StatusNotFound || !strings.Contains(errCode, "ota-nope") {
		t.Fatalf("an unknown task was cancelled with %d/%q", code, errCode)
	}

	code, cancelled, _ := callOTA(t, handleOTACancelReal, http.MethodPost, "/api/v1/ota/cancel", `{"task_id":"`+task.TaskID+`"}`)
	if code != http.StatusOK || cancelled["cancelled"] != true || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel of a live task returned %d %#v", code, cancelled)
	}
	if _, ok := mgr.GetTask(task.TaskID); !ok {
		t.Fatal("the task vanished from the engine instead of being cancelled")
	}
	if _, data, _ := callOTA(t, handleOTAStatusReal, http.MethodGet, "/api/v1/ota/status", ""); data["status"] != "idle" {
		t.Fatalf("a cancelled task still held the manager busy: %#v", data)
	}

	// A finished task cannot be cancelled.
	done := mgr.CreateTask("dev-2", "http://example.invalid/fw2.bin", "3.0")
	done.Status = engine.OTATaskCompleted
	code, _, errCode = callOTA(t, handleOTACancelTaskReal, http.MethodDelete, "/api/v1/ota/tasks/"+done.TaskID, "", "task_id", done.TaskID)
	if code != http.StatusConflict || !strings.HasPrefix(errCode, "ERR_OTA_TASK_NOT_CANCELLABLE") {
		t.Fatalf("a completed task was cancelled with %d/%q", code, errCode)
	}
}

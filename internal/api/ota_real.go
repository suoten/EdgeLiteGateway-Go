package api

// /api/v1/ota/* was a set of echoes. /check printed two hardcoded versions,
// /apply answered "applying" and /rollback "rolled_back" without touching
// anything, and OtaUpdate.vue then told the operator the gateway had upgraded and
// would restart, polling /system/status until the page reloaded. A self-update
// that never happened is worse than a refusal, so these handlers only do what
// this build can actually do:
//
//   - check: ask the configured ota_update_url over HTTP for a release and
//     compare it with the running version.
//   - backups: list the files that are really in the configured backup directory.
//   - status / cancel / tasks: read and control the live engine.OTAManager store.
//   - apply / rollback: refuse. Nothing in this repository downloads a release and
//     swaps the running binary, so the honest answer is 409, not 200.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/security"
)

// gatewayVersion is what /ota/check compares a release feed against. The binary
// prints main.Version, which defaults to the same string but can be overridden at
// link time; SetGatewayVersion keeps this handler from comparing a feed against a
// copy that went stale the first time someone used -X main.Version.
var gatewayVersion = "1.0.0-go"

func SetGatewayVersion(v string) {
	if s := strings.TrimSpace(v); s != "" {
		gatewayVersion = s
	}
}

// otaCheckTimeout keeps a slow or dead release feed from holding the request open;
// the download client inside OTAManager has its own, longer budget.
const otaCheckTimeout = 10 * time.Second

// otaRelease is the upstream shape. Both a bespoke {version, release_notes} feed
// and a GitHub-style {tag_name, body} one are accepted, because that is the whole
// difference between the two feeds an operator is likely to point this at.
type otaRelease struct {
	Version string `json:"version"`
	TagName string `json:"tag_name"`
	Notes   string `json:"release_notes"`
	Body    string `json:"body"`
	URL     string `json:"url"`
	Assets  []struct {
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// RegisterOTARoutesReal mounts the whole /ota surface, replacing both the task
// echoes in extras.go and the self-update echoes in extended_endpoints2.go.
func RegisterOTARoutesReal(g *echo.Group) {
	g.GET("/check", handleOTACheckReal, requirePermission(security.PermOTAManage))
	g.POST("/apply", handleOTAApplyReal, requirePermission(security.PermOTAManage))
	g.POST("/rollback", handleOTARollbackReal, requirePermission(security.PermOTAManage))
	g.GET("/backups", handleOTABackupsReal, requirePermission(security.PermOTAManage))
	g.GET("/status", handleOTAStatusReal, requirePermission(security.PermOTAManage))
	g.POST("/cancel", handleOTACancelReal, requirePermission(security.PermOTAManage))
	g.GET("/tasks", handleOTAListTasksReal, requirePermission(security.PermOTAManage))
	g.POST("/tasks", handleOTACreateTaskReal, requirePermission(security.PermOTAManage))
	g.GET("/tasks/:task_id", handleOTAGetTaskReal, requirePermission(security.PermOTAManage))
	g.DELETE("/tasks/:task_id", handleOTACancelTaskReal, requirePermission(security.PermOTAManage))
}

// otaCapability is repeated on every answer so a client cannot mistake "nothing
// newer out there" for "this gateway can install it".
func otaCapability() map[string]interface{} {
	return map[string]interface{}{
		"self_update": false,
		"note":        "this build has no updater: apply and rollback are refused and a release has to be installed out of band",
	}
}

// handleOTACheckReal asks the configured feed for a release. With no feed it says
// it checked nothing rather than reporting the gateway as up to date.
func handleOTACheckReal(c echo.Context) error {
	data := otaCapability()
	data["current_version"] = gatewayVersion

	url := strings.TrimSpace(config.GetConfig().OTAUpdateURL)
	if url == "" {
		data["checked"] = false
		data["update_available"] = false
		data["has_update"] = false
		data["latest_version"] = gatewayVersion
		data["reason"] = "ota_update_url is not configured, so there was no feed to ask"
		return OK(c, data)
	}

	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, url, nil)
	if err != nil {
		return ErrorCode(c, http.StatusBadGateway, "ERR_OTA_CHECK_FAILED: ota_update_url is not a usable URL: "+err.Error(),
			"ERR_OTA_CHECK_FAILED: ota_update_url is not a usable URL: "+err.Error())
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: otaCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		msg := "ERR_OTA_CHECK_FAILED: the release feed could not be reached: " + err.Error()
		return ErrorCode(c, http.StatusBadGateway, msg, msg)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("ERR_OTA_CHECK_FAILED: the release feed answered %d", resp.StatusCode)
		return ErrorCode(c, http.StatusBadGateway, msg, msg)
	}
	var rel otaRelease
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		msg := "ERR_OTA_CHECK_FAILED: the release feed response could not be read: " + err.Error()
		return ErrorCode(c, http.StatusBadGateway, msg, msg)
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		msg := "ERR_OTA_CHECK_FAILED: the release feed is not JSON: " + err.Error()
		return ErrorCode(c, http.StatusBadGateway, msg, msg)
	}
	latest := strings.TrimSpace(strings.TrimPrefix(rel.Version, "v"))
	if latest == "" {
		latest = strings.TrimSpace(strings.TrimPrefix(rel.TagName, "v"))
	}
	if latest == "" {
		msg := "ERR_OTA_CHECK_FAILED: the release feed carried no version field"
		return ErrorCode(c, http.StatusBadGateway, msg, msg)
	}
	newer := otaVersionIsNewer(latest, gatewayVersion)
	download := strings.TrimSpace(rel.URL)
	if download == "" && len(rel.Assets) > 0 {
		download = strings.TrimSpace(rel.Assets[0].BrowserDownloadURL)
	}
	notes := rel.Notes
	if notes == "" {
		notes = rel.Body
	}
	data["checked"] = true
	data["latest_version"] = latest
	data["update_available"] = newer
	data["has_update"] = newer
	data["release_notes"] = notes
	data["download_url"] = download
	data["feed_url"] = url
	if !newer {
		data["reason"] = "the feed reports " + latest + ", which is not newer than the running " + gatewayVersion
	}
	return OK(c, data)
}

// otaVersionIsNewer compares dotted numeric versions. A trailing qualifier
// (1.2.3-rc1) is dropped: the gateway has no way to rank one, and treating a
// release candidate as newer than the running build would offer an "update" that
// is a downgrade.
func otaVersionIsNewer(candidate, current string) bool {
	a, b := otaVersionParts(candidate), otaVersionParts(current)
	for i := 0; i < len(a) || i < len(b); i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

func otaVersionParts(v string) []int {
	parts := strings.FieldsFunc(strings.TrimSpace(v), func(r rune) bool {
		return r == '.' || r == '-' || r == '+' || r == '_'
	})
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// handleOTAApplyReal refuses instead of pretending to upgrade.
func handleOTAApplyReal(c echo.Context) error {
	msg := "ERR_OTA_UNSUPPORTED: applying an update would replace the running gateway binary and this build has no updater; install the release and restart the service"
	return Conflict(c, msg)
}

// handleOTARollbackReal refuses for the same reason: there is no version store to
// go back to. The database is a different matter and has its own restore API.
func handleOTARollbackReal(c echo.Context) error {
	msg := "ERR_OTA_UNSUPPORTED: there is no previous gateway version stored on this host to roll back to; the backup list below covers database backups, which the backup page restores"
	return Conflict(c, msg)
}

// handleOTABackupsReal reports the configured backup directory as it is on disk.
// These are database backups, not application versions, and the answer says so.
func handleOTABackupsReal(c echo.Context) error {
	dir := strings.TrimSpace(config.GetConfig().Database.BackupDir)
	data := otaCapability()
	data["directory"] = dir
	data["kind"] = "database backups"
	data["backups"] = []interface{}{}
	data["total"] = 0
	if dir == "" {
		data["configured"] = false
		data["reason"] = "no backup directory is configured"
		return OK(c, data)
	}
	data["configured"] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			data["reason"] = "the backup directory does not exist yet, so nothing has been backed up"
			return OK(c, data)
		}
		msg := "ERR_OTA_UNSUPPORTED: the backup directory could not be read: " + err.Error()
		return ErrorCode(c, http.StatusInternalServerError, msg, msg)
	}
	items := make([]interface{}, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, map[string]interface{}{
			"version":    e.Name(),
			"filename":   e.Name(),
			"created_at": info.ModTime().Format(time.RFC3339),
			"size":       info.Size(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].(map[string]interface{})["created_at"].(string) > items[j].(map[string]interface{})["created_at"].(string)
	})
	data["backups"] = items
	data["total"] = len(items)
	return OK(c, data)
}

// handleOTAStatusReal reports the live OTA task store. The progress figure is the
// highest progress among non-terminal tasks, so an idle gateway says 0 truthfully
// instead of printing a bar that never moves.
func handleOTAStatusReal(c echo.Context) error {
	data := otaCapability()
	cont := GetContainer()
	if cont == nil || cont.OTAEngine == nil {
		data["status"] = "unavailable"
		data["progress"] = 0
		data["reason"] = "the OTA manager is not running on this gateway"
		return OK(c, data)
	}
	tasks := cont.OTAEngine.ListTasks()
	status := "idle"
	var progress float64
	counts := map[string]int{}
	for _, t := range tasks {
		counts[string(t.Status)]++
		if t.Status != "completed" && t.Status != "failed" && t.Status != "cancelled" && t.Status != "rolled_back" {
			status = "busy"
			if t.Progress > progress {
				progress = t.Progress
			}
		}
	}
	data["status"] = status
	data["progress"] = progress
	data["task_count"] = len(tasks)
	data["status_counts"] = counts
	data["stats"] = cont.OTAEngine.GetStats()
	return OK(c, data)
}

// handleOTACancelReal cancels a tracked task. It does not answer "cancelled:true"
// to a request that named nothing to cancel.
func handleOTACancelReal(c echo.Context) error {
	var req struct {
		TaskID string `json:"task_id"`
	}
	// An empty body is a real request here: the only thing it can be missing is the
	// id, and cancelOTATask says so. c.Bind on a GET-style empty body returns EOF.
	if err := c.Bind(&req); err != nil && !errors.Is(err, io.EOF) {
		return BadRequest(c, "ERR_COMMON_VALIDATION: invalid request body")
	}
	return cancelOTATask(c, strings.TrimSpace(req.TaskID))
}

func handleOTACancelTaskReal(c echo.Context) error {
	return cancelOTATask(c, strings.TrimSpace(c.Param("task_id")))
}

func cancelOTATask(c echo.Context, taskID string) error {
	cont := GetContainer()
	if cont == nil || cont.OTAEngine == nil {
		return ServiceUnavailable(c, "ERR_OTA_UNSUPPORTED: the OTA manager is not running on this gateway")
	}
	if taskID == "" {
		return BadRequest(c, "ERR_OTA_TASK_ID_REQUIRED: POST /ota/cancel needs {\"task_id\":...}; this build has no self-update task to cancel")
	}
	task, ok := cont.OTAEngine.GetTask(taskID)
	if !ok {
		return NotFound(c, "ERR_OTA_TASK_NOT_FOUND: "+taskID)
	}
	if !cont.OTAEngine.CancelTask(taskID) {
		msg := "ERR_OTA_TASK_NOT_CANCELLABLE: " + taskID + " already finished (" + string(task.Status) + ")"
		return Conflict(c, msg)
	}
	updated, _ := cont.OTAEngine.GetTask(taskID)
	return OK(c, map[string]interface{}{
		"cancelled": true,
		"task_id":   taskID,
		"status":    string(updated.Status),
	})
}

func handleOTAListTasksReal(c echo.Context) error {
	items := []interface{}{}
	cont := GetContainer()
	if cont != nil && cont.OTAEngine != nil {
		for _, t := range cont.OTAEngine.ListTasks() {
			items = append(items, map[string]interface{}{
				"task_id":          t.TaskID,
				"device_id":        t.DeviceID,
				"firmware_url":     t.FirmwareURL,
				"firmware_version": t.FirmwareVer,
				"status":           string(t.Status),
				"progress":         t.Progress,
				"error":            t.Error,
				"created_at":       t.CreatedAt,
				"updated_at":       t.UpdatedAt,
			})
		}
	}
	return OK(c, map[string]interface{}{"items": items, "total": len(items)})
}

func handleOTAGetTaskReal(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.OTAEngine == nil {
		return ServiceUnavailable(c, "ERR_OTA_UNSUPPORTED: the OTA manager is not running on this gateway")
	}
	task, ok := cont.OTAEngine.GetTask(c.Param("task_id"))
	if !ok {
		return NotFound(c, "ERR_OTA_TASK_NOT_FOUND: "+c.Param("task_id"))
	}
	return OK(c, map[string]interface{}{
		"task_id":          task.TaskID,
		"device_id":        task.DeviceID,
		"firmware_url":     task.FirmwareURL,
		"firmware_version": task.FirmwareVer,
		"status":           string(task.Status),
		"progress":         task.Progress,
		"error":            task.Error,
		"created_at":       task.CreatedAt,
		"updated_at":       task.UpdatedAt,
	})
}

// handleOTACreateTaskReal refuses. The engine tracks and cancels tasks, but no
// driver in this build pushes firmware to a device, so accepting a task would put
// it in a queue nothing drains - exactly the lie this file replaces.
func handleOTACreateTaskReal(c echo.Context) error {
	msg := "ERR_OTA_UNSUPPORTED: this build has no device firmware installer, so an OTA task could be queued but never run; the firmware signature API still verifies an image you already hold"
	return Conflict(c, msg)
}

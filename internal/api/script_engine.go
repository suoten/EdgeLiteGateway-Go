package api

import (
	"bytes"
	"fmt"
	"time"

	"github.com/dop251/goja"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// ============================================================================
// Script engine: SQLite-backed script storage with real JavaScript execution
// (goja) for the test/execute endpoints.
// ============================================================================

type scriptRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Language  string `json:"language"`
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// scriptColumnsByID is the one read query every handler that resolves a stored
// script by id uses, so the column order can never drift from scanScriptRow.
const scriptColumnsByID = "SELECT id, name, language, code, timeout_ms, enabled, created_at, updated_at FROM scripts WHERE id = ?"

type scriptReq struct {
	Name      string `json:"name"`
	Language  string `json:"language"`
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
	// Enabled is a pointer so PUT can tell "leave the flag alone" (absent) from
	// "disable this script" (explicit false).
	Enabled *bool `json:"enabled"`
}

func (r *scriptReq) normalize() {
	if r.Language == "" {
		r.Language = "javascript"
	}
	if r.TimeoutMs <= 0 {
		r.TimeoutMs = 5000
	}
	if r.TimeoutMs > 60000 {
		r.TimeoutMs = 60000
	}
}

func scanScriptRow(scanner interface{ Scan(...interface{}) error }) (*scriptRecord, error) {
	var s scriptRecord
	var enabled int
	if err := scanner.Scan(&s.ID, &s.Name, &s.Language, &s.Code, &s.TimeoutMs, &enabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0
	return &s, nil
}

func handleListScripts(c echo.Context) error {
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	rows, err := cont.Database.DB().Query("SELECT id, name, language, code, timeout_ms, enabled, created_at, updated_at FROM scripts ORDER BY updated_at DESC")
	if err != nil {
		logrus.WithError(err).Error("List scripts failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		s, err := scanScriptRow(rows)
		if err != nil {
			continue
		}
		items = append(items, scriptToMap(s))
	}
	return OK(c, items)
}

func scriptToMap(s *scriptRecord) map[string]interface{} {
	return map[string]interface{}{
		"id":         s.ID,
		"name":       s.Name,
		"language":   s.Language,
		"code":       s.Code,
		"timeout_ms": s.TimeoutMs,
		"enabled":    s.Enabled,
		"created_at": s.CreatedAt,
		"updated_at": s.UpdatedAt,
	}
}

func handleCreateScript(c echo.Context) error {
	var req scriptReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	req.normalize()
	if req.Name == "" {
		return BadRequest(c, "name is required")
	}
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	now := time.Now().Format(time.RFC3339)
	s := &scriptRecord{
		ID:        fmt.Sprintf("script-%d", time.Now().UnixNano()),
		Name:      req.Name,
		Language:  req.Language,
		Code:      req.Code,
		TimeoutMs: req.TimeoutMs,
		Enabled:   req.Enabled == nil || *req.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := cont.Database.DB().Exec(
		"INSERT INTO scripts (id, name, language, code, timeout_ms, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		s.ID, s.Name, s.Language, s.Code, s.TimeoutMs, boolToInt(s.Enabled), s.CreatedAt, s.UpdatedAt)
	if err != nil {
		logrus.WithError(err).Error("Create script failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	return Created(c, scriptToMap(s))
}

func handleUpdateScript(c echo.Context) error {
	id := c.Param("id")
	var req scriptReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	req.normalize()
	if req.Name == "" {
		return BadRequest(c, "name is required")
	}
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	// Read the stored row first: a PUT that omits "enabled" must keep the flag the
	// gateway already holds, not fall back to whatever the request struct zeroes to.
	existing, err := scanScriptRow(cont.Database.DB().QueryRow(scriptColumnsByID, id))
	if err != nil {
		return NotFound(c, "Script not found")
	}
	enabled := existing.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	res, err := cont.Database.DB().Exec(
		"UPDATE scripts SET name = ?, language = ?, code = ?, timeout_ms = ?, enabled = ?, updated_at = ? WHERE id = ?",
		req.Name, req.Language, req.Code, req.TimeoutMs, boolToInt(enabled), time.Now().Format(time.RFC3339), id)
	if err != nil {
		logrus.WithError(err).Error("Update script failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NotFound(c, "Script not found")
	}
	return OK(c, map[string]interface{}{"id": id, "updated": true, "enabled": enabled})
}

func handleDeleteScript(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	res, err := cont.Database.DB().Exec("DELETE FROM scripts WHERE id = ?", id)
	if err != nil {
		logrus.WithError(err).Error("Delete script failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	// Deleting an id that was never stored is a 404, not a success: the UI reports
	// "deleted" from this response, and an id that vanished under it should say so.
	if n, _ := res.RowsAffected(); n == 0 {
		return NotFound(c, "ERR_SCRIPT_NOT_FOUND")
	}
	return OK(c, nil)
}

// handleTestScriptCode runs ad-hoc code from the editor (POST /scripts/test).
func handleTestScriptCode(c echo.Context) error {
	var req scriptReq
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	req.normalize()
	if req.Code == "" {
		return BadRequest(c, "code is required")
	}
	output, err := runScriptCode(req.Language, req.Code, req.TimeoutMs)
	if err != nil {
		return OK(c, map[string]interface{}{"output": "", "error": err.Error()})
	}
	return OK(c, map[string]interface{}{"output": output, "error": ""})
}

// handleExecuteScript runs a stored script by id (POST /scripts/:id/execute).
func handleExecuteScript(c echo.Context) error {
	id := c.Param("id")
	cont := GetContainer()
	if cont == nil || cont.Database == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	s, err := scanScriptRow(cont.Database.DB().QueryRow(scriptColumnsByID, id))
	if err != nil {
		return NotFound(c, "Script not found")
	}
	// "disabled" has to stop something: this was the only path that ran a stored
	// script, and it ran it whatever the flag said, so the flag was decoration.
	if !s.Enabled {
		return Conflict(c, "ERR_SCRIPT_DISABLED: script "+id+" is disabled; enable it or run it through POST /scripts/"+id+"/test")
	}
	output, runErr := runScriptCode(s.Language, s.Code, s.TimeoutMs)
	if runErr != nil {
		return OK(c, map[string]interface{}{"script_id": id, "output": "", "error": runErr.Error()})
	}
	return OK(c, map[string]interface{}{"script_id": id, "output": output, "error": ""})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// runScriptCode executes script code. JavaScript runs in a sandboxed goja VM
// with wall-clock timeout; other languages are not supported in-process.
func runScriptCode(language, code string, timeoutMs int) (string, error) {
	if language != "javascript" {
		return "", fmt.Errorf("unsupported language %q for in-process execution (only javascript is supported)", language)
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	if timeoutMs > 60000 {
		timeoutMs = 60000
	}

	vm := goja.New()
	var logs bytes.Buffer
	console := vm.NewObject()
	_ = console.Set("log", func(args ...goja.Value) {
		for i, a := range args {
			if i > 0 {
				logs.WriteString(" ")
			}
			logs.WriteString(a.String())
		}
		logs.WriteString("\n")
	})
	_ = console.Set("error", console.Get("log"))
	_ = console.Set("warn", console.Get("log"))
	_ = vm.Set("console", console)

	timer := time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		vm.Interrupt("execution timeout")
	})
	defer timer.Stop()

	wrapped := "(function(){\n" + code + "\n})()"
	val, err := vm.RunString(wrapped)
	if err != nil {
		return logs.String(), err
	}

	result := ""
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		if jsonFunc, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify")); ok {
			if out, err := jsonFunc(goja.Undefined(), val); err == nil {
				result = out.String()
			} else {
				result = val.String()
			}
		}
	}
	out := logs.String()
	if result != "" {
		if out != "" {
			out += "\n"
		}
		out += result
	}
	return out, nil
}

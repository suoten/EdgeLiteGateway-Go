package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/engine"
)

// withTestLogAggregator installs an in-memory aggregator (no file, no flush
// loop) holding the given entries.
func withTestLogAggregator(t *testing.T, entries []engine.LogEntry) {
	t.Helper()
	prev := GetContainer()
	la := engine.NewLogAggregator("")
	for _, e := range entries {
		la.Ingest(e)
	}
	SetContainer(&ServiceContainer{LogAggregator: la})
	t.Cleanup(func() { SetContainer(prev) })
}

func sampleLogEntries() []engine.LogEntry {
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local)
	return []engine.LogEntry{
		{Timestamp: base, Level: "INFO", Message: "gateway started", Source: "boot"},
		{Timestamp: base.Add(time.Minute), Level: "WARN", Message: `device said "=SUM(A1*2)", then stalled`, Source: "driver", DeviceID: "dev-1", RequestID: "req-1"},
		// Stored lower case on purpose: the level filter must not depend on the
		// ingester's convention.
		{Timestamp: base.Add(2 * time.Minute), Level: "error", Message: "collection failed\nsecond line", Source: "collector", DeviceID: "dev-2"},
		{Timestamp: base.Add(3 * time.Minute), Level: "ERROR", Message: "=HYPERLINK(\"http://evil\")", Source: "collector", DeviceID: "dev-3"},
	}
}

func TestHandleExportLogsWritesRealRows(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	c, rec := setupWithAdmin("GET", "/api/v1/logs/export", "")
	if err := handleExportLogs(c); err != nil {
		t.Fatalf("handleExportLogs error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "timestamp,level,source,device_id,trace_id,message") {
		t.Fatalf("missing header row: %q", body)
	}

	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("export is not parseable CSV: %v\n%s", err, body)
	}
	// Header + 4 entries: a message with a comma, a quote and an embedded
	// newline must each stay inside a single field.
	if len(records) != 5 {
		t.Fatalf("expected 5 CSV records, got %d: %#v", len(records), records)
	}
	for _, r := range records[1:] {
		if len(r) != 6 {
			t.Fatalf("a row has the wrong number of columns: %#v", r)
		}
		if !strings.EqualFold(r[1], "INFO") &&
			!strings.EqualFold(r[1], "WARN") && !strings.EqualFold(r[1], "ERROR") {
			t.Fatalf("level must be normalised for the UI: %#v", r)
		}
	}

	var quoted, formula []string
	for _, r := range records[1:] {
		switch r[3] {
		case "dev-1":
			quoted = r
		case "dev-3":
			formula = r
		}
	}
	if quoted == nil || quoted[5] != `device said "=SUM(A1*2)", then stalled` {
		t.Fatalf("comma/quote message not preserved: %#v", quoted)
	}
	if quoted[2] != "driver" || quoted[4] != "req-1" {
		t.Fatalf("columns shifted for a message containing commas: %#v", quoted)
	}
	// A message that opens with a formula sigil must be neutralised so the
	// spreadsheet treats it as text.
	if formula == nil || formula[5] != "'=HYPERLINK(\"http://evil\")" {
		t.Fatalf("formula injection not neutralised: %#v", formula)
	}
	if !strings.Contains(body, "collection failed") {
		t.Fatalf("multi-line message missing from export: %q", body)
	}
}

func TestHandleExportLogsHonoursFilters(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	for _, tc := range []struct {
		query     string
		wantRows  int
		wantLevel string
	}{
		{"level=warn", 1, "WARN"},
		// Matches an entry the ingester stored as "error" as well as "ERROR".
		{"level=error", 2, "ERROR"},
		{"level=info", 1, "INFO"},
	} {
		c, rec := setupWithAdmin("GET", "/api/v1/logs/export?"+tc.query, "")
		if err := handleExportLogs(c); err != nil {
			t.Fatalf("%s: error: %v", tc.query, err)
		}
		records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
		if err != nil {
			t.Fatalf("%s: parse failed: %v", tc.query, err)
		}
		if len(records)-1 != tc.wantRows {
			t.Fatalf("%s: expected %d rows, got %#v", tc.query, tc.wantRows, records)
		}
		for _, r := range records[1:] {
			if r[1] != tc.wantLevel {
				t.Fatalf("%s: leaked row %#v", tc.query, r)
			}
		}
	}

	// A date-only end_time in the past must exclude everything rather than
	// silently widening the window.
	c2, rec2 := setupWithAdmin("GET", "/api/v1/logs/export?end_time=2020-01-01", "")
	if err := handleExportLogs(c2); err != nil {
		t.Fatalf("error: %v", err)
	}
	if got := strings.TrimSpace(rec2.Body.String()); strings.Count(got, "\n") != 0 {
		t.Fatalf("expected no data rows before 2020, got %q", got)
	}

	// device_id and search narrow the result set too.
	c3, rec3 := setupWithAdmin("GET", "/api/v1/logs/export?device_id=dev-2", "")
	if err := handleExportLogs(c3); err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(rec3.Body.String(), "collection failed") ||
		strings.Contains(rec3.Body.String(), "gateway started") {
		t.Fatalf("device_id filter not applied: %q", rec3.Body.String())
	}

	c4, rec4 := setupWithAdmin("GET", "/api/v1/logs/export?search=REQ-1", "")
	if err := handleExportLogs(c4); err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(rec4.Body.String(), "then stalled") ||
		strings.Contains(rec4.Body.String(), "gateway started") {
		t.Fatalf("search must match the trace id case-insensitively: %q", rec4.Body.String())
	}
}

func TestHandleExportLogsJSONFormat(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	c, rec := setupWithAdmin("GET", "/api/v1/logs/export?format=json", "")
	if err := handleExportLogs(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("json export is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	if rows[0]["device_id"] != "dev-3" {
		t.Fatalf("rows must be newest first, got %+v", rows[0])
	}
}

func TestHandleGetLogsUsesSameFilters(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	c, rec := setupWithAdmin("GET", "/api/v1/logs?level=error&page=1&size=10", "")
	if err := handleGetLogs(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp struct {
		Data struct {
			Items []map[string]interface{} `json:"items"`
			Total int                      `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v\n%s", err, rec.Body.String())
	}
	// The list and the export must agree, otherwise the UI count and the
	// downloaded file disagree.
	if resp.Data.Total != 2 || len(resp.Data.Items) != 2 {
		t.Fatalf("total = %d, items = %d, want 2 each (%+v)", resp.Data.Total, len(resp.Data.Items), resp.Data.Items)
	}
}

func TestLogReadSurfaceRefusesWithoutAggregator(t *testing.T) {
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	t.Cleanup(func() { SetContainer(prev) })

	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		method  string
		path    string
	}{
		{"list", handleGetLogs, "GET", "/api/v1/logs"},
		{"export", handleExportLogs, "GET", "/api/v1/logs/export"},
	} {
		c, rec := setupWithAdmin(tc.method, tc.path, "")
		if err := tc.handler(c); err != nil {
			t.Fatalf("%s: error: %v", tc.name, err)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503: an empty result reads as \"nothing was logged\" (%s)", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ERR_LOG_AGGREGATOR_NOT_READY") {
			t.Fatalf("%s: body = %s, want ERR_LOG_AGGREGATOR_NOT_READY", tc.name, rec.Body.String())
		}
	}
}

func TestHandleLogAggStatsCountsTheBuffer(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	c, rec := setupWithAdmin("GET", "/api/v1/logs/stats", "")
	if err := handleLogAggStats(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			TotalLogs int    `json:"total_logs"`
			Scope     string `json:"scope"`
			Stats     []struct {
				Level string `json:"level"`
				Count int64  `json:"count"`
			} `json:"stats"`
			Buffer map[string]interface{} `json:"buffer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v\n%s", err, rec.Body.String())
	}
	if resp.Data.TotalLogs != 4 {
		t.Fatalf("total_logs = %d, want the 4 buffered entries", resp.Data.TotalLogs)
	}
	// The counters must add up to the total, otherwise the five statistics cards
	// silently under-report what the table below them shows.
	counts := map[string]int64{}
	var sum int64
	for _, s := range resp.Data.Stats {
		counts[s.Level] = s.Count
		sum += s.Count
	}
	if sum != int64(resp.Data.TotalLogs) {
		t.Fatalf("per-level counters sum to %d, want %d (%+v)", sum, resp.Data.TotalLogs, resp.Data.Stats)
	}
	// One entry was ingested as "error" and one as "ERROR": both belong in the
	// ERROR bucket, not a sixth lower-case one.
	if counts["ERROR"] != 2 {
		t.Fatalf("ERROR bucket = %d, want both the upper and lower case entries counted together (%+v)", counts["ERROR"], counts)
	}
	for level := range counts {
		if level != strings.ToUpper(level) {
			t.Fatalf("stats leaked a case-variant bucket %q: %+v", level, counts)
		}
	}
	if counts["INFO"] != 1 || counts["WARN"] != 1 {
		t.Fatalf("level buckets = %+v, want INFO 1 / WARN 1", counts)
	}
	if resp.Data.Scope != "memory_buffer" {
		t.Fatalf("scope = %q, want the reader to know these are buffer counters", resp.Data.Scope)
	}
	if resp.Data.Buffer["file_enabled"] != false {
		t.Fatalf("buffer = %+v, want file_enabled=false for an in-memory aggregator", resp.Data.Buffer)
	}
}

func TestHandleLogAggQueryServesRowsAndPages(t *testing.T) {
	withTestLogAggregator(t, sampleLogEntries())

	c, rec := setupWithAdmin("GET", "/api/v1/logs/query?limit=2&offset=1", "")
	if err := handleLogAggQuery(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Logs   []map[string]interface{} `json:"logs"`
			Total  int                      `json:"total"`
			Limit  int                      `json:"limit"`
			Offset int                      `json:"offset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v\n%s", err, rec.Body.String())
	}
	// total is the filtered set, not the page: the UI paginates on it.
	if resp.Data.Total != 4 || resp.Data.Limit != 2 || resp.Data.Offset != 1 {
		t.Fatalf("envelope = %+v, want total 4 / limit 2 / offset 1", resp.Data)
	}
	if len(resp.Data.Logs) != 2 {
		t.Fatalf("logs = %d rows, want 2: %+v", len(resp.Data.Logs), resp.Data.Logs)
	}
	if resp.Data.Logs[0]["device_id"] != "dev-2" {
		t.Fatalf("offset must skip the newest row: %+v", resp.Data.Logs[0])
	}
	for _, row := range resp.Data.Logs {
		for _, key := range []string{"timestamp", "level", "source", "message"} {
			if s, _ := row[key].(string); s == "" {
				t.Fatalf("row is missing %q: %+v", key, row)
			}
		}
	}

	// The same filters as GET /logs, so the two routes cannot drift apart.
	c2, rec2 := setupWithAdmin("GET", "/api/v1/logs/query?level=error&device_id=dev-3", "")
	if err := handleLogAggQuery(c2); err != nil {
		t.Fatalf("error: %v", err)
	}
	var filtered struct {
		Data struct {
			Logs  []map[string]interface{} `json:"logs"`
			Total int                      `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if filtered.Data.Total != 1 || len(filtered.Data.Logs) != 1 ||
		filtered.Data.Logs[0]["device_id"] != "dev-3" {
		t.Fatalf("filters not applied: %+v", filtered.Data)
	}
	if filtered.Data.Logs[0]["level"] != "ERROR" {
		t.Fatalf("level = %v, want the normalised label", filtered.Data.Logs[0]["level"])
	}

	// An offset past the end is an empty page, not a panic.
	c3, rec3 := setupWithAdmin("GET", "/api/v1/logs/query?offset=99", "")
	if err := handleLogAggQuery(c3); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec3.Code != http.StatusOK || !strings.Contains(rec3.Body.String(), `"total":4`) {
		t.Fatalf("status = %d body = %s", rec3.Code, rec3.Body.String())
	}
}

func TestHandleLogAggSetLevelChangesLogrus(t *testing.T) {
	prevLevel := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(prevLevel) })

	c, rec := setupWithAdmin("PUT", "/api/v1/logs/level", `{"level":"debug"}`)
	if err := handleLogAggSetLevel(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if logrus.GetLevel() != logrus.DebugLevel {
		t.Fatalf("logrus level = %v, the route answered success without changing it", logrus.GetLevel())
	}
	var resp struct {
		Data struct {
			Updated bool   `json:"updated"`
			Level   string `json:"level"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !resp.Data.Updated || resp.Data.Level != "DEBUG" {
		t.Fatalf("body = %s, want updated=true and the new level", rec.Body.String())
	}

	// GET /logs/levels must then report what is actually in effect.
	c2, rec2 := setupWithAdmin("GET", "/api/v1/logs/levels", "")
	if err := handleGetLogLevels(c2); err != nil {
		t.Fatalf("error: %v", err)
	}
	var levels struct {
		Data struct {
			Current   string   `json:"current"`
			Available []string `json:"available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &levels); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if levels.Data.Current != "DEBUG" {
		t.Fatalf("current = %q, want the level the previous call set", levels.Data.Current)
	}
	if len(levels.Data.Available) == 0 {
		t.Fatalf("available = %+v, want the selectable levels", levels.Data.Available)
	}
}

func TestParseLogTime(t *testing.T) {
	start, ok := parseLogTime("2026-09-20", false)
	if !ok || start.Hour() != 0 || start.Day() != 20 {
		t.Fatalf("date-only start = %v ok=%v", start, ok)
	}
	end, ok := parseLogTime("2026-09-20", true)
	if !ok || end.Hour() != 23 || end.Minute() != 59 {
		t.Fatalf("date-only end must cover the whole day, got %v", end)
	}
	if _, ok := parseLogTime("20 September 2026", false); ok {
		t.Fatal("garbage input must not parse")
	}
	ts := time.Date(2026, 9, 20, 6, 30, 0, 0, time.Local)
	got, ok := parseLogTime(ts.Format(time.RFC3339), false)
	if !ok || !got.Equal(ts) {
		t.Fatalf("RFC3339 round trip failed: %v vs %v", got, ts)
	}
}

func TestCsvSafeCell(t *testing.T) {
	for _, in := range []string{"=SUM(A1)", "+1", "-1", "@cmd"} {
		if got := csvSafeCell(in); got != "'"+in {
			t.Fatalf("csvSafeCell(%q) = %q, want prefixed", in, got)
		}
	}
	for _, in := range []string{"plain", "", "1 x", "= not"} {
		if in == "= not" {
			if got := csvSafeCell(in); got != "'= not" {
				t.Fatalf("= must be prefixed even with a space: %q", got)
			}
			continue
		}
		if got := csvSafeCell(in); got != in {
			t.Fatalf("csvSafeCell(%q) = %q, want unchanged", in, got)
		}
	}
}

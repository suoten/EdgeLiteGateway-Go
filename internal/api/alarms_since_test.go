package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

// AlarmList.vue polls GET /alarms?since=<last seen> after a websocket reconnect to
// pick up what it missed. handleListAlarms never read `since`, so the "delta" was
// the newest page of everything: alarms that changed state while the tab was away
// but fired long ago stayed hidden, and the client advanced its watermark as if it
// had seen them.

func withAlarmListService(t *testing.T) *storage.AlarmRepo {
	t.Helper()

	prev := GetContainer()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "alarms.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	cont := NewServiceContainer()
	cont.Database = db
	cont.AlarmRepo = storage.NewAlarmRepo(db)
	cont.AlarmService = services.NewAlarmService(cont.AlarmRepo, nil)
	SetContainer(cont)
	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
	return cont.AlarmRepo
}

func seedAlarmAt(t *testing.T, repo *storage.AlarmRepo, id, status, firedAt, ackedAt, recoveredAt string) {
	t.Helper()
	a := &models.AlarmResponse{
		AlarmID:        id,
		RuleID:         "rule-" + id,
		DeviceID:       "dev-1",
		Severity:       "major",
		Status:         status,
		Message:        "seeded " + id,
		TriggerCount:   1,
		FiredAt:        firedAt,
		AcknowledgedAt: ackedAt,
		RecoveredAt:    recoveredAt,
		RuleType:       "threshold",
		Version:        1,
	}
	if err := repo.Create(a); err != nil {
		t.Fatalf("seed alarm %s: %v", id, err)
	}
}

// listAlarmIDs calls the real list handler and returns the IDs it answered with.
func listAlarmIDs(t *testing.T, query string) ([]string, int) {
	t.Helper()
	c, rec := setupWithAdmin("GET", "/api/v1/alarms"+query, "")
	if err := handleListAlarms(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /alarms%s = %d: %s", query, rec.Code, rec.Body.String())
	}
	var env struct {
		Data  []models.AlarmResponse `json:"data"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body.String(), err)
	}
	ids := make([]string, 0, len(env.Data))
	for _, a := range env.Data {
		ids = append(ids, a.AlarmID)
	}
	sort.Strings(ids)
	return ids, env.Total
}

func TestAlarmsSinceReturnsWhatChangedNotWhatIsNewest(t *testing.T) {
	repo := withAlarmListService(t)
	now := time.Now()
	h := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	// Two alarms fired two hours ago; one of them was acknowledged a minute ago.
	seedAlarmAt(t, repo, "stale-untouched", "firing", h(-2*time.Hour), "", "")
	seedAlarmAt(t, repo, "stale-acked", "acknowledged", h(-2*time.Hour), h(-time.Minute), "")
	seedAlarmAt(t, repo, "stale-recovered", "recovered", h(-2*time.Hour), "", h(-time.Minute))
	// One alarm fired after the watermark.
	seedAlarmAt(t, repo, "fresh", "firing", h(-time.Minute), "", "")

	since := h(-30 * time.Minute)
	ids, total := listAlarmIDs(t, "?since="+url.QueryEscape(since))
	want := []string{"fresh", "stale-acked", "stale-recovered"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("since=%s returned %v, want %v", since, ids, want)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (the page and the count must agree)", total)
	}

	// Without the watermark the whole store is the answer.
	all, allTotal := listAlarmIDs(t, "")
	if len(all) != 4 || allTotal != 4 {
		t.Fatalf("no since: %d rows / total %d, want 4 / 4", len(all), allTotal)
	}
}

func TestAlarmsSinceCombinesWithTheOtherFilters(t *testing.T) {
	repo := withAlarmListService(t)
	now := time.Now()
	h := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	// Recovered before the watermark, so the status matches but the time does not.
	seedAlarmAt(t, repo, "stale-recovered", "recovered", h(-2*time.Hour), "", h(-2*time.Hour))
	// Recovered long after it fired: matches both filters.
	seedAlarmAt(t, repo, "old-recovered-recently", "recovered", h(-2*time.Hour), "", h(-time.Minute))
	seedAlarmAt(t, repo, "untouched-firing", "firing", h(-2*time.Hour), "", "")

	ids, total := listAlarmIDs(t, "?since="+url.QueryEscape(h(-30*time.Minute))+"&status=recovered")
	if len(ids) != 1 || ids[0] != "old-recovered-recently" || total != 1 {
		t.Fatalf("since+status returned %v total %d, want [old-recovered-recently] total 1", ids, total)
	}
}

func TestAlarmsWithAnUnparseableSinceIsRefused(t *testing.T) {
	withAlarmListService(t)

	c, rec := setupWithAdmin("GET", "/api/v1/alarms?since=yesterday", "")
	if err := handleListAlarms(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ErrorCode != "ERR_ALARM_SINCE_INVALID" {
		t.Fatalf("error_code = %q, want ERR_ALARM_SINCE_INVALID", env.ErrorCode)
	}
}

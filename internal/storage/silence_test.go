package storage

import (
	"testing"
	"time"
)

func newSilenceTestRepo(t *testing.T) *AlarmRepo {
	t.Helper()
	db, cleanup := newTestDB(t)
	t.Cleanup(cleanup)
	if err := db.InitTables(); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	return NewAlarmRepo(db)
}

func TestAlarmSilenceCRUDAndMatching(t *testing.T) {
	repo := newSilenceTestRepo(t)
	now := time.Now()

	// No silences initially.
	if repo.IsSilenced("dev-1", "rule-1", "") {
		t.Fatal("expected no silence before any record exists")
	}

	// Device-scoped silence active for the next hour.
	sil := &SilenceRecord{
		ID:        "silence_dev1",
		DeviceID:  "dev-1",
		StartTime: now.Add(-time.Minute).Format(time.RFC3339),
		EndTime:   now.Add(time.Hour).Format(time.RFC3339),
		Reason:    "maintenance",
	}
	if err := repo.CreateSilence(sil); err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}

	if !repo.IsSilenced("dev-1", "rule-x", "") {
		t.Error("device silence should cover any rule on that device")
	}
	if repo.IsSilenced("dev-2", "rule-1", "") {
		t.Error("device silence must not cover other devices")
	}

	// List active includes it; get by ID returns it.
	active, err := repo.ListSilences(true)
	if err != nil || len(active) != 1 {
		t.Fatalf("ListSilences active: %v, len=%d", err, len(active))
	}
	got, err := repo.GetSilence("silence_dev1")
	if err != nil || got == nil || got.DeviceID != "dev-1" || got.Cancelled {
		t.Fatalf("GetSilence: %v, %+v", err, got)
	}

	// Cancel ends the silence immediately.
	if err := repo.CancelSilence("silence_dev1"); err != nil {
		t.Fatalf("CancelSilence: %v", err)
	}
	if repo.IsSilenced("dev-1", "", "") {
		t.Error("cancelled silence must not suppress")
	}

	// Expired silence must not match.
	expired := &SilenceRecord{
		ID:        "silence_expired",
		DeviceID:  "dev-1",
		StartTime: now.Add(-2 * time.Hour).Format(time.RFC3339),
		EndTime:   now.Add(-time.Hour).Format(time.RFC3339),
	}
	if err := repo.CreateSilence(expired); err != nil {
		t.Fatalf("CreateSilence expired: %v", err)
	}
	if repo.IsSilenced("dev-1", "", "") {
		t.Error("expired silence must not suppress")
	}

	// Global silence (all scopes NULL) covers every device/rule.
	global := &SilenceRecord{
		ID:        "silence_global",
		StartTime: now.Add(-time.Minute).Format(time.RFC3339),
		EndTime:   now.Add(time.Hour).Format(time.RFC3339),
	}
	if err := repo.CreateSilence(global); err != nil {
		t.Fatalf("CreateSilence global: %v", err)
	}
	if !repo.IsSilenced("any-dev", "any-rule", "") {
		t.Error("global silence should cover all devices")
	}
}

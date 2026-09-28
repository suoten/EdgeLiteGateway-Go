package engine

import (
	"testing"
	"time"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// Deleting a rule took its evaluation away but left what it had already raised
// "firing" in the store: UnloadRule drops the rule→alarm mapping without
// recovering anything, so the console kept showing an alarm no rule could ever
// clear again — and after a gateway restart not even the in-memory map remembered
// which alarm it was.

func newAlarmTestRepos(t *testing.T) (*storage.RuleRepo, *storage.AlarmRepo) {
	t.Helper()
	db, err := storage.NewDatabase(newTestStorageConfig(t))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return storage.NewRuleRepo(db), storage.NewAlarmRepo(db)
}

func fireAlarm(t *testing.T, rule *models.RuleResponse, alarmRepo *storage.AlarmRepo, alarmID, status string) {
	t.Helper()
	now := time.Now().Format(time.RFC3339)
	if err := alarmRepo.Create(&models.AlarmResponse{
		AlarmID:  alarmID,
		RuleID:   rule.RuleID,
		DeviceID: rule.DeviceID,
		Severity: rule.Severity,
		Status:   status,
		Message:  "stored by the test",
		FiredAt:  now,
		Version:  1,
	}); err != nil {
		t.Fatalf("Create alarm %s: %v", alarmID, err)
	}
}

func TestCloseRuleAlarmsClosesWhatTheRuleLeftOpen(t *testing.T) {
	ruleRepo, alarmRepo := newAlarmTestRepos(t)
	ev := NewRuleEvaluator(nil, ruleRepo, alarmRepo)
	rule := &models.RuleResponse{
		RuleID:     "rule-close-1",
		Name:       "high temp",
		DeviceID:   "dev-1",
		Severity:   "major",
		Enabled:    true,
		RuleType:   "threshold",
		Logic:      "AND",
		Conditions: []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
	}
	ev.LoadRule(rule)

	// Raise one alarm through the real evaluation path so the evaluator tracks it.
	ev.onDataCollected(Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-1",
		Timestamp: time.Now(),
		Data:      DataCollectedEvent{DeviceID: "dev-1", Points: []storage.PointData{{DeviceID: "dev-1", PointName: "temp", Value: 75.0, Quality: "good", Timestamp: time.Now()}}},
	})
	tracked, ok := ev.GetActiveAlarms()[rule.RuleID]
	if !ok {
		t.Fatalf("no tracked alarm after a 75 > 50 sample: %#v", ev.GetActiveAlarms())
	}
	// The evaluator only knows about alarms it raised in this process; this row
	// stands for one that survived a restart.
	fireAlarm(t, rule, alarmRepo, "al-orphan", "acknowledged")
	// Another rule's alarm and an already closed one must stay as they were.
	other := &models.RuleResponse{RuleID: "rule-other", DeviceID: "dev-1", Severity: "minor"}
	fireAlarm(t, other, alarmRepo, "al-other", "firing")
	fireAlarm(t, rule, alarmRepo, "al-closed", "recovered")

	if err := ev.CloseRuleAlarms(rule); err != nil {
		t.Fatalf("CloseRuleAlarms: %v", err)
	}

	for id, want := range map[string]string{
		tracked:     "recovered",
		"al-orphan": "recovered",
		"al-other":  "firing",
		"al-closed": "recovered",
	} {
		a, err := alarmRepo.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if a == nil {
			t.Fatalf("Get(%s) = nil, want the row still there with status %s", id, want)
		}
		if a.Status != want {
			t.Fatalf("%s status = %q, want %q", id, a.Status, want)
		}
	}
	if a, _ := alarmRepo.Get("al-orphan"); a.RecoveredAt == "" {
		t.Fatalf("closed alarm has no recovered_at: the MTTR statistics read it from there")
	}
	if a, _ := alarmRepo.Get("al-closed"); a.RecoveredAt != "" {
		t.Fatalf("already closed alarm was rewritten (recovered_at = %q)", a.RecoveredAt)
	}
	if n := len(ev.GetActiveAlarms()); n != 0 {
		t.Fatalf("active alarms after close = %d, want the rule's entry gone", n)
	}

	// Idempotent: closing again must not error or touch the other rule.
	if err := ev.CloseRuleAlarms(rule); err != nil {
		t.Fatalf("second CloseRuleAlarms: %v", err)
	}
	if a, _ := alarmRepo.Get("al-other"); a.Status != "firing" {
		t.Fatalf("other rule's alarm = %q, want it untouched", a.Status)
	}
}

func TestCloseRuleAlarmsWithoutARuleOrRepo(t *testing.T) {
	ev := NewRuleEvaluator(nil, nil, nil)
	if err := ev.CloseRuleAlarms(nil); err != nil {
		t.Fatalf("CloseRuleAlarms(nil): %v", err)
	}
	if err := ev.CloseRuleAlarms(&models.RuleResponse{RuleID: "rule-none"}); err != nil {
		t.Fatalf("CloseRuleAlarms without an alarm repo: %v", err)
	}
}

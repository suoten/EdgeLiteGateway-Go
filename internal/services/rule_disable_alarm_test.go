package services

import (
	"path/filepath"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

func newRuleAlarmRepos(t *testing.T) (*storage.RuleRepo, *storage.AlarmRepo) {
	t.Helper()
	cfg := &config.AppConfig{Database: config.DatabaseConfig{
		Backend:     "sqlite",
		SQLitePath:  filepath.Join(t.TempDir(), "rule_disable_test.db"),
		PoolSize:    5,
		MaxOverflow: 10,
	}}
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return storage.NewRuleRepo(db), storage.NewAlarmRepo(db)
}

// Disabling a rule stops it from ever being evaluated again, which also means
// nothing could recover the alarm it had raised: the row stayed "firing" on the
// board with a disabled rule behind it, and re-enabling then firing produced a
// second one.
func TestDisablingARuleClosesTheAlarmItLeftOpen(t *testing.T) {
	ruleRepo, alarmRepo := newRuleAlarmRepos(t)
	ev := engine.NewRuleEvaluator(nil, ruleRepo, alarmRepo)
	svc := NewRuleService(ruleRepo, ev)

	rule, err := svc.Create(&models.RuleCreate{
		Name:       "high temp",
		DeviceID:   "dev-1",
		Severity:   "major",
		Logic:      "AND",
		RuleType:   "threshold",
		Conditions: []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
	}, "u-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The alarm the rule raised and still holds open. The evaluator does not
	// track it, which is what happens after a restart or on a rule that fired in
	// an earlier process.
	now := time.Now().Format(time.RFC3339)
	if err := alarmRepo.Create(&models.AlarmResponse{
		AlarmID:  "al-disable",
		RuleID:   rule.RuleID,
		DeviceID: rule.DeviceID,
		Severity: rule.Severity,
		Status:   "firing",
		Message:  "stored by the test",
		FiredAt:  now,
		Version:  1,
	}); err != nil {
		t.Fatalf("Create alarm: %v", err)
	}

	if err := svc.SetEnabled(rule.RuleID, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}

	a, err := alarmRepo.Get("al-disable")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a == nil || a.Status != "recovered" {
		t.Fatalf("alarm after disable = %+v, want recovered: a disabled rule can never recover it", a)
	}
	if a.RecoveredAt == "" {
		t.Fatal("closed alarm has no recovered_at, so MTTR statistics would lose it")
	}
	if id := ev.GetActiveAlarms()[rule.RuleID]; id != "" {
		t.Fatalf("disabled rule still tracks alarm %q", id)
	}

	// Re-enabling adopts nothing: the only row the rule held is already closed.
	if err := svc.SetEnabled(rule.RuleID, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if id := ev.GetActiveAlarms()[rule.RuleID]; id != "" {
		t.Fatalf("re-enabling adopted the closed alarm %q", id)
	}
	if rows, _, _ := alarmRepo.List(models.AlarmFilter{RuleID: rule.RuleID}, 1, 10); len(rows) != 1 {
		t.Fatal("re-enabling raised a new alarm row")
	}
}

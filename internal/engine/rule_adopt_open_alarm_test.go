package engine

import (
	"testing"
	"time"

	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// activeAlarms is an in-memory map, so a gateway restart loses the link between
// a rule and the alarm row it still holds open. The next sample then looked like
// a brand new trigger and inserted a second row; the map pointed at that one
// afterwards, so the original stayed "firing" forever and every restart added
// another open alarm nobody could close.

func sampleEvent(device string, value interface{}) Event {
	return Event{
		Type:      EventTypeDataCollected,
		Source:    "test",
		DeviceID:  device,
		Timestamp: time.Now(),
		Data: DataCollectedEvent{
			DeviceID: device,
			Points:   []storage.PointData{{DeviceID: device, PointName: "temp", Value: value, Quality: "good", Timestamp: time.Now()}},
		},
	}
}

func countAlarms(t *testing.T, repo *storage.AlarmRepo, ruleID string) int {
	t.Helper()
	rows, _, err := repo.List(models.AlarmFilter{RuleID: ruleID}, 1, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(rows)
}

func TestRuleAdoptsTheAlarmStillOpenAfterARestart(t *testing.T) {
	ruleRepo, alarmRepo := newAlarmTestRepos(t)
	rule := &models.RuleResponse{
		RuleID:     "rule-adopt",
		Name:       "high temp",
		DeviceID:   "dev-1",
		Severity:   "major",
		Enabled:    true,
		RuleType:   "threshold",
		Logic:      "AND",
		Conditions: []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
	}
	// The row a previous process left open.
	fireAlarm(t, rule, alarmRepo, "al-open", "firing")

	ev := NewRuleEvaluator(nil, ruleRepo, alarmRepo)
	ev.LoadRule(rule)
	ev.onDataCollected(sampleEvent("dev-1", 75.0))

	if got := countAlarms(t, alarmRepo, rule.RuleID); got != 1 {
		t.Fatalf("alarm rows for the rule = %d, want the open one adopted instead of a second row", got)
	}
	if id := ev.GetActiveAlarms()[rule.RuleID]; id != "al-open" {
		t.Fatalf("tracked alarm = %q, want the stored open row al-open", id)
	}

	// The adoption is what lets a later sample close the row the operator sees.
	ev.onDataCollected(sampleEvent("dev-1", 10.0))
	a, err := alarmRepo.Get("al-open")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a == nil || a.Status != "recovered" {
		t.Fatalf("al-open = %+v, want recovered once the condition clears", a)
	}
}

// With nothing open, the first triggering sample raises exactly one alarm and
// the samples after it do not pile up copies.
func TestRuleRaisesOneAlarmWhenNoneIsOpen(t *testing.T) {
	ruleRepo, alarmRepo := newAlarmTestRepos(t)
	rule := &models.RuleResponse{
		RuleID:     "rule-fresh",
		Name:       "high temp",
		DeviceID:   "dev-1",
		Severity:   "major",
		Enabled:    true,
		RuleType:   "threshold",
		Logic:      "AND",
		Conditions: []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
	}
	ev := NewRuleEvaluator(nil, ruleRepo, alarmRepo)
	ev.LoadRule(rule)
	for i := 0; i < 3; i++ {
		ev.onDataCollected(sampleEvent("dev-1", 75.0))
	}
	if got := countAlarms(t, alarmRepo, rule.RuleID); got != 1 {
		t.Fatalf("alarm rows = %d, want one after three triggering samples", got)
	}
	if id := ev.GetActiveAlarms()[rule.RuleID]; id == "" {
		t.Fatal("the raised alarm is not tracked; it could never recover")
	}
}

// Before the adoption existed every restart of a firing rule added one more open
// row, so an upgraded gateway inherits several alarms per rule of which only the
// newest has an owner. The rest could never leave the board: recovering them by
// hand one at a time is not something the UI offers for a rule that is still
// firing.
func TestLoadRuleClosesTheDuplicateOpenAlarmsOfAnEarlierBuild(t *testing.T) {
	ruleRepo, alarmRepo := newAlarmTestRepos(t)
	rule := &models.RuleResponse{
		RuleID:     "rule-dupes",
		Name:       "high temp",
		DeviceID:   "dev-1",
		Severity:   "major",
		Enabled:    true,
		RuleType:   "threshold",
		Logic:      "AND",
		Conditions: []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
	}
	fireAlarm(t, rule, alarmRepo, "al-first", "firing")
	fireAlarm(t, rule, alarmRepo, "al-acked", "acknowledged")
	fireAlarm(t, rule, alarmRepo, "al-newest", "firing")

	ev := NewRuleEvaluator(nil, ruleRepo, alarmRepo)
	ev.LoadRule(rule)

	if id := ev.GetActiveAlarms()[rule.RuleID]; id != "al-newest" {
		t.Fatalf("tracked alarm = %q, want the newest open row al-newest", id)
	}
	for _, id := range []string{"al-first", "al-acked"} {
		a, err := alarmRepo.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if a == nil || a.Status != "recovered" {
			t.Fatalf("%s = %+v, want the leftover copy closed", id, a)
		}
		if a.RecoveredAt == "" {
			t.Fatalf("%s closed without recovered_at", id)
		}
	}
	if a, _ := alarmRepo.Get("al-newest"); a.Status != "firing" {
		t.Fatalf("the live alarm was closed too (status %q)", a.Status)
	}

	// The one row that stays open is still the one the evaluator owns.
	ev.onDataCollected(sampleEvent("dev-1", 10.0))
	if a, _ := alarmRepo.Get("al-newest"); a.Status != "recovered" {
		t.Fatalf("al-newest = %q, want recovered once the condition clears", a.Status)
	}
}

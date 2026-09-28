package api

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// withAlarmStore installs a container backed by a fresh sqlite database that
// carries an AlarmRepo, which is the shape cmd/edgelite builds at startup. The
// statistics endpoint refuses to serve without it, so the bare test container
// cannot reach the aggregation code.
func withAlarmStore(t *testing.T) *storage.AlarmRepo {
	t.Helper()

	prev := GetContainer()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = t.TempDir() + "/alarms.db"
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	cont := NewServiceContainer()
	cont.Database = db
	cont.AlarmRepo = storage.NewAlarmRepo(db)
	SetContainer(cont)

	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
	return cont.AlarmRepo
}

func seedAlarm(t *testing.T, repo *storage.AlarmRepo, id, ruleType, firedAt, recoveredAt string) {
	t.Helper()
	status := "firing"
	if recoveredAt != "" {
		status = "recovered"
	}
	a := &models.AlarmResponse{
		AlarmID:     id,
		RuleID:      "rule-" + id,
		DeviceID:    "dev-1",
		Severity:    "major",
		Status:      status,
		Message:     "seeded",
		RuleType:    ruleType,
		FiredAt:     firedAt,
		RecoveredAt: recoveredAt,
		Version:     1,
	}
	if err := repo.Create(a); err != nil {
		t.Fatalf("seed alarm %s: %v", id, err)
	}
}

type statsResponse struct {
	Summary struct {
		Firing       int      `json:"firing"`
		Acknowledged int      `json:"acknowledged"`
		Recovered    int      `json:"recovered"`
		WindowCount  int      `json:"window_count"`
		AICount      int      `json:"ai_count"`
		AIRatio      *float64 `json:"ai_ratio"`
		MTTRSeconds  *float64 `json:"mttr_seconds"`
		MTBFSeconds  *float64 `json:"mtbf_seconds"`
	} `json:"summary"`
	Trend      []interface{} `json:"trend"`
	TopDevices []interface{} `json:"top_devices"`
	TopRules   []interface{} `json:"top_rules"`
}

func getAlarmStatistics(t *testing.T, query string) (statsResponse, int) {
	t.Helper()
	c, rec := setupWithAdmin("GET", "/api/v1/alarms/statistics"+query, "")
	if err := handleGetAlarmStatistics(c); err != nil {
		t.Fatalf("handleGetAlarmStatistics: %v", err)
	}
	// OK() wraps the payload, so the summary lives under "data".
	var envelope struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Data    statsResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return envelope.Data, rec.Code
}

// TestAlarmStatisticsReportNullForUnderivableMetrics: the MTTR/MTBF/AI cards on
// the alarm page used to show "0 min / 0 h / 0%" for a metric the backend never
// computed. Zero MTTR reads as "we recover instantly", which is the opposite of
// "nothing has recovered yet".
func TestAlarmStatisticsReportNullForUnderivableMetrics(t *testing.T) {
	repo := withAlarmStore(t)

	out, code := getAlarmStatistics(t, "")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	s := out.Summary
	if s.MTTRSeconds != nil {
		t.Errorf("mttr_seconds = %v with no alarms, want null", *s.MTTRSeconds)
	}
	if s.MTBFSeconds != nil {
		t.Errorf("mtbf_seconds = %v with no alarms, want null", *s.MTBFSeconds)
	}
	if s.AIRatio != nil {
		t.Errorf("ai_ratio = %v with no alarms, want null", *s.AIRatio)
	}
	if s.WindowCount != 0 || s.AICount != 0 {
		t.Errorf("window_count/ai_count = %d/%d, want 0/0", s.WindowCount, s.AICount)
	}

	// One firing alarm: MTBF needs a pair of events and MTTR needs a recovery,
	// so both must still be null rather than collapsing to zero.
	seedAlarm(t, repo, "solo", "threshold",
		time.Now().Add(-time.Hour).Format(time.RFC3339), "")
	out, _ = getAlarmStatistics(t, "")
	if out.Summary.MTTRSeconds != nil || out.Summary.MTBFSeconds != nil {
		t.Errorf("with a single firing alarm mttr/mtbf = %v/%v, want null/null",
			out.Summary.MTTRSeconds, out.Summary.MTBFSeconds)
	}
	if out.Summary.AIRatio == nil || *out.Summary.AIRatio != 0 {
		t.Errorf("ai_ratio = %v, want a real 0 (0 of 1 alarms is a measurement)", out.Summary.AIRatio)
	}
}

// TestAlarmStatisticsDerivesMTTRMTBFAndAIRatioFromRows locks in the arithmetic
// behind the four cards, including that ai_inference is what makes an alarm
// count as AI-produced.
func TestAlarmStatisticsDerivesMTTRMTBFAndAIRatioFromRows(t *testing.T) {
	repo := withAlarmStore(t)

	// Three firings inside a 7-day window: 48 h ago (recovered after 20 min),
	// 46 h ago (AI, still firing) and 2 h ago. The span between the first and
	// last firing is 46 h, so MTBF is 46 h / 2 gaps = 23 h.
	now := time.Now().Truncate(time.Second)
	seedAlarm(t, repo, "a1", "threshold", now.Add(-48*time.Hour).Format(time.RFC3339),
		now.Add(-48*time.Hour).Add(20*time.Minute).Format(time.RFC3339))
	seedAlarm(t, repo, "a2", "ai_inference", now.Add(-46*time.Hour).Format(time.RFC3339), "")
	seedAlarm(t, repo, "fresh", "threshold", now.Add(-2*time.Hour).Format(time.RFC3339), "")
	// Outside the 7-day window: it must not move any windowed number, even
	// though the all-time status counts still see it.
	seedAlarm(t, repo, "old", "ai_inference", now.AddDate(0, 0, -30).Format(time.RFC3339),
		now.AddDate(0, 0, -29).Format(time.RFC3339))

	out, code := getAlarmStatistics(t, "?days=7")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	s := out.Summary
	if s.WindowCount != 3 {
		t.Fatalf("window_count = %d, want 3 (the 30-day-old alarm is outside the window)", s.WindowCount)
	}
	if s.MTTRSeconds == nil || math.Abs(*s.MTTRSeconds-1200) > 5 {
		t.Fatalf("mttr_seconds = %v, want 20 min (only a1 recovered)", s.MTTRSeconds)
	}
	if s.MTBFSeconds == nil || math.Abs(*s.MTBFSeconds-82800) > 60 {
		t.Fatalf("mtbf_seconds = %v, want 23 h (46 h span over 2 gaps)", s.MTBFSeconds)
	}
	if s.AICount != 1 {
		t.Errorf("ai_count = %d, want 1", s.AICount)
	}
	if s.AIRatio == nil || math.Abs(*s.AIRatio-1.0/3.0) > 1e-9 {
		t.Errorf("ai_ratio = %v, want 1/3 as a 0..1 fraction", s.AIRatio)
	}
	if s.Recovered != 2 {
		t.Errorf("recovered = %d, want 2 (status counts stay all-time)", s.Recovered)
	}

	// The narrower window must actually narrow the aggregation: at days=1 only
	// the 2 h-old firing remains, so there is no recovery to average and no
	// interval between failures.
	out, _ = getAlarmStatistics(t, "?days=1")
	if out.Summary.WindowCount != 1 {
		t.Fatalf("days=1 window_count = %d, want 1", out.Summary.WindowCount)
	}
	if out.Summary.MTTRSeconds != nil {
		t.Errorf("days=1 mttr_seconds = %v, want null (the only alarm in range never recovered)", *out.Summary.MTTRSeconds)
	}
	if out.Summary.MTBFSeconds != nil {
		t.Errorf("days=1 mtbf_seconds = %v, want null (a single firing has no interval)", *out.Summary.MTBFSeconds)
	}
	if out.Summary.AIRatio == nil || *out.Summary.AIRatio != 0 {
		t.Errorf("days=1 ai_ratio = %v, want a real 0 (0 of 1 alarms in range is AI)", out.Summary.AIRatio)
	}
}

// TestAlarmStatisticsRefusesWithoutStore: the handler used to answer 200 with an
// all-zero summary when the alarm store was missing, which the UI could not
// tell apart from a quiet plant.
func TestAlarmStatisticsRefusesWithoutStore(t *testing.T) {
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	t.Cleanup(func() { SetContainer(prev) })

	c, rec := setupWithAdmin("GET", "/api/v1/alarms/statistics", "")
	if err := handleGetAlarmStatistics(c); err != nil {
		t.Fatalf("handleGetAlarmStatistics: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503, body %s", rec.Code, rec.Body.String())
	}
}

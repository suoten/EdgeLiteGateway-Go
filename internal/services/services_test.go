package services

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// --- SystemService Tests ---

func TestNewSystemService(t *testing.T) {
	s := NewSystemService(nil)
	if s == nil {
		t.Fatal("NewSystemService returned nil")
	}
	if s.startTime.IsZero() {
		t.Fatal("StartTime should be set")
	}
}

func TestSystemServiceGetSystemInfo(t *testing.T) {
	s := NewSystemService(nil)
	info := s.GetSystemInfo()

	if info["version"] != "1.0.0-go" {
		t.Fatalf("Expected version '1.0.0-go', got %v", info["version"])
	}
	if info["go_runtime"] != true {
		t.Fatal("Expected go_runtime to be true")
	}
	if info["start_time"] == nil {
		t.Fatal("Expected start_time to be set")
	}
	uptime, ok := info["uptime_s"].(float64)
	if !ok {
		t.Fatal("Expected uptime_s to be float64")
	}
	if uptime < 0 {
		t.Fatal("Uptime should not be negative")
	}
}

// --- NotifyService Tests ---

func TestNewNotifyService(t *testing.T) {
	s := NewNotifyService()
	if s == nil {
		t.Fatal("NewNotifyService returned nil")
	}
	if s.httpClient == nil {
		t.Fatal("HTTP client should be initialized")
	}
}

func TestNotifyServiceSetConfig(t *testing.T) {
	s := NewNotifyService()
	s.SetConfig(nil)
	// Should not panic
}

func TestNotifyServiceSendNotificationNoConfig(t *testing.T) {
	s := NewNotifyService()
	// Without config, should not panic and should return nil
	err := s.SendNotification([]string{"dingtalk"}, "Test", "Test message", "warning")
	if err != nil {
		t.Fatalf("SendNotification should not return error without config, got: %v", err)
	}
}

func TestNotifyServiceSendNotificationAllChannels(t *testing.T) {
	s := NewNotifyService()
	// Test with empty channels (should default to all)
	err := s.SendNotification([]string{}, "Test Title", "Test Body", "critical")
	if err != nil {
		t.Fatalf("SendNotification with empty channels failed: %v", err)
	}
}

func TestNotifyServiceSendNotificationSpecificChannels(t *testing.T) {
	s := NewNotifyService()
	channels := []string{"dingtalk", "email", "webhook", "wecom"}
	err := s.SendNotification(channels, "Alert", "Device offline", "high")
	if err != nil {
		t.Fatalf("SendNotification with specific channels failed: %v", err)
	}
}

func TestNotifyServiceSendAlarmNotification(t *testing.T) {
	s := NewNotifyService()
	// Pin an empty config: without it the service reads the process config, and a
	// developer's real webhook URL would receive test messages during `go test`.
	s.SetConfig(&config.NotifyConfig{})
	notif := &AlarmNotification{
		AlarmID:   "alarm-001",
		RuleID:    "rule-001",
		RuleName:  "Temperature High",
		DeviceID:  "device-001",
		Severity:  "critical",
		Action:    "trigger",
		Message:   "Temperature exceeded threshold",
		Timestamp: time.Now().Format(time.RFC3339),
	}
	results := s.SendAlarmNotification(notif, []string{"dingtalk", "email"})
	if results == nil {
		t.Fatal("SendAlarmNotification should return results map")
	}
}

// A channel with no settings never delivered, so the per-channel alarm result
// must say so rather than reporting a silent success.
func TestNotifyServiceAlarmResultsReflectDelivery(t *testing.T) {
	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{})
	notif := &AlarmNotification{Severity: "critical", RuleName: "Temp High", Action: "firing"}
	results := s.SendAlarmNotification(notif, []string{"dingtalk", "webhook"})
	if results["dingtalk"] || results["webhook"] {
		t.Fatalf("unconfigured channels must not report delivery, got %v", results)
	}
}

// Fan-out only reports channels that were expected to deliver and failed; an
// unconfigured sibling must not turn a successful webhook into an error.
func TestNotifyServiceFanOutSkipsUnconfiguredChannels(t *testing.T) {
	var received int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL},
	})
	if err := s.SendNotification([]string{"dingtalk", "webhook"}, "t", "m", "warning"); err != nil {
		t.Fatalf("fan-out with one configured channel failed: %v", err)
	}
	if received != 1 {
		t.Fatalf("expected 1 webhook delivery, got %d", received)
	}
}

func TestNotifyServiceSendNotificationReportsDeliveryFailure(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL},
	})
	err := s.SendNotification([]string{"webhook"}, "t", "m", "critical")
	if err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("expected a webhook delivery error, got %v", err)
	}
}

// notify.<channel>.cooldown_seconds was editable and read by nothing, so a
// flapping rule emitted one notification per evaluation forever. The limit is
// scoped to the identical text: a blanket per-channel cooldown would drop the
// next device's alarm.
func TestNotifyCooldownSuppressesIdenticalMessage(t *testing.T) {
	var received int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL, CooldownSeconds: 60},
	})

	if err := s.SendNotification([]string{"webhook"}, "t", "m", "critical"); err != nil {
		t.Fatalf("first send failed: %v", err)
	}
	err := s.SendNotification([]string{"webhook"}, "t", "m", "critical")
	if err == nil || !strings.Contains(err.Error(), "cooldown_seconds") {
		t.Fatalf("identical repeat must be suppressed by the cooldown, got %v", err)
	}
	if received != 1 {
		t.Fatalf("suppressed send must not reach the endpoint, got %d deliveries", received)
	}
	if err := s.SendNotification([]string{"webhook"}, "t", "another device is on fire", "critical"); err != nil {
		t.Fatalf("a different alarm must not be swallowed by the cooldown: %v", err)
	}
	if received != 2 {
		t.Fatalf("expected 2 deliveries, got %d", received)
	}
}

func TestNotifyMaxPerMinuteCapsTheChannel(t *testing.T) {
	var received int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL, MaxPerMinute: 2},
	})
	for i := 0; i < 2; i++ {
		if err := s.SendNotification([]string{"webhook"}, "t", fmt.Sprintf("distinct %d", i), "critical"); err != nil {
			t.Fatalf("send %d failed: %v", i, err)
		}
	}
	err := s.SendNotification([]string{"webhook"}, "t", "distinct 2", "critical")
	if err == nil || !strings.Contains(err.Error(), "max_per_minute=2") {
		t.Fatalf("third send must hit the per-minute cap, got %v", err)
	}
	if received != 2 {
		t.Fatalf("expected 2 deliveries, got %d", received)
	}
}

// A send that the endpoint rejected must not consume the budget, otherwise one
// outage would lock the channel for the rest of the window.
func TestNotifyFailedSendDoesNotConsumeBudget(t *testing.T) {
	var received int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL, MaxPerMinute: 1, CooldownSeconds: 60},
	})
	for i := 0; i < 3; i++ {
		if err := s.SendNotification([]string{"webhook"}, "t", fmt.Sprintf("m%d", i), "critical"); err == nil {
			t.Fatalf("send %d should report the 500", i)
		}
	}
	if received != 3 {
		t.Fatalf("failed sends must keep being attempted, got %d", received)
	}
}

// WeCom's two storm knobs existed only in the notification page: the config
// section had no fields to store them in, so a save dropped them and notifyLimits
// reported the channel as unlimited. A flapping rule could therefore hammer the
// robot until WeCom blocked it, which loses every later alarm.
func TestNotifyWeComHonoursItsStormLimits(t *testing.T) {
	var received int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := NewNotifyService()
	s.SetConfig(&config.NotifyConfig{
		Wechat: config.NotifyWechatConfig{WebhookURL: sink.URL, MaxPerMinute: 2},
	})
	for i := 0; i < 2; i++ {
		if err := s.SendNotification([]string{"wecom"}, "t", fmt.Sprintf("wecom %d", i), "critical"); err != nil {
			t.Fatalf("send %d failed: %v", i, err)
		}
	}
	err := s.SendNotification([]string{"wecom"}, "t", "wecom 2", "critical")
	if err == nil || !strings.Contains(err.Error(), "max_per_minute=2") {
		t.Fatalf("third send must hit the WeCom per-minute cap, got %v", err)
	}
	if received != 2 {
		t.Fatalf("expected 2 deliveries, got %d", received)
	}

	s.SetConfig(&config.NotifyConfig{
		Wechat: config.NotifyWechatConfig{WebhookURL: sink.URL, CooldownSeconds: 60},
	})
	if err := s.SendNotification([]string{"wechat"}, "t", "repeat me", "critical"); err != nil {
		t.Fatalf("first send failed: %v", err)
	}
	if err := s.SendNotification([]string{"wechat"}, "t", "repeat me", "critical"); err == nil ||
		!strings.Contains(err.Error(), "cooldown_seconds") {
		t.Fatalf("identical WeCom repeat must be suppressed by the cooldown, got %v", err)
	}
	if received != 3 {
		t.Fatalf("expected 3 deliveries before the cooldown took effect, got %d", received)
	}
}

// The rule evaluator owns alarm transitions and cannot import this package, so
// NewAlarmService registers itself as the evaluator's hook. Without that hook an
// alarm is written to the database and pushed to the UI while no operator is
// ever notified.
func TestAlarmServiceNotifiesWhenRuleTriggers(t *testing.T) {
	delivered := make(chan string, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	bus := engine.NewEventBus(8)
	evaluator := engine.NewRuleEvaluator(bus, nil, nil)
	alarmSvc := NewAlarmService(nil, evaluator)
	notifySvc := NewNotifyService()
	notifySvc.SetConfig(&config.NotifyConfig{
		Webhook: config.NotifyWebhookConfig{Enabled: true, URL: sink.URL},
	})
	alarmSvc.SetNotifyService(notifySvc)

	evaluator.LoadRule(&models.RuleResponse{
		RuleID:         "rule-notify-1",
		Name:           "High Temp",
		Severity:       "critical",
		Enabled:        true,
		Conditions:     []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}},
		Logic:          "AND",
		NotifyChannels: []string{"webhook"},
	})

	now := time.Now()
	bus.PublishSync(engine.Event{
		Type:      engine.EventTypeDataCollected,
		Source:    "test",
		DeviceID:  "dev-1",
		Timestamp: now,
		Data: engine.DataCollectedEvent{
			DeviceID: "dev-1",
			Points:   []storage.PointData{{DeviceID: "dev-1", PointName: "temp", Value: 99, Quality: "good", Timestamp: now}},
		},
	})

	select {
	case body := <-delivered:
		if !strings.Contains(body, "High Temp") {
			t.Fatalf("notification lost the rule name: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rule trigger produced no notification; alarm -> notify wiring is broken")
	}
}

// --- DataService Tests ---

func TestNewDataService(t *testing.T) {
	s := NewDataService(nil, nil)
	if s == nil {
		t.Fatal("NewDataService returned nil")
	}
}

func TestDataServiceGetCachedEmpty(t *testing.T) {
	// DataService with nil cache will panic, so we test with a real cache
	cache := storage.NewCacheManager(100)
	s := NewDataService(nil, cache)
	cached := s.GetCached()
	if cached == nil {
		t.Fatal("GetCached should not return nil")
	}
}

// --- AlarmStatistics Tests ---

func TestAlarmStatisticsDefault(t *testing.T) {
	var stats AlarmStatistics
	// Default should be zero value
	if stats.TotalCount != 0 {
		t.Fatalf("Expected 0 total, got %d", stats.TotalCount)
	}
}

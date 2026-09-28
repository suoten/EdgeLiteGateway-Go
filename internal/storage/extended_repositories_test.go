package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
)

func newTestDB(t *testing.T) (*Database, func()) {
	t.Helper()
	cfg := &config.AppConfig{
		Database: config.DatabaseConfig{
			Backend:     "sqlite",
			SQLitePath:  filepath.Join(t.TempDir(), "test.db"),
			PoolSize:    5,
			MaxOverflow: 10,
			BackupDir:   filepath.Join(t.TempDir(), "backups"),
		},
		InfluxDB: config.InfluxDBConfig{
			SQLiteTSPath: filepath.Join(t.TempDir(), "test_ts.db"),
		},
	}
	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	cleanup := func() {
		db.Close()
		// db.Close() now waits for healthMonitor to exit via WaitGroup,
		// but a small delay helps ensure Windows releases the file handle.
		time.Sleep(50 * time.Millisecond)
	}
	return db, cleanup
}

// --- DeviceRepo Update Tests ---

func TestDeviceRepoUpdate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewDeviceRepo(db)
	device := &models.DeviceResponse{
		DeviceID:        "dev-update-1",
		Name:            "Original",
		Protocol:        "modbus",
		Status:          "unknown",
		CollectInterval: 5,
		Points:          []models.PointDef{},
	}
	_ = repo.Create(device, "admin")

	newName := "Updated Name"
	newInterval := 10
	points := []models.PointDef{{Name: "temp", Address: "400001", DataType: "float32"}}
	updates := &models.DeviceUpdate{
		Name:            &newName,
		CollectInterval: &newInterval,
		Points:          &points,
	}
	if err := repo.Update("dev-update-1", updates); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, _ := repo.Get("dev-update-1")
	if got.Name != "Updated Name" {
		t.Fatalf("Expected name 'Updated Name', got '%s'", got.Name)
	}
	if got.CollectInterval != 10 {
		t.Fatalf("Expected interval 10, got %d", got.CollectInterval)
	}
	if len(got.Points) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(got.Points))
	}
}

func TestDeviceRepoGetNonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewDeviceRepo(db)
	got, err := repo.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get non-existent failed: %v", err)
	}
	if got != nil {
		t.Fatal("Expected nil for non-existent device")
	}
}

func TestDeviceRepoListEmpty(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewDeviceRepo(db)
	devices, total, err := repo.List(1, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 0 {
		t.Fatalf("Expected total 0, got %d", total)
	}
	if len(devices) != 0 {
		t.Fatal("Expected nil/empty devices")
	}
}

func TestDeviceRepoListPagination(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewDeviceRepo(db)
	for i := 0; i < 15; i++ {
		device := &models.DeviceResponse{
			DeviceID:        "dev-pag-" + string(rune('a'+i)),
			Name:            "Device " + string(rune('a'+i)),
			Protocol:        "modbus",
			Status:          "online",
			CollectInterval: 5,
			Points:          []models.PointDef{},
		}
		_ = repo.Create(device, "admin")
	}

	devices, total, err := repo.List(1, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 15 {
		t.Fatalf("Expected total 15, got %d", total)
	}
	if len(devices) != 10 {
		t.Fatalf("Expected 10 devices, got %d", len(devices))
	}

	devices2, total2, _ := repo.List(2, 10)
	if total2 != 15 {
		t.Fatalf("Expected total 15, got %d", total2)
	}
	if len(devices2) != 5 {
		t.Fatalf("Expected 5 devices on page 2, got %d", len(devices2))
	}
}

// --- RuleRepo Extended Tests ---

func TestRuleRepoUpdate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	rule := &models.RuleResponse{
		RuleID:         "rule-upd-1",
		Name:           "Original Rule",
		Severity:       "warning",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	_ = repo.Create(rule, "admin")

	newName := "Updated Rule"
	newSeverity := "critical"
	conditions := []models.RuleCondition{{Point: "temp", Operator: ">", Threshold: 50}}
	updates := &models.RuleUpdate{
		Name:       &newName,
		Severity:   &newSeverity,
		Conditions: &conditions,
	}
	if err := repo.Update("rule-upd-1", updates); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, _ := repo.Get("rule-upd-1")
	if got.Name != "Updated Rule" {
		t.Fatalf("Expected name 'Updated Rule', got '%s'", got.Name)
	}
	if got.Severity != "critical" {
		t.Fatalf("Expected severity 'critical', got '%s'", got.Severity)
	}
}

func TestRuleRepoList(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	for i := 0; i < 5; i++ {
		rule := &models.RuleResponse{
			RuleID:         "rule-list-" + string(rune('a'+i)),
			Name:           "Rule " + string(rune('a'+i)),
			Severity:       "warning",
			Enabled:        true,
			NotifyChannels: []string{"dingtalk"},
		}
		_ = repo.Create(rule, "admin")
	}

	rules, total, err := repo.List(1, 10, "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 5 {
		t.Fatalf("Expected total 5, got %d", total)
	}
	if len(rules) != 5 {
		t.Fatalf("Expected 5 rules, got %d", len(rules))
	}
}

func TestRuleRepoListByDevice(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	rule := &models.RuleResponse{
		RuleID:         "rule-dev-1",
		Name:           "Device Rule",
		DeviceID:       "device-xyz",
		Severity:       "warning",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	_ = repo.Create(rule, "admin")

	rules, total, err := repo.List(1, 10, "device-xyz")
	if err != nil {
		t.Fatalf("List by device failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("Expected total 1, got %d", total)
	}
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}
}

func TestRuleRepoIncrementInference(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	rule := &models.RuleResponse{
		RuleID:         "rule-inf-1",
		Name:           "Test",
		Severity:       "warning",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	_ = repo.Create(rule, "admin")

	_ = repo.IncrementInferenceCount("rule-inf-1")
	_ = repo.IncrementInferenceCount("rule-inf-1")
	got, _ := repo.Get("rule-inf-1")
	if got.InferenceCount != 2 {
		t.Fatalf("Expected inference count 2, got %d", got.InferenceCount)
	}
}

func TestRuleRepoIncrementError(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	rule := &models.RuleResponse{
		RuleID:         "rule-err-1",
		Name:           "Test",
		Severity:       "warning",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	_ = repo.Create(rule, "admin")

	_ = repo.IncrementErrorCount("rule-err-1")
	got, _ := repo.Get("rule-err-1")
	if got.ErrorCount != 1 {
		t.Fatalf("Expected error count 1, got %d", got.ErrorCount)
	}
}

func TestRuleRepoGetNonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRuleRepo(db)
	got, err := repo.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get non-existent failed: %v", err)
	}
	if got != nil {
		t.Fatal("Expected nil for non-existent rule")
	}
}

// --- AlarmRepo Tests ---

func TestAlarmRepoCRUD(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	alarm := &models.AlarmResponse{
		AlarmID:      "alarm-1",
		RuleID:       "rule-1",
		DeviceID:     "dev-1",
		Severity:     "critical",
		Status:       "firing",
		Message:      "Temperature too high",
		TriggerCount: 1,
		FiredAt:      time.Now().Format(time.RFC3339),
		RuleType:     "threshold",
		Version:      1,
	}
	err := repo.Create(alarm)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := repo.Get("alarm-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("Alarm not found")
	}
	if got.Severity != "critical" {
		t.Fatalf("Expected severity 'critical', got '%s'", got.Severity)
	}
}

func TestAlarmRepoAcknowledge(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	alarm := &models.AlarmResponse{
		AlarmID:      "alarm-ack-1",
		RuleID:       "rule-1",
		Severity:     "warning",
		Status:       "firing",
		Message:      "Test",
		TriggerCount: 1,
		FiredAt:      time.Now().Format(time.RFC3339),
		RuleType:     "threshold",
		Version:      1,
	}
	_ = repo.Create(alarm)

	err := repo.Acknowledge("alarm-ack-1", "user-1")
	if err != nil {
		t.Fatalf("Acknowledge failed: %v", err)
	}

	got, _ := repo.Get("alarm-ack-1")
	if got.Status != "acknowledged" {
		t.Fatalf("Expected status 'acknowledged', got '%s'", got.Status)
	}
}

func TestAlarmRepoRecover(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	alarm := &models.AlarmResponse{
		AlarmID:      "alarm-rec-1",
		RuleID:       "rule-1",
		Severity:     "warning",
		Status:       "firing",
		Message:      "Test",
		TriggerCount: 1,
		FiredAt:      time.Now().Format(time.RFC3339),
		RuleType:     "threshold",
		Version:      1,
	}
	_ = repo.Create(alarm)

	err := repo.Recover("alarm-rec-1")
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	got, _ := repo.Get("alarm-rec-1")
	if got.Status != "recovered" {
		t.Fatalf("Expected status 'recovered', got '%s'", got.Status)
	}
}

func TestAlarmRepoList(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	for i := 0; i < 3; i++ {
		alarm := &models.AlarmResponse{
			AlarmID:      "alarm-list-" + string(rune('a'+i)),
			RuleID:       "rule-1",
			Severity:     "warning",
			Status:       "firing",
			Message:      "Test",
			TriggerCount: 1,
			FiredAt:      time.Now().Format(time.RFC3339),
			RuleType:     "threshold",
			Version:      1,
		}
		_ = repo.Create(alarm)
	}

	filter := models.AlarmFilter{Status: "firing"}
	alarms, total, err := repo.List(filter, 1, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("Expected total 3, got %d", total)
	}
	if len(alarms) != 3 {
		t.Fatalf("Expected 3 alarms, got %d", len(alarms))
	}
}

func TestAlarmRepoListBySeverity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	alarm1 := &models.AlarmResponse{
		AlarmID: "alarm-sev-1", RuleID: "r1", Severity: "critical", Status: "firing",
		Message: "T", TriggerCount: 1, FiredAt: time.Now().Format(time.RFC3339),
		RuleType: "threshold", Version: 1,
	}
	alarm2 := &models.AlarmResponse{
		AlarmID: "alarm-sev-2", RuleID: "r1", Severity: "warning", Status: "firing",
		Message: "T", TriggerCount: 1, FiredAt: time.Now().Format(time.RFC3339),
		RuleType: "threshold", Version: 1,
	}
	_ = repo.Create(alarm1)
	_ = repo.Create(alarm2)

	filter := models.AlarmFilter{Severity: "critical"}
	alarms, total, err := repo.List(filter, 1, 10)
	if err != nil {
		t.Fatalf("List by severity failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("Expected total 1, got %d", total)
	}
	if len(alarms) != 1 {
		t.Fatalf("Expected 1 alarm, got %d", len(alarms))
	}
}

func TestAlarmRepoGetNonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewAlarmRepo(db)
	got, err := repo.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get non-existent failed: %v", err)
	}
	if got != nil {
		t.Fatal("Expected nil for non-existent alarm")
	}
}

// --- UserRepo Extended Tests ---

func TestUserRepoGetByID(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	_ = repo.Create("user-id-1", "testuser1", "hash", "admin")

	got, err := repo.GetByID("user-id-1")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if got == nil {
		t.Fatal("User not found")
	}
	if got.UserID != "user-id-1" {
		t.Fatalf("Expected user_id 'user-id-1', got '%s'", got.UserID)
	}
}

func TestUserRepoGetByIDNonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	got, err := repo.GetByID("nonexistent")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if got != nil {
		t.Fatal("Expected nil for non-existent user")
	}
}

func TestUserRepoList(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	for i := 0; i < 5; i++ {
		_ = repo.Create("user-l-"+string(rune('a'+i)), "user"+string(rune('a'+i)), "hash", "admin")
	}

	users, total, err := repo.List(1, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 5 {
		t.Fatalf("Expected total 5, got %d", total)
	}
	if len(users) != 5 {
		t.Fatalf("Expected 5 users, got %d", len(users))
	}
}

func TestUserRepoUpdatePassword(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	_ = repo.Create("user-pwd-1", "pwduser", "oldhash", "admin")

	err := repo.UpdatePassword("pwduser", "newhash")
	if err != nil {
		t.Fatalf("UpdatePassword failed: %v", err)
	}

	got, _ := repo.GetByUsername("pwduser")
	if got.PasswordHash != "newhash" {
		t.Fatalf("Expected hash 'newhash', got '%s'", got.PasswordHash)
	}
	if got.MustChangePassword {
		t.Fatal("MustChangePassword should be false after password update")
	}
}

func TestUserRepoUpdatePasswordByID(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	_ = repo.Create("user-pwdid-1", "pwduser2", "oldhash", "admin")

	err := repo.UpdatePasswordByID("user-pwdid-1", "newhash2")
	if err != nil {
		t.Fatalf("UpdatePasswordByID failed: %v", err)
	}

	got, _ := repo.GetByID("user-pwdid-1")
	if got.PasswordHash != "newhash2" {
		t.Fatalf("Expected hash 'newhash2', got '%s'", got.PasswordHash)
	}
}

func TestUserRepoUpdate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	_ = repo.Create("user-upd-1", "upduser", "hash", "admin")

	newRole := "operator"
	enabled := false
	err := repo.Update("user-upd-1", &newRole, &enabled)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, _ := repo.GetByID("user-upd-1")
	if got.Role != "operator" {
		t.Fatalf("Expected role 'operator', got '%s'", got.Role)
	}
	if got.Enabled {
		t.Fatal("Expected enabled=false")
	}
}

func TestUserRepoDelete(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	_ = repo.Create("user-del-1", "deluser", "hash", "admin")

	err := repo.Delete("user-del-1")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	got, _ := repo.GetByID("user-del-1")
	if got != nil {
		t.Fatal("User should be nil after Delete")
	}
}

func TestEnsureAdminUser(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewUserRepo(db)
	err := EnsureAdminUser(repo, "admin", "hashedpassword")
	if err != nil {
		t.Fatalf("EnsureAdminUser failed: %v", err)
	}

	got, _ := repo.GetByUsername("admin")
	if got == nil {
		t.Fatal("Admin user not created")
	}
	if got.Role != "admin" {
		t.Fatalf("Expected role 'admin', got '%s'", got.Role)
	}

	// Second call should not duplicate
	err = EnsureAdminUser(repo, "admin", "hashedpassword")
	if err != nil {
		t.Fatalf("EnsureAdminUser second call failed: %v", err)
	}

	users, total, _ := repo.List(1, 10)
	if total != 1 {
		t.Fatalf("Expected 1 user, got %d", total)
	}
	if len(users) != 1 {
		t.Fatalf("Expected 1 user, got %d", len(users))
	}
}

// --- TemplateRepo Tests ---

func TestTemplateRepoCRUD(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewTemplateRepo(db)
	tpl := &models.TemplateResponse{
		Name:           "tpl-test",
		Protocol:       "modbus",
		ConfigTemplate: map[string]interface{}{"host": "127.0.0.1"},
		PointTemplates: []models.PointDef{{Name: "temp", Address: "400001", DataType: "float32"}},
	}
	err := repo.Create(tpl)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := repo.Get("tpl-test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("Template not found")
	}
	if got.Protocol != "modbus" {
		t.Fatalf("Expected protocol 'modbus', got '%s'", got.Protocol)
	}

	templates, err := repo.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("Expected 1 template, got %d", len(templates))
	}

	err = repo.Delete("tpl-test")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	got, _ = repo.Get("tpl-test")
	if got != nil {
		t.Fatal("Template should be nil after Delete")
	}
}

func TestTemplateRepoGetNonExistent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewTemplateRepo(db)
	got, err := repo.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get non-existent failed: %v", err)
	}
	if got != nil {
		t.Fatal("Expected nil for non-existent template")
	}
}

// --- RateLimitRepo Tests ---

func TestRateLimitRepoCheckAndIncrement(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewRateLimitRepo(db)

	// First 3 requests should be allowed (limit=3)
	for i := 0; i < 3; i++ {
		allowed, err := repo.CheckAndIncrement("test-key", 3)
		if err != nil {
			t.Fatalf("CheckAndIncrement %d failed: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Request %d should be allowed", i)
		}
	}

	// 4th request should be blocked
	allowed, err := repo.CheckAndIncrement("test-key", 3)
	if err != nil {
		t.Fatalf("CheckAndIncrement 4 failed: %v", err)
	}
	if allowed {
		t.Fatal("4th request should be blocked")
	}
}

// --- TimeSeriesStorage Tests ---

func newTestTS(t *testing.T) (*TimeSeriesStorage, func()) {
	t.Helper()
	cfg := &config.AppConfig{
		InfluxDB: config.InfluxDBConfig{
			SQLiteTSPath: filepath.Join(t.TempDir(), "test_ts.db"),
		},
	}
	ts, err := NewTimeSeriesStorage(cfg)
	if err != nil {
		t.Fatalf("NewTimeSeriesStorage failed: %v", err)
	}
	cleanup := func() {
		ts.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return ts, cleanup
}

func TestTimeSeriesStorageWriteAndQuery(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()

	points := []PointData{
		{DeviceID: "dev-1", PointName: "temp", Value: 25.5, Quality: "good", Timestamp: time.Now()},
		{DeviceID: "dev-1", PointName: "humidity", Value: 60.0, Quality: "good", Timestamp: time.Now()},
		{DeviceID: "dev-1", PointName: "status", Value: "online", Quality: "good", Timestamp: time.Now()},
	}
	err := ts.WritePoints(points)
	if err != nil {
		t.Fatalf("WritePoints failed: %v", err)
	}

	results, err := ts.QueryPoints("dev-1", "temp", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryPoints failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if results[0].Value != 25.5 {
		t.Fatalf("Expected value 25.5, got %v", results[0].Value)
	}
}

func TestTimeSeriesStorageGetLatest(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()

	now := time.Now()
	points := []PointData{
		{DeviceID: "dev-latest", PointName: "temp", Value: 20.0, Quality: "good", Timestamp: now},
		{DeviceID: "dev-latest", PointName: "temp", Value: 25.0, Quality: "good", Timestamp: now.Add(time.Second)},
		{DeviceID: "dev-latest", PointName: "temp", Value: 30.0, Quality: "good", Timestamp: now.Add(2 * time.Second)},
	}
	_ = ts.WritePoints(points)

	latest, err := ts.GetLatestPoints("dev-latest")
	if err != nil {
		t.Fatalf("GetLatestPoints failed: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("Expected 1 point, got %d", len(latest))
	}
	if latest["temp"].Value != 30.0 {
		t.Fatalf("Expected latest value 30.0, got %v", latest["temp"].Value)
	}
}

func TestTimeSeriesStorageCheckHealth(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()

	if !ts.CheckHealth() {
		t.Fatal("TimeSeriesStorage should be healthy")
	}
	if !ts.IsUsingFallback() {
		t.Fatal("Should be using fallback (no InfluxDB token)")
	}
}

func TestTimeSeriesStorageIntValues(t *testing.T) {
	ts, cleanup := newTestTS(t)
	defer cleanup()

	points := []PointData{
		{DeviceID: "dev-int", PointName: "count", Value: 42, Quality: "good", Timestamp: time.Now()},
		{DeviceID: "dev-int", PointName: "flag", Value: true, Quality: "good", Timestamp: time.Now()},
	}
	err := ts.WritePoints(points)
	if err != nil {
		t.Fatalf("WritePoints with int/bool failed: %v", err)
	}

	results, err := ts.QueryPoints("dev-int", "count", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryPoints failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
}

// --- CacheManager Tests ---

func TestCacheManagerPushAndGetAll(t *testing.T) {
	cm := NewCacheManager(5)
	if cm.Count() != 0 {
		t.Fatal("Expected count 0 initially")
	}

	cm.Push(PointData{DeviceID: "d1", PointName: "p1", Value: 1.0})
	cm.Push(PointData{DeviceID: "d1", PointName: "p2", Value: 2.0})

	if cm.Count() != 2 {
		t.Fatalf("Expected count 2, got %d", cm.Count())
	}

	all := cm.GetAll()
	if len(all) != 2 {
		t.Fatalf("Expected 2 items, got %d", len(all))
	}
}

func TestCacheManagerOverflow(t *testing.T) {
	cm := NewCacheManager(3)

	for i := 0; i < 5; i++ {
		cm.Push(PointData{DeviceID: "d", PointName: "p", Value: float64(i)})
	}

	if cm.Count() != 3 {
		t.Fatalf("Expected count 3 (capacity), got %d", cm.Count())
	}
}

func TestCacheManagerClear(t *testing.T) {
	cm := NewCacheManager(5)
	cm.Push(PointData{DeviceID: "d", PointName: "p", Value: 1.0})
	cm.Push(PointData{DeviceID: "d", PointName: "p2", Value: 2.0})

	cm.Clear()
	if cm.Count() != 0 {
		t.Fatalf("Expected count 0 after Clear, got %d", cm.Count())
	}
}

// --- OfflineQueue Tests ---

func TestOfflineQueueEnqueueDequeue(t *testing.T) {
	q, err := NewOfflineQueue(filepath.Join(t.TempDir(), "offline_test.db"))
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()

	err = q.Enqueue(map[string]interface{}{"key": "value"})
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	payload, err := q.Dequeue()
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}
	if payload == nil || payload.Payload == "" {
		t.Fatal("Expected non-empty payload")
	}
	// The claimed row stays until the publisher acks it, and is then deleted so
	// the backlog cannot grow without bound.
	if pending, _, _ := q.Stats(); pending != 1 {
		t.Fatalf("claimed row must stay in the queue until Ack, pending=%d", pending)
	}
	if err := q.Ack(payload.ID); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}
	if pending, _, _ := q.Stats(); pending != 0 {
		t.Fatalf("Acked row must be deleted, pending=%d", pending)
	}
}

func TestOfflineQueueDequeueEmpty(t *testing.T) {
	q, err := NewOfflineQueue(filepath.Join(t.TempDir(), "offline_empty.db"))
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()

	item, err := q.Dequeue()
	if err != nil {
		t.Fatalf("Dequeue on empty queue failed: %v", err)
	}
	if item != nil {
		t.Fatal("Expected nil item from empty queue")
	}
}

// A Nacked row goes back to 'pending' and is retried; after maxRetries it is
// dropped rather than blocking the backlog forever.
func TestOfflineQueueNackAndRetryCap(t *testing.T) {
	q, err := NewOfflineQueue(filepath.Join(t.TempDir(), "offline_nack.db"))
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()
	q.maxRetries = 2

	if err := q.Enqueue(map[string]interface{}{"k": "v"}); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	first, err := q.Dequeue()
	if err != nil || first == nil {
		t.Fatalf("Dequeue failed: %v %v", first, err)
	}
	if err := q.Nack(first.ID); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}
	second, err := q.Dequeue()
	if err != nil || second == nil {
		t.Fatal("nacked row must be claimable again")
	}
	if second.Retries <= first.Retries {
		t.Fatalf("retries must increase per claim: %d -> %d", first.Retries, second.Retries)
	}
	if err := q.Nack(second.ID); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}
	third, err := q.Dequeue()
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}
	if third != nil {
		t.Fatalf("item past the retry cap must be dropped, got %+v", third)
	}
	if q.Dropped() == 0 {
		t.Fatal("dropped counter must report the retry-cap discard")
	}
}

// The backlog limit drops the oldest undelivered rows instead of growing the
// offline database until the gateway runs out of disk.
func TestOfflineQueueBacklogLimit(t *testing.T) {
	q, err := NewOfflineQueue(filepath.Join(t.TempDir(), "offline_limit.db"))
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()
	q.maxRows = 5

	for i := 0; i < 12; i++ {
		if err := q.Enqueue(map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}
	}
	pending, _, err := q.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if pending != 5 {
		t.Fatalf("backlog must stay capped at maxRows, pending=%d", pending)
	}
	if q.Dropped() != 7 {
		t.Fatalf("expected 7 dropped rows, got %d", q.Dropped())
	}
	// The survivors are the newest ones.
	item, err := q.Dequeue()
	if err != nil || item == nil {
		t.Fatalf("Dequeue failed: %v %v", err, item)
	}
	if !strings.Contains(item.Payload, `"i":7`) {
		t.Fatalf("oldest survivor should be i=7, got %s", item.Payload)
	}
}

// Upgrading a gateway must not leave the previous implementation's unreachable
// 'sent' rows holding disk forever.
func TestOfflineQueueMigratesLegacySentRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offline_legacy.db")

	// Write the database exactly as the old version left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE offline_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		payload TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		retries INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending'
	)`); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec("INSERT INTO offline_queue (payload, status) VALUES (?, 'sent')", fmt.Sprintf(`{"old":%d}`, i)); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}
	if _, err := db.Exec("INSERT INTO offline_queue (payload, status) VALUES ('{\"keep\":true}', 'pending')"); err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	db.Close()

	q, err := NewOfflineQueue(path)
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()

	if pending, _, _ := q.Stats(); pending != 1 {
		t.Fatalf("only the pending row should remain, pending=%d", pending)
	}
	// The dropped counter must not be inflated by rows that were already delivered.
	if q.Dropped() != 0 {
		t.Fatalf("legacy cleanup must not count as drops, got %d", q.Dropped())
	}
	item, err := q.Dequeue()
	if err != nil || item == nil {
		t.Fatalf("Dequeue failed: %v %v", err, item)
	}
	if !strings.Contains(item.Payload, `"keep":true`) {
		t.Fatalf("pending row must survive migration, got %s", item.Payload)
	}
}

// --- Database Additional Tests ---

func TestDatabaseDBAccessor(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	if db.DB() == nil {
		t.Fatal("DB() should return non-nil")
	}
}

func TestDatabaseGetAuditDBPathExt(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	path := db.GetAuditDBPath()
	if path == "" {
		t.Fatal("AuditDBPath should not be empty")
	}
}

func TestDatabaseHealthCheckAfterClose(t *testing.T) {
	db, _ := newTestDB(t)

	db.Close()
	time.Sleep(50 * time.Millisecond)

	err := db.HealthCheck()
	if err == nil {
		t.Fatal("HealthCheck should fail after Close")
	}
	if db.IsHealthy() {
		t.Fatal("Database should not be healthy after Close")
	}
}

func TestDatabaseFileExistsExt(t *testing.T) {
	cfg := &config.AppConfig{
		Database: config.DatabaseConfig{
			Backend:     "sqlite",
			SQLitePath:  filepath.Join(t.TempDir(), "exists_test.db"),
			PoolSize:    5,
			MaxOverflow: 10,
			BackupDir:   filepath.Join(t.TempDir(), "backups"),
		},
	}

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(cfg.Database.SQLitePath); os.IsNotExist(err) {
		t.Fatal("Database file was not created")
	}
}

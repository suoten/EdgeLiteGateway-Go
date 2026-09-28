package services

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"edgelite/internal/config"
)

// --- Alarm Silence Service Tests ---

func TestAlarmSilenceService(t *testing.T) {
	svc := NewAlarmSilenceService()

	rule := &SilenceRule{
		ID:        "silence-1",
		DeviceID:  "device-1",
		RuleID:    "rule-1",
		Reason:    "maintenance",
		StartTime: time.Now().Add(-1 * time.Minute),
		EndTime:   time.Now().Add(10 * time.Minute),
	}

	svc.Silence(rule)

	if !svc.IsSilenced("device-1", "rule-1") {
		t.Error("Expected device-1/rule-1 to be silenced")
	}

	// IsSilenced checks if deviceID matches OR ruleID matches
	// Since rule.RuleID == "rule-1", checking with rule-1 will match regardless of deviceID
	if !svc.IsSilenced("device-2", "rule-1") {
		t.Error("Expected device-2/rule-1 to be silenced (ruleID OR match)")
	}

	silenced := svc.ListSilenced()
	if len(silenced) != 1 {
		t.Errorf("Expected 1 silenced rule, got %d", len(silenced))
	}

	svc.Unsilell("silence-1")
	if svc.IsSilenced("device-1", "rule-1") {
		t.Error("Expected device-1/rule-1 to NOT be silenced after unsilell")
	}
}

func TestAlarmSilenceExpired(t *testing.T) {
	svc := NewAlarmSilenceService()

	rule := &SilenceRule{
		ID:        "silence-expired",
		DeviceID:  "device-2",
		RuleID:    "rule-2",
		StartTime: time.Now().Add(-20 * time.Minute),
		EndTime:   time.Now().Add(-1 * time.Minute), // Already expired
	}

	svc.Silence(rule)

	if svc.IsSilenced("device-2", "rule-2") {
		t.Error("Expired silence rule should not silence")
	}

	silenced := svc.ListSilenced()
	if len(silenced) != 0 {
		t.Errorf("Expected 0 active silenced rules, got %d", len(silenced))
	}
}

// --- Backup Scheduler Tests ---

// writeRealSQLiteDB creates a database the scheduler can actually snapshot;
// a plain file is not a database and VACUUM INTO rejects it.
func writeRealSQLiteDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (note) VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestNewBackupSchedulerUsesConfig(t *testing.T) {
	s := NewBackupScheduler(config.BackupConfig{
		Enabled:       true,
		IntervalHours: 6,
		RetainDays:    3,
		BackupDir:     "snapshots",
		MinFreeMB:     250,
	}, "data/app.db")
	st := s.Status()
	if st.IntervalHours != 6 || st.RetainDays != 3 || st.MinFreeMB != 250 {
		t.Errorf("config not applied: interval=%d retain=%d minFree=%d", st.IntervalHours, st.RetainDays, st.MinFreeMB)
	}
	if st.BackupDir != "snapshots" {
		t.Errorf("BackupDir = %q, want %q", st.BackupDir, "snapshots")
	}

	// Zeroed config must not produce a zero-interval ticker or no retention.
	defaults := NewBackupScheduler(config.BackupConfig{}, "data/app.db").Status()
	if defaults.IntervalHours != 24 || defaults.RetainDays != 7 || defaults.BackupDir != "data/backups" {
		t.Errorf("defaults not applied: %+v", defaults)
	}
}

func TestBackupSchedulerStartStop(t *testing.T) {
	tmpDir := t.TempDir()
	scheduler := NewBackupScheduler(config.BackupConfig{Enabled: true, IntervalHours: 1}, filepath.Join(tmpDir, "test.db"))

	scheduler.Start(t.Context())
	st := scheduler.Status()
	if !st.Running || !st.Enabled {
		t.Errorf("scheduler should report Running+Enabled after Start: %+v", st)
	}
	if st.NextRun == "" {
		t.Error("NextRun should be set while running and enabled")
	}
	if st.LastRun != "" {
		t.Errorf("LastRun should be empty before the first backup, got %q", st.LastRun)
	}

	scheduler.Stop()
	st = scheduler.Status()
	if st.Running {
		t.Error("scheduler should not report Running after Stop")
	}
	if st.NextRun != "" {
		t.Errorf("NextRun should be cleared after Stop, got %q", st.NextRun)
	}
}

// A disabled section must idle the loop without needing a process restart, and
// re-enabling it through ApplyConfig must schedule the next run again.
func TestBackupSchedulerApplyConfigTakesEffectAtRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	scheduler := NewBackupScheduler(config.BackupConfig{Enabled: true, IntervalHours: 4, BackupDir: filepath.Join(tmpDir, "backups")}, filepath.Join(tmpDir, "test.db"))
	scheduler.Start(t.Context())
	t.Cleanup(scheduler.Stop)

	st := scheduler.ApplyConfig(config.BackupConfig{Enabled: false, IntervalHours: 4, BackupDir: filepath.Join(tmpDir, "backups")})
	if st.Enabled {
		t.Error("ApplyConfig should disable scheduled backups")
	}
	if st.NextRun != "" {
		t.Errorf("a disabled scheduler must not report a next run, got %q", st.NextRun)
	}

	st = scheduler.ApplyConfig(config.BackupConfig{Enabled: true, IntervalHours: 2, RetainDays: 3, BackupDir: filepath.Join(tmpDir, "snapshots"), MinFreeMB: 512})
	if !st.Enabled || st.IntervalHours != 2 || st.RetainDays != 3 || st.MinFreeMB != 512 {
		t.Errorf("ApplyConfig did not apply the new schedule: %+v", st)
	}
	if st.BackupDir != filepath.Join(tmpDir, "snapshots") {
		t.Errorf("BackupDir = %q, want %q", st.BackupDir, filepath.Join(tmpDir, "snapshots"))
	}
	if st.NextRun == "" {
		t.Error("re-enabling must schedule a next run without a restart")
	}
}

func TestBackupSchedulerBackupNow(t *testing.T) {
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, "data")
	backupDir := filepath.Join(tmpDir, "backups")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "test.db")
	writeRealSQLiteDB(t, dbPath)

	scheduler := NewBackupScheduler(config.BackupConfig{IntervalHours: 1, RetainDays: 7, BackupDir: backupDir}, dbPath)
	name, size, err := scheduler.BackupNow()
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	if size <= 0 {
		t.Errorf("BackupNow reported size %d", size)
	}
	info, err := os.Stat(filepath.Join(backupDir, name))
	if err != nil {
		t.Fatalf("backup %q not found: %v", name, err)
	}
	if info.Size() != size {
		t.Errorf("size %d does not match the file on disk (%d)", size, info.Size())
	}
	st := scheduler.Status()
	if st.LastRun == "" || st.LastError != "" {
		t.Errorf("status after success: last_run=%q last_error=%q", st.LastRun, st.LastError)
	}

	// The snapshot must be restorable and contain the committed rows.
	db, err := sql.Open("sqlite", filepath.Join(backupDir, name))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if count != 2 {
		t.Errorf("backup contains %d rows, want 2", count)
	}
}

func TestBackupSchedulerBackupNowReportsFailure(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	if err := os.WriteFile(dbPath, []byte("test database content"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	scheduler := NewBackupScheduler(config.BackupConfig{IntervalHours: 1, BackupDir: filepath.Join(tmpDir, "backups")}, dbPath)
	if _, _, err := scheduler.BackupNow(); err == nil {
		t.Fatal("BackupNow should fail for a source that is not a SQLite database")
	}
	st := scheduler.Status()
	if st.LastError == "" {
		t.Error("failed backup should be visible through Status().LastError")
	}
	if st.LastRun != "" {
		t.Errorf("failed backup should not record a last_run, got %q", st.LastRun)
	}
}

func TestBackupSchedulerRespectsMinFreeSpace(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	writeRealSQLiteDB(t, dbPath)

	scheduler := NewBackupScheduler(config.BackupConfig{IntervalHours: 1, BackupDir: filepath.Join(tmpDir, "backups"), MinFreeMB: 1_000_000_000}, dbPath)
	if _, _, err := scheduler.BackupNow(); err == nil {
		t.Fatal("BackupNow should refuse to run below min_free_mb")
	}
	if _, err := os.Stat(filepath.Join(scheduler.backupDir)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(scheduler.backupDir))
		if len(entries) != 0 {
			t.Errorf("no backup should be written when free space is insufficient, found %d files", len(entries))
		}
	}
}

func TestBackupSchedulerCleanKeepsForeignFiles(t *testing.T) {
	tmpDir := t.TempDir()
	scheduler := NewBackupScheduler(config.BackupConfig{IntervalHours: 1, BackupDir: tmpDir, RetainDays: 1, MinFreeMB: 0}, filepath.Join(tmpDir, "test.db"))

	expired := filepath.Join(tmpDir, "edgelite_backup_20200101_000000.db")
	foreign := filepath.Join(tmpDir, "operator_drop.tar.gz")
	subdir := filepath.Join(tmpDir, "shared_with_others")
	for _, f := range []string{expired, foreign} {
		if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Retention is mtime-based, so the expired backup needs an old timestamp.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	scheduler.cleanOldBackups()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Error("backup older than retain_days should be pruned")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("retention must not delete files the scheduler did not create")
	}
	if _, err := os.Stat(subdir); err != nil {
		t.Error("retention must not delete directories")
	}
}

// The pre-restore safety copies are full database files, so they have to age out
// like any other backup; otherwise every restore permanently doubles the space
// the backup directory holds.
func TestBackupSchedulerPrunesPreRestoreSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	scheduler := NewBackupScheduler(config.BackupConfig{IntervalHours: 1, BackupDir: tmpDir, RetainDays: 1, MinFreeMB: 0}, filepath.Join(tmpDir, "test.db"))

	expired := filepath.Join(tmpDir, "pre_restore_20200101_000000.db")
	fresh := filepath.Join(tmpDir, "pre_restore_20990101_000000.db")
	for _, f := range []string{expired, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	scheduler.cleanOldBackups()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Error("aged pre_restore snapshot must be pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("pre_restore snapshot inside retain_days must survive")
	}
}

// --- Command Approval Service Tests ---

func TestCommandApprovalService(t *testing.T) {
	svc := NewCommandApprovalService()

	cmd := &CommandApproval{
		ID:          "cmd-1",
		DeviceID:    "device-1",
		Point:       "setpoint",
		Value:       42.5,
		RequestedBy: "admin",
		RequestedAt: time.Now(),
		Status:      "pending",
	}

	svc.RequestApproval(cmd)

	pending := svc.ListPending()
	if len(pending) != 1 {
		t.Fatalf("Expected 1 pending, got %d", len(pending))
	}

	if svc.IsApproved("cmd-1") {
		t.Error("Should not be approved yet")
	}

	if err := svc.Approve("cmd-1", "manager"); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	if !svc.IsApproved("cmd-1") {
		t.Error("Should be approved after Approve()")
	}

	pending = svc.ListPending()
	if len(pending) != 0 {
		t.Errorf("Expected 0 pending after approval, got %d", len(pending))
	}
}

func TestCommandApprovalReject(t *testing.T) {
	svc := NewCommandApprovalService()

	cmd := &CommandApproval{
		ID:          "cmd-2",
		DeviceID:    "device-1",
		Point:       "setpoint",
		Value:       100,
		RequestedBy: "admin",
		RequestedAt: time.Now(),
		Status:      "pending",
	}

	svc.RequestApproval(cmd)

	if err := svc.Reject("cmd-2", "manager"); err != nil {
		t.Fatalf("Reject failed: %v", err)
	}

	if svc.IsApproved("cmd-2") {
		t.Error("Should NOT be approved after rejection")
	}
}

func TestCommandApprovalNotFound(t *testing.T) {
	svc := NewCommandApprovalService()

	if err := svc.Approve("nonexistent", "user"); err == nil {
		t.Error("Expected error for approving nonexistent command")
	}
	if err := svc.Reject("nonexistent", "user"); err == nil {
		t.Error("Expected error for rejecting nonexistent command")
	}
}

// --- Shadow Service Tests ---

func TestShadowService(t *testing.T) {
	svc := NewShadowService()

	// Initially no shadow
	shadow := svc.GetShadow("device-1")
	if shadow != nil {
		t.Error("Expected nil shadow initially")
	}

	// Update reported state
	svc.UpdateReported("device-1", map[string]interface{}{
		"temperature": 25.5,
		"humidity":    60,
	})

	shadow = svc.GetShadow("device-1")
	if shadow == nil {
		t.Fatal("Expected shadow after update")
	}
	if shadow.ReportedState["temperature"] != 25.5 {
		t.Errorf("Expected temperature=25.5, got %v", shadow.ReportedState["temperature"])
	}
	if shadow.Version != 2 {
		t.Errorf("Expected version=2, got %d", shadow.Version)
	}

	// Update desired state
	svc.UpdateDesired("device-1", map[string]interface{}{
		"temperature": 30.0,
	})

	shadow = svc.GetShadow("device-1")
	if shadow.DesiredState["temperature"] != 30.0 {
		t.Errorf("Expected desired temperature=30.0, got %v", shadow.DesiredState["temperature"])
	}

	// Get delta (difference between desired and reported)
	delta := svc.GetDelta("device-1")
	if delta == nil {
		t.Fatal("Expected non-nil delta")
	}
	if delta["temperature"] != 30.0 {
		t.Errorf("Expected delta temperature=30.0, got %v", delta["temperature"])
	}
	if _, exists := delta["humidity"]; exists {
		t.Error("humidity should not be in delta (not in desired state)")
	}
}

func TestShadowServiceDeltaNoDiff(t *testing.T) {
	svc := NewShadowService()

	// Set reported and desired to same values
	svc.UpdateReported("device-1", map[string]interface{}{"temp": 25.0})
	svc.UpdateDesired("device-1", map[string]interface{}{"temp": 25.0})

	delta := svc.GetDelta("device-1")
	if len(delta) != 0 {
		t.Errorf("Expected empty delta when reported==desired, got %v", delta)
	}
}

func TestShadowServiceDeltaNoShadow(t *testing.T) {
	svc := NewShadowService()
	delta := svc.GetDelta("nonexistent")
	if delta != nil {
		t.Error("Expected nil delta for nonexistent device")
	}
}

// --- DB Monitor Service Tests ---

func TestDBMonitorService(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "monitor_test.db")
	os.WriteFile(dbPath, []byte("test db content"), 0644)

	monitor := NewDBMonitorService(dbPath)
	result := monitor.CheckHealth()

	if result["db_size_bytes"] == nil {
		t.Error("Expected db_size_bytes in health check")
	}
	if result["timestamp"] == nil {
		t.Error("Expected timestamp in health check")
	}
}

func TestDBMonitorServiceNonExistentDB(t *testing.T) {
	monitor := NewDBMonitorService("/nonexistent/path/db.db")
	result := monitor.CheckHealth()

	if result["timestamp"] == nil {
		t.Error("Expected timestamp even for non-existent DB")
	}
	if result["db_size_bytes"] != nil {
		t.Error("Should not have db_size_bytes for non-existent DB")
	}
}

// TestDBMonitorServiceMeasuresRealDatabase guards the driver-name bug: the
// monitor opened "sqlite3" while only modernc's "sqlite" is registered, so
// sql.Open always failed, the error was swallowed, and every gateway reported
// an empty database. It also pins the key names the page reads.
func TestDBMonitorServiceMeasuresRealDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_tags_name ON tags(name)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := db.Exec(`INSERT INTO tags(name) VALUES ('t')`); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	monitor := NewDBMonitorService(path)
	stats, err := monitor.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats["measured"] != true {
		t.Fatalf("stats were not measured: %+v", stats)
	}
	if stats["table_count"] != 1 || stats["total_rows"] != int64(4) {
		t.Fatalf("wrong counts: %+v", stats)
	}
	if size, _ := stats["db_size"].(int64); size <= 0 {
		t.Fatalf("db_size must be a real byte count: %+v", stats)
	}
	// The page binds db_size/wal_size/free_node_pages, not the *_bytes names.
	for _, key := range []string{"db_size", "wal_size", "page_size", "free_node_pages"} {
		if _, ok := stats[key]; !ok {
			t.Fatalf("missing stat key %q: %+v", key, stats)
		}
	}

	tables, err := monitor.GetTableDetails()
	if err != nil {
		t.Fatalf("GetTableDetails: %v", err)
	}
	if len(tables) != 1 || tables[0]["name"] != "tags" {
		t.Fatalf("wrong table details: %+v", tables)
	}
	if rows, _ := tables[0]["row_count"].(int64); rows != 4 {
		t.Fatalf("wrong row count: %+v", tables[0])
	}
	// Sizes are only reported when SQLite's dbstat module is compiled in; the
	// page renders "-" for absent keys, so either shape is acceptable here.
	if size, ok := tables[0]["total_size"]; ok {
		if bytes, _ := size.(int64); bytes <= 0 {
			t.Fatalf("total_size present but zero: %+v", tables[0])
		}
	}
}

// TestDBMonitorServiceUnmeasurableDBReportsIt guarantees the swallowed-error
// shape: a path that is not a database must say measured=false and carry the
// reason, because the handler turns an error into an all-zero 200 response.
func TestDBMonitorServiceUnmeasurableDBReportsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-db.db")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	stats, err := NewDBMonitorService(path).GetStats()
	if err != nil {
		t.Fatalf("GetStats must not fail, it reports through the map: %v", err)
	}
	if stats["measured"] == true {
		t.Fatalf("garbage file reported as measured: %+v", stats)
	}
	if reason, _ := stats["measure_error"].(string); reason == "" {
		t.Fatalf("no reason reported: %+v", stats)
	}
}

// --- MCP Service Tests ---

func TestMCPService(t *testing.T) {
	svc := NewMCPService()

	tool := MCPTool{
		Name:        "echo",
		Description: "Echo back the input",
		Handler: func(params map[string]interface{}) (interface{}, error) {
			return params["message"], nil
		},
	}

	svc.RegisterTool(tool)

	tools := svc.ListTools()
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(tools))
	}

	result, err := svc.CallTool("echo", map[string]interface{}{"message": "hello"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result != "hello" {
		t.Errorf("Expected 'hello', got %v", result)
	}

	// Call nonexistent tool
	_, err = svc.CallTool("nonexistent", nil)
	if err == nil {
		t.Error("Expected error for nonexistent tool")
	}
}

// --- Video Service Tests ---

func TestVideoService(t *testing.T) {
	svc := NewVideoService()

	if err := svc.StartStream("cam-1", "rtsp://localhost/stream1", "rtsp"); err != nil {
		t.Fatalf("StartStream failed: %v", err)
	}

	stream := svc.GetStream("cam-1")
	if stream == nil {
		t.Fatal("Expected stream after StartStream")
	}
	if stream.URL != "rtsp://localhost/stream1" {
		t.Errorf("Expected URL 'rtsp://localhost/stream1', got '%s'", stream.URL)
	}
	if stream.Status != "started" {
		t.Errorf("Expected status 'started', got '%s'", stream.Status)
	}

	streams := svc.ListStreams()
	if len(streams) != 1 {
		t.Errorf("Expected 1 stream, got %d", len(streams))
	}

	if err := svc.StopStream("cam-1"); err != nil {
		t.Fatalf("StopStream failed: %v", err)
	}

	stream = svc.GetStream("cam-1")
	if stream.Status != "stopped" {
		t.Errorf("Expected status 'stopped', got '%s'", stream.Status)
	}
}

func TestVideoServiceStopNonExistent(t *testing.T) {
	svc := NewVideoService()
	if err := svc.StopStream("nonexistent"); err != nil {
		t.Errorf("StopStream for non-existent should not error, got: %v", err)
	}
}

// --- I18n Service Tests ---

func TestI18nService(t *testing.T) {
	svc := NewI18nService()

	// Default language is zh-CN
	if msg := svc.Translate("device.created"); msg != "设备创建成功" {
		t.Errorf("Expected Chinese message, got '%s'", msg)
	}

	// Switch to English
	svc.SetLanguage("en-US")
	if msg := svc.Translate("device.created"); msg != "Device created successfully" {
		t.Errorf("Expected English message, got '%s'", msg)
	}

	// Unknown key should return the key itself
	if msg := svc.Translate("unknown.key"); msg != "unknown.key" {
		t.Errorf("Expected key as fallback, got '%s'", msg)
	}

	// Unknown language should return the key
	svc.SetLanguage("fr-FR")
	if msg := svc.Translate("device.created"); msg != "device.created" {
		t.Errorf("Expected key for unknown language, got '%s'", msg)
	}
}

// --- Service Manager Tests ---

func TestServiceManager(t *testing.T) {
	sm := NewServiceManager()

	started := false
	stopped := false

	sm.Register("test-service",
		func() error {
			started = true
			return nil
		},
		func() error {
			stopped = true
			return nil
		},
	)

	sm.StartAll()

	status := sm.GetStatus()
	if len(status) != 1 {
		t.Fatalf("Expected 1 service, got %d", len(status))
	}
	if status[0].Status != "running" {
		t.Errorf("Expected status 'running', got '%s'", status[0].Status)
	}
	if !started {
		t.Error("StartFunc should have been called")
	}

	// StartAll again should be idempotent
	sm.StartAll()

	sm.StopAll()
	if !stopped {
		t.Error("StopFunc should have been called")
	}
}

func TestServiceManagerStartError(t *testing.T) {
	sm := NewServiceManager()

	sm.Register("failing-service",
		func() error { return assertError("start failed") },
		func() error { return nil },
	)

	sm.StartAll()

	status := sm.GetStatus()
	if status[0].Status != "error" {
		t.Errorf("Expected status 'error', got '%s'", status[0].Status)
	}
}

// --- Audit Service Tests ---

func TestAuditService(t *testing.T) {
	svc := NewAuditService(nil)

	entry := AuditEntry{
		ID:           "audit-1",
		UserID:       "user-1",
		Username:     "admin",
		Action:       "create",
		ResourceType: "device",
		ResourceID:   "device-1",
		IPAddress:    "127.0.0.1",
	}
	svc.Log(entry)

	entries, total := svc.List(AuditFilter{}, 1, 10)
	if total != 1 {
		t.Errorf("Expected total=1, got %d", total)
	}
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}
	if entries[0].UserID != "user-1" {
		t.Errorf("Expected user-1, got %s", entries[0].UserID)
	}
	if entries[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should be set automatically")
	}
}

func TestAuditServiceFilter(t *testing.T) {
	svc := NewAuditService(nil)

	svc.Log(AuditEntry{ID: "a1", UserID: "user-1", Action: "create"})
	svc.Log(AuditEntry{ID: "a2", UserID: "user-2", Action: "delete"})
	svc.Log(AuditEntry{ID: "a3", UserID: "user-1", Action: "update"})

	// Filter by user
	entries, total := svc.List(AuditFilter{UserID: "user-1"}, 1, 10)
	if total != 2 {
		t.Errorf("Expected 2 entries for user-1, got %d", total)
	}
	if len(entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(entries))
	}

	// Filter by action
	_, total = svc.List(AuditFilter{Action: "delete"}, 1, 10)
	if total != 1 {
		t.Errorf("Expected 1 delete entry, got %d", total)
	}
}

func TestAuditServicePagination(t *testing.T) {
	svc := NewAuditService(nil)

	for i := 0; i < 5; i++ {
		svc.Log(AuditEntry{ID: string(rune('a' + i)), UserID: "user-1", Action: "create"})
	}

	entries, total := svc.List(AuditFilter{}, 2, 2)
	if total != 5 {
		t.Errorf("Expected total=5, got %d", total)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 entries on page 2, got %d", len(entries))
	}

	// Page beyond results
	entries, total = svc.List(AuditFilter{}, 10, 2)
	if total != 5 {
		t.Errorf("Expected total=5, got %d", total)
	}
	if len(entries) != 0 {
		t.Errorf("Expected 0 entries on page 10, got %d", len(entries))
	}
}

func TestAuditServiceOverflow(t *testing.T) {
	svc := NewAuditService(nil)
	svc.maxSize = 3

	for i := 0; i < 5; i++ {
		svc.Log(AuditEntry{ID: string(rune('a' + i)), UserID: "user-1"})
	}

	_, total := svc.List(AuditFilter{}, 1, 10)
	if total != 3 {
		t.Errorf("Expected 3 entries (max), got %d", total)
	}
}

// --- Parse Interval Tests ---

func TestParseInterval(t *testing.T) {
	cases := []struct {
		interval string
		expected time.Duration
	}{
		{"1m", time.Minute},
		{"5m", 5 * time.Minute},
		{"15m", 15 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
	}

	for _, tc := range cases {
		d, err := parseInterval(tc.interval)
		if err != nil {
			t.Errorf("parseInterval('%s') failed: %v", tc.interval, err)
		}
		if d != tc.expected {
			t.Errorf("parseInterval('%s') = %v, expected %v", tc.interval, d, tc.expected)
		}
	}

	// Invalid interval
	_, err := parseInterval("invalid")
	if err == nil {
		t.Error("Expected error for invalid interval")
	}
}

// --- ToFloat64 Tests ---

func TestToFloat64(t *testing.T) {
	if v := toFloat64(float64(42.5)); v != 42.5 {
		t.Errorf("Expected 42.5, got %f", v)
	}
	if v := toFloat64(int(42)); v != 42.0 {
		t.Errorf("Expected 42.0, got %f", v)
	}
	if v := toFloat64(true); v != 1.0 {
		t.Errorf("Expected 1.0 for true, got %f", v)
	}
	if v := toFloat64(false); v != 0.0 {
		t.Errorf("Expected 0.0 for false, got %f", v)
	}
	if v := toFloat64("not a number"); v != 0.0 {
		t.Errorf("Expected 0.0 for string, got %f", v)
	}
}

// --- Helper ---

func assertError(msg string) error {
	return &testError{msg: msg}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

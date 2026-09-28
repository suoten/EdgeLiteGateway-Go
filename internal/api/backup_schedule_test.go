package api

// Tests for PUT/GET /system/backup/schedule. The schedule used to be read-only
// in the API (PUT answered 405) while the scheduler ignored the `backup:`
// section completely, so the page displayed settings nothing acted on.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"edgelite/internal/config"
	"edgelite/internal/services"
)

func writeBackupSourceDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("wal: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t DEFAULT VALUES`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return path
}

func TestUpdateBackupSchedulePersistsAndApplies(t *testing.T) {
	withIsolatedConfig(t)

	backupDir := t.TempDir()
	cfg := config.GetConfig()
	cfg.Backup.BackupDir = backupDir

	cont := GetContainer()
	cont.BackupScheduler = services.NewBackupScheduler(cfg.Backup, writeBackupSourceDB(t))

	c, rec := setupWithAdmin("PUT", "/api/v1/system/backup/schedule",
		`{"enabled":true,"interval_hours":6,"retain_days":3,"min_free_mb":200,"not_a_real_setting":1}`)
	if err := handleUpdateBackupSchedule(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var env struct {
		Code int `json:"code"`
		Data struct {
			Enabled       bool     `json:"enabled"`
			IntervalHours int      `json:"interval_hours"`
			RetainDays    int      `json:"retain_days"`
			MinFreeMB     int      `json:"min_free_mb"`
			BackupDir     string   `json:"backup_dir"`
			UpdatedKeys   []string `json:"updated_keys"`
			IgnoredKeys   []string `json:"ignored_keys"`
			Scheduler     struct {
				Running bool `json:"running"`
			} `json:"scheduler"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Data.IntervalHours != 6 || env.Data.RetainDays != 3 || !env.Data.Enabled || env.Data.MinFreeMB != 200 {
		t.Fatalf("response does not report the new schedule: %+v", env.Data)
	}
	// backup_dir was not in the patch and must survive the merge.
	if env.Data.BackupDir != backupDir {
		t.Errorf("backup_dir = %q, want the stored %q", env.Data.BackupDir, backupDir)
	}
	wantUpdated := []string{"enabled", "interval_hours", "min_free_mb", "retain_days"}
	if !reflect.DeepEqual(env.Data.UpdatedKeys, wantUpdated) {
		t.Errorf("updated_keys = %v, want %v", env.Data.UpdatedKeys, wantUpdated)
	}
	if !reflect.DeepEqual(env.Data.IgnoredKeys, []string{"not_a_real_setting"}) {
		t.Errorf("ignored_keys = %v, want [not_a_real_setting]", env.Data.IgnoredKeys)
	}

	// The response is only honest if a restart reads the same values back.
	persisted := persistedConfig(t)
	if persisted.Backup.IntervalHours != 6 || persisted.Backup.RetainDays != 3 ||
		!persisted.Backup.Enabled || persisted.Backup.MinFreeMB != 200 || persisted.Backup.BackupDir != backupDir {
		t.Errorf("persisted backup section differs: %+v", persisted.Backup)
	}
	// The live scheduler must pick the change up without a restart.
	st := cont.BackupScheduler.Status()
	if st.IntervalHours != 6 || st.RetainDays != 3 || !st.Enabled || st.MinFreeMB != 200 {
		t.Errorf("running scheduler still on the old schedule: %+v", st)
	}
}

func TestUpdateBackupScheduleRejectsBadValue(t *testing.T) {
	withIsolatedConfig(t)
	cont := GetContainer()
	cont.BackupScheduler = services.NewBackupScheduler(config.GetConfig().Backup, "unused.db")

	c, rec := setupWithAdmin("PUT", "/api/v1/system/backup/schedule", `{"interval_hours":"every-day"}`)
	if err := handleUpdateBackupSchedule(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(config.LoadedConfigPath()); err != nil {
		t.Fatalf("config file missing: %v", err)
	}
	if got := persistedConfig(t); got.Backup.IntervalHours == -1 {
		t.Error("persisted config should be untouched")
	}
}

func TestGetBackupScheduleReportsSchedulerState(t *testing.T) {
	withIsolatedConfig(t)

	cfg := config.GetConfig()
	cfg.Backup.BackupDir = t.TempDir()
	cfg.Backup.Enabled = true
	cfg.Backup.IntervalHours = 3

	scheduler := services.NewBackupScheduler(cfg.Backup, writeBackupSourceDB(t))
	cont := GetContainer()
	cont.BackupScheduler = scheduler

	c, rec := setupWithAdmin("GET", "/api/v1/system/backup/schedule", "")
	if err := handleGetBackupSchedule(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	var before struct {
		Data struct {
			LastRun string `json:"last_run"`
			NextRun string `json:"next_run"`
			Running bool   `json:"running"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Data.LastRun != "" {
		t.Errorf("last_run should be empty before any backup, got %q", before.Data.LastRun)
	}

	if _, _, err := scheduler.BackupNow(); err != nil {
		t.Fatalf("BackupNow: %v", err)
	}

	c2, rec2 := setupWithAdmin("GET", "/api/v1/system/backup/schedule", "")
	if err := handleGetBackupSchedule(c2); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	var after struct {
		Data struct {
			LastRun  string `json:"last_run"`
			NextRun  string `json:"next_run"`
			Interval int    `json:"interval_hours"`
			MinFree  int    `json:"min_free_mb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Data.LastRun == "" {
		t.Error("last_run should reflect the backup that just ran")
	}
	// next_run only exists for a running loop; the scheduler is not started here,
	// so an empty value is the honest answer.
	if after.Data.NextRun != "" {
		t.Errorf("next_run should stay empty while the loop is stopped, got %q", after.Data.NextRun)
	}
	if after.Data.Interval != 3 {
		t.Errorf("interval_hours = %d, want 3", after.Data.Interval)
	}
}

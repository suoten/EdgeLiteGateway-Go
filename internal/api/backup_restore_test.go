package api

// Tests for POST /system/backup/restore. The endpoint used to answer
// "Restore initiated. Service restart required." with HTTP 200 while writing
// nothing anywhere, so two pages told the operator the restore had succeeded.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"edgelite/internal/config"
	"edgelite/internal/storage"
)

func makeRestoreBackupFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("wal: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (note TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO marker VALUES ('from-backup')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestRestoreStagesForNextStart(t *testing.T) {
	withIsolatedConfig(t)

	dir := t.TempDir()
	cfg := config.GetConfig()
	cfg.Database.SQLitePath = filepath.Join(dir, "app.db")
	cfg.Backup.BackupDir = filepath.Join(dir, "backups")

	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	GetContainer().Database = db

	const name = "edgelite_backup_test.db"
	makeRestoreBackupFile(t, filepath.Join(cfg.Backup.BackupDir, name))

	c, rec := setupWithAdmin("POST", "/api/v1/system/backup/restore", `{"filename":"`+name+`"}`)
	if err := handleRestoreBackup(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Filename    string `json:"filename"`
			Staged      bool   `json:"staged"`
			Applied     bool   `json:"applied"`
			EffectiveOn string `json:"effective_on"`
			Checksum    string `json:"checksum"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Data.Staged || env.Data.Applied {
		t.Errorf("response must say staged-not-applied, got %+v", env.Data)
	}
	if env.Data.EffectiveOn != "restart" {
		t.Errorf("effective_on = %q, want restart", env.Data.EffectiveOn)
	}
	if len(env.Data.Checksum) != 64 {
		t.Errorf("checksum = %q, want a sha256 hex digest", env.Data.Checksum)
	}

	// The marker has to be on disk, because that is the only thing that makes
	// the claim true.
	marker, err := storage.LoadPendingRestore(db.Path())
	if err != nil || marker == nil {
		t.Fatalf("no staged restore marker was written: pr=%v err=%v", marker, err)
	}
	if marker.Filename != name || marker.Checksum != env.Data.Checksum {
		t.Errorf("marker content = %+v", marker)
	}

	// The schedule endpoint has to surface it, and cancel has to remove it.
	gc, grec := setupWithAdmin("GET", "/api/v1/system/backup/schedule", "")
	if err := handleGetBackupSchedule(gc); err != nil {
		t.Fatalf("GET handler error: %v", err)
	}
	var getEnv struct {
		Data struct {
			PendingRestore *struct {
				Filename string `json:"filename"`
			} `json:"pending_restore"`
		} `json:"data"`
	}
	if err := json.Unmarshal(grec.Body.Bytes(), &getEnv); err != nil {
		t.Fatalf("decode schedule: %v", err)
	}
	if getEnv.Data.PendingRestore == nil || getEnv.Data.PendingRestore.Filename != name {
		t.Errorf("GET /system/backup/schedule does not report the staged restore: %s", grec.Body.String())
	}

	cc, crec := setupWithAdmin("POST", "/api/v1/system/backup/restore", `{"cancel":true}`)
	if err := handleRestoreBackup(cc); err != nil {
		t.Fatalf("cancel handler error: %v", err)
	}
	if crec.Code != 200 {
		t.Fatalf("cancel status = %d, want 200", crec.Code)
	}
	if pr, err := storage.LoadPendingRestore(db.Path()); err != nil || pr != nil {
		t.Errorf("marker should be gone after cancel, got pr=%v err=%v", pr, err)
	}
}

func TestRestoreRejectsUnusableBackup(t *testing.T) {
	withIsolatedConfig(t)

	dir := t.TempDir()
	cfg := config.GetConfig()
	cfg.Database.SQLitePath = filepath.Join(dir, "app.db")
	cfg.Backup.BackupDir = filepath.Join(dir, "backups")

	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	GetContainer().Database = db

	if err := os.MkdirAll(cfg.Backup.BackupDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	broken := filepath.Join(cfg.Backup.BackupDir, "broken.db")
	if err := os.WriteFile(broken, []byte("truncate me not"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c, rec := setupWithAdmin("POST", "/api/v1/system/backup/restore", `{"filename":"broken.db"}`)
	if err := handleRestoreBackup(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 for a backup that is not a usable database (body %s)", rec.Code, rec.Body.String())
	}
	if pr, err := storage.LoadPendingRestore(db.Path()); err != nil || pr != nil {
		t.Errorf("an unusable backup must not be staged, got pr=%v err=%v", pr, err)
	}

	missing, mrec := setupWithAdmin("POST", "/api/v1/system/backup/restore", `{"filename":"edgelite_backup_never_taken.db"}`)
	if err := handleRestoreBackup(missing); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if mrec.Code != 404 {
		t.Errorf("status = %d, want 404 for a backup that does not exist", mrec.Code)
	}
}

// The restore list is the operator's only view of what can be rolled back to, so
// it must not offer a snapshot's WAL/SHM sidecar as if it were a database.
func TestListBackupsSkipsSqliteSidecars(t *testing.T) {
	withIsolatedConfig(t)

	dir := t.TempDir()
	cfg := config.GetConfig()
	cfg.Backup.BackupDir = dir
	const snapshot = "edgelite_backup_20260101_000000.db"
	for _, name := range []string{snapshot, snapshot + "-wal", snapshot + "-shm", "pre_restore_20260101_000001.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	c, rec := setupWithAdmin("GET", "/api/v1/system/backup", "")
	if err := handleListBackups(c); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	var env struct {
		Data []struct {
			Filename string `json:"filename"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, b := range env.Data {
		got[b.Filename] = true
	}
	if !got[snapshot] || !got["pre_restore_20260101_000001.db"] {
		t.Errorf("real snapshots must be listed, got %s", rec.Body.String())
	}
	for _, sidecar := range []string{snapshot + "-wal", snapshot + "-shm"} {
		if got[sidecar] {
			t.Errorf("sidecar %s must not be offered as a backup", sidecar)
		}
	}
}

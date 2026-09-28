package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeSQLiteDB creates a database with one row whose note identifies it, so a
// test can tell which file the gateway is actually serving.
func makeSQLiteDB(t *testing.T, path, note string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("wal: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS marker (note TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM marker`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO marker (note) VALUES (?)`, note); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var note string
	if err := db.QueryRow(`SELECT note FROM marker LIMIT 1`).Scan(&note); err != nil {
		return "ERR:" + err.Error()
	}
	return note
}

func TestPendingRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeSQLiteDB(t, dbPath, "live")

	backupPath := filepath.Join(backupDir, "edgelite_backup_test.db")
	makeSQLiteDB(t, backupPath, "from-backup")
	sum, err := FileChecksum(backupPath)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}

	if err := StageRestore(dbPath, PendingRestore{
		Filename:   filepath.Base(backupPath),
		BackupPath: backupPath,
		Checksum:   sum,
	}); err != nil {
		t.Fatalf("StageRestore: %v", err)
	}
	pr, err := LoadPendingRestore(dbPath)
	if err != nil || pr == nil {
		t.Fatalf("LoadPendingRestore: pr=%v err=%v", pr, err)
	}
	if pr.Filename != filepath.Base(backupPath) {
		t.Errorf("staged filename = %q", pr.Filename)
	}

	applied, snapshot, err := ApplyPendingRestore(dbPath, backupDir)
	if err != nil {
		t.Fatalf("ApplyPendingRestore: %v", err)
	}
	if !applied {
		t.Fatal("ApplyPendingRestore reported nothing applied")
	}
	if got := readMarker(t, dbPath); got != "from-backup" {
		t.Errorf("database now holds %q, the restore did not replace it", got)
	}
	if snapshot == "" || filepath.Dir(snapshot) != backupDir {
		t.Errorf("pre-restore snapshot should land in the backup dir for undo, got %q", snapshot)
	} else if got := readMarker(t, snapshot); got != "live" {
		t.Errorf("pre-restore snapshot holds %q, want the previous database %q", got, "live")
	}
	if _, err := os.Stat(PendingRestorePath(dbPath)); !os.IsNotExist(err) {
		t.Error("the marker must be cleared once applied, otherwise every boot re-restores")
	}
	// The WAL of the replaced database must be gone, or SQLite would replay it
	// on top of the restored file.
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Error("stale -wal left behind after restore")
	}
	// A second start finds nothing staged.
	again, _, err := ApplyPendingRestore(dbPath, backupDir)
	if err != nil || again {
		t.Errorf("second apply = %v, err = %v, want false/nil", again, err)
	}
}

func TestApplyPendingRestoreRejectsTamperedBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	makeSQLiteDB(t, dbPath, "live")
	backupPath := filepath.Join(dir, "backup.db")
	makeSQLiteDB(t, backupPath, "from-backup")
	sum, err := FileChecksum(backupPath)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if err := StageRestore(dbPath, PendingRestore{Filename: "backup.db", BackupPath: backupPath, Checksum: sum}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// The operator's backup file is replaced after the request was made.
	makeSQLiteDB(t, backupPath, "evil")

	applied, _, err := ApplyPendingRestore(dbPath, dir)
	if err == nil || !strings.Contains(err.Error(), "changed after it was requested") {
		t.Fatalf("want a checksum mismatch error, got applied=%v err=%v", applied, err)
	}
	if applied {
		t.Error("a tampered backup must not be applied")
	}
	if got := readMarker(t, dbPath); got != "live" {
		t.Errorf("the live database was modified: %q", got)
	}
	if _, err := LoadPendingRestore(dbPath); err != nil {
		t.Errorf("the marker should survive a failed apply so the operator sees it: %v", err)
	}
}

func TestApplyPendingRestoreRejectsCorruptBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	makeSQLiteDB(t, dbPath, "live")
	backupPath := filepath.Join(dir, "broken.db")
	if err := os.WriteFile(backupPath, []byte("not a database at all"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum, err := FileChecksum(backupPath)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if err := StageRestore(dbPath, PendingRestore{Filename: "broken.db", BackupPath: backupPath, Checksum: sum}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if _, _, err := ApplyPendingRestore(dbPath, dir); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("want an integrity-check error, got %v", err)
	}
	if got := readMarker(t, dbPath); got != "live" {
		t.Errorf("the live database was modified: %q", got)
	}
}

func TestLoadPendingRestoreWithoutMarker(t *testing.T) {
	pr, err := LoadPendingRestore(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("LoadPendingRestore: %v", err)
	}
	if pr != nil {
		t.Errorf("expected no staged restore, got %+v", pr)
	}
	if err := ClearPendingRestore(filepath.Join(t.TempDir(), "app.db")); err != nil {
		t.Errorf("ClearPendingRestore on a clean instance: %v", err)
	}
}

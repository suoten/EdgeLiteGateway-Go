package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"edgelite/internal/config"
)

func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='t'`).Scan(&name); err == sql.ErrNoRows {
		return -1
	} else if err != nil {
		t.Fatalf("inspect schema in %s: %v", path, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count rows in %s: %v", path, err)
	}
	return n
}

// TestBackupToIncludesUncheckpointedWAL is the regression guard for the old
// os.ReadFile copy: in WAL mode committed rows sit in <db>-wal until a
// checkpoint, so a byte copy of the main file restores an older database while
// reporting success.
func TestBackupToIncludesUncheckpointedWAL(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(dir, "app.db")
	cfg.Database.PoolSize = 1

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	defer db.Close()

	ctx := t.Context()
	if _, err := db.DB().ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO t (note) VALUES ('one'), ('two'), ('three')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The writer is still open, so nothing has been checkpointed into app.db yet.
	wal := cfg.Database.SQLitePath + "-wal"
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("expected a WAL file next to the database: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("expected uncheckpointed WAL data, but the WAL file is empty")
	}

	naive := filepath.Join(dir, "naive_copy.db")
	raw, err := os.ReadFile(cfg.Database.SQLitePath)
	if err != nil {
		t.Fatalf("read main db: %v", err)
	}
	if err := os.WriteFile(naive, raw, 0600); err != nil {
		t.Fatalf("write naive copy: %v", err)
	}
	copied := countRows(t, naive)
	if copied >= 3 {
		t.Fatalf("expected the raw file copy to lose WAL data, but it contains %d rows", copied)
	}
	t.Logf("raw file copy of the live database is lossy: table t row count = %d (-1 means the table itself is missing)", copied)

	snapshot := filepath.Join(dir, "backup.db")
	size, err := db.BackupTo(ctx, snapshot)
	if err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if size <= 0 {
		t.Fatalf("BackupTo reported size %d", size)
	}
	if got := countRows(t, snapshot); got != 3 {
		t.Errorf("VACUUM INTO backup contains %d rows, want 3", got)
	}

	// The scheduler only knows the database by path, so the read-only reopen has
	// to cope with a WAL that a live connection still owns.
	byPath := filepath.Join(dir, "backup-by-path.db")
	size, err = BackupSQLiteFile(cfg.Database.SQLitePath, byPath)
	if err != nil {
		t.Fatalf("BackupSQLiteFile while the pool is open: %v", err)
	}
	if size <= 0 {
		t.Fatalf("BackupSQLiteFile reported size %d", size)
	}
	if got := countRows(t, byPath); got != 3 {
		t.Errorf("path-based backup contains %d rows, want 3", got)
	}
}

// TestBackupSQLiteFileWithoutLiveWriter covers the scheduler path, which only
// knows the database by path and runs while the application connection may
// already be closed.
func TestBackupSQLiteFileWithoutLiveWriter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "closed.db")

	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (note) VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Closing checkpoints the WAL back into the main file.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	dst := filepath.Join(dir, "backup.db")
	size, err := BackupSQLiteFile(src, dst)
	if err != nil {
		t.Fatalf("BackupSQLiteFile: %v", err)
	}
	if size <= 0 {
		t.Fatalf("BackupSQLiteFile reported size %d", size)
	}
	if got := countRows(t, dst); got != 2 {
		t.Errorf("backup contains %d rows, want 2", got)
	}
}

func TestBackupSQLiteFileRejectsNonDatabaseSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "not-a-db.db")
	if err := os.WriteFile(src, []byte("test database content"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := BackupSQLiteFile(src, filepath.Join(dir, "backup.db")); err == nil {
		t.Fatal("expected an error for a source that is not a SQLite database, got nil")
	}
}

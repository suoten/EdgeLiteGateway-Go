package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // register sqlite driver (side-effect import)
)

// Staged restore. The gateway keeps the database open in WAL mode, so a backup
// can only replace it while no connection owns the file. A restore therefore
// writes a marker next to the database and the next boot applies it before the
// pool is created.

// PendingRestore describes a restore that was requested through the API and is
// waiting for the next start.
type PendingRestore struct {
	Filename   string    `json:"filename"`
	BackupPath string    `json:"backup_path"`
	Checksum   string    `json:"checksum"`
	StagedAt   time.Time `json:"staged_at"`
}

// PendingRestorePath returns the marker file for a database path.
func PendingRestorePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), ".restore_pending.json")
}

// FileChecksum returns the SHA-256 of a file, so a backup swapped on disk after
// the operator asked for it is detected instead of restored.
func FileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifySQLiteFile opens a database read-only and asks SQLite whether it is
// structurally sound. Restoring a truncated or corrupted snapshot would take the
// gateway down at boot, so it is rejected here instead.
func VerifySQLiteFile(path string) error {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", filepath.ToSlash(path)))
	if err != nil {
		return fmt.Errorf("open %s for verification: %w", path, err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("integrity check on %s: %w", path, err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed for %s: %s", path, result)
	}
	return nil
}

// StageRestore records that the operator asked for this backup to become the
// database on the next start.
func StageRestore(dbPath string, pr PendingRestore) error {
	if pr.Filename == "" || pr.BackupPath == "" {
		return fmt.Errorf("restore request is incomplete")
	}
	data, err := json.MarshalIndent(&pr, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore request: %w", err)
	}
	path := PendingRestorePath(dbPath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write restore marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit restore marker: %w", err)
	}
	return nil
}

// LoadPendingRestore returns nil when no restore is waiting.
func LoadPendingRestore(dbPath string) (*PendingRestore, error) {
	data, err := os.ReadFile(PendingRestorePath(dbPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read restore marker: %w", err)
	}
	var pr PendingRestore
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("restore marker at %s is corrupt: %w", PendingRestorePath(dbPath), err)
	}
	return &pr, nil
}

// ClearPendingRestore removes the marker after a restore has been applied (or
// after the operator cancelled it).
func ClearPendingRestore(dbPath string) error {
	if err := os.Remove(PendingRestorePath(dbPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove restore marker: %w", err)
	}
	return nil
}

// ApplyPendingRestore replaces the live database with the staged backup. It
// snapshots the current database into snapshotDir first, so a wrong choice
// stays undoable from the backup list, and drops the WAL/shm files that belong
// to the database being replaced. applied is false when no restore is waiting.
func ApplyPendingRestore(dbPath, snapshotDir string) (applied bool, snapshotPath string, err error) {
	pr, err := LoadPendingRestore(dbPath)
	if err != nil {
		return false, "", err
	}
	if pr == nil {
		return false, "", nil
	}
	if _, err := os.Stat(pr.BackupPath); err != nil {
		return false, "", fmt.Errorf("staged backup %s is not available: %w", pr.BackupPath, err)
	}
	sum, err := FileChecksum(pr.BackupPath)
	if err != nil {
		return false, "", err
	}
	if pr.Checksum != "" && sum != pr.Checksum {
		return false, "", fmt.Errorf("staged backup %s changed after it was requested (sha256 %s, expected %s)", pr.BackupPath, sum, pr.Checksum)
	}
	if err := VerifySQLiteFile(pr.BackupPath); err != nil {
		return false, "", err
	}

	if snapshotDir == "" {
		snapshotDir = filepath.Dir(dbPath)
	}
	snapshotPath = filepath.Join(
		snapshotDir,
		fmt.Sprintf("pre_restore_%s.db", time.Now().Format("20060102_150405")),
	)
	if _, err := os.Stat(dbPath); err == nil {
		if _, err := BackupSQLiteFile(dbPath, snapshotPath); err != nil {
			return false, "", fmt.Errorf("snapshot the current database before restore: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return false, "", err
	} else {
		snapshotPath = ""
	}

	if err := replaceFileWith(pr.BackupPath, dbPath); err != nil {
		return false, snapshotPath, err
	}
	// The old WAL belongs to the database that was just replaced.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return false, snapshotPath, fmt.Errorf("remove %s: %w", dbPath+suffix, err)
		}
	}
	if err := ClearPendingRestore(dbPath); err != nil {
		return false, snapshotPath, err
	}
	return true, snapshotPath, nil
}

func replaceFileWith(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	tmp := dst + ".restoring"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate %s: %w", dst, err)
	}
	return nil
}

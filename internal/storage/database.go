// Package storage provides database and time-series storage for EdgeLiteGateway.
package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"edgelite/internal/config"

	"github.com/sirupsen/logrus"
)

// Database wraps a SQLite database connection with WAL mode.
// It provides health checking, automatic reconnection, and connection pooling.
type Database struct {
	db         *sql.DB
	path       string
	mu         sync.Mutex
	auditDB    string
	healthy    atomic.Bool
	closed     atomic.Bool
	dsn        string
	cfg        *config.AppConfig
	stopHealth chan struct{}
	wg         sync.WaitGroup
}

// NewDatabase creates a new Database instance.
// It configures connection pooling, performs an initial health check,
// and starts a background health monitor that automatically reconnects
// on connection loss.
func NewDatabase(cfg *config.AppConfig) (*Database, error) {
	dbPath := cfg.Database.SQLitePath
	// modernc.org/sqlite requires _pragma=... params; mattn-style params
	// (_busy_timeout= etc.) are silently ignored, leaving busy_timeout=0 and
	// journal_mode=DELETE, which causes intermittent SQLITE_BUSY errors.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", dbPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	maxOpen := cfg.Database.PoolSize + cfg.Database.MaxOverflow
	if maxOpen <= 0 {
		maxOpen = 15
	}
	maxIdle := cfg.Database.PoolSize
	if maxIdle <= 0 {
		maxIdle = 5
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(30 * time.Minute) // reclaim idle connections

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	d := &Database{
		db:         db,
		path:       dbPath,
		auditDB:    "data/audit.db",
		dsn:        dsn,
		cfg:        cfg,
		stopHealth: make(chan struct{}),
	}
	d.healthy.Store(true)

	if err := d.InitTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to init tables: %w", err)
	}

	// Start background health monitor
	d.wg.Add(1)
	go d.healthMonitor()

	return d, nil
}

// DB returns the underlying *sql.DB.
func (d *Database) DB() *sql.DB {
	return d.db
}

// Path returns the database file path.
func (d *Database) Path() string {
	return d.path
}

// Close closes the database connection and stops the health monitor.
func (d *Database) Close() error {
	d.closed.Store(true)
	d.healthy.Store(false)
	close(d.stopHealth)
	d.wg.Wait()
	// Close all idle connections before closing to ensure Windows releases the file lock
	d.db.SetMaxOpenConns(0)
	d.db.SetMaxIdleConns(0)
	return d.db.Close()
}

// IsHealthy returns whether the database is currently healthy.
func (d *Database) IsHealthy() bool {
	return d.healthy.Load()
}

// HealthCheck performs a ping against the database and updates the health status.
// Returns an error if the database is not reachable.
func (d *Database) HealthCheck() error {
	if err := d.db.Ping(); err != nil {
		if d.healthy.Load() {
			logrus.WithError(err).Error("Database health check failed")
			d.healthy.Store(false)
		}
		return err
	}
	if !d.healthy.Load() {
		logrus.Info("Database health check passed, marking as healthy")
		d.healthy.Store(true)
	}
	return nil
}

// healthMonitor runs a background goroutine that periodically pings the database.
// On failure, it attempts to reconnect by closing and reopening the connection.
func (d *Database) healthMonitor() {
	defer d.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopHealth:
			return
		case <-ticker.C:
			if d.closed.Load() {
				return
			}
			if err := d.db.Ping(); err != nil {
				if d.healthy.Load() {
					logrus.WithError(err).Warn("Database ping failed, attempting reconnect")
					d.healthy.Store(false)
				}
				d.tryReconnect()
			} else if !d.healthy.Load() {
				logrus.Info("Database reconnected successfully")
				d.healthy.Store(true)
			}
		}
	}
}

// tryReconnect attempts to close and reopen the database connection.
// It retries up to 3 times with exponential backoff.
func (d *Database) tryReconnect() {
	d.mu.Lock()
	defer d.mu.Unlock()

	maxRetries := 3
	backoff := 2 * time.Second

	for i := 0; i < maxRetries; i++ {
		if d.closed.Load() {
			return
		}

		// Close old connection (ignore error — it may already be broken)
		_ = d.db.Close()
		time.Sleep(backoff)

		db, err := sql.Open("sqlite", d.dsn)
		if err != nil {
			logrus.WithError(err).WithField("attempt", i+1).Warn("Failed to reopen database")
			backoff *= 2
			continue
		}

		maxOpen := d.cfg.Database.PoolSize + d.cfg.Database.MaxOverflow
		if maxOpen <= 0 {
			maxOpen = 15
		}
		maxIdle := d.cfg.Database.PoolSize
		if maxIdle <= 0 {
			maxIdle = 5
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxIdle)
		db.SetConnMaxLifetime(time.Hour)
		db.SetConnMaxIdleTime(30 * time.Minute)

		if err := db.Ping(); err != nil {
			_ = db.Close()
			logrus.WithError(err).WithField("attempt", i+1).Warn("Failed to ping reopened database")
			backoff *= 2
			continue
		}

		// Success — swap in new connection
		d.db = db
		logrus.WithField("attempt", i+1).Info("Database reconnected successfully")
		return
	}

	logrus.Error("Database reconnect failed after all retries")
}

// InitTables creates all required tables if they don't exist.
func (d *Database) InitTables() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS devices (
			device_id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'unknown',
			config TEXT NOT NULL DEFAULT '{}',
			points TEXT NOT NULL DEFAULT '[]',
			collect_interval INTEGER NOT NULL DEFAULT 5,
			created_by TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			version INTEGER NOT NULL DEFAULT 1,
			write_verify INTEGER NOT NULL DEFAULT 1,
			write_rate_limit INTEGER NOT NULL DEFAULT 0,
			write_audit INTEGER NOT NULL DEFAULT 1,
			write_whitelist TEXT NOT NULL DEFAULT '[]'
		)`,
		`CREATE TABLE IF NOT EXISTS rules (
			rule_id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			device_id TEXT,
			conditions TEXT NOT NULL DEFAULT '[]',
			logic TEXT NOT NULL DEFAULT 'AND',
			duration INTEGER NOT NULL DEFAULT 0,
			severity TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			notify_channels TEXT NOT NULL DEFAULT '["dingtalk"]',
			script TEXT,
			rule_type TEXT,
			created_by TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			version INTEGER NOT NULL DEFAULT 1,
			inference_count INTEGER NOT NULL DEFAULT 0,
			error_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS alarms (
			alarm_id TEXT PRIMARY KEY,
			rule_id TEXT NOT NULL,
			device_id TEXT,
			severity TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'firing',
			message TEXT NOT NULL DEFAULT '',
			trigger_value TEXT NOT NULL DEFAULT '{}',
			trigger_count INTEGER NOT NULL DEFAULT 1,
			fired_at TEXT NOT NULL,
			acknowledged_at TEXT,
			acknowledged_by TEXT,
			recovered_at TEXT,
			rule_type TEXT NOT NULL DEFAULT 'threshold',
			version INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS alarm_silences (
			id TEXT PRIMARY KEY,
			alarm_id TEXT,
			device_id TEXT,
			rule_id TEXT,
			start_time TEXT NOT NULL,
			end_time TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			operator TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			cancelled INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_alarm_silences_active ON alarm_silences(cancelled, end_time)`,
		`CREATE TABLE IF NOT EXISTS device_linkages (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			source_device_id TEXT NOT NULL,
			source_point TEXT NOT NULL,
			condition_op TEXT NOT NULL,
			threshold REAL NOT NULL DEFAULT 0,
			target_device_id TEXT NOT NULL,
			target_point TEXT NOT NULL,
			target_value TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			trigger_count INTEGER NOT NULL DEFAULT 0,
			last_triggered_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			user_id TEXT PRIMARY KEY,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT,
			version INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS templates (
			name TEXT PRIMARY KEY,
			protocol TEXT NOT NULL,
			config_template TEXT NOT NULL DEFAULT '{}',
			point_templates TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS rate_limits (
			key TEXT PRIMARY KEY,
			count INTEGER NOT NULL DEFAULT 0,
			window_start TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS config_versions (
			version INTEGER PRIMARY KEY AUTOINCREMENT,
			config_json TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			created_by TEXT,
			change_summary TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS scripts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			language TEXT NOT NULL DEFAULT 'javascript',
			code TEXT NOT NULL DEFAULT '',
			timeout_ms INTEGER NOT NULL DEFAULT 5000,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS rule_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id TEXT NOT NULL,
			version INTEGER NOT NULL,
			snapshot TEXT NOT NULL,
			snapshot_hash TEXT NOT NULL,
			change_summary TEXT,
			created_by TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(rule_id, version)
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			session_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			username TEXT NOT NULL,
			role TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			expires_at TEXT NOT NULL,
			ip_address TEXT,
			user_agent TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS ai_model_stats (
			model_id TEXT PRIMARY KEY,
			inference_count INTEGER NOT NULL DEFAULT 0,
			error_count INTEGER NOT NULL DEFAULT 0,
			total_latency_ms REAL NOT NULL DEFAULT 0,
			last_inference_at TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			resource_type TEXT NOT NULL DEFAULT '',
			resource_id TEXT NOT NULL DEFAULT '',
			ip_address TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'success',
			details TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			prev_hash TEXT NOT NULL DEFAULT '',
			hash TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS downsample_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tier INTEGER NOT NULL DEFAULT 0,
			rows_processed INTEGER NOT NULL DEFAULT 0,
			rows_archived INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'success',
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			completed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS export_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			filename TEXT NOT NULL,
			device_id TEXT NOT NULL,
			point_name TEXT NOT NULL DEFAULT '',
			start_time TEXT NOT NULL DEFAULT '',
			end_time TEXT NOT NULL DEFAULT '',
			format TEXT NOT NULL DEFAULT 'csv',
			row_count INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'success',
			created_at TEXT NOT NULL
		)`,
	}

	for _, q := range queries {
		if _, err := d.db.Exec(q); err != nil {
			return fmt.Errorf("failed to execute: %w", err)
		}
	}

	// Create indexes for query performance optimization.
	// These indexes cover the most frequent query patterns used by repositories.
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_devices_protocol ON devices(protocol)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_status ON devices(status)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_created_at ON devices(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_device_id ON rules(device_id)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_enabled ON rules(enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_created_at ON rules(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_status ON alarms(status)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_severity ON alarms(severity)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_device_id ON alarms(device_id)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_rule_id ON alarms(rule_id)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_fired_at ON alarms(fired_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_rule_versions_rule_id ON rule_versions(rule_id, version)`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,
		`CREATE INDEX IF NOT EXISTS idx_users_user_id ON users(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_templates_name ON templates(name)`,
		`CREATE INDEX IF NOT EXISTS idx_alarms_status_fired_at ON alarms(status, fired_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs(action)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_user_id ON audit_logs(user_id)`,
	}
	for _, idx := range indexes {
		if _, err := d.db.Exec(idx); err != nil {
			logrus.WithError(err).WithField("index", idx).Warn("Failed to create index")
			// Non-fatal: index creation failure should not block startup
		}
	}

	return nil
}

// GetAuditDBPath returns the audit database path.
func (d *Database) GetAuditDBPath() string {
	return d.auditDB
}

// GetSetting returns a persisted system setting value, or "" when unset.
func (d *Database) GetSetting(key string) (string, error) {
	var value string
	err := d.db.QueryRow(`SELECT value FROM system_settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

// SetSetting upserts a persisted system setting.
func (d *Database) SetSetting(key, value string) error {
	_, err := d.db.Exec(`INSERT INTO system_settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value)
	return err
}

// DeleteSetting removes a persisted system setting. It does not report whether
// the key existed: callers that must distinguish "already absent" read first.
func (d *Database) DeleteSetting(key string) error {
	_, err := d.db.Exec(`DELETE FROM system_settings WHERE key = ?`, key)
	return err
}

// ListSettingKeys returns every setting key with the given prefix, sorted.
// Callers that store one family of records per key use this to enumerate them.
func (d *Database) ListSettingKeys(prefix string) ([]string, error) {
	rows, err := d.db.Query(`SELECT key FROM system_settings WHERE key LIKE ? ORDER BY key`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ============================================================================
// Audit log persistence — append-only trail with per-row hash tamper evidence.
// ============================================================================

// auditWriteMu serializes audit inserts so the hash chain stays consistent
// when concurrent handlers log events at the same time.
var auditWriteMu sync.Mutex

// AuditRecord is one persisted audit trail row. Details is pre-marshaled JSON,
// CreatedAt a local RFC3339 string (consistent with the rest of the schema).
type AuditRecord struct {
	ID           int64
	UserID       string
	Username     string
	Action       string
	ResourceType string
	ResourceID   string
	IPAddress    string
	Status       string
	Details      string
	CreatedAt    string
}

func auditRowHash(prevHash string, r AuditRecord) string {
	h := sha256.New()
	for _, part := range []string{prevHash, r.CreatedAt, r.UserID, r.Username, r.Action,
		r.ResourceType, r.ResourceID, r.IPAddress, r.Status, r.Details} {
		h.Write([]byte(part))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// InsertAuditLog appends one audit row, chaining its hash to the previous row.
func (d *Database) InsertAuditLog(r AuditRecord) (int64, error) {
	auditWriteMu.Lock()
	defer auditWriteMu.Unlock()

	var prevHash string
	_ = d.db.QueryRow(`SELECT hash FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&prevHash)
	hash := auditRowHash(prevHash, r)
	res, err := d.db.Exec(`INSERT INTO audit_logs
		(user_id, username, action, resource_type, resource_id, ip_address, status, details, created_at, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.UserID, r.Username, r.Action, r.ResourceType, r.ResourceID, r.IPAddress, r.Status, r.Details, r.CreatedAt, prevHash, hash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAuditLogs returns a filtered, newest-first page of audit rows plus the
// total count. userID matches either the stored user_id or the username, since
// the UI filter is a free-text input that may hold either. resourceID narrows the
// result to one object (a device write history, for instance), and status to one
// outcome. Both are applied in SQL rather than after paging so a page cannot come
// back empty by luck.
func (d *Database) ListAuditLogs(userID, action, resourceType, resourceID, status, startTime, endTime string, page, size int) ([]AuditRecord, int, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}

	where := []string{"1=1"}
	args := []interface{}{}
	if userID != "" {
		where = append(where, "(user_id = ? OR username = ?)")
		args = append(args, userID, userID)
	}
	if action != "" {
		where = append(where, "action = ?")
		args = append(args, action)
	}
	if resourceType != "" {
		where = append(where, "resource_type = ?")
		args = append(args, resourceType)
	}
	if resourceID != "" {
		where = append(where, "resource_id = ?")
		args = append(args, resourceID)
	}
	if status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if startTime != "" {
		where = append(where, "created_at >= ?")
		args = append(args, startTime)
	}
	if endTime != "" {
		where = append(where, "created_at <= ?")
		args = append(args, endTime)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := d.db.Query(`SELECT id, user_id, username, action, resource_type, resource_id, ip_address, status, details, created_at
		FROM audit_logs WHERE `+whereSQL+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	recs := make([]AuditRecord, 0, size)
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.Action, &r.ResourceType, &r.ResourceID, &r.IPAddress, &r.Status, &r.Details, &r.CreatedAt); err != nil {
			return nil, 0, err
		}
		recs = append(recs, r)
	}
	return recs, total, rows.Err()
}

// AuditIntegrity recomputes each row's tamper-evidence hash and verifies chain
// linkage. The first surviving row is treated as genesis — its predecessor may
// legitimately have been removed by retention cleanup.
func (d *Database) AuditIntegrity() (total int, brokenAt []int64, err error) {
	rows, err := d.db.Query(`SELECT id, user_id, username, action, resource_type, resource_id, ip_address, status, details, created_at, prev_hash, hash
		FROM audit_logs ORDER BY id ASC`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	brokenAt = []int64{}
	lastHash := ""
	first := true
	for rows.Next() {
		var r AuditRecord
		var prevHash, storedHash string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.Action, &r.ResourceType, &r.ResourceID, &r.IPAddress, &r.Status, &r.Details, &r.CreatedAt, &prevHash, &storedHash); err != nil {
			return 0, nil, err
		}
		total++
		if expected := auditRowHash(prevHash, r); expected != storedHash {
			if len(brokenAt) < 100 {
				brokenAt = append(brokenAt, r.ID)
			}
		} else if !first && prevHash != lastHash {
			if len(brokenAt) < 100 {
				brokenAt = append(brokenAt, r.ID)
			}
		}
		lastHash = storedHash
		first = false
	}
	return total, brokenAt, rows.Err()
}

// CleanupAuditLogs deletes rows older than the given local RFC3339 cutoff.
func (d *Database) CleanupAuditLogs(before string) (int64, error) {
	res, err := d.db.Exec(`DELETE FROM audit_logs WHERE created_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DownsampleRun is one recorded execution of a downsample tier.
type DownsampleRun struct {
	ID            int64  `json:"id"`
	Tier          int    `json:"tier"`
	RowsProcessed int64  `json:"rows_processed"`
	RowsArchived  int64  `json:"rows_archived"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	StartedAt     string `json:"started_at"`
	CompletedAt   string `json:"completed_at"`
}

func (d *Database) InsertDownsampleRun(r DownsampleRun) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO downsample_runs (tier, rows_processed, rows_archived, status, error, started_at, completed_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Tier, r.RowsProcessed, r.RowsArchived, r.Status, r.Error, r.StartedAt, r.CompletedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *Database) ListDownsampleRuns(limit int) ([]DownsampleRun, error) {
	rows, err := d.db.Query(
		`SELECT id, tier, rows_processed, rows_archived, status, error, started_at, completed_at FROM downsample_runs ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DownsampleRun
	for rows.Next() {
		var r DownsampleRun
		if err := rows.Scan(&r.ID, &r.Tier, &r.RowsProcessed, &r.RowsArchived, &r.Status, &r.Error, &r.StartedAt, &r.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DownsampleNetDeleted returns the cumulative raw rows removed by downsampling.
func (d *Database) DownsampleNetDeleted() (int64, error) {
	var net int64
	err := d.db.QueryRow(`SELECT COALESCE(SUM(rows_processed - rows_archived), 0) FROM downsample_runs WHERE status = 'success'`).Scan(&net)
	return net, err
}

// ExportRecord is one persisted data-export file (CSV/JSON under data/exports).
type ExportRecord struct {
	ID        int64  `json:"id"`
	Filename  string `json:"filename"`
	DeviceID  string `json:"device_id"`
	PointName string `json:"point_name"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Format    string `json:"format"`
	RowCount  int64  `json:"row_count"`
	SizeBytes int64  `json:"size_bytes"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func (d *Database) InsertExportRecord(r ExportRecord) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO export_records (filename, device_id, point_name, start_time, end_time, format, row_count, size_bytes, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Filename, r.DeviceID, r.PointName, r.StartTime, r.EndTime, r.Format, r.RowCount, r.SizeBytes, r.Status, r.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *Database) ListExportRecords(limit int) ([]ExportRecord, error) {
	rows, err := d.db.Query(
		`SELECT id, filename, device_id, point_name, start_time, end_time, format, row_count, size_bytes, status, created_at FROM export_records ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportRecord
	for rows.Next() {
		var r ExportRecord
		if err := rows.Scan(&r.ID, &r.Filename, &r.DeviceID, &r.PointName, &r.StartTime, &r.EndTime, &r.Format, &r.RowCount, &r.SizeBytes, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Database) GetExportRecord(id int64) (*ExportRecord, error) {
	var r ExportRecord
	err := d.db.QueryRow(
		`SELECT id, filename, device_id, point_name, start_time, end_time, format, row_count, size_bytes, status, created_at FROM export_records WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.Filename, &r.DeviceID, &r.PointName, &r.StartTime, &r.EndTime, &r.Format, &r.RowCount, &r.SizeBytes, &r.Status, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// PruneExportRecords keeps only the newest `keep` export records and returns
// the removed ones so the caller can delete their files.
func (d *Database) PruneExportRecords(keep int) ([]ExportRecord, error) {
	rows, err := d.db.Query(
		`SELECT id, filename FROM export_records WHERE id NOT IN (SELECT id FROM export_records ORDER BY id DESC LIMIT ?)`,
		keep,
	)
	if err != nil {
		return nil, err
	}
	var removed []ExportRecord
	type idFile struct {
		id       int64
		filename string
	}
	var pending []idFile
	for rows.Next() {
		var f idFile
		if err := rows.Scan(&f.id, &f.filename); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range pending {
		if _, err := d.db.Exec(`DELETE FROM export_records WHERE id = ?`, f.id); err != nil {
			return removed, err
		}
		removed = append(removed, ExportRecord{ID: f.id, Filename: f.filename})
	}
	return removed, nil
}

// BackupTo writes a transactionally consistent copy of the live database into
// dstPath using VACUUM INTO.
//
// A plain file copy of the .db is not a usable backup: the gateway keeps the
// database in WAL mode, so the most recent committed transactions still live in
// <db>-wal and never made it into the main file. Restoring such a "backup" rolls
// the gateway back by whatever the checkpoint had not absorbed yet — on a busy
// instance that is multiple megabytes of telemetry and alarms that the operator
// believes are saved. VACUUM INTO reads through the WAL and emits a single
// self-contained file, so no sidecar has to travel with the backup.
func (d *Database) BackupTo(ctx context.Context, dstPath string) (int64, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("database is not open")
	}
	if _, err := d.db.ExecContext(ctx, `VACUUM INTO ?`, dstPath); err != nil {
		return 0, fmt.Errorf("vacuum into %s failed: %w", dstPath, err)
	}
	info, err := os.Stat(dstPath)
	if err != nil {
		return 0, fmt.Errorf("backup %s was written but cannot be stat'ed: %w", dstPath, err)
	}
	return info.Size(), nil
}

// BackupSQLiteFile makes the same kind of consistent copy from a database that
// the caller only knows by path (the backup scheduler has no live handle).
func BackupSQLiteFile(srcPath, dstPath string) (int64, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", filepath.ToSlash(srcPath))
	src, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, fmt.Errorf("failed to open %s for backup: %w", srcPath, err)
	}
	defer src.Close()
	if _, err := src.Exec(`VACUUM INTO ?`, dstPath); err != nil {
		return 0, fmt.Errorf("vacuum into %s failed: %w", dstPath, err)
	}
	info, err := os.Stat(dstPath)
	if err != nil {
		return 0, fmt.Errorf("backup %s was written but cannot be stat'ed: %w", dstPath, err)
	}
	return info.Size(), nil
}

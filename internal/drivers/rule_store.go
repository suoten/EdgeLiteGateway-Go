package drivers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sirupsen/logrus"
)

// RuleStore provides SQLite-backed rule persistence with version management.
type RuleStore struct {
	mu     sync.Mutex
	dbPath string
	db     *sql.DB
}

// NewRuleStore creates a new RuleStore.
func NewRuleStore(dbPath string) *RuleStore {
	store := &RuleStore{
		dbPath: dbPath,
	}
	store.initDB()
	return store
}

func (s *RuleStore) initDB() {
	// PRAGMAs must ride in the DSN: db.Exec only sets them on one pooled
	// connection, while busy_timeout/WAL are per-connection settings.
	db, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		logrus.WithError(err).Error("Failed to open rule store database")
		return
	}
	// Configure connection pool
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(time.Hour)

	// Create tables
	db.Exec(`CREATE TABLE IF NOT EXISTS rules (
		rule_id TEXT PRIMARY KEY,
		snapshot TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS rule_versions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		rule_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		snapshot TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_rule_versions_rule ON rule_versions(rule_id, version)")

	s.db = db
}

// Close closes the database connection.
func (s *RuleStore) Close() {
	if s.db != nil {
		s.db.Close()
	}
}

// Ping checks database connectivity.
func (s *RuleStore) Ping() error {
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}
	return s.db.Ping()
}

// IsHealthy returns true if the database is reachable.
func (s *RuleStore) IsHealthy() bool {
	if s.db == nil {
		return false
	}
	return s.db.Ping() == nil
}

// SaveRule saves or updates a rule.
func (s *RuleStore) SaveRule(rule *EdgeRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}

	snapshot, err := json.Marshal(rule)
	if err != nil {
		return fmt.Errorf("marshal rule: %w", err)
	}

	updated_at := rule.RuleID // Use timestamp from rule
	_ = updated_at            // Suppress unused warning

	// Upsert rule
	_, err = s.db.Exec(
		`INSERT INTO rules (rule_id, snapshot, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(rule_id) DO UPDATE SET snapshot=excluded.snapshot, updated_at=excluded.updated_at`,
		rule.RuleID, string(snapshot), rule.RuleID,
	)
	if err != nil {
		return fmt.Errorf("save rule: %w", err)
	}

	// Save version
	var maxVersion int
	row := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM rule_versions WHERE rule_id = ?", rule.RuleID)
	if err := row.Scan(&maxVersion); err != nil {
		return fmt.Errorf("query max version: %w", err)
	}

	_, err = s.db.Exec(
		"INSERT INTO rule_versions (rule_id, version, snapshot, created_at) VALUES (?, ?, ?, ?)",
		rule.RuleID, maxVersion+1, string(snapshot), rule.RuleID,
	)
	return err
}

// DeleteRule deletes a rule by ID.
func (s *RuleStore) DeleteRule(ruleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := s.db.Exec("DELETE FROM rules WHERE rule_id = ?", ruleID)
	return err
}

// LoadRules loads all rules from the store.
func (s *RuleStore) LoadRules() []*EdgeRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query("SELECT snapshot FROM rules")
	if err != nil {
		logrus.WithError(err).Error("Failed to load rules")
		return nil
	}
	defer rows.Close()

	var rules []*EdgeRule
	for rows.Next() {
		var snapshot string
		if err := rows.Scan(&snapshot); err != nil {
			continue
		}
		var rule EdgeRule
		if err := json.Unmarshal([]byte(snapshot), &rule); err != nil {
			logrus.WithError(err).Warn("Failed to unmarshal rule snapshot")
			continue
		}
		rules = append(rules, &rule)
	}
	return rules
}

// GetRuleVersions returns version history for a rule.
func (s *RuleStore) GetRuleVersions(ruleID string) ([]map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	rows, err := s.db.Query(
		"SELECT version, snapshot, created_at FROM rule_versions WHERE rule_id = ? ORDER BY version DESC",
		ruleID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []map[string]interface{}
	for rows.Next() {
		var version int
		var snapshot, createdAt string
		if err := rows.Scan(&version, &snapshot, &createdAt); err != nil {
			continue
		}
		versions = append(versions, map[string]interface{}{
			"version":    version,
			"snapshot":  snapshot,
			"created_at": createdAt,
		})
	}
	return versions, nil
}

// RollbackRule rolls back a rule to a specific version.
func (s *RuleStore) RollbackRule(ruleID string, version int) (*EdgeRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	var snapshot string
	err := s.db.QueryRow(
		"SELECT snapshot FROM rule_versions WHERE rule_id = ? AND version = ?",
		ruleID, version,
	).Scan(&snapshot)
	if err != nil {
		return nil, fmt.Errorf("query version: %w", err)
	}

	var rule EdgeRule
	if err := json.Unmarshal([]byte(snapshot), &rule); err != nil {
		return nil, fmt.Errorf("unmarshal rule: %w", err)
	}

	// Update the current rule with the rolled-back version
	_, err = s.db.Exec(
		"UPDATE rules SET snapshot = ?, updated_at = ? WHERE rule_id = ?",
		snapshot, ruleID, ruleID,
	)
	if err != nil {
		return nil, fmt.Errorf("update rule: %w", err)
	}

	return &rule, nil
}

package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"edgelite/internal/config"
	"edgelite/internal/models"
)

func newTestConfig(t *testing.T) *config.AppConfig {
	tmpDir := t.TempDir()
	return &config.AppConfig{
		Database: config.DatabaseConfig{
			Backend:     "sqlite",
			SQLitePath:  filepath.Join(tmpDir, "test.db"),
			PoolSize:    5,
			MaxOverflow: 10,
			BackupDir:   filepath.Join(tmpDir, "backups"),
		},
	}
}

func TestNewDatabase(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if db.Path() == "" {
		t.Fatal("Database path is empty")
	}

	if !db.IsHealthy() {
		t.Fatal("Database should be healthy after creation")
	}
}

func TestDatabaseHealthCheck(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if err := db.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	if !db.IsHealthy() {
		t.Fatal("Database should be healthy after HealthCheck")
	}
}

func TestDatabaseInitTables(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Tables should be created by NewDatabase
	tables := []string{"devices", "rules", "alarms", "users", "sessions"}
	for _, table := range tables {
		var count int
		err := db.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		if err != nil {
			t.Fatalf("Failed to check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("Table %s was not created", table)
		}
	}
}

func TestDatabaseGetAuditDBPath(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if db.GetAuditDBPath() == "" {
		t.Fatal("Audit DB path is empty")
	}
}

func TestDatabaseClose(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if db.IsHealthy() {
		t.Fatal("Database should not be healthy after Close")
	}
}

// --- DeviceRepo Tests ---

func TestDeviceRepoCRUD(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewDeviceRepo(db)

	// Create
	device := &models.DeviceResponse{
		DeviceID:        "test-device-1",
		Name:            "Test Device",
		Protocol:        "modbus",
		Status:          "unknown",
		Config:          map[string]interface{}{"host": "127.0.0.1"},
		Points:          []models.PointDef{},
		CollectInterval: 5,
	}
	err = repo.Create(device, "admin")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Get
	got, err := repo.Get("test-device-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("Device not found after Create")
	}
	if got.Name != "Test Device" {
		t.Fatalf("Expected name 'Test Device', got '%s'", got.Name)
	}

	// List
	devices, total, err := repo.List(1, 10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("Expected total 1, got %d", total)
	}
	if len(devices) != 1 {
		t.Fatalf("Expected 1 device, got %d", len(devices))
	}

	// Delete
	err = repo.Delete("test-device-1")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deleted
	got, err = repo.Get("test-device-1")
	if err != nil {
		t.Fatalf("Get after Delete failed: %v", err)
	}
	if got != nil {
		t.Fatal("Device should be nil after Delete")
	}
}

func TestDeviceRepoUpdateStatus(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewDeviceRepo(db)

	device := &models.DeviceResponse{
		DeviceID:        "test-device-2",
		Name:            "Test Device 2",
		Protocol:        "modbus",
		Status:          "unknown",
		CollectInterval: 5,
		Points:          []models.PointDef{},
	}
	_ = repo.Create(device, "admin")

	err = repo.UpdateStatus("test-device-2", "online")
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	got, _ := repo.Get("test-device-2")
	if got.Status != "online" {
		t.Fatalf("Expected status 'online', got '%s'", got.Status)
	}
}

// --- RuleRepo Tests ---

func TestRuleRepoCRUD(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewRuleRepo(db)

	// Create
	rule := &models.RuleResponse{
		RuleID:         "test-rule-1",
		Name:           "Test Rule",
		Severity:       "warning",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	err = repo.Create(rule, "admin")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Get
	got, err := repo.Get("test-rule-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("Rule not found after Create")
	}
	if got.Name != "Test Rule" {
		t.Fatalf("Expected name 'Test Rule', got '%s'", got.Name)
	}

	// Delete
	err = repo.Delete("test-rule-1")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestRuleRepoSetEnabled(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewRuleRepo(db)

	rule := &models.RuleResponse{
		RuleID:         "test-rule-2",
		Name:           "Test Rule 2",
		Severity:       "critical",
		Enabled:        true,
		NotifyChannels: []string{"dingtalk"},
	}
	_ = repo.Create(rule, "admin")

	err = repo.SetEnabled("test-rule-2", false)
	if err != nil {
		t.Fatalf("SetEnabled failed: %v", err)
	}

	got, _ := repo.Get("test-rule-2")
	if got.Enabled {
		t.Fatal("Rule should be disabled after SetEnabled(false)")
	}
}

// --- UserRepo Tests ---

func TestUserRepoCRUD(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewUserRepo(db)

	// Create
	err = repo.Create("test-user-1", "testuser", "hashedpassword", "admin")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify by username
	got, err := repo.GetByUsername("testuser")
	if err != nil {
		t.Fatalf("GetByUsername failed: %v", err)
	}
	if got == nil {
		t.Fatal("User not found")
	}
	if got.Username != "testuser" {
		t.Fatalf("Expected username 'testuser', got '%s'", got.Username)
	}
}

// --- Ensure file exists ---
func TestDatabaseFileExists(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(cfg.Database.SQLitePath); os.IsNotExist(err) {
		t.Fatal("Database file was not created")
	}
}

// --- Connection pool settings ---
func TestDatabaseConnectionPool(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Database.PoolSize = 3
	cfg.Database.MaxOverflow = 7

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Verify connection pool settings
	stats := db.DB().Stats()
	if stats.MaxOpenConnections != 10 {
		t.Fatalf("Expected MaxOpenConnections=10, got %d", stats.MaxOpenConnections)
	}
}

// --- Concurrent access test ---
func TestDatabaseConcurrentAccess(t *testing.T) {
	cfg := newTestConfig(t)

	db, err := NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// SQLite with WAL mode supports concurrent reads but serializes writes.
	// Use a transaction to batch insert all devices to avoid SQLITE_BUSY.
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatalf("Failed to begin transaction: %v", err)
	}

	for i := 0; i < 10; i++ {
		_, err := tx.Exec("INSERT INTO devices (device_id, name, protocol, status, config, points, collect_interval, created_at, updated_at, version) VALUES (?, ?, ?, ?, '{}', '[]', 5, ?, ?, 1)",
			"concurrent-device-"+strconv.Itoa(i),
			"Concurrent Device",
			"modbus",
			"unknown",
			time.Now().Format(time.RFC3339),
			time.Now().Format(time.RFC3339),
		)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("Concurrent write %d failed: %v", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Transaction commit failed: %v", err)
	}

	// Verify all 10 devices were inserted
	var count int
	err = db.DB().QueryRow("SELECT COUNT(*) FROM devices WHERE device_id LIKE 'concurrent-device-%'").Scan(&count)
	if err != nil {
		t.Fatalf("Count query failed: %v", err)
	}
	if count != 10 {
		t.Fatalf("Expected 10 concurrent devices, got %d", count)
	}
}

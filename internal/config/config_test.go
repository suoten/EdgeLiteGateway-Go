package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultServerConfig(t *testing.T) {
	cfg := defaultServerConfig()
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Expected default host '127.0.0.1', got '%s'", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Fatalf("Expected default port 8080, got %d", cfg.Port)
	}
}

func TestDefaultDatabaseConfig(t *testing.T) {
	cfg := defaultDatabaseConfig()
	if cfg.Backend != "sqlite" {
		t.Fatalf("Expected backend 'sqlite', got '%s'", cfg.Backend)
	}
	if cfg.SQLitePath != "data/edgelite.db" {
		t.Fatalf("Expected sqlite_path 'data/edgelite.db', got '%s'", cfg.SQLitePath)
	}
	if cfg.PoolSize != 5 {
		t.Fatalf("Expected pool_size 5, got %d", cfg.PoolSize)
	}
}

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Include security.secret_key to pass validation
	yamlContent := `
server:
  host: 0.0.0.0
  port: 9090
database:
  backend: sqlite
  sqlite_path: data/test.db
  pool_size: 10
logging:
  level: debug
  json_format: true
security:
  secret_key: test-secret-key-for-unit-testing-only-1234567890
  csrf_secret: test-csrf-secret-for-unit-testing-only-1234567890
`

	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	// Set EDGELITE_CONFIG to our test path so LoadConfig("") finds it
	os.Setenv("EDGELITE_CONFIG", cfgPath)
	defer os.Unsetenv("EDGELITE_CONFIG")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("Expected host '0.0.0.0', got '%s'", cfg.Server.Host)
	}
	if cfg.Server.Port != 9090 {
		t.Fatalf("Expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Database.PoolSize != 10 {
		t.Fatalf("Expected pool_size 10, got %d", cfg.Database.PoolSize)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("Expected logging level 'debug', got '%s'", cfg.Logging.Level)
	}
	if !cfg.Logging.JSONFormat {
		t.Fatal("Expected json_format to be true")
	}
}

func TestLoadConfigNonExistent(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("LoadConfig should fail for non-existent file")
	}
}

func TestSetGlobalConfig(t *testing.T) {
	cfg := &AppConfig{
		Server:   defaultServerConfig(),
		Database: defaultDatabaseConfig(),
	}
	SetGlobalConfig(cfg)

	got := GetConfig()
	if got == nil {
		t.Fatal("GetConfig returned nil after SetGlobalConfig")
	}
	if got.Server.Host != cfg.Server.Host {
		t.Fatal("Global config mismatch")
	}
}

func TestGetConfigNil(t *testing.T) {
	// Reset global config
	ResetConfig()
	// GetConfig is lazy-init and will create a default config if nil,
	// so it should never return nil
	cfg := GetConfig()
	if cfg == nil {
		t.Fatal("GetConfig should never return nil (lazy-init with defaults)")
	}
	// Clean up
	ResetConfig()
}

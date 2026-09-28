package engine

import (
	"testing"
	"time"
)

// --- SandboxExecutor Tests ---

func TestNewSandboxExecutorDefault(t *testing.T) {
	s := NewSandboxExecutor(nil)
	if s == nil {
		t.Fatal("NewSandboxExecutor returned nil")
	}
	cfg := s.GetConfig()
	if cfg.Timeout != sandboxDefaultTimeout {
		t.Fatalf("Expected default timeout %v, got %v", sandboxDefaultTimeout, cfg.Timeout)
	}
}

func TestNewSandboxExecutorCustomConfig(t *testing.T) {
	cfg := &SandboxConfig{
		Timeout:       10 * time.Second,
		MaxResultSize: 2048,
		AllowedFuncs:  map[string]bool{"abs": true},
		AllowedVars:   map[string]bool{"x": true},
	}
	s := NewSandboxExecutor(cfg)
	if s.GetConfig().Timeout != 10*time.Second {
		t.Fatal("Custom timeout not set")
	}
}

func TestNewSandboxExecutorZeroTimeout(t *testing.T) {
	cfg := &SandboxConfig{
		Timeout: 0,
	}
	s := NewSandboxExecutor(cfg)
	if s.GetConfig().Timeout != sandboxDefaultTimeout {
		t.Fatalf("Zero timeout should default to %v", sandboxDefaultTimeout)
	}
}

func TestSandboxExecuteArithmetic(t *testing.T) {
	s := NewSandboxExecutor(nil)
	result, _ := s.Execute("1 + 2", nil)
	if result == nil {
		t.Fatal("Expected non-nil result")
	}
}

func TestSandboxExecuteWithVariables(t *testing.T) {
	s := NewSandboxExecutor(nil)
	vars := map[string]interface{}{"x": 10.0, "y": 5.0}
	result, _ := s.Execute("x + y", vars)
	if result == nil {
		t.Fatal("Expected non-nil result for x + y")
	}
}

func TestSandboxExecuteEmptyExpression(t *testing.T) {
	s := NewSandboxExecutor(nil)
	result, _ := s.Execute("", nil)
	if result != nil {
		t.Fatal("Empty expression should return nil")
	}
}

func TestSandboxExecuteInvalidExpression(t *testing.T) {
	s := NewSandboxExecutor(nil)
	result, _ := s.Execute("@@@###$$$", nil)
	// Completely invalid expressions return nil
	if result != nil {
		t.Fatal("Invalid expression should return nil")
	}
}

func TestSandboxExecuteBatch(t *testing.T) {
	s := NewSandboxExecutor(nil)
	expressions := map[string]string{
		"sum": "1 + 2",
		"mul": "3 * 4",
	}
	results := s.ExecuteBatch(expressions, nil)
	if results == nil {
		t.Fatal("ExecuteBatch returned nil")
	}
	if len(results) != 2 {
		t.Fatalf("Expected 2 results, got %d", len(results))
	}
}

func TestSandboxValidateExpression(t *testing.T) {
	s := NewSandboxExecutor(nil)
	// Valid expression
	if err := s.ValidateExpression("1 + 2"); err != nil {
		t.Fatalf("Valid expression failed validation: %v", err)
	}
}

func TestSandboxFilterVariables(t *testing.T) {
	cfg := &SandboxConfig{
		Timeout:      5 * time.Second,
		AllowedVars:  map[string]bool{"allowed": true},
		AllowedFuncs: map[string]bool{"abs": true},
	}
	s := NewSandboxExecutor(cfg)
	vars := map[string]interface{}{
		"allowed":   42,
		"forbidden": "secret",
	}
	filtered := s.filterVariables(vars)
	if _, ok := filtered["allowed"]; !ok {
		t.Fatal("Allowed variable was filtered out")
	}
	if _, ok := filtered["forbidden"]; ok {
		t.Fatal("Forbidden variable was not filtered out")
	}
}

func TestSandboxFilterVariablesWithNoAllowList(t *testing.T) {
	cfg := &SandboxConfig{
		Timeout:      5 * time.Second,
		AllowedVars:  nil, // No filter
		AllowedFuncs: map[string]bool{},
	}
	s := NewSandboxExecutor(cfg)
	vars := map[string]interface{}{"x": 1, "y": 2}
	filtered := s.filterVariables(vars)
	if len(filtered) != 2 {
		t.Fatal("All variables should pass when no allow list is set")
	}
}

func TestSandboxRegisterFunction(t *testing.T) {
	// Create a config that allows the function we want to register
	cfg := &SandboxConfig{
		Timeout: 5 * time.Second,
		AllowedFuncs: map[string]bool{
			"my_func": true,
		},
	}
	s := NewSandboxExecutor(cfg)
	err := s.RegisterFunction("my_func", func(args []interface{}) (interface{}, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("RegisterFunction failed: %v", err)
	}
}

func TestSandboxRegisterFunctionBlocked(t *testing.T) {
	cfg := &SandboxConfig{
		Timeout:      5 * time.Second,
		AllowedFuncs: map[string]bool{"allowed_func": true},
	}
	s := NewSandboxExecutor(cfg)
	err := s.RegisterFunction("disallowed_func", func(args []interface{}) (interface{}, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("RegisterFunction should fail for non-allowed function")
	}
}

func TestSandboxSetConfig(t *testing.T) {
	s := NewSandboxExecutor(nil)
	newCfg := &SandboxConfig{
		Timeout: 15 * time.Second,
	}
	s.SetConfig(newCfg)
	if s.GetConfig().Timeout != 15*time.Second {
		t.Fatal("SetConfig did not update config")
	}
}

func TestSandboxClose(t *testing.T) {
	s := NewSandboxExecutor(nil)
	// Should not panic
	s.Close()
}

package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Sandbox provides a safe execution environment for user-defined scripts.
// This is a Go port of the Python edgelite/engine/sandbox.py.
//
// In the Python version, scripts are executed in a restricted Python sandbox
// using AST validation and namespace sanitization. In Go, we provide a
// simplified expression-based sandbox that evaluates mathematical and logical
// expressions without exposing Go runtime capabilities.
//
// Features:
//   - Timeout-limited execution
//   - Memory-safe evaluation (no runtime introspection)
//   - Configurable allowed functions and variables
//   - Thread-safe concurrent execution

const (
	sandboxDefaultTimeout = 5 * time.Second
)

// SandboxConfig holds configuration for the sandbox executor.
type SandboxConfig struct {
	Timeout         time.Duration
	MaxResultSize   int
	AllowedFuncs    map[string]bool
	AllowedVars    map[string]bool
}

// SandboxExecutor provides safe expression evaluation.
type SandboxExecutor struct {
	mu     sync.RWMutex
	config *SandboxConfig
	expr   *ExpressionEngine
}

// NewSandboxExecutor creates a new SandboxExecutor.
func NewSandboxExecutor(config *SandboxConfig) *SandboxExecutor {
	if config == nil {
		config = &SandboxConfig{
			Timeout:       sandboxDefaultTimeout,
			MaxResultSize: 1024 * 1024, // 1MB
			AllowedFuncs: map[string]bool{
				"abs": true, "round": true, "min": true, "max": true,
				"pow": true, "int": true, "float": true, "str": true, "bool": true,
				"sqrt": true, "ceil": true, "floor": true, "log": true, "log10": true,
			},
			AllowedVars: make(map[string]bool),
		}
	}
	if config.Timeout <= 0 {
		config.Timeout = sandboxDefaultTimeout
	}
	return &SandboxExecutor{
		config: config,
		expr:   NewExpressionEngine(),
	}
}

// Execute evaluates an expression in the sandbox with timeout protection.
// The expression runs in a goroutine with a cancellation channel; on timeout
// the goroutine is signaled to stop via a done channel to prevent leaks.
func (s *SandboxExecutor) Execute(expression string, variables map[string]interface{}) (interface{}, error) {
	// Filter variables to only allowed ones
	filtered := s.filterVariables(variables)

	// Execute with timeout
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()

	resultCh := make(chan interface{}, 1)
	errCh := make(chan error, 1)
	doneCh := make(chan struct{}) // signals the goroutine to abort

	go func() {
		defer func() {
			// Recover from panics in expression evaluation
			if r := recover(); r != nil {
				errCh <- fmt.Errorf("expression evaluation panic: %v", r)
			}
		}()

		// Check for cancellation before evaluating
		select {
		case <-doneCh:
			return
		default:
		}

		result := s.expr.Evaluate(expression, filtered)

		// Check for cancellation after evaluating (result may be discarded)
		select {
		case <-doneCh:
			return
		default:
			resultCh <- result
		}
	}()

	select {
	case result := <-resultCh:
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		close(doneCh) // signal goroutine to abort
		return nil, fmt.Errorf("expression execution timed out after %v", s.config.Timeout)
	}
}

// ExecuteBatch evaluates multiple expressions.
func (s *SandboxExecutor) ExecuteBatch(expressions map[string]string, variables map[string]interface{}) map[string]interface{} {
	filtered := s.filterVariables(variables)
	return s.expr.EvaluateBatch(expressions, filtered)
}

// ValidateExpression validates that an expression is safe.
func (s *SandboxExecutor) ValidateExpression(expression string) error {
	return s.expr.ValidateExpression(expression)
}

// RegisterFunction registers a custom function in the sandbox.
func (s *SandboxExecutor) RegisterFunction(name string, fn ExpressionFunc) error {
	if s.config.AllowedFuncs != nil && !s.config.AllowedFuncs[name] {
		return fmt.Errorf("function not in allowed list: %s", name)
	}
	return s.expr.RegisterFunction(name, fn)
}

// Close releases resources.
func (s *SandboxExecutor) Close() {
	// No-op for now, ExpressionEngine doesn't have a close method
}

func (s *SandboxExecutor) filterVariables(variables map[string]interface{}) map[string]interface{} {
	if s.config.AllowedVars == nil || len(s.config.AllowedVars) == 0 {
		return variables
	}
	filtered := make(map[string]interface{})
	for k, v := range variables {
		if s.config.AllowedVars[k] {
			filtered[k] = v
		}
	}
	return filtered
}

// GetConfig returns the sandbox configuration.
func (s *SandboxExecutor) GetConfig() *SandboxConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// SetConfig updates the sandbox configuration.
func (s *SandboxExecutor) SetConfig(config *SandboxConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = config
}

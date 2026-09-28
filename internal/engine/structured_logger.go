package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// LogLevel represents the severity level of a log message.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelFatal
)

// String returns the string representation of a log level.
func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	case LogLevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// StructuredLogger provides structured logging with context support.
// It supports JSON-formatted log output with optional context fields.
type StructuredLogger struct {
	mu             sync.Mutex
	output         *os.File
	level          LogLevel
	includeContext bool
	includeTraceback bool
	context        map[string]interface{}
}

// NewStructuredLogger creates a new StructuredLogger.
func NewStructuredLogger(output *os.File, level LogLevel, includeContext, includeTraceback bool) *StructuredLogger {
	if output == nil {
		output = os.Stdout
	}
	return &StructuredLogger{
		output:           output,
		level:            level,
		includeContext:  includeContext,
		includeTraceback: includeTraceback,
		context:          make(map[string]interface{}),
	}
}

// Setup initializes the logger.
func (l *StructuredLogger) Setup() error {
	return nil
}

// SetContext sets context fields that will be included in every log entry.
func (l *StructuredLogger) SetContext(fields map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.context = make(map[string]interface{}, len(fields))
	for k, v := range fields {
		l.context[k] = v
	}
}

// ClearContext clears all context fields.
func (l *StructuredLogger) ClearContext() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.context = make(map[string]interface{})
}

// LogWithData logs a message with additional data fields.
func (l *StructuredLogger) LogWithData(level LogLevel, msg string, data map[string]interface{}) {
	if level < l.level {
		return
	}

	entry := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"level":     level.String(),
		"message":    msg,
	}

	// Add context fields
	if l.includeContext {
		l.mu.Lock()
		for k, v := range l.context {
			entry[k] = v
		}
		l.mu.Unlock()
	}

	// Add data fields
	for k, v := range data {
		entry[k] = v
	}

	// Add caller info
	if pc, file, line, ok := runtime.Caller(2); ok {
		fn := runtime.FuncForPC(pc)
		if fn != nil {
			entry["func"] = fn.Name()
		}
		entry["file"] = filepath.Base(file)
		entry["line"] = line
	}

	// Add traceback for errors
	if l.includeTraceback && level >= LogLevelError {
		entry["traceback"] = getStackTrace()
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(entry)
	if err != nil {
		// Fallback to simple format
		fmt.Fprintf(l.output, "[%s] %s: %s\n", level.String(), time.Now().Format(time.RFC3339), msg)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.output, string(jsonData))
}

// Debug logs a debug message.
func (l *StructuredLogger) Debug(msg string, data ...map[string]interface{}) {
	var d map[string]interface{}
	if len(data) > 0 {
		d = data[0]
	}
	l.LogWithData(LogLevelDebug, msg, d)
}

// Info logs an info message.
func (l *StructuredLogger) Info(msg string, data ...map[string]interface{}) {
	var d map[string]interface{}
	if len(data) > 0 {
		d = data[0]
	}
	l.LogWithData(LogLevelInfo, msg, d)
}

// Warn logs a warning message.
func (l *StructuredLogger) Warn(msg string, data ...map[string]interface{}) {
	var d map[string]interface{}
	if len(data) > 0 {
		d = data[0]
	}
	l.LogWithData(LogLevelWarn, msg, d)
}

// Error logs an error message.
func (l *StructuredLogger) Error(msg string, data ...map[string]interface{}) {
	var d map[string]interface{}
	if len(data) > 0 {
		d = data[0]
	}
	l.LogWithData(LogLevelError, msg, d)
}

// Fatal logs a fatal message and exits.
func (l *StructuredLogger) Fatal(msg string, data ...map[string]interface{}) {
	var d map[string]interface{}
	if len(data) > 0 {
		d = data[0]
	}
	l.LogWithData(LogLevelFatal, msg, d)
	os.Exit(1)
}

// getStackTrace returns a stack trace as a string.
func getStackTrace() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// ParseLogLevel parses a string to a LogLevel.
func ParseLogLevel(s string) LogLevel {
	switch strings.ToUpper(s) {
	case "DEBUG":
		return LogLevelDebug
	case "INFO":
		return LogLevelInfo
	case "WARN", "WARNING":
		return LogLevelWarn
	case "ERROR":
		return LogLevelError
	case "FATAL":
		return LogLevelFatal
	default:
		return LogLevelInfo
	}
}

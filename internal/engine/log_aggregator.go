package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
)

// LogAggregator collects and aggregates logs from multiple sources.
// This is a Go port of the Python edgelite/engine/log_aggregator.py.
//
// Features:
//   - In-memory ring buffer for recent logs
//   - File-based log rotation
//   - Structured JSON log entries
//   - Level filtering and search

const (
	logAggBufferSize    = 10000
	logAggFlushInterval = 5 * time.Second
)

// aggregatorDiag reports faults from the flush path. The standard logrus
// logger feeds this aggregator through LogrusHook, so a message logged from
// flush would be ingested and written again on the next tick, keeping the log
// file busy forever.
var aggregatorDiag = func() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(os.Stderr)
	return l
}()

// LogEntry represents a single log entry.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Source    string    `json:"source,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	DeviceID  string    `json:"device_id,omitempty"`
}

// LogAggregator aggregates logs from multiple sources.
type LogAggregator struct {
	mu         sync.RWMutex
	buffer     []LogEntry
	bufferPos  int
	bufferFull bool
	// ingested counts every entry ever added, flushed counts those already
	// written to the file, so a flush only appends what is new instead of
	// re-emitting the whole ring.
	ingested    uint64
	flushed     uint64
	logFile     *os.File
	logPath     string
	logDir      string
	maxBytes    int64
	backupCount int
	fileSize    int64
	rotations   uint64
	skipped     uint64
	writeErrs   uint64
	started     bool
	cancelFunc  context.CancelFunc
}

// NewLogAggregator creates a new LogAggregator.
func NewLogAggregator(logDir string) *LogAggregator {
	return &LogAggregator{
		buffer: make([]LogEntry, logAggBufferSize),
		logDir: logDir,
	}
}

// Start begins the log aggregator.
func (la *LogAggregator) Start(ctx context.Context) error {
	la.mu.Lock()
	if la.started {
		la.mu.Unlock()
		return nil
	}
	la.started = true
	la.mu.Unlock()

	// Open log file
	if la.logDir != "" {
		if err := os.MkdirAll(la.logDir, 0755); err != nil {
			logrus.WithError(err).Warn("Failed to create log directory")
		} else {
			logPath := filepath.Join(la.logDir, "edgelite_aggregated.log")
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				logrus.WithError(err).Warn("Failed to open aggregated log file")
			} else {
				la.logFile = f
				la.logPath = logPath
				if st, err := f.Stat(); err == nil {
					la.fileSize = st.Size()
				}
			}
		}
	}

	if lc := config.GetConfig().Logging; lc.MaxBytes > 0 {
		la.maxBytes = int64(lc.MaxBytes)
		la.backupCount = lc.BackupCount
	}

	childCtx, cancel := context.WithCancel(ctx)
	la.cancelFunc = cancel

	go la.flushLoop(childCtx)
	logrus.Info("LogAggregator started")
	return nil
}

// Stop stops the log aggregator.
func (la *LogAggregator) Stop() {
	la.mu.Lock()
	la.started = false
	la.mu.Unlock()

	if la.cancelFunc != nil {
		la.cancelFunc()
		la.cancelFunc = nil
	}

	la.mu.Lock()
	if la.logFile != nil {
		la.logFile.Close()
		la.logFile = nil
	}
	la.mu.Unlock()

	logrus.Info("LogAggregator stopped")
}

// Ingest adds a log entry to the aggregator.
func (la *LogAggregator) Ingest(entry LogEntry) {
	la.mu.Lock()
	defer la.mu.Unlock()

	la.buffer[la.bufferPos] = entry
	la.bufferPos = (la.bufferPos + 1) % logAggBufferSize
	if la.bufferPos == 0 {
		la.bufferFull = true
	}
	la.ingested++
}

// Query returns log entries matching the given filters.
func (la *LogAggregator) Query(level string, source string, since time.Time, limit int) []LogEntry {
	la.mu.RLock()
	defer la.mu.RUnlock()

	var entries []LogEntry
	if la.bufferFull {
		// Read entire buffer
		for i := 0; i < logAggBufferSize; i++ {
			idx := (la.bufferPos + i) % logAggBufferSize
			entry := la.buffer[idx]
			if la.matchesFilter(entry, level, source, since) {
				entries = append(entries, entry)
				if limit > 0 && len(entries) >= limit {
					break
				}
			}
		}
	} else {
		for i := 0; i < la.bufferPos; i++ {
			entry := la.buffer[i]
			if la.matchesFilter(entry, level, source, since) {
				entries = append(entries, entry)
				if limit > 0 && len(entries) >= limit {
					break
				}
			}
		}
	}
	return entries
}

func (la *LogAggregator) matchesFilter(entry LogEntry, level, source string, since time.Time) bool {
	// Levels arrive as "warn"/"warning" from the UI while the buffer stores the
	// canonical label, so compare normalised forms.
	if level != "" && NormalizeLogLevel(entry.Level) != NormalizeLogLevel(level) {
		return false
	}
	if source != "" && entry.Source != source {
		return false
	}
	if !since.IsZero() && entry.Timestamp.Before(since) {
		return false
	}
	return true
}

func (la *LogAggregator) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(logAggFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			la.flush()
		}
	}
}

// flush appends the entries ingested since the last flush to the log file,
// rotating it once it grows past logging.max_bytes.
func (la *LogAggregator) flush() {
	la.mu.Lock()
	defer la.mu.Unlock()

	if la.logFile == nil {
		return
	}

	// The ring only keeps the newest logAggBufferSize entries; if it wrapped
	// before the previous flush the older ones are gone, so advance the cursor
	// past them instead of re-reading overwritten slots.
	if pending := la.ingested - la.flushed; pending > logAggBufferSize {
		skipped := pending - logAggBufferSize
		la.flushed += skipped
		la.skipped += skipped
		aggregatorDiag.WithField("skipped", skipped).
			Warn("Aggregated log file fell behind the ring buffer, older entries were not written")
	}

	for s := la.flushed + 1; s <= la.ingested; s++ {
		entry := la.buffer[(s-1)%logAggBufferSize]
		if entry.Timestamp.IsZero() {
			continue
		}
		// Without the ids several devices logging the same message in the same
		// second collapse into byte-identical lines.
		line := fmt.Sprintf("[%s] %s %s source=%s device_id=%s request_id=%s",
			entry.Timestamp.Format(time.RFC3339),
			entry.Level,
			entry.Message,
			entry.Source,
			entry.DeviceID,
			entry.RequestID,
		)
		if err := la.writeLine(line); err != nil {
			la.writeErrs++
			aggregatorDiag.WithError(err).Warn("Failed to write aggregated log")
			la.flushed = la.ingested
			return
		}
	}
	la.flushed = la.ingested
}

// writeLine appends one line, rotating first when the file would exceed the
// configured size limit.
func (la *LogAggregator) writeLine(line string) error {
	if la.maxBytes > 0 && la.fileSize+int64(len(line)+1) > la.maxBytes {
		if err := la.rotate(); err != nil {
			return err
		}
	}
	n, err := la.logFile.WriteString(line + "\n")
	la.fileSize += int64(n)
	return err
}

// rotate closes the log file and shifts it, plus up to backupCount-1 earlier
// backups, one slot forward: edgelite_aggregated.log.N is the oldest.
func (la *LogAggregator) rotate() error {
	if err := la.logFile.Close(); err != nil {
		aggregatorDiag.WithError(err).Warn("Failed to close aggregated log file during rotation")
	}
	la.logFile = nil
	la.rotations++

	if la.backupCount > 0 {
		os.Remove(fmt.Sprintf("%s.%d", la.logPath, la.backupCount))
		for i := la.backupCount - 1; i >= 1; i-- {
			if err := os.Rename(fmt.Sprintf("%s.%d", la.logPath, i), fmt.Sprintf("%s.%d", la.logPath, i+1)); err != nil && !os.IsNotExist(err) {
				aggregatorDiag.WithError(err).Warn("Failed to shift aggregated log backup")
			}
		}
		if err := os.Rename(la.logPath, la.logPath+".1"); err == nil {
			aggregatorDiag.WithField("path", la.logPath).WithField("size", la.fileSize).
				Info("Aggregated log rotated")
		} else {
			// A reader (tail, editor) can keep a file unrenamable on Windows;
			// truncating is the only way to honour the size limit then.
			aggregatorDiag.WithError(err).Warn("Failed to rename aggregated log, truncating instead")
			if err := os.Truncate(la.logPath, 0); err != nil {
				aggregatorDiag.WithError(err).Warn("Failed to truncate aggregated log")
			}
		}
	} else if err := os.Truncate(la.logPath, 0); err != nil {
		aggregatorDiag.WithError(err).Warn("Failed to truncate aggregated log")
	}

	f, err := os.OpenFile(la.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		la.fileSize = 0
		return err
	}
	la.logFile = f
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	} else {
		aggregatorDiag.WithError(err).Warn("Failed to stat aggregated log after rotation")
	}
	la.fileSize = size
	return nil
}

// GetStats returns aggregator statistics.
func (la *LogAggregator) GetStats() map[string]interface{} {
	la.mu.RLock()
	defer la.mu.RUnlock()
	count := la.bufferPos
	if la.bufferFull {
		count = logAggBufferSize
	}
	return map[string]interface{}{
		"buffer_size":     count,
		"buffer_capacity": logAggBufferSize,
		"file_enabled":    la.logFile != nil,
		"file_path":       la.logPath,
		"file_size":       la.fileSize,
		"max_bytes":       la.maxBytes,
		"backup_count":    la.backupCount,
		"written":         la.flushed,
		"rotations":       la.rotations,
		"skipped":         la.skipped,
		"write_errors":    la.writeErrs,
	}
}

// NormalizeLogLevel maps logrus level names to the canonical uppercase
// labels used by the log aggregation UI.
func NormalizeLogLevel(level string) string {
	switch strings.ToUpper(level) {
	case "WARNING":
		return "WARN"
	case "TRACE":
		return "DEBUG"
	default:
		return strings.ToUpper(level)
	}
}

// LogrusHook feeds standard logrus entries into a LogAggregator so the
// in-memory buffer mirrors what the gateway logs at runtime.
type LogrusHook struct {
	aggregator *LogAggregator
}

// NewLogrusHook creates a logrus hook that ingests entries into the aggregator.
func NewLogrusHook(aggregator *LogAggregator) *LogrusHook {
	return &LogrusHook{aggregator: aggregator}
}

// Levels returns all log levels.
func (h *LogrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire ingests the logrus entry into the aggregator buffer.
func (h *LogrusHook) Fire(entry *logrus.Entry) error {
	if h.aggregator == nil {
		return nil
	}
	source := dataString(entry, "component")
	if source == "" {
		source = dataString(entry, "module")
	}
	if source == "" {
		source = dataString(entry, "service")
	}
	if source == "" {
		source = "gateway"
	}
	requestID := dataString(entry, "req_id")
	if requestID == "" {
		requestID = dataString(entry, "request_id")
	}
	if requestID == "" {
		requestID = dataString(entry, "trace_id")
	}
	h.aggregator.Ingest(LogEntry{
		Timestamp: entry.Time,
		Level:     NormalizeLogLevel(entry.Level.String()),
		Message:   logMessageWithFields(entry),
		Source:    source,
		RequestID: requestID,
		DeviceID:  dataString(entry, "device_id"),
	})
	return nil
}

// logFieldsWithColumns are the entry.Data keys Fire stores in their own LogEntry
// column; the rest have to travel inside the message.
var logFieldsWithColumns = map[string]bool{
	"component":  true,
	"module":     true,
	"service":    true,
	"req_id":     true,
	"request_id": true,
	"trace_id":   true,
	"device_id":  true,
}

// logMessageWithFields appends the structured fields the logger was given to the
// message. Fire used to keep only the handful of keys it has columns for, so a
// middleware line that recorded method, path and status reached the log page as
// the bare string "Request error" -- hundreds of identical rows that answer no
// question, and a search box that cannot find the endpoint that failed.
func logMessageWithFields(entry *logrus.Entry) string {
	keys := make([]string, 0, len(entry.Data))
	for k := range entry.Data {
		if !logFieldsWithColumns[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return entry.Message
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(entry.Message)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		v := fmt.Sprintf("%v", entry.Data[k])
		if strings.ContainsAny(v, ` 	"`) {
			b.WriteString(strconv.Quote(v))
		} else {
			b.WriteString(v)
		}
	}
	return b.String()
}

func dataString(entry *logrus.Entry, key string) string {
	v, _ := entry.Data[key].(string)
	return v
}

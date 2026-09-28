package engine

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
)

// readLogLines returns the lines of the aggregated log file in dir.
func readLogLines(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "edgelite_aggregated.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open aggregated log: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan aggregated log: %v", err)
	}
	return lines
}

func uniqueCount(lines []string) int {
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l] = true
	}
	return len(seen)
}

func startAggregator(t *testing.T, dir string) *LogAggregator {
	t.Helper()
	la := NewLogAggregator(dir)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		la.Stop()
		cancel()
	})
	if err := la.Start(ctx); err != nil {
		t.Fatalf("start aggregator: %v", err)
	}
	return la
}

func ingestMessages(la *LogAggregator, from, to int) {
	base := time.Now()
	for i := from; i < to; i++ {
		la.Ingest(LogEntry{
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Level:     "INFO",
			Message:   fmt.Sprintf("msg-%d", i),
			Source:    "test",
		})
	}
}

// A flush must only append what arrived since the previous one: re-writing the
// whole ring every interval grew the file without bound.
func TestLogAggregatorFlushWritesEachEntryOnce(t *testing.T) {
	dir := t.TempDir()
	la := startAggregator(t, dir)

	ingestMessages(la, 0, 3)
	la.flush()
	if got := readLogLines(t, dir); len(got) != 3 {
		t.Fatalf("first flush must write 3 lines, got %d: %q", len(got), got)
	}

	ingestMessages(la, 3, 5)
	la.flush()
	lines := readLogLines(t, dir)
	if len(lines) != 5 {
		t.Fatalf("second flush must append only the 2 new lines (total 5), got %d: %q", len(lines), lines)
	}
	if uniqueCount(lines) != len(lines) {
		t.Fatalf("flush must not duplicate entries, %d lines with %d unique", len(lines), uniqueCount(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "msg-4") {
		t.Fatalf("newest entry must be last, got %q", lines[len(lines)-1])
	}

	la.flush()
	if got := readLogLines(t, dir); len(got) != 5 {
		t.Fatalf("flush with no new entries must write nothing, got %d lines", len(got))
	}
}

// Once the ring wraps, the entries it overwrote can never be written; the
// cursor must skip them rather than re-emit the surviving ones from scratch.
func TestLogAggregatorFlushSkipsRingOverwrites(t *testing.T) {
	dir := t.TempDir()
	la := startAggregator(t, dir)

	total := logAggBufferSize + 5
	ingestMessages(la, 0, total)
	la.flush()

	lines := readLogLines(t, dir)
	if len(lines) != logAggBufferSize {
		t.Fatalf("flush must write exactly the surviving %d entries, got %d", logAggBufferSize, len(lines))
	}
	if uniqueCount(lines) != len(lines) {
		t.Fatalf("surviving entries written twice: %d lines, %d unique", len(lines), uniqueCount(lines))
	}
	if want := fmt.Sprintf("msg-%d", total-1); !strings.Contains(lines[len(lines)-1], want) {
		t.Fatalf("last line must be the newest entry, got %q", lines[len(lines)-1])
	}
}

func TestLogAggregatorRotatesAtMaxBytes(t *testing.T) {
	cfg := config.DefaultAppConfig()
	cfg.Logging.MaxBytes = 4096
	cfg.Logging.BackupCount = 2
	maxBytes := int64(cfg.Logging.MaxBytes)
	previous := config.GetConfig()
	config.SetGlobalConfig(cfg)
	t.Cleanup(func() { config.SetGlobalConfig(previous) })

	dir := t.TempDir()
	la := startAggregator(t, dir)

	ingestMessages(la, 0, 3000)
	la.flush()

	info, err := os.Stat(filepath.Join(dir, "edgelite_aggregated.log"))
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if info.Size() > maxBytes+1024 {
		t.Fatalf("active log must stay near max_bytes (%d), got %d bytes", maxBytes, info.Size())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var rotated []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "edgelite_aggregated.log.") {
			rotated = append(rotated, e.Name())
		}
	}
	if len(rotated) == 0 {
		t.Fatal("rotating past max_bytes must keep at least one backup")
	}
	for _, name := range rotated {
		if name == "edgelite_aggregated.log.3" {
			t.Fatalf("backup_count %d must not keep %s", cfg.Logging.BackupCount, name)
		}
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if st.Size() > maxBytes+1024 {
			t.Fatalf("backup %s exceeds max_bytes: %d bytes", name, st.Size())
		}
	}
}

// With no log directory the aggregator stays in-memory only and flush is a
// no-op; Query must still filter levels by canonical name.
func TestLogAggregatorQueryNormalizesLevels(t *testing.T) {
	la := NewLogAggregator("")
	now := time.Now()
	la.Ingest(LogEntry{Timestamp: now, Level: "WARN", Message: "disk almost full", Source: "engine"})
	la.Ingest(LogEntry{Timestamp: now, Level: "ERROR", Message: "collect failed", Source: "engine"})
	la.flush()

	for _, want := range []string{"warn", "WARNING", "warning"} {
		if got := la.Query(want, "", time.Time{}, 0); len(got) != 1 || got[0].Message != "disk almost full" {
			t.Fatalf("level %q should match the WARN entry, got %+v", want, got)
		}
	}
	if got := la.Query("", "", time.Time{}, 0); len(got) != 2 {
		t.Fatalf("no filter must return every entry, got %d", len(got))
	}
}

// The flush path must not log through the standard logger: LogrusHook feeds
// that logger back into Ingest, so a rotation notice would be written again on
// the next tick and the file would never go quiet.
func TestLogAggregatorFlushDoesNotFeedItself(t *testing.T) {
	cfg := config.DefaultAppConfig()
	cfg.Logging.MaxBytes = 2048
	cfg.Logging.BackupCount = 1
	previous := config.GetConfig()
	config.SetGlobalConfig(cfg)
	t.Cleanup(func() { config.SetGlobalConfig(previous) })

	dir := t.TempDir()
	la := startAggregator(t, dir)
	// Run with only this hook installed, and restore whatever was there after.
	savedHooks := logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(savedHooks) })
	logrus.AddHook(NewLogrusHook(la))

	ingestMessages(la, 0, 2000)
	before := la.ingested
	la.flush()
	if fed := la.ingested - before; fed != 0 {
		t.Fatalf("flush fed %d messages back into the aggregator, file grows forever", fed)
	}
	stats := la.GetStats()
	rotations, _ := stats["rotations"].(uint64)
	if rotations == 0 {
		t.Fatalf("expected at least one rotation at max_bytes=%d, stats=%v", cfg.Logging.MaxBytes, stats)
	}
	if written, _ := stats["written"].(uint64); written != 2000 {
		t.Fatalf("written counter = %v, want 2000", stats["written"])
	}
}

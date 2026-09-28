package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// The log page is the first place an operator looks, and it used to show only the
// bare message: Fire kept the handful of keys it has columns for and dropped the
// rest, so every failed HTTP request in the gateway arrived as the identical
// string "Request error" -- hundreds of rows that answer no question, and a
// search box that cannot find the endpoint that broke.

func TestLogrusHookKeepsStructuredFields(t *testing.T) {
	la := startAggregator(t, t.TempDir())
	hook := NewLogrusHook(la)

	entry := logrus.NewEntry(logrus.New())
	entry.Time = time.Now()
	entry.Level = logrus.WarnLevel
	entry.Message = "Request error"
	entry.Data = logrus.Fields{
		"path":      "/api/v1/devices",
		"method":    "GET",
		"status":    404,
		"error":     "context deadline exceeded",
		"device_id": "dev-7",
		"trace_id":  "req-7",
		"component": "http",
	}
	if err := hook.Fire(entry); err != nil {
		t.Fatalf("Fire returned an error: %v", err)
	}

	rows := la.Query("", "", time.Time{}, 0)
	if len(rows) != 1 {
		t.Fatalf("Query returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	for _, want := range []string{
		"Request error",
		"error=\"context deadline exceeded\"",
		"method=GET",
		"path=/api/v1/devices",
		"status=404",
	} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want it to carry %s", got.Message, want)
		}
	}
	// Keys with their own column stay in that column: repeating them in the
	// message would make every row longer without telling anyone anything new.
	for _, unwanted := range []string{"device_id=", "trace_id=", "component="} {
		if strings.Contains(got.Message, unwanted) {
			t.Errorf("message = %q, want %s only in its own field", got.Message, unwanted)
		}
	}
	if got.DeviceID != "dev-7" || got.RequestID != "req-7" || got.Source != "http" {
		t.Errorf("columns = device %q request %q source %q, want dev-7 / req-7 / http",
			got.DeviceID, got.RequestID, got.Source)
	}
	// Ordering is deterministic so two reads of the same log line are comparable.
	if idx := strings.Index(got.Message, "error="); idx > strings.Index(got.Message, "method=") {
		t.Errorf("fields are not sorted by key: %q", got.Message)
	}
}

func TestLogrusHookKeepsPlainMessageUntouched(t *testing.T) {
	la := startAggregator(t, t.TempDir())
	hook := NewLogrusHook(la)

	entry := logrus.NewEntry(logrus.New())
	entry.Time = time.Now()
	entry.Level = logrus.InfoLevel
	entry.Message = "collector started"
	for _, data := range []logrus.Fields{nil, {}, {"device_id": "dev-1"}} {
		entry.Data = data
		if err := hook.Fire(entry); err != nil {
			t.Fatalf("Fire returned an error: %v", err)
		}
	}

	rows := la.Query("", "", time.Time{}, 0)
	if len(rows) != 3 {
		t.Fatalf("Query returned %d rows, want 3", len(rows))
	}
	for i, row := range rows {
		if row.Message != "collector started" {
			t.Errorf("row %d message = %q, want the message alone", i, row.Message)
		}
	}
}

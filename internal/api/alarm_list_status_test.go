package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// GET /alarms filters status against the store verbatim, so ?status=active —
// the word the silence API uses — returned an empty page with 200 and read as
// "the plant has no alarms" when the query could not have matched anything.

func TestListAlarmsRejectsUnknownStatus(t *testing.T) {
	SetContainer(NewServiceContainer())
	c, rec := setupWithAdmin("GET", "/api/v1/alarms?status=active", "")
	if err := handleListAlarms(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a filter that can never match", rec.Code)
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ErrorCode != "ERR_ALARM_INVALID_STATUS" {
		t.Fatalf("error_code = %q, want ERR_ALARM_INVALID_STATUS", env.ErrorCode)
	}
	// The message has to name the values that do work; a bare "invalid status"
	// leaves the caller guessing.
	for _, want := range []string{"firing", "acknowledged", "recovered"} {
		if !strings.Contains(env.Message, want) {
			t.Fatalf("message %q does not list the accepted status %q", env.Message, want)
		}
	}
}

func TestListAlarmsAcceptsRealStatuses(t *testing.T) {
	SetContainer(NewServiceContainer())
	for _, status := range []string{"", "firing", "acknowledged", "recovered"} {
		path := "/api/v1/alarms"
		if status != "" {
			path += "?status=" + status
		}
		c, rec := setupWithAdmin("GET", path, "")
		if err := handleListAlarms(c); err != nil {
			t.Fatalf("status=%q error: %v", status, err)
		}
		if rec.Code == http.StatusBadRequest {
			t.Fatalf("status=%q rejected with 400, want it accepted", status)
		}
	}
}

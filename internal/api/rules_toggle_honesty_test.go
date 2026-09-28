package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

// Rule toggles wrote with a bare UPDATE, so "no row matched" was indistinguishable
// from "done": the batch endpoint answered success_count for IDs that named no
// rule and the single endpoint answered 200 for a rule that does not exist. These
// tests pin each route to the store's own verdict.

// withRuleService installs a container whose RuleService reads a fresh sqlite
// database, which is the only way a toggle can actually match (or miss) a row.
// The evaluator stays nil: toggling through it also starts alarm bookkeeping that
// these tests are not about.
func withRuleService(t *testing.T) *storage.RuleRepo {
	t.Helper()

	prev := GetContainer()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "rules.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	cont := NewServiceContainer()
	cont.Database = db
	cont.RuleRepo = storage.NewRuleRepo(db)
	cont.RuleService = services.NewRuleService(cont.RuleRepo, nil)
	SetContainer(cont)
	t.Cleanup(func() {
		SetContainer(prev)
		db.Close()
	})
	return cont.RuleRepo
}

func seedRule(t *testing.T, repo *storage.RuleRepo, id, createdBy string, enabled bool) {
	t.Helper()
	rule := &models.RuleResponse{
		RuleID:         id,
		Name:           "seeded " + id,
		Severity:       "major",
		Enabled:        enabled,
		NotifyChannels: []string{"email"},
	}
	if err := repo.Create(rule, createdBy); err != nil {
		t.Fatalf("seed rule %s: %v", id, err)
	}
}

// callRuleHandler runs a rule handler with the given path parameter and actor, and
// decodes the envelope it answered with.
func callRuleHandler(t *testing.T, h func(echo.Context) error, method, path, body, ruleID, userID, role string) (int, string, map[string]interface{}) {
	t.Helper()
	c, rec := setupEcho(method, path, body)
	if ruleID != "" {
		c.SetParamNames("rule_id")
		c.SetParamValues(ruleID)
	}
	if userID != "" {
		c.Set("user", &UserContext{UserID: userID, Username: userID, Role: role})
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Code      int                    `json:"code"`
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body.String(), err)
	}
	if env.Code == 0 {
		if rec.Code < 200 || rec.Code > 299 {
			t.Fatalf("HTTP %d answered with the success code 0: %s", rec.Code, rec.Body.String())
		}
	} else if env.Code != rec.Code {
		t.Fatalf("envelope code %d disagrees with the HTTP status %d", env.Code, rec.Code)
	}
	return rec.Code, env.ErrorCode, env.Data
}

func TestTogglingARuleThatDoesNotExistIsNotASuccess(t *testing.T) {
	withRuleService(t)

	for _, h := range []struct {
		name string
		fn   func(echo.Context) error
	}{
		{"enable", handleEnableRule},
		{"disable", handleDisableRule},
	} {
		code, errCode, data := callRuleHandler(t, h.fn, http.MethodPost,
			"/api/v1/rules/no-such-rule/"+h.name, "", "no-such-rule", "u-admin", "admin")
		if code != http.StatusNotFound || errCode != "ERR_RULE_NOT_FOUND" {
			t.Fatalf("%s: status = %d code = %q, want 404 ERR_RULE_NOT_FOUND", h.name, code, errCode)
		}
		if data != nil {
			t.Fatalf("%s: a missing rule must not return a rule body, got %v", h.name, data)
		}
	}
}

func TestToggleActuallyChangesTheRuleItReports(t *testing.T) {
	repo := withRuleService(t)
	seedRule(t, repo, "rule-on", "u-admin", true)

	code, _, data := callRuleHandler(t, handleDisableRule, http.MethodPost,
		"/api/v1/rules/rule-on/disable", "", "rule-on", "u-admin", "admin")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", code, data)
	}
	if got := data["enabled"]; got != false {
		t.Fatalf("response says enabled = %v, want false", got)
	}
	stored, err := repo.Get("rule-on")
	if err != nil || stored == nil {
		t.Fatalf("read back rule-on: %v", err)
	}
	if stored.Enabled {
		t.Fatal("the store still says the rule is enabled after a reported disable")
	}

	callRuleHandler(t, handleEnableRule, http.MethodPost,
		"/api/v1/rules/rule-on/enable", "", "rule-on", "u-admin", "admin")
	if stored, _ := repo.Get("rule-on"); stored == nil || !stored.Enabled {
		t.Fatal("enable reported success but left the rule disabled in the store")
	}
}

func TestBatchToggleCountsOnlyTheRulesItChanged(t *testing.T) {
	repo := withRuleService(t)
	seedRule(t, repo, "rule-a", "u-admin", true)
	seedRule(t, repo, "rule-b", "u-admin", true)

	code, errCode, data := callRuleHandler(t, handleBatchDisableRules, http.MethodPost,
		"/api/v1/rules/batch/disable",
		`{"rule_ids":["rule-a","ghost-1","ghost-2"]}`, "", "u-admin", "admin")
	if code != http.StatusOK {
		t.Fatalf("status = %d code = %q, want 200", code, errCode)
	}
	if data["success_count"] != float64(1) {
		t.Fatalf("success_count = %v, want 1 (only rule-a exists)", data["success_count"])
	}
	failed, ok := data["failed"].(map[string]interface{})
	if !ok || len(failed) != 2 {
		t.Fatalf("failed = %v, want one entry per missing rule", data["failed"])
	}
	for _, id := range []string{"ghost-1", "ghost-2"} {
		if failed[id] != "ERR_RULE_NOT_FOUND" {
			t.Fatalf("failed[%s] = %v, want ERR_RULE_NOT_FOUND", id, failed[id])
		}
	}
	if stored, _ := repo.Get("rule-a"); stored == nil || stored.Enabled {
		t.Fatal("rule-a was counted as disabled but the store still says enabled")
	}
	if stored, _ := repo.Get("rule-b"); stored == nil || !stored.Enabled {
		t.Fatal("rule-b was not asked for, yet its enabled flag changed")
	}
}

func TestBatchToggleRefusesAnEmptyOrOversizedIdList(t *testing.T) {
	withRuleService(t)

	code, errCode, _ := callRuleHandler(t, handleBatchEnableRules, http.MethodPost,
		"/api/v1/rules/batch/enable", `{"rule_ids":[]}`, "", "u-admin", "admin")
	if code != http.StatusBadRequest || errCode != "ERR_COMMON_VALIDATION" {
		t.Fatalf("empty list: status = %d code = %q, want 400 ERR_COMMON_VALIDATION", code, errCode)
	}
}

func TestTogglingAnotherUsersRuleIsRefusedAndChangesNothing(t *testing.T) {
	repo := withRuleService(t)
	seedRule(t, repo, "rule-mine", "u-owner", true)

	code, errCode, _ := callRuleHandler(t, handleDisableRule, http.MethodPost,
		"/api/v1/rules/rule-mine/disable", "", "rule-mine", "u-stranger", "operator")
	if code != http.StatusForbidden || errCode != "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED" {
		t.Fatalf("status = %d code = %q, want 403 ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED", code, errCode)
	}
	if stored, _ := repo.Get("rule-mine"); stored == nil || !stored.Enabled {
		t.Fatal("a refused toggle still disabled the rule")
	}

	code, errCode, data := callRuleHandler(t, handleBatchDisableRules, http.MethodPost,
		"/api/v1/rules/batch/disable", `{"rule_ids":["rule-mine"]}`, "", "u-stranger", "operator")
	if code != http.StatusOK {
		t.Fatalf("batch: status = %d code = %q, want 200 with a per-id failure", code, errCode)
	}
	if data["success_count"] != float64(0) {
		t.Fatalf("batch success_count = %v, want 0", data["success_count"])
	}
	if failed, _ := data["failed"].(map[string]interface{}); failed["rule-mine"] != "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED" {
		t.Fatalf("batch failed = %v, want rule-mine denied", data["failed"])
	}
}

func TestToggleFailureLabelsOnlyAMissingRuleAsMissing(t *testing.T) {
	missing := fmt.Errorf("%w: rule-gone", storage.ErrRuleNotFound)
	if got := ruleToggleFailure(missing, true); got != "ERR_RULE_NOT_FOUND" {
		t.Fatalf("missing rule while enabling = %q, want ERR_RULE_NOT_FOUND", got)
	}
	store := errors.New("database is locked")
	if got := ruleToggleFailure(store, true); got != "ERR_RULE_ENABLE_FAILED" {
		t.Fatalf("store failure while enabling = %q, want ERR_RULE_ENABLE_FAILED", got)
	}
	if got := ruleToggleFailure(store, false); got != "ERR_RULE_DISABLE_FAILED" {
		t.Fatalf("store failure while disabling = %q, want ERR_RULE_DISABLE_FAILED", got)
	}
}

func TestToggleWithoutARuleServiceSaysSo(t *testing.T) {
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	t.Cleanup(func() { SetContainer(prev) })

	for _, h := range []struct {
		name string
		fn   func(echo.Context) error
	}{
		{"enable", handleEnableRule},
		{"batch/disable", handleBatchDisableRules},
	} {
		body := ""
		if h.name == "batch/disable" {
			body = `{"rule_ids":["rule-a"]}`
		}
		code, errCode, _ := callRuleHandler(t, h.fn, http.MethodPost,
			"/api/v1/rules/"+h.name, body, "rule-a", "u-admin", "admin")
		if code != http.StatusServiceUnavailable || errCode != "ERR_COMMON_SERVICE_NOT_READY" {
			t.Fatalf("%s: status = %d code = %q, want 503 ERR_COMMON_SERVICE_NOT_READY", h.name, code, errCode)
		}
	}
}

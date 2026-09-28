package storage

import (
	"errors"
	"testing"

	"edgelite/internal/models"
)

// SetEnabled answered nil for an UPDATE that matched no row, so the API counted
// rules that do not exist among the ones it had just changed. The repo is where
// that distinction can still be made.
func TestRuleSetEnabledOnAMissingRuleReportsNotFound(t *testing.T) {
	db, err := NewDatabase(newTestConfig(t))
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	repo := NewRuleRepo(db)
	seeded := &models.RuleResponse{RuleID: "keep-me", Name: "keep", Severity: "major", Enabled: true}
	if err := repo.Create(seeded, "admin"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	err = repo.SetEnabled("never-stored", false)
	if !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("SetEnabled(missing) = %v, want an ErrRuleNotFound", err)
	}
	got, err := repo.Get("keep-me")
	if err != nil || got == nil {
		t.Fatalf("Get(keep-me) = %v, %v", got, err)
	}
	if !got.Enabled {
		t.Fatal("the unrelated rule lost its enabled flag")
	}

	if err := repo.SetEnabled("keep-me", false); err != nil {
		t.Fatalf("SetEnabled on a stored rule = %v, want nil", err)
	}
	got, _ = repo.Get("keep-me")
	if got == nil || got.Enabled {
		t.Fatalf("rule still reports enabled = %v after a successful disable", got.Enabled)
	}
}

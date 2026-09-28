package storage

// Persistence for the edge preprocessing rules and the derived-point
// expressions. Both were only ever answered from literals in the HTTP layer, so
// a rule created through the UI disappeared on the next read and never reached
// the engine that is supposed to apply it.
//
// Records live in system_settings, one key per record, which is how this
// database already keeps other single-family lists (see ListSettingKeys). One
// key per record means creating or deleting a rule cannot overwrite an edit
// another request made in between, which a whole-list blob would have.

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"edgelite/internal/models"
)

const (
	preprocessRuleKeyPrefix   = "preprocess_rule_"
	expressionConfigKeyPrefix = "expression_config_"
)

// ErrNotFound is returned when a record the caller named is not stored.
var ErrNotFound = errors.New("record not found")

// PreprocessRuleStore reads and writes preprocessing rules.
type PreprocessRuleStore struct{ db *Database }

// NewPreprocessRuleStore binds a store to an open database.
func NewPreprocessRuleStore(db *Database) *PreprocessRuleStore {
	return &PreprocessRuleStore{db: db}
}

func (s *PreprocessRuleStore) key(id string) string {
	return preprocessRuleKeyPrefix + strings.TrimSpace(id)
}

// List returns every stored rule, ordered by the key the UI lists them by
// (device then rule id, which is what the sorted prefix listing gives).
func (s *PreprocessRuleStore) List() ([]models.PreprocessRule, error) {
	keys, err := s.db.ListSettingKeys(preprocessRuleKeyPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]models.PreprocessRule, 0, len(keys))
	for _, key := range keys {
		raw, err := s.db.GetSetting(key)
		if err != nil {
			return nil, err
		}
		rule, err := decodeRule(key, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Get returns one rule by id.
func (s *PreprocessRuleStore) Get(id string) (models.PreprocessRule, error) {
	raw, err := s.db.GetSetting(s.key(id))
	if err != nil {
		return models.PreprocessRule{}, err
	}
	if raw == "" {
		return models.PreprocessRule{}, ErrNotFound
	}
	return decodeRule(s.key(id), raw)
}

// Save upserts a rule keyed by its ID.
func (s *PreprocessRuleStore) Save(rule models.PreprocessRule) error {
	if strings.TrimSpace(rule.ID) == "" {
		return errors.New("ERR_COMMON_VALIDATION: a preprocessing rule needs an id to be stored under")
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return s.db.SetSetting(s.key(rule.ID), string(raw))
}

// Delete removes a rule and reports whether anything was stored under the id.
func (s *PreprocessRuleStore) Delete(id string) (bool, error) {
	if _, err := s.Get(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.db.DeleteSetting(s.key(id)); err != nil {
		return false, err
	}
	return true, nil
}

func decodeRule(key, raw string) (models.PreprocessRule, error) {
	var rule models.PreprocessRule
	if err := json.Unmarshal([]byte(raw), &rule); err != nil {
		return rule, errCorruptSetting(key, err)
	}
	// A record written without an id, or moved under another key, would be
	// undeletable through the API, so the key the record was read from wins.
	if rule.ID == "" {
		rule.ID = strings.TrimPrefix(key, preprocessRuleKeyPrefix)
	}
	return rule, nil
}

// ExpressionConfigStore reads and writes derived-point expressions.
type ExpressionConfigStore struct{ db *Database }

// NewExpressionConfigStore binds a store to an open database.
func NewExpressionConfigStore(db *Database) *ExpressionConfigStore {
	return &ExpressionConfigStore{db: db}
}

func (s *ExpressionConfigStore) key(id string) string {
	return expressionConfigKeyPrefix + strings.TrimSpace(id)
}

// List returns every stored expression.
func (s *ExpressionConfigStore) List() ([]models.ExpressionConfig, error) {
	keys, err := s.db.ListSettingKeys(expressionConfigKeyPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]models.ExpressionConfig, 0, len(keys))
	for _, key := range keys {
		raw, err := s.db.GetSetting(key)
		if err != nil {
			return nil, err
		}
		var expr models.ExpressionConfig
		if err := json.Unmarshal([]byte(raw), &expr); err != nil {
			return nil, errCorruptSetting(key, err)
		}
		if expr.ID == "" {
			expr.ID = strings.TrimPrefix(key, expressionConfigKeyPrefix)
		}
		out = append(out, expr)
	}
	return out, nil
}

// Get returns one expression by id.
func (s *ExpressionConfigStore) Get(id string) (models.ExpressionConfig, error) {
	key := s.key(id)
	raw, err := s.db.GetSetting(key)
	if err != nil {
		return models.ExpressionConfig{}, err
	}
	if raw == "" {
		return models.ExpressionConfig{}, ErrNotFound
	}
	var expr models.ExpressionConfig
	if err := json.Unmarshal([]byte(raw), &expr); err != nil {
		return expr, errCorruptSetting(key, err)
	}
	if expr.ID == "" {
		expr.ID = strings.TrimPrefix(key, expressionConfigKeyPrefix)
	}
	return expr, nil
}

// Save upserts an expression keyed by its ID.
func (s *ExpressionConfigStore) Save(expr models.ExpressionConfig) error {
	if strings.TrimSpace(expr.ID) == "" {
		return errors.New("ERR_COMMON_VALIDATION: an expression needs an id to be stored under")
	}
	raw, err := json.Marshal(expr)
	if err != nil {
		return err
	}
	return s.db.SetSetting(s.key(expr.ID), string(raw))
}

// Delete removes an expression and reports whether it was there.
func (s *ExpressionConfigStore) Delete(id string) (bool, error) {
	raw, err := s.db.GetSetting(s.key(id))
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	if err := s.db.DeleteSetting(s.key(id)); err != nil {
		return false, err
	}
	return true, nil
}

func errCorruptSetting(key string, err error) error {
	return &CorruptSettingError{Key: key, Err: err}
}

// CorruptSettingError names the key that could not be decoded, so an operator
// can find the one record to remove instead of the list request failing with a
// message that mentions nothing.
type CorruptSettingError struct {
	Key string
	Err error
}

func (e *CorruptSettingError) Error() string {
	return "stored setting " + e.Key + " could not be decoded: " + e.Err.Error()
}

func (e *CorruptSettingError) Unwrap() error { return e.Err }

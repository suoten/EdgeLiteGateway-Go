package models

// RuleCondition represents a single rule condition.
type RuleCondition struct {
	Point       string   `json:"point"`
	Operator    string   `json:"operator"` // >, >=, <, <=, ==, !=
	Threshold   float64  `json:"threshold"`
	Type        string   `json:"type"` // threshold, ai_inference
	ModelID     string   `json:"model_id,omitempty"`
	AIThreshold *float64 `json:"ai_threshold,omitempty"`
}

// RuleCreate represents a create rule request.
type RuleCreate struct {
	Name           string          `json:"name"`
	DeviceID       string          `json:"device_id,omitempty"`
	Conditions     []RuleCondition `json:"conditions"`
	Logic          string          `json:"logic"` // AND, OR, NOT
	Duration       int             `json:"duration"`
	Severity       string          `json:"severity"` // critical, major, warning, minor, info
	NotifyChannels []string        `json:"notify_channels"`
	Script         string          `json:"script,omitempty"`
	RuleType       string          `json:"rule_type,omitempty"`
}

// RuleUpdate represents an update rule request.
type RuleUpdate struct {
	Name           *string          `json:"name,omitempty"`
	DeviceID       *string          `json:"device_id,omitempty"`
	Conditions     *[]RuleCondition `json:"conditions,omitempty"`
	Logic          *string          `json:"logic,omitempty"`
	Duration       *int             `json:"duration,omitempty"`
	Severity       *string          `json:"severity,omitempty"`
	NotifyChannels *[]string        `json:"notify_channels,omitempty"`
	Script         *string          `json:"script,omitempty"`
	RuleType       *string          `json:"rule_type,omitempty"`
}

// RuleResponse represents a rule response.
type RuleResponse struct {
	RuleID         string          `json:"rule_id"`
	Name           string          `json:"name"`
	DeviceID       string          `json:"device_id,omitempty"`
	Conditions     []RuleCondition `json:"conditions"`
	Logic          string          `json:"logic"`
	Duration       int             `json:"duration"`
	Severity       string          `json:"severity"`
	Enabled        bool            `json:"enabled"`
	NotifyChannels []string        `json:"notify_channels"`
	Script         string          `json:"script,omitempty"`
	RuleType       string          `json:"rule_type,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at,omitempty"`
	CreatedBy      string          `json:"created_by,omitempty"`
	Version        int             `json:"version"`
	InferenceCount int             `json:"inference_count"`
	ErrorCount     int             `json:"error_count"`
}

// RuleTestRequest represents a rule test request.
type RuleTestRequest struct {
	PointValues map[string]float64 `json:"point_values"`
}

// RuleVersionItem represents a rule version list item.
type RuleVersionItem struct {
	Version       int    `json:"version"`
	CreatedBy     string `json:"created_by,omitempty"`
	CreatedAt     string `json:"created_at"`
	ChangeSummary string `json:"change_summary,omitempty"`
	SnapshotHash  string `json:"snapshot_hash"`
}

// RuleVersionDetail represents a rule version detail.
type RuleVersionDetail struct {
	RuleID        string                 `json:"rule_id"`
	Version       int                    `json:"version"`
	Snapshot      map[string]interface{} `json:"snapshot"`
	SnapshotHash  string                 `json:"snapshot_hash"`
	ChangeSummary string                 `json:"change_summary,omitempty"`
	CreatedBy     string                 `json:"created_by,omitempty"`
	CreatedAt     string                 `json:"created_at"`
}

// RuleRollbackRequest represents a rule rollback request.
type RuleRollbackRequest struct {
	Version int `json:"version"`
}

// ValidRuleTypes are the allowed rule types.
var ValidRuleTypes = []string{"threshold", "ai_inference", "script"}

// isValidRuleType checks if a rule type is valid.
func isValidRuleType(rt string) bool {
	for _, valid := range ValidRuleTypes {
		if rt == valid {
			return true
		}
	}
	return false
}

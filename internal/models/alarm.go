package models

// AlarmResponse represents an alarm response.
type AlarmResponse struct {
	AlarmID        string                 `json:"alarm_id"`
	RuleID         string                 `json:"rule_id"`
	DeviceID       string                 `json:"device_id,omitempty"`
	Severity       string                 `json:"severity"`
	Status         string                 `json:"status"`
	Message        string                 `json:"message"`
	TriggerValue   map[string]interface{} `json:"trigger_value"`
	TriggerCount   int                    `json:"trigger_count"`
	FiredAt        string                 `json:"fired_at"`
	TriggeredAt    string                 `json:"triggered_at,omitempty"` // alias for FiredAt, used by correlation
	AcknowledgedAt string                 `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string                 `json:"acknowledged_by,omitempty"`
	RecoveredAt    string                 `json:"recovered_at,omitempty"`
	RuleType       string                 `json:"rule_type"`
	Version        int                    `json:"version"`
}

// AlarmGroup represents a group of correlated alarms.
type AlarmGroup struct {
	DeviceID       string          `json:"device_id"`
	Severity       string          `json:"severity"`
	Count          int             `json:"count"`
	Alarms         []AlarmResponse `json:"alarms"`
	FirstTriggered string          `json:"first_triggered"`
	LastTriggered  string          `json:"last_triggered"`
}

// AlarmAckRequest represents an alarm acknowledge request.
type AlarmAckRequest struct{}

// AlarmFilter represents alarm filter parameters.
type AlarmFilter struct {
	Status    string `query:"status,omitempty" json:"status,omitempty"`
	Severity  string `query:"severity,omitempty" json:"severity,omitempty"`
	DeviceID  string `query:"device_id,omitempty" json:"device_id,omitempty"`
	RuleID    string `query:"rule_id,omitempty" json:"rule_id,omitempty"`
	RuleType  string `query:"rule_type,omitempty" json:"rule_type,omitempty"`
	Search    string `query:"search,omitempty" json:"search,omitempty"`
	StartTime string `query:"start_time,omitempty" json:"start_time,omitempty"`
	EndTime   string `query:"end_time,omitempty" json:"end_time,omitempty"`
	// Since keeps the alarms created or changed at or after the instant, which is
	// what a reconnecting client asks for when it wants the events it missed.
	Since string `query:"since,omitempty" json:"since,omitempty"`
	// UnackOvertimeMinutes: only unacknowledged firing alarms older than N minutes.
	UnackOvertimeMinutes int `query:"unack_overtime_minutes,omitempty" json:"unack_overtime_minutes,omitempty"`
	SortBy               string `query:"sort_by,omitempty" json:"sort_by,omitempty"`
	SortOrder            string `query:"sort_order,omitempty" json:"sort_order,omitempty"`
}

// AlarmBatchAckRequest represents a batch alarm acknowledge request.
type AlarmBatchAckRequest struct {
	AlarmIDs []string `json:"alarm_ids"`
}

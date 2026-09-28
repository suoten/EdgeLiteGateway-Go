package storage

import (
	"database/sql"
	"errors"
	"time"
)

// LinkageRuleRecord is one persisted device linkage rule.
type LinkageRuleRecord struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	SourceDeviceID  string   `json:"source_device_id"`
	SourcePoint     string   `json:"source_point"`
	ConditionOp     string   `json:"condition_op"`
	Threshold       float64  `json:"threshold"`
	TargetDeviceID  string   `json:"target_device_id"`
	TargetPoint     string   `json:"target_point"`
	TargetValue     string   `json:"target_value"`
	Enabled         bool     `json:"enabled"`
	TriggerCount    int      `json:"trigger_count"`
	LastTriggeredAt string   `json:"last_triggered_at"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

// ListDeviceLinkages returns all linkage rules ordered by creation time.
func (r *AlarmRepo) ListDeviceLinkages() ([]LinkageRuleRecord, error) {
	rows, err := r.db.db.Query(`SELECT id, name, source_device_id, source_point, condition_op, threshold,
		target_device_id, target_point, target_value, enabled, trigger_count, last_triggered_at, created_at, updated_at
		FROM device_linkages ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LinkageRuleRecord{}
	for rows.Next() {
		var s LinkageRuleRecord
		var name, targetValue, lastTriggered, updatedAt sql.NullString
		var enabled int
		if err := rows.Scan(&s.ID, &name, &s.SourceDeviceID, &s.SourcePoint, &s.ConditionOp, &s.Threshold,
			&s.TargetDeviceID, &s.TargetPoint, &targetValue, &enabled, &s.TriggerCount, &lastTriggered, &s.CreatedAt, &updatedAt); err != nil {
			return nil, err
		}
		s.Name, s.TargetValue, s.LastTriggeredAt, s.UpdatedAt = name.String, targetValue.String, lastTriggered.String, updatedAt.String
		s.Enabled = enabled == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateDeviceLinkage persists a new linkage rule.
func (r *AlarmRepo) CreateDeviceLinkage(s *LinkageRuleRecord) error {
	createdAt := s.CreatedAt
	if createdAt == "" {
		createdAt = time.Now().Format(time.RFC3339)
	}
	_, err := r.db.db.Exec(
		`INSERT INTO device_linkages (id, name, source_device_id, source_point, condition_op, threshold,
		 target_device_id, target_point, target_value, enabled, trigger_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		s.ID, s.Name, s.SourceDeviceID, s.SourcePoint, s.ConditionOp, s.Threshold,
		s.TargetDeviceID, s.TargetPoint, s.TargetValue, boolToInt(s.Enabled), createdAt,
	)
	return err
}

// SetDeviceLinkageEnabled enables or disables a linkage rule.
func (r *AlarmRepo) SetDeviceLinkageEnabled(id string, enabled bool) error {
	res, err := r.db.db.Exec(`UPDATE device_linkages SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), time.Now().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	return ensureRowUpdated(res)
}

// RecordLinkageTrigger bumps the trigger counter of a rule that just actuated a
// device. The counter is the only evidence the operator gets that a rule fired,
// so a rule row that no longer exists is reported instead of being ignored.
func (r *AlarmRepo) RecordLinkageTrigger(id string, at time.Time) error {
	res, err := r.db.db.Exec(`UPDATE device_linkages SET trigger_count = trigger_count + 1,
		last_triggered_at = ?, updated_at = ? WHERE id = ?`,
		at.Format(time.RFC3339), at.Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	return ensureRowUpdated(res)
}

// DeleteDeviceLinkage removes a linkage rule.
func (r *AlarmRepo) DeleteDeviceLinkage(id string) error {
	res, err := r.db.db.Exec(`DELETE FROM device_linkages WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return ensureRowUpdated(res)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ensureRowUpdated(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("linkage rule not found")
	}
	return nil
}

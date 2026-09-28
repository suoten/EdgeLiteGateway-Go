package storage

import (
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"edgelite/internal/models"
)

// DeviceRepo handles device CRUD operations.
type DeviceRepo struct {
	db *Database
}

// NewDeviceRepo creates a new DeviceRepo.
func NewDeviceRepo(db *Database) *DeviceRepo {
	return &DeviceRepo{db: db}
}

// Create inserts a new device.
func (r *DeviceRepo) Create(d *models.DeviceResponse, createdBy string) error {
	configJSON, _ := json.Marshal(d.Config)
	pointsJSON, _ := json.Marshal(d.Points)
	_, err := r.db.db.Exec(
		`INSERT INTO devices (device_id, name, protocol, status, config, points, collect_interval, created_by, created_at, updated_at, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.DeviceID, d.Name, d.Protocol, d.Status, string(configJSON), string(pointsJSON),
		d.CollectInterval, createdBy, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339), 1,
	)
	return err
}

// Get retrieves a device by ID.
func (r *DeviceRepo) Get(deviceID string) (*models.DeviceResponse, error) {
	row := r.db.db.QueryRow(`SELECT device_id, name, protocol, status, config, points, collect_interval, created_by, created_at, updated_at, version FROM devices WHERE device_id = ?`, deviceID)
	d := &models.DeviceResponse{}
	var configJSON, pointsJSON, createdAt, updatedAt sql.NullString
	var createdBy sql.NullString
	err := row.Scan(&d.DeviceID, &d.Name, &d.Protocol, &d.Status, &configJSON, &pointsJSON, &d.CollectInterval, &createdBy, &createdAt, &updatedAt, &d.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	d.CreatedBy = createdBy.String
	d.CreatedAt = createdAt.String
	d.UpdatedAt = updatedAt.String
	if err := json.Unmarshal([]byte(configJSON.String), &d.Config); err != nil {
		return nil, fmt.Errorf("invalid config JSON for device %s: %w", deviceID, err)
	}
	if err := json.Unmarshal([]byte(pointsJSON.String), &d.Points); err != nil {
		return nil, fmt.Errorf("invalid points JSON for device %s: %w", deviceID, err)
	}
	return d, nil
}

// List returns all devices with pagination.
func (r *DeviceRepo) List(page, size int) ([]models.DeviceResponse, int, error) {
	var total int
	err := r.db.db.QueryRow("SELECT COUNT(*) FROM devices").Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	rows, err := r.db.db.Query(
		`SELECT device_id, name, protocol, status, config, points, collect_interval, created_by, created_at, updated_at, version
		 FROM devices ORDER BY created_at DESC LIMIT ? OFFSET ?`, size, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var devices []models.DeviceResponse
	for rows.Next() {
		d := models.DeviceResponse{}
		var configJSON, pointsJSON, createdAt, updatedAt sql.NullString
		var createdBy sql.NullString
		if err := rows.Scan(&d.DeviceID, &d.Name, &d.Protocol, &d.Status, &configJSON, &pointsJSON, &d.CollectInterval, &createdBy, &createdAt, &updatedAt, &d.Version); err != nil {
			return nil, 0, err
		}
		d.CreatedBy = createdBy.String
		d.CreatedAt = createdAt.String
		d.UpdatedAt = updatedAt.String
		if err := json.Unmarshal([]byte(configJSON.String), &d.Config); err != nil {
			return nil, 0, fmt.Errorf("invalid config JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(pointsJSON.String), &d.Points); err != nil {
			return nil, 0, fmt.Errorf("invalid points JSON: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("device rows iteration error: %w", err)
	}
	return devices, total, nil
}

// ListAll returns all devices without pagination (used for collector restoration on startup).
func (r *DeviceRepo) ListAll() ([]models.DeviceResponse, error) {
	rows, err := r.db.db.Query(
		`SELECT device_id, name, protocol, status, config, points, collect_interval, created_by, created_at, updated_at, version
		 FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []models.DeviceResponse
	for rows.Next() {
		d := models.DeviceResponse{}
		var configJSON, pointsJSON, createdAt, updatedAt sql.NullString
		var createdBy sql.NullString
		if err := rows.Scan(&d.DeviceID, &d.Name, &d.Protocol, &d.Status, &configJSON, &pointsJSON, &d.CollectInterval, &createdBy, &createdAt, &updatedAt, &d.Version); err != nil {
			return nil, err
		}
		d.CreatedBy = createdBy.String
		d.CreatedAt = createdAt.String
		d.UpdatedAt = updatedAt.String
		if err := json.Unmarshal([]byte(configJSON.String), &d.Config); err != nil {
			return nil, fmt.Errorf("invalid config JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(pointsJSON.String), &d.Points); err != nil {
			return nil, fmt.Errorf("invalid points JSON: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("device rows iteration error: %w", err)
	}
	return devices, nil
}
// All field updates are applied in a single transaction; if any update fails,
// the entire operation is rolled back.
func (r *DeviceRepo) Update(deviceID string, d *models.DeviceUpdate) error {
	now := time.Now().Format(time.RFC3339)

	tx, err := r.db.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if d.Name != nil {
		if _, err = tx.Exec("UPDATE devices SET name = ?, updated_at = ?, version = version + 1 WHERE device_id = ?", *d.Name, now, deviceID); err != nil {
			return fmt.Errorf("failed to update name: %w", err)
		}
	}
	if d.Config != nil {
		configJSON, _ := json.Marshal(d.Config)
		if _, err = tx.Exec("UPDATE devices SET config = ?, updated_at = ?, version = version + 1 WHERE device_id = ?", string(configJSON), now, deviceID); err != nil {
			return fmt.Errorf("failed to update config: %w", err)
		}
	}
	if d.Points != nil {
		pointsJSON, _ := json.Marshal(*d.Points)
		if _, err = tx.Exec("UPDATE devices SET points = ?, updated_at = ?, version = version + 1 WHERE device_id = ?", string(pointsJSON), now, deviceID); err != nil {
			return fmt.Errorf("failed to update points: %w", err)
		}
	}
	if d.CollectInterval != nil {
		if _, err = tx.Exec("UPDATE devices SET collect_interval = ?, updated_at = ?, version = version + 1 WHERE device_id = ?", *d.CollectInterval, now, deviceID); err != nil {
			return fmt.Errorf("failed to update collect_interval: %w", err)
		}
	}

	return tx.Commit()
}

// Delete deletes a device.
func (r *DeviceRepo) Delete(deviceID string) error {
	_, err := r.db.db.Exec("DELETE FROM devices WHERE device_id = ?", deviceID)
	return err
}

// UpdateStatus updates device status.
func (r *DeviceRepo) UpdateStatus(deviceID, status string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := r.db.db.Exec("UPDATE devices SET status = ?, updated_at = ? WHERE device_id = ?", status, now, deviceID)
	return err
}

// SetOwner reassigns a device to another user. created_by is the column the
// non-admin visibility filter reads, so it is the only change that makes a
// transferred device appear in the new owner's list.
func (r *DeviceRepo) SetOwner(deviceID, userID string) error {
	res, err := r.db.db.Exec("UPDATE devices SET created_by = ?, updated_at = ? WHERE device_id = ?",
		userID, time.Now().Format(time.RFC3339), deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("device %s not found", deviceID)
	}
	return nil
}

// RuleRepo handles rule CRUD operations.
type RuleRepo struct {
	db *Database
}

// NewRuleRepo creates a new RuleRepo.
func NewRuleRepo(db *Database) *RuleRepo {
	return &RuleRepo{db: db}
}

// Create inserts a new rule.
func (r *RuleRepo) Create(rule *models.RuleResponse, createdBy string) error {
	conditionsJSON, _ := json.Marshal(rule.Conditions)
	notifyJSON, _ := json.Marshal(rule.NotifyChannels)
	enabled := 1
	if !rule.Enabled {
		enabled = 0
	}
	_, err := r.db.db.Exec(
		`INSERT INTO rules (rule_id, name, device_id, conditions, logic, duration, severity, enabled, notify_channels, script, rule_type, created_by, created_at, updated_at, version, inference_count, error_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.RuleID, rule.Name, rule.DeviceID, string(conditionsJSON), rule.Logic, rule.Duration, rule.Severity,
		enabled, string(notifyJSON), rule.Script, rule.RuleType, createdBy,
		time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339), 1, 0, 0,
	)
	return err
}

// Get retrieves a rule by ID.
func (r *RuleRepo) Get(ruleID string) (*models.RuleResponse, error) {
	row := r.db.db.QueryRow(`SELECT rule_id, name, device_id, conditions, logic, duration, severity, enabled, notify_channels, script, rule_type, created_by, created_at, updated_at, version, inference_count, error_count FROM rules WHERE rule_id = ?`, ruleID)
	rule := &models.RuleResponse{}
	var conditionsJSON, notifyJSON, createdAt, updatedAt sql.NullString
	var createdBy, deviceID, script, ruleType sql.NullString
	var enabled int
	err := row.Scan(&rule.RuleID, &rule.Name, &deviceID, &conditionsJSON, &rule.Logic, &rule.Duration, &rule.Severity, &enabled, &notifyJSON, &script, &ruleType, &createdBy, &createdAt, &updatedAt, &rule.Version, &rule.InferenceCount, &rule.ErrorCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	rule.DeviceID = deviceID.String
	rule.CreatedBy = createdBy.String
	rule.CreatedAt = createdAt.String
	rule.UpdatedAt = updatedAt.String
	rule.Script = script.String
	rule.RuleType = ruleType.String
	rule.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(conditionsJSON.String), &rule.Conditions); err != nil {
		return nil, fmt.Errorf("invalid conditions JSON for rule %s: %w", ruleID, err)
	}
	if err := json.Unmarshal([]byte(notifyJSON.String), &rule.NotifyChannels); err != nil {
		return nil, fmt.Errorf("invalid notify JSON for rule %s: %w", ruleID, err)
	}
	return rule, nil
}

// List returns all rules with pagination.
func (r *RuleRepo) List(page, size int, deviceID string) ([]models.RuleResponse, int, error) {
	var total int
	if deviceID != "" {
		err := r.db.db.QueryRow("SELECT COUNT(*) FROM rules WHERE device_id = ?", deviceID).Scan(&total)
		if err != nil {
			return nil, 0, err
		}
	} else {
		err := r.db.db.QueryRow("SELECT COUNT(*) FROM rules").Scan(&total)
		if err != nil {
			return nil, 0, err
		}
	}
	offset := (page - 1) * size
	var rows *sql.Rows
	var err error
	if deviceID != "" {
		rows, err = r.db.db.Query(`SELECT rule_id, name, device_id, conditions, logic, duration, severity, enabled, notify_channels, script, rule_type, created_by, created_at, updated_at, version, inference_count, error_count FROM rules WHERE device_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`, deviceID, size, offset)
	} else {
		rows, err = r.db.db.Query(`SELECT rule_id, name, device_id, conditions, logic, duration, severity, enabled, notify_channels, script, rule_type, created_by, created_at, updated_at, version, inference_count, error_count FROM rules ORDER BY created_at DESC LIMIT ? OFFSET ?`, size, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var rules []models.RuleResponse
	for rows.Next() {
		rule := models.RuleResponse{}
		var conditionsJSON, notifyJSON, createdAt, updatedAt sql.NullString
		var createdBy, deviceID, script, ruleType sql.NullString
		var enabled int
		if err := rows.Scan(&rule.RuleID, &rule.Name, &deviceID, &conditionsJSON, &rule.Logic, &rule.Duration, &rule.Severity, &enabled, &notifyJSON, &script, &ruleType, &createdBy, &createdAt, &updatedAt, &rule.Version, &rule.InferenceCount, &rule.ErrorCount); err != nil {
			return nil, 0, err
		}
		rule.DeviceID = deviceID.String
		rule.CreatedBy = createdBy.String
		rule.CreatedAt = createdAt.String
		rule.UpdatedAt = updatedAt.String
		rule.Script = script.String
		rule.RuleType = ruleType.String
		rule.Enabled = enabled == 1
		if err := json.Unmarshal([]byte(conditionsJSON.String), &rule.Conditions); err != nil {
			return nil, 0, fmt.Errorf("invalid conditions JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(notifyJSON.String), &rule.NotifyChannels); err != nil {
			return nil, 0, fmt.Errorf("invalid notify JSON: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("rule rows iteration error: %w", err)
	}
	return rules, total, nil
}

// Update applies all provided fields in a single atomic UPDATE and bumps version once.
func (r *RuleRepo) Update(ruleID string, rule *models.RuleUpdate) error {
	now := time.Now().Format(time.RFC3339)

	var sets []string
	var args []interface{}
	if rule.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *rule.Name)
	}
	if rule.Conditions != nil {
		conditionsJSON, _ := json.Marshal(*rule.Conditions)
		sets = append(sets, "conditions = ?")
		args = append(args, string(conditionsJSON))
	}
	if rule.Logic != nil {
		sets = append(sets, "logic = ?")
		args = append(args, *rule.Logic)
	}
	if rule.Duration != nil {
		sets = append(sets, "duration = ?")
		args = append(args, *rule.Duration)
	}
	if rule.Severity != nil {
		sets = append(sets, "severity = ?")
		args = append(args, *rule.Severity)
	}
	if rule.NotifyChannels != nil {
		notifyJSON, _ := json.Marshal(*rule.NotifyChannels)
		sets = append(sets, "notify_channels = ?")
		args = append(args, string(notifyJSON))
	}
	if rule.Script != nil {
		sets = append(sets, "script = ?")
		args = append(args, *rule.Script)
	}
	if rule.RuleType != nil {
		sets = append(sets, "rule_type = ?")
		args = append(args, *rule.RuleType)
	}
	if rule.DeviceID != nil {
		sets = append(sets, "device_id = ?")
		args = append(args, *rule.DeviceID)
	}
	if len(sets) == 0 {
		return nil
	}
	// Bump version exactly once per update call, regardless of how many fields changed.
	sets = append(sets, "updated_at = ?", "version = version + 1")
	args = append(args, now, ruleID)
	if _, err := r.db.db.Exec("UPDATE rules SET "+strings.Join(sets, ", ")+" WHERE rule_id = ?", args...); err != nil {
		return fmt.Errorf("failed to update rule: %w", err)
	}
	return nil
}

// Delete deletes a rule.
func (r *RuleRepo) Delete(ruleID string) error {
	_, err := r.db.db.Exec("DELETE FROM rules WHERE rule_id = ?", ruleID)
	return err
}

// ErrRuleNotFound reports that no rule carries the ID an operation named. An
// UPDATE that matches nothing used to report no error at all, so a batch toggle
// counted rules that do not exist among its successes.
var ErrRuleNotFound = errors.New("rule not found")

// SetEnabled enables/disables a rule.
func (r *RuleRepo) SetEnabled(ruleID string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	now := time.Now().Format(time.RFC3339)
	res, err := r.db.db.Exec("UPDATE rules SET enabled = ?, updated_at = ? WHERE rule_id = ?", e, now, ruleID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrRuleNotFound, ruleID)
	}
	return nil
}

// IncrementInferenceCount increments the inference count.
func (r *RuleRepo) IncrementInferenceCount(ruleID string) error {
	_, err := r.db.db.Exec("UPDATE rules SET inference_count = inference_count + 1 WHERE rule_id = ?", ruleID)
	return err
}

// IncrementErrorCount increments the error count.
func (r *RuleRepo) IncrementErrorCount(ruleID string) error {
	_, err := r.db.db.Exec("UPDATE rules SET error_count = error_count + 1 WHERE rule_id = ?", ruleID)
	return err
}

// SetOwner reassigns a rule to another user, for the same reason as
// DeviceRepo.SetOwner: ownership, not a share record, decides who can see it.
func (r *RuleRepo) SetOwner(ruleID, userID string) error {
	res, err := r.db.db.Exec("UPDATE rules SET created_by = ?, updated_at = ? WHERE rule_id = ?",
		userID, time.Now().Format(time.RFC3339), ruleID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("rule %s not found", ruleID)
	}
	return nil
}

// AlarmRepo handles alarm CRUD operations.
type AlarmRepo struct {
	db *Database
}

// NewAlarmRepo creates a new AlarmRepo.
func NewAlarmRepo(db *Database) *AlarmRepo {
	return &AlarmRepo{db: db}
}

// SilenceRecord represents an alarm suppression window.
type SilenceRecord struct {
	ID        string `json:"id"`
	AlarmID   string `json:"alarm_id"`
	DeviceID  string `json:"device_id"`
	RuleID    string `json:"rule_id"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	CreatedAt string `json:"created_at"`
	Cancelled bool   `json:"cancelled"`
}

// CreateSilence persists an alarm silence window. Empty scope fields are stored as NULL.
func (r *AlarmRepo) CreateSilence(s *SilenceRecord) error {
	nullIfEmpty := func(v string) interface{} {
		if v == "" {
			return nil
		}
		return v
	}
	createdAt := s.CreatedAt
	if createdAt == "" {
		createdAt = time.Now().Format(time.RFC3339)
	}
	_, err := r.db.db.Exec(
		`INSERT INTO alarm_silences (id, alarm_id, device_id, rule_id, start_time, end_time, reason, operator, created_at, cancelled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		s.ID, nullIfEmpty(s.AlarmID), nullIfEmpty(s.DeviceID), nullIfEmpty(s.RuleID),
		s.StartTime, s.EndTime, s.Reason, nullIfEmpty(s.Operator), createdAt,
	)
	return err
}

// ListSilences returns silence windows; when activeOnly, only uncancelled windows that end in the future.
func (r *AlarmRepo) ListSilences(activeOnly bool) ([]SilenceRecord, error) {
	query := `SELECT id, alarm_id, device_id, rule_id, start_time, end_time, reason, operator, created_at, cancelled FROM alarm_silences`
	if activeOnly {
		query += " WHERE cancelled = 0 AND end_time > ?"
	}
	query += " ORDER BY created_at DESC"
	rows, err := r.db.db.Query(query, time.Now().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SilenceRecord
	for rows.Next() {
		var s SilenceRecord
		var alarmID, deviceID, ruleID, operator sql.NullString
		var cancelled int
		if err := rows.Scan(&s.ID, &alarmID, &deviceID, &ruleID, &s.StartTime, &s.EndTime, &s.Reason, &operator, &s.CreatedAt, &cancelled); err != nil {
			return nil, err
		}
		s.AlarmID, s.DeviceID, s.RuleID, s.Operator = alarmID.String, deviceID.String, ruleID.String, operator.String
		s.Cancelled = cancelled == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSilence retrieves one silence window by ID.
func (r *AlarmRepo) GetSilence(id string) (*SilenceRecord, error) {
	row := r.db.db.QueryRow(`SELECT id, alarm_id, device_id, rule_id, start_time, end_time, reason, operator, created_at, cancelled FROM alarm_silences WHERE id = ?`, id)
	var s SilenceRecord
	var alarmID, deviceID, ruleID, operator sql.NullString
	var cancelled int
	err := row.Scan(&s.ID, &alarmID, &deviceID, &ruleID, &s.StartTime, &s.EndTime, &s.Reason, &operator, &s.CreatedAt, &cancelled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	s.AlarmID, s.DeviceID, s.RuleID, s.Operator = alarmID.String, deviceID.String, ruleID.String, operator.String
	s.Cancelled = cancelled == 1
	return &s, nil
}

// CancelSilence marks a silence window as cancelled.
func (r *AlarmRepo) CancelSilence(id string) error {
	_, err := r.db.db.Exec(`UPDATE alarm_silences SET cancelled = 1 WHERE id = ?`, id)
	return err
}

// IsSilenced reports whether device/rule/alarm is covered by an active silence window.
func (r *AlarmRepo) IsSilenced(deviceID, ruleID, alarmID string) bool {
	now := time.Now().Format(time.RFC3339)
	count := 0
	query := `SELECT COUNT(*) FROM alarm_silences
		WHERE cancelled = 0 AND end_time > ? AND start_time <= ?
		  AND ((device_id IS NOT NULL AND device_id = ?)
		    OR (rule_id IS NOT NULL AND rule_id = ?)
		    OR (alarm_id IS NOT NULL AND alarm_id = ?)
		    OR (device_id IS NULL AND rule_id IS NULL AND alarm_id IS NULL))`
	_ = r.db.db.QueryRow(query, now, now, deviceID, ruleID, alarmID).Scan(&count)
	return count > 0
}

// Create inserts a new alarm.
func (r *AlarmRepo) Create(a *models.AlarmResponse) error {
	triggerJSON, _ := json.Marshal(a.TriggerValue)
	_, err := r.db.db.Exec(
		`INSERT INTO alarms (alarm_id, rule_id, device_id, severity, status, message, trigger_value, trigger_count, fired_at, acknowledged_at, acknowledged_by, recovered_at, rule_type, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.AlarmID, a.RuleID, a.DeviceID, a.Severity, a.Status, a.Message, string(triggerJSON),
		a.TriggerCount, a.FiredAt, a.AcknowledgedAt, a.AcknowledgedBy, a.RecoveredAt, a.RuleType, a.Version,
	)
	return err
}

// Get retrieves an alarm by ID.
func (r *AlarmRepo) Get(alarmID string) (*models.AlarmResponse, error) {
	row := r.db.db.QueryRow(`SELECT alarm_id, rule_id, device_id, severity, status, message, trigger_value, trigger_count, fired_at, acknowledged_at, acknowledged_by, recovered_at, rule_type, version FROM alarms WHERE alarm_id = ?`, alarmID)
	a := &models.AlarmResponse{}
	var triggerJSON, deviceID, ackAt, ackBy, recAt sql.NullString
	err := row.Scan(&a.AlarmID, &a.RuleID, &deviceID, &a.Severity, &a.Status, &a.Message, &triggerJSON, &a.TriggerCount, &a.FiredAt, &ackAt, &ackBy, &recAt, &a.RuleType, &a.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.DeviceID = deviceID.String
	a.AcknowledgedAt = ackAt.String
	a.AcknowledgedBy = ackBy.String
	a.RecoveredAt = recAt.String
	if err := json.Unmarshal([]byte(triggerJSON.String), &a.TriggerValue); err != nil {
		return nil, fmt.Errorf("invalid trigger JSON for alarm %s: %w", alarmID, err)
	}
	return a, nil
}

// List returns alarms with filters and pagination.
func (r *AlarmRepo) List(filter models.AlarmFilter, page, size int) ([]models.AlarmResponse, int, error) {
	query := "SELECT alarm_id, rule_id, device_id, severity, status, message, trigger_value, trigger_count, fired_at, acknowledged_at, acknowledged_by, recovered_at, rule_type, version FROM alarms WHERE 1=1"
	countQuery := "SELECT COUNT(*) FROM alarms WHERE 1=1"
	args := []interface{}{}
	if filter.Status != "" {
		query += " AND status = ?"
		countQuery += " AND status = ?"
		args = append(args, filter.Status)
	}
	if filter.Severity != "" {
		query += " AND severity = ?"
		countQuery += " AND severity = ?"
		args = append(args, filter.Severity)
	}
	if filter.DeviceID != "" {
		query += " AND device_id = ?"
		countQuery += " AND device_id = ?"
		args = append(args, filter.DeviceID)
	}
	if filter.RuleID != "" {
		query += " AND rule_id = ?"
		countQuery += " AND rule_id = ?"
		args = append(args, filter.RuleID)
	}
	if filter.RuleType != "" {
		query += " AND rule_type = ?"
		countQuery += " AND rule_type = ?"
		args = append(args, filter.RuleType)
	}
	if filter.Search != "" {
		like := "%" + filter.Search + "%"
		query += " AND (message LIKE ? OR rule_id LIKE ? OR device_id LIKE ?)"
		countQuery += " AND (message LIKE ? OR rule_id LIKE ? OR device_id LIKE ?)"
		args = append(args, like, like, like)
	}
	if filter.StartTime != "" {
		query += " AND fired_at >= ?"
		countQuery += " AND fired_at >= ?"
		args = append(args, filter.StartTime)
	}
	if filter.EndTime != "" {
		query += " AND fired_at <= ?"
		countQuery += " AND fired_at <= ?"
		args = append(args, filter.EndTime)
	}
	if filter.Since != "" {
		// "Since the last time I looked" is not the same question as "fired
		// since": an alarm that fired hours ago and was acknowledged a minute ago
		// is exactly what a client that was offline is missing.
		sinceClause := " AND (fired_at >= ? OR acknowledged_at >= ? OR recovered_at >= ?)"
		query += sinceClause
		countQuery += sinceClause
		args = append(args, filter.Since, filter.Since, filter.Since)
	}
	if filter.UnackOvertimeMinutes > 0 {
		cutoff := time.Now().Add(-time.Duration(filter.UnackOvertimeMinutes) * time.Minute).Format(time.RFC3339)
		query += " AND (acknowledged_at IS NULL OR acknowledged_at = '') AND fired_at <= ?"
		countQuery += " AND (acknowledged_at IS NULL OR acknowledged_at = '') AND fired_at <= ?"
		args = append(args, cutoff)
	}
	var total int
	countArgs := make([]interface{}, len(args))
	copy(countArgs, args)
	err := r.db.db.QueryRow(countQuery, countArgs...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	// Whitelist sort column to keep the ORDER BY clause injection-safe.
	sortCol := "fired_at"
	if filter.SortBy == "trigger_count" {
		sortCol = "trigger_count"
	}
	sortOrder := "DESC"
	if filter.SortOrder == "asc" {
		sortOrder = "ASC"
	}
	query += " ORDER BY " + sortCol + " " + sortOrder + " LIMIT ? OFFSET ?"
	args = append(args, size, (page-1)*size)
	rows, err := r.db.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var alarms []models.AlarmResponse
	for rows.Next() {
		a := models.AlarmResponse{}
		var triggerJSON, deviceID, ackAt, ackBy, recAt sql.NullString
		if err := rows.Scan(&a.AlarmID, &a.RuleID, &deviceID, &a.Severity, &a.Status, &a.Message, &triggerJSON, &a.TriggerCount, &a.FiredAt, &ackAt, &ackBy, &recAt, &a.RuleType, &a.Version); err != nil {
			return nil, 0, err
		}
		a.DeviceID = deviceID.String
		a.AcknowledgedAt = ackAt.String
		a.AcknowledgedBy = ackBy.String
		a.RecoveredAt = recAt.String
		if err := json.Unmarshal([]byte(triggerJSON.String), &a.TriggerValue); err != nil {
			return nil, 0, fmt.Errorf("invalid trigger JSON: %w", err)
		}
		alarms = append(alarms, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("alarm rows iteration error: %w", err)
	}
	return alarms, total, nil
}

// Acknowledge acknowledges an alarm.
func (r *AlarmRepo) Acknowledge(alarmID, userID string) error {
	now := time.Now().Format(time.RFC3339)
	// Only firing alarms can be acknowledged; report no-match (recovered/missing) as an
	// error so callers don't count recovered alarms as successfully acknowledged.
	res, err := r.db.db.Exec("UPDATE alarms SET status = 'acknowledged', acknowledged_at = ?, acknowledged_by = ?, version = version + 1 WHERE alarm_id = ? AND status = 'firing'", now, userID, alarmID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Recover marks an alarm as recovered.
func (r *AlarmRepo) Recover(alarmID string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := r.db.db.Exec("UPDATE alarms SET status = 'recovered', recovered_at = ?, version = version + 1 WHERE alarm_id = ? AND status IN ('firing', 'acknowledged')", now, alarmID)
	return err
}

// RecoverByRule closes every still-open alarm a rule holds and reports how many
// rows it moved. A rule that is deleted is no longer evaluated by anything, so
// an alarm left firing under it could never recover on its own and stayed on
// the board as a phantom with no rule behind it.
func (r *AlarmRepo) RecoverByRule(ruleID string) (int, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := r.db.db.Exec("UPDATE alarms SET status = 'recovered', recovered_at = ?, version = version + 1 WHERE rule_id = ? AND status IN ('firing', 'acknowledged')", now, ruleID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RecoverStaleByRule closes every still-open alarm a rule holds except keepID.
// A rule has one live alarm at a time, so the extras can only come from a build
// that raised a fresh one on every restart; they had no owner left to close
// them and kept the board red forever.
func (r *AlarmRepo) RecoverStaleByRule(ruleID, keepID string) (int, error) {
	if keepID == "" {
		return 0, nil
	}
	now := time.Now().Format(time.RFC3339)
	res, err := r.db.db.Exec("UPDATE alarms SET status = 'recovered', recovered_at = ?, version = version + 1 WHERE rule_id = ? AND alarm_id <> ? AND status IN ('firing', 'acknowledged')", now, ruleID, keepID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// OpenAlarmIDByRule returns the newest alarm a rule still holds open
// ("firing" or "acknowledged"), or "" when it holds none. The evaluator uses it
// to adopt an alarm that survived a restart instead of raising a second copy.
func (r *AlarmRepo) OpenAlarmIDByRule(ruleID string) (string, error) {
	var alarmID string
	err := r.db.db.QueryRow(`SELECT alarm_id FROM alarms
		WHERE rule_id = ? AND status IN ('firing', 'acknowledged')
		ORDER BY fired_at DESC, rowid DESC LIMIT 1`, ruleID).Scan(&alarmID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return alarmID, nil
}

// Delete removes an alarm from the database.
func (r *AlarmRepo) Delete(alarmID string) error {
	_, err := r.db.db.Exec("DELETE FROM alarms WHERE alarm_id = ?", alarmID)
	return err
}

// UserRepo handles user CRUD operations.
type UserRepo struct {
	db *Database
}

// NewUserRepo creates a new UserRepo.
func NewUserRepo(db *Database) *UserRepo {
	return &UserRepo{db: db}
}

// UserRecord is the internal user record.
type UserRecord struct {
	UserID             string
	Username           string
	PasswordHash       string
	Role               string
	Enabled            bool
	MustChangePassword bool
	PasswordChangedAt  string
	CreatedAt          string
	UpdatedAt          string
	Version            int
}

// Create inserts a new user.
func (r *UserRepo) Create(userID, username, passwordHash, role string) error {
	_, err := r.db.db.Exec(
		`INSERT INTO users (user_id, username, password_hash, role, enabled, must_change_password, created_at, version)
		 VALUES (?, ?, ?, ?, 1, 1, ?, 1)`,
		userID, username, passwordHash, role, time.Now().Format(time.RFC3339),
	)
	return err
}

// GetByUsername retrieves a user by username (alias for GetByUsernameWithPassword).
func (r *UserRepo) GetByUsername(username string) (*UserRecord, error) {
	return r.GetByUsernameWithPassword(username)
}

// GetByUsernameWithPassword retrieves a user by username including password hash.
func (r *UserRepo) GetByUsernameWithPassword(username string) (*UserRecord, error) {
	row := r.db.db.QueryRow(`SELECT user_id, username, password_hash, role, enabled, must_change_password, password_changed_at, created_at, updated_at, version FROM users WHERE username = ?`, username)
	u := &UserRecord{}
	var enabled, mustChange int
	var passwordChangedAt, updatedAt sql.NullString
	err := row.Scan(&u.UserID, &u.Username, &u.PasswordHash, &u.Role, &enabled, &mustChange, &passwordChangedAt, &u.CreatedAt, &updatedAt, &u.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.Enabled = enabled == 1
	u.MustChangePassword = mustChange == 1
	u.PasswordChangedAt = passwordChangedAt.String
	u.UpdatedAt = updatedAt.String
	return u, nil
}

// GetByID retrieves a user by ID.
func (r *UserRepo) GetByID(userID string) (*UserRecord, error) {
	row := r.db.db.QueryRow(`SELECT user_id, username, password_hash, role, enabled, must_change_password, password_changed_at, created_at, updated_at, version FROM users WHERE user_id = ?`, userID)
	u := &UserRecord{}
	var enabled, mustChange int
	var passwordChangedAt, updatedAt sql.NullString
	err := row.Scan(&u.UserID, &u.Username, &u.PasswordHash, &u.Role, &enabled, &mustChange, &passwordChangedAt, &u.CreatedAt, &updatedAt, &u.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.Enabled = enabled == 1
	u.MustChangePassword = mustChange == 1
	u.PasswordChangedAt = passwordChangedAt.String
	u.UpdatedAt = updatedAt.String
	return u, nil
}

// List returns all users with pagination.
func (r *UserRepo) List(page, size int) ([]UserRecord, int, error) {
	var total int
	err := r.db.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	rows, err := r.db.db.Query(`SELECT user_id, username, password_hash, role, enabled, must_change_password, password_changed_at, created_at, updated_at, version FROM users ORDER BY created_at DESC LIMIT ? OFFSET ?`, size, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var users []UserRecord
	for rows.Next() {
		u := UserRecord{}
		var enabled, mustChange int
		var passwordChangedAt, updatedAt sql.NullString
		if err := rows.Scan(&u.UserID, &u.Username, &u.PasswordHash, &u.Role, &enabled, &mustChange, &passwordChangedAt, &u.CreatedAt, &updatedAt, &u.Version); err != nil {
			return nil, 0, err
		}
		u.Enabled = enabled == 1
		u.MustChangePassword = mustChange == 1
		u.PasswordChangedAt = passwordChangedAt.String
		u.UpdatedAt = updatedAt.String
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("user rows iteration error: %w", err)
	}
	return users, total, nil
}

// UpdatePasswordByID updates a user's password by user ID.
func (r *UserRepo) UpdatePasswordByID(userID, passwordHash string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := r.db.db.Exec("UPDATE users SET password_hash = ?, must_change_password = 0, password_changed_at = ?, updated_at = ?, version = version + 1 WHERE user_id = ?", passwordHash, now, now, userID)
	return err
}

// UpdatePassword updates a user's password by username.
func (r *UserRepo) UpdatePassword(username, passwordHash string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := r.db.db.Exec("UPDATE users SET password_hash = ?, must_change_password = 0, password_changed_at = ?, updated_at = ?, version = version + 1 WHERE username = ?", passwordHash, now, now, username)
	return err
}

// Update updates a user using a transaction to ensure atomicity.
func (r *UserRepo) Update(userID string, role *string, enabled *bool) error {
	now := time.Now().Format(time.RFC3339)

	tx, err := r.db.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if role != nil {
		if _, err = tx.Exec("UPDATE users SET role = ?, updated_at = ?, version = version + 1 WHERE user_id = ?", *role, now, userID); err != nil {
			return fmt.Errorf("failed to update role: %w", err)
		}
	}
	if enabled != nil {
		e := 0
		if *enabled { e = 1 }
		if _, err = tx.Exec("UPDATE users SET enabled = ?, updated_at = ?, version = version + 1 WHERE user_id = ?", e, now, userID); err != nil {
			return fmt.Errorf("failed to update enabled: %w", err)
		}
	}

	return tx.Commit()
}

// Delete deletes a user.
func (r *UserRepo) Delete(userID string) error {
	_, err := r.db.db.Exec("DELETE FROM users WHERE user_id = ?", userID)
	return err
}

// TemplateRepo handles device template CRUD.
type TemplateRepo struct {
	db *Database
}

// NewTemplateRepo creates a new TemplateRepo.
func NewTemplateRepo(db *Database) *TemplateRepo {
	return &TemplateRepo{db: db}
}

// Create inserts a new template.
func (r *TemplateRepo) Create(t *models.TemplateResponse) error {
	configJSON, _ := json.Marshal(t.ConfigTemplate)
	pointsJSON, _ := json.Marshal(t.PointTemplates)
	_, err := r.db.db.Exec(
		`INSERT INTO templates (name, protocol, config_template, point_templates, created_at) VALUES (?, ?, ?, ?, ?)`,
		t.Name, t.Protocol, string(configJSON), string(pointsJSON), time.Now().Format(time.RFC3339),
	)
	return err
}

// Get retrieves a template by name.
func (r *TemplateRepo) Get(name string) (*models.TemplateResponse, error) {
	row := r.db.db.QueryRow(`SELECT name, protocol, config_template, point_templates, created_at FROM templates WHERE name = ?`, name)
	t := &models.TemplateResponse{}
	var configJSON, pointsJSON, createdAt sql.NullString
	err := row.Scan(&t.Name, &t.Protocol, &configJSON, &pointsJSON, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) { return nil, nil }
		return nil, err
	}
	t.CreatedAt = createdAt.String
	if err := json.Unmarshal([]byte(configJSON.String), &t.ConfigTemplate); err != nil {
		return nil, fmt.Errorf("invalid config template JSON: %w", err)
	}
	if err := json.Unmarshal([]byte(pointsJSON.String), &t.PointTemplates); err != nil {
		return nil, fmt.Errorf("invalid point templates JSON: %w", err)
	}
	return t, nil
}

// List returns all templates.
func (r *TemplateRepo) List() ([]models.TemplateResponse, error) {
	rows, err := r.db.db.Query(`SELECT name, protocol, config_template, point_templates, created_at FROM templates ORDER BY created_at DESC`)
	if err != nil { return nil, err }
	defer rows.Close()
	var templates []models.TemplateResponse
	for rows.Next() {
		t := models.TemplateResponse{}
		var configJSON, pointsJSON, createdAt sql.NullString
		if err := rows.Scan(&t.Name, &t.Protocol, &configJSON, &pointsJSON, &createdAt); err != nil {
			return nil, err
		}
		t.CreatedAt = createdAt.String
		if err := json.Unmarshal([]byte(configJSON.String), &t.ConfigTemplate); err != nil {
			return nil, fmt.Errorf("invalid config template JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(pointsJSON.String), &t.PointTemplates); err != nil {
			return nil, fmt.Errorf("invalid point templates JSON: %w", err)
		}
		templates = append(templates, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("template rows iteration error: %w", err)
	}
	return templates, nil
}

// Delete deletes a template.
func (r *TemplateRepo) Delete(name string) error {
	_, err := r.db.db.Exec("DELETE FROM templates WHERE name = ?", name)
	return err
}

// RateLimitRepo handles rate limiting persistence.
type RateLimitRepo struct {
	db *Database
}

// NewRateLimitRepo creates a new RateLimitRepo.
func NewRateLimitRepo(db *Database) *RateLimitRepo {
	return &RateLimitRepo{db: db}
}

// CheckAndIncrement checks and increments the rate limit counter for a key.
// Uses a transaction with INSERT ON CONFLICT to atomically check and increment,
// eliminating the TOCTOU race condition between SELECT and UPDATE.
func (r *RateLimitRepo) CheckAndIncrement(key string, limit int) (bool, error) {
	now := time.Now()
	windowStart := now.Add(-time.Minute).Format(time.RFC3339)

	tx, err := r.db.db.Begin()
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}

	var count int
	err = tx.QueryRow("SELECT count FROM rate_limits WHERE key = ? AND window_start > ?", key, windowStart).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return false, err
	}
	if count >= limit {
		_ = tx.Rollback()
		return false, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		// Use INSERT OR REPLACE to handle the case where a stale key exists
		_, err = tx.Exec("INSERT INTO rate_limits (key, count, window_start) VALUES (?, 1, ?) ON CONFLICT(key) DO UPDATE SET count = 1, window_start = excluded.window_start", key, now.Format(time.RFC3339))
	} else {
		_, err = tx.Exec("UPDATE rate_limits SET count = count + 1 WHERE key = ? AND window_start > ?", key, windowStart)
	}
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureAdminUser creates a default admin user if no users exist.
func EnsureAdminUser(userRepo *UserRepo, username, passwordHash string) error {
	users, _, err := userRepo.List(1, 1)
	if err != nil {
		return fmt.Errorf("failed to check users: %w", err)
	}
	if len(users) > 0 {
		return nil
	}
	userID := generateUUID()
	return userRepo.Create(userID, username, passwordHash, "admin")
}

func generateUUID() string {
	// Use crypto/rand for a UUID-like identifier to avoid collisions under concurrency.
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("u-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

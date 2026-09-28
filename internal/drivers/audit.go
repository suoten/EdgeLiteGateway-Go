package drivers

import (
	"fmt"
	"sync"
	"time"
)

// AuditAction represents the type of audit action.
type AuditAction string

const (
	AuditActionWrite        AuditAction = "write"
	AuditActionConfigChange AuditAction = "config_change"
	AuditActionFailover     AuditAction = "failover"
	AuditActionReconnect    AuditAction = "reconnect"
)

// AuditRecord represents a single audit record.
type AuditRecord struct {
	Timestamp time.Time
	DeviceID  string
	Action    AuditAction
	PointID   string
	User      string
	OldValue  interface{}
	NewValue  interface{}
	Result    string
	ErrorMsg  string
	Details   map[string]interface{}
}

// AuditLog provides audit logging for driver operations.
type AuditLog struct {
	mu      sync.Mutex
	records []AuditRecord
	maxSize int
}

// NewAuditLog creates a new AuditLog.
func NewAuditLog(maxSize int) *AuditLog {
	if maxSize <= 0 {
		maxSize = 1000
	}
	return &AuditLog{
		records: make([]AuditRecord, 0, maxSize),
		maxSize: maxSize,
	}
}

// LogWrite logs a write operation.
func (a *AuditLog) LogWrite(deviceID, pointID, user string, oldValue, newValue interface{}, result, errMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(AuditRecord{
		Timestamp: time.Now(),
		DeviceID:  deviceID,
		Action:    AuditActionWrite,
		PointID:   pointID,
		User:      user,
		OldValue:  oldValue,
		NewValue:  newValue,
		Result:    result,
		ErrorMsg:  errMsg,
	})
}

// LogConfigChange logs a configuration change.
func (a *AuditLog) LogConfigChange(deviceID, user string, oldConfig, newConfig map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(AuditRecord{
		Timestamp: time.Now(),
		DeviceID:  deviceID,
		Action:    AuditActionConfigChange,
		User:      user,
		OldValue:  oldConfig,
		NewValue:  newConfig,
		Details: map[string]interface{}{
			"old_config": oldConfig,
			"new_config": newConfig,
		},
	})
}

// LogFailover logs a link failover event.
func (a *AuditLog) LogFailover(deviceID, fromHost, toHost string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(AuditRecord{
		Timestamp: time.Now(),
		DeviceID:  deviceID,
		Action:    AuditActionFailover,
		OldValue:  fromHost,
		NewValue:  toHost,
		Details: map[string]interface{}{
			"from_host": fromHost,
			"to_host":   toHost,
		},
	})
}

// LogReconnect logs a reconnection attempt.
func (a *AuditLog) LogReconnect(deviceID string, success bool) {
	result := "success"
	if !success {
		result = "failure"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(AuditRecord{
		Timestamp: time.Now(),
		DeviceID:  deviceID,
		Action:    AuditActionReconnect,
		Result:    result,
	})
}

func (a *AuditLog) addLocked(record AuditRecord) {
	a.records = append(a.records, record)
	if len(a.records) > a.maxSize {
		a.records = a.records[1:]
	}
}

// GetRecent returns the most recent audit records.
func (a *AuditLog) GetRecent(limit int) []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > len(a.records) {
		limit = len(a.records)
	}
	result := make([]AuditRecord, limit)
	copy(result, a.records[len(a.records)-limit:])
	return result
}

// GetByDevice returns audit records for a specific device.
func (a *AuditLog) GetByDevice(deviceID string, limit int) []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []AuditRecord
	for i := len(a.records) - 1; i >= 0 && len(result) < limit; i-- {
		if a.records[i].DeviceID == deviceID {
			result = append(result, a.records[i])
		}
	}
	return result
}

// GetByAction returns audit records for a specific action type.
func (a *AuditLog) GetByAction(action AuditAction, limit int) []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []AuditRecord
	for i := len(a.records) - 1; i >= 0 && len(result) < limit; i-- {
		if a.records[i].Action == action {
			result = append(result, a.records[i])
		}
	}
	return result
}

// GetStats returns statistics about the audit log.
func (a *AuditLog) GetStats() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	stats := map[string]interface{}{
		"total_records": len(a.records),
		"by_action":     make(map[AuditAction]int),
	}
	byAction := make(map[AuditAction]int)
	for _, r := range a.records {
		byAction[r.Action]++
	}
	stats["by_action"] = byAction
	return stats
}

// ExportCSV exports audit records as CSV.
func (a *AuditLog) ExportCSV(startTime, endTime *time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	csv := "timestamp,device_id,action,point_id,user,old_value,new_value,result,error_msg\n"
	for _, r := range a.records {
		if startTime != nil && r.Timestamp.Before(*startTime) {
			continue
		}
		if endTime != nil && r.Timestamp.After(*endTime) {
			continue
		}
		csv += fmt.Sprintf("%s,%s,%s,%s,%s,%v,%v,%s,%s\n",
			r.Timestamp.Format(time.RFC3339),
			r.DeviceID,
			r.Action,
			r.PointID,
			r.User,
			r.OldValue,
			r.NewValue,
			r.Result,
			r.ErrorMsg,
		)
	}
	return csv
}

// ToDict converts an audit record to a map.
func (r *AuditRecord) ToDict() map[string]interface{} {
	return map[string]interface{}{
		"timestamp": r.Timestamp.Format(time.RFC3339),
		"device_id": r.DeviceID,
		"action":    string(r.Action),
		"point_id":  r.PointID,
		"user":      r.User,
		"old_value": r.OldValue,
		"new_value": r.NewValue,
		"result":    r.Result,
		"error_msg": r.ErrorMsg,
		"details":   r.Details,
	}
}

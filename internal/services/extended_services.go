package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// ============================================================================
// Alarm Correlation Service - 告警关联分析
// Groups related alarms by device, time window, and rule patterns.
// ============================================================================

type AlarmCorrelationService struct {
	alarmRepo *storage.AlarmRepo
	mu        sync.Mutex
	// Correlation window in seconds
	correlationWindow int
}

func NewAlarmCorrelationService(alarmRepo *storage.AlarmRepo) *AlarmCorrelationService {
	return &AlarmCorrelationService{
		alarmRepo:         alarmRepo,
		correlationWindow: 300, // 5 minutes
	}
}

// CorrelateAlarms groups active alarms by device and time window.
func (s *AlarmCorrelationService) CorrelateAlarms() ([]models.AlarmGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filter := models.AlarmFilter{
		Status:   "active",
		Severity: "",
	}
	alarms, _, err := s.alarmRepo.List(filter, 1, 1000)
	if err != nil {
		return nil, err
	}

	// Group by device ID and time window
	groups := make(map[string]*models.AlarmGroup)
	for _, alarm := range alarms {
		key := fmt.Sprintf("%s:%s", alarm.DeviceID, alarm.Severity)
		if g, ok := groups[key]; ok {
			g.Alarms = append(g.Alarms, alarm)
			g.Count++
		} else {
			groups[key] = &models.AlarmGroup{
				DeviceID:       alarm.DeviceID,
				Severity:       alarm.Severity,
				Count:          1,
				Alarms:         []models.AlarmResponse{alarm},
				FirstTriggered: alarm.FiredAt,
				LastTriggered:  alarm.FiredAt,
			}
		}
	}

	result := make([]models.AlarmGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}
	return result, nil
}

// ============================================================================
// Alarm Silence Service - 告警屏蔽
// Allows silencing alarms for a period (maintenance windows, etc.)
// ============================================================================

type AlarmSilenceService struct {
	mu       sync.Mutex
	silenced map[string]*SilenceRule
}

type SilenceRule struct {
	ID          string
	DeviceID    string
	RuleID      string
	Reason      string
	StartTime   time.Time
	EndTime     time.Time
	CreatedBy   string
}

func NewAlarmSilenceService() *AlarmSilenceService {
	return &AlarmSilenceService{
		silenced: make(map[string]*SilenceRule),
	}
}

func (s *AlarmSilenceService) Silence(req *SilenceRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silenced[req.ID] = req
	logrus.WithField("rule_id", req.ID).
		WithField("device_id", req.DeviceID).
		Info("Alarm silenced")
}

func (s *AlarmSilenceService) Unsilell(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.silenced, id)
}

func (s *AlarmSilenceService) IsSilenced(deviceID, ruleID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, rule := range s.silenced {
		if rule.DeviceID == deviceID || rule.RuleID == ruleID {
			if now.After(rule.StartTime) && now.Before(rule.EndTime) {
				return true
			}
		}
	}
	return false
}

func (s *AlarmSilenceService) ListSilenced() []SilenceRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	result := make([]SilenceRule, 0)
	for _, rule := range s.silenced {
		if now.Before(rule.EndTime) {
			result = append(result, *rule)
		}
	}
	return result
}

// ============================================================================
// Backup Scheduler Service - 定时备份
// Automatically backs up the database at configured intervals.
// ============================================================================

type BackupScheduler struct {
	mu         sync.Mutex
	backupDir  string
	dataPath   string
	interval   time.Duration
	maxBackups int
	retainDays int
	minFreeMB  int
	enabled    bool
	running    bool
	started    time.Time
	lastRun    time.Time
	lastError  string
	cancelFunc context.CancelFunc
	baseCtx    context.Context
	wake       chan struct{}
}

// BackupStatus is what the schedule page needs to show real state instead of
// echoing config values back with an invented next_run.
type BackupStatus struct {
	Running       bool   `json:"running"`
	Enabled       bool   `json:"enabled"`
	BackupDir     string `json:"backup_dir"`
	IntervalHours int    `json:"interval_hours"`
	RetainDays    int    `json:"retain_days"`
	MaxBackups    int    `json:"max_backups"`
	MinFreeMB     int    `json:"min_free_mb"`
	LastRun       string `json:"last_run"`
	NextRun       string `json:"next_run"`
	LastError     string `json:"last_error"`
}

// normalizeBackupConfig applies the defaults the scheduler needs so that a
// zeroed `backup:` section cannot produce a 0-length ticker or no retention.
func normalizeBackupConfig(cfg config.BackupConfig) (dir string, interval time.Duration, retainDays, minFreeMB int) {
	dir = cfg.BackupDir
	if dir == "" {
		dir = "data/backups"
	}
	hours := cfg.IntervalHours
	if hours <= 0 {
		hours = 24
	}
	retainDays = cfg.RetainDays
	if retainDays <= 0 {
		retainDays = 7
	}
	return dir, time.Duration(hours) * time.Hour, retainDays, cfg.MinFreeMB
}

// NewBackupScheduler builds the scheduler from the `backup:` config section so
// interval_hours / retain_days / min_free_mb actually drive it.
func NewBackupScheduler(cfg config.BackupConfig, dataPath string) *BackupScheduler {
	dir, interval, retainDays, minFreeMB := normalizeBackupConfig(cfg)
	return &BackupScheduler{
		backupDir:  dir,
		dataPath:   dataPath,
		interval:   interval,
		maxBackups: 30,
		retainDays: retainDays,
		minFreeMB:  minFreeMB,
		enabled:    cfg.Enabled,
		wake:       make(chan struct{}, 1),
	}
}

// Start launches the backup loop. It is started even when the section is
// disabled so that PUT /system/backup/schedule can enable backups at runtime;
// the loop itself honours `enabled`.
func (bs *BackupScheduler) Start(ctx context.Context) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.running {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	bs.cancelFunc = cancel
	bs.baseCtx = ctx
	bs.running = true
	bs.started = time.Now()
	bs.lastError = ""
	go bs.run(runCtx)
	logrus.WithFields(logrus.Fields{
		"dir":      bs.backupDir,
		"interval": bs.interval.String(),
		"enabled":  bs.enabled,
	}).Info("Backup scheduler started")
}

func (bs *BackupScheduler) Stop() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.cancelFunc != nil {
		bs.cancelFunc()
	}
	bs.cancelFunc = nil
	bs.running = false
}

// ApplyConfig re-reads the `backup:` section so a schedule change takes effect
// without restarting the gateway.
func (bs *BackupScheduler) ApplyConfig(cfg config.BackupConfig) BackupStatus {
	dir, interval, retainDays, minFreeMB := normalizeBackupConfig(cfg)
	bs.mu.Lock()
	bs.backupDir = dir
	bs.interval = interval
	bs.retainDays = retainDays
	bs.minFreeMB = minFreeMB
	bs.enabled = cfg.Enabled
	if cfg.Enabled {
		bs.started = time.Now()
	}
	bs.mu.Unlock()

	select {
	case bs.wake <- struct{}{}:
	default:
	}
	return bs.Status()
}

// Status reports the scheduler's live state; safe to call before Start.
func (bs *BackupScheduler) Status() BackupStatus {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	st := BackupStatus{
		Running:       bs.running,
		Enabled:       bs.enabled,
		BackupDir:     bs.backupDir,
		IntervalHours: int(bs.interval / time.Hour),
		RetainDays:    bs.retainDays,
		MaxBackups:    bs.maxBackups,
		MinFreeMB:     bs.minFreeMB,
		LastError:     bs.lastError,
	}
	if !bs.lastRun.IsZero() {
		st.LastRun = bs.lastRun.Format(time.RFC3339)
	}
	if bs.running && bs.enabled {
		base := bs.lastRun
		if base.IsZero() {
			base = bs.started
		}
		if !base.IsZero() {
			st.NextRun = base.Add(bs.interval).Format(time.RFC3339)
		}
	}
	return st
}

func (bs *BackupScheduler) run(ctx context.Context) {
	for {
		bs.mu.Lock()
		enabled, interval := bs.enabled, bs.interval
		bs.mu.Unlock()
		if !enabled {
			select {
			case <-ctx.Done():
				return
			case <-bs.wake:
			}
			continue
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-bs.wake:
			// Config changed: re-arm with the new interval / enabled flag.
			timer.Stop()
		case <-timer.C:
			bs.performBackup()
		}
	}
}

func (bs *BackupScheduler) performBackup() {
	if _, _, err := bs.BackupNow(); err != nil {
		logrus.WithField("error", err.Error()).Error("Backup: scheduled backup failed")
	}
}

// BackupNow takes one consistent snapshot and returns its file name and size.
// It uses VACUUM INTO rather than a file copy: in WAL mode the newest committed
// rows can still live in `<db>-wal`, so copying the main file yields a backup
// that quietly restores an older database.
func (bs *BackupScheduler) BackupNow() (string, int64, error) {
	bs.mu.Lock()
	dir, dataPath, minFree := bs.backupDir, bs.dataPath, bs.minFreeMB
	bs.mu.Unlock()

	if err := os.MkdirAll(dir, 0755); err != nil {
		return bs.fail(fmt.Errorf("create backup dir %s: %w", dir, err))
	}
	if minFree > 0 {
		if free, ok := availableSpaceMB(dir); ok && free < int64(minFree) {
			return bs.fail(fmt.Errorf("insufficient free space in %s: %d MB free, %d MB required", dir, free, minFree))
		}
	}

	name := bs.uniqueBackupName(dir)
	size, err := storage.BackupSQLiteFile(dataPath, filepath.Join(dir, name))
	if err != nil {
		return bs.fail(fmt.Errorf("backup %s: %w", dataPath, err))
	}

	bs.mu.Lock()
	bs.lastRun = time.Now()
	bs.lastError = ""
	bs.mu.Unlock()

	bs.cleanOldBackups()
	logrus.WithFields(logrus.Fields{"file": name, "size_bytes": size}).Info("Backup completed")
	return name, size, nil
}

// uniqueBackupName avoids clobbering an earlier snapshot taken in the same
// second, which is easy to hit when the UI button races the scheduled run.
func (bs *BackupScheduler) uniqueBackupName(dir string) string {
	stamp := time.Now().Format("20060102_150405")
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("edgelite_backup_%s.db", stamp)
		if i > 0 {
			name = fmt.Sprintf("edgelite_backup_%s-%02d.db", stamp, i)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name
		}
	}
	return fmt.Sprintf("edgelite_backup_%s-%d.db", stamp, 100)
}

func (bs *BackupScheduler) fail(err error) (string, int64, error) {
	bs.mu.Lock()
	bs.lastError = err.Error()
	bs.mu.Unlock()
	return "", 0, err
}

// availableSpaceMB reports free bytes in MB for the volume holding dir.
// ok is false when the platform cannot answer, in which case the caller must
// not treat it as "out of space".
func availableSpaceMB(dir string) (int64, bool) {
	du, err := disk.Usage(dir)
	if err != nil || du == nil {
		return 0, false
	}
	return int64(du.Free / 1024 / 1024), true
}

// cleanOldBackups removes expired backups by age and then caps the count. It is
// restricted to files this scheduler created: the backup directory is shared
// with operator drops and uploads, so deleting "the oldest entries" would eat
// unrelated files.
func (bs *BackupScheduler) cleanOldBackups() {
	bs.mu.Lock()
	dir, retainDays, maxBackups := bs.backupDir, bs.retainDays, bs.maxBackups
	bs.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type backupFile struct {
		path    string
		modTime time.Time
	}
	var backups []backupFile
	cutoff := time.Now().Add(-time.Duration(retainDays) * 24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !isBackupName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if retainDays > 0 && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, entry.Name()))
			continue
		}
		backups = append(backups, backupFile{filepath.Join(dir, entry.Name()), info.ModTime()})
	}
	if maxBackups <= 0 || len(backups) <= maxBackups {
		return
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].modTime.Before(backups[j].modTime) })
	for _, b := range backups[:len(backups)-maxBackups] {
		os.Remove(b.path)
	}
}

// isBackupName reports whether retention may delete this file. The automatic
// snapshots (edgelite_backup_) and the safety copies taken while applying a
// restore (pre_restore_) both live here, and both have to count against the
// retention budget: with only the first prefix every restore left a full-size
// copy of the database on disk forever.
func isBackupName(name string) bool {
	if !strings.HasSuffix(name, ".db") {
		return false
	}
	return strings.HasPrefix(name, "edgelite_backup_") || strings.HasPrefix(name, "pre_restore_")
}

// ============================================================================
// Data Import/Export Service - 数据导入导出
// ============================================================================

type DataImportExportService struct {
	tsStorage *storage.TimeSeriesStorage
}

func NewDataImportExportService(tsStorage *storage.TimeSeriesStorage) *DataImportExportService {
	return &DataImportExportService{tsStorage: tsStorage}
}

// ExportData exports time series data to CSV format.
func (s *DataImportExportService) ExportData(deviceID, pointName string, startTime, endTime time.Time) ([]byte, error) {
	records, err := s.tsStorage.QueryPoints(deviceID, pointName, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("export query failed: %w", err)
	}

	var csv strings.Builder
	csv.WriteString("device_id,point_name,value,quality,timestamp\n")
	for _, r := range records {
		csv.WriteString(fmt.Sprintf("%s,%s,%v,%s,%s\n",
			r.DeviceID, r.PointName, r.Value, r.Quality, r.Timestamp.Format(time.RFC3339)))
	}
	return []byte(csv.String()), nil
}

// ============================================================================
// Command Approval Service - 命令审批
// Requires approval for write commands to critical devices.
// ============================================================================

type CommandApprovalService struct {
	mu        sync.Mutex
	pending   map[string]*CommandApproval
	approved  map[string]bool
}

type CommandApproval struct {
	ID         string
	DeviceID   string
	Point      string
	Value      interface{}
	RequestedBy string
	RequestedAt time.Time
	Status     string // pending, approved, rejected
	ApprovedBy string
	ApprovedAt time.Time
}

func NewCommandApprovalService() *CommandApprovalService {
	return &CommandApprovalService{
		pending:  make(map[string]*CommandApproval),
		approved: make(map[string]bool),
	}
}

func (s *CommandApprovalService) RequestApproval(req *CommandApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[req.ID] = req
}

func (s *CommandApprovalService) Approve(id, approver string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd, ok := s.pending[id]
	if !ok {
		return fmt.Errorf("approval request not found: %s", id)
	}
	cmd.Status = "approved"
	cmd.ApprovedBy = approver
	cmd.ApprovedAt = time.Now()
	s.approved[id] = true
	return nil
}

func (s *CommandApprovalService) Reject(id, rejecter string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd, ok := s.pending[id]
	if !ok {
		return fmt.Errorf("approval request not found: %s", id)
	}
	cmd.Status = "rejected"
	cmd.ApprovedBy = rejecter
	cmd.ApprovedAt = time.Now()
	return nil
}

func (s *CommandApprovalService) IsApproved(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approved[id]
}

func (s *CommandApprovalService) ListPending() []*CommandApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*CommandApproval, 0)
	for _, cmd := range s.pending {
		if cmd.Status == "pending" {
			result = append(result, cmd)
		}
	}
	return result
}

// ============================================================================
// Shadow Service - 设备影子
// Maintains last known state and desired state for devices.
// ============================================================================

type ShadowService struct {
	mu      sync.Mutex
	shadows map[string]*DeviceShadow
}

type DeviceShadow struct {
	DeviceID      string
	ReportedState map[string]interface{}
	DesiredState  map[string]interface{}
	LastUpdated   time.Time
	Version       int
}

func NewShadowService() *ShadowService {
	return &ShadowService{
		shadows: make(map[string]*DeviceShadow),
	}
}

func (s *ShadowService) GetShadow(deviceID string) *DeviceShadow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shadows[deviceID]
}

func (s *ShadowService) ListShadows() []*DeviceShadow {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*DeviceShadow, 0, len(s.shadows))
	for _, shadow := range s.shadows {
		list = append(list, shadow)
	}
	return list
}

func (s *ShadowService) DeleteShadow(deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.shadows[deviceID]; !ok {
		return false
	}
	delete(s.shadows, deviceID)
	return true
}

func (s *ShadowService) UpdateReported(deviceID string, state map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	shadow, ok := s.shadows[deviceID]
	if !ok {
		shadow = &DeviceShadow{
			DeviceID: deviceID,
			Version:  1,
		}
		s.shadows[deviceID] = shadow
	}
	// Replace semantics: callers (UI editor) submit the full reported state.
	replaced := make(map[string]interface{}, len(state))
	for k, v := range state {
		replaced[k] = v
	}
	shadow.ReportedState = replaced
	shadow.LastUpdated = time.Now()
	shadow.Version++
}

func (s *ShadowService) UpdateDesired(deviceID string, state map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	shadow, ok := s.shadows[deviceID]
	if !ok {
		shadow = &DeviceShadow{
			DeviceID: deviceID,
			Version:  1,
		}
		s.shadows[deviceID] = shadow
	}
	// Replace semantics: callers (UI editor) submit the full desired state,
	// and an empty state clears all desired values.
	replaced := make(map[string]interface{}, len(state))
	for k, v := range state {
		replaced[k] = v
	}
	shadow.DesiredState = replaced
	shadow.LastUpdated = time.Now()
	shadow.Version++
}

func (s *ShadowService) GetDelta(deviceID string) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	shadow, ok := s.shadows[deviceID]
	if !ok {
		return nil
	}
	delta := make(map[string]interface{})
	for k, desiredVal := range shadow.DesiredState {
		reportedVal, exists := shadow.ReportedState[k]
		if !exists || !equalValues(reportedVal, desiredVal) {
			delta[k] = desiredVal
		}
	}
	return delta
}

func equalValues(a, b interface{}) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// ============================================================================
// DB Monitor Service - 数据库监控
// Monitors database file sizes and connection health.
// ============================================================================

type DBMonitorService struct {
	mu         sync.Mutex
	dataPath   string
	lastCheck  time.Time
	checkInterval time.Duration
}

func NewDBMonitorService(dataPath string) *DBMonitorService {
	return &DBMonitorService{
		dataPath:      dataPath,
		checkInterval: 60 * time.Second,
	}
}

func (m *DBMonitorService) CheckHealth() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCheck = time.Now()

	result := map[string]interface{}{
		"timestamp": m.lastCheck.Format(time.RFC3339),
	}

	if info, err := os.Stat(m.dataPath); err == nil {
		result["db_size_bytes"] = info.Size()
		result["db_size_mb"] = float64(info.Size()) / (1024 * 1024)
		result["db_modified"] = info.ModTime().Format(time.RFC3339)
	}

	// Check WAL file
	walPath := m.dataPath + "-wal"
	if info, err := os.Stat(walPath); err == nil {
		result["wal_size_bytes"] = info.Size()
	}

	// Check SHM file
	shmPath := m.dataPath + "-shm"
	if info, err := os.Stat(shmPath); err == nil {
		result["shm_size_bytes"] = info.Size()
	}

	return result
}

// GetStats returns database statistics for the DbMonitor dashboard.
//
// The keys are the names the page and the handler's zero fallback read; the
// service used to answer db_size_bytes/wal_size_bytes, so a successful
// measurement still rendered as blank cells. sql.Open here failed for the whole
// life of this endpoint (it asked for the unregistered "sqlite3" driver) and the
// error was dropped on the floor, which made every gateway report an empty
// database instead of a broken monitor.
func (m *DBMonitorService) GetStats() (map[string]interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCheck = time.Now()

	result := map[string]interface{}{
		"db_size":         0,
		"table_count":     0,
		"total_rows":      0,
		"wal_size":        0,
		"page_size":       0,
		"free_node_pages": 0,
		"measured":        false,
	}

	if info, err := os.Stat(m.dataPath); err == nil {
		result["db_size"] = info.Size()
	}
	walPath := m.dataPath + "-wal"
	if info, err := os.Stat(walPath); err == nil {
		result["wal_size"] = info.Size()
	}

	tables, err := m.tableNamesLocked()
	if err != nil {
		result["measure_error"] = err.Error()
		logrus.WithError(err).Warn("DB monitor cannot open the database")
		return result, nil
	}
	defer tables.db.Close()

	result["measured"] = true
	var totalRows int64
	for _, name := range tables.names {
		var count int64
		if err := tables.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM [%s]", name)).Scan(&count); err == nil {
			totalRows += count
		}
	}
	result["table_count"] = len(tables.names)
	result["total_rows"] = totalRows

	var pageSize, freePages int64
	if err := tables.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err == nil {
		result["page_size"] = pageSize
	}
	if err := tables.db.QueryRow("PRAGMA freelist_count").Scan(&freePages); err == nil {
		result["free_node_pages"] = freePages
	}

	return result, nil
}

// sqliteTables lists the user tables and the handle to read them from.
type sqliteTables struct {
	db    *sql.DB
	names []string
}

func (m *DBMonitorService) tableNamesLocked() (*sqliteTables, error) {
	db, err := sql.Open("sqlite", m.dataPath)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// sqlite_* internals (autoindexes, sqlite_sequence) are not the operator's
	// tables and would inflate both the count and the row total.
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		db.Close()
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	if err := rows.Err(); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteTables{db: db, names: names}, nil
}

// quoteSQLiteIdent escapes an object name for use inside a double-quoted
// identifier. The names come from sqlite_master, so this is defence in depth
// against a table created with a quote in its name.
func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// tablePageSizes returns bytes per table and per index, keyed by the object
// name. It needs the dbstat virtual table, which is not compiled into every
// SQLite build; when it is missing the caller reports no sizes rather than
// inventing zeros.
func tablePageSizes(db *sql.DB) (map[string]int64, error) {
	rows, err := db.Query("SELECT name, SUM(pgsize) FROM dbstat GROUP BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sizes := map[string]int64{}
	for rows.Next() {
		var name string
		var bytes int64
		if err := rows.Scan(&name, &bytes); err == nil {
			sizes[name] = bytes
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sizes, nil
}

// indexNamesOf returns the indexes attached to a table.
func indexNamesOf(db *sql.DB, table string) []string {
	rows, err := db.Query("PRAGMA index_list(" + quoteSQLiteIdent(table) + ")")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err == nil {
			names = append(names, name)
		}
	}
	return names
}

// GetTableDetails returns per-table statistics.
func (m *DBMonitorService) GetTableDetails() ([]map[string]interface{}, error) {
	tables, err := m.tableNamesLocked()
	if err != nil {
		logrus.WithError(err).Warn("DB monitor cannot open the database for table details")
		return []map[string]interface{}{}, err
	}
	defer tables.db.Close()
	db := tables.db

	sizes, sizesErr := tablePageSizes(db)
	if sizesErr != nil {
		logrus.WithError(sizesErr).Debug("dbstat is unavailable, per-table sizes are omitted")
	}

	var tablesOut []map[string]interface{}
	for _, name := range tables.names {
		info := map[string]interface{}{"name": name}
		var count int64
		if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM [%s]", name)).Scan(&count); err == nil {
			info["row_count"] = count
		}
		if sizes != nil {
			dataSize := sizes[name]
			var indexSize int64
			for _, idx := range indexNamesOf(db, name) {
				indexSize += sizes[idx]
			}
			info["data_size"] = dataSize
			info["index_size"] = indexSize
			info["total_size"] = dataSize + indexSize
		}
		tablesOut = append(tablesOut, info)
	}
	if tablesOut == nil {
		tablesOut = []map[string]interface{}{}
	}
	return tablesOut, nil
}

// Vacuum executes VACUUM on the database.
func (m *DBMonitorService) Vacuum() error {
	db, err := sql.Open("sqlite", m.dataPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("VACUUM")
	return err
}

// Reindex executes REINDEX on the database.
func (m *DBMonitorService) Reindex() error {
	db, err := sql.Open("sqlite", m.dataPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("REINDEX")
	return err
}

// ============================================================================

type MCPService struct {
	mu      sync.Mutex
	tools   map[string]MCPTool
}

type MCPTool struct {
	Name        string
	Description string
	// Parameters is a JSON-Schema-shaped description of the params map the
	// handler accepts. A caller can only invoke a tool whose arguments it can
	// construct, so the registry has to carry that contract: without it
	// /mcp/tools advertises names that have to be called by guesswork.
	Parameters map[string]interface{}
	Handler    func(params map[string]interface{}) (interface{}, error)
}

func NewMCPService() *MCPService {
	return &MCPService{
		tools: make(map[string]MCPTool),
	}
}

func (s *MCPService) RegisterTool(tool MCPTool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool.Name] = tool
}

func (s *MCPService) CallTool(name string, params map[string]interface{}) (interface{}, error) {
	s.mu.Lock()
	tool, ok := s.tools[name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", name)
	}
	return tool.Handler(params)
}

func (s *MCPService) ListTools() []MCPTool {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]MCPTool, 0, len(s.tools))
	for _, tool := range s.tools {
		result = append(result, tool)
	}
	return result
}

// ============================================================================
// Video Service - 视频服务
// ============================================================================

type VideoService struct {
	mu      sync.Mutex
	streams map[string]*VideoStream
}

type VideoStream struct {
	DeviceID  string
	URL       string
	Protocol  string // rtsp, rtmp, http
	Status    string
	StartedAt time.Time
}

func NewVideoService() *VideoService {
	return &VideoService{
		streams: make(map[string]*VideoStream),
	}
}

func (s *VideoService) StartStream(deviceID, url, protocol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[deviceID] = &VideoStream{
		DeviceID:  deviceID,
		URL:       url,
		Protocol:  protocol,
		Status:    "started",
		StartedAt: time.Now(),
	}
	return nil
}

func (s *VideoService) StopStream(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream, ok := s.streams[deviceID]; ok {
		stream.Status = "stopped"
	}
	return nil
}

func (s *VideoService) GetStream(deviceID string) *VideoStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[deviceID]
}

func (s *VideoService) ListStreams() []*VideoStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*VideoStream, 0, len(s.streams))
	for _, stream := range s.streams {
		result = append(result, stream)
	}
	return result
}

// ============================================================================
// i18n Service - 国际化
// ============================================================================

type I18nService struct {
	mu       sync.Mutex
	messages map[string]map[string]string // lang -> key -> message
	lang     string
}

func NewI18nService() *I18nService {
	s := &I18nService{
		messages: make(map[string]map[string]string),
		lang:     "zh-CN",
	}
	// Default messages
	s.messages["zh-CN"] = map[string]string{
		"device.created":       "设备创建成功",
		"device.updated":       "设备更新成功",
		"device.deleted":       "设备删除成功",
		"rule.created":         "规则创建成功",
		"rule.updated":         "规则更新成功",
		"alarm.triggered":      "告警触发",
		"alarm.acknowledged":   "告警已确认",
		"system.error":         "系统错误",
	}
	s.messages["en-US"] = map[string]string{
		"device.created":       "Device created successfully",
		"device.updated":       "Device updated successfully",
		"device.deleted":       "Device deleted successfully",
		"rule.created":         "Rule created successfully",
		"rule.updated":         "Rule updated successfully",
		"alarm.triggered":      "Alarm triggered",
		"alarm.acknowledged":   "Alarm acknowledged",
		"system.error":         "System error",
	}
	return s
}

func (s *I18nService) SetLanguage(lang string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lang = lang
}

func (s *I18nService) Translate(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msgs, ok := s.messages[s.lang]; ok {
		if msg, ok := msgs[key]; ok {
			return msg
		}
	}
	return key
}

// ============================================================================
// Service Manager - 服务管理器
// Manages lifecycle of all background services.
// ============================================================================

type ServiceManager struct {
	mu       sync.Mutex
	services map[string]ManagedService
}

type ManagedService struct {
	Name      string
	Status    string // running, stopped, error
	StartedAt time.Time
	Error     error
	StartFunc func() error
	StopFunc  func() error
}

func NewServiceManager() *ServiceManager {
	return &ServiceManager{
		services: make(map[string]ManagedService),
	}
}

func (sm *ServiceManager) Register(name string, startFunc, stopFunc func() error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.services[name] = ManagedService{
		Name:      name,
		Status:    "stopped",
		StartFunc: startFunc,
		StopFunc:  stopFunc,
	}
}

func (sm *ServiceManager) StartAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for name, svc := range sm.services {
		if svc.Status == "running" {
			continue
		}
		err := svc.StartFunc()
		if err != nil {
			svc.Status = "error"
			svc.Error = err
			logrus.WithField("service", name).
				WithField("error", err.Error()).
				Error("Failed to start service")
		} else {
			svc.Status = "running"
			svc.StartedAt = time.Now()
			logrus.WithField("service", name).Info("Service started")
		}
		sm.services[name] = svc
	}
}

func (sm *ServiceManager) StopAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for name, svc := range sm.services {
		if svc.Status != "running" {
			continue
		}
		err := svc.StopFunc()
		if err != nil {
			logrus.WithField("service", name).
				WithField("error", err.Error()).
				Warn("Failed to stop service")
		} else {
			svc.Status = "stopped"
			logrus.WithField("service", name).Info("Service stopped")
		}
		sm.services[name] = svc
	}
}

func (sm *ServiceManager) GetStatus() []ManagedService {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	result := make([]ManagedService, 0, len(sm.services))
	for _, svc := range sm.services {
		result = append(result, svc)
	}
	return result
}

// ============================================================================
// Audit Service - 审计日志服务
// ============================================================================

type AuditService struct {
	mu      sync.Mutex
	entries []AuditEntry
	maxSize int
	db      *storage.Database
}

// AuditEntry JSON 字段与前端 AuditLog 接口（api/index.ts）保持一致：
// log_id / resource_type / ip_address / created_at。
type AuditEntry struct {
	ID           string                 `json:"log_id"`
	UserID       string                 `json:"user_id"`
	Username     string                 `json:"username"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resource_type"`
	ResourceID   string                 `json:"resource_id"`
	IPAddress    string                 `json:"ip_address"`
	Status       string                 `json:"status"`
	Details      map[string]interface{} `json:"details"`
	CreatedAt    time.Time              `json:"created_at"`
}

type AuditFilter struct {
	UserID       string
	Action       string
	ResourceType string
	// ResourceID narrows the trail to one object, e.g. the write history of a
	// single device.
	ResourceID string
	Status     string
	StartTime  string
	EndTime    string
}

func NewAuditService(db *storage.Database) *AuditService {
	return &AuditService{
		entries: make([]AuditEntry, 0),
		maxSize: 10000,
		db:      db,
	}
}

// Log records an audit event to the persistent store (best effort) and the
// in-memory ring used as fallback when the DB is unavailable.
func (s *AuditService) Log(entry AuditEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.Status == "" {
		entry.Status = "success"
	}
	if entry.Details == nil {
		entry.Details = map[string]interface{}{}
	}
	if s.db != nil {
		detailsJSON, err := json.Marshal(entry.Details)
		if err != nil {
			detailsJSON = []byte("{}")
		}
		if id, err := s.db.InsertAuditLog(storage.AuditRecord{
			UserID:       entry.UserID,
			Username:     entry.Username,
			Action:       entry.Action,
			ResourceType: entry.ResourceType,
			ResourceID:   entry.ResourceID,
			IPAddress:    entry.IPAddress,
			Status:       entry.Status,
			Details:      string(detailsJSON),
			CreatedAt:    entry.CreatedAt.Format(time.RFC3339),
		}); err == nil {
			entry.ID = strconv.FormatInt(id, 10)
		} else {
			logrus.WithError(err).Warn("Audit: failed to persist entry")
		}
	}
	s.entries = append(s.entries, entry)
	if len(s.entries) > s.maxSize {
		s.entries = s.entries[len(s.entries)-s.maxSize:]
	}
}

// List returns a newest-first page of audit entries. It reads from the
// persistent store and falls back to the in-memory ring on DB failure.
func (s *AuditService) List(filter AuditFilter, page, size int) ([]AuditEntry, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if s.db != nil {
		recs, total, err := s.db.ListAuditLogs(filter.UserID, filter.Action, filter.ResourceType, filter.ResourceID, filter.Status, filter.StartTime, filter.EndTime, page, size)
		if err == nil {
			entries := make([]AuditEntry, 0, len(recs))
			for _, r := range recs {
				entries = append(entries, auditEntryFromRecord(r))
			}
			return entries, total
		}
		logrus.WithError(err).Warn("Audit: DB list failed, falling back to memory")
	}
	return s.listMemory(filter, page, size)
}

// listMemory mirrors the DB path: newest first, filtered in SQL-equivalent
// order, then paged. Walking s.entries backwards keeps the fallback in the same
// order as the store it replaces; reversing after slicing would drop the newest
// rows onto pages the caller never reaches.
func (s *AuditService) listMemory(filter AuditFilter, page, size int) ([]AuditEntry, int) {
	var filtered []AuditEntry
	for i := len(s.entries) - 1; i >= 0; i-- {
		entry := s.entries[i]
		if filter.UserID != "" && entry.UserID != filter.UserID && entry.Username != filter.UserID {
			continue
		}
		if filter.Action != "" && entry.Action != filter.Action {
			continue
		}
		if filter.ResourceType != "" && entry.ResourceType != filter.ResourceType {
			continue
		}
		if filter.ResourceID != "" && entry.ResourceID != filter.ResourceID {
			continue
		}
		if filter.Status != "" && entry.Status != filter.Status {
			continue
		}
		ts := entry.CreatedAt.Format(time.RFC3339)
		if filter.StartTime != "" && ts < filter.StartTime {
			continue
		}
		if filter.EndTime != "" && ts > filter.EndTime {
			continue
		}
		filtered = append(filtered, entry)
	}
	total := len(filtered)
	start := (page - 1) * size
	if start >= total {
		return nil, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return filtered[start:end], total
}

// Integrity verifies the tamper-evidence hash chain of the persistent trail.
func (s *AuditService) Integrity() (total int, brokenAt []int64, err error) {
	if s.db != nil {
		return s.db.AuditIntegrity()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), []int64{}, nil
}

// Cleanup deletes audit entries older than the cutoff and prunes the memory ring.
func (s *AuditService) Cleanup(cutoff time.Time) (int64, error) {
	cutoffStr := cutoff.Format(time.RFC3339)
	if s.db != nil {
		deleted, err := s.db.CleanupAuditLogs(cutoffStr)
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		kept := s.entries[:0]
		for _, e := range s.entries {
			if e.CreatedAt.Format(time.RFC3339) >= cutoffStr {
				kept = append(kept, e)
			}
		}
		s.entries = kept
		s.mu.Unlock()
		return deleted, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	kept := s.entries[:0]
	for _, e := range s.entries {
		if e.CreatedAt.Format(time.RFC3339) >= cutoffStr {
			kept = append(kept, e)
		} else {
			deleted++
		}
	}
	s.entries = kept
	return deleted, nil
}

func auditEntryFromRecord(r storage.AuditRecord) AuditEntry {
	entry := AuditEntry{
		ID:           strconv.FormatInt(r.ID, 10),
		UserID:       r.UserID,
		Username:     r.Username,
		Action:       r.Action,
		ResourceType: r.ResourceType,
		ResourceID:   r.ResourceID,
		IPAddress:    r.IPAddress,
		Status:       r.Status,
		Details:      map[string]interface{}{},
	}
	if r.Details != "" {
		_ = json.Unmarshal([]byte(r.Details), &entry.Details)
	}
	if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
		entry.CreatedAt = t
	}
	return entry
}

// ============================================================================
// Historical Data Service - 历史数据服务
// ============================================================================

type HistoricalDataService struct {
	tsStorage *storage.TimeSeriesStorage
}

func NewHistoricalDataService(tsStorage *storage.TimeSeriesStorage) *HistoricalDataService {
	return &HistoricalDataService{tsStorage: tsStorage}
}

// GetAggregatedData returns aggregated historical data.
func (s *HistoricalDataService) GetAggregatedData(deviceID, pointName string, startTime, endTime time.Time, interval string) ([]map[string]interface{}, error) {
	records, err := s.tsStorage.QueryPoints(deviceID, pointName, startTime, endTime)
	if err != nil {
		return nil, err
	}

	// Group by interval
	intervalDuration, err := parseInterval(interval)
	if err != nil {
		return nil, err
	}

	groups := make(map[int64][]storage.PointData)
	for _, r := range records {
		bucket := r.Timestamp.Unix() / int64(intervalDuration.Seconds())
		groups[bucket] = append(groups[bucket], r)
	}

	result := make([]map[string]interface{}, 0, len(groups))
	for bucket, records := range groups {
		var sum, min, max float64
		first := true
		for _, r := range records {
			val := toFloat64(r.Value)
			if first {
				min, max = val, val
				first = false
			}
			sum += val
			if val < min {
				min = val
			}
			if val > max {
				max = val
			}
		}
		count := len(records)
		result = append(result, map[string]interface{}{
			"timestamp": time.Unix(bucket*int64(intervalDuration.Seconds()), 0).Format(time.RFC3339),
			"avg":       sum / float64(count),
			"min":       min,
			"max":       max,
			"count":     count,
		})
	}

	return result, nil
}

func parseInterval(interval string) (time.Duration, error) {
	switch interval {
	case "1m":
		return time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "15m":
		return 15 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown interval: %s", interval)
	}
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	}
	return 0
}

// JSON helper for marshal/unmarshal
func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// Ensure strings package is used
var _ = strings.TrimSpace

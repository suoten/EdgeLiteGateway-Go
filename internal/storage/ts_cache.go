package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register sqlite driver (side-effect import)

	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/constants"
)

// TimeSeriesStorage handles time-series data storage with InfluxDB fallback to SQLite.
type TimeSeriesStorage struct {
	sqliteTSDB   *sql.DB
	influxURL    string
	influxToken  string
	influxOrg    string
	influxBucket string
	fallback     bool
	mu           sync.RWMutex
}

// NewTimeSeriesStorage creates a new TimeSeriesStorage.
func NewTimeSeriesStorage(cfg *config.AppConfig) (*TimeSeriesStorage, error) {
	ts := &TimeSeriesStorage{
		influxURL:    cfg.InfluxDB.URL,
		influxToken:  cfg.InfluxDB.Token,
		influxOrg:    cfg.InfluxDB.Org,
		influxBucket: cfg.InfluxDB.Bucket,
		fallback:     cfg.InfluxDB.Token == "",
	}

	// Always open SQLite TS as fallback
	// modernc.org/sqlite needs _pragma= params; mattn-style ones are ignored.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", cfg.InfluxDB.SQLiteTSPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open TS database: %w", err)
	}
	ts.sqliteTSDB = db

	// Configure connection pool for concurrent access
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(30 * time.Minute)

	if err := ts.initTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to init TS tables: %w", err)
	}

	return ts, nil
}

// initTables creates the time-series table.
func (t *TimeSeriesStorage) initTables() error {
	_, err := t.sqliteTSDB.Exec(`CREATE TABLE IF NOT EXISTS time_series (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		device_id TEXT NOT NULL,
		point_name TEXT NOT NULL,
		value REAL,
		value_str TEXT,
		quality TEXT NOT NULL DEFAULT 'good',
		timestamp TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return err
	}
	_, err = t.sqliteTSDB.Exec(`CREATE INDEX IF NOT EXISTS idx_ts_device_point ON time_series(device_id, point_name, timestamp)`)
	return err
}

// PointData represents a single time-series data point.
type PointData struct {
	DeviceID  string      `json:"device_id"`
	PointName string      `json:"point_name"`
	Value     interface{} `json:"value"`
	Quality   string      `json:"quality"`
	Timestamp time.Time   `json:"timestamp"`
}

// WritePoints writes multiple data points.
// Uses a transaction for atomic batch insertion. The mutex is used only to
// serialize transaction creation to avoid SQLite "database is locked" errors
// under high concurrency — the lock is released as soon as the transaction
// is committed or rolled back.
func (t *TimeSeriesStorage) WritePoints(points []PointData) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tx, err := t.sqliteTSDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO time_series (device_id, point_name, value, value_str, quality, timestamp) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range points {
		floatVal, strVal := splitPointValue(p.Value)
		quality := p.Quality
		if quality == "" {
			quality = "good"
		}
		if !floatVal.Valid && !strVal.Valid && quality == "good" {
			// A row that carries no value at all cannot be a good measurement;
			// labelling it good is what hid the dropped-value bug below.
			quality = "bad"
		}
		if _, err := stmt.Exec(p.DeviceID, p.PointName, floatVal, strVal, quality, p.Timestamp.Format(time.RFC3339Nano)); err != nil {
			logrus.WithField("device_id", p.DeviceID).
				WithField("point_name", p.PointName).
				WithError(err).
				Warn("Failed to insert point to time-series storage")
			return err
		}
	}
	return tx.Commit()
}

// splitPointValue maps a driver value onto the two storage columns.
//
// It works on reflect.Kind rather than a hand-written type list because the
// register drivers return uint16/int16/uint32 for a read: a `case float64 /
// float32 / int / int64 / int32 / bool / string` switch left every Modbus,
// S7, FINS and MC integer sample stored as value=NULL, value_str=NULL while
// quality still said "good". History for those tags was silently empty.
func splitPointValue(v interface{}) (sql.NullFloat64, sql.NullString) {
	var fv sql.NullFloat64
	var sv sql.NullString
	if v == nil {
		return fv, sv
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.Float64, fv.Valid = float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		fv.Float64, fv.Valid = float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		fv.Float64, fv.Valid = rv.Float(), true
	case reflect.Bool:
		if rv.Bool() {
			fv.Float64 = 1
		}
		fv.Valid = true
	case reflect.String:
		sv.String, sv.Valid = rv.String(), true
	default:
		// Slices, structs and []byte are stored as JSON instead of being
		// dropped: a stored text can be read back, a NULL cannot.
		if b, err := json.Marshal(v); err == nil {
			sv.String, sv.Valid = string(b), true
		}
	}
	return fv, sv
}

// tsText renders a range bound so it can be compared against stored timestamps.
// Samples are written with the server's local UTC offset, and the column is
// filtered as text (see idx_ts_device_point), so a bound that arrives as
// Zulu/other-offset — e.g. the ISO strings the UI sends for an absolute date
// range — would sort on the wrong side of every stored row and silently return
// nothing. Re-expressing the bound in the writer's zone makes the lexical
// comparison agree with chronologic order.
func tsText(t time.Time) string {
	return t.In(time.Local).Format(time.RFC3339Nano)
}

// QueryPoints queries time-series data.
func (t *TimeSeriesStorage) QueryPoints(deviceID, pointName string, startTime, endTime time.Time) ([]PointData, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	query := `SELECT device_id, point_name, value, value_str, quality, timestamp FROM time_series WHERE device_id = ?`
	args := []interface{}{deviceID}
	// Empty pointName means "all points" for the device.
	if pointName != "" {
		query += ` AND point_name = ?`
		args = append(args, pointName)
	}
	query += ` AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp ASC`
	args = append(args, tsText(startTime), tsText(endTime))
	rows, err := t.sqliteTSDB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []PointData
	for rows.Next() {
		p := PointData{}
		var floatVal sql.NullFloat64
		var strVal sql.NullString
		var tsStr string
		if err := rows.Scan(&p.DeviceID, &p.PointName, &floatVal, &strVal, &p.Quality, &tsStr); err != nil {
			return nil, err
		}
		if floatVal.Valid {
			p.Value = floatVal.Float64
		} else if strVal.Valid {
			p.Value = strVal.String
		}
		p.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		results = append(results, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// DeviceQualityRow is a per-device data-quality aggregate over a time window.
type DeviceQualityRow struct {
	DeviceID       string
	TotalRows      int64
	ValidRows      int64
	InvalidRows    int64
	DistinctPoints int64
	LastTimestamp  string
}

// QualityByDevice aggregates time-series quality counters per device since the given time.
func (t *TimeSeriesStorage) QualityByDevice(since time.Time) ([]DeviceQualityRow, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rows, err := t.sqliteTSDB.Query(`
		SELECT device_id,
		       COUNT(*) AS total,
		       SUM(CASE WHEN quality IN ('good', 'downsampled') THEN 1 ELSE 0 END) AS valid,
		       SUM(CASE WHEN quality <> 'good' AND quality <> 'downsampled' THEN 1 ELSE 0 END) AS invalid,
		       COUNT(DISTINCT point_name) AS points,
		       MAX(timestamp) AS last_ts
		FROM time_series
		WHERE timestamp >= ?
		GROUP BY device_id`, tsText(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceQualityRow
	for rows.Next() {
		var r DeviceQualityRow
		if err := rows.Scan(&r.DeviceID, &r.TotalRows, &r.ValidRows, &r.InvalidRows, &r.DistinctPoints, &r.LastTimestamp); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PointQualityRow is one point's quality counters within a time window.
type PointQualityRow struct {
	PointName     string
	TotalRows     int64
	ValidRows     int64
	InvalidRows   int64
	LastTimestamp string
}

// QualityByPoint aggregates quality counters per point for one device.
func (t *TimeSeriesStorage) QualityByPoint(deviceID string, since time.Time) ([]PointQualityRow, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rows, err := t.sqliteTSDB.Query(`
		SELECT point_name,
		       COUNT(*) AS total,
		       SUM(CASE WHEN quality IN ('good', 'downsampled') THEN 1 ELSE 0 END) AS valid,
		       SUM(CASE WHEN quality <> 'good' AND quality <> 'downsampled' THEN 1 ELSE 0 END) AS invalid,
		       MAX(timestamp) AS last_ts
		FROM time_series
		WHERE device_id = ? AND timestamp >= ?
		GROUP BY point_name
		ORDER BY point_name`, deviceID, tsText(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PointQualityRow
	for rows.Next() {
		var r PointQualityRow
		if err := rows.Scan(&r.PointName, &r.TotalRows, &r.ValidRows, &r.InvalidRows, &r.LastTimestamp); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QualityBucketRow is one hourly bucket of quality counters.
type QualityBucketRow struct {
	Bucket    string
	TotalRows int64
	ValidRows int64
}

// QualityHourlyBuckets returns quality counters bucketed by hour, oldest first,
// for one device or (deviceID empty) for the whole gateway. Stored timestamps
// share one fixed-offset local text format, so SUBSTR yields sortable buckets -
// the same trick RunDownsample uses.
func (t *TimeSeriesStorage) QualityHourlyBuckets(deviceID string, since time.Time) ([]QualityBucketRow, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	q := `SELECT SUBSTR(timestamp, 1, 13) AS bucket,
	              COUNT(*) AS total,
	              SUM(CASE WHEN quality IN ('good', 'downsampled') THEN 1 ELSE 0 END) AS valid
	       FROM time_series
	       WHERE timestamp >= ?`
	args := []interface{}{tsText(since)}
	if deviceID != "" {
		q += " AND device_id = ?"
		args = append(args, deviceID)
	}
	q += " GROUP BY bucket ORDER BY bucket"
	rows, err := t.sqliteTSDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QualityBucketRow
	for rows.Next() {
		var r QualityBucketRow
		if err := rows.Scan(&r.Bucket, &r.TotalRows, &r.ValidRows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DownsampleTierResult reports the outcome of one tier's aggregation.
type DownsampleTierResult struct {
	Tier          int   `json:"tier"`
	RowsProcessed int64 `json:"rows_processed"`
	RowsArchived  int64 `json:"rows_archived"`
}

// RunDownsample aggregates raw samples older than each tier age into bucket
// averages: tier3 → 1-day buckets, tier2 → 1-hour, tier1 → 1-minute.
// Stored timestamps share one fixed-offset local text format, so bucket
// boundaries can be derived with SUBSTR while preserving lexical order.
func (t *TimeSeriesStorage) RunDownsample(tier1AgeDays, tier2AgeDays, tier3AgeDays int) ([]DownsampleTierResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	offset := now.Format("-07:00")
	tiers := []struct {
		tier    int
		ageDays int
		keepLen int
		suffix  string
	}{
		{3, tier3AgeDays, 10, "T00:00:00" + offset},
		{2, tier2AgeDays, 13, ":00:00" + offset},
		{1, tier1AgeDays, 16, ":00" + offset},
	}
	var out []DownsampleTierResult
	for _, tier := range tiers {
		if tier.ageDays <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -tier.ageDays).Format(time.RFC3339Nano)
		bucketExpr := fmt.Sprintf("SUBSTR(timestamp, 1, %d) || '%s'", tier.keepLen, tier.suffix)
		res, err := t.sqliteTSDB.Exec(fmt.Sprintf(`
			INSERT INTO time_series (device_id, point_name, value, value_str, quality, timestamp)
			SELECT device_id, point_name, AVG(value), MAX(value_str), 'downsampled', %s
			FROM time_series
			WHERE timestamp < ? AND quality <> 'downsampled'
			GROUP BY device_id, point_name, %s`, bucketExpr, bucketExpr), cutoff)
		if err != nil {
			return out, err
		}
		archived, _ := res.RowsAffected()
		res2, err := t.sqliteTSDB.Exec(`DELETE FROM time_series WHERE timestamp < ? AND quality <> 'downsampled'`, cutoff)
		if err != nil {
			return out, err
		}
		processed, _ := res2.RowsAffected()
		out = append(out, DownsampleTierResult{Tier: tier.tier, RowsProcessed: processed, RowsArchived: archived})
	}
	return out, nil
}

// DownsampleCounts returns total and already-downsampled row counts.
func (t *TimeSeriesStorage) DownsampleCounts() (total int64, downsampled int64, err error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	err = t.sqliteTSDB.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN quality = 'downsampled' THEN 1 ELSE 0 END), 0) FROM time_series`,
	).Scan(&total, &downsampled)
	return
}

// GetLatestPoints returns the latest value for each point of a device.
//
// The latest row per point is picked inside the device's own id range. The
// previous shape tested `id = (SELECT MAX(id) ... WHERE device_id = t1.device_id
// AND point_name = t1.point_name)` against every row of time_series, and SQLite
// kept it as a correlated subquery: 60s on a 120k-row store, during which this
// RLock blocked WritePoints, so one read of a single device stalled collection
// gateway-wide until the HTTP timeout killed the request.
func (t *TimeSeriesStorage) GetLatestPoints(deviceID string) (map[string]PointData, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rows, err := t.sqliteTSDB.Query(`
		SELECT device_id, point_name, value, value_str, quality, timestamp FROM time_series
		WHERE device_id = ?
		AND id IN (SELECT MAX(id) FROM time_series WHERE device_id = ? GROUP BY point_name)`,
		deviceID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make(map[string]PointData)
	for rows.Next() {
		p := PointData{}
		var floatVal sql.NullFloat64
		var strVal sql.NullString
		var tsStr string
		if err := rows.Scan(&p.DeviceID, &p.PointName, &floatVal, &strVal, &p.Quality, &tsStr); err != nil {
			return nil, err
		}
		if floatVal.Valid {
			p.Value = floatVal.Float64
		} else if strVal.Valid {
			p.Value = strVal.String
		}
		p.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		results[p.PointName] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Close closes the storage.
// SetMaxOpenConns(0) before Close() to release all pooled connections
// and ensure Windows file handles are freed promptly.
func (t *TimeSeriesStorage) Close() error {
	if t.sqliteTSDB != nil {
		t.sqliteTSDB.SetMaxOpenConns(0)
		t.sqliteTSDB.SetMaxIdleConns(0)
		return t.sqliteTSDB.Close()
	}
	return nil
}

// IsUsingFallback returns true if using SQLite fallback.
func (t *TimeSeriesStorage) IsUsingFallback() bool {
	return t.fallback
}

// CheckHealth checks if the storage is healthy.
func (t *TimeSeriesStorage) CheckHealth() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.sqliteTSDB == nil {
		return false
	}
	// Actually ping the database to verify connectivity
	if err := t.sqliteTSDB.Ping(); err != nil {
		logrus.WithError(err).Warn("TimeSeriesStorage health check failed")
		return false
	}
	return true
}

// CacheManager manages an in-memory ring buffer cache for data points.
type CacheManager struct {
	mu      sync.RWMutex
	buffer  []PointData
	maxSize int
	head    int
	tail    int
	count   int
}

// NewCacheManager creates a new CacheManager.
func NewCacheManager(capacity int) *CacheManager {
	if capacity <= 0 {
		capacity = constants.CacheMaxSize
	}
	return &CacheManager{
		buffer:  make([]PointData, capacity),
		maxSize: capacity,
	}
}

// Push adds a data point to the ring buffer.
func (c *CacheManager) Push(p PointData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer[c.head] = p
	c.head = (c.head + 1) % c.maxSize
	if c.count == c.maxSize {
		c.tail = (c.tail + 1) % c.maxSize
	} else {
		c.count++
	}
}

// GetAll returns all cached points.
func (c *CacheManager) GetAll() []PointData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]PointData, 0, c.count)
	idx := c.tail
	for i := 0; i < c.count; i++ {
		result = append(result, c.buffer[idx])
		idx = (idx + 1) % c.maxSize
	}
	return result
}

// Count returns the number of cached points.
func (c *CacheManager) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.count
}

// Clear empties the cache.
func (c *CacheManager) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head = 0
	c.tail = 0
	c.count = 0
}

// OfflineQueue manages offline data persistence with SQLite.
//
// Rows live only as long as they are undelivered: a message is claimed
// (status 'in_flight'), then deleted on Ack or returned to 'pending' on Nack.
// Delivered rows used to stay in the table forever with status 'sent', so an
// MQTT outage grew the file without bound (the config's max_queue_size was only
// ever read back to draw a watermark, never enforced), and messages were marked
// delivered before the publish was attempted, so a failed publish lost them.
type OfflineQueue struct {
	mu         sync.Mutex
	db         *sql.DB
	path       string
	maxRows    int
	maxRetries int
	delivered  int64
	dropped    int64
}

// OfflineItem is one claimed queue entry.
type OfflineItem struct {
	ID      int64
	Payload string
	Retries int
}

// NewOfflineQueue creates a new OfflineQueue.
func NewOfflineQueue(path string) (*OfflineQueue, error) {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open offline queue: %w", err)
	}
	// Configure connection pool for offline queue
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(30 * time.Minute)

	q := &OfflineQueue{db: db, path: path}
	mqttCfg := config.GetConfig().MQTT
	q.maxRows = mqttCfg.MaxQueueSize
	if q.maxRows <= 0 {
		q.maxRows = constants.MQTTQueueMaxSize
	}
	q.maxRetries = mqttCfg.MaxRetries
	if q.maxRetries <= 0 {
		q.maxRetries = constants.MQTTOfflineMaxRetries
	}
	if err := q.initTables(); err != nil {
		db.Close()
		return nil, err
	}
	return q, nil
}

func (q *OfflineQueue) initTables() error {
	_, err := q.db.Exec(`CREATE TABLE IF NOT EXISTS offline_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		payload TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		retries INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending'
	)`)
	if err != nil {
		return err
	}
	if _, err = q.db.Exec("CREATE INDEX IF NOT EXISTS idx_offline_queue_status_id ON offline_queue(status, id ASC)"); err != nil {
		return err
	}
	// A crash between claim and delivery leaves rows 'in_flight' that nothing
	// would ever pick up again.
	_, err = q.db.Exec("UPDATE offline_queue SET status = 'pending' WHERE status = 'in_flight'")
	if err != nil {
		return err
	}
	// Databases written by the previous implementation left rows behind with
	// status 'sent'; Dequeue only ever selects 'pending', so those rows are
	// unreachable and could never be delivered again. Reclaim them at open
	// instead of letting them hold disk until trim counts their deletion as a
	// drop, and tell the operator how much was freed.
	var legacy int64
	if err := q.db.QueryRow("SELECT COUNT(*) FROM offline_queue WHERE status = 'sent'").Scan(&legacy); err != nil {
		return err
	}
	if legacy > 0 {
		if _, err := q.db.Exec("DELETE FROM offline_queue WHERE status = 'sent'"); err != nil {
			return err
		}
		logrus.WithField("path", q.path).
			WithField("rows", legacy).
			Warn("Removed unreachable offline queue rows left by an older version")
	}
	return nil
}

// Enqueue adds an item to the offline queue, dropping the oldest undelivered
// rows once the configured backlog limit is exceeded: a gateway that keeps
// sampling through a long outage must not fill its own disk.
func (q *OfflineQueue) Enqueue(payload interface{}) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := q.db.Exec("INSERT INTO offline_queue (payload) VALUES (?)", string(data)); err != nil {
		return err
	}
	return q.trimLocked()
}

// trimLocked enforces maxRows over the undelivered backlog.
func (q *OfflineQueue) trimLocked() error {
	var n int
	if err := q.db.QueryRow("SELECT COUNT(*) FROM offline_queue").Scan(&n); err != nil {
		return err
	}
	over := n - q.maxRows
	if over <= 0 {
		return nil
	}
	res, err := q.db.Exec(`DELETE FROM offline_queue WHERE id IN
		(SELECT id FROM offline_queue ORDER BY id ASC LIMIT ?)`, over)
	if err != nil {
		return err
	}
	if dropped, err := res.RowsAffected(); err == nil && dropped > 0 {
		q.dropped += dropped
		logrus.WithField("dropped", dropped).
			WithField("max_queue_size", q.maxRows).
			Warn("Offline queue backlog limit reached, oldest undelivered rows dropped")
	}
	return nil
}

// Dequeue claims the oldest undelivered item without deleting it: the caller
// must Ack after a successful publish, or Nack to have it retried.
func (q *OfflineQueue) Dequeue() (*OfflineItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var item OfflineItem
	err = tx.QueryRow(`SELECT id, payload, retries FROM offline_queue
		WHERE status = 'pending' ORDER BY id ASC LIMIT 1`).Scan(&item.ID, &item.Payload, &item.Retries)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE offline_queue SET status = 'in_flight', retries = retries + 1 WHERE id = ?", item.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	item.Retries++
	return &item, nil
}

// Ack removes a claimed item after it has been delivered.
func (q *OfflineQueue) Ack(id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec("DELETE FROM offline_queue WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil {
		q.delivered += n
	}
	return nil
}

// Nack returns a claimed item to the queue, or drops it once it has exhausted
// its retries so one poison message cannot block the backlog forever.
func (q *OfflineQueue) Nack(id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(`DELETE FROM offline_queue WHERE id = ? AND retries >= ?`, id, q.maxRetries)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		q.dropped += n
		logrus.WithField("id", id).WithField("max_retries", q.maxRetries).
			Warn("Offline queue item exceeded retry limit and was dropped")
		return nil
	}
	_, err = q.db.Exec("UPDATE offline_queue SET status = 'pending' WHERE id = ?", id)
	return err
}

// Stats returns the undelivered backlog plus lifetime delivered/dropped counts.
func (q *OfflineQueue) Stats() (pending, sent int64, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err = q.db.QueryRow(`SELECT COUNT(*) FROM offline_queue WHERE status IN ('pending', 'in_flight')`).Scan(&pending); err != nil {
		return 0, 0, err
	}
	return pending, q.delivered, nil
}

// Purge discards the whole undelivered backlog and reports how many items were
// removed. The "clear queue" control needs this: reporting success without it
// left the operator watching a backlog that kept growing.
func (q *OfflineQueue) Purge() (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(`DELETE FROM offline_queue WHERE status IN ('pending', 'in_flight')`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	q.dropped += n
	return n, nil
}

// Dropped returns how many items were discarded by the backlog limit or the
// retry cap, so a status endpoint can stop reporting a lossless-looking queue.
func (q *OfflineQueue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Close closes the offline queue database.
func (q *OfflineQueue) Close() error {
	q.db.SetMaxOpenConns(0)
	q.db.SetMaxIdleConns(0)
	return q.db.Close()
}

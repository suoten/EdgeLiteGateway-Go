package drivers

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// OfflineSyncManager manages offline data synchronization.
// When the gateway loses connection to the cloud, data is buffered locally
// and synced when connectivity is restored.
type OfflineSyncManager struct {
	mu             sync.Mutex
	online         int32
	queue          []map[string]interface{}
	maxQueueSize   int
	syncInterval   time.Duration
	uploadCallback func(data []map[string]interface{}) (int, error)
	lastSyncTime   *time.Time
	// 使用 atomic.Int64 自带 8 字节对齐，避免 linux/arm 32 位下原子操作 panic
	totalSynced atomic.Int64
	totalFailed atomic.Int64
	cancelFn    context.CancelFunc
}

// NewOfflineSyncManager creates a new OfflineSyncManager.
func NewOfflineSyncManager(maxQueueSize int, syncIntervalSec int) *OfflineSyncManager {
	if maxQueueSize <= 0 {
		maxQueueSize = 10000
	}
	if syncIntervalSec <= 0 {
		syncIntervalSec = 30
	}
	return &OfflineSyncManager{
		queue:        make([]map[string]interface{}, 0, maxQueueSize),
		maxQueueSize: maxQueueSize,
		syncInterval: time.Duration(syncIntervalSec) * time.Second,
	}
}

// Start starts the sync loop.
func (m *OfflineSyncManager) Start(ctx context.Context) {
	childCtx, cancel := context.WithCancel(ctx)
	m.cancelFn = cancel
	go m.syncLoop(childCtx)
}

// Stop stops the sync loop.
func (m *OfflineSyncManager) Stop() {
	if m.cancelFn != nil {
		m.cancelFn()
	}
}

// SetOnline sets the online status.
func (m *OfflineSyncManager) SetOnline(online bool) {
	if online {
		atomic.StoreInt32(&m.online, 1)
	} else {
		atomic.StoreInt32(&m.online, 0)
	}
}

// IsOnline returns whether the system is online.
func (m *OfflineSyncManager) IsOnline() bool {
	return atomic.LoadInt32(&m.online) == 1
}

// SetUploadCallback sets the callback for uploading synced data.
func (m *OfflineSyncManager) SetUploadCallback(cb func(data []map[string]interface{}) (int, error)) {
	m.uploadCallback = cb
}

// Enqueue adds data to the offline queue.
func (m *OfflineSyncManager) Enqueue(data map[string]interface{}) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) >= m.maxQueueSize {
		// Drop oldest entry
		m.queue = m.queue[1:]
	}
	m.queue = append(m.queue, data)
	return true
}

// ForceSync forces an immediate sync attempt.
func (m *OfflineSyncManager) ForceSync() int {
	return m.doSync()
}

func (m *OfflineSyncManager) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(m.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.IsOnline() && len(m.queue) > 0 {
				m.doSync()
			}
		}
	}
}

func (m *OfflineSyncManager) doSync() int {
	if m.uploadCallback == nil {
		return 0
	}
	m.mu.Lock()
	if len(m.queue) == 0 {
		m.mu.Unlock()
		return 0
	}
	data := make([]map[string]interface{}, len(m.queue))
	copy(data, m.queue)
	m.queue = m.queue[:0]
	m.mu.Unlock()

	count, err := m.uploadCallback(data)
	now := time.Now()
	m.mu.Lock()
	m.lastSyncTime = &now
	m.mu.Unlock()

	if err != nil {
		m.totalFailed.Add(int64(len(data)))
		// Re-enqueue data on failure
		m.mu.Lock()
		// Prepend data back
		newQueue := make([]map[string]interface{}, 0, len(data)+len(m.queue))
		newQueue = append(newQueue, data...)
		newQueue = append(newQueue, m.queue...)
		if len(newQueue) > m.maxQueueSize {
			newQueue = newQueue[len(newQueue)-m.maxQueueSize:]
		}
		m.queue = newQueue
		m.mu.Unlock()
		logrus.WithError(err).Error("Offline sync failed, data re-queued")
		return 0
	}

	m.totalSynced.Add(int64(count))
	logrus.Infof("Offline sync: uploaded %d/%d records", count, len(data))
	return count
}

// GetStats returns statistics about the offline sync.
func (m *OfflineSyncManager) GetStats() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lastSync interface{}
	if m.lastSyncTime != nil {
		lastSync = m.lastSyncTime.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"online":         m.IsOnline(),
		"queue_size":     len(m.queue),
		"max_queue_size": m.maxQueueSize,
		"total_synced":   m.totalSynced.Load(),
		"total_failed":   m.totalFailed.Load(),
		"last_sync_time": lastSync,
	}
}

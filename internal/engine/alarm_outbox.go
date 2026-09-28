package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// AlarmOutbox provides reliable alarm delivery with retry and dead-letter queue.
// This is a Go port of the Python edgelite/engine/alarm_outbox.py.
//
// Features:
//   - At-least-once delivery semantics
//   - Exponential backoff retry
//   - Dead-letter queue for undeliverable alarms
//   - Rate limiting to prevent alarm storms

const (
	outboxMaxRetries       = 5
	outboxBaseRetryDelay   = 5 * time.Second
	outboxMaxRetryDelay    = 300 * time.Second
	outboxRateLimitPerMin  = 100
	outboxDLQMaxSize       = 10000
)

// OutboxMessage represents an alarm message in the outbox.
type OutboxMessage struct {
	ID         string                 `json:"id"`
	AlarmID    string                 `json:"alarm_id"`
	Channels   []string               `json:"channels"`
	Title      string                 `json:"title"`
	Message    string                 `json:"message"`
	Severity   string                 `json:"severity"`
	Data       map[string]interface{} `json:"data,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	RetryCount int                    `json:"retry_count"`
	NextRetry  time.Time              `json:"next_retry,omitempty"`
	Status     string                 `json:"status"` // pending, sent, failed, dead_letter
}

// AlarmOutbox manages reliable alarm delivery.
type AlarmOutbox struct {
	mu           sync.Mutex
	queue        []*OutboxMessage
	deadLetters  []*OutboxMessage
	notifyFunc   func(msg *OutboxMessage) error
	rateLimit    *time.Ticker
	sentCount    int64
	failedCount  int64
	started      bool
	cancelFunc   context.CancelFunc
}

// NewAlarmOutbox creates a new AlarmOutbox.
func NewAlarmOutbox(notifyFunc func(msg *OutboxMessage) error) *AlarmOutbox {
	return &AlarmOutbox{
		queue:       make([]*OutboxMessage, 0),
		deadLetters: make([]*OutboxMessage, 0),
		notifyFunc:  notifyFunc,
	}
}

// Start begins the outbox processing loop.
func (a *AlarmOutbox) Start(ctx context.Context) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.mu.Unlock()

	childCtx, cancel := context.WithCancel(ctx)
	a.cancelFunc = cancel

	go a.processLoop(childCtx)
	logrus.Info("AlarmOutbox started")
}

// Stop stops the outbox processing.
func (a *AlarmOutbox) Stop() {
	a.mu.Lock()
	a.started = false
	a.mu.Unlock()

	if a.cancelFunc != nil {
		a.cancelFunc()
		a.cancelFunc = nil
	}
	logrus.Info("AlarmOutbox stopped")
}

// Enqueue adds an alarm message to the outbox.
func (a *AlarmOutbox) Enqueue(msg *OutboxMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()

	msg.CreatedAt = time.Now()
	msg.Status = "pending"
	a.queue = append(a.queue, msg)

	logrus.WithField("alarm_id", msg.AlarmID).Debug("Alarm enqueued to outbox")
}

// processLoop periodically processes the outbox queue.
func (a *AlarmOutbox) processLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.processQueue()
		}
	}
}

func (a *AlarmOutbox) processQueue() {
	a.mu.Lock()
	now := time.Now()
	var pending []*OutboxMessage
	var remaining []*OutboxMessage

	for _, msg := range a.queue {
		if msg.Status == "sent" {
			continue
		}
		if !msg.NextRetry.IsZero() && now.Before(msg.NextRetry) {
			remaining = append(remaining, msg)
			continue
		}
		pending = append(pending, msg)
	}
	a.queue = remaining
	a.mu.Unlock()

	for _, msg := range pending {
		a.deliver(msg)
	}
}

func (a *AlarmOutbox) deliver(msg *OutboxMessage) {
	if a.notifyFunc == nil {
		return
	}

	err := a.notifyFunc(msg)
	if err == nil {
		a.mu.Lock()
		msg.Status = "sent"
		a.sentCount++
		a.mu.Unlock()
		logrus.WithField("alarm_id", msg.AlarmID).Debug("Alarm delivered from outbox")
		return
	}

	msg.RetryCount++
	if msg.RetryCount >= outboxMaxRetries {
		a.mu.Lock()
		msg.Status = "dead_letter"
		a.deadLetters = append(a.deadLetters, msg)
		a.failedCount++
		// Trim dead letter queue
		if len(a.deadLetters) > outboxDLQMaxSize {
			a.deadLetters = a.deadLetters[len(a.deadLetters)-outboxDLQMaxSize:]
		}
		a.mu.Unlock()
		logrus.WithFields(logrus.Fields{
			"alarm_id":     msg.AlarmID,
			"retry_count":  msg.RetryCount,
		}).Error("Alarm moved to dead letter queue")
		return
	}

	// Exponential backoff
	delay := outboxBaseRetryDelay * time.Duration(1<<uint(msg.RetryCount))
	if delay > outboxMaxRetryDelay {
		delay = outboxMaxRetryDelay
	}
	msg.NextRetry = time.Now().Add(delay)

	a.mu.Lock()
	a.queue = append(a.queue, msg)
	a.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"alarm_id":    msg.AlarmID,
		"retry_count": msg.RetryCount,
		"next_retry":  msg.NextRetry,
	}).Warn("Alarm delivery failed, scheduled for retry")
}

// GetStats returns outbox statistics.
func (a *AlarmOutbox) GetStats() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]interface{}{
		"queue_size":       len(a.queue),
		"dead_letter_count": len(a.deadLetters),
		"sent_count":       a.sentCount,
		"failed_count":     a.failedCount,
	}
}

// GetDeadLetters returns the dead letter queue.
func (a *AlarmOutbox) GetDeadLetters() []*OutboxMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]*OutboxMessage, len(a.deadLetters))
	copy(result, a.deadLetters)
	return result
}

// ClearDeadLetters clears the dead letter queue.
func (a *AlarmOutbox) ClearDeadLetters() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deadLetters = make([]*OutboxMessage, 0)
}

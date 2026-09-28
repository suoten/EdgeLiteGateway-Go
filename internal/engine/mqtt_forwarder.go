package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	paho "github.com/eclipse/paho.mqtt.golang"

	"edgelite/internal/config"
	"edgelite/internal/constants"
	"edgelite/internal/storage"
)

// MQTTForwarder forwards collected data points to an external MQTT broker.
// It supports offline queueing, retry logic, and automatic reconnection.
type MQTTForwarder struct {
	mu              sync.Mutex
	cfg             *config.MQTTConfig
	eventBus        *EventBus
	offlineQueue    *storage.OfflineQueue
	cache           *storage.CacheManager

	// Connection state
	connected      bool
	connecting     bool
	lastConnectAt  time.Time
	lastError      string
	mqttClient     paho.Client

	// Publishing queue
	publishQueue   chan mqttMessage
	wg             sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
	started        bool
	unsubscribe    func()

	// Statistics
	muStats        sync.Mutex
	totalPublished int64
	totalQueued    int64
	totalFailed    int64
	totalRetried   int64
	lastFlushAt    time.Time
}

// mqttMessage represents a message to be published to MQTT.
type mqttMessage struct {
	Topic   string
	Payload []byte
	QoS     int
}

// NewMQTTForwarder creates a new MQTTForwarder.
func NewMQTTForwarder(cfg *config.MQTTConfig, eventBus *EventBus, offlineQueue *storage.OfflineQueue, cache *storage.CacheManager) *MQTTForwarder {
	return &MQTTForwarder{
		cfg:          cfg,
		eventBus:    eventBus,
		offlineQueue: offlineQueue,
		cache:       cache,
		publishQueue: make(chan mqttMessage, cfg.MaxQueueSize),
	}
}

// Start starts the MQTT forwarder.
func (m *MQTTForwarder) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return fmt.Errorf("MQTT forwarder already started")
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.started = true
	m.mu.Unlock()

	// Subscribe to data collected events
	if m.eventBus != nil {
		m.unsubscribe = m.eventBus.Subscribe(EventTypeDataCollected, m.onDataCollected)
	}

	// Start publisher goroutine
	m.wg.Add(1)
	go m.publishLoop()

	// Start reconnection loop
	m.wg.Add(1)
	go m.reconnectLoop()

	// Start offline queue flusher
	m.wg.Add(1)
	go m.offlineFlushLoop()

	logrus.Info("MQTTForwarder started")
	return nil
}

// Stop stops the MQTT forwarder.
func (m *MQTTForwarder) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = false
	if m.cancel != nil {
		m.cancel()
	}
	if m.unsubscribe != nil {
		m.unsubscribe()
		m.unsubscribe = nil
	}
	if m.mqttClient != nil {
		m.mqttClient.Disconnect(500) // 500ms graceful disconnect
		m.mqttClient = nil
	}
	m.mu.Unlock()

	m.wg.Wait()

	// Close the offline queue database after all goroutines have stopped
	// to ensure no concurrent access.
	if m.offlineQueue != nil {
		if err := m.offlineQueue.Close(); err != nil {
			logrus.WithError(err).Warn("Failed to close offline queue database")
		}
	}

	logrus.Info("MQTTForwarder stopped")
	return nil
}

// onDataCollected handles data collected events by forwarding to MQTT.
func (m *MQTTForwarder) onDataCollected(event Event) {
	data, ok := event.Data.(DataCollectedEvent)
	if !ok {
		return
	}

	// Build MQTT topic and payload
	topic := fmt.Sprintf("%s/devices/%s/data", m.cfg.TopicPrefix, data.DeviceID)

	payload, err := json.Marshal(map[string]interface{}{
		"device_id": data.DeviceID,
		"points":    data.Points,
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"source":    data.Source,
	})
	if err != nil {
		logrus.WithField("device_id", data.DeviceID).
			WithField("error", err.Error()).
			Error("Failed to marshal MQTT payload")
		return
	}

	msg := mqttMessage{
		Topic:   topic,
		Payload: payload,
		QoS:     1,
	}

	select {
	case m.publishQueue <- msg:
		m.muStats.Lock()
		m.totalQueued++
		m.muStats.Unlock()
	default:
		// Queue full, enqueue to offline storage
		if m.offlineQueue != nil {
			if err := m.offlineQueue.Enqueue(msg); err == nil {
				m.muStats.Lock()
				m.totalQueued++
				m.muStats.Unlock()
				return
			}
		}
		m.muStats.Lock()
		m.totalFailed++
		m.muStats.Unlock()
		logrus.WithField("device_id", data.DeviceID).
			Warn("MQTT publish queue full, message dropped")
	}
}

// publishLoop reads from the publish queue and sends messages.
func (m *MQTTForwarder) publishLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case msg := <-m.publishQueue:
			m.publish(msg)
		}
	}
}

// errMQTTNotConnected distinguishes "broker is down" from a broker-side reject.
var errMQTTNotConnected = errors.New("mqtt client is not connected")

// publishNow sends one message and reports the outcome without touching the
// counters, so the live path and the offline flush can each decide what to do
// with a failure.
func (m *MQTTForwarder) publishNow(msg mqttMessage) error {
	m.mu.Lock()
	connected := m.connected
	client := m.mqttClient
	m.mu.Unlock()

	if !connected || client == nil || !client.IsConnected() {
		return errMQTTNotConnected
	}
	token := client.Publish(msg.Topic, byte(msg.QoS), false, msg.Payload)
	token.Wait()
	return token.Error()
}

// publish sends a single live message. A message the broker did not accept goes
// to the offline queue for another attempt: it used to be counted as failed and
// dropped, so every publish error silently cost the northbound one sample.
//
// Lock ordering: muStats may be acquired independently (never nested with mu)
// to avoid AB-BA deadlock with GetStats which acquires mu then muStats.
func (m *MQTTForwarder) publish(msg mqttMessage) {
	err := m.publishNow(msg)
	if err == nil {
		m.muStats.Lock()
		m.totalPublished++
		m.lastFlushAt = time.Now()
		m.muStats.Unlock()
		return
	}

	if m.offlineQueue != nil {
		if enqueueErr := m.offlineQueue.Enqueue(msg); enqueueErr == nil {
			logrus.WithField("topic", msg.Topic).WithError(err).
				Warn("MQTT publish failed, message queued for retry")
			return
		}
	}
	m.muStats.Lock()
	m.totalFailed++
	m.muStats.Unlock()
	logrus.WithField("topic", msg.Topic).WithError(err).Warn("MQTT publish failed")
}

// reconnectLoop attempts to reconnect to the MQTT broker with exponential backoff.
// 无 broker（如边缘网关未配置 MQTT）时固定 5s 重试会刷屏日志，失败后延迟翻倍
// （上限 5 分钟），连接成功后复位为基础间隔。
func (m *MQTTForwarder) reconnectLoop() {
	defer m.wg.Done()
	baseDelay := time.Duration(constants.MQTTReconnectDelay) * time.Second
	const maxDelay = 5 * time.Minute
	delay := baseDelay

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(delay):
		}
		if m.IsConnected() {
			delay = baseDelay
			continue
		}
		if m.tryConnect() {
			delay = baseDelay
		} else {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// tryConnect attempts to connect to the MQTT broker.
// 返回是否成功建立连接，供重连循环做退避决策。
func (m *MQTTForwarder) tryConnect() bool {
	m.mu.Lock()
	if m.connecting {
		m.mu.Unlock()
		return m.connected
	}
	m.connecting = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.connecting = false
		m.mu.Unlock()
	}()

	if m.cfg.Broker == "" {
		return false
	}

	// Create MQTT client options
	brokerURL := m.cfg.Broker
	if m.cfg.Port > 0 {
		brokerURL = fmt.Sprintf("%s:%d", m.cfg.Broker, m.cfg.Port)
	}
	opts := paho.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(fmt.Sprintf("edgelite-%s", uuid.New().String()[:8]))
	if m.cfg.Username != "" {
		opts.SetUsername(m.cfg.Username)
		opts.SetPassword(m.cfg.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetCleanSession(true)

	// Set connection handlers
	opts.OnConnect = func(client paho.Client) {
		m.mu.Lock()
		m.connected = true
		m.lastConnectAt = time.Now()
		m.lastError = ""
		m.mu.Unlock()

		logrus.WithField("broker", m.cfg.Broker).Info("MQTT connected")

		if m.eventBus != nil {
			m.eventBus.Publish(Event{
				Type:   EventTypeMQTTConnected,
				Source: "mqtt_forwarder",
				Data:   map[string]interface{}{"broker": m.cfg.Broker},
			})
		}

		// Flush offline queue
		if m.offlineQueue != nil {
			go m.flushOffline()
		}
	}

	opts.OnConnectionLost = func(client paho.Client, err error) {
		m.mu.Lock()
		m.connected = false
		m.lastError = err.Error()
		m.mu.Unlock()

		logrus.WithError(err).WithField("broker", m.cfg.Broker).Warn("MQTT connection lost")
	}

	client := paho.NewClient(opts)
	token := client.Connect()
	token.Wait()

	m.mu.Lock()
	m.lastConnectAt = time.Now()
	if token.Error() != nil {
		m.connected = false
		m.lastError = token.Error().Error()
		m.mu.Unlock()
		logrus.WithError(token.Error()).WithField("broker", m.cfg.Broker).Warn("MQTT connect failed")
		return false
	}
	m.mqttClient = client
	m.connected = client.IsConnected()
	m.mu.Unlock()
	return true
}

// offlineFlushLoop periodically flushes the offline queue when reconnected.
func (m *MQTTForwarder) offlineFlushLoop() {
	defer m.wg.Done()
	interval := time.Duration(constants.MQTTQueuePollInterval * float64(time.Second))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.IsConnected() || m.offlineQueue == nil {
				continue
			}
			m.flushOffline()
		}
	}
}

// flushOffline drains the offline queue and publishes messages.
// It processes at most a bounded number of messages per invocation to prevent
// unbounded loops that could starve other goroutines. Each row is only deleted
// after the broker accepted it, so a failed publish leaves the sample queued for
// the next cycle instead of losing it.
func (m *MQTTForwarder) flushOffline() {
	const maxFlushPerCycle = 500
	for flushed := 0; flushed < maxFlushPerCycle; flushed++ {
		item, err := m.offlineQueue.Dequeue()
		if err != nil {
			logrus.WithError(err).Warn("Offline queue claim failed")
			return
		}
		if item == nil {
			return
		}

		var msg mqttMessage
		if err := json.Unmarshal([]byte(item.Payload), &msg); err != nil {
			m.muStats.Lock()
			m.totalFailed++
			m.muStats.Unlock()
			// Undeliverable as MQTT metadata; remove it so it cannot starve the queue.
			if ackErr := m.offlineQueue.Ack(item.ID); ackErr != nil {
				logrus.WithError(ackErr).Warn("Offline queue corrupt item cleanup failed")
			}
			continue
		}

		m.muStats.Lock()
		m.totalRetried++
		m.muStats.Unlock()

		if err := m.publishNow(msg); err != nil {
			if nackErr := m.offlineQueue.Nack(item.ID); nackErr != nil {
				logrus.WithError(nackErr).Warn("Offline queue retry scheduling failed")
			}
			if errors.Is(err, errMQTTNotConnected) {
				return
			}
			logrus.WithField("topic", msg.Topic).WithError(err).
				Warn("Offline MQTT publish failed, will retry")
			return
		}
		if ackErr := m.offlineQueue.Ack(item.ID); ackErr != nil {
			logrus.WithError(ackErr).Warn("Offline queue acknowledgement failed")
		}
	}
}

// IsConnected returns whether the MQTT broker is connected.
func (m *MQTTForwarder) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// IsConfigured reports whether an MQTT broker is configured.
func (m *MQTTForwarder) IsConfigured() bool {
	return m.cfg.Broker != ""
}

// IsStarted reports whether the forwarder's loops are running, which is not the
// same as being connected: a configured-but-down broker leaves a started
// forwarder retrying, and a caller must not restart a forwarder the operator
// deliberately stopped.
func (m *MQTTForwarder) IsStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// OfflineQueueStats returns pending/sent counts from the persistent offline queue.
func (m *MQTTForwarder) OfflineQueueStats() (pending, sent int64, err error) {
	if m.offlineQueue == nil {
		return 0, 0, nil
	}
	return m.offlineQueue.Stats()
}

// ClearOfflineQueue discards the persisted backlog and reports how many items
// were removed. An absent queue is an error rather than "cleared 0", because
// the caller asked for data loss it cannot get.
func (m *MQTTForwarder) ClearOfflineQueue() (int64, error) {
	if m.offlineQueue == nil {
		return 0, fmt.Errorf("offline queue is not configured")
	}
	return m.offlineQueue.Purge()
}

// GetStats returns forwarder statistics.
func (m *MQTTForwarder) GetStats() map[string]interface{} {
	// Lock ordering: always acquire mu before muStats to prevent AB-BA deadlock.
	// In publish(), mu is acquired first then muStats — we must follow the same order here.
	m.mu.Lock()
	connected := m.connected
	lastError := m.lastError
	m.mu.Unlock()

	m.muStats.Lock()
	defer m.muStats.Unlock()
	return map[string]interface{}{
		"connected":       connected,
		"last_error":      lastError,
		"total_published": m.totalPublished,
		"total_queued":    m.totalQueued,
		"total_failed":    m.totalFailed,
		"total_retried":   m.totalRetried,
		"queue_length":    len(m.publishQueue),
		"queue_capacity":  cap(m.publishQueue),
		"last_flush_at":   m.lastFlushAt.Format(time.RFC3339),
	}
}

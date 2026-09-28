// Package ws provides WebSocket real-time communication for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/ws/ package.
// It includes a connection manager and channel-based message routing.
package ws

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// generateClientID generates a unique client ID using UUID v4.
func generateClientID() string {
	return "ws-" + uuid.New().String()
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// In dev mode, allow all origins for convenience.
		// Use config.IsDevMode() (accepts DEV_MODE=true/1 case-insensitively) instead of
		// re-reading the env var here — the duplicated local check drifted from it.
		if config.IsDevMode() {
			return true
		}
		// In production, check Origin header against the Host
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // Non-browser clients (e.g., curl, API tools)
		}
		// Security: use strict origin matching instead of strings.Contains
		// to prevent subdomain bypass attacks (e.g., evil.com.attacker.com).
		// Parse the origin URL and compare scheme+host+port with the request Host.
		parsedOrigin, err := url.Parse(origin)
		if err != nil {
			return false
		}
		originHost := parsedOrigin.Hostname()
		if originHost == "" {
			return false
		}
		// Compare origin hostname with request host (port-aware)
		reqHost := r.Host
		// Strip port from request host for comparison if present
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
		}
		// loopback 别名等价：前端 dev server (localhost:3000) 经 vite 代理后
		// Host 被改写为 127.0.0.1:8080，严格字符串比较会拒绝合法的回环来源（403）
		if isLoopbackHost(originHost) && isLoopbackHost(reqHost) {
			return true
		}
		return originHost == reqHost || parsedOrigin.Host == r.Host
	},
}

// isLoopbackHost reports whether host is a loopback name/address
// (localhost, 127.x.x.x, ::1), which are equivalent for origin checks.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ChannelType represents a WebSocket channel.
type ChannelType string

const (
	ChannelRealtime    ChannelType = "realtime"
	ChannelAlarm       ChannelType = "alarm"
	ChannelDevice      ChannelType = "device"
	ChannelIntegration ChannelType = "integration"
	ChannelAI          ChannelType = "ai"
)

// Client represents a connected WebSocket client.
type Client struct {
	ID       string
	conn     *websocket.Conn
	channels map[ChannelType]bool
	send     chan []byte
	mu       sync.Mutex
	closed   bool
	// dropped counts frames discarded because send was full. Without it a
	// laggy tab lost alarms silently and the UI still looked connected.
	dropped atomic.Uint64
}

// noteDrop records one discarded frame for this client and the manager.
func (c *Client) noteDrop(m *Manager, channel ChannelType, messageType string) {
	n := c.dropped.Add(1)
	m.dropped.Add(1)
	if n == 1 {
		logrus.WithField("client_id", c.ID).
			WithField("channel", string(channel)).
			WithField("type", messageType).
			Warn("WebSocket send buffer full, frames are being dropped; the client will miss updates until it drains")
	}
}

// Manager manages WebSocket connections and message routing.
type Manager struct {
	mu      sync.RWMutex
	clients map[string]*Client
	started bool
	stopped atomic.Bool // global stop flag to short-circuit Broadcast after Stop()
	dropped atomic.Uint64
}

// NewManager creates a new WebSocket Manager.
func NewManager() *Manager {
	return &Manager{
		clients: make(map[string]*Client),
	}
}

// HandleWS handles a WebSocket upgrade and connection.
func (m *Manager) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logrus.WithField("error", err.Error()).Warn("WebSocket upgrade failed")
		return
	}

	client := &Client{
		ID:       generateClientID(),
		conn:     conn,
		channels: make(map[ChannelType]bool),
		send:     make(chan []byte, 256),
	}

	m.mu.Lock()
	m.clients[client.ID] = client
	m.mu.Unlock()

	// Start read and write pumps
	go m.readPump(client)
	go m.writePump(client)

	logrus.WithField("client_id", client.ID).Debug("WebSocket client connected")
}

// readPump reads messages from the client.
func (m *Manager) readPump(client *Client) {
	defer func() {
		if r := recover(); r != nil {
			logrus.WithField("client_id", client.ID).
				WithField("panic", r).
				Warn("readPump panicked")
		}
		m.removeClient(client)
		client.conn.Close()
	}()

	// Set a read deadline slightly longer than the write pump's ping interval
	// (30s ticker). The pong handler resets the deadline each time a pong
	// is received, so a healthy client will never time out. A dead or
	// unresponsive client will be evicted after 60 seconds.
	_ = client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.conn.SetPongHandler(func(string) error {
		_ = client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			break
		}

		// Parse message
		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		action, _ := msg["action"].(string)
		channel, _ := msg["channel"].(string)

		// 应用层心跳：前端心跳发送 {"type":"ping"} 并以 JSON pong 刷新 lastPongTime。
		// 协议级 ping/pong 由浏览器自动应答，JS onmessage 不可见，无法作为前端心跳依据，
		// 不回应会导致前端每 60s 误判超时并无限重连。
		if msg["type"] == "ping" {
			ts, _ := msg["ts"].(float64)
			reply, _ := json.Marshal(map[string]interface{}{"type": "pong", "ts": ts})
			select {
			case client.send <- reply:
			default:
				client.noteDrop(m, "control", "pong")
			}
			continue
		}

		// 认证回执：认证已在 HTTP 升级阶段完成（query token / Cookie），
		// 此处仅回 auth ok，前端收到后立即置 connected，不必等 5s 超时兜底
		if msg["type"] == "auth" {
			reply, _ := json.Marshal(map[string]interface{}{"type": "auth", "status": "ok"})
			select {
			case client.send <- reply:
			default:
				client.noteDrop(m, "auth", "auth_ok")
			}
			continue
		}

		switch action {
		case "subscribe":
			client.mu.Lock()
			client.channels[ChannelType(channel)] = true
			client.mu.Unlock()
			logrus.WithField("client_id", client.ID).
				WithField("channel", channel).
				Debug("Client subscribed")
		case "unsubscribe":
			client.mu.Lock()
			delete(client.channels, ChannelType(channel))
			client.mu.Unlock()
		}
	}
}

// writePump writes messages to the client.
func (m *Manager) writePump(client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		if r := recover(); r != nil {
			logrus.WithField("client_id", client.ID).
				WithField("panic", r).
				Warn("writePump panicked")
		}
		ticker.Stop()
		client.conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.send:
			if !ok {
				_ = client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			_ = client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			_ = client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// removeClient removes a client from the manager.
func (m *Manager) removeClient(client *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clients, client.ID)
}

// Broadcast sends a message to all clients subscribed to a channel.
func (m *Manager) Broadcast(channel ChannelType, messageType string, data interface{}) {
	// Short-circuit if the manager has been stopped — avoids unnecessary work
	// and prevents send-on-closed-channel panics.
	if m.stopped.Load() {
		return
	}

	message := map[string]interface{}{
		"channel": string(channel),
		"type":    messageType,
		"data":    data,
		"time":    time.Now().Format(time.RFC3339Nano),
	}
	payload, _ := json.Marshal(message)

	// Snapshot clients under RLock, then send without holding the lock
	// to avoid potential deadlock with Stop() which acquires client.mu.
	m.mu.RLock()
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.RUnlock()

	for _, client := range clients {
		client.mu.Lock()
		if client.closed {
			client.mu.Unlock()
			continue
		}
		subscribed := client.channels[channel]
		client.mu.Unlock()
		if !subscribed {
			continue
		}
		// Use a recover guard to prevent panic if the channel was closed
		// between the check above and the send below (race with Stop()).
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Channel was closed by Stop(), safe to ignore
					_ = r
				}
			}()
			select {
			case client.send <- payload:
			default:
				client.noteDrop(m, channel, messageType)
			}
		}()
	}
}

// BroadcastEvent broadcasts an EventBus event to WebSocket clients.
func (m *Manager) BroadcastEvent(event engine.Event) {
	var channel ChannelType
	switch event.Type {
	case engine.EventTypeDataCollected, engine.EventTypeDataProcessed:
		channel = ChannelRealtime
	case engine.EventTypeAlarmTriggered, engine.EventTypeAlarmRecovered, engine.EventTypeAlarmAcknowledged:
		channel = ChannelAlarm
	case engine.EventTypeDeviceOnline, engine.EventTypeDeviceOffline, engine.EventTypeDeviceError,
		engine.EventTypeDeviceCreated, engine.EventTypeDeviceUpdated, engine.EventTypeDeviceDeleted:
		channel = ChannelDevice
	case engine.EventTypeAIInference:
		channel = ChannelAI
	default:
		return
	}
	m.Broadcast(channel, string(event.Type), event.Data)
}

// SubscribeToEventBus subscribes the manager to EventBus events.
func (m *Manager) SubscribeToEventBus(eventBus *engine.EventBus) {
	eventBus.SubscribeAll(m.BroadcastEvent)
	logrus.Info("WebSocket manager subscribed to EventBus")
}

// GetClientCount returns the number of connected clients.
func (m *Manager) GetClientCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients)
}

// GetStats returns manager statistics.
func (m *Manager) GetStats() map[string]interface{} {
	m.mu.RLock()
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.RUnlock()

	channels := make(map[string]int)
	droppingClients := make(map[string]uint64)
	for _, client := range clients {
		client.mu.Lock()
		for ch := range client.channels {
			channels[string(ch)]++
		}
		client.mu.Unlock()
		if d := client.dropped.Load(); d > 0 {
			droppingClients[client.ID] = d
		}
	}

	m.mu.RLock()
	totalClients := len(m.clients)
	m.mu.RUnlock()

	return map[string]interface{}{
		"total_clients": totalClients,
		"channels":      channels,
		// dropped_frames is cumulative for the process, not per client: a UI
		// that only ever shows the connected flag cannot report lost frames.
		"dropped_frames":   m.dropped.Load(),
		"dropping_clients": droppingClients,
		"dropping_count":   len(droppingClients),
	}
}

// Start starts the WebSocket manager (for lifecycle integration).
func (m *Manager) Start() {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
}

// Stop stops the WebSocket manager and closes all connections.
func (m *Manager) Stop() {
	// Set the global stop flag FIRST so that any concurrent Broadcast calls
	// short-circuit before we start closing channels.
	m.stopped.Store(true)

	m.mu.Lock()
	m.started = false
	clients := make(map[string]*Client, len(m.clients))
	for k, v := range m.clients {
		clients[k] = v
	}
	m.mu.Unlock()

	for _, client := range clients {
		client.mu.Lock()
		if !client.closed {
			client.closed = true
			close(client.send)
		}
		client.mu.Unlock()
		client.conn.Close()
	}
}

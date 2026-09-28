package engine

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// CascadeManager manages multi-gateway cascade topology using mDNS-like discovery.
// This is a Go port of the Python edgelite/engine/cascade_manager.py.
//
// Key features:
//   - Neighbor discovery via local network broadcast
//   - HMAC-SHA256 authenticated forwarding
//   - Topology loop detection via hop count
//   - Nonce-based replay protection

const (
	cascadeHopLimit    = 16
	cascadeTokenTTL    = 300 // seconds
	cascadeTokenHashLen = 16
)

// TopologyStatus represents the role of a node in the cascade topology.
type TopologyStatus string

const (
	TopologyStandalone TopologyStatus = "standalone"
	TopologyParent     TopologyStatus = "parent"
	TopologyChild      TopologyStatus = "child"
	TopologyPeer       TopologyStatus = "peer"
)

// NeighborInfo holds information about a neighboring gateway.
type NeighborInfo struct {
	NeighborID  string            `json:"neighbor_id"`
	Host        string            `json:"host"`
	Port        int               `json:"port"`
	Role        string            `json:"role"`
	Properties  map[string]string `json:"properties"`
	LastSeen    time.Time         `json:"last_seen"`
}

// CascadeTopology holds the current cascade topology data.
type CascadeTopology struct {
	LocalID   string           `json:"local_id"`
	Status    TopologyStatus   `json:"status"`
	ParentID  string           `json:"parent_id,omitempty"`
	Children  []string         `json:"children,omitempty"`
	Peers     []NeighborInfo   `json:"peers,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// CascadeManager manages multi-gateway cascade.
type CascadeManager struct {
	mu               sync.RWMutex
	localID          string
	parentHost       string
	parentPort       int
	parentScheme     string
	servicePort      int
	cascadeToken     string
	allowedNeighbors map[string]bool
	running          bool
	neighbors        map[string]*NeighborInfo
	topology         *CascadeTopology
	seenNonces       map[string]bool
	nonceMu          sync.Mutex
	httpClient       *http.Client
	cancelFunc       context.CancelFunc
}

// NewCascadeManager creates a new CascadeManager.
func NewCascadeManager(localID, parentHost string, parentPort int, cascadeToken string) *CascadeManager {
	if localID == "" {
		localID, _ = os.Hostname()
	}
	return &CascadeManager{
		localID:          localID,
		parentHost:       parentHost,
		parentPort:       parentPort,
		parentScheme:     "http",
		cascadeToken:     cascadeToken,
		allowedNeighbors: nil,
		neighbors:        make(map[string]*NeighborInfo),
		topology:         &CascadeTopology{LocalID: localID, Status: TopologyStandalone},
		seenNonces:       make(map[string]bool),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Start begins the cascade manager.
func (cm *CascadeManager) Start(ctx context.Context) error {
	cm.mu.Lock()
	if cm.running {
		cm.mu.Unlock()
		return nil
	}
	cm.running = true
	cm.mu.Unlock()

	// Start parent connection maintenance if configured
	if cm.parentHost != "" {
		cm.topology.Status = TopologyChild
		cm.topology.ParentID = fmt.Sprintf("%s:%d", cm.parentHost, cm.parentPort)

		childCtx, cancel := context.WithCancel(ctx)
		cm.cancelFunc = cancel
		go cm.maintainParentConnection(childCtx)
	}

	logrus.WithField("local_id", cm.localID).
		WithField("role", cm.topology.Status).
		Info("CascadeManager started")
	return nil
}

// Stop stops the cascade manager.
func (cm *CascadeManager) Stop() {
	cm.mu.Lock()
	cm.running = false
	cm.mu.Unlock()

	if cm.cancelFunc != nil {
		cm.cancelFunc()
		cm.cancelFunc = nil
	}
	logrus.Info("CascadeManager stopped")
}

// GetTopology returns the current topology.
func (cm *CascadeManager) GetTopology() *CascadeTopology {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.topology
}

// GetNeighbors returns a snapshot of discovered neighbors.
func (cm *CascadeManager) GetNeighbors() []NeighborInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	result := make([]NeighborInfo, 0, len(cm.neighbors))
	for _, n := range cm.neighbors {
		result = append(result, *n)
	}
	return result
}

// ForwardToParent forwards data to the parent gateway.
func (cm *CascadeManager) ForwardToParent(data map[string]interface{}) bool {
	if cm.parentHost == "" || cm.parentPort == 0 {
		logrus.Warn("Parent node not configured, cannot forward data")
		return false
	}

	// Hop count check for topology loop detection
	hopCount := 0
	if hc, ok := data["_cascade_hop_count"]; ok {
		if h, ok := hc.(int); ok {
			hopCount = h
		} else if h, ok := hc.(float64); ok {
			hopCount = int(h)
		}
	}
	hopCount++
	data["_cascade_hop_count"] = hopCount
	if hopCount > cascadeHopLimit {
		logrus.WithField("hop_count", hopCount).
			Error("Cascade hop count exceeded limit, dropping forward (possible topology loop)")
		return false
	}

	url := fmt.Sprintf("%s://%s:%d/api/v1/integration/cascade/forward",
		cm.parentScheme, cm.parentHost, cm.parentPort)

	body, err := json.Marshal(data)
	if err != nil {
		logrus.WithError(err).Error("Failed to marshal forward data")
		return false
	}

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		logrus.WithError(err).Error("Failed to create forward request")
		return false
	}

	// Set authentication headers
	headers := cm.buildCascadeHeaders(body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = nopCloser{newBytesReader(body)}

	resp, err := cm.httpClient.Do(req)
	if err != nil {
		logrus.WithError(err).Error("Forward data to parent node failed")
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == 200
}

// VerifyCascadeRequest verifies an incoming cascade request's HMAC signature.
func (cm *CascadeManager) VerifyCascadeRequest(headers http.Header, body []byte) (bool, string) {
	if cm.cascadeToken == "" {
		return false, "cascade token not configured on receiver"
	}

	signature := headers.Get("X-Cascade-Token")
	timestampStr := headers.Get("X-Cascade-Timestamp")
	nonce := headers.Get("X-Cascade-Nonce")

	if signature == "" || timestampStr == "" || nonce == "" {
		return false, "missing cascade auth headers"
	}

	var ts int64
	fmt.Sscanf(timestampStr, "%d", &ts)
	if absInt64(time.Now().Unix()-ts) > cascadeTokenTTL {
		return false, "timestamp out of allowed window"
	}

	// Nonce replay protection
	cm.nonceMu.Lock()
	if cm.seenNonces[nonce] {
		cm.nonceMu.Unlock()
		return false, "nonce replay detected"
	}
	cm.seenNonces[nonce] = true
	if len(cm.seenNonces) > 10000 {
		cm.seenNonces = make(map[string]bool)
		cm.seenNonces[nonce] = true
	}
	cm.nonceMu.Unlock()

	// Recompute signature
	message := append([]byte(timestampStr+nonce), body...)
	mac := hmac.New(sha256.New, []byte(cm.cascadeToken))
	mac.Write(message)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return false, "signature mismatch"
	}
	return true, ""
}

// CheckHopCount checks the hop count of forwarded data.
func (cm *CascadeManager) CheckHopCount(data map[string]interface{}) (bool, int) {
	hop := 0
	if h, ok := data["_cascade_hop_count"]; ok {
		switch v := h.(type) {
		case int:
			hop = v
		case float64:
			hop = int(v)
		}
	}
	if hop > cascadeHopLimit {
		logrus.WithField("hop_count", hop).
			Error("Cascade hop count exceeded limit, dropping (possible topology loop)")
		return false, hop
	}
	return true, hop
}

// UpdateConfig updates cascade configuration.
func (cm *CascadeManager) UpdateConfig(config map[string]interface{}) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if host, ok := config["parent_host"].(string); ok {
		cm.parentHost = host
	}
	if port, ok := config["parent_port"].(float64); ok {
		cm.parentPort = int(port)
	}
	if scheme, ok := config["parent_scheme"].(string); ok {
		if scheme == "http" || scheme == "https" {
			cm.parentScheme = scheme
		}
	}
	if role, ok := config["role"].(string); ok {
		switch TopologyStatus(role) {
		case TopologyParent, TopologyChild, TopologyPeer, TopologyStandalone:
			cm.topology.Status = TopologyStatus(role)
		}
	}
	if cm.parentHost != "" {
		cm.topology.ParentID = fmt.Sprintf("%s:%d", cm.parentHost, cm.parentPort)
		cm.topology.Status = TopologyChild
	}
}

// AddNeighbor manually adds or updates a neighbor.
func (cm *CascadeManager) AddNeighbor(n *NeighborInfo) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	n.LastSeen = time.Now()
	cm.neighbors[n.NeighborID] = n
	cm.rebuildTopology()
}

// RemoveNeighbor removes a neighbor by ID.
func (cm *CascadeManager) RemoveNeighbor(neighborID string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, ok := cm.neighbors[neighborID]; !ok {
		return false
	}
	delete(cm.neighbors, neighborID)
	cm.rebuildTopology()
	return true
}

func (cm *CascadeManager) rebuildTopology() {
	neighbors := make([]NeighborInfo, 0, len(cm.neighbors))
	for _, n := range cm.neighbors {
		neighbors = append(neighbors, *n)
	}
	cm.topology.Peers = neighbors
	cm.topology.UpdatedAt = time.Now()
}

func (cm *CascadeManager) maintainParentConnection(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cm.parentHost != "" {
				success := cm.ForwardToParent(map[string]interface{}{
					"type":     "heartbeat",
					"local_id": cm.localID,
				})
				if !success {
					logrus.WithField("parent", fmt.Sprintf("%s:%d", cm.parentHost, cm.parentPort)).
						Debug("Parent heartbeat failed")
				}
			}
		}
	}
}

func (cm *CascadeManager) buildCascadeHeaders(body []byte) map[string]string {
	if cm.cascadeToken == "" {
		return map[string]string{}
	}
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	nonce := generateNonce()
	message := append([]byte(timestamp+nonce), body...)
	mac := hmac.New(sha256.New, []byte(cm.cascadeToken))
	mac.Write(message)
	signature := hex.EncodeToString(mac.Sum(nil))
	return map[string]string{
		"X-Cascade-Token":     signature,
		"X-Cascade-Timestamp": timestamp,
		"X-Cascade-Nonce":     nonce,
	}
}

func (cm *CascadeManager) computeTokenHash() string {
	if cm.cascadeToken == "" {
		return ""
	}
	h := sha256.Sum256([]byte(cm.cascadeToken))
	return hex.EncodeToString(h[:])[:cascadeTokenHashLen]
}

func (cm *CascadeManager) verifyNeighbor(neighborID, neighborTokenHash string) bool {
	if cm.cascadeToken == "" {
		return true
	}
	expected := cm.computeTokenHash()
	if neighborTokenHash != expected {
		return false
	}
	if cm.allowedNeighbors != nil {
		return cm.allowedNeighbors[neighborID]
	}
	return true
}

func (cm *CascadeManager) getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// nopCloser wraps a reader to implement ReadCloser.
type nopCloser struct {
	*bytesReader
}

func (nopCloser) Close() error { return nil }

// bytesReader is a minimal bytes reader.
type bytesReader struct {
	data []byte
	pos  int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

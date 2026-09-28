package engine

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// WebhookAuthMode represents the authentication mode for webhooks.
type WebhookAuthMode string

const (
	WebhookAuthNone   WebhookAuthMode = "none"
	WebhookAuthToken  WebhookAuthMode = "token"
	WebhookAuthBasic  WebhookAuthMode = "basic"
	WebhookAuthBearer WebhookAuthMode = "bearer"
)

// WebhookAuthMiddleware provides authentication for webhook endpoints.
type WebhookAuthMiddleware struct {
	mode        WebhookAuthMode
	token       string
	username    string
	password    string
	replayCache *ReplayCache
}

// ReplayCache prevents replay attacks by tracking used nonces.
type ReplayCache struct {
	mu     sync.Mutex
	nonces map[string]time.Time
	maxAge time.Duration
}

// NewReplayCache creates a new ReplayCache.
func NewReplayCache(maxAgeSec int) *ReplayCache {
	if maxAgeSec <= 0 {
		maxAgeSec = 300 // 5 minutes
	}
	return &ReplayCache{
		nonces: make(map[string]time.Time),
		maxAge: time.Duration(maxAgeSec) * time.Second,
	}
}

// CheckAndAdd checks if a nonce is already used and adds it if not.
func (r *ReplayCache) CheckAndAdd(nonce string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Cleanup old entries
	now := time.Now()
	for n, ts := range r.nonces {
		if now.Sub(ts) > r.maxAge {
			delete(r.nonces, n)
		}
	}
	// Check if nonce already exists
	if _, exists := r.nonces[nonce]; exists {
		return false // Replay detected
	}
	r.nonces[nonce] = now
	return true
}

// NewWebhookAuthMiddleware creates a new WebhookAuthMiddleware.
func NewWebhookAuthMiddleware(mode, token, username, password string) *WebhookAuthMiddleware {
	return &WebhookAuthMiddleware{
		mode:        WebhookAuthMode(mode),
		token:       resolveEnvVar(token),
		username:    resolveEnvVar(username),
		password:    resolveEnvVar(password),
		replayCache: NewReplayCache(300),
	}
}

// resolveEnvVar resolves environment variable references in the format "${ENV_VAR}".
func resolveEnvVar(value string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		envName := value[2 : len(value)-1]
		return os.Getenv(envName)
	}
	return value
}

// Verify verifies the authentication of an incoming webhook request.
func (m *WebhookAuthMiddleware) Verify(authHeader string, timestamp, nonce string) (bool, string) {
	switch m.mode {
	case WebhookAuthNone:
		return true, "auth disabled"
	case WebhookAuthToken:
		return m.verifyToken(authHeader), "token"
	case WebhookAuthBasic:
		return m.verifyBasic(authHeader), "basic"
	case WebhookAuthBearer:
		return m.verifyBearer(authHeader), "bearer"
	default:
		return false, "unknown auth mode"
	}
}

// VerifyReplay checks for replay attacks using timestamp and nonce.
func (m *WebhookAuthMiddleware) VerifyReplay(timestamp, nonce string) bool {
	if timestamp == "" || nonce == "" {
		return false
	}
	// Check timestamp is within acceptable window (5 minutes)
	var ts time.Time
	if t, err := time.Parse(time.RFC3339, timestamp); err == nil {
		ts = t
	} else if t, err := time.Parse(time.RFC1123, timestamp); err == nil {
		ts = t
	} else {
		// Try Unix timestamp
		var unixTs int64
		if _, err := fmt.Sscanf(timestamp, "%d", &unixTs); err == nil {
			ts = time.Unix(unixTs, 0)
		} else {
			return false
		}
	}
	if time.Since(ts) > 5*time.Minute || time.Until(ts) > 5*time.Minute {
		return false
	}
	// Check nonce for replay
	return m.replayCache.CheckAndAdd(nonce)
}

func (m *WebhookAuthMiddleware) verifyToken(authHeader string) bool {
	if m.token == "" {
		return false
	}
	// Token can be in Authorization header or X-Webhook-Token header
	token := authHeader
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = authHeader[7:]
	} else if strings.HasPrefix(authHeader, "Token ") {
		token = authHeader[6:]
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(m.token)) == 1
}

func (m *WebhookAuthMiddleware) verifyBasic(authHeader string) bool {
	if m.username == "" || m.password == "" {
		return false
	}
	if !strings.HasPrefix(authHeader, "Basic ") {
		return false
	}
	encoded := authHeader[6:]
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false
	}
	usernameMatch := subtle.ConstantTimeCompare([]byte(parts[0]), []byte(m.username)) == 1
	passwordMatch := subtle.ConstantTimeCompare([]byte(parts[1]), []byte(m.password)) == 1
	return usernameMatch && passwordMatch
}

func (m *WebhookAuthMiddleware) verifyBearer(authHeader string) bool {
	if m.token == "" {
		return false
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	token := authHeader[7:]
	return subtle.ConstantTimeCompare([]byte(token), []byte(m.token)) == 1
}

// GetMode returns the current auth mode.
func (m *WebhookAuthMiddleware) GetMode() WebhookAuthMode {
	return m.mode
}

// Package security provides JWT authentication, RBAC, password hashing, and CSRF protection.
package security

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"edgelite/internal/config"
)

// Claims represents JWT claims.
type Claims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// JWTManager handles JWT token generation and verification.
type JWTManager struct {
	secretKey         []byte
	algorithm         string
	accessExpiration  time.Duration
	refreshExpiration time.Duration
	keyID             string
	previousKey       []byte
	previousKeyID     string
}

var (
	jwtManagerOnce sync.Once
	jwtManager     *JWTManager
	jwtManagerLock sync.Mutex
)

// ResetJWTManagerForTest resets the JWT manager singleton.
// This is intended for testing only, to ensure test isolation.
func ResetJWTManagerForTest() {
	jwtManagerLock.Lock()
	defer jwtManagerLock.Unlock()
	jwtManager = nil
	jwtManagerOnce = sync.Once{}
}

// GetJWTManager returns a singleton JWTManager.
func GetJWTManager() *JWTManager {
	jwtManagerOnce.Do(func() {
		cfg := config.GetConfig()
		jwtManager = &JWTManager{
			secretKey:         []byte(cfg.Security.SecretKey),
			algorithm:         cfg.Security.Algorithm,
			accessExpiration:  time.Duration(cfg.Security.AccessTokenExpireMinutes) * time.Minute,
			refreshExpiration: time.Duration(cfg.Security.RefreshTokenExpireDays) * 24 * time.Hour,
			keyID:             cfg.Security.KeyID,
			previousKey:       []byte(cfg.Security.SecretKeyPrevious),
			previousKeyID:     cfg.Security.PreviousKeyID,
		}
	})
	return jwtManager
}

// GenerateAccessToken creates a new access token.
func (m *JWTManager) GenerateAccessToken(userID, username, role string) (string, int, error) {
	expirationTime := time.Now().Add(m.accessExpiration)
	claims := &Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   userID,
			// jti 必须每个 token 唯一：登出按 jti 吊销，若复用 keyID 会把之后
			// 所有新签发的 token 一并拉黑（keyID 只作为 header kid 用于轮换）
			ID: uuid.New().String(),
		},
	}
	token := jwt.NewWithClaims(jwt.GetSigningMethod(m.algorithm), claims)
	if m.keyID != "" {
		token.Header["kid"] = m.keyID
	}
	tokenString, err := token.SignedString(m.secretKey)
	if err != nil {
		return "", 0, err
	}
	return tokenString, int(m.accessExpiration.Seconds()), nil
}

// GenerateRefreshToken creates a new refresh token.
func (m *JWTManager) GenerateRefreshToken(userID, username, role string) (string, error) {
	expirationTime := time.Now().Add(m.refreshExpiration)
	claims := &Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   userID,
			ID:        uuid.New().String(),
		},
	}
	token := jwt.NewWithClaims(jwt.GetSigningMethod(m.algorithm), claims)
	if m.keyID != "" {
		token.Header["kid"] = m.keyID
	}
	return token.SignedString(m.secretKey)
}

// VerifyToken verifies a JWT token and returns claims.
func (m *JWTManager) VerifyToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method is HMAC (prevent alg=none attack)
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		// Check kid header for key rotation
		if kid, ok := token.Header["kid"].(string); ok {
			if m.previousKeyID != "" && kid == m.previousKeyID && len(m.previousKey) > 0 {
				return m.previousKey, nil
			}
		}
		return m.secretKey, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	// A per-user revocation ("log this user out everywhere") has to be enforced
	// here instead of at each call site: the WebSocket authenticator verifies
	// tokens directly, and a cutoff only the HTTP middleware honoured would leave
	// already-open sockets serving a user an admin just revoked.
	if RevokedForUser(claims) {
		return nil, ErrTokenRevokedForUser
	}
	return claims, nil
}

// HashPassword hashes a password using bcrypt.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(bytes), err
}

// CheckPassword compares a plaintext password with a bcrypt hash.
func CheckPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Role constants
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Permission represents a single permission.
type Permission string

const (
	// Device permissions
	PermDeviceCreate          Permission = "device:create"
	PermDeviceRead            Permission = "device:read"
	PermDeviceUpdate          Permission = "device:update"
	PermDeviceDelete          Permission = "device:delete"
	PermDeviceWrite           Permission = "device:write"
	PermDeviceWritePolicyEdit Permission = "device:write_policy_edit"

	// Rule permissions
	PermRuleCreate Permission = "rule:create"
	PermRuleRead   Permission = "rule:read"
	PermRuleUpdate Permission = "rule:update"
	PermRuleDelete Permission = "rule:delete"

	// Alarm permissions
	PermAlarmRead Permission = "alarm:read"
	PermAlarmAck  Permission = "alarm:ack"

	// Data permissions
	PermDataRead   Permission = "data:read"
	PermDataExport Permission = "data:export"
	PermDataImport Permission = "data:import"

	// User permissions
	PermUserCreate Permission = "user:create"
	PermUserRead   Permission = "user:read"
	PermUserUpdate Permission = "user:update"
	PermUserDelete Permission = "user:delete"

	// System permissions
	PermSystemConfig  Permission = "system:config"
	PermSystemBackup  Permission = "system:backup"
	PermSystemRestore Permission = "system:restore"
	PermSystemLogs    Permission = "system:logs"

	// Video permissions
	PermVideoRead Permission = "video:read"

	// OTA permissions
	PermOTAManage Permission = "ota:manage"

	// Audit permissions
	PermAuditRead Permission = "audit:read"

	// SCADA permissions
	PermSCADAEdit Permission = "scada:edit"

	// Integration permissions
	PermIntegrationManage Permission = "integration:manage"

	// Driver permissions
	PermDriverConfig Permission = "driver:config"

	// Platform permissions
	PermPlatformManage Permission = "platform:manage"

	// Notify permissions
	PermNotifyConfig Permission = "notify:config"

	// Expression permissions
	PermExpressionConfig Permission = "expression:config"

	// Preprocess permissions
	PermPreprocessConfig Permission = "preprocess:config"
)

// RolePermissions maps roles to their allowed permissions.
var RolePermissions = map[string][]Permission{
	RoleAdmin: {
		PermDeviceCreate, PermDeviceRead, PermDeviceUpdate, PermDeviceDelete, PermDeviceWrite, PermDeviceWritePolicyEdit,
		PermRuleCreate, PermRuleRead, PermRuleUpdate, PermRuleDelete,
		PermAlarmRead, PermAlarmAck,
		PermDataRead, PermDataExport, PermDataImport,
		PermUserCreate, PermUserRead, PermUserUpdate, PermUserDelete,
		PermSystemConfig, PermSystemBackup, PermSystemRestore, PermSystemLogs,
		PermVideoRead, PermOTAManage, PermAuditRead, PermSCADAEdit,
		PermIntegrationManage, PermDriverConfig, PermPlatformManage, PermNotifyConfig,
		PermExpressionConfig, PermPreprocessConfig,
	},
	RoleOperator: {
		PermDeviceRead, PermDeviceUpdate, PermDeviceWrite,
		PermRuleCreate, PermRuleRead, PermRuleUpdate, PermRuleDelete,
		PermAlarmRead, PermAlarmAck,
		PermDataRead, PermDataExport,
		PermVideoRead, PermOTAManage, PermSCADAEdit,
		PermDriverConfig,
	},
	RoleViewer: {
		PermDeviceRead, PermRuleRead, PermAlarmRead, PermDataRead, PermVideoRead,
	},
}

// HasPermission checks if a role has a specific permission.
func HasPermission(role string, perm Permission) bool {
	perms, ok := RolePermissions[role]
	if !ok {
		return false
	}
	for _, p := range perms {
		if p == perm {
			return true
		}
	}
	return false
}

// TokenRevocationList manages revoked tokens in memory.
type TokenRevocationList struct {
	mu      sync.RWMutex
	tokens  map[string]time.Time
	maxSize int
}

var revocationList = &TokenRevocationList{
	tokens:  make(map[string]time.Time),
	maxSize: 100000,
}

// RevokeToken adds a token to the revocation list.
func RevokeToken(tokenID string, ttl time.Duration) {
	revocationList.mu.Lock()
	defer revocationList.mu.Unlock()
	// Evict expired entries when at capacity
	if len(revocationList.tokens) >= revocationList.maxSize {
		now := time.Now()
		for id, t := range revocationList.tokens {
			if now.Sub(t) > ttl {
				delete(revocationList.tokens, id)
			}
		}
		// If still at capacity after evicting expired, remove oldest entries
		if len(revocationList.tokens) >= revocationList.maxSize {
			// Find and remove the oldest entry
			var oldestID string
			var oldestTime time.Time
			for id, t := range revocationList.tokens {
				if oldestID == "" || t.Before(oldestTime) {
					oldestID = id
					oldestTime = t
				}
			}
			delete(revocationList.tokens, oldestID)
		}
	}
	revocationList.tokens[tokenID] = time.Now()
}

// IsRevoked checks if a token is revoked.
func IsRevoked(tokenID string) bool {
	revocationList.mu.RLock()
	defer revocationList.mu.RUnlock()
	_, revoked := revocationList.tokens[tokenID]
	return revoked
}

// ErrTokenRevokedForUser is returned by VerifyToken when the token predates the
// revocation cutoff recorded for its user.
var ErrTokenRevokedForUser = errors.New("token revoked for this user")

// UserTokenCutoffs records, per user, the instant before which every token they
// were issued must be treated as dead. Access tokens are stateless and nothing
// here stores a session row, so "revoke all of this user's sessions" can only be
// expressed as a timestamp: every token whose iat falls before it is rejected,
// and tokens minted afterwards (a re-login, a refreshed access token) keep working.
type UserTokenCutoffs struct {
	mu      sync.RWMutex
	cutoffs map[string]time.Time
	maxSize int
}

var userTokenCutoffs = &UserTokenCutoffs{
	cutoffs: make(map[string]time.Time),
	maxSize: 10000,
}

// RevokeUserTokens invalidates every token issued for userID before `before`.
// The cutoff is truncated to whole seconds because JWT iat has second
// resolution: keeping the sub-second remainder would compare a unix-second
// timestamp against a finer clock and reject tokens minted after the revocation.
// The accepted side of that trade is a window of at most one second in which a
// token issued just before the revoke still validates.
func RevokeUserTokens(userID string, before time.Time) {
	if userID == "" {
		return
	}
	userTokenCutoffs.mu.Lock()
	defer userTokenCutoffs.mu.Unlock()
	if len(userTokenCutoffs.cutoffs) >= userTokenCutoffs.maxSize {
		var oldestID string
		var oldest time.Time
		for id, t := range userTokenCutoffs.cutoffs {
			if oldestID == "" || t.Before(oldest) {
				oldestID = id
				oldest = t
			}
		}
		delete(userTokenCutoffs.cutoffs, oldestID)
	}
	userTokenCutoffs.cutoffs[userID] = before.Truncate(time.Second)
}

// UserTokenCutoff reports the revocation instant recorded for userID, if any.
func UserTokenCutoff(userID string) (time.Time, bool) {
	userTokenCutoffs.mu.RLock()
	defer userTokenCutoffs.mu.RUnlock()
	t, ok := userTokenCutoffs.cutoffs[userID]
	return t, ok
}

// RevokedForUser reports whether claims falls inside a per-user revocation window.
func RevokedForUser(claims *Claims) bool {
	if claims == nil {
		return false
	}
	userID := claims.UserID
	if userID == "" {
		userID = claims.Subject
	}
	if userID == "" {
		// Nothing to revoke against; the jti list still applies to such a token.
		return false
	}
	userTokenCutoffs.mu.RLock()
	cutoff, ok := userTokenCutoffs.cutoffs[userID]
	userTokenCutoffs.mu.RUnlock()
	if !ok {
		return false
	}
	// A token with no iat cannot be shown to postdate the cutoff, so it fails closed.
	return claims.IssuedAt == nil || claims.IssuedAt.Time.Before(cutoff)
}

// LoginAttemptTracker tracks failed login attempts per IP and username.
type LoginAttemptTracker struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

var loginTracker = &LoginAttemptTracker{
	attempts: make(map[string][]time.Time),
}

// RecordFailedLogin records a failed login attempt.
func RecordFailedLogin(key string) {
	loginTracker.mu.Lock()
	defer loginTracker.mu.Unlock()
	now := time.Now()
	attempts := loginTracker.attempts[key]
	attempts = append(attempts, now)
	// Keep only attempts within the window
	window := 15 * time.Minute
	cutoff := now.Add(-window)
	filtered := attempts[:0]
	for _, t := range attempts {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	loginTracker.attempts[key] = filtered
}

// GetFailedAttempts returns the number of recent failed attempts.
func GetFailedAttempts(key string) int {
	loginTracker.mu.Lock()
	defer loginTracker.mu.Unlock()
	now := time.Now()
	window := 15 * time.Minute
	cutoff := now.Add(-window)
	count := 0
	for _, t := range loginTracker.attempts[key] {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// IsLockedOut checks if an IP or username is locked out.
func IsLockedOut(key string, threshold int) bool {
	return GetFailedAttempts(key) >= threshold
}

// ClearLoginAttempts clears failed attempts for a key.
func ClearLoginAttempts(key string) {
	loginTracker.mu.Lock()
	defer loginTracker.mu.Unlock()
	delete(loginTracker.attempts, key)
}

// CSRFManager handles CSRF token generation and verification.
type CSRFManager struct {
	secret []byte
}

var (
	csrfManager     *CSRFManager
	csrfManagerLock sync.Mutex
)

// ResetCSRFManagerForTest resets the CSRF manager singleton.
// This is intended for testing only, to ensure test isolation.
func ResetCSRFManagerForTest() {
	csrfManagerLock.Lock()
	defer csrfManagerLock.Unlock()
	csrfManager = nil
}

// GetCSRFManager returns a singleton CSRFManager.
func GetCSRFManager() *CSRFManager {
	csrfManagerLock.Lock()
	defer csrfManagerLock.Unlock()
	if csrfManager != nil {
		return csrfManager
	}
	cfg := config.GetConfig()
	secret := cfg.Security.CSRFSecret
	if secret == "" {
		secret = cfg.Security.SecretKey
	}
	csrfManager = &CSRFManager{secret: []byte(secret)}
	return csrfManager
}

// GenerateCSRFToken creates a CSRF token.
func (m *CSRFManager) GenerateCSRFToken() (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"typ": "csrf",
	})
	return token.SignedString(m.secret)
}

// VerifyCSRFToken verifies a CSRF token.
func (m *CSRFManager) VerifyCSRFToken(tokenString string) error {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Security: verify signing method is HMAC (prevent alg=none attack)
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil || !token.Valid {
		return fmt.Errorf("invalid CSRF token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return fmt.Errorf("invalid CSRF claims")
	}
	if typ, _ := claims["typ"].(string); typ != "csrf" {
		return fmt.Errorf("invalid token type")
	}
	return nil
}

// DataMasker masks sensitive data in logs and responses.
type DataMasker struct{}

var sensitivePatterns = []string{"password", "secret", "token", "api_key", "apikey", "private_key"}

// MaskString masks a string if it appears to be sensitive.
func MaskString(s string) string {
	if len(s) <= 2 {
		return "***"
	}
	return string(s[0]) + "***" + string(s[len(s)-1])
}

// IsSensitiveKey checks if a key name is sensitive.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, p := range sensitivePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

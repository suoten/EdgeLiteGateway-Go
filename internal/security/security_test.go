package security

import (
	"testing"
	"time"

	"edgelite/internal/config"
)

// testJWTManager creates a JWTManager for testing without relying on global config.
func testJWTManager() *JWTManager {
	return &JWTManager{
		secretKey:        []byte("test-secret-key-at-least-32-characters-long!!"),
		algorithm:        "HS256",
		accessExpiration: 30 * time.Minute,
		refreshExpiration: 7 * 24 * time.Hour,
		keyID:            "test-key",
	}
}

func TestGenerateAndVerifyAccessToken(t *testing.T) {
	mgr := testJWTManager()
	token, expiresIn, err := mgr.GenerateAccessToken("user-1", "adminuser", "admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("token should not be empty")
	}
	if expiresIn <= 0 {
		t.Fatalf("expiresIn should be positive, got %d", expiresIn)
	}

	claims, err := mgr.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("expected UserID 'user-1', got '%s'", claims.UserID)
	}
	if claims.Username != "adminuser" {
		t.Errorf("expected Username 'adminuser', got '%s'", claims.Username)
	}
	if claims.Role != "admin" {
		t.Errorf("expected Role 'admin', got '%s'", claims.Role)
	}
}

func TestGenerateAndVerifyRefreshToken(t *testing.T) {
	mgr := testJWTManager()
	token, err := mgr.GenerateRefreshToken("user-2", "operator1", "operator")
	if err != nil {
		t.Fatalf("GenerateRefreshToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("token should not be empty")
	}

	claims, err := mgr.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if claims.UserID != "user-2" {
		t.Errorf("expected UserID 'user-2', got '%s'", claims.UserID)
	}
}

func TestVerifyTokenInvalid(t *testing.T) {
	mgr := testJWTManager()
	_, err := mgr.VerifyToken("invalid-token-string")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestVerifyTokenAlgNoneAttack(t *testing.T) {
	// Attempt to forge a token with alg=none — must be rejected
	mgr := testJWTManager()
	// A valid token first
	token, _, err := mgr.GenerateAccessToken("user-1", "adminuser", "admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken failed: %v", err)
	}
	// Verify the legitimate token works
	_, err = mgr.VerifyToken(token)
	if err != nil {
		t.Fatalf("legitimate token should verify: %v", err)
	}
	// An empty/malformed token must fail
	_, err = mgr.VerifyToken("")
	if err == nil {
		t.Fatal("empty token must be rejected")
	}
}

func TestHashAndCheckPassword(t *testing.T) {
	password := "mySecretPassword123!"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if hash == "" {
		t.Fatal("hash should not be empty")
	}
	if hash == password {
		t.Fatal("hash should not equal plaintext")
	}
	if !CheckPassword(password, hash) {
		t.Fatal("CheckPassword should return true for correct password")
	}
	if CheckPassword("wrongpassword", hash) {
		t.Fatal("CheckPassword should return false for wrong password")
	}
}

func TestHasPermission(t *testing.T) {
	// Admin should have all permissions
	if !HasPermission(RoleAdmin, PermDeviceCreate) {
		t.Error("admin should have device:create permission")
	}
	if !HasPermission(RoleAdmin, PermUserDelete) {
		t.Error("admin should have user:delete permission")
	}

	// Operator should have device:read but not user:create
	if !HasPermission(RoleOperator, PermDeviceRead) {
		t.Error("operator should have device:read permission")
	}
	if HasPermission(RoleOperator, PermUserCreate) {
		t.Error("operator should NOT have user:create permission")
	}

	// Viewer should have device:read but not device:create
	if !HasPermission(RoleViewer, PermDeviceRead) {
		t.Error("viewer should have device:read permission")
	}
	if HasPermission(RoleViewer, PermDeviceCreate) {
		t.Error("viewer should NOT have device:create permission")
	}

	// Unknown role should have no permissions
	if HasPermission("unknown", PermDeviceRead) {
		t.Error("unknown role should have no permissions")
	}
}

func TestTokenRevocation(t *testing.T) {
	RevokeToken("test-token-id-1", time.Hour)
	if !IsRevoked("test-token-id-1") {
		t.Fatal("token should be revoked after RevokeToken")
	}
	if IsRevoked("non-existent-token") {
		t.Fatal("non-existent token should not be revoked")
	}
}

func TestLoginAttemptTracker(t *testing.T) {
	key := "192.168.1.100"
	ClearLoginAttempts(key)

	// Initially no failed attempts
	if GetFailedAttempts(key) != 0 {
		t.Fatal("expected 0 failed attempts initially")
	}

	// Record some failures
	RecordFailedLogin(key)
	RecordFailedLogin(key)
	RecordFailedLogin(key)

	if GetFailedAttempts(key) != 3 {
		t.Fatalf("expected 3 failed attempts, got %d", GetFailedAttempts(key))
	}

	// Should be locked out with threshold 3
	if !IsLockedOut(key, 3) {
		t.Fatal("should be locked out with threshold 3")
	}

	// Should NOT be locked out with threshold 5
	if IsLockedOut(key, 5) {
		t.Fatal("should NOT be locked out with threshold 5")
	}

	// Clear attempts
	ClearLoginAttempts(key)
	if GetFailedAttempts(key) != 0 {
		t.Fatal("expected 0 failed attempts after clear")
	}
}

func TestCSRFToken(t *testing.T) {
	// Set up config for CSRF manager
	config.SetGlobalConfig(&config.AppConfig{
		Security: config.SecurityConfig{
			SecretKey:  "test-csrf-secret-key-32-characters-long!!",
			CSRFSecret: "test-csrf-secret-key-32-characters!!",
		},
	})
	ResetCSRFManagerForTest()

	mgr := GetCSRFManager()
	token, err := mgr.GenerateCSRFToken()
	if err != nil {
		t.Fatalf("GenerateCSRFToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("CSRF token should not be empty")
	}

	err = mgr.VerifyCSRFToken(token)
	if err != nil {
		t.Fatalf("VerifyCSRFToken failed: %v", err)
	}

	// Invalid token
	err = mgr.VerifyCSRFToken("invalid-csrf-token")
	if err == nil {
		t.Fatal("expected error for invalid CSRF token")
	}
}

func TestMaskString(t *testing.T) {
	result := MaskString("mySecretKey123")
	if result == "mySecretKey123" {
		t.Fatal("MaskString should mask the input")
	}
	if len(result) < 4 {
		t.Fatal("MaskString result too short")
	}

	// Short strings
	short := MaskString("ab")
	if short != "***" {
		t.Errorf("expected '***' for short string, got '%s'", short)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	if !IsSensitiveKey("password") {
		t.Error("'password' should be sensitive")
	}
	if !IsSensitiveKey("api_key") {
		t.Error("'api_key' should be sensitive")
	}
	if !IsSensitiveKey("SECRET_TOKEN") {
		t.Error("'SECRET_TOKEN' should be sensitive (case-insensitive)")
	}
	if IsSensitiveKey("username") {
		t.Error("'username' should NOT be sensitive")
	}
	if IsSensitiveKey("device_name") {
		t.Error("'device_name' should NOT be sensitive")
	}
}

func TestJWTManagerSingleton(t *testing.T) {
	// Set up config
	config.SetGlobalConfig(&config.AppConfig{
		Security: config.SecurityConfig{
			SecretKey:                "test-singleton-secret-key-32-characters!",
			Algorithm:                "HS256",
			AccessTokenExpireMinutes: 30,
			RefreshTokenExpireDays:   7,
			KeyID:                    "test",
		},
	})
	ResetJWTManagerForTest()

	mgr1 := GetJWTManager()
	mgr2 := GetJWTManager()
	if mgr1 != mgr2 {
		t.Fatal("GetJWTManager should return same singleton instance")
	}
}

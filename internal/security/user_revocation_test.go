package security

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// "Revoke this user's sessions" has no session table behind it, so the only
// truthful implementation is a per-user timestamp every token is compared
// against. These tests pin the four properties that make that usable:
// tokens issued before the cutoff die, tokens issued after it (the re-login the
// client performs immediately afterwards) live, other users are untouched, and a
// token without iat cannot claim to be newer.

func revocationTestManager() *JWTManager {
	return &JWTManager{
		secretKey:        []byte("user-revocation-test-secret-key-32chars"),
		algorithm:        "HS256",
		accessExpiration: time.Hour,
	}
}

func mintAccess(t *testing.T, m *JWTManager, userID string) string {
	t.Helper()
	token, _, err := m.GenerateAccessToken(userID, "user-"+userID, "viewer")
	if err != nil {
		t.Fatalf("GenerateAccessToken(%s): %v", userID, err)
	}
	return token
}

func TestUserCutoffRejectsTokensIssuedBeforeIt(t *testing.T) {
	m := revocationTestManager()
	token := mintAccess(t, m, "u-cut-off-old")

	// The cutoff is deliberately in the future relative to the token's second:
	// iat only has second resolution, so a revoke landing in the same second as
	// the mint is accepted by design (see RevokeUserTokens).
	RevokeUserTokens("u-cut-off-old", time.Now().Add(2*time.Second))

	if _, err := m.VerifyToken(token); !errors.Is(err, ErrTokenRevokedForUser) {
		t.Fatalf("VerifyToken after revoke = %v, want ErrTokenRevokedForUser", err)
	}
}

func TestUserCutoffKeepsTokensIssuedAfterIt(t *testing.T) {
	m := revocationTestManager()
	RevokeUserTokens("u-cut-off-new", time.Now().Add(-2*time.Second))

	token := mintAccess(t, m, "u-cut-off-new")
	claims, err := m.VerifyToken(token)
	if err != nil {
		t.Fatalf("a token minted after the revoke must still verify, got %v", err)
	}
	if claims.UserID != "u-cut-off-new" {
		t.Fatalf("claims.UserID = %q", claims.UserID)
	}
}

func TestUserCutoffOnlyReachesItsOwnUser(t *testing.T) {
	m := revocationTestManager()
	mine := mintAccess(t, m, "u-bystander")
	RevokeUserTokens("u-someone-else", time.Now().Add(2*time.Second))

	if _, err := m.VerifyToken(mine); err != nil {
		t.Fatalf("revoking another user broke this one: %v", err)
	}
}

func TestTokenWithoutIssuedAtFailsClosedAfterRevoke(t *testing.T) {
	m := revocationTestManager()
	// A token with no iat cannot be shown to postdate the cutoff, so it is the
	// case that has to fail closed rather than the one that gets the benefit of
	// the doubt.
	claims := &Claims{
		UserID:   "u-no-iat",
		Username: "no-iat",
		Role:     "viewer",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Subject:   "u-no-iat",
			ID:        "jti-no-iat",
		},
	}
	token, err := jwt.NewWithClaims(jwt.GetSigningMethod("HS256"), claims).SignedString(m.secretKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := m.VerifyToken(token); err != nil {
		t.Fatalf("the same token must verify before any revoke: %v", err)
	}

	RevokeUserTokens("u-no-iat", time.Now())
	if _, err := m.VerifyToken(token); !errors.Is(err, ErrTokenRevokedForUser) {
		t.Fatalf("VerifyToken for an iat-less token after revoke = %v, want ErrTokenRevokedForUser", err)
	}
}

func TestRevokeUserTokensIgnoresAnEmptyUserID(t *testing.T) {
	m := revocationTestManager()
	before := mintAccess(t, m, "u-after-empty-revoke")
	RevokeUserTokens("", time.Now().Add(2*time.Second))

	if _, ok := UserTokenCutoff(""); ok {
		t.Fatal("an empty user id must not record a cutoff: it would be compared against tokens that carry no subject")
	}
	if _, err := m.VerifyToken(before); err != nil {
		t.Fatalf("revoking the empty user id broke a real user: %v", err)
	}

	// A token carrying no subject at all is not "revoked", it is simply not
	// matchable — the jti list stays the mechanism for those, and rejecting every
	// subject-less token here would also break the logout path, which verifies a
	// token precisely in order to revoke it.
	anonymous, err := jwt.NewWithClaims(jwt.GetSigningMethod("HS256"), &Claims{
		Role: "viewer",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        "jti-anonymous",
		},
	}).SignedString(m.secretKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := m.VerifyToken(anonymous); err != nil {
		t.Fatalf("a token with no subject must still verify on its own signature: %v", err)
	}
}

func TestUserCutoffIsStoredAtTheResolutionItIsComparedAt(t *testing.T) {
	// VerifyToken compares a unix-second iat against the cutoff. Storing the
	// sub-second remainder would make the comparison finer than the data, and a
	// token minted in the same second as the revoke would be rejected.
	stamp := time.Now()
	stamp = time.Date(stamp.Year(), stamp.Month(), stamp.Day(), stamp.Hour(), stamp.Minute(), stamp.Second(), 678_000_000, stamp.Location())
	RevokeUserTokens("u-truncated", stamp)

	got, ok := UserTokenCutoff("u-truncated")
	if !ok {
		t.Fatal("no cutoff recorded")
	}
	if want := stamp.Truncate(time.Second); !got.Equal(want) {
		t.Fatalf("cutoff = %v, want it truncated to %v", got, want)
	}

	m := revocationTestManager()
	// iat == the cutoff second, i.e. a login performed inside the same second.
	fresh := mintAccessAt(t, m, "u-truncated", stamp)
	if _, err := m.VerifyToken(fresh); err != nil {
		t.Fatalf("a token issued in the cutoff's own second must survive: %v", err)
	}
	stale := mintAccessAt(t, m, "u-truncated", stamp.Add(-time.Second))
	if _, err := m.VerifyToken(stale); !errors.Is(err, ErrTokenRevokedForUser) {
		t.Fatalf("a token from the second before = %v, want ErrTokenRevokedForUser", err)
	}
}

func mintAccessAt(t *testing.T, m *JWTManager, userID string, issuedAt time.Time) string {
	t.Helper()
	claims := &Claims{
		UserID:   userID,
		Username: "user-" + userID,
		Role:     "viewer",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(issuedAt.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			Subject:   userID,
			ID:        "jti-" + userID + "-" + issuedAt.String(),
		},
	}
	token, err := jwt.NewWithClaims(jwt.GetSigningMethod(m.algorithm), claims).SignedString(m.secretKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

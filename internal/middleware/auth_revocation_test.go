package middleware

// AuthMiddleware verifies tokens through security.JWTManager, which is where the
// per-user revocation cutoff is enforced. These tests pin that a revoke really
// takes effect on the request path (the HTTP layer is what an attacker's stolen
// token keeps using) and that a token minted afterwards — the re-login the client
// performs right after a password change — still gets through.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/security"
)

func revocationJWT(t *testing.T) *security.JWTManager {
	t.Helper()
	prev := config.GetConfig()
	cfg := *prev
	cfg.Security.SecretKey = "middleware-revocation-test-secret-32ch"
	cfg.Security.Algorithm = "HS256"
	if cfg.Security.AccessTokenExpireMinutes <= 0 {
		cfg.Security.AccessTokenExpireMinutes = 30
	}
	config.SetGlobalConfig(&cfg)
	security.ResetJWTManagerForTest()
	t.Cleanup(func() {
		config.SetGlobalConfig(prev)
		security.ResetJWTManagerForTest()
	})
	return security.GetJWTManager()
}

func accessToken(t *testing.T, m *security.JWTManager, userID, username string) string {
	t.Helper()
	token, _, err := m.GenerateAccessToken(userID, username, "operator")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	return token
}

func passThroughAuth(t *testing.T, token string) (echo.Context, bool, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	c := e.NewContext(req, httptest.NewRecorder())

	called := false
	err := AuthMiddleware()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})(c)
	// The middleware is driven directly here, so nothing renders its return value
	// into a status code; the *echo.HTTPError is what Echo would turn into a 401.
	return c, called, err
}

func assertUnauthorized(t *testing.T, err error, called bool) {
	t.Helper()
	if called {
		t.Fatal("the route handler ran with a revoked token")
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("AuthMiddleware returned %#v, want an echo 401", err)
	}
	if httpErr.Code != http.StatusUnauthorized {
		t.Fatalf("AuthMiddleware failed with %d, want 401", httpErr.Code)
	}
}

func TestAuthMiddlewareRejectsTokensIssuedBeforeAUserRevoke(t *testing.T) {
	m := revocationJWT(t)
	token := accessToken(t, m, "u-mw-revoked", "revokeduser")
	security.RevokeUserTokens("u-mw-revoked", time.Now().Add(2*time.Second))

	_, called, err := passThroughAuth(t, token)
	assertUnauthorized(t, err, called)
}

func TestAuthMiddlewareAcceptsTokensIssuedAfterAUserRevoke(t *testing.T) {
	m := revocationJWT(t)
	security.RevokeUserTokens("u-mw-refilled", time.Now().Add(-2*time.Second))
	token := accessToken(t, m, "u-mw-refilled", "refilleduser")

	c, called, err := passThroughAuth(t, token)
	if err != nil || !called {
		t.Fatalf("err = %v handler-called = %v, want the re-login path to reach the route", err, called)
	}
	if c.Get("user_id") != "u-mw-refilled" {
		t.Fatalf("user_id context = %#v", c.Get("user_id"))
	}
}

func TestAuthMiddlewareRevokesThroughTheCookiePathToo(t *testing.T) {
	// After a page refresh the browser has no in-memory token and authenticates
	// with the HttpOnly cookie alone, so a cutoff that only the header path
	// honoured would leave the refreshed tab fully authorized.
	m := revocationJWT(t)
	token := accessToken(t, m, "u-mw-cookie", "cookieuser")
	security.RevokeUserTokens("u-mw-cookie", time.Now().Add(2*time.Second))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	req.AddCookie(&http.Cookie{Name: "edgelite_access", Value: token})
	c := e.NewContext(req, httptest.NewRecorder())

	called := false
	err := AuthMiddleware()(func(c echo.Context) error {
		called = true
		return nil
	})(c)
	assertUnauthorized(t, err, called)
}

func TestOptionalAuthMiddlewareDropsARevokedUsersIdentity(t *testing.T) {
	m := revocationJWT(t)
	token := accessToken(t, m, "u-mw-optional", "optionaluser")
	security.RevokeUserTokens("u-mw-optional", time.Now().Add(2*time.Second))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	reached := false
	if err := OptionalAuthMiddleware()(func(c echo.Context) error {
		reached = true
		return c.String(http.StatusOK, "ok")
	})(c); err != nil {
		t.Fatalf("optional auth must let the request through: %v", err)
	}
	if !reached {
		t.Fatal("optional auth must not block the request")
	}
	if c.Get("user_id") != nil {
		t.Fatalf("a revoked token must not populate the user context, got %#v", c.Get("user_id"))
	}
}

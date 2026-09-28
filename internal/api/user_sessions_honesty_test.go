package api

// GET /users/:user_id/sessions answered `[]` and DELETE answered "All sessions
// revoked" while revoking nothing: a token minted before the DELETE stayed valid
// until it expired, so an admin who reset a compromised account's password left
// the attacker on the line and was told the opposite. Both handlers now work off
// the per-user cutoff in internal/security, and the list handler refuses instead
// of implying nobody is logged in.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

const sessionTestSecret = "user-session-revocation-test-secret-32b"

func useUserSessionStore(t *testing.T) *storage.UserRepo {
	t.Helper()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "main.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	cont := GetContainer()
	prevDB, prevUsers := cont.Database, cont.UserRepo
	cont.Database, cont.UserRepo = db, storage.NewUserRepo(db)
	t.Cleanup(func() {
		cont.Database, cont.UserRepo = prevDB, prevUsers
		_ = db.Close()
	})
	return cont.UserRepo
}

// useSessionJWT installs a JWT manager with a known key so the tokens these
// tests mint are the ones VerifyToken is asked about, then restores whatever
// configuration the rest of the package was left with.
func useSessionJWT(t *testing.T) *security.JWTManager {
	t.Helper()
	prev := config.GetConfig()
	cfg := *prev
	cfg.Security.SecretKey = sessionTestSecret
	cfg.Security.Algorithm = "HS256"
	if cfg.Security.AccessTokenExpireMinutes <= 0 {
		cfg.Security.AccessTokenExpireMinutes = 30
	}
	if cfg.Security.RefreshTokenExpireDays <= 0 {
		cfg.Security.RefreshTokenExpireDays = 7
	}
	config.SetGlobalConfig(&cfg)
	security.ResetJWTManagerForTest()
	t.Cleanup(func() {
		config.SetGlobalConfig(prev)
		security.ResetJWTManagerForTest()
	})
	return security.GetJWTManager()
}

func seedSessionUser(t *testing.T, repo *storage.UserRepo, userID, username, password string) {
	t.Helper()
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := repo.Create(userID, username, hash, "viewer"); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
}

func mintSession(t *testing.T, m *security.JWTManager, userID, username string) string {
	t.Helper()
	token, _, err := m.GenerateAccessToken(userID, username, "viewer")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	return token
}

// crossSecondBoundary waits until the clock has left `from`'s whole second. JWT
// iat has second resolution and a revoke truncated to the same second cannot
// distinguish a token minted moments before from one minted moments after, so a
// test that wants the revoke to bite has to cross that boundary rather than
// assume it already happened.
func crossSecondBoundary(t *testing.T, from time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Truncate(time.Second).Equal(from.Truncate(time.Second)) {
		if time.Now().After(deadline) {
			t.Fatal("wall clock did not advance a second")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type sessionEnvelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	ErrorCode string          `json:"error_code"`
}

func callUserHandler(t *testing.T, h func(echo.Context) error, method, path, userID, body, actorID, actorName string) sessionEnvelope {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("user_id")
	c.SetParamValues(userID)
	if actorID != "" {
		c.Set("user", &UserContext{UserID: actorID, Username: actorName, Role: "admin"})
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env sessionEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body.String(), err)
	}
	env.Code = rec.Code
	return env
}

func (e sessionEnvelope) dataMap(t *testing.T) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("data is not an object (%s): %v", string(e.Data), err)
	}
	return m
}

func TestListUserSessionsRefusesInsteadOfAnsweringEmpty(t *testing.T) {
	useUserSessionStore(t)
	env := callUserHandler(t, handleListUserSessions, http.MethodGet,
		"/api/v1/users/u-list/sessions", "u-list", "", "u-admin", "adminuser")
	if env.Code != http.StatusNotImplemented || env.ErrorCode != "ERR_USER_SESSIONS_UNSUPPORTED" {
		t.Fatalf("status = %d error_code = %q, want 501/ERR_USER_SESSIONS_UNSUPPORTED", env.Code, env.ErrorCode)
	}
	// `[]` was the whole problem: it is indistinguishable from "nobody is logged
	// in", which is exactly the reassurance a 501 must not give.
	if trimmed := strings.TrimSpace(string(env.Data)); trimmed != "null" {
		t.Fatalf("501 must carry no session list, got %s", trimmed)
	}
}

func TestDeleteUserSessionsWithoutAUserStoreRefuses(t *testing.T) {
	cont := GetContainer()
	prev := cont.UserRepo
	cont.UserRepo = nil
	t.Cleanup(func() { cont.UserRepo = prev })

	env := callUserHandler(t, handleDeleteUserSessions, http.MethodDelete,
		"/api/v1/users/u-nobody/sessions", "u-nobody", "", "u-admin", "adminuser")
	if env.Code != http.StatusServiceUnavailable || env.ErrorCode != "ERR_COMMON_SERVICE_NOT_READY" {
		t.Fatalf("status = %d error_code = %q, want 503/ERR_COMMON_SERVICE_NOT_READY", env.Code, env.ErrorCode)
	}
	if _, ok := security.UserTokenCutoff("u-nobody"); ok {
		t.Fatal("a refused revoke must not record a cutoff")
	}
}

func TestDeleteUserSessionsUnknownUserIs404AndRecordsNothing(t *testing.T) {
	repo := useUserSessionStore(t)
	seedSessionUser(t, repo, "u-real", "realperson", "Real!Pass123")

	env := callUserHandler(t, handleDeleteUserSessions, http.MethodDelete,
		"/api/v1/users/u-ghost/sessions", "u-ghost", "", "u-admin", "adminuser")
	if env.Code != http.StatusNotFound || env.ErrorCode != "ERR_USER_NOT_FOUND" {
		t.Fatalf("status = %d error_code = %q, want 404/ERR_USER_NOT_FOUND", env.Code, env.ErrorCode)
	}
	if _, ok := security.UserTokenCutoff("u-ghost"); ok {
		t.Fatal("a revoke of a user that does not exist must not record a cutoff")
	}
}

func TestDeleteUserSessionsRevokesTheTokensItClaimsToHaveRevoked(t *testing.T) {
	repo := useUserSessionStore(t)
	m := useSessionJWT(t)
	seedSessionUser(t, repo, "u-victim", "victimone", "Victim!Pass123")

	issued := time.Now()
	oldToken := mintSession(t, m, "u-victim", "victimone")
	crossSecondBoundary(t, issued)

	env := callUserHandler(t, handleDeleteUserSessions, http.MethodDelete,
		"/api/v1/users/u-victim/sessions", "u-victim", "", "u-admin", "adminuser")
	if env.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", env.Code, env.Message)
	}
	data := env.dataMap(t)
	if data["user_id"] != "u-victim" {
		t.Fatalf("user_id = %#v, want the revoked user's own id", data["user_id"])
	}
	if data["persistent"] != false {
		t.Fatalf("persistent = %#v: the cutoff lives in process memory, so a restart drops it and the response must say so", data["persistent"])
	}
	before, ok := security.UserTokenCutoff("u-victim")
	if !ok {
		t.Fatal("no cutoff recorded for the user whose sessions were revoked")
	}
	reported, err := time.Parse(time.RFC3339, data["revoked_before"].(string))
	if err != nil {
		t.Fatalf("revoked_before = %#v is not RFC3339: %v", data["revoked_before"], err)
	}
	if !reported.Equal(before) {
		t.Fatalf("revoked_before = %v but the cutoff actually stored is %v", reported, before)
	}

	if _, err := m.VerifyToken(oldToken); !errors.Is(err, security.ErrTokenRevokedForUser) {
		t.Fatalf("the token the handler claimed to revoke still verifies: %v", err)
	}
	// The client re-authenticates after a revoke, so a token minted afterwards has
	// to keep working or the endpoint would lock the user out of the account.
	if _, err := m.VerifyToken(mintSession(t, m, "u-victim", "victimone")); err != nil {
		t.Fatalf("a token minted after the revoke must verify: %v", err)
	}
}

func TestAdminPasswordResetRevokesTheTargetsSessionsNotTheAdmins(t *testing.T) {
	repo := useUserSessionStore(t)
	m := useSessionJWT(t)
	seedSessionUser(t, repo, "u-target", "targetuser", "Target!Pass123")
	issued := time.Now()
	targetToken := mintSession(t, m, "u-target", "targetuser")
	adminToken := mintSession(t, m, "u-admin", "adminuser")
	crossSecondBoundary(t, issued)

	env := callUserHandler(t, handleAdminResetPassword, http.MethodPost,
		"/api/v1/users/u-target/reset-password", "u-target", `{"new_password":"Fresh!Pass987"}`, "u-admin", "adminuser")
	if env.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", env.Code, env.Message)
	}
	if _, err := m.VerifyToken(targetToken); !errors.Is(err, security.ErrTokenRevokedForUser) {
		t.Fatalf("a password reset left the target's old token alive: %v", err)
	}
	if _, err := m.VerifyToken(adminToken); err != nil {
		t.Fatalf("revoking the target must not revoke the admin performing the reset: %v", err)
	}
}

func TestUpdateUserPasswordRevokesTheTargetsSessions(t *testing.T) {
	repo := useUserSessionStore(t)
	m := useSessionJWT(t)
	seedSessionUser(t, repo, "u-edited", "editeduser", "Edited!Pass123")
	issued := time.Now()
	oldToken := mintSession(t, m, "u-edited", "editeduser")
	crossSecondBoundary(t, issued)

	env := callUserHandler(t, handleUpdateUser, http.MethodPut,
		"/api/v1/users/u-edited", "u-edited", `{"password":"Edited!Pass456"}`, "u-admin", "adminuser")
	if env.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", env.Code, env.Message)
	}
	if _, err := m.VerifyToken(oldToken); !errors.Is(err, security.ErrTokenRevokedForUser) {
		t.Fatalf("an admin password edit left the old token alive: %v", err)
	}
}

func TestUserPasswordChangeRevokesItsOwnOldToken(t *testing.T) {
	repo := useUserSessionStore(t)
	m := useSessionJWT(t)
	seedSessionUser(t, repo, "u-self", "selfuser", "Old!Pass1234")

	issued := time.Now()
	oldToken := mintSession(t, m, "u-self", "selfuser")
	crossSecondBoundary(t, issued)

	env := callUserHandler(t, handleChangePassword, http.MethodPost,
		"/api/v1/auth/change-password", "", `{"old_password":"Old!Pass1234","new_password":"Brand!Pass99"}`,
		"u-self", "selfuser")
	if env.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", env.Code, env.Message)
	}
	if _, err := m.VerifyToken(oldToken); !errors.Is(err, security.ErrTokenRevokedForUser) {
		t.Fatalf("changing a password left the session that made the call's old token usable elsewhere: %v", err)
	}
	// Both UI paths re-login straight after this response, and that fresh token is
	// minted after the cutoff, so the actor stays signed in.
	if _, err := m.VerifyToken(mintSession(t, m, "u-self", "selfuser")); err != nil {
		t.Fatalf("the re-login token issued after a password change must verify: %v", err)
	}
}

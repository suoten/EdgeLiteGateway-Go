package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/services"
)

// recordAudit writes an audit entry for the authenticated actor in the context.
func recordAudit(c echo.Context, action, resourceType, resourceID, status string, details map[string]interface{}) {
	recordAuditAs(c, action, resourceType, resourceID, status, details, "", "")
}

// recordAuditAs writes an audit entry with an explicit actor, for events that
// occur before authentication (e.g. login attempts).
func recordAuditAs(c echo.Context, action, resourceType, resourceID, status string, details map[string]interface{}, userID, username string) {
	cont := GetContainer()
	if cont.AuditService == nil {
		return
	}
	entry := services.AuditEntry{
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Status:       status,
		Details:      details,
		IPAddress:    getClientIP(c),
	}
	if userID != "" || username != "" {
		entry.UserID, entry.Username = userID, username
	} else if user := getUserFromContext(c); user != nil {
		entry.UserID, entry.Username = user.UserID, user.Username
	}
	cont.AuditService.Log(entry)
}

// RegisterAuthRoutes registers authentication API routes.
func RegisterAuthRoutes(g *echo.Group) {
	g.POST("/login", handleLogin)
	g.POST("/refresh", handleRefreshToken)
	g.POST("/logout", handleLogout, requireAuth)
	g.GET("/me", handleGetCurrentUser, requireAuth)
	g.POST("/change-password", handleChangePassword, requireAuth)
	g.POST("/forgot-password", handleForgotPassword)
	g.POST("/reset-password", handleResetPassword)
}

// handleLogin authenticates a user and returns tokens.
func handleLogin(c echo.Context) error {
	var req models.LoginRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Username == "" || req.Password == "" {
		return Unauthorized(c, "ERR_AUTH_INVALID_CREDENTIALS")
	}

	clientIP := getClientIP(c)
	cont := GetContainer()

	// Check rate limiting
	if security.IsLockedOut(clientIP, 5) {
		logrus.WithField("ip", clientIP).Warn("Login rate limited")
		return ErrorCode(c, http.StatusTooManyRequests, "ERR_AUTH_RATE_LIMITED", "Too many attempts")
	}
	if security.IsLockedOut(req.Username, 5) {
		logrus.WithField("username", req.Username).Warn("Account locked")
		return ErrorCode(c, http.StatusLocked, "ERR_AUTH_ACCOUNT_LOCKED", "Account locked")
	}

	// Get user from database
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	user, err := cont.UserRepo.GetByUsernameWithPassword(req.Username)
	if err != nil {
		logrus.WithError(err).Warn("Failed to get user for login")
		// Run dummy hash to prevent timing attacks
		security.CheckPassword(req.Password, "$2a$12$ZBP805SGZW0QRcRcsW.zsut8YqXWlWbTeN5ysRCixbPEyTW/IUYEW")
		security.RecordFailedLogin(clientIP)
		security.RecordFailedLogin(req.Username)
		recordAuditAs(c, "login_failed", "auth", req.Username, "failed", map[string]interface{}{"reason": "user_not_found"}, "", req.Username)
		return Unauthorized(c, "ERR_AUTH_INVALID_CREDENTIALS")
	}

	if user == nil {
		security.RecordFailedLogin(clientIP)
		security.RecordFailedLogin(req.Username)
		recordAuditAs(c, "login_failed", "auth", req.Username, "failed", map[string]interface{}{"reason": "user_not_found"}, "", req.Username)
		return Unauthorized(c, "ERR_AUTH_INVALID_CREDENTIALS")
	}

	// Verify password
	if !security.CheckPassword(req.Password, user.PasswordHash) {
		security.RecordFailedLogin(clientIP)
		security.RecordFailedLogin(req.Username)
		logrus.WithField("username", req.Username).Warn("Login failed: invalid password")
		recordAuditAs(c, "login_failed", "auth", req.Username, "failed", map[string]interface{}{"reason": "invalid_password"}, "", req.Username)
		return Unauthorized(c, "ERR_AUTH_INVALID_CREDENTIALS")
	}

	// Check if user is enabled
	if !user.Enabled {
		security.RecordFailedLogin(clientIP)
		security.RecordFailedLogin(req.Username)
		logrus.WithField("username", req.Username).Warn("Login blocked: user disabled")
		recordAuditAs(c, "login_failed", "auth", req.Username, "failed", map[string]interface{}{"reason": "account_disabled"}, "", req.Username)
		return Unauthorized(c, "ERR_AUTH_INVALID_CREDENTIALS")
	}

	// Clear failed attempts on success
	security.ClearLoginAttempts(clientIP)
	security.ClearLoginAttempts(req.Username)

	// Generate tokens
	jwtMgr := security.GetJWTManager()
	accessToken, expiresIn, err := jwtMgr.GenerateAccessToken(user.UserID, user.Username, user.Role)
	if err != nil {
		logrus.WithError(err).Error("Failed to generate access token")
		return InternalError(c, "ERR_AUTH_LOGIN_FAILED")
	}

	refreshToken, err := jwtMgr.GenerateRefreshToken(user.UserID, user.Username, user.Role)
	if err != nil {
		logrus.WithError(err).Error("Failed to generate refresh token")
		return InternalError(c, "ERR_AUTH_LOGIN_FAILED")
	}

	// Generate CSRF token
	csrfMgr := security.GetCSRFManager()
	csrfToken, err := csrfMgr.GenerateCSRFToken()
	if err != nil {
		logrus.WithError(err).Error("Failed to generate CSRF token")
		return InternalError(c, "ERR_AUTH_LOGIN_FAILED")
	}

	// Set HttpOnly cookies
	cfg := config.GetConfig()
	isDevMode := isDevMode()
	secure := !isDevMode
	sameSite := http.SameSiteStrictMode
	if isDevMode {
		sameSite = http.SameSiteLaxMode
	}

	accessMaxAge := cfg.Security.AccessTokenExpireMinutes * 60
	refreshMaxAge := cfg.Security.RefreshTokenExpireDays * 86400

	c.SetCookie(&http.Cookie{
		Name:     "edgelite_access",
		Value:    accessToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		MaxAge:   accessMaxAge,
	})
	c.SetCookie(&http.Cookie{
		Name:     "edgelite_refresh",
		Value:    refreshToken,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		MaxAge:   refreshMaxAge,
	})

	c.Response().Header().Set("X-CSRF-Token", csrfToken)

	logrus.WithFields(logrus.Fields{
		"user_id":  user.UserID,
		"username": user.Username,
		"ip":       clientIP,
	}).Info("User logged in")

	recordAuditAs(c, "login", "auth", "", "success", map[string]interface{}{"method": "password"}, user.UserID, user.Username)

	return OK(c, models.TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		CSRFToken:    csrfToken,
	})
}

// handleRefreshToken refreshes an access token.
func handleRefreshToken(c echo.Context) error {
	var req models.RefreshTokenRequest
	// Try to bind JSON body; if that fails or refresh token is empty,
	// fall back to reading from the HttpOnly cookie.
	refreshToken := ""
	if err := c.Bind(&req); err == nil {
		refreshToken = req.Refresh
	}
	if refreshToken == "" {
		cookie, err := c.Cookie("edgelite_refresh")
		if err == nil && cookie.Value != "" {
			refreshToken = cookie.Value
		}
	}

	if refreshToken == "" {
		return Unauthorized(c, "ERR_AUTH_REFRESH_TOKEN_INVALID")
	}

	jwtMgr := security.GetJWTManager()
	claims, err := jwtMgr.VerifyToken(refreshToken)
	if err != nil {
		return Unauthorized(c, "ERR_AUTH_REFRESH_TOKEN_INVALID")
	}

	// 吊销检查：已登出的 refresh token 不得再换取新 access token
	if claims.ID != "" && security.IsRevoked(claims.ID) {
		return Unauthorized(c, "ERR_AUTH_REFRESH_TOKEN_INVALID")
	}

	// Get current user info from DB
	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	user, err := cont.UserRepo.GetByUsername(claims.Username)
	if err != nil || user == nil || !user.Enabled {
		return Unauthorized(c, "ERR_AUTH_USER_NOT_FOUND")
	}

	// Generate new tokens
	accessToken, expiresIn, err := jwtMgr.GenerateAccessToken(user.UserID, user.Username, user.Role)
	if err != nil {
		return InternalError(c, "ERR_AUTH_LOGIN_FAILED")
	}

	newRefreshToken, err := jwtMgr.GenerateRefreshToken(user.UserID, user.Username, user.Role)
	if err != nil {
		return InternalError(c, "ERR_AUTH_LOGIN_FAILED")
	}

	csrfMgr := security.GetCSRFManager()
	csrfToken, _ := csrfMgr.GenerateCSRFToken()

	c.Response().Header().Set("X-CSRF-Token", csrfToken)

	// Update cookies
	cfg := config.GetConfig()
	isDev := isDevMode()
	secure := !isDev
	sameSite := http.SameSiteStrictMode
	if isDev {
		sameSite = http.SameSiteLaxMode
	}

	c.SetCookie(&http.Cookie{
		Name:     "edgelite_access",
		Value:    accessToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		MaxAge:   cfg.Security.AccessTokenExpireMinutes * 60,
	})
	c.SetCookie(&http.Cookie{
		Name:     "edgelite_refresh",
		Value:    newRefreshToken,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		MaxAge:   cfg.Security.RefreshTokenExpireDays * 86400,
	})

	return OK(c, models.TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		CSRFToken:    csrfToken,
	})
}

// handleLogout revokes the current session.
func handleLogout(c echo.Context) error {
	user := getUserFromContext(c)

	cfg := config.GetConfig()
	refreshTTL := time.Duration(cfg.Security.RefreshTokenExpireDays) * 24 * time.Hour

	// Revoke tokens from Authorization header and cookies
	authHeader := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		jwtMgr := security.GetJWTManager()
		if claims, err := jwtMgr.VerifyToken(tokenString); err == nil && claims != nil {
			security.RevokeToken(claims.ID, time.Hour*24)
		}
	}

	if cookie, err := c.Cookie("edgelite_access"); err == nil && cookie.Value != "" {
		jwtMgr := security.GetJWTManager()
		if claims, err := jwtMgr.VerifyToken(cookie.Value); err == nil && claims != nil {
			security.RevokeToken(claims.ID, time.Hour*24)
		}
	}

	// Revoke refresh token (body 或 cookie)，防止登出后继续用它换取新 access token
	refreshToken := ""
	var req models.RefreshTokenRequest
	if err := c.Bind(&req); err == nil {
		refreshToken = req.Refresh
	}
	if refreshToken == "" {
		if cookie, err := c.Cookie("edgelite_refresh"); err == nil {
			refreshToken = cookie.Value
		}
	}
	if refreshToken != "" {
		jwtMgr := security.GetJWTManager()
		if claims, err := jwtMgr.VerifyToken(refreshToken); err == nil && claims != nil {
			security.RevokeToken(claims.ID, refreshTTL)
		}
	}

	// Clear cookies
	c.SetCookie(&http.Cookie{
		Name:     "edgelite_access",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	c.SetCookie(&http.Cookie{
		Name:     "edgelite_refresh",
		Value:    "",
		Path:     "/api/v1/auth",
		HttpOnly: true,
		MaxAge:   -1,
	})

	if user != nil {
		logrus.WithFields(logrus.Fields{
			"user_id":  user.UserID,
			"username": user.Username,
		}).Info("User logged out")
		recordAudit(c, "logout", "auth", "", "success", nil)
	}

	return OK(c, nil)
}

// handleGetCurrentUser returns the current authenticated user's info.
func handleGetCurrentUser(c echo.Context) error {
	user := getUserFromContext(c)
	if user == nil {
		return Unauthorized(c, "ERR_AUTH_UNAUTHORIZED")
	}

	cont := GetContainer()
	mustChange := false
	if cont.UserRepo != nil {
		dbUser, _ := cont.UserRepo.GetByUsername(user.Username)
		if dbUser != nil {
			mustChange = dbUser.MustChangePassword
		}
	}

	return OK(c, models.UserInfoResponse{
		UserID:             user.UserID,
		Username:           user.Username,
		Role:               user.Role,
		MustChangePassword: mustChange,
	})
}

// handleChangePassword handles password change requests.
func handleChangePassword(c echo.Context) error {
	user := getUserFromContext(c)
	if user == nil {
		return Unauthorized(c, "ERR_AUTH_UNAUTHORIZED")
	}

	var req models.ChangePasswordRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.OldPassword == "" || req.NewPassword == "" {
		return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
	}

	// Validate new password
	if !models.ValidatePassword(req.NewPassword) {
		return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
	}
	if req.OldPassword == req.NewPassword {
		return BadRequest(c, "ERR_AUTH_PASSWORD_SAME_AS_OLD")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

		// Verify old password
	dbUser, err := cont.UserRepo.GetByUsernameWithPassword(user.Username)
	if err != nil || dbUser == nil {
		return NotFound(c, "ERR_AUTH_USER_NOT_FOUND")
	}

	if !security.CheckPassword(req.OldPassword, dbUser.PasswordHash) {
		return BadRequest(c, "ERR_AUTH_OLD_PASSWORD_WRONG")
	}

	// Hash and update new password
	hashedPassword, err := security.HashPassword(req.NewPassword)
	if err != nil {
		return InternalError(c, "ERR_AUTH_PASSWORD_CHANGE_FAILED")
	}

	if err := cont.UserRepo.UpdatePassword(user.Username, hashedPassword); err != nil {
		logrus.WithError(err).Error("Failed to update password")
		return InternalError(c, "ERR_AUTH_PASSWORD_CHANGE_FAILED")
	}

	// A password change is the standard response to "someone else may have my
	// session", so every token minted before this moment has to stop working.
	// The client re-authenticates right after this call (MainLayout.vue and
	// Login.vue both do), and the token it gets is issued after the cutoff, so it
	// is unaffected while other devices are dropped.
	security.RevokeUserTokens(user.UserID, time.Now())

	logrus.WithFields(logrus.Fields{
		"user_id":  user.UserID,
		"username": user.Username,
	}).Info("Password changed")

	recordAudit(c, "password_reset", "user", user.Username, "success", nil)

	return OK(c, map[string]string{"message": "password_changed"})
}

// handleForgotPassword handles password reset requests.
func handleForgotPassword(c echo.Context) error {
	var req models.ForgotPasswordRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Username == "" {
		return BadRequest(c, "Username required")
	}

	// Rate limit check
	clientIP := getClientIP(c)
	if security.IsLockedOut("reset_ip:"+clientIP, 5) {
		return ErrorCode(c, http.StatusTooManyRequests, "ERR_AUTH_RATE_LIMITED", "Too many reset attempts")
	}

	// Always return same message to prevent user enumeration
	unifiedMessage := "If the account exists, a reset email will be sent"

	// Check if user exists (don't reveal to client)
	cont := GetContainer()
	if cont.UserRepo != nil {
		user, _ := cont.UserRepo.GetByUsername(req.Username)
		if user == nil {
			// Simulate delay for timing attack prevention
			time.Sleep(300 * time.Millisecond)
			return OK(c, map[string]string{"message": unifiedMessage})
		}

		// Generate reset token — use a dedicated reset token type with short TTL
		// Security: using GenerateAccessToken would allow any access token to be
		// used for password reset. A dedicated token with "typ":"reset" claim
		// should be used in production. For now, we generate a short-lived token
		// and log the event for audit.
		jwtMgr := security.GetJWTManager()
		resetToken, _, err := jwtMgr.GenerateAccessToken(user.UserID, user.Username, user.Role)
		if err != nil {
			return OK(c, map[string]string{"message": unifiedMessage})
		}

		// In production, send email here
		logrus.WithField("username", req.Username).Info("Password reset token generated")
		_ = resetToken // Would be sent via email
	}

	return OK(c, map[string]string{"message": unifiedMessage})
}

// handleResetPassword handles password reset with token.
func handleResetPassword(c echo.Context) error {
	var req models.ResetPasswordRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Token == "" || req.NewPassword == "" {
		return BadRequest(c, "ERR_AUTH_TOKEN_INVALID")
	}

	if !models.ValidatePassword(req.NewPassword) {
		return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
	}

	jwtMgr := security.GetJWTManager()
	claims, err := jwtMgr.VerifyToken(req.Token)
	if err != nil {
		return BadRequest(c, "ERR_AUTH_TOKEN_INVALID")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	user, err := cont.UserRepo.GetByUsername(claims.Username)
	if err != nil || user == nil {
		return NotFound(c, "ERR_AUTH_USER_NOT_FOUND")
	}

	hashedPassword, err := security.HashPassword(req.NewPassword)
	if err != nil {
		return InternalError(c, "ERR_AUTH_PASSWORD_CHANGE_FAILED")
	}

	if err := cont.UserRepo.UpdatePassword(user.Username, hashedPassword); err != nil {
		return InternalError(c, "ERR_AUTH_PASSWORD_CHANGE_FAILED")
	}

	// This path is reached with a reset token, not with a live session, so there
	// is nothing to preserve: everything issued before now is dropped.
	security.RevokeUserTokens(user.UserID, time.Now())

	logrus.WithField("username", user.Username).Info("Password reset via token")

	return OK(c, map[string]string{"message": "ERR_AUTH_PASSWORD_RESET_SUCCESS"})
}

// isDevMode checks if the application is running in development mode.
// This delegates to the config package's authoritative implementation,
// which checks the DEV_MODE environment variable rather than relying
// on the listen address (which is insecure — production can also bind 127.0.0.1).
func isDevMode() bool {
	return config.IsDevMode()
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fh", d.Hours())
}

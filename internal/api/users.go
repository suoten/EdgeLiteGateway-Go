package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// RegisterUserRoutes registers user management API routes.
func RegisterUserRoutes(g *echo.Group) {
	g.GET("", handleListUsers, requirePermission(security.PermUserRead))
	g.POST("", handleCreateUser, requirePermission(security.PermUserCreate))
	g.GET("/me", handleGetCurrentUserExtended, requireAuth)
	g.GET("/:user_id", handleGetUser, requirePermission(security.PermUserRead))
	g.PUT("/:user_id", handleUpdateUser, requirePermission(security.PermUserUpdate))
	g.DELETE("/:user_id", handleDeleteUser, requirePermission(security.PermUserDelete))
	g.POST("/:user_id/reset-password", handleAdminResetPassword, requirePermission(security.PermUserUpdate))
	g.GET("/:user_id/sessions", handleListUserSessions, requirePermission(security.PermUserRead))
	g.DELETE("/:user_id/sessions", handleDeleteUserSessions, requirePermission(security.PermUserUpdate))
}

func handleListUsers(c echo.Context) error {
	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	page, size := parsePagination(c)

	users, total, err := cont.UserRepo.List(page, size)
	if err != nil {
		logrus.WithError(err).Error("List users failed")
		return InternalError(c, "ERR_USER_LIST_FAILED")
	}

	// Convert to response (strip password hash)
	result := make([]models.UserResponse, 0, len(users))
	for _, u := range users {
		result = append(result, toUserResponse(&u))
	}

	return OKPaged(c, result, total, page, size)
}

func handleCreateUser(c echo.Context) error {
	var req models.UserCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if !models.ValidateUsername(req.Username) {
		return BadRequest(c, "ERR_USER_USERNAME_INVALID")
	}
	if !models.ValidatePassword(req.Password) {
		return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
	}

	// Validate role
	validRoles := map[string]bool{"admin": true, "operator": true, "viewer": true}
	if !validRoles[req.Role] {
		return BadRequest(c, "ERR_USER_ROLE_INVALID")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	// Check if user already exists
	existing, _ := cont.UserRepo.GetByUsername(req.Username)
	if existing != nil {
		return Conflict(c, "ERR_USER_ALREADY_EXISTS")
	}

	// Hash password
	hashedPassword, err := security.HashPassword(req.Password)
	if err != nil {
		return InternalError(c, "ERR_USER_CREATE_FAILED")
	}

	userID := fmt.Sprintf("u-%s", uuid.New().String()[:8])
	if err := cont.UserRepo.Create(userID, req.Username, hashedPassword, req.Role); err != nil {
		logrus.WithError(err).Error("Create user failed")
		return InternalError(c, "ERR_USER_CREATE_FAILED")
	}

	user, _ := cont.UserRepo.GetByID(userID)
	resp := toUserResponse(user)

	logrus.WithFields(logrus.Fields{
		"user_id":  userID,
		"username": req.Username,
		"role":     req.Role,
	}).Info("User created")

	recordAudit(c, "user_create", "user", req.Username, "success", map[string]interface{}{"user_id": userID, "role": req.Role})

	return Created(c, resp)
}

func handleGetCurrentUserExtended(c echo.Context) error {
	user := getUserFromContext(c)
	if user == nil {
		return Unauthorized(c, "ERR_AUTH_UNAUTHORIZED")
	}

	cont := GetContainer()
	if cont.UserRepo != nil {
		dbUser, _ := cont.UserRepo.GetByID(user.UserID)
		if dbUser != nil {
			return OK(c, toUserResponse(dbUser))
		}
	}

	return OK(c, models.UserInfoResponse{
		UserID:   user.UserID,
		Username: user.Username,
		Role:     user.Role,
	})
}

func handleGetUser(c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return BadRequest(c, "User ID required")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	user, err := cont.UserRepo.GetByID(userID)
	if err != nil || user == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	return OK(c, toUserResponse(user))
}

func handleUpdateUser(c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return BadRequest(c, "User ID required")
	}

	var req models.UserUpdate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	// Check if user exists
	existing, err := cont.UserRepo.GetByID(userID)
	if err != nil || existing == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	// Validate role if changing
	if req.Role != nil {
		validRoles := map[string]bool{"admin": true, "operator": true, "viewer": true}
		if !validRoles[*req.Role] {
			return BadRequest(c, "ERR_USER_ROLE_INVALID")
		}
		// Prevent demoting the last admin
		if existing.Role == "admin" && *req.Role != "admin" {
			// Check if this is the last admin
			users, _, _ := cont.UserRepo.List(1, 1000)
			adminCount := 0
			for _, u := range users {
				if u.Role == "admin" && u.Enabled {
					adminCount++
				}
			}
			if adminCount <= 1 {
				return BadRequest(c, "ERR_USER_LAST_ADMIN")
			}
		}
	}

	// Update password if provided
	if req.Password != nil {
		if !models.ValidatePassword(*req.Password) {
			return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
		}
		hashedPassword, err := security.HashPassword(*req.Password)
		if err != nil {
			return InternalError(c, "ERR_USER_UPDATE_FAILED")
		}
		if err := cont.UserRepo.UpdatePasswordByID(userID, hashedPassword); err != nil {
			return InternalError(c, "ERR_USER_UPDATE_FAILED")
		}
		// Same reasoning as the admin reset: whoever held a token minted with the
		// old password must not keep holding access.
		security.RevokeUserTokens(userID, time.Now())
	}

	// Update role/enabled
	if err := cont.UserRepo.Update(userID, req.Role, req.Enabled); err != nil {
		logrus.WithError(err).Error("Update user failed")
		return InternalError(c, "ERR_USER_UPDATE_FAILED")
	}

	user, _ := cont.UserRepo.GetByID(userID)

	details := map[string]interface{}{}
	if req.Role != nil {
		details["role"] = *req.Role
	}
	if req.Enabled != nil {
		details["enabled"] = *req.Enabled
	}
	if req.Password != nil {
		details["password"] = "changed"
	}
	recordAudit(c, "user_update", "user", existing.Username, "success", details)

	return OK(c, toUserResponse(user))
}

func handleDeleteUser(c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return BadRequest(c, "User ID required")
	}

	currentUser := getUserFromContext(c)
	if currentUser != nil && currentUser.UserID == userID {
		return BadRequest(c, "ERR_USER_CANNOT_DELETE_SELF")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	// Check if user exists
	existing, err := cont.UserRepo.GetByID(userID)
	if err != nil || existing == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	// Prevent deleting the last admin
	if existing.Role == "admin" {
		users, _, _ := cont.UserRepo.List(1, 1000)
		adminCount := 0
		for _, u := range users {
			if u.Role == "admin" && u.Enabled {
				adminCount++
			}
		}
		if adminCount <= 1 {
			return BadRequest(c, "ERR_USER_LAST_ADMIN")
		}
	}

	if err := cont.UserRepo.Delete(userID); err != nil {
		return InternalError(c, "ERR_USER_DELETE_FAILED")
	}

	logrus.WithField("user_id", userID).Info("User deleted")
	recordAudit(c, "user_delete", "user", existing.Username, "success", map[string]interface{}{"user_id": userID})
	return OK(c, nil)
}

func handleAdminResetPassword(c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return BadRequest(c, "User ID required")
	}

	type ResetRequest struct {
		NewPassword string `json:"new_password"`
	}
	var req ResetRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if !models.ValidatePassword(req.NewPassword) {
		return BadRequest(c, "ERR_AUTH_PASSWORD_POLICY")
	}

	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}

	existing, _ := cont.UserRepo.GetByID(userID)
	if existing == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	hashedPassword, err := security.HashPassword(req.NewPassword)
	if err != nil {
		return InternalError(c, "ERR_USER_PASSWORD_RESET_FAILED")
	}

	if err := cont.UserRepo.UpdatePasswordByID(userID, hashedPassword); err != nil {
		return InternalError(c, "ERR_USER_PASSWORD_RESET_FAILED")
	}

	// A password reset is also the response to a compromised account, so the
	// sessions the attacker holds have to go with it. The admin performing this
	// call authenticates with their own user id, which this cutoff does not touch.
	security.RevokeUserTokens(existing.UserID, time.Now())

	logrus.WithField("user_id", userID).Info("Admin reset user password")
	recordAudit(c, "password_reset", "user", existing.Username, "success", map[string]interface{}{"user_id": userID, "by": "admin"})
	return OK(c, map[string]string{"message": "Password reset successfully"})
}

// handleListUserSessions answers with 501 rather than an empty list.
//
// An empty array would be a lie in two directions: nothing in this gateway keeps
// a session row (the `sessions` table is created by the schema but never
// written), and a client that rendered [] would tell an admin a user is idle
// while their token is still being accepted on every request. Tokens are only
// observable through revocation, which is what DELETE below performs.
func handleListUserSessions(c echo.Context) error {
	return ErrorCode(c, http.StatusNotImplemented, "ERR_USER_SESSIONS_UNSUPPORTED",
		"Tokens are stateless, so active sessions cannot be listed; use DELETE to revoke every token issued before now")
}

// handleDeleteUserSessions revokes every token the user currently holds by
// recording a cutoff timestamp: tokens whose iat falls before it stop verifying.
// The cutoff lives in process memory, exactly like the per-token revocation list
// logout uses, so `persistent` says out loud that a gateway restart clears it.
func handleDeleteUserSessions(c echo.Context) error {
	userID := c.Param("user_id")
	cont := GetContainer()
	if cont.UserRepo == nil {
		return ServiceUnavailable(c, "Database not ready")
	}
	existing, err := cont.UserRepo.GetByID(userID)
	if err != nil || existing == nil {
		return NotFound(c, "ERR_USER_NOT_FOUND")
	}

	cutoff := time.Now().Truncate(time.Second)
	security.RevokeUserTokens(existing.UserID, cutoff)
	recordAudit(c, "session_revoke", "user", existing.Username, "success",
		map[string]interface{}{"user_id": existing.UserID, "revoked_before": cutoff.UTC().Format(time.RFC3339)})

	return OK(c, map[string]interface{}{
		"user_id":        existing.UserID,
		"revoked_before": cutoff.UTC().Format(time.RFC3339),
		"persistent":     false,
	})
}

// toUserResponse converts a UserRecord to a UserResponse (without password hash).
func toUserResponse(u *storage.UserRecord) models.UserResponse {
	if u == nil {
		return models.UserResponse{}
	}
	return models.UserResponse{
		UserID:             u.UserID,
		Username:           u.Username,
		Role:               u.Role,
		Enabled:            u.Enabled,
		MustChangePassword: u.MustChangePassword,
		PasswordChangedAt:  u.PasswordChangedAt,
		CreatedAt:          u.CreatedAt,
		UpdatedAt:          u.UpdatedAt,
		Version:            u.Version,
	}
}

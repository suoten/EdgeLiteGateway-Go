package models

import "regexp"

// UserCreate represents a create user request.
type UserCreate struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"` // admin, operator, viewer
}

// UserUpdate represents an update user request.
type UserUpdate struct {
	Password *string `json:"password,omitempty"`
	Role     *string `json:"role,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// UserResponse represents a user response.
type UserResponse struct {
	UserID             string `json:"user_id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	Enabled            bool   `json:"enabled"`
	MustChangePassword bool   `json:"must_change_password"`
	PasswordChangedAt  string `json:"password_changed_at,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at,omitempty"`
	Version            int    `json:"version"`
}

// LoginRequest represents a login request.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TokenResponse represents a token response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	CSRFToken    string `json:"csrf_token,omitempty"`
}

// UserInfoResponse represents the current user info response.
type UserInfoResponse struct {
	UserID             string `json:"user_id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
}

// RefreshTokenRequest represents a refresh token request.
type RefreshTokenRequest struct {
	Refresh string `json:"refresh,omitempty"`
}

// ChangePasswordRequest represents a change password request.
type ChangePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ForgotPasswordRequest represents a forgot password request.
type ForgotPasswordRequest struct {
	Username string `json:"username"`
}

// ResetPasswordRequest represents a reset password request.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// LogoutRequest represents a logout request.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

var (
	passwordLetterRegex  = regexp.MustCompile(`[a-zA-Z]`)
	passwordDigitRegex   = regexp.MustCompile(`\d`)
	passwordSpecialRegex = regexp.MustCompile(`[!@#$%^&*()_+\-=\[\]{}|;':\",.<>?` + "`" + `~]`)
	usernameValidRegex   = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
)

// ValidatePassword checks password complexity: must contain letters, digits, and special chars.
func ValidatePassword(password string) bool {
	if len(password) < 8 || len(password) > 72 {
		return false
	}
	return passwordLetterRegex.MatchString(password) &&
		passwordDigitRegex.MatchString(password) &&
		passwordSpecialRegex.MatchString(password)
}

// ValidateUsername checks username format.
func ValidateUsername(username string) bool {
	return len(username) >= 3 && len(username) <= 32 && usernameValidRegex.MatchString(username)
}

package api

import (
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"edgelite/internal/constants"
	"edgelite/internal/security"
)

// UserContext holds the authenticated user info extracted from JWT.
type UserContext struct {
	UserID   string
	Username string
	Role     string
}

// getUserFromContext extracts the authenticated user from Echo context.
// Returns nil if not authenticated.
func getUserFromContext(c echo.Context) *UserContext {
	// Try to get from context (set by auth middleware)
	if val := c.Get("user"); val != nil {
		if user, ok := val.(*UserContext); ok {
			return user
		}
		if m, ok := val.(map[string]string); ok {
			return &UserContext{
				UserID:   m["user_id"],
				Username: m["username"],
				Role:     m["role"],
			}
		}
	}

	// Fallback: parse from Authorization header
	authHeader := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		jwtMgr := security.GetJWTManager()
		claims, err := jwtMgr.VerifyToken(tokenString)
		if err == nil && claims != nil {
			return &UserContext{
				UserID:   claims.UserID,
				Username: claims.Username,
				Role:     claims.Role,
			}
		}
	}

	// Fallback: try cookie
	cookie, err := c.Cookie("edgelite_access")
	if err == nil && cookie.Value != "" {
		jwtMgr := security.GetJWTManager()
		claims, err := jwtMgr.VerifyToken(cookie.Value)
		if err == nil && claims != nil {
			return &UserContext{
				UserID:   claims.UserID,
				Username: claims.Username,
				Role:     claims.Role,
			}
		}
	}

	return nil
}

// requireAuth is a middleware that ensures the user is authenticated.
func requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		user := getUserFromContext(c)
		if user == nil {
			return Unauthorized(c, "Authentication required")
		}
		c.Set("user", user)
		return next(c)
	}
}

// requirePermission returns middleware that checks if the user has a specific permission.
func requirePermission(perm security.Permission) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := getUserFromContext(c)
			if user == nil {
				return Unauthorized(c, "Authentication required")
			}
			if !security.HasPermission(user.Role, perm) {
				return Forbidden(c, "Permission denied")
			}
			c.Set("user", user)
			return next(c)
		}
	}
}

// getClientIP extracts the real client IP with trusted proxy support.
func getClientIP(c echo.Context) string {
	// Direct client IP from Echo
	return c.RealIP()
}

// parsePagination extracts and validates page/size query parameters.
// Defaults: page=1, size=20. Size is capped at constants.MaxQuerySize
// to prevent DoS via excessively large page requests.
func parsePagination(c echo.Context) (int, int) {
	page, _ := strconv.Atoi(c.QueryParam("page"))
	size, _ := strconv.Atoi(c.QueryParam("size"))
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > constants.MaxQuerySize {
		size = constants.MaxQuerySize
	}
	return page, size
}

// paginateSlice pages a set that was already narrowed in memory, so that
// filtering and paging agree on the same rows.
func paginateSlice[T any](items []T, page, size int) []T {
	start := (page - 1) * size
	if start >= len(items) {
		return nil
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

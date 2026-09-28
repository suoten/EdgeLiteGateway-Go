// Package middleware provides HTTP middleware for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/middleware/ package.
// It includes CSRF protection, rate limiting, request ID injection,
// token renewal, and security response headers.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/security"
)

// RequestIDMiddleware injects a unique request ID into each request.
func RequestIDMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			reqID := c.Request().Header.Get("X-Request-ID")
			if reqID == "" {
				reqID = generateRequestID()
			}
			c.Request().Header.Set("X-Request-ID", reqID)
			c.Response().Header().Set("X-Request-ID", reqID)
			c.Set("request_id", reqID)
			return next(c)
		}
	}
}

// generateRequestID generates a short unique request ID.
// If crypto/rand fails (extremely unlikely), falls back to a timestamp-based ID
// to ensure the request can still be processed.
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use UnixNano for uniqueness when crypto/rand is unavailable
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CSRFMiddleware provides CSRF protection for state-changing requests.
// Only applies to browser-based requests (cookie auth), not API token auth.
func CSRFMiddleware() echo.MiddlewareFunc {
	csrfManager := security.GetCSRFManager()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Skip CSRF for GET/HEAD/OPTIONS
			method := c.Request().Method
			if method == "GET" || method == "HEAD" || method == "OPTIONS" {
				return next(c)
			}

			// Skip for API token auth (non-browser clients)
			authHeader := c.Request().Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				return next(c)
			}

			// Skip for unauthenticated requests (public endpoints like
			// /auth/login, /auth/refresh, /auth/forgot-password): these carry
			// no auth cookie and are not CSRF-protected resources.
			// The path check is also required: a stale/expired access cookie
			// must not block login or token refresh, otherwise a user whose
			// CSRF token has also expired could never sign in again.
			path := c.Request().URL.Path
			if _, cerr := c.Cookie("edgelite_access"); cerr != nil ||
				strings.HasPrefix(path, "/api/v1/auth/login") ||
				strings.HasPrefix(path, "/api/v1/auth/refresh") ||
				strings.HasPrefix(path, "/api/v1/auth/forgot-password") {
				return next(c)
			}

			// Check CSRF token from header
			token := c.Request().Header.Get("X-CSRF-Token")
			if token == "" {
				token = c.FormValue("csrf_token")
			}

			if token == "" {
				return echo.NewHTTPError(http.StatusForbidden, "CSRF token missing")
			}

			if err := csrfManager.VerifyCSRFToken(token); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, "CSRF token invalid")
			}

			return next(c)
		}
	}
}

// SetCSRFCookie sets the CSRF token cookie for the response.
func SetCSRFCookie(secure bool) echo.MiddlewareFunc {
	csrfManager := security.GetCSRFManager()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Generate and set CSRF cookie if not present
			_, err := c.Cookie("csrf_token")
			if err != nil {
				token, terr := csrfManager.GenerateCSRFToken()
				if terr == nil {
					c.SetCookie(&http.Cookie{
						Name:     "csrf_token",
						Value:    token,
						Path:     "/",
						HttpOnly: false,
						Secure:   secure,
						SameSite: http.SameSiteStrictMode,
					})
					c.Response().Header().Set("X-CSRF-Token", token)
				}
			}
			return next(c)
		}
	}
}

// RateLimitEntry tracks rate limit counts per key.
type RateLimitEntry struct {
	count       int
	windowStart time.Time
}

// RateLimiter provides in-memory rate limiting.
type RateLimiter struct {
	mu               sync.Mutex
	entries          map[string]*RateLimitEntry
	requestsPerMinute int
}

// NewRateLimiter creates a new RateLimiter.
func NewRateLimiter(requestsPerMinute int) *RateLimiter {
	return &RateLimiter{
		entries:           make(map[string]*RateLimitEntry),
		requestsPerMinute: requestsPerMinute,
	}
}

// RateLimitMiddleware applies rate limiting per IP address.
func RateLimitMiddleware(limiter *RateLimiter) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ip := c.RealIP()
			if !limiter.Allow(ip) {
				logrus.WithField("ip", ip).Warn("Rate limit exceeded")
				c.Response().Header().Set("Retry-After", "60")
				return echo.NewHTTPError(http.StatusTooManyRequests, "Rate limit exceeded")
			}
			return next(c)
		}
	}
}

// Allow checks if a request from the given key is allowed.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	entry, ok := r.entries[key]
	if !ok || now.Sub(entry.windowStart) > time.Minute {
		r.entries[key] = &RateLimitEntry{count: 1, windowStart: now}
		// Clean up old entries
		if len(r.entries) > constants.AuthAttemptsLimit {
			for k, e := range r.entries {
				if now.Sub(e.windowStart) > time.Minute {
					delete(r.entries, k)
				}
			}
		}
		return true
	}

	entry.count++
	return entry.count <= r.requestsPerMinute
}

// SecurityHeadersMiddleware adds security-related HTTP headers.
func SecurityHeadersMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-XSS-Protection", "1; mode=block")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("X-Permitted-Cross-Domain-Policies", "none")
			h.Set("Cache-Control", "no-store, no-cache, must-revalidate")
			h.Set("Pragma", "no-cache")
			h.Set("Expires", "0")
			// HSTS: only enforce when HTTPS is in use (skip dev mode HTTP)
			if c.Request().TLS != nil {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			return next(c)
		}
	}
}

// CORSMiddleware adds CORS headers.
func CORSMiddleware(allowedOrigins []string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			origin := c.Request().Header.Get("Origin")
			if origin != "" {
				allowed := false
				for _, o := range allowedOrigins {
					if o == "*" || o == origin {
						allowed = true
						break
					}
				}
				if allowed {
					h := c.Response().Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, X-Request-ID")
					h.Set("Access-Control-Allow-Credentials", "true")
					h.Set("Access-Control-Max-Age", "86400")
				}
			}
			if c.Request().Method == "OPTIONS" {
				return c.NoContent(http.StatusOK)
			}
			return next(c)
		}
	}
}

// AuthMiddleware validates JWT tokens and sets user context.
func AuthMiddleware() echo.MiddlewareFunc {
	jwtManager := security.GetJWTManager()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			var token string
			authHeader := c.Request().Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}
			// LP-02: token 存于 HttpOnly Cookie，页面刷新后 JS 内存中无 token，
			// 浏览器请求只携带 cookie，必须支持 cookie 认证。
			// 空 Bearer 头（如客户端 token 过期清空后仍发送 "Bearer "）也回退到 Cookie，
			// 否则 Cookie 有效时仍会 401。
			if token == "" {
				if cookie, cerr := c.Cookie("edgelite_access"); cerr == nil && cookie.Value != "" {
					token = cookie.Value
				}
			}
			if token == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "Authorization header required")
			}

			claims, err := jwtManager.VerifyToken(token)
			if err != nil {
				logrus.WithError(err).Debug("Token verification failed")
				return echo.NewHTTPError(http.StatusUnauthorized, "Invalid or expired token")
			}

			// Check if token has been revoked (e.g. after logout)
			if claims.ID != "" && security.IsRevoked(claims.ID) {
				return echo.NewHTTPError(http.StatusUnauthorized, "Token has been revoked")
			}

			// Set user context
			c.Set("user_id", claims.UserID)
			c.Set("username", claims.Username)
			c.Set("role", claims.Role)

			return next(c)
		}
	}
}

// OptionalAuthMiddleware validates JWT tokens if present but doesn't require them.
func OptionalAuthMiddleware() echo.MiddlewareFunc {
	jwtManager := security.GetJWTManager()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authHeader := c.Request().Header.Get("Authorization")
			if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
				token := strings.TrimPrefix(authHeader, "Bearer ")
				claims, err := jwtManager.VerifyToken(token)
				if err == nil && (claims.ID == "" || !security.IsRevoked(claims.ID)) {
					c.Set("user_id", claims.UserID)
					c.Set("username", claims.Username)
					c.Set("role", claims.Role)
				}
			}
			return next(c)
		}
	}
}

// RequireRole returns a middleware that checks if the user has the required role.
func RequireRole(roles ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userRole, ok := c.Get("role").(string)
			if !ok {
				return echo.NewHTTPError(http.StatusForbidden, "Role information not found")
			}

			allowed := false
			for _, role := range roles {
				if userRole == role {
					allowed = true
					break
				}
			}

			if !allowed {
				return echo.NewHTTPError(http.StatusForbidden, "Insufficient permissions")
			}

			return next(c)
		}
	}
}

// LoggerMiddleware logs each request with sensitive data redacted.
// It uses the log sanitizer to ensure passwords, tokens, and API keys
// are not leaked in log output.
func LoggerMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			latency := time.Since(start)

			req := c.Request()
			resp := c.Response()

			// Build log fields with sanitization
			// resp.Status 在 echo 错误处理器写回响应前仍是默认值 200，此处从
			// *echo.HTTPError 提取真实状态码，避免日志出现 "status=200" 的 401 误导排障
			status := resp.Status
			if he, ok := err.(*echo.HTTPError); ok {
				status = he.Code
			}
			fields := logrus.Fields{
				"method":  req.Method,
				"path":    SanitizeString(req.URL.Path),
				"status":  status,
				"latency": latency.String(),
				"ip":      c.RealIP(),
				"req_id":  c.Get("request_id"),
			}

			// Sanitize query parameters
			if req.URL.RawQuery != "" {
				fields["query"] = SanitizeString(req.URL.RawQuery)
			}

			if err != nil {
				fields["error"] = SanitizeString(err.Error())
				logrus.WithFields(fields).Error("Request failed")
			} else if status >= 400 {
				logrus.WithFields(fields).Warn("Request error")
			} else {
				logrus.WithFields(fields).Info("Request")
			}

			return err
		}
	}
}

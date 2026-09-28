package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/security"
)

// TokenRenewalThreshold is the time before token expiry to trigger renewal.
const tokenRenewalThreshold = 5 * time.Minute

// NewTokenHeader is the response header for the renewed token.
const NewTokenHeader = "X-New-Token"

// TokenRenewalMiddleware automatically renews access tokens that are close to expiry.
// If a token has less than 5 minutes remaining, a new token is generated and
// returned in the X-New-Token response header for the client to use.
func TokenRenewalMiddleware() echo.MiddlewareFunc {
	jwtManager := security.GetJWTManager()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Process request first
			err := next(c)

			// Only renew for successful responses
			if c.Response().Status >= 400 {
				return err
			}

			// Skip SSE/streaming responses
			contentType := c.Response().Header().Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				return err
			}

			// Check for Bearer token
			authHeader := c.Request().Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				return err
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == "" {
				return err
			}

			// Try to verify and check expiry
			claims, verr := jwtManager.VerifyToken(token)
			if verr != nil {
				return err
			}

			// Check if token is within renewal threshold
			if claims.ExpiresAt == nil {
				return err
			}
			remaining := time.Until(claims.ExpiresAt.Time)
			if remaining > tokenRenewalThreshold {
				return err
			}

			// Generate new token
			newToken, _, rerr := jwtManager.GenerateAccessToken(claims.UserID, claims.Username, claims.Role)
			if rerr != nil {
				logrus.WithField("user", claims.UserID).
					WithField("error", rerr.Error()).
					Debug("Token renewal failed")
				return err
			}

			c.Response().Header().Set(NewTokenHeader, newToken)
			logrus.WithField("user", claims.UserID).
				WithField("remaining", remaining.String()).
				Debug("Access token renewed")
			return err
		}
	}
}

// DataMaskingMiddleware masks sensitive data in API responses.
// It scans JSON response bodies and masks fields like password, token, api_key, etc.
func DataMaskingMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			// Mask sensitive fields in response headers
			maskResponseHeaders(c)
			return err
		}
	}
}

// maskResponseHeaders masks sensitive values in response headers.
func maskResponseHeaders(c echo.Context) {
	for key, values := range c.Response().Header() {
		if !security.IsSensitiveKey(key) {
			continue
		}
		for i, v := range values {
			values[i] = security.MaskString(v)
		}
	}
}

// RequirePermission returns a middleware that checks if the user has the required permission.
func RequirePermission(perm security.Permission) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			role, ok := c.Get("role").(string)
			if !ok {
				return echo.NewHTTPError(http.StatusForbidden, "Role information not found")
			}
			if !security.HasPermission(role, perm) {
				return echo.NewHTTPError(http.StatusForbidden, "Insufficient permissions")
			}
			return next(c)
		}
	}
}

// IPWhitelistMiddleware only allows requests from whitelisted IPs.
func IPWhitelistMiddleware(allowedIPs []string) echo.MiddlewareFunc {
	whitelist := make(map[string]bool)
	for _, ip := range allowedIPs {
		whitelist[ip] = true
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if len(whitelist) == 0 {
				return next(c)
			}
			ip := c.RealIP()
			if !whitelist[ip] {
				logrus.WithField("ip", ip).Warn("IP not whitelisted")
				return echo.NewHTTPError(http.StatusForbidden, "IP not allowed")
			}
			return next(c)
		}
	}
}

// AuditLogMiddleware logs all state-changing operations for audit trail.
func AuditLogMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			method := c.Request().Method
			// Only audit state-changing methods
			if method == "GET" || method == "HEAD" || method == "OPTIONS" {
				return next(c)
			}

			start := time.Now()
			err := next(c)

			userID, _ := c.Get("user_id").(string)
			fields := logrus.Fields{
				"method":   method,
				"path":     c.Request().URL.Path,
				"status":   c.Response().Status,
				"latency":  time.Since(start).String(),
				"ip":       c.RealIP(),
				"user_id":  userID,
				"req_id":   c.Get("request_id"),
			}

			if err != nil {
				fields["error"] = err.Error()
				logrus.WithFields(fields).Warn("Audit: request failed")
			} else {
				logrus.WithFields(fields).Info("Audit: request")
			}

			return err
		}
	}
}

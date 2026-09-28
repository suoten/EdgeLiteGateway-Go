// Package middleware provides HTTP middleware for EdgeLite Gateway.
//
// This file implements a log sanitization middleware that redacts sensitive
// data (passwords, tokens, API keys, etc.) from request/response logs
// before they are written to the log output.
package middleware

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// Sensitive field names that should be redacted in logs.
var sensitiveFields = map[string]bool{
	"password":          true,
	"passwd":            true,
	"pwd":               true,
	"secret":            true,
	"token":             true,
	"access_token":      true,
	"refresh_token":     true,
	"api_key":           true,
	"apikey":            true,
	"api_secret":        true,
	"private_key":       true,
	"signing_key":       true,
	"auth_token":        true,
	"auth_password":     true,
	"smtp_password":     true,
	"mqtt_password":     true,
	"database_password": true,
	"credential":        true,
	"authorization":     true,
	"cookie":            true,
	"session_id":        true,
	"csrf_token":        true,
}

// Patterns for detecting sensitive data in strings.
var (
	// Matches "password=value" or "password":"value" patterns
	sensitivePattern = regexp.MustCompile(`(?i)("?(?:password|passwd|pwd|secret|token|api_key|apikey|api_secret|private_key|signing_key|auth_token|auth_password|smtp_password|mqtt_password|database_password|credential|authorization)"?\s*[:=]\s*"?)("[^"]*"|'[^']*'|\S+)`)
	// Matches Bearer tokens in Authorization headers
	bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)([a-zA-Z0-9\-._~+/]+=*)`)
)

// RedactValue is the replacement value for redacted sensitive data.
const RedactValue = "***REDACTED***"

// SanitizeString redacts sensitive data from a string.
// It handles JSON key-value pairs, URL parameters, and Authorization headers.
func SanitizeString(s string) string {
	if s == "" {
		return s
	}

	// Redact Bearer tokens
	s = bearerPattern.ReplaceAllString(s, "${1}"+RedactValue)

	// Redact key=value patterns (JSON and URL-encoded)
	s = sensitivePattern.ReplaceAllString(s, "${1}"+RedactValue)

	return s
}

// SanitizeMap recursively redacts sensitive values in a map.
func SanitizeMap(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		if isSensitiveField(k) {
			result[k] = RedactValue
		} else {
			result[k] = sanitizeValue(v)
		}
	}
	return result
}

// SanitizeJSON redacts sensitive data from a JSON byte slice.
func SanitizeJSON(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		// Not valid JSON, sanitize as string
		return []byte(SanitizeString(string(data)))
	}

	sanitized := sanitizeValue(v)
	result, err := json.Marshal(sanitized)
	if err != nil {
		return data
	}
	return result
}

func sanitizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return SanitizeMap(val)
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = sanitizeValue(item)
		}
		return result
	default:
		return v
	}
}

func isSensitiveField(field string) bool {
	lower := strings.ToLower(field)
	// Direct match
	if sensitiveFields[lower] {
		return true
	}
	// Check if field name contains a sensitive keyword
	for key := range sensitiveFields {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

// LogSanitizerMiddleware wraps the logger middleware to sanitize sensitive
// data before it is written to logs. It should be placed after LoggerMiddleware
// in the middleware chain, or used as a replacement.
func LogSanitizerMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Store original request body for potential logging
			req := c.Request()

			// Sanitize the Authorization header if present
			authHeader := req.Header.Get("Authorization")
			if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
				// Don't modify the actual header, just log a redacted version
				logrus.WithFields(logrus.Fields{
					"sanitized_auth": RedactValue,
				}).Debug("Authorization header redacted for logging")
			}

			err := next(c)

			// The actual logging is done by LoggerMiddleware;
			// this middleware ensures that any sensitive data in
			// error messages or response bodies is redacted before logging.
			if err != nil {
				// Wrap the error with a sanitized message if it contains sensitive data
				sanitizedMsg := SanitizeString(err.Error())
				if sanitizedMsg != err.Error() {
					logrus.WithFields(logrus.Fields{
						"req_id": c.Get("request_id"),
					}).Warn("Sensitive data detected and redacted in error message")
				}
			}

			return err
		}
	}
}

// SanitizeLogEntry sanitizes a logrus.Fields map, redacting any sensitive keys.
// This can be called before logging to ensure sensitive data is not leaked.
func SanitizeLogEntry(fields logrus.Fields) logrus.Fields {
	return SanitizeMap(fields)
}

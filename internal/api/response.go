// Package api provides RESTful API handlers for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/api/ package.
// It includes route handlers for auth, devices, rules, alarms, data,
// system, users, video, notify, drivers, platforms, and more.
package api

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// APIResponse is the standard JSON response format for all API endpoints.
// It mirrors the Python ApiResponse model: {code, message, data, error_code}.
type APIResponse struct {
	Code      int         `json:"code"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data"`
	ErrorCode string      `json:"error_code,omitempty"`
}

// PagedResponse is the standard paged response format.
type PagedResponse struct {
	Code  int         `json:"code"`
	Data  interface{} `json:"data"`
	Total int         `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"size"`
}

// OK returns a successful API response with data.
func OK(c echo.Context, data interface{}) error {
	return c.JSON(http.StatusOK, APIResponse{
		Code:    0,
		Message: "success",
		Data:    data,
	})
}

// OKPaged returns a successful paged response.
func OKPaged(c echo.Context, data interface{}, total, page, size int) error {
	return c.JSON(http.StatusOK, PagedResponse{
		Code:  0,
		Data:  data,
		Total: total,
		Page:  page,
		Size:  size,
	})
}

// Created returns a 201 Created response.
func Created(c echo.Context, data interface{}) error {
	return c.JSON(http.StatusCreated, APIResponse{
		Code:    0,
		Message: "created",
		Data:    data,
	})
}

// ErrorMsg returns an error response with a specific status code and message.
func ErrorMsg(c echo.Context, status int, message string) error {
	return c.JSON(status, APIResponse{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}

// ErrorCode returns an error response with an error code.
func ErrorCode(c echo.Context, status int, errorCode string, message string) error {
	return c.JSON(status, APIResponse{
		Code:      status,
		Message:   message,
		Data:      nil,
		ErrorCode: errorCode,
	})
}

// derivedErrorCode returns message as the error code when it is itself an
// ERR_* code (handlers pass codes via the message parameter); otherwise the
// helper's generic fallback code is used. This keeps error_code consistent
// with message so clients can always translate error_code.
func derivedErrorCode(fallback, message string) string {
	if strings.HasPrefix(message, "ERR_") {
		return message
	}
	return fallback
}

// NotFound returns a 404 error.
func NotFound(c echo.Context, message string) error {
	if message == "" {
		message = "Not Found"
	}
	return ErrorCode(c, http.StatusNotFound, derivedErrorCode("ERR_COMMON_NOT_FOUND", message), message)
}

// BadRequest returns a 400 error.
func BadRequest(c echo.Context, message string) error {
	if message == "" {
		message = "Bad Request"
	}
	return ErrorCode(c, http.StatusBadRequest, derivedErrorCode("ERR_COMMON_VALIDATION", message), message)
}

// Forbidden returns a 403 error.
func Forbidden(c echo.Context, message string) error {
	if message == "" {
		message = "Forbidden"
	}
	return ErrorCode(c, http.StatusForbidden, derivedErrorCode("ERR_AUTHZ_PERMISSION_DENIED", message), message)
}

// Unauthorized returns a 401 error.
func Unauthorized(c echo.Context, message string) error {
	if message == "" {
		message = "Unauthorized"
	}
	return ErrorCode(c, http.StatusUnauthorized, derivedErrorCode("ERR_AUTH_UNAUTHORIZED", message), message)
}

// InternalError returns a 500 error.
func InternalError(c echo.Context, message string) error {
	if message == "" {
		message = "Internal Server Error"
	}
	return ErrorCode(c, http.StatusInternalServerError, derivedErrorCode("ERR_COMMON_INTERNAL", message), message)
}

// ServiceUnavailable returns a 503 error.
func ServiceUnavailable(c echo.Context, message string) error {
	if message == "" {
		message = "Service Unavailable"
	}
	return ErrorCode(c, http.StatusServiceUnavailable, derivedErrorCode("ERR_COMMON_SERVICE_NOT_READY", message), message)
}

// Conflict returns a 409 error.
func Conflict(c echo.Context, message string) error {
	if message == "" {
		message = "Conflict"
	}
	return ErrorCode(c, http.StatusConflict, derivedErrorCode("ERR_COMMON_CONFLICT", message), message)
}

// sanitizeForHeader removes characters that could be used for HTTP header injection
// (CR, LF, and other control characters) from a string intended for use in a
// response header value.
func sanitizeForHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

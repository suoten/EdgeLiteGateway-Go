// Package models defines all data models for EdgeLiteGateway.
// This is a 1:1 port of the Python edgelite/models/ package.
package models

// PointValue represents a single data point value.
type PointValue struct {
	Name      string      `json:"name"`
	Value     interface{} `json:"value"`
	Quality   string      `json:"quality"`
	Timestamp string      `json:"timestamp,omitempty"`
}

// ApiResponse represents a standard API response envelope.
type ApiResponse struct {
	Success   bool        `json:"success"`
	Data      interface{} `json:"data,omitempty"`
	Message   string      `json:"message,omitempty"`
	Error     string      `json:"error,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
}

// PagedResponse represents a paginated API response.
type PagedResponse struct {
	Success    bool        `json:"success"`
	Data       interface{} `json:"data"`
	Page       int         `json:"page"`
	Size       int         `json:"size"`
	Total      int64       `json:"total"`
	TotalPages int         `json:"total_pages"`
}

// ErrorResponse represents an error response.
type ErrorResponse struct {
	Success   bool     `json:"success"`
	Error     string   `json:"error"`
	Code      string   `json:"code,omitempty"`
	Details   []string `json:"details,omitempty"`
	RequestID string   `json:"request_id,omitempty"`
}

// SortParams represents sorting parameters.
type SortParams struct {
	SortBy    string `query:"sort_by,omitempty"`
	SortOrder string `query:"sort_order,omitempty"` // asc, desc
}

package models

// PaginationParams holds pagination request parameters.
type PaginationParams struct {
	Page int `query:"page" json:"page"`
	Size int `query:"size" json:"size"`
}

// DefaultPagination returns default pagination params.
func DefaultPagination() PaginationParams {
	return PaginationParams{Page: 1, Size: 20}
}

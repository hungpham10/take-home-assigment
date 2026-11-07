// Package httpx provides shared HTTP response helpers and error mapping.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error is the canonical API error envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Error Error `json:"error"`
}

// WriteJSON writes a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the canonical error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, errorBody{Error: Error{Code: code, Message: message}})
}

// Known platform error codes and their HTTP status mapping.
var statusByCode = map[string]int{
	"NAMESPACE_NOT_READY":        http.StatusConflict,            // 409
	"CONFLICT":                   http.StatusConflict,            // 409
	"SCRIPT_NOT_VALIDATED":       http.StatusConflict,            // 409
	"INSUFFICIENT_MOCK_CAPACITY": http.StatusConflict,            // 409
	"INVALID_TRANSITION":         http.StatusConflict,            // 409
	"OBJECT_NOT_FOUND":           http.StatusNotFound,            // 404
	"QUOTA_EXCEEDED":             http.StatusTooManyRequests,     // 429
	"VALIDATION_FAILED":          http.StatusUnprocessableEntity, // 422
	"BAD_REQUEST":                http.StatusBadRequest,          // 400
	"UNAUTHORIZED":               http.StatusUnauthorized,        // 401
	"FORBIDDEN":                  http.StatusForbidden,           // 403
}

// WriteCodeError maps a platform error code to its HTTP status.
func WriteCodeError(w http.ResponseWriter, code, message string) {
	status, ok := statusByCode[code]
	if !ok {
		status = http.StatusInternalServerError
	}
	WriteError(w, status, code, message)
}

// ErrNotFound is a sentinel handlers can return.
var ErrNotFound = errors.New("not found")

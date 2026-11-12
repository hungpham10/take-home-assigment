package namespace

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unit: mapDeleteNamespaceError classifies transition errors from
// fn_namespace_transition. check_violation (23514) with INVALID_TRANSITION
// prefix → 409; everything else → 500 INTERNAL (real DB errors must not be
// masked as client errors).
func TestMapDeleteNamespaceError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{
			name:     "check_violation INVALID_TRANSITION -> 409",
			err:      &pgconn.PgError{Code: "23514", Message: "INVALID_TRANSITION namespace APPLYING -> DELETING"},
			wantCode: http.StatusConflict,
			wantErr:  "INVALID_TRANSITION",
		},
		{
			name:     "OBJECT_NOT_FOUND P0001 -> 409",
			err:      &pgconn.PgError{Code: "P0001", Message: "OBJECT_NOT_FOUND namespace <id>"},
			wantCode: http.StatusConflict,
			wantErr:  "OBJECT_NOT_FOUND",
		},
		{
			name:     "other pg error (function missing) -> 500 INTERNAL",
			err:      &pgconn.PgError{Code: "42883", Message: "function fn_namespace_transition(uuid, unknown, ...) does not exist"},
			wantCode: http.StatusInternalServerError,
			wantErr:  "INTERNAL",
		},
		{
			name:     "non-pg error -> 500 INTERNAL",
			err:      &pgconn.PgError{Code: "08001", Message: "connection refused"},
			wantCode: http.StatusInternalServerError,
			wantErr:  "INTERNAL",
		},
		{
			name:     "nil error -> 500 INTERNAL (defensive)",
			err:      nil,
			wantCode: http.StatusInternalServerError,
			wantErr:  "INTERNAL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, errCode := mapDeleteNamespaceError(tt.err)
			if code != tt.wantCode {
				t.Errorf("mapDeleteNamespaceError(%v) code = %d, want %d", tt.err, code, tt.wantCode)
			}
			if errCode != tt.wantErr {
				t.Errorf("mapDeleteNamespaceError(%v) errCode = %q, want %q", tt.err, errCode, tt.wantErr)
			}
		})
	}
}

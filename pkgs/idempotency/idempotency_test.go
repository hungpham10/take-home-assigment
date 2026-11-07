package idempotency

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequireKey enforces the mandatory Idempotency-Key header (moved from
// the namespace package where a local duplicate used to live).
func TestRequireKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want int
	}{
		{"missing key", "", 422},
		{"key present", "abc", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := RequireKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

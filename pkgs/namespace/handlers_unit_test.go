package namespace

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireIdempotencyKey(t *testing.T) {
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
			h := requireIdempotencyKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestCheckIfMatch(t *testing.T) {
	cases := []struct {
		name   string
		ifM    string
		wantOK bool
		wantSt int
	}{
		{"no header takes latest", "", true, 200},
		{"matching etag", `"3"`, true, 200},
		{"weak etag", `W/"3"`, true, 200},
		{"stale etag", `"2"`, false, 412},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/", nil)
			if tc.ifM != "" {
				req.Header.Set("If-Match", tc.ifM)
			}
			rec := httptest.NewRecorder()
			ok := checkIfMatch(rec, req, 3)
			if ok != tc.wantOK || rec.Code != tc.wantSt {
				t.Errorf("checkIfMatch = (%v, %d), want (%v, %d)", ok, rec.Code, tc.wantOK, tc.wantSt)
			}
		})
	}
}

func TestETagFormat(t *testing.T) {
	if got := etagFor(7); got != `"7"` {
		t.Errorf("etagFor(7) = %q", got)
	}
}

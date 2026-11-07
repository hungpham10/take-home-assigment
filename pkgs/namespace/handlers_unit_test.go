package namespace

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequirePrincipalFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no headers", nil, 401},
		{"tenant only", map[string]string{"X-Tenant-Id": "t1"}, 401},
		{"user only", map[string]string{"X-User-Id": "u1"}, 401},
		{"both headers", map[string]string{"X-Tenant-Id": "t1", "X-User-Id": "u1"}, 200},
		{"full with role", map[string]string{"X-Tenant-Id": "t1", "X-User-Id": "u1", "X-User-Role": "admin"}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Principal
			h := RequirePrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = PrincipalOf(r)
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 200 {
				if got.TenantID != "t1" || got.UserID != "u1" {
					t.Errorf("principal = %+v", got)
				}
			}
		})
	}
}

func TestIsAdminNormalization(t *testing.T) {
	if !(Principal{Role: "admin"}).IsAdmin() {
		t.Error("admin role must normalize to IsAdmin")
	}
	for _, role := range []string{"", "user", "Admin", "ADMIN"} {
		if (Principal{Role: role}).IsAdmin() {
			t.Errorf("role %q must NOT normalize to IsAdmin", role)
		}
	}
}

func TestFromContextAbsent(t *testing.T) {
	if _, ok := FromContext(t.Context()); ok {
		t.Error("FromContext must report absent principal")
	}
}

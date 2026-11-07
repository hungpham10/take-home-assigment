package authz

import "testing"

func TestRequireSelfOrAdmin(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		tt   string // target tenant
		tu   string // target user
		want error
	}{
		{"self", Principal{TenantID: "t1", UserID: "u1"}, "t1", "u1", nil},
		{"admin same tenant", Principal{TenantID: "t1", UserID: "a1", Role: "admin"}, "t1", "u1", nil},
		{"other user", Principal{TenantID: "t1", UserID: "u2"}, "t1", "u1", ErrForbidden},
		{"admin cross tenant", Principal{TenantID: "t1", UserID: "a1", Role: "admin"}, "t2", "u1", ErrForbidden},
		{"role spoof lookalike", Principal{TenantID: "t1", UserID: "u2", Role: "Admin"}, "t1", "u1", ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequireSelfOrAdmin(tc.p, tc.tt, tc.tu); got != tc.want {
				t.Errorf("RequireSelfOrAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}

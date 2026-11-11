package authz_test

// DB-backed role resolution tests (plan16 API-AUTHZ-04): RequirePrincipalWithRole
// must treat tbl_user_roles as the authoritative role source and fail closed on
// spoofed headers without a DB row. External test package — testutil imports
// server → authz, so the internal package cannot import it.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// run fires one request with the given principal headers.
func run(t *testing.T, h http.Handler, tenantID, userID, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-User-Id", userID)
	if role != "" {
		req.Header.Set("X-User-Role", role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRequirePrincipalWithRole exercises the guard through a tiny admin-only
// mutation: RequireSelfOrAdmin against the owner user (self passes; admins in
// the same tenant pass; everyone else 403).
func TestRequirePrincipalWithRole(t *testing.T) {
	db := testutil.TestDB(t)

	// One tenant: owner ("user" role), admin ("admin" role), stranger (no role).
	suffix := time.Now().UnixNano()
	tenant := store.Tenant{Name: "A", Slug: "az" + string(rune('a'+suffix%26)) + time.Now().Format("150405.000000000"), Status: "ACTIVE"}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	mkUser := func(email, role string) store.User {
		u := store.User{TenantID: tenant.ID, Email: email, DisplayName: "U"}
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if err := db.Create(&store.UserRole{UserID: u.ID, Role: role}).Error; err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	owner := mkUser("owner-"+tenant.ID+"@x.co", "user")
	admin := mkUser("admin-"+tenant.ID+"@x.co", "admin")
	peer := mkUser("peer-"+tenant.ID+"@x.co", "user")
	stranger := mkUser("stranger-"+tenant.ID+"@x.co", "")

	var calls int
	h := authz.RequirePrincipalWithRole(db)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := authz.PrincipalOf(r)
		if err := authz.RequireSelfOrAdmin(p, tenant.ID, owner.ID); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		calls++
		w.WriteHeader(http.StatusOK)
	}))

	// Spoofed X-User-Role=admin with NO DB role → fail-closed 401 (the identity
	// itself is unprovisioned — header role alone proves nothing).
	if rec := run(t, h, tenant.ID, stranger.ID, "admin"); rec.Code != 401 {
		t.Errorf("spoofed admin, no DB role: %d, want 401", rec.Code)
	}

	// Spoofed header on a user whose DB role is "user": DB role wins → the
	// admin-only mutation on ANOTHER owner's resource is rejected 403.
	if rec := run(t, h, tenant.ID, peer.ID, "admin"); rec.Code != 403 {
		t.Errorf("DB role user + spoofed admin header: %d, want 403", rec.Code)
	}

	// DB role admin + header claims "user": DB wins → admin mutation allowed.
	if rec := run(t, h, tenant.ID, admin.ID, "user"); rec.Code != 200 {
		t.Errorf("DB role admin + header user: %d, want 200", rec.Code)
	}

	// Legit self access with no DB role and empty header → proceeds unprivileged.
	if rec := run(t, h, tenant.ID, owner.ID, ""); rec.Code != 200 {
		t.Errorf("self access, no roles: %d, want 200", rec.Code)
	}

	// Missing identity headers still fail closed 401 (RequirePrincipal base).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != 401 {
		t.Errorf("no headers: %d, want 401", rec.Code)
	}
	if calls != 2 {
		t.Errorf("successful calls = %d, want 2", calls)
	}
}

// TestRequirePrincipalWithRoleResolvesExternalSubject covers plan 37: nginx
// forwards the OIDC `sub`, which is opaque (Dex: base64url protobuf). The
// middleware must map it to tbl_users.id; unknown subjects fail closed.
func TestRequirePrincipalWithRoleResolvesExternalSubject(t *testing.T) {
	db := testutil.TestDB(t)

	tenant := store.Tenant{Name: "B", Slug: "bz" + time.Now().Format("150405.000000000"), Status: "ACTIVE"}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatal(err)
	}
	user := store.User{TenantID: tenant.ID, Email: "sub-" + tenant.ID + "@x.co", DisplayName: "U"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserRole{UserID: user.ID, Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	// Opaque subject shaped exactly like Dex emits it (base64url protobuf).
	// Suffix keeps the row unique — the test DB is shared across runs.
	subject := "CiQxMTExMTExMS0xMTExLTQxMTEtODExMS0xMTExMTExMTExMTESBWxvY2Fs" + time.Now().Format("150405.000000000")
	if err := db.Create(&store.UserExternalIdentity{
		UserID: user.ID, Issuer: "http://dex:5556/dex", Subject: subject,
	}).Error; err != nil {
		t.Fatal(err)
	}

	var got authz.Principal
	h := authz.RequirePrincipalWithRole(db)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = authz.PrincipalOf(r)
		w.WriteHeader(http.StatusOK)
	}))

	// Opaque subject → resolved to the local UUID, DB role applied.
	if rec := run(t, h, tenant.ID, subject, ""); rec.Code != 200 {
		t.Fatalf("known subject: %d, want 200", rec.Code)
	}
	if got.UserID != user.ID {
		t.Errorf("resolved user id = %q, want %q", got.UserID, user.ID)
	}
	if !got.IsAdmin() {
		t.Errorf("role = %q, want admin from tbl_user_roles", got.Role)
	}

	// Unmapped subject → fail closed 401.
	if rec := run(t, h, tenant.ID, "not-a-uuid-and-not-mapped", ""); rec.Code != 401 {
		t.Errorf("unknown subject: %d, want 401", rec.Code)
	}

	// Subject mapped in a DIFFERENT tenant must not resolve (tenant isolation).
	other := store.Tenant{Name: "C", Slug: "cz" + time.Now().Format("150405.000000000"), Status: "ACTIVE"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if rec := run(t, h, other.ID, subject, ""); rec.Code != 401 {
		t.Errorf("cross-tenant subject: %d, want 401", rec.Code)
	}

	// UUID pass-through keeps working (legacy header mode / access tokens).
	if rec := run(t, h, tenant.ID, user.ID, ""); rec.Code != 200 {
		t.Errorf("uuid pass-through: %d, want 200", rec.Code)
	}
}

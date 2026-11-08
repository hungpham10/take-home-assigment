package namespace_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/store"
	"github.com/hungpham/loadtest-platform/pkgs/testutil"
)

// Tests for create-with-spec (plans/18 PR A): POST /namespaces with
// spec{services,databases,ingress} — atomic create, optional apply.

// specFull returns a valid create-with-spec body.
func specFull(name, slug string) map[string]any {
	slug = slug + "-" + fmt.Sprintf("%x", time.Now().UnixNano())
	return map[string]any{
		"name": name, "slug": slug, "description": "full spec",
		"spec": map[string]any{
			"services": []map[string]any{{
				"name":        "widgets",
				"description": "widget service",
				"deploy":      map[string]any{"image": "widgets:1", "replicas": 2},
				"openapi": map[string]any{
					"format": "json", "version": "3.0.0", "content_type": "application/json",
					"content": widgetSpec,
				},
			}},
			"databases": []map[string]any{{
				"name": "main", "engine": "postgres", "version": "16",
			}},
			"ingress": []map[string]any{{
				"name": "public", "class": "nginx",
				"hosts": []map[string]any{{"host": "api.example.com", "service_id": "widgets", "port": 8080}},
			}},
		},
	}
}

func TestINT_NS_02_CreateWithSpecFull(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "spec-1"), specFull("env-full", "env-full-slug"))
	if rec.Code != 201 {
		t.Fatalf("create-with-spec: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Namespace store.Namespace `json:"namespace"`
		Services  []string        `json:"services"`
		Databases []string        `json:"databases"`
		Ingress   []string        `json:"ingress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Namespace.Slug == nil || !strings.HasPrefix(*resp.Namespace.Slug, "env-full-slug-") {
		t.Errorf("slug = %v", resp.Namespace.Slug)
	}
	if resp.Namespace.Visibility == nil || *resp.Namespace.Visibility != "tenant" {
		t.Errorf("visibility = %v", resp.Namespace.Visibility)
	}
	if len(resp.Services) != 1 || !strings.HasPrefix(resp.Services[0], "svc-") {
		t.Errorf("services = %v", resp.Services)
	}
	if len(resp.Databases) != 1 || !strings.HasPrefix(resp.Databases[0], "db-") {
		t.Errorf("databases = %v", resp.Databases)
	}
	if len(resp.Ingress) != 1 || !strings.HasPrefix(resp.Ingress[0], "ing-") {
		t.Errorf("ingress = %v", resp.Ingress)
	}

	// DB rows actually exist for the tenant.
	var nsCount, svcCount, dbCount, ingCount int64
	db.Table("tbl_namespaces").Where("tenant_id = ? AND id = ?", resp.Namespace.TenantID, resp.Namespace.ID).Count(&nsCount)
	db.Table("tbl_services").Where("namespace_id = ?", resp.Namespace.ID).Count(&svcCount)
	db.Table("tbl_databases").Where("namespace_id = ?", resp.Namespace.ID).Count(&dbCount)
	db.Table("tbl_ingress").Where("namespace_id = ?", resp.Namespace.ID).Count(&ingCount)
	if nsCount != 1 || svcCount != 1 || dbCount != 1 || ingCount != 1 {
		t.Fatalf("rows: ns=%d svc=%d db=%d ing=%d", nsCount, svcCount, dbCount, ingCount)
	}
	var svc store.Service
	if err := db.Where("namespace_id = ?", resp.Namespace.ID).First(&svc).Error; err != nil {
		t.Fatal(err)
	}
	if svc.ServiceID == nil || !strings.HasPrefix(*svc.ServiceID, "svc-") {
		t.Errorf("service_id = %v", svc.ServiceID)
	}
	if len(svc.OpenAPISpec) == 0 || svc.SpecHash == nil {
		t.Errorf("openapi not stored: %v %v", svc.OpenAPISpec, svc.SpecHash)
	}
}

func TestINT_NS_02_CreateWithSpecAtomicRollback(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	body := specFull("env-rollback", "env-rollback-slug")
	spec := body["spec"].(map[string]any)
	spec["ingress"] = []map[string]any{{
		"name":  "public",
		"hosts": []map[string]any{{"host": "api.example.com", "service_id": "ghost", "port": 80}},
	}}

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "spec-2"), body)
	if rec.Code != 422 {
		t.Fatalf("invalid spec create: %d %s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("error should mention unknown service: %s", rec.Body.String())
	}

	// ZERO rows for this tenant (atomic rollback).
	tenantID := principal["X-Tenant-Id"]
	for _, table := range []string{"tbl_namespaces", "tbl_services", "tbl_databases", "tbl_ingress"} {
		var c int64
		col := "tenant_id"
		if table != "tbl_namespaces" {
			col = "tbl_namespaces.tenant_id"
			table = table + " JOIN tbl_namespaces ON tbl_namespaces.id = " + table + ".namespace_id"
		}
		db.Table(table).Where(col+" = ?", tenantID).Count(&c)
		if c != 0 {
			t.Errorf("%s rows = %d, want 0 (rollback failed)", table, c)
		}
	}
}

func TestINT_NS_02_CreateWithSpecApply(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	body := specFull("env-apply", "env-apply-slug")
	body["apply"] = true
	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "spec-3"), body)
	if rec.Code != 201 {
		t.Fatalf("create+apply: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Namespace store.Namespace `json:"namespace"`
		Status    string          `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "READY" || resp.Namespace.Status != "READY" {
		t.Errorf("status = %q, ns.Status = %q, want READY", resp.Status, resp.Namespace.Status)
	}
	if !resp.Namespace.ReadyForLoadtest {
		t.Errorf("ready_for_loadtest = false")
	}
}

func TestINT_NS_21_CreateWithSpecBadOpenAPI(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	body := specFull("env-bad", "env-bad-slug")
	svc := body["spec"].(map[string]any)["services"].([]map[string]any)[0]
	svc["openapi"] = map[string]any{"format": "json", "content": brokenSpec}

	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "spec-4"), body)
	if rec.Code != 422 {
		t.Fatalf("bad openapi create: %d %s, want 422", rec.Code, rec.Body.String())
	}
	tenantID := principal["X-Tenant-Id"]
	var c int64
	db.Table("tbl_namespaces").Where("tenant_id = ?", tenantID).Count(&c)
	if c != 0 {
		t.Errorf("namespace rows = %d, want 0", c)
	}
}

func TestINT_NS_02_CreateWithSpecBackwardCompat(t *testing.T) {
	h, db := testServer(t)
	base := "/api/loadtest/v1/namespaces"
	principal := testutil.TenantUserHeaders(t, db, "user")

	// Legacy 3-field body: no spec → old response shape (bare namespace).
	rec := testutil.Do(t, h, http.MethodPost, base, testutil.WithKey(principal, "spec-5"),
		map[string]string{"name": "env-legacy", "description": "old style"})
	if rec.Code != 201 {
		t.Fatalf("legacy create: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != `"1"` {
		t.Errorf("ETag = %q", rec.Header().Get("ETag"))
	}
	var ns store.Namespace
	if err := json.Unmarshal(rec.Body.Bytes(), &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Name != "env-legacy" || ns.Status != "DRAFT" {
		t.Errorf("ns = %+v", ns)
	}
	var svcCount, dbCount, ingCount int64
	db.Table("tbl_services").Where("namespace_id = ?", ns.ID).Count(&svcCount)
	db.Table("tbl_databases").Where("namespace_id = ?", ns.ID).Count(&dbCount)
	db.Table("tbl_ingress").Where("namespace_id = ?", ns.ID).Count(&ingCount)
	if svcCount+dbCount+ingCount != 0 {
		t.Errorf("legacy create created components: %d %d %d", svcCount, dbCount, ingCount)
	}
}

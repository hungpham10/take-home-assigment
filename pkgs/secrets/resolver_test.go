package secrets

import "testing"

func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"acme":    "ACME",
		"acme-co": "ACME_CO",
		"a.b.c":   "A_B_C",
	}
	for in, want := range cases {
		if got := SanitizeSlug(in); got != want {
			t.Errorf("SanitizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveFromEnv(t *testing.T) {
	r := Resolver{Lookup: func(key string) (string, bool) {
		if key == "LTP_TENANT_ACME_CO_SESSION_SECRET" {
			return "s3cr3t", true
		}
		return "", false
	}}
	if got := r.Resolve("acme-co", SessionSecret); got != "s3cr3t" {
		t.Errorf("Resolve = %q, want s3cr3t", got)
	}
	if got := r.Resolve("acme-co", JWTSecret); got != "" {
		t.Errorf("missing secret must resolve empty, got %q", got)
	}
	all := r.ResolveAll("acme-co")
	if len(all) != 1 || all[SessionSecret] != "s3cr3t" {
		t.Errorf("ResolveAll = %v", all)
	}
}

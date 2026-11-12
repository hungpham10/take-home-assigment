// Package secrets resolves tenant secrets from the runtime environment.
//
// Secrets are NEVER stored in the database: they live in SOPS-encrypted
// files (env/secrets.<ENV>.enc.yaml) decrypted at boot by scripts/release.sh
// (SOPS_AGE_KEY_CONTENT → dotenv → exported env vars). Resolution convention:
//
//	LTP_TENANT_<SLUG>_OIDC_CLIENT_SECRET
//	LTP_TENANT_<SLUG>_SESSION_SECRET
//	LTP_TENANT_<SLUG>_JWT_SECRET
//
// <SLUG> is the tenant slug upper-cased with non-alphanumerics → '_'.
package secrets

import (
	"fmt"
	"os"
	"strings"
)

// Secret names served to nginx auth-config.
const (
	OIDCClientSecret = "oidc_client_secret"
	SessionSecret    = "session_secret"
	JWTSecret        = "jwt_secret"
)

// Prefix is the env var namespace for tenant secrets.
const Prefix = "LTP_TENANT_"

// Resolver reads secrets from an environment source (os.Getenv by default;
// injectable for tests).
type Resolver struct {
	Lookup func(key string) (string, bool)
}

// New returns a Resolver bound to the process environment.
func New() Resolver {
	return Resolver{Lookup: os.LookupEnv}
}

// envKey builds the env var name for a tenant slug + secret kind.
func envKey(slug, kind string) string {
	var b strings.Builder
	b.WriteString(Prefix)
	b.WriteString(SanitizeSlug(slug))
	b.WriteString("_")
	b.WriteString(strings.ToUpper(strings.TrimSpace(kind)))
	return b.String()
}

// SanitizeSlug maps a tenant slug to its env-var component (ACME_CO).
func SanitizeSlug(slug string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(slug) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Resolve returns the named secret for a tenant slug; empty string when unset
// (caller decides fallback/logging — never log values).
func (r Resolver) Resolve(slug, kind string) string {
	if r.Lookup == nil {
		return ""
	}
	if v, ok := r.Lookup(envKey(slug, kind)); ok {
		return v
	}
	return ""
}

// ResolveAll returns every known secret kind for a tenant (present ones only).
func (r Resolver) ResolveAll(slug string) map[string]string {
	out := map[string]string{}
	for _, kind := range []string{OIDCClientSecret, SessionSecret, JWTSecret} {
		if v := r.Resolve(slug, kind); v != "" {
			out[kind] = v
		}
	}
	return out
}

// String never exposes values — safe for logs.
func (r Resolver) String() string { return fmt.Sprintf("secrets.Resolver(prefix=%s)", Prefix) }

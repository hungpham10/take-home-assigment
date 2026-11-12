# Secrets Management (SOPS + age)

Every secret (client_secret, session_secret, jwt_secret, DB DSN...) **lives neither in plaintext** in the repo **nor in the DB**.

## Layout

| File | Commit? | Role |
|---|---|---|
| `env/secrets.<ENV>.yaml` | ❌ gitignored | plaintext, dev machine/secret store only |
| `env/secrets.<ENV>.enc.yaml` | ✅ | SOPS-encrypted (AES256-GCM, age recipient in `.sops.yaml`) |
| `.sops.yaml` | ✅ | creation rules + age **public** key |
| `age-key.txt` | ❌ gitignored | age private key — distributed outside the repo |
| `env/secrets.dev.yaml.example` | ✅ | template for the key conventions |

## Flow

1. **Encrypt:** `sops -e env/secrets.dev.yaml > env/secrets.dev.enc.yaml`
2. **Runtime decrypt:** `scripts/release.sh` — if `SOPS_AGE_KEY_CONTENT` is present, run `sops -d --output-type dotenv` → export env vars; without a key → fall back to plain env vars (dev).
3. **Consume:** the controller reads env vars:
   - `DATABASE_DSN`, `OIDC_SESSION_SECRET` — controller runtime
   - `LTP_TENANT_<SLUG>_<SECRET>` — per-tenant secret (`pkgs/secrets.Resolver`, slug sanitized `acme-co → ACME_CO`):
     `OIDC_CLIENT_SECRET`, `SESSION_SECRET`, `JWT_SECRET`

## The DB holds no secrets

`tbl_tenant_auth_profiles.config` (JSONB) keeps only non-sensitive fields (`jwt_mode`, `issuer`, `client_id`, `jwks_uri`, `oidc_expected_alg`). Migration `0007` strips the sensitive keys from existing data. `GET /api/admin/v1/tenant/{host}/auth-config` merges: DB (non-secret) + env (secret); when a secret is missing → WARN log **without the value** (nginx Lua has its own env-var fallback).

## Security rules
- The private age key only arrives via `SOPS_AGE_KEY_CONTENT` (secret store) — no file, no logs.
- The auth-config response contains secrets: trusted nginx channel only, never log the body.
- CI **has no age key**: tests must not depend on secrets; resolver tests inject env vars.
- Rotate a secret: edit the plaintext → re-encrypt → commit the new `.enc`; the nginx cache expires on its own after 600s.

## Encryption at rest (field-level, `sops.rs` pattern)

Alongside SOPS file-level encryption, sensitive values that **must be recoverable from the DB** (e.g. the `abt_` plaintext for admin reveal) are encrypted with **AES-256-GCM** using `LTP_MASTER_KEY` (from the SOPS env): blob = `nonce(12) || ciphertext` — pattern `opsense-rs/crates/opsense-mlib/src/sops.rs`.

- `pkgs/secrets/crypto.go` — `Encrypt`/`Decrypt`/`LoadMasterKey`/`GenerateMasterKey`
- `tbl_access_tokens.token_encrypted` (migration 0008) — the encrypted plaintext; `token_hash` remains the authentication path (introspection)
- `POST /api/admin/v1/tokens/{id}/reveal` — self-or-admin (audit-logged); 503 when `LTP_MASTER_KEY` is not configured
- Generate a key: `openssl rand -hex 32`

## References
- `scripts/release.sh` (decrypt_secrets) — the repo's existing pattern.
- `plans/15-sops-secrets.md` — detailed design.

# Admin & OIDC Integration API — `/api/admin/v1`

This document describes the **control-plane / account-management API** under **`/api/admin/v1`**, used to integrate **OIDC/JWT identity** with the platform and to support user-scoped authorization (owner-only access; `admin` can access across owners within a tenant). It complements the BFF data-plane API under **`/api/loadtest/v1`** (see [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md)).

**Related docs:**
- [`docs/nginx-oidc-auth.md`](nginx-oidc-auth.md) — edge OIDC/JWT/tenant pattern in `conf/nginx`; nginx calls some `/api/admin/v1` endpoints for tenant resolution, auth config, and token introspection.
- [`docs/architecture.md`](architecture.md) — system architecture, trust boundary, owner/admin authorization model.
- [`docs/k6-operator-go-client.md`](k6-operator-go-client.md) — load execution via k6-operator (Go K8s API, no `kubectl`).

**Stack (MVP, per `AGENTS.MD`):** Go + go-chi, Swagger/OpenAPI, PostgreSQL (`sql/` migrations), nginx routing/auth policy in `conf/nginx`. Nginx may proxy `/api/admin/v1` to the same Go backend (or a dedicated admin service) depending on deployment; keep routing decisions in `conf/nginx`.

## 1. Purpose and scope

**POC (locked):** **OIDC/nginx auth is used**. nginx (`conf/nginx`, OpenResty/Lua pattern) authenticates (OIDC/JWT/`abt_` as applicable) and reflects identity/permissions from OIDC claims/introspection into trusted headers. The Go backend **must still include go-chi middleware** to extract those trusted headers, build a canonical principal (`tenant_id`, `user_id`/principal, `is_admin`, roles), enforce tenant isolation + owner/admin RBAC, and fail closed if required identity is missing on protected routes. **`tenant_id`** scopes the multi-domain/org loadtest service boundary (always applied). **`abt_`** is a platform-issued service-account/base token (useful for CLI/automation). Admin normalization remains canonical and never derived from untrusted client input.

`/api/admin/v1` provides **general account/tenant/auth integration management** so that:
1. **nginx (OpenResty/Lua)** can resolve tenant identity and per-tenant/per-`oidc_name` auth configuration at the edge, and can introspect platform-managed base tokens (`abt_`).
2. **Administrators/operators** can manage tenants, OIDC/JWT auth profiles, users/principals, roles, and platform-issued tokens/sessions needed for access control.
3. The **Go backend** can enforce the canonical authorization rule for `/api/loadtest/v1` resources: **tenant isolation always; owner-only by default; `admin` role may access/manage across owners within the same tenant**.

> **Trust boundary:** endpoints called by nginx (tenant lookup/auth-config/introspect) are security-sensitive. Expose them only on trusted paths (e.g., unix socket/private upstream/internal route) or protect with strict service-to-service controls. Do **not** expose identity-header injection or raw auth secrets publicly. nginx-injected headers (`X-Tenant-Id`, `X-User-Id`, etc.) are trusted only at the nginx→backend boundary.

## 2. Identity & authorization contract (canonical)

### 2.0 Trust boundary & secret handling (security-critical)
- **Header spoofing:** backend must trust `X-Tenant-Id`, `X-User-Id`, `X-User-Role`, claim headers **only** on trusted ingress (nginx→backend over unix socket/private network/mTLS). Nginx must **strip/overwrite** these headers from client requests. Protected backend routes fail closed (`401`) if required headers are absent.
- **Auth-config secrets:** `GET /tenant/{host}/auth-config` returns sensitive values (`oidc_client_secret`, `session_secret`, `jwt_secret`). Restrict to internal calls from nginx (unix socket/private upstream/service auth). Do not expose to browsers. Nginx cache (`ngx.shared.tenant`) must not be logged; consider minimizing cached secret material or using short TTL + rotation. Audit/rotate secrets.
- **`abt_` tokens:** issue once, store hashed; introspection internal-only; constant-time lookup; revoke/delete supported.


This section replaces ad-hoc integration notes and defines the contract the Go backend must implement.

### 2.1 Trusted headers (after boundary enforcement) (nginx → backend)
For protected routes, nginx authenticates (OIDC session / Bearer JWT / `abt_` introspection per `docs/nginx-oidc-auth.md`) and injects trusted headers:

| Header | Meaning | Required |
|---|---|---|
| `X-Tenant-Id` | Resolved tenant identifier | Yes (protected routes) |
| `X-User-Id` | Authenticated subject: local `tbl_users.id` (UUID, from `abt_` introspection / legacy header mode) **or** the raw OIDC `sub`. `public` only for explicitly public routes. See §2.1.1. | Yes (protected routes) |
| `X-User-Role` | Role claim when available (e.g., Bearer-JWT path in current nginx pattern) | No, but used for admin normalization |
| `X-Client-Id`, `X-Audience-Id` | JWT `azp`/`aud` when available | Optional |
| `X-User-Email` / `X-Auth-User-Id` / `X-Auth-Email` | Convenience claims in some flows (e.g., docs/OIDC) | Optional |

### 2.2 Admin normalization
- Define a **single normalized rule**: `is_admin = true` when the principal has an admin role.
- **Canonical mapping (to finalize in implementation):** accept normalized admin from `X-User-Role` (e.g., `admin`) and/or tenant/account role membership resolved server-side (e.g., introspection result, user-role table, configured admin group/claim). Do not rely on client payload.
- **Known nginx gap (prod/edge path):** `X-User-Role` propagation is not uniform across all auth paths in the current `conf/nginx` pattern (Bearer-JWT sets it; OIDC cookie/id-token and `abt_` introspection may not). `/api/admin/v1` account/role/introspection design must provide enough data to normalize admin consistently; align `conf/nginx` maps/vhosts if header propagation is extended (validate with `scripts/nginx.sh` / `nginx -t`). Treat historical `/api/admin/*` + `oidc_name=m2m` notes in config as caveats — **enforce RBAC in Go**.

### 2.1.1 Resolving `X-User-Id` to a local user (OIDC `sub` is opaque)

OIDC `sub` is **not** a local user id and cannot be treated as one:

- Dex (v2.41) always emits `sub = base64url(protobuf{1: userID, 2: connectorID})`
  (`server/oauth2.go:genSubject`) — there is no setting to disable it, so even a
  UUID-shaped `staticPasswords[].userID` arrives wrapped in protobuf+base64.
- `tbl_namespaces.owner_user_id` is `UUID REFERENCES tbl_users(id)`, so the raw `sub`
  fails at the first write (`invalid input syntax for type uuid`).

Resolution rule enforced by `authz.RequirePrincipalWithRole(db)` (see `pkgs/authz/identity.go`,
plan 37):

| `X-User-Id` value | Action |
|---|---|
| valid UUID | used as-is (legacy header mode, access tokens, internal callers) |
| anything else | looked up in `tbl_user_external_identities` (`subject`) joined to `tbl_users` **scoped to `X-Tenant-Id`**; the resulting `tbl_users.id` becomes the principal |
| lookup miss | `401 identity not recognized` (fail closed — no auto-provisioning) |

Populating the mapping (`tbl_user_external_identities.issuer/subject` → user) is an admin
provisioning concern; the E2E seeds it from the `sub` of the freshly minted token.

### 2.3 Authorization rules
- **Fail closed:** missing required identity headers on a protected route → `401`.
- **Tenant isolation:** always scope by `X-Tenant-Id`; never cross tenants.
- **Ownership:** load-test/catalog resources store immutable `tenant_id` + `owner_user_id` (from `X-User-Id`) on create. Default access filter: `tenant_id = ? AND owner_user_id = ?`.
- **Admin override:** `is_admin` may read/manage across owners **within the same tenant** only. Audit admin cross-owner actions.
- **Errors:** `401` unauthenticated, `403` forbidden, `404` not found (apply a consistent policy for cross-owner/hidden resources to avoid leaking existence).

## 3. Nginx integration endpoints (required by edge pattern)

These endpoints are called by `conf/nginx` Lua (see `docs/nginx-oidc-auth.md`, `vhost/04-api.conf`, `vhost/02-openid.conf`, `vhost/05-docs.conf`). Paths/names below reflect the current pattern; if implementation chooses different names, update `conf/nginx` accordingly (ask before breaking routing changes).

### 3.1 `GET /api/admin/v1/tenant/{host}/id` — Resolve tenant id (public-path helper)
- **Used by nginx:** public (`$jwt_required == 0`) flow to obtain tenant id before pass-through.
- **Auth (edge):** internal/trusted call from nginx; not a general public browser API. Apply network/trust controls.
- **Path params:** `host` — request host (tenant routing key).
- **Response `200`:** tenant identifier (body may be plain id or JSON, e.g. `"123"` or `{ "tenant_id": "123" }`) — **match nginx Lua parsing/expectations** when implementing.
- **Errors:** `404` tenant not found for host → nginx treats as tenant resolution failure (`502`/error path in current pattern), `5xx` on backend failure.

### 3.2 `GET /api/admin/v1/tenant/{host}/auth-config` — Resolve tenant auth config
- **Used by nginx:** protected API, `/login`, `/logout`, `/callback`, docs; cached in `ngx.shared.tenant` (typically 600s) by key `auth:{host}:{oidc_name}`.
- **Query params:** `name` = `oidc_name` (auth profile selector, e.g. `default`, `openapi`). Default behavior must align with `conf/nginx/map/01-jwt.conf`.
- **Response `200` (JSON, consumed by Lua):**
```json
{
  "auth": {
    "id": 123,
    "jwt_mode": "jwks",
    "oidc_jwks_url": "https://issuer/.../jwks",
    "jwt_secret": null,
    "oidc_issuer": "https://issuer",
    "oidc_client_id": "...",
    "oidc_client_secret": "...",
    "oidc_expected_alg": "RS256",
    "session_secret": "..."
  }
}
```
- **Field guidance:** `jwt_mode` drives Bearer JWT verification (`jwks` → `oidc_jwks_url`; shared-secret/HS mode → `jwt_secret`). OIDC fields drive `lua-resty-openidc` discovery/client/session. `id` is used for tenant context and session cache segment isolation (e.g., `oidc_cache_{auth.id}`) — keep stable/tenant-safe.
- **Security:** contains secrets (`client_secret`, `session_secret`, `jwt_secret`). Restrict to trusted nginx/backend channel; never expose to browsers/clients. Support rotation without breaking active sessions where possible.
- **Errors:** `404` unknown host/`oidc_name` → nginx fails auth/config load (missing `oidc_name` can hard-fail routes). Return clear errors; never leak secrets; never log response bodies containing secrets.

### 3.3 `POST /api/admin/v1/token/introspect` — Introspect platform base token (`abt_`)
- **Used by nginx:** when `Authorization: Bearer abt_...` is presented on `/api/`; result cached briefly (e.g., ~60s) in nginx shared cache.
- **Request (from nginx, internal):** includes token and tenant context (current pattern passes `X-Tenant-Id` and body/header with bearer token — align with implementation).
```json
{ "token": "abt_xxx" }
```
- **Response `200` (active):**
```json
{ "active": true, "user_id": "u-123", "tenant_id": "123", "roles": ["user"], "is_admin": false, "expires_at": "2026-11-06T06:00:00Z" }
```
- **Response `200` (inactive/invalid):** `{ "active": false }` (preferred) or `401` — align with nginx Lua handling.
- **Inferred fields for RBAC:** include enough to normalize admin (`roles`/`is_admin`) so `/api/loadtest/v1` owner/admin checks are consistent even when JWT `X-User-Role` is unavailable.
- **Security:** internal-only; constant-time/hashed token lookup; revoke/delete support; audit issuance and revocation.

## 4. General account & tenant management (admin API, inferred)

These endpoints support “quản lý chung chung về mặt tài khoản” and OIDC integration operations. Implement incrementally; protect with strong admin/service auth and audit. Exact schemas are finalized in `plans/`/Swagger.

### 4.1 Tenants & auth profiles
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/admin/v1/tenants` | List tenants (admin/operator). |
| `POST` | `/api/admin/v1/tenants` | Create tenant. |
| `GET` | `/api/admin/v1/tenants/{tenant_id}` | Get tenant. |
| `PATCH` | `/api/admin/v1/tenants/{tenant_id}` | Update tenant. |
| `GET` | `/api/admin/v1/tenants/{tenant_id}/hosts` | List host mappings used for `{host}` resolution. |
| `PUT` | `/api/admin/v1/tenants/{tenant_id}/hosts/{host}` | Bind/unbind host to tenant. |
| `GET` | `/api/admin/v1/tenants/{tenant_id}/auth-profiles` | List OIDC/JWT auth profiles (`oidc_name` selectors). |
| `POST` | `/api/admin/v1/tenants/{tenant_id}/auth-profiles` | Create profile (`default`, `openapi`, future `m2m` with valid config). |
| `GET` | `/api/admin/v1/tenants/{tenant_id}/auth-profiles/{name}` | Get profile (secrets redacted by default). |
| `PATCH` | `/api/admin/v1/tenants/{tenant_id}/auth-profiles/{name}` | Update/rotate secrets; manage `jwt_mode`, issuer, JWKS URL, client credentials, session secret. |
| `DELETE` | `/api/admin/v1/tenants/{tenant_id}/auth-profiles/{name}` | Remove profile (guard: ensure nginx/routing not depending on it). |

### 4.2 Users / principals / roles
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/admin/v1/users` | List users/principals within admin scope/tenant; filters `q`, `role`, pagination. |
| `POST` | `/api/admin/v1/users` | Create or provision a local/platform principal mapping (if needed alongside IdP). |
| `GET` | `/api/admin/v1/users/{user_id}` | Get principal, linked IdP subject(s), roles, status. |
| `PATCH` | `/api/admin/v1/users/{user_id}` | Update status/metadata; assign/remove roles. |
| `POST` | `/api/admin/v1/users/{user_id}/roles` | Grant role (e.g., `admin`, `user`) tenant-scoped. |
| `DELETE` | `/api/admin/v1/users/{user_id}/roles/{role}` | Revoke role. |
| `GET` | `/api/admin/v1/me` | (Optional, if routed here) Current principal from trusted headers; useful parity with `/api/loadtest/v1/me`. |

**Role model (MVP):** minimum roles `admin` and `user` (or equivalent). `admin` grants cross-owner access within tenant for `/api/loadtest/v1` resources only where policy allows; does not bypass tenant isolation.

### 4.3 Platform tokens (`abt_`) and service credentials
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/admin/v1/users/{user_id}/tokens` | Issue platform base token (`abt_`) for a user/principal; return token once. |
| `GET` | `/api/admin/v1/users/{user_id}/tokens` | List token metadata (never return secret values). |
| `POST` | `/api/admin/v1/tokens/{token_id}/revoke` | Revoke token (self-or-admin); introspection must return inactive. |
| `POST` | `/api/admin/v1/token/introspect` | Nginx/internal introspection (see §3.3). |

### 4.4 Audit & health (inferred)
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/admin/v1/audit` | Admin audit trail (token issuance/revocation, role changes, admin cross-owner actions reported by services). |
| `GET` | `/api/admin/v1/health` or `/health` | Service health (route per nginx/system config). |

## 5. Data model implications (`sql/`)
Plan migrations (illustrative; finalize names/constraints in `plans/`): `tenants`, `tenant_hosts`, `tenant_auth_profiles` (store secrets encrypted/managed appropriately), `users/principals`, `user_external_identities` (issuer+subject), `user_roles` (tenant-scoped), `access_tokens` (hashed `abt_`, owner, tenant, roles snapshot/is_admin, expiry, revoked_at), `audit_log`. Index by host, `(tenant_id, oidc_name)`, issuer+subject, token hash, `(tenant_id, user_id)`.

## 6. Nginx / `conf/nginx` alignment checklist
- [ ] Route/protect `/api/admin/v1/*` appropriately: tenant/auth-config/introspect endpoints must be reachable by nginx’s internal calls as the current Lua expects; public exposure must be restricted.
- [ ] Keep `$jwt_required` / `$oidc_name` maps consistent (`map/01-jwt.conf`). Adding `oidc_name=m2m` requires a real auth profile/config; missing config causes route failures (current caveat).
- [ ] Ensure method allowlist supports admin operations (e.g., `DELETE` for revocation) per `www.conf`/vhost policy.
- [ ] If admin normalization needs roles in OIDC/`abt_` paths, update nginx to inject `X-User-Role` (or a dedicated header) consistently — config change in `conf/nginx`, validate with `scripts/nginx.sh` / `nginx -t`.
- [ ] Never cache or log secrets from auth-config/introspect responses.

## 7. Implementation sequence (MVP slice)
1. Implement nginx-required endpoints first: `GET /tenant/{host}/id`, `GET /tenant/{host}/auth-config?name=`, `POST /token/introspect` with schemas matching `conf/nginx` Lua expectations.
2. Add minimal tenant/auth-profile storage and `abt_` issuance/revocation + role/is_admin in introspection.
3. Add auth middleware in `pkgs/` for principal extraction/normalization and owner/admin checks for `/api/loadtest/v1`.
4. Expand account management CRUD, RBAC, audit, Swagger docs — all TDD, `sql/` migrations, PR workflow per `AGENTS.MD`.

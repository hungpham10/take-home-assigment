# Nginx OIDC / Identity Pattern (`conf/nginx`) — notes for per-user APIs

This note summarizes the **OIDC/JWT + tenant** pattern implemented in `conf/nginx` (OpenResty + Lua). It is **reference behavior from this config set** (upstream in this config is `http://backend` → unix socket `/var/run/axum`; the MVP in this repo will be a **Go + go-chi** backend behind nginx, so the same **trust-boundary/header contract** can be applied).

> ⚠️ **`/var/run/axum` là FILE unix socket, không phải thư mục.** `upstream backend`
> (`http.conf`) và **mọi** `httpc:connect("unix:…")` trong Lua phải cùng trỏ
> `/var/run/axum`; controller bind đúng path đó và `pkgs/listener` tự mkdir parent +
> xoá stale socket + chmod 666. Nếu path này bị tạo thành thư mục (`mkdir` trong
> entrypoint, `emptyDir` mount vào `/var/run/axum`, hoặc đổi thành
> `/var/run/axum/controller.sock`) ⇒ controller không bind được ⇒ nginx
> `Connection refused` ⇒ `/auth-config` fail ⇒ mọi request `/api/*` chết trước handler.
> `scripts/k8s-apply.sh` có guard chặn lỗi này.

> ⚠️ **DNS cho cosocket.** Lua gọi JWKS/OIDC discovery bằng `lua-resty-http`, nên cần
> `resolver` đúng môi trường (`resolver 127.0.0.11` chỉ đúng với Docker Compose). Conf dùng
> placeholder `%%NGINX_RESOLVER%%`, `scripts/release.sh` thay bằng env `NGINX_RESOLVER`
> (default `127.0.0.11`; k8s: `kube-dns.kube-system.svc.cluster.local`).

The admin/control-plane endpoints called by nginx and the canonical owner/admin account contract are specified in [`docs/admin-oidc-integration-api.md`](admin-oidc-integration-api.md) (`/api/admin/v1`).

## Goal (upcoming)
APIs will be **user-scoped**: a user can see/manage only their own load tests, **except users with an admin role**, who can see/manage across users (within tenant scope, as applicable).

## High-level architecture
- **nginx (OpenResty/Lua)** is the auth/identity enforcement point at the edge.
- It resolves **tenant** and **auth config** per request (by `host` + `oidc_name`), verifies identity (OIDC session cookie / Bearer JWT / `abt_` base token), then **injects trusted identity headers** and proxies to the backend.
- **Backend authorization is still required**: nginx identifies *who* the caller is; the backend must enforce *what* they can access (owner vs admin, tenant isolation). Backend must only trust these headers when traffic comes from nginx (private network / unix socket / appropriate deployment controls).

Key files:
- `conf/nginx/http.conf` — OpenResty/Lua setup, `lua_shared_dict` (`tenant`, `discovery`, `jwks`, `sessions`, `resty_jwt_verification_cache_jwks`), Redis-backed session support, trusted certs.
- `conf/nginx/map/01-jwt.conf` — `$jwt_required` (`0` public, `1` force/session path, `2` require auth) and `$oidc_name` (auth config selector, e.g. `default`, `openapi` for docs).
- `conf/nginx/vhost/02-openid.conf` — browser OIDC `/login`, `/logout`, `/callback` (per tenant + `oidc_name`).
- `conf/nginx/vhost/04-api.conf` — `/api/` authn, tenant resolution, header injection, proxy to backend.
- `conf/nginx/vhost/05-docs.conf` + `conf/nginx/map/03-docs.conf` — Swagger/OpenAPI protection/routing.
- `conf/nginx/vhost/01-system.conf` — `/health`, `/metrics` (proxied; access log off). `www.conf` enforces method allowlist (includes `OPTIONS`/`DELETE`).

## Tenant + auth-config resolution
For protected paths, nginx fetches tenant auth config from the backend control endpoint (cached in `ngx.shared.tenant`, typically 600s):

- Cache key pattern: `auth:{host}:{oidc_name}` (and tenant-id lookup for public path).
- Request: `GET /api/admin/v1/tenant/{host}/auth-config?name={oidc_name}` (and `/tenant/{host}/id` for public).
- Tenant config provides values like `auth.id` (tenant id), `jwt_mode` (`jwks` vs shared secret), `oidc_jwks_url`/`jwt_secret`, OIDC `issuer/client_id/client_secret/expected_alg/session_secret`, etc.
- `/login`/`logout` may fall back to env vars for backward compat; `/callback` **requires** dynamic config to avoid session/cache mismatch.

### Dex as dev OIDC provider (plans/34)
Local compose runs **Dex** (`ghcr.io/dexidp/dex:v2.41.1`, port 5556) with `conf/dex/config.dev.yaml`
(issuer `http://localhost:5556/dex`, static client `ltp-test`, static user `dev-user@example.com`/`password`).
Tenant auth-config seeded in DB uses the in-network issuer `http://dex:5556/dex`; host-side tools/tests
use `http://localhost:5556/dex` (same port-forward). See [`packaging-deployment.md`](packaging-deployment.md).

## Authentication modes (`/api/`, `vhost/04-api.conf`)
1. **Public (`$jwt_required == 0`)**: resolve tenant, set `X-Tenant-Id`, set `X-User-Id: public`, pass through.
2. **Bearer token present (`Authorization: Bearer ...`)**:
   - **`abt_` base tokens**: introspected via backend `POST /api/admin/v1/token/introspect` (cached ~60s); on active, sets `X-User-Id` from introspection result.
   - **JWT** (otherwise): verified per tenant config — `jwks` (asymmetric, `oidc_jwks_url`) or shared-secret/HS mode (`jwt_secret`). On success, nginx extracts claims and sets identity headers (see below).
3. **OIDC web session / cookie fallback**: uses `lua-resty-openidc` with Redis session storage, tenant-specific `client_id/secret/issuer`, `cache_segment = oidc_cache_{tenant auth.id}`, validates issuer/audience against tenant config. Used for browser flows and forced/session paths as configured.
4. **No valid auth** when required (`$jwt_required == 2` or invalid token): returns `401`.

## Identity headers injected to backend
Observed in `04-api.conf` / `05-docs.conf`:
- `X-Tenant-Id` — tenant identifier resolved from tenant config/lookup.
- `X-User-Id` — `sub` (JWT/OIDC id token) or introspected user id; `public` for public routes.
- `X-User-Role` — **set in the Bearer-JWT verification branch** from `jwt_obj.payload.role` (when present). Comment notes RBAC is performed by backend.
- `X-Client-Id`, `X-Audience-Id` — from JWT claims (`azp`, `aud`) in Bearer-JWT branch.
- Docs flow may set `X-Auth-User-Id` / `X-Auth-Email` (see `05-docs.conf`).

**Canonical contract (POC locked):** nginx performs authn (OIDC/JWT/`abt_` as applicable) and reflects identity/permissions from OIDC claims/introspection into trusted headers; **Go go-chi middleware** extracts headers, builds canonical principal (`tenant_id`, `user_id`/principal, `is_admin`, roles), and enforces RBAC/tenant isolation/fail-closed. 
- `X-User-Role`/admin propagation may still require `conf/nginx` alignment across **all** auth paths (Bearer JWT, OIDC session/id-token, `abt_` introspection) so the backend can normalize admin consistently. Until aligned, backend may additionally normalize admin from trusted introspection/role-mapping sources (never from untrusted client input) per implementation decision.
- Historical notes about `/api/admin/*` RBAC and `oidc_name=m2m` are reference caveats; MVP backend enforces RBAC in Go.

## Backend authorization contract (recommended for load-test APIs)
For load-test resources stored/managed by the service:

- Every request carries **`X-Tenant-Id`** + **`X-User-Id`** (except explicit public endpoints).
- **Ownership**: persist `tenant_id` + `owner_user_id` (from `X-User-Id`) on each load-test/session/run record. Default queries filter by `tenant_id` AND `owner_user_id`.
- **Admin override**: if the authenticated principal has the **admin role** (normalized from `X-User-Role` / introspection result / configured admin group), allow read/manage across owners **within the same tenant** (and only where appropriate). Never bypass tenant isolation unless explicitly designed.
- **Never trust client-supplied owner IDs** for scoping; derive identity/role only from nginx-injected headers at the trusted boundary.
- Return `404` for authenticated access to another user’s resource (anti-existence-leak), `403` only for genuinely unauthorized actions, `401` for missing/invalid auth (handled primarily by nginx; backend should still fail closed if headers absent on protected routes).

## Implications for Go + go-chi MVP (POC locked)
- **POC uses OIDC/nginx auth** (`conf/nginx`): nginx authenticates (OIDC session/JWT/`abt_` introspection as applicable) and reflects identity/permissions from OIDC claims/introspection into trusted headers.
- **Go backend still requires middleware** (go-chi, `pkgs/`) to extract/validate trusted headers, build canonical principal (`tenant_id`, `user_id`/principal, `is_admin`, roles), inject into request context, and **enforce RBAC/tenant isolation/fail-closed** in handlers. nginx reflects permissions; Go enforces authorization decisions.
- Keep API routing/auth policy in `conf/nginx` (maps/vhosts) consistent; address role propagation gaps where needed for canonical admin normalization.
- Swagger/OpenAPI should document required auth and caller scoping (admin sees all in tenant). Serve Swagger per `conf/nginx` docs routing/auth model.
- When shipping k6-operator `TestRun` resources (see `docs/k6-operator-go-client.md`), include **tenant/user/session labels and unique names**; do not rely on Kubernetes RBAC alone for app-level per-user visibility.


## Security notes (MVP hardening)
- **Never trust client-supplied `X-Tenant-Id`/`X-User-*`:** nginx must clear incoming values and set them after successful authn/tenant resolution.
- **Auth-config contains secrets:** internal endpoint only; avoid logging cache contents/responses; prefer unix socket/private network; rotate secrets.
- **Role propagation:** ensure `conf/nginx` + backend middleware produce consistent canonical admin normalization across JWT bearer, OIDC session, and `abt_` introspection paths before relying on admin cross-owner access. nginx changes (if needed) must be validated with `scripts/nginx.sh` / `nginx -t`.
- Treat historical caveats (`/api/admin/*` RBAC, `oidc_name=m2m`) as reference; MVP backend enforces RBAC in Go.

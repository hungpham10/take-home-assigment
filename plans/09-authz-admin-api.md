# Plan 09 — Authz Middleware + `/api/admin/v1`

## Mục tiêu
Principal từ trusted headers (nginx inject), RBAC fail-closed, và admin API slice khớp đúng expectations của Lua trong `conf/nginx`.

## Dependencies
- Plan 08 (server skeleton), Plan 03 (bảng admin/auth).

## Deliverables
- `pkgs/authz`:
  - Middleware đọc `X-Tenant-Id`, `X-User-Id`, `X-User-Role`, `X-Client-Id`, `X-Audience-Id` → canonical `Principal`. **Fail-closed**: thiếu header bắt buộc → 401.
  - RBAC: `admin` cross-owner trong cùng tenant, `user` owner-only; mọi query lọc `tenant_id`.
  - Ghi chú gap đã biết: nginx chỉ set `X-User-Role` trên Bearer-JWT path (docs/nginx-oidc-auth.md).
- API `/api/admin/v1` (TDD):
  - `GET /tenant/{host}/id`
  - `GET /tenant/{host}/auth-config?name=` — payload khớp đúng Lua (`conf/nginx/map/01-jwt.conf`, vhost openid).
  - `POST /token/introspect` — cache ~60s phía caller; verify hash `abt_` từ `tbl_access_tokens`.
  - CRUD tenants/auth-profiles/users/roles/tokens + audit log ghi mỗi mutation.

## Verification
- Unit test middleware (thiếu header → 401; role sai → 403).
- Integration test: curl qua nginx local → auth-config trả đúng schema Lua parse được.

## Ghi chú
- Token `abt_` chỉ lưu hash; plaintext chỉ trả 1 lần lúc issue.

# Database Schema

Nguồn chân lý: `sql/` migrations (quy ước xem `sql/README.md`). Tài liệu này mô tả từng bảng, index, invariant.

## Migration 0001 — Admin/Auth domain

### tbl_tenants
Tenant (org boundary). `slug` unique; `status`: `ACTIVE|SUSPENDED|DELETED`.

### tbl_tenant_hosts
Map hostname → tenant cho nginx. `host` unique; `is_default`.

### tbl_tenant_auth_profiles
Cấu hình auth per tenant, schema khớp expectations Lua trong `conf/nginx` (issuer, client_id, jwks_uri, `config JSONB`). Unique `(tenant_id, name)`.

### tbl_users
`tenant_id` immutable. Unique `(tenant_id, email)`.

### tbl_user_external_identities
Liên kết OIDC: unique `(issuer, subject)`.

### tbl_user_roles
Role MVP: `admin` (cross-owner trong tenant) | `user` (owner-only). Unique `(user_id, role)`.

### tbl_access_tokens
Token dịch vụ `abt_...`: **chỉ lưu SHA-256 hash** (`token_hash` unique), `prefix` = 8 ký tự đầu để lookup nhanh. `expires_at`, `revoked_at` nullable. Plaintext chỉ trả 1 lần lúc issue.

### tbl_audit_log
Append-only. `actor JSONB` chứa principal từ trusted headers. Index `(tenant_id, created_at DESC)` và `(object_type, object_id)`.

## Invariants
- Mọi mutation trên domain admin ghi 1 row `tbl_audit_log`.
- Tenant/user/token không xóa cứng — dùng status/`revoked_at`.

## State transitions & events (từ migration 0004, pattern chi tiết ở plans/04)
- Các object có state machine (`tbl_namespaces`, `tbl_scripts`, `tbl_load_tests`, `tbl_datasets`) chuyển trạng thái **chỉ** qua `fn_<object>_transition(...)`:
  `SELECT ... FOR UPDATE` → check `tbl_status_transitions` → `UPDATE status, version+1` → `INSERT tbl_<object>_events` — tất cả trong 1 transaction; transition trái phép → exception (app map `409/422`).
- `tbl_<object>` = current state, nguồn đọc cho list/get (JOIN + index); `tbl_<object>_events` append-only, chỉ JOIN khi xem lịch sử.
- Bảng transition map: `tbl_status_transitions (object_type, from_status, to_status, allowed)` — seed toàn bộ transition hợp lệ.

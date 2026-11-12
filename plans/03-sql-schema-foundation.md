# Plan 03 — SQL Schema Foundation (quy ước + migration 0001 admin/auth)

## Mục tiêu
Chốt quy ước schema toàn platform và viết migration đầu tiên cho domain admin/auth — nguồn chân lý duy nhất của DB theo AGENTS.md (`sql/`).

## Dependencies
- Plan 01. (CI job validate SQL từ Plan 02 sẽ tự chạy sau khi file xuất hiện.)

## Deliverables

### 1. `sql/README.md` — quy ước chung
- Đặt tên: `tbl_<object>` cho bảng chính; `tbl_<object>_events` cho bảng sự kiện trạng thái; `fn_<object>_transition` cho DB function chuyển trạng thái.
- Mọi bảng chính có: `id UUID PK (gen_random_uuid())`, `tenant_id UUID`, `created_at`/`updated_at TIMESTAMPTZ`, trigger tự cập nhật `updated_at`.
- Migration up-only, đánh số `NNNN_<tên>.sql`, áp dụng tuần tự; tracking bằng bảng `tbl_schema_migrations (id, filename, applied_at)` — do migration runner của controller chạy, KHÔNG dùng migration tự sửa DB ngoài `sql/`.
- App role DB chỉ được `SELECT/INSERT/UPDATE`; bảng events và audit là **append-only** (`REVOKE UPDATE, DELETE`).

### 2. Migration `sql/0001_admin_auth.sql`
- `tbl_tenants` (`id`, `name`, `slug` unique, `status`, timestamps).
- `tbl_tenant_hosts` (`tenant_id` FK, `host` unique, `is_default`) — map hostname → tenant cho nginx.
- `tbl_tenant_auth_profiles` (`tenant_id`, `name`, `issuer`, `client_id`, `jwks_uri`, `config JSONB`, unique `(tenant_id, name)`) — schema khớp expectations của `conf/nginx` Lua.
- `tbl_users` (`id`, `tenant_id` immutable, `email`, `display_name`, `owner_user_id` self, timestamps).
- `tbl_user_external_identities` (`user_id`, `issuer`, `subject`, unique `(issuer, subject)`).
- `tbl_user_roles` (`user_id`, `role` CHECK IN ('admin','user'), scope tenant).
- `tbl_access_tokens` (`id`, `user_id`, `token_hash` unique — token `abt_` không bao giờ lưu plaintext, `prefix` 8 ký tự đầu để lookup, `expires_at`, `revoked_at`).
- `tbl_audit_log` (`id BIGSERIAL`, `tenant_id`, `actor JSONB`, `action`, `object_type`, `object_id`, `details JSONB`, `created_at`) — append-only.

### 3. Docs
- `docs/database-schema.md`: mô tả từng bảng, cột, index, invariant (bắt buộc trước code theo AGENTS.md).

## Verification
- CI job `sql-migrations` áp `0001` vào Postgres sạch → xanh.
- Test thủ công: insert tenant + host + user + token hash, đảm bảo unique constraints hoạt động; thử UPDATE `tbl_audit_log` bằng app role → bị từ chối.

## Ghi chú
- Idempotency: mỗi migration guard bằng `tbl_schema_migrations` (INSERT ... ON CONFLICT DO NOTHING đầu file, hoặc runner check trước).

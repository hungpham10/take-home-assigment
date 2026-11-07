# Database Schema

Nguồn chân lý: `sql/` migrations (quy ước xem `sql/README.md`). Tài liệu này mô tả từng bảng, index, invariant.

## Migration 0001 — Admin/Auth domain

### tbl_tenants
Tenant (org boundary). `slug` unique; `status`: `ACTIVE|SUSPENDED|DELETED`.

### tbl_tenant_hosts
Map hostname → tenant cho nginx. `host` unique; `is_default`.

### tbl_tenant_auth_profiles
Cấu hình auth per tenant, schema khớp expectations Lua trong `conf/nginx` (issuer, client_id, jwks_uri, `config JSONB`). Unique `(tenant_id, name)`. **Không chứa secret** — secret lấy từ SOPS env (`docs/secrets-management.md`); migration 0007 strip các key nhạy cảm.

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

## Migration 0002 — Status transitions (pattern)

- `tbl_status_transitions (object_type, from_status, to_status, allowed)` — transition map chung.
- `fn_is_transition_allowed(object_type, from, to)` — check map.
- `fn_status_transition(main_table, events_table, object_type, id, to_status, actor, reason, meta)` — template generic: FOR UPDATE lock → check map → UPDATE status+version → INSERT event, trong 1 transaction caller. Domain migrations tạo wrapper `fn_<object>_transition`.

## Migration 0003 — Namespace & component domain

### tbl_namespaces (current state)
- `status`: `DRAFT, VALIDATING, INVALID, APPLYING, DATA_LOADING, READY, DEGRADED, DELETING, DELETED` (CHECK constraint).
- `ready_for_loadtest BOOL` — gate tạo loadtest; `version` cho ETag/If-Match.
- TTL: `ttl_expires_at` (NULL = không TTL) + `cleanup_policy JSONB` — reaper (Plan 13) đọc index partial.
- Unique `(tenant_id, name)`; index `(tenant_id, status)`, `(owner_user_id, status)`.
- Chỉ transition sang `DELETED` (soft delete) — không xóa cứng.

### tbl_namespace_events
Append-only; index `(object_id, id)`; wrapper `fn_namespace_transition(...)`.

### Components (FK CASCADE về namespace, không state machine — dùng audit)
- `tbl_services`: `openapi_spec JSONB` (NULL → chặn readiness, 422), `spec_hash` (đổi spec → revalidate), `parsed_summary JSONB` (operations index → `targets[]`), unique `(namespace_id, name)`.
- `tbl_databases`: engine + `profile JSONB`.
- `tbl_ingress`: host + `paths JSONB`.
- `tbl_dependencies`: `type` (service|database|mock) + `ref_id` + `config` — nguồn suy ra NetworkPolicy egress (ADR-002).
- `tbl_network_policies`: `spec` đã resolve + `observed_generation`.

## Migration 0004 — Script & Loadtest domain

### tbl_scripts (current state)
- `status`: `PENDING_VALIDATION, VALIDATING, VALIDATED, INVALID, VALIDATION_FAILED`; `INVALID/VALIDATION_FAILED` có thể revalidate (`→VALIDATING`).
- `size_bytes CHECK ≤ 1MiB` (giới hạn ConfigMap); content lưu qua `content_ref` (ConfigMap/staging), chỉ lưu `content_hash`.
- `tbl_script_validations`: kết quả mỗi lần validate (`(script_id, attempt)` unique, `errors JSONB` → 422).

### tbl_load_tests (current state)
- `status`: `QUEUED, PROVISIONING, RUNNING, COMPLETED, FAILED, STOPPING, STOPPED, TIMED_OUT, TIMEOUT_CLEANED, CLEANUP_FAILED` (`CANCELED` reserved).
- `load_config JSONB`, `targets JSONB` (bắt buộc), `runner_type k8s|local`, `runner_ref`.
- `timeout_at` + index partial cho reaper (TIMED_OUT); app gate: namespace `ready_for_loadtest` + script `VALIDATED` (409 nếu không).
- `tbl_loadtest_events` append-only; `fn_loadtest_transition`.

### tbl_test_events
Stream runtime append-only, unique `(load_test_id, seq)`, `type log|metric|status`.

## State transitions & events (pattern chi tiết ở plans/04)
- Các object có state machine (`tbl_namespaces`, `tbl_scripts`, `tbl_load_tests`, `tbl_datasets`) chuyển trạng thái **chỉ** qua `fn_<object>_transition(...)`:
  `SELECT ... FOR UPDATE` → check `tbl_status_transitions` → `UPDATE status, version+1` → `INSERT tbl_<object>_events` — tất cả trong 1 transaction; transition trái phép → exception (app map `409/422`).
- `tbl_<object>` = current state, nguồn đọc cho list/get (JOIN + index); `tbl_<object>_events` append-only, chỉ JOIN khi xem lịch sử.
- Bảng transition map: `tbl_status_transitions (object_type, from_status, to_status, allowed)` — seed toàn bộ transition hợp lệ.

## Migration 0005 — Platform plumbing

- `tbl_idempotency_keys`: UNIQUE `(tenant_id, endpoint, key)` — middleware Idempotency-Key; reaper dọn >24h.
- `tbl_outbox_events`: transactional outbox — ghi cùng transaction với state change, worker dispatch; partial index trên `dispatched_at IS NULL`.
- `tbl_reservation_ledger`: quota `vusers|concurrent_loadtests|mock_pool`, state `RESERVED→COMMITTED|RELEASED`; vượt quota → 429.
- `tbl_datasets`: `pii_classification masked|synthetic|non_prod_raw`, restore_config JSONB; lifecycle 7 trạng thái qua `fn_dataset_transition`.

> Schema SQL MVP controller chốt tại migration này. Mọi thay đổi sau = migration mới + update doc này.

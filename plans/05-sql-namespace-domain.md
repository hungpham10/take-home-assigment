# Plan 05 — SQL Migration 0002: Namespace & Component Domain

## Mục tiêu
Schema cho mô hình namespace-first: Environment chứa services/databases/ingress/dependencies/network policies.

## Dependencies
- Plan 03 (quy ước), Plan 04 (pattern events).

## Deliverables — `sql/0002_namespace_component.sql`

### `tbl_namespaces`
- `id UUID PK`, `tenant_id` immutable, `owner_user_id` immutable, `name` (unique per tenant), `description`.
- `status TEXT NOT NULL DEFAULT 'DRAFT'` — hợp lệ: `DRAFT, VALIDATING, INVALID, APPLYING, DATA_LOADING, READY, DEGRADED, DELETING, DELETED`.
- `ready_for_loadtest BOOL NOT NULL DEFAULT false`.
- `version INT NOT NULL DEFAULT 1` (optimistic concurrency / ETag).
- TTL/cleanup: `ttl_expires_at TIMESTAMPTZ NULL`, `cleanup_policy JSONB`.
- Index: `(tenant_id, name)` unique; `(status)` cho reaper; `(ttl_expires_at) WHERE ttl_expires_at IS NOT NULL`.
- `tbl_namespace_events` theo pattern Plan 04 + `fn_namespace_transition`.

### Component tables (FK `ON DELETE CASCADE` về namespace, có `version` riêng, không cần events — dùng audit log)
- `tbl_services` (`namespace_id`, `name`, `openapi_spec JSONB` hoặc ref, `spec_hash`, `parsed_summary JSONB`, unique `(namespace_id, name)`).
- `tbl_databases` (`namespace_id`, `name`, `engine`, `profile JSONB`).
- `tbl_ingress` (`namespace_id`, `name`, `host`, `paths JSONB`).
- `tbl_dependencies` (`namespace_id`, `type` (service|database|mock), `ref_id`, `config JSONB`) — nguồn suy ra NetworkPolicy.
- `tbl_network_policies` (`namespace_id`, `resolved_from_dependencies`, `spec JSONB`, `observed_generation`).

### Docs
- Cập nhật `docs/database-schema.md` + `docs/namespace-management-api.md` (nếu cần lệch schema).

## Verification
- CI sql-migrations áp `0001→0002` tuần tự → xanh.
- Test: transition `DRAFT→READY` phải bị từ chối (phải qua VALIDATING/APPLYING); unique `(tenant_id, name)` chặn trùng.

## Ghi chú
- Không cho xóa cứng namespace row — chỉ transition sang `DELETED` (soft delete, reaper dọn tài nguyên K8s).

# Plan 07 — SQL Migration 0004: Platform Plumbing

## Mục tiêu
Các bảng hạ tầng dùng chung: idempotency, transactional outbox, quota ledger, datasets, và transition map chung.

## Dependencies
- Plan 03, 04 (pattern); tham chiếu namespace/script/loadtest (05, 06).

## Deliverables — `sql/0004_platform_plumbing.sql`

### `tbl_idempotency_keys`
- `key TEXT`, `endpoint TEXT`, `tenant_id UUID`, `request_hash TEXT`, `response_status INT`, `response_body JSONB`, `created_at`.
- UNIQUE `(tenant_id, endpoint, key)` — phục vụ middleware bắt buộc Idempotency-Key trên `POST /namespaces`, `/apply`, `/loadtests`, `/loadtests/{id}/stop`.
- TTL cleanup: index `created_at`, reaper xóa key cũ (>24h).

### `tbl_outbox_events`
- `id BIGSERIAL`, `aggregate_type`, `aggregate_id`, `event_type`, `payload JSONB`, `dispatched_at NULL`, `attempts INT`, `next_retry_at`.
- Ghi **cùng transaction** với state change (outbox pattern); dispatch bởi worker controller; index partial `(id) WHERE dispatched_at IS NULL`.

### `tbl_reservation_ledger`
- `id`, `tenant_id`, `resource_type` (vusers|concurrent_loadtests|mock_pool), `amount INT`, `load_test_id NULL`, `state` (RESERVED|COMMITTED|RELEASED), `expires_at`.
- Dùng tính quota + trả 429 khi vượt.

### `tbl_datasets`
- `id`, `tenant_id`, `namespace_id NULL`, `name`, `source_ref` (S3 pg_dump custom format), `size_bytes`.
- `pii_classification TEXT CHECK IN ('masked','synthetic','non_prod_raw')` + check pattern PII lúc import (app-level).
- `status`: `IMPORTING, AVAILABLE, INVALID, EXPIRED, SUSPENDED, DELETING, DELETED`.
- `restore_config JSONB` (vd `{timeout:"30m", parallel_jobs:4}`).
- `tbl_dataset_events` + `fn_dataset_transition` (pattern Plan 04).

### `tbl_status_transitions` (nếu chọn transition map chung — quyết định từ Plan 04)
- `object_type TEXT`, `from_status TEXT`, `to_status TEXT`, `allowed BOOL NOT NULL DEFAULT true`.
- UNIQUE `(object_type, from_status, to_status)`; seed toàn bộ transition hợp lệ của namespace/script/loadtest/dataset.

### Migration runner
- `pkgs/store/migrate.go`: đọc `sql/*.sql` theo thứ tự, track `tbl_schema_migrations`, chạy trong transaction.

## Verification
- CI migration 0001→0004 tuần tự → xanh.
- Test: idempotency UNIQUE chặn trùng key; outbox row ghi cùng transaction với `fn_loadtest_transition` hoặc rollback cả hai.

## Ghi chú
- Sau plan này, schema SQL cho MVP controller được coi là **chốt** — mọi thay đổi sau phải qua migration mới + update docs.

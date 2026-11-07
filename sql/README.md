# SQL Conventions

`sql/` là **nguồn chân lý duy nhất** của schema PostgreSQL (theo AGENTS.md). Không alter DB ngoài migration.

## Migration files
- Tên: `NNNN_<tên_domain>.sql`, đánh số tăng dần, áp dụng **tuần tự**.
- Up-only; không viết down migration (revert = migration mới).
- CI job `sql-migrations` áp toàn bộ vào Postgres sạch bằng `psql -v ON_ERROR_STOP=1` — mọi migration phải chạy từ đầu không lỗi.
- Migration runner của controller (Plan 07) track bảng `tbl_schema_migrations (id, filename, applied_at)`.

## Naming
- Bảng chính (current state): `tbl_<object>` — vd `tbl_namespaces`, `tbl_load_tests`.
- Bảng sự kiện trạng thái: `tbl_<object>_events` — vd `tbl_namespace_events`.
- DB function chuyển trạng thái: `fn_<object>_transition(...)` — xem plan 04.
- Index: `idx_<table>_<cols_underscore>`; trigger: `trg_<table>_<mục_đích>`.
- Tất cả identifier lowercase snake_case.

## Standard columns
Mọi bảng chính:
- `id UUID PRIMARY KEY DEFAULT gen_random_uuid()`
- `tenant_id UUID` (immutable, optional với bảng toàn cục)
- `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
- `updated_at TIMESTAMPTZ NOT NULL DEFAULT now()` + trigger `fn_touch_updated_at()` tự cập nhật.
- `version INT NOT NULL DEFAULT 1` cho object có optimistic concurrency (ETag/If-Match).

## Current state vs events
- `tbl_<object>` = **current state** — nguồn đọc duy nhất cho status/version; queries đọc (list/get) luôn từ đây, tối ưu bằng JOIN + index.
- `tbl_<object>_events` = append-only log chuyển trạng thái; chỉ đọc khi xem lịch sử/timeline.
- Transition chỉ qua `fn_<object>_transition` trong 1 transaction (Plan 04); app không bao giờ `UPDATE status` trực tiếp.

## Append-only
`tbl_*_events`, `tbl_audit_log`, `tbl_test_events`: `REVOKE UPDATE, DELETE` cho app role.

## Enums
Dùng `TEXT` + `CHECK (status IN (...))` (dễ migrate thêm trạng thái hơn native enum).

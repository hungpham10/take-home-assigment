# Plan 06 — SQL Migration 0003: Script & Loadtest Domain

## Mục tiêu
Schema cho lifecycle script validation và load test (k6).

## Dependencies
- Plan 04 (events pattern), Plan 05 (namespace FK).

## Deliverables — `sql/0003_script_loadtest.sql`

### `tbl_scripts`
- `id`, `namespace_id` FK, `name`, `content_ref` (ConfigMap/staging ref), `content_hash`, `size_bytes` (CHECK `<= 1048576` — giới hạn ConfigMap 1MiB).
- `status`: `PENDING_VALIDATION, VALIDATING, VALIDATED, INVALID, VALIDATION_FAILED`.
- `version`, timestamps + `tbl_script_events` + `fn_script_transition`.

### `tbl_script_validations`
- `script_id` FK, `attempt INT`, `status`, `errors JSONB` (chi tiết lỗi để trả 422), `validated_at`.
- Unique `(script_id, attempt)`.

### `tbl_load_tests`
- `id`, `namespace_id` FK, `script_id` FK, `name`, `created_by`.
- `load_config JSONB` (vd `{vusers:200, duration:"10m", timeout_minutes:15}`).
- `targets JSONB NOT NULL` — bắt buộc, resolved từ parsed OpenAPI của services.
- `runner_type TEXT CHECK IN ('k8s','local')`, `runner_ref` (TestRun name / job id).
- `status`: `QUEUED, PROVISIONING, RUNNING, COMPLETED, FAILED, STOPPING, STOPPED, TIMED_OUT, TIMEOUT_CLEANED, CLEANUP_FAILED` (`CANCELED` reserved, không dùng).
- `started_at`, `finished_at`, `timeout_at` (dành cho reaper), `version`.
- `tbl_loadtest_events` + `fn_loadtest_transition`.

### `tbl_test_events`
- Stream event runtime: `load_test_id`, `seq BIGINT`, `type` (log|metric|status), `payload JSONB`, `created_at`; index `(load_test_id, seq)`.
- Append-only.

### Invariant (CHECK / function-level)
- Loadtest chỉ tạo khi namespace `ready_for_loadtest = true` (app enforce + guard trong function tạo).
- `STOPPING` chỉ đến từ `RUNNING|PROVISIONING|QUEUED`.

## Verification
- CI migration xanh; SQL test: lifecycle chuẩn QUEUED→…→COMPLETED qua `fn_loadtest_transition`; transition `COMPLETED→RUNNING` bị chặn; size check chặn script >1MiB.

## Ghi chú
- Tạo loadtest cần gate script `VALIDATED` + `SCRIPT_NOT_VALIDATED` → 409 (app enforce; test integration ở Plan 13).

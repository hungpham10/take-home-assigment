# Plan 12 — Scripts API & Validation Lifecycle

## Mục tiêu
Upload/validate k6 scripts với lifecycle rõ ràng, validation chạy async.

## Dependencies
- Plan 06 (schema), Plan 10 (API nền), Plan 04 (transition).

## Deliverables
- Endpoints `/api/loadtest/v1/namespaces/{id}/scripts`: upload (≤1MiB, CHECK DB chặn), list/get/delete (soft), `POST /scripts/{id}/validate`.
- Validation job (async): 
  - Local mode: chạy `k6 inspect` trong process/worker pool.
  - K8s mode: Job chạy `k6 inspect`/archive (theo docs), watcher observe kết quả.
  - Kết quả ghi `tbl_script_validations` → transition `PENDING_VALIDATION→VALIDATING→VALIDATED|INVALID|VALIDATION_FAILED`.
- Trả lỗi validation chi tiết (JSONB) để client fix; remote refs trong script phải qua validation (SSRF-safe resolution — Plan 13 dùng targets đã resolve).

## Verification
- TDD: upload script hợp lệ → validate → `VALIDATED`; script cú pháp sai → `INVALID` + errors; script >1MiB → 422.

## Ghi chú
- Reuse pattern events Plan 04 cho `tbl_script_events`.

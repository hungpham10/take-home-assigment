# Plan 10 — Namespace API (`/api/loadtest/v1/namespaces`)

## Mục tiêu
CRUD namespace với Idempotency-Key, ETag optimistic concurrency, và state transition qua DB function (Plan 04) — không UPDATE status trực tiếp.

## Dependencies
- Plan 05 (schema), Plan 04 (transition function), Plan 09 (authz).

## Deliverables
- CRUD endpoints + component endpoints (`/services`, `/databases`, `/ingress`, `/dependencies`, `/network-policies`).
- Middleware Idempotency-Key (bảng `tbl_idempotency_keys`): bắt buộc trên `POST /namespaces`, `/apply`; replay trả cached response.
- ETag/`If-Match` từ `version` — mismatch → `412`.
- Error mapping: thiếu/x INVALID OpenAPI → `422`; transition trái phép / namespace chưa READY → `409 NAMESPACE_NOT_READY`; vượt quota → `429`.
- Transition gọi `fn_namespace_transition` trong transaction; đồng thời ghi outbox event (Plan 07).
- List/get có filter `status`, pagination.

## Verification
- Unit + integration test (testcontainers Postgres): happy path CRUD; replay idempotent; If-Match sai → 412; transition `DRAFT→READY` trực tiếp → 409.

## Ghi chú
- RBAC: user chỉ thấy namespace của mình; admin thấy cả tenant.

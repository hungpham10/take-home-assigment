# Plan 11 — OpenAPI Validation, Readiness & Apply

## Mục tiêu
Gate chất lượng cho namespace: OpenAPI bắt buộc per service, index `targets[]` từ spec, readiness criteria, và apply flow.

## Dependencies
- Plan 10 (namespace API), Plan 05 (component schema).

## Deliverables
- `pkgs/serviceopenapi`:
  - Parse + validate spec khi tạo/cập nhật service → lỗi → `422` và chặn readiness.
  - Index operations → `targets[]` (`operationId` hoặc `method+path` — quyết định cuối trong plan này, khớp docs architecture.md §8).
- Endpoints:
  - `POST /namespaces/{id}/validate` — chạy validation toàn bộ components → transition `DRAFT→VALIDATING→INVALID|APPLYING`.
  - `POST /namespaces/{id}/apply` — resolve dependencies → network policies spec → transition `APPLYING→DATA_LOADING→READY` (MVP: set `ready_for_loadtest` khi xong).
  - `GET /namespaces/{id}/readiness` — checklist chi tiết từng gate.
- Outbox events cho mỗi bước; audit log.

## Verification
- TDD: service thiếu spec → 422; spec sai → 422 + errors JSONB chi tiết; apply thành công → `READY` + `ready_for_loadtest=true` + events đầy đủ.

## Ghi chú
- Mock capacity check (`INSUFFICIENT_MOCK_CAPACITY` → 409) để sau khi có mock-service; chừa hook trong readiness.

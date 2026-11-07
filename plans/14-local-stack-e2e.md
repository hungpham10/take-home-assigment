# Plan 14 — Local Stack, E2E Smoke Test & k6 Perf Test

## Mục tiêu
Chạy được toàn bộ MVP trên máy: docker compose, seed data, smoke test end-to-end, perf test là merge gate.

## Dependencies
- Plan 08–13 (đầy đủ controller).

## Deliverables
- `docker-compose.yml`: postgres, nginx (dùng `conf/nginx` có sẵn), controller (build image qua Earthfile), k6 (nếu cần cho local runner).
- **`Earthfile`** — build container image theo pattern `sources/opsense-rs/Earthfile` (multi-stage: deps → build → runtime image tối giản; target `+controller-image`, `+mocking-image` khi có mock-service; dùng cho compose `build:` và CI integration images).
- Seed script (tenant demo + user admin + token `abt_`).
- E2E smoke (script hoặc Go test): login qua nginx → tạo namespace → add service + OpenAPI → validate → apply → READY → upload script → validate → tạo loadtest (local runner) → RUNNING → COMPLETED → events/metrics đọc được → stop/reuse flows.
- `tests/perf/` k6: benchmark theo bảng ước lượng dưới đây — kết quả là quality gate trước merge (AGENTS.md).

### Performance targets (k6) — ước lượng QPS per API (control-plane, bursty profile)

| API | Ước lượng QPS (peak) | Ghi chú |
|---|---|---|
| `GET /health`, `/ready`, `/metrics` | 5–10 | probe |
| `POST /api/admin/v1/token/introspect` | 50–100 | nginx gọi mỗi request có token; cache 60s giảm DB hit |
| `GET /api/admin/v1/tenant/{host}/auth-config` | 20–50 | cache ~600s phía nginx |
| `GET /api/admin/v1/tenant/{host}/id` | 20–50 | |
| Namespaces CRUD (GET list/get) | 30–50 | đọc current-state `tbl_<object>`, JOIN + index |
| `POST /namespaces`, `/apply` | 1–5 | bursty, Idempotency-Key |
| Scripts upload/validate | 2–5 | |
| Loadtests create/stop | 1–5 | bursty |
| `GET /loadtests/{id}/events` | 10–20 | cold data, index `(object_id, id)` |
| `/api/loadtest/v1/me`, `/guardrails` | 10–20 | |

Latency targets: **p95 < 100ms** cho reads, **p95 < 300ms** cho writes (trừ apply/validate — async, đo thời gian enqueue thay vì hoàn tất). k6 script benchmark từng nhóm API trên.
- README cập nhật cách chạy toàn bộ.

## Verification
- `docker compose up -d` → chạy smoke script pass từ đầu đến cuối.
- k6 perf test đạt target; CI có job chạy perf test (tuỳ chọn, nightly).

## Ghi chú
- mock-service để phase sau (sau khi controller ổn) — chừa endpoint mock-status/mock pools stub theo docs.
- Sau plan này: CodeGraph review, bump version `scripts/bump.sh`, PR merge gates.

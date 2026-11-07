# Plan 08 — Controller Bootstrap (`cmds/loadtest-controller`)

## Mục tiêu
Dựng skeleton service control-plane: HTTP server go-chi, health endpoints, config, Postgres, migration runner, Swagger.

## Dependencies
- Plan 01 (go.mod), Plan 03–07 (schema + migration runner).

## Deliverables
- `cmds/loadtest-controller/main.go`: wire config → logger → pgx pool → router → server (graceful shutdown).
- `pkgs/config`: env config (DSN, port, log level, runner mode).
- `pkgs/httpx`: response helpers (error envelope chuẩn, mapping lỗi DB → 409/422/404).
- Endpoints: `GET /health`, `GET /ready` (check DB), `GET /metrics` (prometheus).
- Swagger/OpenAPI spec phục vụ API (`/api/loadtest/v1`, `/api/admin/v1` — docs sẵn trong `docs/bff-loadtest-api-spec.md`).
- TDD: test HTTP handlers bằng `httptest`.

## Verification
- `go test ./...` xanh; `docker compose up postgres` + chạy controller → `/ready` 200, migrations áp dụng.

## Ghi chú
- Mock-service KHÔNG dùng package controller (biên theo AGENTS.md).

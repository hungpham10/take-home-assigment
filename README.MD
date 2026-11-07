# Load Testing Platform (MVP)

Self-service ephemeral load testing platform. Namespace-first: một **Environment** chứa services/databases/ingress/dependencies; load test (k6) chỉ chạy khi namespace `READY`.

## Thành phần (MVP)
- `cmds/loadtest-controller` — control plane BFF: namespace/component CRUD, OpenAPI validation, script/loadtest lifecycle, RBAC. DB: PostgreSQL.
- `cmds/mock-service` — data plane mock runtime (phase sau).
- `conf/nginx` — API gateway/routing (OIDC/JWT/tenant headers).
- `sql/` — PostgreSQL migrations (single source of truth).

## Development

```bash
make test    # go test ./... -race
make lint    # golangci-lint run
make build   # build cmds/loadtest-controller -> bin/
```

Yêu cầu: Go 1.23+, golangci-lint, Docker (local stack qua `docker compose`).

## Workflow
- Thiết kế/plan trước code: xem [plans/README.md](plans/README.md) (thứ tự thực thi 01→14).
- Tài liệu kiến trúc & contract API: [docs/](docs/).
- Mỗi plan = 1 feature branch + 1 PR, merge bằng **squash and merge** (xem [AGENTS.MD](AGENTS.MD)).
- CI: GitHub Actions — lint + `go test -race` + build + validate SQL migrations.

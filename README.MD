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

## Local stack & E2E

```bash
docker compose up -d --build          # postgres + controller (migrations tự chạy)
./tests/e2e-smoke.sh                  # smoke: ns→service→validate→apply→script→loadtest→delete
# k6 perf (merge gate):
k6 run -e BASE=http://localhost:8080 -e TENANT=<uuid> -e USER=<uuid> tests/perf/control-plane.js
```

Container image build qua Earthly (pattern opsense-rs): `earthly +controller-image`.
Secrets: SOPS `env/secrets.<ENV>.enc.yaml` — xem `docs/secrets-management.md`.

## Workflow
- Thiết kế/plan trước code: xem [plans/README.md](plans/README.md) (thứ tự thực thi 01→14).
- Tài liệu kiến trúc & contract API: [docs/](docs/).
- Mỗi plan = 1 feature branch + 1 PR, merge bằng **squash and merge** (xem [AGENTS.MD](AGENTS.MD)).
- CI: GitHub Actions — lint + `go test -race` + build + validate SQL migrations.

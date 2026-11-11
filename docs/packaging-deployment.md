# Packaging & Deployment — AIO serve image + Dex dev stack (plans/34)

Pattern tham chiếu: `sources/opsense-rs/Earthfile` + `docker-compose.yml` (multi-stage
Earthly, fast-path prebuilt binary, compose OIDC stack, integration test suite).

## 1. Images (Earthfile)

| Target | Image | Nội dung |
|---|---|---|
| `+build` | — | Build 2 Go binary: `loadtest-controller`, `mock-service` (golang:1.23-bookworm). Fast path: copy `bin/` từ host nếu có (`COPY --if-exists`), fallback `go build` trong Earthly. |
| `+serve` | `loadtest-platform-serve` | **AIO image** (openresty 1.27.1.2-bookworm-fat): nginx OpenResty + supervisor + **controller (unix socket)** + **mock-service (TCP :8090)** + alloy. Entrypoint `scripts/entrypoint.sh` → SOPS decrypt → supervisord. |
| `+mock-service` | `loadtest-platform-mock-service` | Data-plane unit riêng (debian-slim + binary, TCP :8090) — dùng cho k8s deployment mock-service scale ngang. |
| `+test` | — | `go test ./... -race` trong container (CI fallback). |
| `+all` | | Build & push `serve` + `mock-service`. |
| `+multi` | | `+all` cho `linux/amd64` + `linux/arm64` (BUILD --platform x2). |
| `+integration-images` | | Build local `loadtest-platform-serve:local` + `loadtest-platform-mock-service:local`, **no push** — dùng bởi `make test-integration`. |

Registry/tag: `ghcr.io/hungpham10/loadtest-platform/{serve,mock-service}:${VERSION}` (ARG global `REGISTRY`/`IMAGE_PREFIX`/`VERSION`).

### AIO serve — process model (supervisor `conf/supervisor/ltp.conf`)

| Program | Command | Listener |
|---|---|---|
| `controller` | `/app/loadtest-controller` | `ADDR=unix:/var/run/axum` — controller tự tạo unix socket, nginx upstream `unix:/var/run/axum` (giữ nguyên `conf/nginx`, không đổi sang TCP) |
| `mock-service` | `/app/mock-service` | `ADDR=:8090` (internal, không có public vhost — architecture.md khóa "no public ingress") |
| `nginx` | `/app/nginx.sh` | OpenResty foreground, port 8080 (cổng public duy nhất) |
| `alloy` | `/app/alloy.sh` | metrics scrape `localhost:8080` (gateway), `USE_ALLOY` toggle |

Entrypoint chain (`scripts/entrypoint.sh` → `scripts/release.sh`):
`mkdir -p /var/run/axum` → `decrypt_secrets` (SOPS age, `/app/secrets/secrets.<APP_ENV>.enc.yaml`) →
`prepare` (không chạy SQL — **controller tự migrate qua embedded SQL** `embed.go`; chỉ đợi PostgreSQL sẵn sàng
nếu `POSTGRES_HOST` set) → `boot` (sed placeholders `%%HTTP_SERVER%%`, `%%NGINX_LOG%%` trong conf nginx) → `exec supervisord -n`.

## 2. Dex dev stack (docker-compose.yml)

Services: `postgres:16-alpine`, `valkey:9-alpine` (Redis cho lua-resty-session, `noeviction`),
`dex` (`ghcr.io/dexidp/dex:v2.41.1`, mount `conf/dex/config.dev.yaml`, port 5556),
`loadtest-serve` (port 8080), `mock-service` (port 8090, optional).

Dex config dev (`conf/dex/config.dev.yaml`, pattern opsense-rs):
- issuer `http://localhost:5556/dex` (port-forward compose → host test dùng được cùng URL)
- static client `ltp-test` / redirect `http://localhost:8080/callback`
- static user `dev-user@example.com` / `password` (bcrypt)

OIDC auth flow vẫn như `docs/nginx-oidc-auth.md`: nginx Lua lấy auth-config per-tenant
từ controller `GET /api/admin/v1/tenant/{host}/auth-config?name=` (issuer seed trong DB
là `http://dex:5556/dex` — in-network), env `OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET` chỉ là fallback.

## 3. Integration test (pattern opsense-rs)

- Suite Go `tests/integration/` (build tag `//go:build integration`):
  - `common.go` — wait `/health` qua gateway; skip nếu stack không reachable,
    **fail hard khi `LTP_INTEGRATION=1`** (chống green giả trong CI).
  - `dex.go` — port Go của opsense `common/dex.rs`: discovery → GET authorize → parse form →
    POST credentials → approval → capture `code=` → token endpoint → RS256 id_token.
  - `integration_health_test.go` — smoke `GET http://127.0.0.1:8080/health` qua nginx.
  - `integration_oauth_test.go` — login Dex từ host (`http://localhost:5556/dex`) →
    flow `/login` + `/callback` của gateway → bearer JWT gọi endpoint admin.
  - Tenant seed issuer in-network (`http://dex:5556/dex`) tại test bootstrap (psql, pattern `tests/e2e-smoke.sh`).
- `scripts/mint-dex-token.py` — CLI host-side (stdlib): mint id_token từ Dex cho debug/manual test.
- Makefile: `test-integration` (earthly +integration-images → `docker compose up -d --wait` →
  `go test -tags integration ./tests/integration/... -count=1`), `test-integration-down`, `up`, `down`.

## 4. Bootstrap checklist (dev)

```sh
earthly +integration-images          # build 2 image local
docker compose up -d --wait          # postgres + valkey + dex + serve
curl http://localhost:8080/health    # qua nginx → UDS → controller
make test-integration                # full suite
```

## 5. Deploy lên Kubernetes (kind / multi-cluster sau)

Manifest kustomize tại `deploy/k8s/` (base + overlay `kind/`), RBAC least-privilege cho controller, và secrets 2 tầng (SOPS/age file-level + `pkgs/secrets` field-level AES-256-GCM).

- Hướng dẫn chi tiết: `docs/k8s-deployment.md` (sinh age key, mã hoá secrets, deploy, port-forward, bearer token từ Dex, CLI `sops` encrypt/decrypt, CI job).
- Plan chi tiết: `plans/36-k8s-manifests.md`.

```sh
earthly +integration-images
kind load docker-image loadtest-platform-serve:local loadtest-platform-mock-service:local --name ltp
kubectl apply -k deploy/k8s/overlays/kind
kubectl -n ltp-system rollout status deploy/serve --timeout=180s
kubectl -n ltp-system port-forward svc/serve 8080:8080 &
TOKEN=$(python3 scripts/mint-dex-token.py)
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" http://localhost:8080/health
```

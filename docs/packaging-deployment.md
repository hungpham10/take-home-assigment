# Packaging & Deployment — AIO serve image + Dex dev stack (plans/34)

Reference pattern: `sources/opsense-rs/Earthfile` + `docker-compose.yml` (multi-stage
Earthly, fast-path prebuilt binary, compose OIDC stack, integration test suite).

> **Version = git tag** (`docs/versioning.md`, plan 38). Both the binary and the image carry
> the version from the tag; `/version` is the runtime truth.

## 1. Images (Earthfile)

| Target | Image | Contents |
|---|---|---|
| `+build` | — | Builds the 2 Go binaries with version ldflags (golang:1.26-bookworm). Fast path: copy `bin/` from the host **only when** the ELF and `--version` match `VERSION`; on a mismatch ⇒ `go build` inside Earthly. |
| `+serve` | `loadtest-platform-serve` | **AIO image** (openresty 1.27.1.2-bookworm-fat): nginx OpenResty + supervisor + **controller (unix socket)** + **mock-service (TCP :8090)** + alloy. Entrypoint `scripts/entrypoint.sh` → SOPS decrypt → supervisord. |
| `+mock-service` | `loadtest-platform-mock-service` | Standalone data-plane unit (debian-slim + binary, TCP :8090) — used by the k8s mock-service deployment for horizontal scaling. |
| `+test` | — | `go test ./... -race` in the container (CI fallback). |
| `+all` | — | Build & push `serve` + `mock-service` (pushes only when run with `--push`). |
| `+multi` | — | `+all` for `linux/amd64` + `linux/arm64`. |
| `+integration-images` | — | Builds local `loadtest-platform-serve:local` + `loadtest-platform-mock-service:local`, **no push** — used by `make test-integration`. |

Registry/tag (release, `earthly --push --build-arg VERSION=X.Y.Z ... +all`):
`ghcr.io/<owner>/loadtest-platform/{serve,mock-service}:<VERSION>` **and** `:latest`.
Dev/local keeps the `:local` tag (overlay `kind`, `imagePullPolicy: IfNotPresent`).

Global Earthfile args: `REGISTRY`, `IMAGE_PREFIX`, `VERSION` (default `dev`),
`GIT_COMMIT`, `BUILD_DATE`. `VERSION`/`GIT_COMMIT` are embedded into the binary via
`-ldflags` (`pkgs/version`).

### AIO serve — process model (supervisor `conf/supervisor/ltp.conf`)

| Program | Command | Listener |
|---|---|---|
| `controller` | `/app/loadtest-controller` | `ADDR=unix:/var/run/axum` — the controller creates the unix socket itself, nginx upstream `unix:/var/run/axum` (keeps `conf/nginx` unchanged, no switch to TCP) |
| `mock-service` | `/app/mock-service` | `ADDR=:8090` (internal, no public vhost — architecture.md locks "no public ingress") |
| `nginx` | `/app/nginx.sh` | OpenResty foreground, port 8080 (the only public port) |
| `alloy` | `/app/alloy.sh` | metrics scrape `localhost:8080` (gateway), `USE_ALLOY` toggle |

Entrypoint chain (`scripts/entrypoint.sh` → `scripts/release.sh`):
`decrypt_secrets` (SOPS age, `/app/secrets/secrets.<APP_ENV>.enc.yaml`) →
`prepare` (does not run SQL — the **controller migrates itself via embedded SQL** `embed.go`; only waits for PostgreSQL
to be ready if `POSTGRES_HOST` is set) → `boot` (sed placeholders `%%HTTP_SERVER%%`, `%%NGINX_LOG%%`, `%%NGINX_RESOLVER%%` in the nginx conf) → `exec supervisord -n`.

> `/var/run/axum` is a **unix socket file**, created by `pkgs/listener` (mkdir parent + remove stale + chmod 666).
> Do not `mkdir -p /var/run/axum` and do not mount a volume on this path — turning it into a directory will make the
> controller fail to bind. Details: `docs/k8s-deployment.md`, `docs/nginx-oidc-auth.md`.

## 2. Dex dev stack (docker-compose.yml)

Services: `postgres:16-alpine`, `valkey:9-alpine` (Redis for lua-resty-session, `noeviction`),
`dex` (`ghcr.io/dexidp/dex:v2.41.1`, mount `conf/dex/config.dev.yaml`, port 5556),
`loadtest-serve` (port 8080), `mock-service` (port 8090, optional).

Dex dev config (`conf/dex/config.dev.yaml`, opsense-rs pattern):
- issuer `http://localhost:5556/dex` (compose port-forward → host tests can use the same URL)
- static client `ltp-test` / redirect `http://localhost:8080/callback`
- static user `dev-user@example.com` / `password` (bcrypt)

The OIDC auth flow stays as in `docs/nginx-oidc-auth.md`: nginx Lua fetches the per-tenant auth-config
from the controller `GET /api/admin/v1/tenant/{host}/auth-config?name=` (the issuer seeded in the DB
is `http://dex:5556/dex` — in-network), the `OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET` env vars are only a fallback.

## 3. Integration test (opsense-rs pattern)

- Go suite `tests/integration/` (build tag `//go:build integration`):
  - `common.go` — wait for `/health` through the gateway; skip if the stack is not reachable,
    **fail hard when `LTP_INTEGRATION=1`** (guards against false greens in CI).
  - `dex.go` — Go port of opsense `common/dex.rs`: discovery → GET authorize → parse form →
    POST credentials → approval → capture `code=` → token endpoint → RS256 id_token.
  - `integration_health_test.go` — smoke `GET http://127.0.0.1:8080/health` through nginx.
  - `integration_oauth_test.go` — Dex login from the host (`http://localhost:5556/dex`) →
    gateway `/login` + `/callback` flow → bearer JWT calls the admin endpoint.
  - Tenant seed issuer in-network (`http://dex:5556/dex`) at test bootstrap (psql, `tests/e2e-smoke.sh` pattern).
- `scripts/mint-dex-token.py` — host-side CLI (stdlib): mint an id_token from Dex for debug/manual test.
- Makefile: `test-integration` (earthly +integration-images → `docker compose up -d --wait` →
  `go test -tags integration ./tests/integration/... -count=1`), `test-integration-down`, `up`, `down`.

## 4. Bootstrap checklist (dev)

```sh
earthly +integration-images          # build 2 local images
docker compose up -d --wait          # postgres + valkey + dex + serve
curl http://localhost:8080/health    # via nginx → UDS → controller
make test-integration                # full suite
```

## 5. Deploy to Kubernetes (kind / multi-cluster later)

Kustomize manifests under `deploy/k8s/`:

| Overlay | Image | Used for |
|---|---|---|
| `kind` | `loadtest-platform-{serve,mock-service}:local`, `IfNotPresent` | local dev, in-cluster CI (image pre-loaded into kind) |
| `release` | `ghcr.io/hungpham10/loadtest-platform/*:<version>`, `Always` | deploying a real release |

Render the manifest pinned to the right tag (docs/versioning.md):

```sh
make k8s-render                       # release overlay, tag = latest git tag
make k8s-render VERSION=0.1.1         # pick a specific tag
make k8s-render OVERLAY=kind          # :local
kubectl apply -f bin/k8s/release.yaml
```

`scripts/k8s-render.sh` copies the overlay to a temp dir, writes `newTag` = version, then
runs `kubectl kustomize` into `bin/k8s/<overlay>.yaml`. The runtime truth is still
`GET /version` on the pod (do not trust the tag in the manifest if the pod has not rolled out).

- Detailed guide: `docs/k8s-deployment.md` (generate age key, encrypt secrets, deploy, port-forward, bearer token from Dex, `sops` CLI encrypt/decrypt, CI job).
- Detailed plan: `plans/36-k8s-manifests.md`, versioning: `plans/38-version-management.md`.

```sh
earthly +integration-images
kind load docker-image loadtest-platform-serve:local loadtest-platform-mock-service:local --name ltp
kubectl apply -k deploy/k8s/overlays/kind
kubectl -n ltp-system rollout status deploy/serve --timeout=180s
kubectl -n ltp-system port-forward svc/serve 8080:8080 &
TOKEN=$(python3 scripts/mint-dex-token.py)
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" http://localhost:8080/health
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" http://localhost:8080/version
```

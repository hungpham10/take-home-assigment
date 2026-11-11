# plans/34 — AIO serve image + Dex compose + integration tests

Pattern: `sources/opsense-rs/Earthfile` + `docker-compose.yml`. Docs: `docs/packaging-deployment.md`.

## Quyết định (2026-11-11)
- **Backend upstream nginx → controller: giữ UDS** (`unix:/var/run/axum`, `ADDR=unix:/var/run/axum`). Không đụng 14 call-site Lua `httpc:connect("unix:...")`. HTTP client library `pkgs/listener` hỗ trợ cả unix + TCP.
- **Integration test OAuth: bearer trực tiếp từ Dex** — mint id_token qua HTML login flow của Dex, gửi `Authorization: Bearer` qua nginx, verify JWT/JWKS + header injection. Không test full browser `/login`+`/callback`.
- `docs/mock-service-api.md` diff (297 dòng) giữ trong PR này (reconcile docs plans 27–33), commit chung.
- máy dev: chỉ có `docker-compose` (v1), Makefile dùng `COMPOSE ?= docker-compose`.

## Mục tiêu
1. Earthfile rewrite: image AIO `serve` (OpenResty + supervisor + controller-UDS + mock-service + alloy), image `mock-service` riêng, targets `test/all/multi/integration-images`.
2. docker-compose: postgres + valkey (noeviction, session lua) + dex v2.41.1 (conf/dex/config.dev.yaml, issuer localhost:5556/dex) + loadtest-serve (SOPS mount, healthcheck /health) + mock-service (optional).
3. Integration test suite Go (pattern opsense-rs) + Makefile `test-integration`.
4. Controller unix socket (TDD): `ADDR=unix:/var/run/axum` → `net.Listen("unix")` + rm stale socket + chmod 666; TCP giữ nguyên. nginx `conf/nginx` KHÔNG đổi.
5. Sửa các blocker tìm được khi review working tree (xem phần Checklist).

## Checklist
- [x] Docs: `docs/packaging-deployment.md` (mới), cập nhật `docs/nginx-oidc-auth.md`, `docs/mvp-status.md`
- [x] TDD: unix-socket listener — failing test trước (`cmds/loadtest-controller`), sau đó implement (`pkgs/listener` + `net.Listen("unix")` + rm stale socket + chmod 666; TCP giữ nguyên)
- [x] `conf/supervisor/ltp.conf` — 4 programs: controller (unix), mock-service (:8090), nginx, alloy
- [x] `conf/dex/config.dev.yaml` — issuer localhost:5556/dex, client `ltp-test`, redirect :8080/callback, static user dev
- [x] `conf/config.alloy` — label `rust-server` → `loadtest-controller`
- [x] `scripts/entrypoint.sh` — mkdir `/var/run/axum`; `scripts/release.sh` — `prepare` chỉ wait-DB (không chạy SQL, controller tự migrate), `boot` sed placeholders nginx
- [x] Earthfile: `build` (2 binary, fast path `bin/`), `serve` (openresty-fat + lua-resty libs + alloy), `mock-service`, `test`, `all`, `multi`, `integration-images`; `.earthignore`
- [x] docker-compose.yml: postgres / valkey (noeviction) / dex (v2.41.1, healthcheck discovery) / loadtest-serve (healthcheck, depends_on) / mock-service
- [x] `tests/integration/` (tag `integration`): common.go (wait/skip + `LTP_INTEGRATION=1` fail hard), dex.go (login flow port của common/dex.rs), integration_health_test.go, integration_oauth_test.go (tenant seed in-network issuer)
- [x] `scripts/mint-dex-token.py` (host CLI, optional)
- [x] Makefile: `test-integration`, `test-integration-down`, `up`, `down`
- [ ] **Blocker Earthfile**: thiếu `anvouk/lua-resty-jwt-verification` + `hamishforbes/lua-ffi-zlib` (04-api.conf:17 require resty.jwt-verification-jwks → 500)
- [ ] **Blocker compose**: thiếu `NGINX_DIR` env → release.sh boot() sed vào `/http.d/default.conf` crash-loop
- [ ] **Blocker redirect_uri**: dex.go + mint-dex-token.py dùng `https://localhost:8080/callback` nhưng Dex đăng ký `http://localhost:8080/callback` → Unregistered redirect_uri
- [ ] **Refactor release.sh**: tách hàm `main "$@"`, entrypoint gọi tường minh (tránh shift ảnh hưởng positional params)
- [ ] Gates: `go test ./...` + `golangci-lint run` green; `earthly +integration-images` build ok; `make test-integration` pass

## Verification
1. `earthly +integration-images`
2. `make test-integration` (compose up --wait → go test -tags integration)
3. `curl http://localhost:8080/health`, `curl http://localhost:5556/dex/.well-known/openid-configuration`
4. `make test`, `golangci-lint run`
5. `docker-compose down -v`

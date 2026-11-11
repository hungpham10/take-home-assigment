# Plan 36 — Deploy platform lên kind (k8s manifests) + full test in-cluster trên CI

## Goal
Triển khai toàn bộ AIO serve stack (`nginx OpenResty + supervisor + controller + alloy + mock-service`), Dex OIDC, valkey, postgres control-plane lên **kind local** và chạy **full deploy E2E in-cluster** trên GitHub Actions:
- kind cluster + Calico + CNPG + k6-operator (tái dùng cơ chế có sẵn trong job "Full Deploy E2E (kind + CNPG)")
- build image `serve` + `mock-service` qua Earthly, nạp vào kind (`kind load docker-image`)
- `kubectl apply -k deploy/k8s/overlays/kind` → rollout status
- `port-forward` svc/serve (8080), svc/dex (5556) → chạy lifecycle test: bearer token từ Dex qua `scripts/mint-dex-token.py` → create-with-spec → validate → apply → loadtest COMPLETED → DELETE ns
- Tái dùng `tests/integration/...` suite qua port-forward (INT-OAUTH-01/02 + health)

Kết quả: kiểm chứng cả data-plane (gateway + Dex JWT + controller UDS) và k8s path (Env Reconciler + Status Watcher + k8s runner + CNPG env DB) trong cluster thật.

## Quyết định (đã chốt)
1. **kind local** cho MVP. Multi-cluster để sau — manifest/RBAC không hardcode tên cluster, có overlay `kind/` và khung `prod/`.
2. **Postgres riêng** (control-plane DB): `StatefulSet` + `postgres:16-alpine` + PVC 1Gi + Service `postgres.ltp-system`. CNPG `Cluster` chỉ dùng cho env DB của namespace test (`demo-db`) — không đổi logic hiện có.
3. **Secrets qua SOPS (age)**: `deploy/k8s/base/serve/secrets.enc.yaml` (commit được) mount bằng ConfigMap vào `/app/secrets/secrets.dev.enc.yaml`; `SOPS_AGE_KEY_CONTENT` qua K8s Secret; `scripts/release.sh` giải mã tại boot (tái dùng mechanism plan 34, không thêm code Go).
4. **Namespace**: `ltp-system` (serve, dex, valkey, postgres), `mock-system` (mock-service — `mockcontrol.MockServiceDNS()` hardcode `mock-service.mock-system.svc.cluster.local`).
5. **OIDC bearer-only**: không sửa `conf/nginx/vhost/02-openid.conf` (tránh đụng redirect_uri/cookie_secure/https). Test dùng bearer token từ Dex qua `scripts/mint-dex-token.py` (đã có sẵn, port từ `sources/opsense-rs/scripts/mint-dex-token.py`).
6. **CLI `sops` subcommand** (phần Plan 37 gộp vào): `loadtest-controller sops encrypt|decrypt` dùng `pkgs/secrets.Encrypt/Decrypt` (mã AES-256-GCM, pattern `sops.rs`), master key từ `LTP_MASTER_KEY` (SOPS env). Dùng để sinh/verify blob `token_encrypted` khi seed SQL.

## Files mới
```
deploy/k8s/
  base/
    kustomization.yaml
    namespace.yaml                    # ltp-system (+ mock-system)
    postgres/statefulset.yaml         # postgres:16-alpine + PVC 1Gi
    postgres/service.yaml             # postgres.ltp-system:5432
    postgres/secret.yaml              # SOPS-encrypted POSTGRES_* (mount là file)
    valkey/deployment.yaml + service.yaml
    dex/configmap.yaml                # từ conf/dex/config.dev.yaml (issuer http://localhost:5556/dex)
    dex/deployment.yaml + service.yaml
    mock-system/namespace.yaml + service.yaml + deployment.yaml
    serve/deployment.yaml             # AIO image: nginx+supervisor+controller+alloy
    serve/service.yaml                # ClusterIP :8080
    serve/serviceaccount.yaml
    serve/rbac.yaml                   # ClusterRole + ClusterRoleBinding (least-privilege)
    serve/secrets.enc.yaml            # SOPS-encrypted env (DATABASE_DSN, OIDC_*, tenant secrets, LTP_MASTER_KEY)
  overlays/
    kind/kustomization.yaml           # image :local, imagePullPolicy IfNotPresent, replicas=1, resources nhỏ
    prod/                             # khung: image registry, TLS, replicas, HPA (không triển trong plan này)
scripts/k8s-apply.sh                  # apply overlay kind + rollout status + port-forward helper
docs/k8s-deployment.md                # MỚI: hướng dẫn deploy k8s + SOPS
```

## Files sửa
- `env/secrets.dev.yaml.example`: đảm bảo có `LTP_MASTER_KEY` (đã có — giữ nguyên)
- `cmds/loadtest-controller/main.go`: dispatch `sops` subcommand trước `config.FromEnv()` (encrypt/decrypt không cần DB)
- `pkgs/secrets/crypto.go`: thêm `EncryptHex/DecryptHex` (in/ra hex, wrapper quanh Encrypt/Decrypt)
- `pkgs/secrets/crypto_test.go`: test table-driven (roundtrip, sai độ dài key, action lạ, hex hỏng, blob < 12 byte, plaintext rỗng)
- `tests/e2e-full-deploy.sh`: thêm `DEPLOY_MODE=in-cluster` (thay block postgres docker + go build controller bằng kind load + kustomize apply + port-forward); bearer token thay `X-Tenant-Id`; thêm verify gateway `/health` + bearer rác 401; gọi lại suite `tests/integration/...` qua port-forward
- `.github/workflows/ci.yml`: job `Full Deploy E2E (in-cluster)` (build images Earthly → kind load → apply → e2e script)
- `docs/packaging-deployment.md`, `docs/mvp-status.md`: trỏ về `docs/k8s-deployment.md`

## RBAC `serve` ClusterRole (theo đúng code đã dùng)
`namespaces` CRUD · `apps/deployments` CRUD (Env Reconciler render + Status Watcher watch) · `services/configmaps/secrets/serviceaccounts` CRUD · `networkpolicies/ingresses` CRUD · `pods` get/list/watch/delete · `jobs` get/list/watch/delete · `clusters.postgresql.cnpg.io` + `/status` · `elasticsearch.k8s.elastic.co` · `testruns.k6.io` + `/status` · `horizontalpodautoscalers` · `leases` (chỉ khi `HA_ENABLED=true`) · `events` create/patch.

## Khai báo và boundary
- KHÔNG sửa `conf/nginx/vhost/02-openid.conf` (bearer-only).
- KHÔNG sửa SQL schema.
- KHÔNG thay đổi `loadtest.io/run-id` label (giữ raw UUID), `nsByRef` key (giữ `spec.ID`).
- UDS `unix:/var/run/axum` giữa nginx và controller giữ nguyên.
- Earthly build image `serve`/`mock-service` (tag `:local`) — không push registry trong plan này (overlay `kind/` dùng `imagePullPolicy: IfNotPresent`).

## Fix log — lần chạy CI in-cluster đầu tiên (2026-11-11)

Job `Full Deploy E2E (in-cluster)` deploy thành công toàn bộ platform nhưng fail ở
`create-with-spec` với `400 Bad Request`. Log `serve` cho thấy **đường Lua auth của
nginx chưa từng được test** (nhánh `DEPLOY_MODE` cũ nói chuyện trực tiếp controller
ở `:8090`, không qua gateway), nên 5 lỗi sau lộ ra cùng lúc:

| # | Lỗi | Nguyên nhân | Sửa |
|---|---|---|---|
| 1 | `400 Bad Request` (trang mặc định openresty, không có upstream) | `H="-H Authorization: Bearer $TOKEN -H Host: localhost"` bị shell word-split → curl nhận `-H Authorization:` (xoá header) + `Host:` (xoá header) ⇒ HTTP/1.1 request **không có `Host`** | `tests/e2e-full-deploy.sh`: truyền header qua curl config file (`-K`), một word duy nhất |
| 2 | `connect() to unix:/var/run/axum failed (111: Connection refused)` | `/var/run/axum` bị tạo thành **thư mục** (`mkdir -p` trong entrypoint + `emptyDir` mount vào đúng path) nên controller không bind được; commit trước đó workaround bằng cách đổi sang `/var/run/axum/controller.sock` | sửa **gốc**: `/var/run/axum` là file socket — bỏ `mkdir -p /var/run/axum` (entrypoint + Dockerfile), bỏ `emptyDir` mount; `ADDR`/`upstream`/Lua giữ `unix:/var/run/axum` (đúng `docs/packaging-deployment.md`, plans/34) |
| 3 | JWKS `http://dex:5556/dex/keys` không resolve trong cluster | `resolver 127.0.0.11` hardcode DNS của Docker | placeholder `%%NGINX_RESOLVER%%` (sed trong `release.sh`, default `127.0.0.11`) + env `NGINX_RESOLVER=kube-dns.kube-system.svc.cluster.local`. Lưu ý: resolver của nginx **không** áp dụng search domain trong `/etc/resolv.conf` ⇒ `dex` trả `SERVFAIL` (`dex could not be resolved (2: Server failure)`), phải dùng FQDN `dex.ltp-system.svc.cluster.local` trong auth profile |
| 4 | `X-Tenant-Id` = id của **auth profile** ⇒ FK `tbl_namespaces.tenant_id` fail | `pkgs/adminapi` trả `ID: profile.ID` (đúng như reference `sys_oidc.id`), nhưng schema repo này scope theo `tbl_tenants.id` và docs nói `id` là tenant context | `ID: profile.TenantID` + assert trong `handlers_test.go` |
| 5 | `X-User-Id` = `CiQxMTEx…` ⇒ `invalid input syntax for type uuid` | Dex v2.41 **luôn** trả `sub = base64url(protobuf{userID, connectorID})` (không tắt được), trong khi `tbl_namespaces.owner_user_id UUID REFERENCES tbl_users(id)` | plan 37: controller resolve `X-User-Id` qua `tbl_user_external_identities` (UUID pass-through); E2E decode `sub` từ token rồi seed mapping; Dex ConfigMap vẫn đặt `userID` = UUID cố định |
| 6 | `Invalid Token … failed fetching jwks: dex could not be resolved (2: Server failure)` | `resolver` của cosocket không dùng search domain/ndots ⇒ tên ngắn `dex` trả SERVFAIL | auth profile dùng FQDN `http://dex.ltp-system.svc.cluster.local:5556/dex` |
| 7 | Service kẹt `cluster_error`, namespace không READY | ClusterRole `serve` thiếu `apps/deployments` ⇒ Env Reconciler `patch Deployment/echo` bị 403 (`deployments.apps … is forbidden`) | thêm rule `apiGroups: ["apps"], resources: ["deployments"]` vào `deploy/k8s/base/serve/rbac.yaml` |

Regression guard: `scripts/k8s-apply.sh` fail sớm nếu `conf/nginx/**` còn trỏ
`unix:/var/run/axum/…` (path con) — dấu hiệu `/var/run/axum` đang bị dùng như thư mục.
Lỗi #2 rất khó thấy: chỉ 500/`Connection refused` trong log pod.

## Definition of done
- `go test ./...` green; `golangci-lint run` 0 issues
- `sh -n tests/e2e-full-deploy.sh scripts/k8s-apply.sh` OK
- `plans/36-k8s-manifests.md`, `docs/k8s-deployment.md` viết xong trước code (AGENTS.MD)
- Branch `feat/36-k8s-manifests` tách từ `feat/implement-services-for-serving-as-loadtest-platform` (đã có plan 34), PR squash-merge sau CI xanh
- Local (tuỳ chọn, sau khi network ổn): `earthly +integration-images` → `make up` → `make test-integration` → `docker-compose down -v`

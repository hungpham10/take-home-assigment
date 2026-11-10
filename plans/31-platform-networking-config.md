# Plan 31 — Platform networking config (env-driven defaults)

## Mục tiêu

Khắc phục ba ghi chú thực hiện (implementation notes):

1. `k6OperatorNamespace` đọc từ `os.Getenv` tại package init (`component_runnerpolicy.go:21`) — không thể test bằng `t.Setenv`, bypass `pkgs/config`, và có thể sai namespace nếu k6-operator được cài ở nơi khác.
2. `defaultDenySpec` hardcode node CIDR `172.18.0.0/16` (kind/colima) — production cluster cần cấu hình hoặc tìm kiếm.
3. `RunNetworkPolicies` không thêm mock-service egress, ngược lại docs `network-policy.md` và comment `render.go` cho rằng run-scoped policy đã xử lý. mock-service chạy ở namespace `mock-system`, nên cross-namespace egress là bắt buộc khi bật mocks.

Quyết định (đã duyệt qua câu hỏi):

- Thêm `PlatformNet` vào `envreconciler.Env`, đọc env vars qua `pkgs/config.Config`, wire qua `orchestration.go` và `server.Deps` (không còn package var).
- Mock-service egress làm **baseline** trong `default-deny` (namespace-wide `namespaceSelector` mock-system + các port), additive; kết hợp run policy.
- Node CIDR: danh sách có thể cấu hình, mặc định `172.18.0.0/16`, kiểm tra `net.ParseCIDR` khi khởi động.

File thay đổi (liệt kê chi tiết ở §Files).

## Thay đổi

### 1. `plans/31-platform-networking-config.md` (này)

### 2. `docs/network-policy.md`
- §2: mock-service allowance là quy tắc egress được render trong `default-deny` (namespace-scoped), không chỉ là lời hứa văn bản.
- §3.2: runner thừa hưởng phép mock additively từ namespace baseline; xóa implication rằng `RunNetworkPolicies` tự render mock-service egress.
- Thêm phần **Configuration**: 4 env var, default, hướng dẫn production, quy tắc kiểm tra.
- Prerequisite: `mock-system` phải cho phép ingress từ `lt-env-*` (triết proprietà deployment, không nằm trong repo này).
- Sửa comment `render.go:257-261` cho khớp thực tế.

### 3. `docs/k8s-orchestration.md`
- Thêm 4 env var mới vào bảng/env contract của controller deployment (RBAC không thay đổi — chỉ NetworkPolicy).

### 4. `docs/mock-service-api.md`
- Pin contract endpoint: Service `mock-service` trong `mock-system`, port 80 → container 8090; client gọi `http://mock-service.mock-system.svc.cluster.local`; NetworkPolicy đánh giá port 8090 post-DNAT.

### 5. `pkgs/config/config.go`
- Thêm fields: `K6OperatorNamespace`, `K8SNodeCIDRs []string`, `MockServiceNamespace`, `MockServicePorts []int32`.
- `FromEnv` đọc defaults: `k6-operator-system`, `172.18.0.0/16`, `mock-system`, `80,8090`; validate `net.ParseCIDR` (mỗi entry) và port 1–65535.

### 6. `pkgs/envreconciler/render.go`
- `type PlatformNet struct` + `Env.Net PlatformNet` + `netConfig()` (áp defaults, zero value = hành vi cũ).
- `defaultDenySpec(env)`: một `ipBlock` mỗi node CIDR; thêm mock-service egress rule (`namespaceSelector` = configured namespace, TCP các port mock).
- Xóa comment cũ tại `render.go:257-261`.

### 7. `pkgs/envreconciler/component_runnerpolicy.go`
- Xóa package-level `k6OperatorNamespace` và `os` import; đọc `env.netConfig().K6OperatorNamespace`.

### 8. `pkgs/envreconciler/db.go`
- `LoadEnvs(db, net PlatformNet)`; gán `env.Net` cho mọi env (bao gồm cả `DELETING`).

### 9. `cmds/loadtest-controller/orchestration.go`
- Xây dựng `PlatformNet` một lần từ `config.Config`, truyền vào `LoadEnvs`, `applyNowFn`.

### 10. `cmds/loadtest-controller/main.go`
- Truyền `PlatformNet` vào `server.Deps.Net` (qua `applyNowFn` đã có sẵn, hoặc trực tiếp).

### 11. `pkgs/server/server.go` + `pkgs/loadtest/handlers.go`
- Thêm `Net envreconciler.PlatformNet` vào `Deps`; handler tạo run-policy `Env{..., Net: d.Net}`.

### 12. `tests/harness/watcher/golden_test.go`
- Cập nhật `LoadEnvs(sharedDB)` → `LoadEnvs(sharedDB, envreconciler.PlatformNet{})`.

### 13. Tests mới (TDD, trước code)
- `pkgs/config/config_test.go`: defaults, overrides, invalid CIDR, invalid port.
- `pkgs/envreconciler/component_policy_test.go`:
  - `TestDefaultDenyDefaultsNodeCIDR`
  - `TestDefaultDenyUsesConfiguredNodeCIDRs`
  - `TestDefaultDenyAllowsMockServiceEgress`
  - `TestRunAPI6565UsesConfiguredK6OperatorNamespace`

## Không làm trong plan này
- Deploy `mock-service` / `mock-system` manifests, RBAC (prerequisite được ghi docs).
- Auto-discovery Node IP (chỉ config, theo quyết định).
- Mock-service egress có điều kiện theo `svc.mock` (baseline thay thế).
- Bất kỳ thay đổi nào dưới `sources/`.

## Acceptance criteria
- `go build ./...` ✅
- `go test ./...` ✅ (bao gồm tests mới `TestPlatform*` + `TestDefaultDeny*`/`TestRunAPI6565UsesConfigured...`)
- `golangci-lint run ./...` ✅
- `sh -n tests/e2e-full-deploy.sh` ✅
- Push `feat/31-platform-networking-config`, mở PR, squash-merge khi CI green.

## Files thay đổi (đã làm)
- `docs/network-policy.md` (§2 mock-service row, §3.2 mock note, phần §8 Configuration mới, đánh lại số §9→§10/§11)
- `docs/k8s-orchestration.md` (các env var platform networking trong bảng deployment)
- `docs/mock-service-api.md` (contract port mock 80→8090)
- `pkgs/config/config.go` (các fields PlatformNet + FromEnv parsing/validation)
- `pkgs/envreconciler/render.go` (kiểu PlatformNet, trường Env.Net, netConfig(), defaultDenySpec có CIDRs + mock-service egress, helpers cidrBlocks/mockServicePorts)
- `pkgs/envreconciler/component_runnerpolicy.go` (bỏ package-init k6OperatorNamespace; đọc env.Net)
- `pkgs/envreconciler/db.go` (LoadEnvs(db, net); gán env.Net)
- `cmds/loadtest-controller/orchestration.go` (hàm platformNet(); nối net vào LoadEnvs/applyNowFn)
- `cmds/loadtest-controller/main.go` (xây net một lần; truyền vào Deps.Net + applyNowFn)
- `pkgs/server/server.go` (trường Net trên Deps; truyền vào loadtest.Deps)
- `pkgs/loadtest/handlers.go` (trường Net trên Deps; truyền vào RunNetworkPolicies Env)
- `tests/harness/watcher/golden_test.go` (LoadEnvs(sharedDB, PlatformNet{}))
- `pkgs/config/config_test.go` (TestPlatformNetworkOverrides, InvalidCIDR, InvalidPort; defaults trong TestFromEnvDefaults)
- `pkgs/envreconciler/component_policy_internal_test.go` (mới: TestDefaultDenyDefaultsNodeCIDR, UsesConfiguredNodeCIDRs, AllowsMockServiceEgress, TestRunAPI6565UsesConfiguredK6OperatorNamespace)
- `plans/31-platform-networking-config.md` (plan này)

# Plan 27 — k6-operator K8s runner + full-lifecycle E2E (NetworkPolicy fixes)

## Mục tiêu
Đưa `POST /loadtests` sang chạy thật trên cluster qua k6-operator `TestRun`, render NetworkPolicy chạy đúng theo spec, và mở rộng `tests/e2e-full-deploy.sh` để kiểm tra vòng đời đầy đủ: namespace READY → script upload/validate → create loadtest → run → kết quả (status/events/unreliable) → cleanup. Cùng sửa các NetworkPolicy bug chỉ hiện ra khi chạy thật trong cluster.

## Tại sao k6-operator (không local k6)
- `Local` runner chạy `k6` binary trên host controller; `e2e-full-deploy.sh` chạy trong container/CI không có k6 (`docs/mvp-status.md` §7.2, `tests/e2e-smoke.sh` chấp nhận 500 khi thiếu k6).
- `TestRun` pod chạy **trong namespace `lt-env-<id>`** → bị `default-deny` (Ingress + Egress) chặn → các NetworkPolicy assertion thực sự có ý nghĩa.
- L3/L4 probe scenarios (runner→echo ✅, runner→DB ❌, other-service→DB ❌, bound-service→DB ✅) chỉ có thể chạy trên cluster.

## Giai đoạn (theo AGENTS.md: docs → plans → code → SQL → TDD → local test → PR)

### 0. Docs + plan (này) — GATE
- `plans/27-k8s-runner-e2e.md` (tạo mới)
- `docs/network-policy.md`: §1/§2 per-service egress + DB ingress từ bound services; §3.2 target selector = **component name**; §3.3 cleanup = xóa khi run đạt terminal status; §5 thêm quy tắc 6565 được render.
- `docs/k8s-orchestration.md` §6 RBAC: bổ sung `testruns.k6.io` + `pods/log` (configmaps đã có).
- `docs/loadtest-manager-api.md`: `RUNNER_MODE=k8s` semantics; kết quả run = status + events + unreliable (metrics pod-log deferred sang benchmark plan).
- `docs/harness-testing.md` §12.4: k6-operator là prerequisite, thêm cases E2E-FD-04…09.
- `docs/mvp-status.md`: runner row → K8s runner done, k6 host dependency bỏ cho cluster mode.

### 1. Code: k6-operator K8s runner (`pkgs/runner/k8s.go`)
- `K8sTestRunRunner`: Provision → ConfigMap `<run>-script` (≤1 MiB, script content) trong env ns; Start → tạo `TestRun` (k6.io/v1alpha1) với `spec.script.configMap`, `spec.arguments`, labels `loadtest.io/run-id`, `app=k6`, `k6_cr=<testrun>`; Status → map `status.stage` (`initialization/initialized/created` → PROVISIONING, `started` → RUNNING, `finished` → COMPLETED, `stopped` → STOPPED, `error` → FAILED); Stop → xóa TestRun (idempotent).
- `newRunner`: chọn `K8sTestRunRunner` khi `RUNNER_MODE=k8s`, require K8s client, fail-closed nếu thiếu.
- TDD: test với `pkgs/k8s.NewFake()` (Apply/Get/Delete/List/Watch đã có) — không cần cluster thật.

### 2. Code: NetworkPolicy fixes (chỉ hiện ra khi chạy thật trong cluster)
- **Port coercion**: `collectTargets` lưu port làm `int32`, `RunNetworkPolicies` type-assert `float64` → port = 0 → rule bỏ qua `ports`. Thêm helper nhận `int`/`int32`/`float64`/`json.Number`.
- **Label mismatch**: run egress chọn `loadtest.io/component-id = service_id` (`svc-<rand>`) nhưng Deployment/Service gắn label bằng `svc.Name` (`echo`). Targets mang thêm `service_name`; run policy chọn theo name.
- **Per-service egress split**: `defaultDenySpec` áp `renderEgress` lên `podSelector: {}` → mọi pod trong ns kết nối mọi dependency. Tách thành một policy per service (chỉ service đó đến dependencies của nó).
- **DB ingress từ bound services only**: `component_databasepolicy.go` hiện `from: {podSelector: {}}` (mọi pod). Chỉ cho phép các service có `database_bindings` đến DB.
- **6565 allowance**: initializer/starter ↔ runner TCP 6565 trong run policy (doc §5).
- **Run-policy cleanup**: xóa 2 run policy + ConfigMap khi run đạt terminal status (doc §3.3 claim chưa implement). Reconciler `finish` + timeout path.
- **Per-target ingress**: mỗi target một ingress policy chọn pod target (component-id/name), port-scoped (theo quyết định user).

### 3. Code: ctx detachment
`POST /loadtests` gọi `Runner.Start(ctx, …)` với `ctx` từ `r.Context()` + `defer cancel()` → ctx bị huỷ ngay sau khi handler trả → k6/TestRun bị chặt ngay. Provision/Start nhận context độc lập (không cancel khi HTTP request kết thúc), cancel duy nhất qua `Stop`.

### 4. Code: E2E script extensions (`tests/e2e-full-deploy.sh`)
Sau READY + NP assertions (đã có), thêm:
- cài k6-operator (`bundle.yaml` pinned tag), chờ CRD established + controller ready; preload `grafana/k6`.
- controller với `RUNNER_MODE=k8s`.
- upload script (`http://echo:80/`) → poll `VALIDATED` → tạo loadtest.
- assert run policies: tên `loadtest-run-{egress,ingress}-<id>`, `component-kind=run-policy`, `run-id`, egress `ports:[80]`, target selector `component-id=echo`, quy tắc 6565.
- poll `COMPLETED` (deadline ~3m), kiểm tra `started_at`/`finished_at`, event sequence, `unreliable` có mặt.
- probes từ busybox pod gắn `loadtest.io/run-id=<id>` (k6 image không có shell): runner→echo ✅, runner→demo-db ❌, other-service→DB ❌, bound-service→DB ✅.
- giữ delete namespace + ns-gone assertions, bổ sung run policies gone.

### 5. CI (`.github/workflows/ci.yml`)
`full-deploy-e2e`: cài k6-operator (bundle.yaml), preload `grafana/k6`, `RUNNER_MODE=k8s`. Timeout 30m giữ (operator ~2 phút).

### 6. Không làm trong plan này (deferred sang benchmark plan A→D)
- `sql/0012` `result_summary`/`thresholds_failed`/`result_collected_at` — K6 metrics từ pod log (k6 OSS không có endpoint metrics mặc định).
- `K8sTestRunRunner` đã có ở plan này; benchmark làm result collection + `tests/e2e-run-k8s.sh`.
- `tests/e2e-run-k8s.sh` riêng.

## Acceptance criteria
- [ ] `go build ./...` ✅
- [ ] `go test ./...` ✅ (bao gồm unit tests mới cho runner + policy)
- [ ] `golangci-lint run` clean
- [ ] E2E full-deploy chạy xanh trên kind + CNPG + k6-operator (LOCAL hoặc CI)
- [ ] 4 probe scenarios đều đúng (✅/❌ như kỳ vọng)
- [ ] CodeGraph review: không trùng code, không thiếu logic edge case

## Test matrix (E2E)
| # | Scenario | Kỳ vọng |
|---|----------|---------|
| 1 | runner → echo (port 80) | ✅ cho phép |
| 2 | runner → demo-db (port 5432) | ❌ từ chối (không phải target) |
| 3 | other-service → demo-db | ❌ từ chối (không bound) |
| 4 | bound-service → demo-db | ✅ cho phép (database_bindings) |
| 5 | undeclared egress (internet/metadata) | ❌ từ chối (default-deny) |

### Root cause found & fix (E2E step 11 stall — loadtest kẹt RUNNING, không đến COMPLETED)

**Root cause:** k6-operator `StartJobs` (`sources/k6-operator/internal/controller/k6_start.go:77-84`) probes `http://<runner-service-ClusterIP>:6565/v1/status` **from its controller pod in `k6-operator-system`** to decide the service is ready before advancing `TestRun.status.stage` `created` → `started`. The `loadtest-run-api-<run>` NetworkPolicy only allowed ingress from runner pods in the same env namespace (`loadtest.io/run-id=<run-id>`). Calico drops the cross-namespace probe → stage stays `created` forever → starter job never created → k6 runs `--paused` indefinitely → RUNNING never reaches COMPLETED.

Phản ánh thêm: fallback pod-readiness trong `Status()` ban đầu match mọi pod có `loadtest.io/run-id` — bao gồm initializer pod (`Completed`) — nên chuyển PROVISIONING→RUNNING quá sớm (started_at gán khi initializer còn chạy), rồi kẹt RUNNING vì runner chưa bao giờ start.

**Fix (áp dụng trong PR #32):**
- `pkgs/envreconciler/component_runnerpolicy.go`: `loadtest-run-api-<run>` thêm ingress rule thứ 2 — `namespaceSelector kubernetes.io/metadata.name=k6-operator-system → TCP 6565` (namespace có thể override qua `K6_OPERATOR_NAMESPACE`). Giữ rule từ runner pods trong cùng run (starter job).
- `pkgs/runner/k8s.go`: pod fallback selector giờ yêu cầu `runner=true` (chỉ tính pod runner đang Running, không tính initializer Completed).
- TDD: `TestRunAPI6565AllowsK6OperatorNamespace` (api NP có 2 rule: runner + k6-operator ns), `TestK8sTestRunRunnerStatusFallsBackToRunnerPod` (runner pod Running → RUNNING), `TestK8sTestRunRunnerStatusIgnoresInitializerOnlyPod` (initializer-only → PROVISIONING).
- `tests/e2e-full-deploy.sh`: khi step 11 fail, dump `kubectl get testrun -o json`, `kubectl get jobs -o wide`, `kubectl -n k6-operator-system logs` để chẩn đoán nhanh lần sau.
- `docs/network-policy.md`: §3.2 #3 (api policy với 2 rule ingress), §5 (cross-namespace 6565 allowance).

## Risks
- k6-operator image pull trong kind: `kind load docker-image` + rollout gate; fail loudly, không silent skip.
- Pod-log metrics (k6 summary) **không** trong plan này → E2E chỉ assert lifecycle (status/events), không assert metrics.

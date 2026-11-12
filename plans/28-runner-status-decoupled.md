# Plan 28 — Decouple platform runner status from k6-operator stage machine

## Mục tiêu

Đưa PR #32 xanh trên CI: `k6-operator K8sTestRunRunner` (`RUNNER_MODE=k8s`) cho phép E2E loadtest chạy đến `COMPLETED` thay vì kẹt `RUNNING` đến `timeout_at`.

Nguyên nhân gốc (đã xác nhận bằng log CI `gh run view --log` và tái hiện local trên cluster kind v1.37):

- **CI (k8s 1.31 + Calico):** `Create(Service "<run>-service-1")` trong `k6-operator launchTest` bị từ chối (dump Services lúc fail không có service đó, `events` không có event) → `stage` không bao giờ được gán `created` → reconcile sau gặp `job with the name <run>-1 exists` và trả lỗi → không bao giờ retry Service → stage vĩnh viễn `initialized`. `kubectl get events` **không** hữu ích vì Event chỉ sinh cho hành động thành công; phải đọc operator log `--tail=400`.
- **Local (k8s 1.37 + kindnet):** `stage` lên `started` (Service tạo OK) nhưng kubelet probe `:6565/v1/status` timeout (default probe `timeoutSeconds:1`, period `10s`, failureThreshold 3 trên VM 2 CPU/2 GB) ⇒ liveness kill k6 container ⇒ Job `-1` `Failed` (BackoffLimitExceeded), starter job treo (curl không `--max-time`). Loadtest vẫn RUNNING đến `timeout_at`.

**Quyết định:** giữ k6-operator (không thay đổi `sources/`, read-only), tách trạng thái platform khỏi máy trạng thái `stage` của operator — `Status()` đọc **Job `-1`** làm nguồn sự thật trước khi map `stage`; đồng thời render `paused: "false"` + `initializer.disabled: "true"` + probe rõ ràng để k6 tự chạy và không bị probe giết.

## Thay đổi

### 1. `docs/network-policy.md`
- Bỏ nghi vấn NetworkPolicy là nguyên nhân stall (đã loại bằng bằng chứng local: kindnet không enforce mà spec y nguyên vẫn chạy `finished`; CI fail là API write `Create(Service)`, NP không tham gia validate/ghi object).
- Thêm ghi chú: k6-operator `TestRun.spec.runner.*Probe` cấu hình được; render probe rõ ràng để tránh default probe `timeoutSeconds:1` kill runner trên cluster tải.
- Thêm sección "Runner lifecycle vs platform status" liệt kê dependency `initialized→created→started` và tại sao platform không được tin ausschließlich vào `stage`.

### 2. `docs/loadtest-manager-api.md`
- Ghi rõ **nguồn sự thật của status**: `Status()` ưu tiên Job `-1` conditions (`Complete`→COMPLETED, `Failed`/`Error`→FAILED) trước khi map `status.stage`; cuối cùng fallback pod `runner=true` Running→RUNNING. `stage` của operator chỉ là thông tin tham khảo, không phải nguồn sự thật cho terminal status.

### 3. `pkgs/runner/k8s.go`
- `Start()` render TestRun thêm:
  - `spec.paused = "false"` — k6 tự chạy, không phụ thuộc starter/Service 6565/handshake.
  - `spec.initializer.disabled = "true"` — initializer đã validate script ở platform, bỏ chặng này (28s vs 45s local).
  - `spec.runner.readinessProbe` + `spec.runner.livenessProbe` rõ ràng: `timeoutSeconds 3-5s`, `initialDelaySeconds`, `periodSeconds`, `failureThreshold` cao — tránh default probe 1s kill runner trên cluster tải.
- `Status()`:
  1. Nếu TestRun thiếu → `STOPPED` + `Missing: true` (giữ).
  2. **MỚI:** Lấy Job `-1` trong cùng namespace; nếu `status.conditions` có `Complete` → `COMPLETED`; `Failed`/`Error` → `FAILED`. Đây là nguồn sự thật về kết quả.
  3. Sau đó mới map `status.stage`: `started`→RUNNING, `finished`→COMPLETED, `stopped`→STOPPED, `error`→FAILED, còn lại (rỗng/initialization/initialized/created) → PROVISIONING.
  4. Fallback cuối: pod `runner=true` phase `Running` → RUNNING (giữ, cho trường hợp stage kẹt nhưng pod đang chạy).
- Ghi chú tài liệu trong comment vì sao không tin stage một mình.

### 4. `tests/e2e-full-deploy.sh`
- **Bỏ** workaround "delete orphaned job+service to unblock" (nó chỉ mở lại guard, không tạo được Service, và che mất lỗi thật).
- Step 11 failure dump bổ sung:
  - `kubectl get events -n $ENVNS --sort-by=.lastTimestamp | tail -20` (giữ để xem pod scheduling).
  - `kubectl -n k6-operator-system logs deploy/k6-operator-controller-manager --tail=400 | grep -E "Failed to launch|Reconciler error|job with the name"` (nơi tìm trigger thật của `Create(Service)` fail).
  - `kubectl describe testrun -n $ENVNS $LT_ID` (stage + conditions).
  - `kubectl get jobs -n $ENVNS -o wide` (Job conditions: Complete/Failed).
  - In `LT_STATUS` trước khi fail.

### 5. `pkgs/envreconciler/reconciler.go` (latent bug, không phải nguyên nhân lần này)
- `prune()` bỏ qua object có label `loadtest.io/run-id` (Job/Service của k6-operator không nằm trong desired set của env reconciler; không được coi là drift và bị xóa).

### 6. TDD (trước code, theo AGENTS.md)
- `TestK8sTestRunRunnerStatusJobCompleteIsCompleted` — stage `initialized` (kẹt) + Job `Complete` → `COMPLETED`.
- `TestK8sTestRunRunnerStatusJobFailedIsFailed` — stage `initialized` + Job `Failed` → `FAILED`.
- `TestK8sTestRunRunnerStartRendersPausedFalse` — render có `paused: "false"`.
- `TestK8sTestRunRunnerStartRendersInitializerDisabled` — render có `initializer.disabled: "true"`.
- `TestK8sTestRunRunnerStartRendersRunnerProbes` — render có `runner.readinessProbe` + `runner.livenessProbe`.
- Giữ test hiện tại: stage mapping, runner-pod fallback, initializer-only pod, missing, stop.

## Acceptance criteria

- [ ] `go build ./...` ✅
- [ ] `go test ./...` ✅ (bao gồm unit tests mới cho runner)
- [ ] `golangci-lint run` clean
- [ ] E2E full-deploy chạy xanh local (kind + CNPG + k6-operator) rồi xanh CI
- [ ] CodeGraph review: không trùng code, không thiếu logic edge case
- [ ] Push branch `feat/28-runner-status-decoupled`, mở PR, squash-merge khi green

## Không làm trong plan này

- Patch k6-operator (`sources/` read-only). Non-idempotent `launchTest` (Job tạo xong, Service fail ⇒ deadlock) là upstream defect; báo issue kèm repro local nếu muốn sau khi CI xanh.
- K6 metrics từ pod log (`result_summary`, `thresholds_failed`) — deferred sang benchmark plan.
- `tests/e2e-run-k8s.sh` riêng.

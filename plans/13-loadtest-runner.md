# Plan 13 — Loadtest API + Runner Interface (K8s / Local)

## Mục tiêu
Lifecycle load test qua Runner interface: dùng K8s `TestRun` khi có cluster, fallback local k6/docker khi không có — chọn bằng config.

## Dependencies
- Plan 06 (schema), Plan 11 (targets + readiness gate), Plan 12 (script VALIDATED).

## Deliverables
- `pkgs/runner`:
  ```go
  type Runner interface {
      Provision(ctx, LoadTest) (runnerRef string, err error)
      Start(ctx, LoadTest) error
      Stop(ctx, runnerRef) error
      Status(ctx, runnerRef) (RunStatus, error)
      StreamEvents(ctx, runnerRef) (<-chan Event, error)
  }
  ```
  - `K8sTestRunRunner`: Go client tạo `k6.io/v1alpha1 TestRun` (sample code `docs/k6-operator-go-client.md`), script staged ConfigMap ≤1MiB hoặc volumeClaim; không kubectl.
  - `LocalRunner`: chạy k6 binary/docker local, map trạng thái sang cùng model.
- Endpoints `/api/loadtest/v1/namespaces/{id}/loadtests`: create (gate: namespace READY + targets[] + script VALIDATED, else `409 SCRIPT_NOT_VALIDATED`/`NAMESPACE_NOT_READY`; quota → 429 qua `tbl_reservation_ledger`), `POST /{id}/stop` (Idempotency-Key, `STOPPING→STOPPED`), logs/events/metrics, rerun.
- Reconciler + reaper: poll Runner status → `fn_loadtest_transition`; timeout → `TIMED_OUT→TIMEOUT_CLEANED`; lỗi cleanup → `CLEANUP_FAILED`; TTL namespace.
- Reservation ledger commit/release theo kết quả run.

## Verification
- TDD với fake Runner; integration với LocalRunner + k6 binary chạy thật vào echo server; transition events đúng thứ tự; stop khi RUNNING → STOPPED.

## Ghi chú
- Runner mode chọn qua config `RUNNER_MODE=k8s|local`; mặc định local cho dev.

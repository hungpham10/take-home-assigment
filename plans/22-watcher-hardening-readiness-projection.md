# Plan 22 — Watcher hardening: readiness projection, Watch→workqueue, WATCH-01..06

## Goal
Plan 21 đã dựng khung hai loop; plan 22 đóng các gap còn lại so với `docs/namespace-status-watcher.md` và `docs/namespace-management-api.md` §3: `GET /readiness` chưa projection-backed, watcher chỉ polling (chưa có event source), `apply_deadline` không được enforce, DEGRADED-with-running-run chưa flag unreliable, harness WATCH-01..06 chưa tồn tại.

## Dependencies
- Plans/21 (pkgs/k8s adapter + Fake watch feed, pkgs/statuswatcher, migration 0011).
- `docs/namespace-status-watcher.md` §3 (event source), §5 (aggregation), §6 (DEGRADED policy), §9 (WATCH-01..06).
- `docs/namespace-management-api.md` §3 (readiness projection contract).

## Decisions locked (với user)
1. **Event source = Watch→workqueue thật:** consumer per GVK qua `k8s.Client.Watch` (selector `managed-by=loadtest-controller`), handler đọc label `namespace-id` → enqueue key vào workqueue dedup/rate-limited; worker gọi `ReconcileStatus` (level-triggered, không tin event payload); polling resync 5m giữ làm safety net.
2. **Readiness projection chỉ khi K8S mode:** legacy mode giữ `readinessGates` (MVP stack không cluster không break).
3. **WATCH-01..06 trên cluster thật:** colima local (`colima start --kubernetes`), GitHub Actions dùng kind action; fake-client CI cases cho phần không cần cluster.

## Deliverables
1. **Readiness projection (K8S mode)** — `pkgs/namespace/lifecycle.go` `readiness`: đọc `tbl_component_status` group theo component_type + `blocking_reasons`; shape payload giữ contract §3; legacy giữ `readinessGates`.
2. **Watch→workqueue** — `pkgs/statuswatcher/queue.go`: workqueue dedup/rate-limited (client-go `util/workqueue`), consumer per GVK với reconnect backoff; `Start` chạy queue workers + resync ticker.
3. **apply_deadline** — enforce trong `aggregate()` (APPLYING/DATA_LOADING quá deadline → DEGRADED reason `apply_deadline_exceeded`); wire đủ 5 knobs trong `cmds/loadtest-controller/orchestration.go`.
4. **DEGRADED-with-running-run** — transition `→DEGRADED` kèm metadata `namespace_degraded_at` (store.Transition p_meta); loadtest events flag `unreliable` khi namespace DEGRADED sau `started_at` của run (run không bị stop — docs §6).
5. **WATCH-01..06** — `tests/harness/watcher/`: fake-client cases cho CI thường (01/02/03/06) + `//go:build harness_k8s` scaffold chống cluster thật (04/05 full flow); GH Actions job mới dùng kind; README hướng dẫn colima.
6. **Docs** — plans/README row; cập nhật `docs/namespace-status-watcher.md` §9 (implemented), `docs/mvp-status.md`, `docs/architecture.md` (event source Watch→workqueue).

## Out of scope
K8s k6 runner, outbox worker, quota gate 429, informer cache sharing across replicas / leader election sharding (single-replica MVP; docs §3 note giữ cho sau).

## Verification
- Unit: projection payload khớp tbl_component_status; Fake watch feed → reconcile chạy; dropped/duplicate events converge; deadline → DEGRADED; degraded flag trong events.
- Harness: `go test -tags harness_k8s ./tests/harness/watcher/...` trên colima; CI kind job xanh.
- `go test -race ./...` + `golangci-lint run` + `scripts/e2e-smoke.sh` regression.

# Plan 26 — Full deploy E2E (database CNPG + echo-server) + bỏ fakeWatcher

## Goal
(1) Full-test deploy namespace với database (CNPG Cluster) + service demo (echo-server) chạy thật end-to-end — trước đây WATCH-07 chỉ bare nginx. (2) Bỏ fakeWatcher (`pkgs/k8s/watcher.go`) — watch path test real với kind; Fake giữ Apply/Get/List/Delete cho unit fault-injection.

## Deliverables
1. `scripts/e2e-full-deploy.sh`: kind/CNPG bootstrap → API create ns + database + echo service → validate/apply → READY → kubectl verify → port-forward curl echo → dump projection.
2. Fake Watch removal: xóa watcher.go, bỏ Watch khỏi Fake; `TestWatchEventTriggersReconcile` → `TestWATCH08WatchEventsTriggerReconcile` (kind); watcher convergence tests chuyển kind harness.
3. CI job `full-deploy-e2e` (kind + CNPG, 30m).
4. Docs: plans/26, README, mvp-status.

## Verification
Local demo xanh (READY + curl echo); CI 6/6 jobs; e2e-smoke regression.

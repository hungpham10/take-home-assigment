# Plan 21 — K8s Reconciler + Status Watcher + Mock Runtime (code)

## Goal
Implement the code that plans 19/20 locked as docs-only (both point here): close the gap between the two-loop design and the current "synchronous/direct" reality — `runApply` writes DATA_LOADING→READY straight into PG, `readinessGates` computes on the fly, no K8s watch exists anywhere, and `cmds/mock-service` is missing (`readinessGates` hard-codes mocks as blocking; `mocksReconcile` is a STUB). Delivered as 5 PRs (A–E), TDD, kind cluster local.

## Dependencies
- Plans 17–20 (service/database/ingress specs; k8s-orchestration.md; namespace-status-watcher.md).
- `pkgs/store/transitions.go` (`store.Transition` → `fn_namespace_transition`) — the only status writer.
- `pkgs/testutil.NewServer` composition root for integration tests.
- `docs/k8s-orchestration.md` §4.1 (8-step apply order), `docs/namespace-status-watcher.md` §2–§8 (projection, evaluators, fencing, hysteresis, STATUS_STALE).

## PR A — `feat/21a-migration-k8s-adapter`
1. Migration `sql/0011_component_status.sql`:
   - `tbl_component_status` per `docs/namespace-status-watcher.md` §2 (namespace_id, component_type CHECK, component_id, required, desired/observed_generation, phase CHECK PENDING|PROGRESSING|READY|FAILED|UNKNOWN, reason, message, last_transition_at, observed_at; UNIQUE(namespace_id, component_type, component_id)).
   - `tbl_namespaces`: `desired_generation BIGINT NOT NULL DEFAULT 0`, `blocking_reasons JSONB`, `status_observed_at TIMESTAMPTZ`.
   - Seed transition delta (missing in `sql/0003:62-76`): `APPLYING→READY`, `APPLYING→DEGRADED`, `DATA_LOADING→INVALID`, `DEGRADED→READY` (keep `DEGRADED→VALIDATING`).
2. `pkgs/k8s` adapter: narrow interface `Apply/Get/Delete/List/Watch` (dynamic client + unstructured, SSA, fieldManager `loadtest-controller`, labels `loadtest.io/{tenant-id,namespace-id,component-id,managed-by}`, annotation `spec_hash`), fake implementation for unit tests + fault-injection hooks (docs/k8s-orchestration.md §5).

## PR B — `feat/21b-env-reconciler`
3. `pkgs/envreconciler`: leader-elected, level-triggered loop reading desired state from PG; SSA apply in the 8-step order (Namespace+quota → NetworkPolicy → CNPG → restore Job → mock bindings → Deployments+Services → Ingress); prune by `managed-by` label; writes **K8s only** — never readiness.
4. Rework `pkgs/namespace/lifecycle.go` `runApply`: drop the direct DATA_LOADING→READY write; apply only bumps `desired_generation` and transitions `→APPLYING`; the watcher finishes the job. Remove the constant `ObservedGeneration: 1` on `NetworkPolicy`.

## PR C — `feat/21c-status-watcher`
5. `pkgs/statuswatcher`: watch/GVR handlers filtered on `managed-by=loadtest-controller`; handlers read `loadtest.io/namespace-id` label → enqueue key `namespace_id` (dedup + rate-limited workqueue); resync 5m + startup relist; worker recomputes **from cache, never event payload**; evaluators per component type per `namespace-status-watcher.md` §4 (generation fencing: at most PROGRESSING unless `observed_generation == desired_generation`); aggregator writes `status/version/ready_for_loadtest/blocking_reasons/status_observed_at` in **one tx via `store.Transition`**. Hysteresis grace=60s/stable=60s; apply_deadline=15m; fail-closed STATUS_STALE (staleness_threshold=15m). Config via `pkgs/config` (`WATCHER_*` env).
6. Readiness projection: `GET /readiness` reads `tbl_component_status` + `tbl_namespaces.blocking_reasons` (no more on-the-fly `readinessGates` for cluster-managed namespaces); `POST /loadtests` gate returns 409 `NAMESPACE_NOT_READY` reason `STATUS_STALE` when `status_observed_at` is stale.

## PR D — `feat/21d-mock-runtime`
7. `cmds/mock-service` + `pkgs/mockruntime`: independent data-plane binary (imports only pkgs/httpx-level helpers — no controller/namespace/authz coupling): host-path routing, variants, latency injection, pool concurrency, `GET /health`, reports `observed_generation` from snapshot binding; no PG writes, no public ingress (MOCK-VER-01).
8. `pkgs/mockcontrol` (controller side): binding ConfigMap `mock-binding-<component_id>` (`schema_version`/`generation`), Service `ExternalName` wiring, inject `MOCK_<COMPONENT>_URL` into SUT env. Remove the STUB `mocksReconcile` (`pkgs/namespace/views.go`) and the hard-coded blocking mocks gate in `readinessGates` — mock becomes a real watcher evaluator.

## PR E — `feat/21e-harness-kind`
9. Harness WATCH-01..06 (`tests/harness/`, `//go:build harness_k8s`, kind) + fake-client integration tests for regular CI: informer→PG sync, generation fencing, hysteresis, STATUS_STALE, DEGRADED-with-running-run, apply deadline.
10. Docs: `docs/mvp-status.md`, `docs/namespace-status-watcher.md` header, `docs/architecture.md` §2.1 (🔜→✅), `docs/database-schema.md`, `plans/README.md` row.

## Out of scope
K8s k6 runner (RUNNER_MODE=k8s), outbox worker, quota gate 429, Helm mode, env TTL reaper — unchanged from mvp-status §7.

## Verification
- `make test` + `golangci-lint run` per PR.
- PR A: migration applies on a fresh DB; transition seed test proves `APPLYING→READY` now legal.
- PR B/C: unit tests with fake client (fault injection: apply fail, watch reconnect); kind integration — create namespace via API, controller applies `lt-env-*` resources, watcher populates `tbl_component_status`, `GET /readiness` projection-backed.
- PR D: mock-service standalone run, `observed_generation` tracks binding version.
- PR E: `go test -tags harness_k8s ./tests/harness/...` green on kind; `tests/e2e-smoke.sh` regression green.

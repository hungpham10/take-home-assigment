# Plan 20 — Namespace Status Watcher design (docs/namespace-status-watcher.md)

## Goal
`GET /readiness` currently has no data source: components have no persisted per-component state ("no state machine" per `docs/database-schema.md`), so the payload `components.services.ready/total` is not projection-backed. This plan delivers the design doc for the **Status Watcher** — the observed→PG loop complementing the Env Reconciler (`docs/k8s-orchestration.md`, desired→K8s). **No code in this plan**; implementation (migration + watcher + harness tests) is plan 21.

## Dependencies
- Plans 17–19 (component spec/readiness, k8s-orchestration conventions: labels, `spec_hash`).
- `sql/0003_namespace_component.sql` — existing `fn_namespace_transition` + seed transitions.
- `docs/harness-testing.md` §2/§4 (DB-TXN-01/02/10, fault-injection hooks).

## Decisions locked
1. **DEGRADED with a RUNNING load test:** the run continues, its result is flagged unreliable (spirit of `mock-service-api.md` §1.1); `POST /loadtests` gate unchanged.
2. **`required` components:** mock/ingress failure **also blocks** READY (not just service+database+network_policy).
3. **Timing defaults (all configurable):** grace=60s, stable=60s, apply_deadline=15m, resync=5m, staleness_threshold=15m (3× resync).

## Deliverables (docs-only)
1. **`docs/namespace-status-watcher.md`**: two-loop separation; data model sketch (`tbl_component_status` projection; `tbl_namespaces` + `desired_generation`/`blocking_reasons`/`status_observed_at`); watcher architecture (shared informers → namespace_id workqueue → recompute from cache → evaluators per GVR → upsert → aggregate → `fn_namespace_transition` in 1 tx); evaluator table + generation fencing; namespace aggregation rules + hysteresis; DEGRADED-with-running-run policy; fail-closed `STATUS_STALE` gate; config table; harness `WATCH-01..06` descriptions.
2. **Transition seed delta (documented, applied in plan 21):** existing seed (`sql/0003:63-75`) is **missing** `APPLYING→READY`, `APPLYING→DEGRADED`, `DATA_LOADING→INVALID`, `DEGRADED→READY`; it already has `DEGRADED→VALIDATING` (re-validate path). Plan 21 migration adds the missing rows; the doc notes both paths.
3. **Link updates** (contract unchanged): `namespace-management-api.md` §3 (readiness is projection-backed), `k8s-orchestration.md` §4/§7 (observed→PG delegated to watcher; shared labels/`spec_hash`), `architecture.md` (doc list + §3.3), `harness-testing.md` (WATCH-01..06 entries, implementation plan 21), `database-schema.md` (projection note).
4. **`plans/README.md`:** plan 20 row (deps 17–19).

## Code (plan 21, out of scope here)
- Migration (0011): `tbl_component_status`, namespace columns, seed transition delta.
- `pkgs/watcher` (or extend `pkgs/loadtest` reconciler infra): informers, evaluators, aggregator; leader election; config.
- Harness WATCH-01..06.

## Verification
- Transition names/seed cross-checked against `sql/0003_namespace_component.sql` (done — see delta above).
- `GET /readiness` payload shape unchanged (only its data source is documented).
- No changes to `pkgs/`, `sql/`, `conf/` — docs-only.

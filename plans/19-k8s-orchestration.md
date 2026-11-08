# Plan 19 — K8s Orchestration design (docs/k8s-orchestration.md)

## Goal
Close the documentation gap: current docs describe **what** (API contracts, metadata, readiness) but not **how** desired state in PostgreSQL becomes real Kubernetes resources. This plan delivers the design doc `docs/k8s-orchestration.md` — **no code in this plan**; the implementation (adapter `pkgs/k8s` + Env Reconciler) follows as plan 20 per the locked design.

## Dependencies
- Plans 17, 18 (service/database/ingress spec + readiness checklist).
- `docs/architecture.md` §3.3 (Env Reconciler sequence), §4 (K8s orchestrator/adapter row).
- `docs/harness-testing.md` §2.3/§4 (fake K8s client, RUN-*, fault-injection hooks).
- `docs/k6-operator-go-client.md` (dynamic client + unstructured pattern, RBAC sketch).

## Decisions locked (ADR input)
1. **Manifest render-and-apply, no Helm for SUT** (ADR-004): the controller renders `deploy {image, replicas, port, env, health}` directly into `Deployment` + `Service`, applied with **Server-Side Apply** and field manager `loadtest-controller`. Helm is a later extension (`deploy.mode: manifest|helm`).
2. **Ingress:** `networking.k8s.io/v1` Ingress, `ingressClassName` from spec (nginx).
3. **Mock wiring per protocol:** Service `ExternalName` for internal protocols (HTTP/gRPC); route via Ingress when the mock must be reached by host/path; the controller injects the DNS/URL into SUT env in both cases.

## Deliverables (docs-only)
1. **`docs/k8s-orchestration.md`** with sections:
   - §1 Scope & model: Environment ⇄ K8s Namespace 1:1 (`lt-env-<namespace_id>`), ownership label convention (`loadtest.io/tenant-id`, `namespace-id`, `component-id`, `managed-by`).
   - §2 ADR-004 manifest vs Helm.
   - §3 Component → K8s resource mapping (service / database CNPG / ingress / mock / NetworkPolicy) + ready criteria per kind.
   - §4 Env Reconciler: 8-step apply order, ownership/prune, `spec_hash` annotation, informer watch keyed by `namespace_id`, leader election, idempotent/at-least-once (matches harness `RUN-*`, `STATE-MEM-01`).
   - §5 Adapter `pkgs/k8s`: narrow interface (`Apply/Get/Delete/List/Watch`) enabling fake client + fault injection.
   - §6 RBAC table: controller ServiceAccount → verbs per resource.
   - §7 Readiness mapping into the `namespace-management-api.md` §3 payload (`blocking_reasons`).
   - §8 Mock wiring detail (two paths per protocol, env injection, snapshot `generation`/`observed_generation`).
   - §9 Open items (Helm mode, Gateway API, drift report, env TTL).
2. **Link updates** (contract unchanged):
   - `docs/architecture.md`: doc list at top + §3.3 note + §4 K8s orchestrator row → link `k8s-orchestration.md`.
   - `docs/namespace-management-api.md`: Related list + §1 + `APPLYING` status → link to render/apply/reconcile details.
3. **`plans/README.md`:** add plan 19 row (deps: 17, 18).

## Code (plan 20, out of scope here)
- `pkgs/k8s` adapter + fake client, env reconciler in the controller, migration 0011 `spec_hash`/`applied_generation` if needed, RBAC manifests, extended `harness_k8s` tests.

## Verification
- Cross-check every claim against existing docs (state machine, labels, readiness payload, mock generation, harness test IDs).
- `make docs` (if target exists) / markdown renders fine; no changes to `pkgs/`, `sql/`, `conf/`.

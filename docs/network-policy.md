# NetworkPolicy — L3/L4 enforcement for SUT and load-test runs

**Source of truth:** this file defines how NetworkPolicy resources are rendered and enforced.  
**Related:** [`docs/k8s-orchestration.md`](k8s-orchestration.md) §3.5/§4.1/§6 (render order, RBAC, HA), [`docs/architecture.md`](architecture.md) §3.7 (runner egress), [`docs/namespace-management-api.md`](namespace-management-api.md) §8/§12 (dependencies, open items), [`docs/service-management-api.md`](service-management-api.md) §7 (dependencies), [`docs/loadtest-manager-api.md`](loadtest-manager-api.md) §4/§7 (run-scoped policy, open items).

---

## 1. Scope and non-goals

NetworkPolicy is a **Kubernetes L3/L4** construct: it matches on pod/namespace selectors, IP blocks, protocol, and port. It does **not** inspect HTTP host, path, method, or TLS SNI.

| Enforced (L3/L4) | Not enforced by NetworkPolicy |
|---|---|
| pod → pod (by label/namespace) | HTTP host / path |
| pod → port | HTTP method |
| namespace → namespace | request headers / query |
| IP block CIDR | FQDN allowlist |

**Consequence:** `targets[].service_id` + `ports` can be enforced; `targets[].operation_id` / `method` / `path` are validated and audited at the API layer but **cannot** be blocked by NetworkPolicy alone. L7 enforcement (Cilium L7 policy, egress proxy, or sidecar) and FQDN allowlisting are deferred to post-MVP.

## 2. Policy sources → rendered policies

| Source (desired state) | Field(s) used | Policy rendered | Direction | Selector / target |
|---|---|---|---|---|
| `services[].dependencies[]` (`tbl_dependencies`) | `service_id`, `ports`, `required` | one egress policy **per service** (its own dependencies only) | **egress** (SUT → dependency) | dependency pod selector + ports |
| `database_bindings[]` | `database_id`, `binding` | service → DB egress + DB ingress | **egress** (service → DB pod) / **ingress** (DB pod ← **bound services only**) | DB pod selector + engine port |
| Platform allowances | DNS (kube-system:53), mock-service, ingress-nginx → SUT | per-namespace baseline | egress / ingress | `kube-system` label, mock-service label, ingress-nginx CIDR |
| **Run-scoped** (`targets[]`, §3) | `service_id`, `ports`, `via` | one egress + one ingress per run | **egress** (runner → target) / **ingress** (target ← runner) | runner label `run-id` / target pod selector + port |

**Ordering:** NetworkPolicy is applied **before** workloads (`docs/k8s-orchestration.md` §4.1, step 2) so there is no window of unenforced traffic.

## 3. Runner policy (load-test execution) — MVP spec

This is the gap the previous review identified: `loadtest-manager-api.md` §4 step 7 and `architecture.md` §3.7 mention a "run-scoped NetworkPolicy" without a spec. The spec below is sufficient to render it.

### 3.1 Traffic path (`via`)

`targets[].via` default is **`service`**; `"ingress"` is opt-in.

| `via` | Runner calls | Egress allowed to | Notes |
|---|---|---|---|
| `service` (default) | `<service_id>.<ns>.svc.cluster.local:<port>` (ClusterIP) | target service pod selector + port | traffic hits ClusterIP, kube-proxy DNATs to pod; `ipBlock` on service IP **does not** match post-DNAT, so the rule must point at the **pod selector** (`docs/k8s-orchestration.md` render note). |
| `ingress` | `<ingress host>/<path>` | ingress-nginx backend (+ target if direct) | weaker isolation: ingress-nginx can reach every ingressed service; target-level enforcement is diluted. Use only when host/path routing is required. |

The platform **injects** the resolved base URL into the runner script via env, e.g. `TARGET_<SERVICE_ID>_URL=http://svc-order.lt-env-<id>.svc.cluster.local:8080`. The script must not receive arbitrary remote URLs; only platform-resolved `service_id` targets.

### 3.2 Two policies per run

For each load-test run (`testrun-<id>` in namespace `lt-env-<namespace_id>`):

1. **Runner egress** — `loadtest-run-egress-<run-id>`:
   - podSelector: runner pods selected by label `loadtest.io/run-id=<run-id>` (also `app=k6`, `k6_cr=<testrun>`, `runner=true` set on `TestRun.spec.runner.metadata.labels`).
   - egress allow: resolved target pod selectors + `targets[].ports`; DNS (`kube-system`:53 UDP/TCP); mock-service endpoint when the run uses mocks.
   - everything else denied (default-deny baseline of the namespace still applies).

2. **Target ingress** — `loadtest-run-ingress-<run-id>`:
   - podSelector: target service pods (label `loadtest.io/component-id=<service_name>`, rendered from service **name**, not `service_id`).
   - ingress allow: from runner pods (`loadtest.io/run-id=<run-id>`) on `targets[].ports`.
   - One ingress rule per target (service + port); required because the namespace `default-deny-all` denies ingress by default; without it the runner cannot reach the SUT even when egress is allowed.

### 3.3 Labeling and lifecycle (prune safety)

Run policies use a label set **distinct** from the Env Reconciler / Watcher managed set so they are never pruned as "drift" and never counted in the Watcher's "full expected NetworkPolicy set":

| Label | Value | Purpose |
|---|---|---|
| `loadtest.io/managed-by` | `loadtest-controller` | ownership (all platform resources) |
| `loadtest.io/component-kind` | `run-policy` | **not** `network-policy` → excluded from Watcher expected-set evaluation |
| `loadtest.io/run-id` | `<run-id>` | selects runner pods and scopes the two run policies |
| `loadtest.io/namespace-id` | `<namespace_id>` | reconcile/enqueue scope |

Lifecycle:
1. Platform renders both policies **before** creating the `TestRun` (so enforcement exists when runner pods start).
2. The run-scoped policies + run-scoped script ConfigMap are deleted when the run reaches a **terminal status** (COMPLETED/FAILED/STOPPED/TIMED_OUT), not only when the TestRun object is deleted — the loadtest reconciler owns this cleanup on its `finish`/timeout paths.
3. On namespace teardown, run policies are garbage-collected with the namespace (1 Environment = 1 K8s Namespace, cascade).

## 4. Database policy detail

| Traffic | Direction | Rule | Port |
|---|---|---|---|
| bound service → CNPG Cluster pod | egress / ingress | `database_bindings[]` service selector ↔ DB pod selector | postgres `5432` |
| CNPG instance ↔ instance (replication) | intra-cluster | CNPG pod selector ↔ CNPG pod selector | operator-dependent (typically 5432) |
| operator → Cluster | ingress | operator SA / pod selector → Cluster pod | operator webhook/metrics ports |
| ECK Elasticsearch | egress / ingress | bound service → ECK pod | `9200` (HTTP) / `9300` (transport) |

`database_bindings[].access` (`readwrite`, `readonly`) is **metadata only**; NetworkPolicy cannot distinguish read vs write at L4. Restore Jobs need a separate, minimal egress to the object store / dataset location and to the DB (`docs/database-management-api.md` §10.5).

## 5. k6-operator control-plane pods

Initializer and starter pods (spawned by the operator around the runner) need to reach the runner API on port **6565** (confirmed in `sources/k6-operator/internal/controller/service_ready.go:14`). Render a small allowance:

- initializer/starter pod selector → runner pod selector (`app=k6`, `k6_cr=<testrun>`), TCP 6565.

These pods are operator-managed, not platform-owned; the policy above is rendered by the controller in the same env namespace so CNI can enforce it.

## 6. SUT outbound traffic

`dependencies[]` contains only service / database / mock — there is no `external` dependency type in MVP. If a SUT calls a third-party API, traffic is denied by default; the required workaround is to **mock** the dependency. An `external` type with an allowlisted FQDN is post-MVP (L7/FQDN not enforceable by plain NetworkPolicy anyway).

## 7. Enforcement matrix (CNI)

| Test | What it verifies | Requirement |
|---|---|---|
| `RUN-NET-02` | runner cannot reach non-target services / internet / K8s API / metadata | CNI must enforce NetworkPolicy |
| `INT-K8S-04` | default-deny + per-service policy; SUT reaches declared deps only | CNI must enforce NetworkPolicy |

kind's default CNI (kindnet) and colima's default CNI **may not** enforce NetworkPolicy. Verify before running these tests; if not enforced, install Calico or Cilium in the kind/colima cluster (documented as a test prerequisite, not a controller change).

## 8. Open items / post-MVP

1. `targets[].via` default locked to `service` (decided; documented here).
2. L7 enforcement (operation_id / path / method) → Cilium L7 policy or egress proxy/sidecar.
3. FQDN allowlist for an `external` dependency type.
4. Restore Job egress scoping (object store + DB only, short-lived scoped credentials).
5. Confirm kind/colima CNI enforcement; add Calico/Cilium install step to `tests/e2e-full-deploy.sh` and `tests/e2e-run-k8s.sh` prerequisites if needed.

## 9. Cross-references updated

- [`docs/k8s-orchestration.md`](k8s-orchestration.md) §3.5 now points here for the runner policy and DB detail; §6 RBAC should add `testruns.k6.io`, `configmaps`, `pods`, `pods/log` for the K8s runner.
- [`docs/architecture.md`](architecture.md) §3.7 runner-egress diagram and Open Item #2 / #6 reference this spec.
- [`docs/namespace-management-api.md`](namespace-management-api.md) §8 and §12 open item #6 reference this spec for the derived NetworkPolicy.
- [`docs/service-management-api.md`](service-management-api.md) §7 references this spec for dependency-derived policy.
- [`docs/loadtest-manager-api.md`](loadtest-manager-api.md) §4 step 7 and §7 reference this spec for the run-scoped NetworkPolicy.

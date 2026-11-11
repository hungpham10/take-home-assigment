# Mock Service — Separate Runtime for Mocking (metadata in Service/Database, execution in Mock Service)

This document defines the **mocking model** for the load-testing platform. The model is split by responsibility:

- **Mock definition metadata is stored in namespace components** — **services** and **databases** can declare `mock`/mocking metadata (desired behavior, latency profiles, cloud/storage semantics as applicable). This metadata is part of namespace CRUD and is persisted with the Environment definition.
- **Mock execution/runtime is handled by a separate Mock Service** — a dedicated service/runtime + operator reconciles mock metadata into runnable behavior (pools, endpoints, routing, latency injection, capacity reservations). The Mock Service owns runtime state, capacity pools, reconciliation, and observability; it does **not** become the source of truth for business mock definitions.

**Primary integration points:** [`docs/namespace-management-api.md`](namespace-management-api.md) (namespace component CRUD/readiness), [`docs/service-management-api.md`](service-management-api.md) (service `openapi` required + service mock metadata), [`docs/database-management-api.md`](database-management-api.md) (database mock metadata), [`docs/loadtest-manager-api.md`](loadtest-manager-api.md), [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md), [`docs/architecture.md`](architecture.md), [`docs/k6-operator-go-client.md`](k6-operator-go-client.md).

## 1. Goals

- Let namespace definitions declare mocking behavior **where the dependency is defined** (service API contract or database/cloud dependency metadata).
- Run mocks at **GA scale** through a **separate, capacity-planned runtime** (shared pools, benchmark-derived units, admission control), not one pod per mock.
- Support realistic behavior, especially **response/operation latency duration** (distributions/percentiles, weights, reproducible seed) and, for database/cloud mocks, **operation semantics/latency profiles** appropriate to the dependency type.
- Operate under finite resources with **ACID, async, idempotent** control-plane writes and **fast, projection-backed GETs**. Background operators continuously reconcile desired mock metadata/runtime state with Kubernetes/runtime reality.

## 1.1 Service boundary (locked design)

Mock execution is a **separate deployable data plane** (`mock-service`), while control/admin APIs and state remain in `loadtest-controller` (see `docs/architecture.md` §2.1).

| Surface | Service | Notes |
|---|---|---|
| Namespace/service/database `mock` metadata CRUD | `loadtest-controller` | Canonical definition stored with namespace components. |
| Pool/admin/status/reconcile APIs (`/api/loadtest/v1/mock-*`, `/namespaces/{id}/mock-status`) | `loadtest-controller` | Projection-backed reads; guarded mutations; no runner/user access. |
| Runtime mock endpoints (OpenAPI operations, DB-profile behavior) | `mock-service` | Cluster-internal only, dependency-derived NetworkPolicy; no public ingress; no user RBAC. Deployed in `MOCK_SERVICE_NAMESPACE` (default `mock-system`); Service `mock-service` exposes port **80** → container port **8090**. Clients call `http://mock-service.<mock-system-ns>.svc.cluster.local` and NetworkPolicy egress is evaluated post-DNAT on port **8090** (see `docs/network-policy.md` §8). |
| Snapshot/binding contract (controller → mock-service) | internal | Versioned desired config: `schema_version`, `namespace_id`, `generation`, resolved bindings/operations/latency/pool class. mock-service reports `observed_generation`, health, overhead metrics. |
| Persistence/authz | split | Controller owns PostgreSQL/authz/ledger. mock-service has **no DB hot-path writes** and no tenant/owner authorization decisions. |

**Failure isolation:** controller down → existing mock snapshots keep serving. mock-service down/degraded → namespace/mock status degrades and new runs are blocked per readiness policy; running load-test results must be treated as unreliable if mock SLO/health fails.

## 2. Responsibility split

| Concern | Owner | Notes |
|---|---|---|
| Mock intent/behavior metadata (what to mock) | **Namespace component definition** (`services[]`, `databases[]`) | Stored via namespace/component CRUD; versioned with component/Environment spec. |
| Contract source for API mocks | **Service `openapi`** (required) + service mock metadata | Mock is a behavior view over the OpenAPI contract; do not redefine API shape outside OpenAPI except mock behavior. |
| Data/cloud dependency mock metadata | **Database component** `mock` metadata | Covers database-like/cloud-like dependency behavior needed by tests (latency, operation/result profiles, connection/IO semantics at metadata level). Exact adapters are Mock Service implementation detail. |
| Capacity pools, reservations, runtime placement | **Mock Service** | Finite-resource admission, pool units, no overbooking. |
| Runtime config materialization, endpoint/DNS, … | **Mock Service** | Materialize namespace mock metadata → runtime config; atomic snapshot swap. |

## 3. Service mock metadata (MVP: HTTP services only)

`mock` is optional metadata on the service definition (canonical storage in namespace service component). It describes **API mock behavior**; execution is delegated to the separate Mock Service (`docs/mock-service-api.md`).

```json
{
  "enabled": true,
  "generation": 12,
  "pool_class": "general",
  "routes": [
    {
      "method": "POST",
      "path": "/payments",
      "variants": [
        {
          "weight": 95,
          "status": 200,
          "body": { "source": "schema_example" },
          "latency": { "distribution": "lognormal", "p50_ms": 40, "p95_ms": 120, "p99_ms": 180 }
        },
        {
          "weight": 5,
          "status": 503,
          "error": "upstream_timeout",
          "latency": { "distribution": "fixed", "ms": 900 }
        }
      ]
    }
  ]
}
```

**MVP fields:**
- `enabled`: enable/disable mock behavior for this component
- `generation`: monotonically increasing version (controller increments on mock change)
- `pool_class`: `general` (MVP) or `perf-isolated` (future)
- `routes[]`: per method+path, one or more weighted variants
- `variants[]`: weighted outcomes with `weight`, `status`, `body` (JSON template), `latency`, optional `error`
- `latency`: `fixed` (ms) or `lognormal` (p50/p95/p99/min/max ms)

**Database mock metadata:** Not in MVP. Database/cloud profiles (`profile: "postgres-compatible"`) are a future extension.

## 4. Snapshot contract (controller → mock-service)

Internal contract, not user-facing API. Versioned `schema_version = "v1"`.

```json
{
  "schema_version": "v1",
  "namespace_id": "ns-1a2b3c4d",
  "generation": 12,
  "routes": [
    {
      "method": "POST",
      "path": "/payments",
      "variants": [
        {
          "weight": 95,
          "status": 200,
          "body": { "authorized": true, "id": "pay_123" },
          "latency": { "distribution": "lognormal", "p50_ms": 40, "p95_ms": 120, "p99_ms": 180, "min_ms": 10, "max_ms": 500 }
        },
        {
          "weight": 5,
          "status": 503,
          "error": "upstream_timeout",
          "latency": { "distribution": "fixed", "ms": 900 }
        }
      ]
    }
  ]
}
```

**Rules:**
- `mock-service` rejects unsupported `schema_version` (reports error/`DEGRADED`)
- Applies snapshots atomically with generation fencing (stale generations rejected)
- Reports `observed_generation` via `GET /health`
- No secrets embedded; refs resolved via controller-approved mechanisms

## 5. Controller-side wiring (`mockcontrol`)

The controller (`loadtest-controller`) reconciles mock metadata into Kubernetes resources for each mock-enabled service component:

| Resource | Name | Purpose |
|---|---|---|
| ConfigMap | `mock-binding-<service_name>` | Carries `snapshot.json` + `pool_class`; mounted by mock-service; watched by status watcher for generation fencing |
| Service | `mock-<service_name>` (ExternalName) | Points to `mock-service.mock-system.svc.cluster.local`; SUT traffic routed via DNS |
| Deployment env | `MOCK_<SERVICE_NAME>_URL` | Injected into SUT container env; value `http://mock-service.mock-system.svc.cluster.local` |

**Render order:** Applied in envreconciler step 5 (after NetworkPolicy, before Deployments) per `docs/k8s-orchestration.md` §4.1.

## 6. Mock Service runtime (`mockruntime` + `cmds/mock-service`)

Separate deployable binary: **no PostgreSQL, no authz, no public ingress**.

**Endpoints:**
- `GET /health` → `{ "status": "healthy", "observed_generation": 12 }`
- `ANY /<path>` → Matches snapshot routes by exact `method` + `path`; picks weighted variant; injects latency; returns status/body/error

**Latency injection (hot path, no heavy math):**
- `fixed`: sleeps `ms` milliseconds
- `lognormal`: triangular interpolation between `p50_ms` and `p95_ms` (mode = p50), clamped to `min_ms`/`max_ms`

**Snapshot loading:**
- `ApplySnapshot(s *Snapshot)`: validates `schema_version == "v1"`, rejects negative/older generations, atomically swaps
- `LoadFile(rt, path)`: reads JSON file (for local dev or ConfigMap mount)

**Configuration:**
- `ADDR` (default `:8090`)
- `MOCK_SNAPSHOT_FILE` (optional initial snapshot for local mode)

## 7. Namespace integration & readiness

**Readiness gate:** Namespace `ready_for_loadtest=true` only when required services/databases/ingress/dependencies/network policy **and required mock components** are ready.

**Status watcher evaluator (`evalMocks`):**
- For each service with `mock.enabled=true`:
  1. Check ConfigMap `mock-binding-<svc>` exists in `lt-env-<ns>`
  2. Verify annotation `spec_hash == "mock-gen-<desired_generation>"`
  3. Phase:
     - `READY` + `ObservedGeneration = desired` if annotation matches
     - `PROGRESSING` reason `mock_generation_pending` if annotation mismatch
     - `PENDING` reason `mock_binding_missing` if ConfigMap absent
     - `UNKNOWN` reason `cluster_error` on API error

**Load tests:** Continue to target namespace service OpenAPI operations (`targets[]`) and include the k6 JS script. Mocks may satisfy dependencies, but **runner egress/NetworkPolicy must still be enforced** at the network layer; `targets[]` is not advisory.

**NetworkPolicy:** Allow SUT/runner access only to declared/approved mock endpoints and dependencies. Mock Service runtime endpoints follow dependency-derived policy (see `docs/network-policy.md` §3.2, §8). SUT→mock via ExternalName Service (port 80 → container 8090 post-DNAT).

## 8. Security and validation

- Tenant isolation always; owner-only by default; **admin may manage across owners within the same tenant** (not cross-tenant), per platform rules.
- Validate mock metadata size/complexity (operation count, body templates, regex/predicates), latency bounds, weights, and OpenAPI operation resolution.
- No raw secrets in mock metadata/responses; use secret refs.
- Metrics/logs redact sensitive payloads; status/diagnostics are authz-scoped.
- Mock Service: cluster-internal only, no ingress, no user RBAC, no PG writes.

## 9. API Endpoints (Control Plane)

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/mock-status` | Aggregated mock readiness for namespace (projection-backed from `tbl_component_status`). |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/mocks/reconcile` | Trigger mock reconciliation (`202`); admin/owner. |
| `GET` | `/api/loadtest/v1/mock-pools` | List capacity pools/classes (projection-backed). **Not yet implemented.** |
| `GET` | `/api/loadtest/v1/mock-pools/{pool_id}` | Pool utilization/reservations/health. **Not yet implemented.** |

**Status codes:** `200/201/202` success/accepted, `400/422` validation, `401/403/404` authz, `409` (`INSUFFICIENT_MOCK_CAPACITY` — future, `CONFLICT`, version/state), `429` quota.

## 10. Open decisions / Future work

1. **Mock pools/ledger/admission**: `INSUFFICIENT_MOCK_CAPACITY` gate, pool capacity units, reservation ledger (ACID/idempotent).
2. **Database mock profiles**: `profile: "postgres-compatible"` with operation/shape-based latency & result templates.
3. **Capacity request in mock def**: `rps_units`, `concurrent_connections` per component; normalized pool units.
4. **Active-run snapshot behavior**: Snapshot-by-generation for running load tests vs block mock-affecting apply.
5. **Stateful/sessionful mocks**: MVP is stateless; session affinity as extension.
6. **Protocol scope**: HTTP/OpenAPI in MVP; database/cloud profiles staged.
7. **Endpoint addressing**: Internal DNS convention (`mock-<svc>.<lt-env-ns>.svc.cluster.local`) and NetworkPolicy templates.

## 11. Implementation status (as of 2026-11-11)

| Phase | Feature | Status |
|---|---|---|
| 1 | Metadata schema (`service.mock` JSONB, `mockruntime` types) | ✅ Complete |
| 2 | Benchmark Mock Service runtime | ❌ Not done |
| 3 | Ledger + admission | ❌ Not done |
| 4 | Runtime + operator (`mock-service`, `mockcontrol`, `mockruntime`) | ✅ Complete |
| 5 | Namespace integration (watcher evaluator, readiness) | ✅ Complete |

**Files:**
- `pkgs/mockruntime/runtime.go` — data plane HTTP router, variant selection, latency injection
- `pkgs/mockcontrol/bindings.go` — controller ConfigMap + ExternalName Service + env injection
- `cmds/mock-service/main.go` — separate binary entrypoint
- `pkgs/statuswatcher/watcher.go` — `evalMocks` generation-fenced evaluator
- `pkgs/envreconciler/reconciler.go` — applies mock bindings in step 5

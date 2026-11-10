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
| Runtime config materialization, endpoint/DNS, latency injection, reconcile | **Mock Service operator/runtime** | Level-triggered, idempotent, generation-aware. |
| Readiness contribution | **Namespace/Environment status** (controller projection) | Namespace cannot be `ready_for_loadtest=true` if required mock components/bindings are not ready under policy. |

## 3. Where mock metadata is defined

### 3.1 Service mock metadata (API dependency / mockable service behavior)

Service definitions already require `openapi` (see `docs/service-management-api.md`). Service mock metadata declares how that service/dependency is mocked when the namespace uses mock mode for it.

```json
{
  "service_id": "svc-payment",
  "name": "payment-service",
  "openapi": { "format": "openapi", "version": "3.0.3", "source": "inline", "content_type": "application/yaml", "content": "..." },
  "mock": {
    "enabled": true,
    "mode": "openapi-behavior",
    "provider": "mock-service",
    "pool_class": "general",
    "defaults": { "seed": 42, "request_validation": "lenient" },
    "operations": [
      {
        "operation_id": "authorizePayment",
        "responses": [
          {
            "name": "authorized",
            "match": { "status": 201 },
            "weight": 95,
            "latency": { "distribution": "lognormal", "p50_ms": 40, "p95_ms": 120, "p99_ms": 180, "min_ms": 5, "max_ms": 500 },
            "body": { "source": "schema_example", "example": "authorized" }
          },
          {
            "name": "upstream-timeout",
            "match": { "status": 503 },
            "weight": 5,
            "latency": { "distribution": "fixed", "ms": 900 },
            "headers": { "Retry-After": "1" },
            "body": { "source": "inline", "content": { "error": "upstream_timeout" } }
          }
        ]
      }
    ],
    "capacity_request": { "rps_units": 500, "concurrent_connections": 200 }
  }
}
```

**Rules:** `mock.operations[*]` must resolve to service OpenAPI operations (`operationId` preferred, or method+path). Unknown operations/invalid latency/weights fail validation (`422`).

### 3.2 Database mock metadata (data/cloud dependency behavior)

Database components may declare mocking metadata for test environments where a real managed database/cloud dependency is replaced or simulated by Mock Service behavior. This is **metadata for mocking the dependency behavior**, not a replacement for real database provisioning contracts.

```json
{
  "database_id": "db-orders",
  "name": "orders-primary",
  "engine": "postgres",
  "operator": "cloudnative-pg",
  "ephemeral": { "enabled": true, "mode": "per-loadtest" },
  "mock": {
    "enabled": true,
    "provider": "mock-service",
    "pool_class": "general",
    "profile": "postgres-compatible",
    "defaults": { "seed": 42 },
    "operations": [
      { "op": "connect", "latency": { "distribution": "lognormal", "p50_ms": 8, "p95_ms": 25, "p99_ms": 60 } },
      { "op": "query_read", "match": { "shape": "select_by_id" }, "latency": { "distribution": "lognormal", "p50_ms": 3, "p95_ms": 12, "p99_ms": 30 }, "result": { "source": "template", "rows": 1 } },
      { "op": "query_write", "latency": { "distribution": "lognormal", "p50_ms": 6, "p95_ms": 20, "p99_ms": 45 }, "result": { "source": "template", "affected_rows": 1 } },
      { "op": "transient_failure", "weight": 1, "latency": { "distribution": "fixed", "ms": 120 }, "error": { "class": "timeout" } }
    ],
    "capacity_request": { "rps_units": 300, "concurrent_connections": 100 }
  }
}
```

**Notes:**
- Database/cloud mock `profile`/`op` vocabulary is implementation-defined but must be **validated and versioned**. MVP should start with a small, explicit operation set (e.g., connect/read/write/error) rather than arbitrary SQL emulation unless explicitly scoped.
- “Cloud mocking” (object store, queue, cache, managed DB behavior, etc.) is modeled as **dependency profiles + operation latency/result metadata** owned by the component definition; runtime adapters live in Mock Service.
- If `mock.enabled=true`, namespace apply/readiness must make it unambiguous whether the dependency uses **real provisioning**, **mock runtime**, or **disabled** — do not allow ambiguous dual-active mode.

## 4. Latency/duration model (service and database mocks)

Latency/duration is first-class for both service API responses and database/cloud operations.

| Field concept | Meaning |
|---|---|
| `distribution` | `fixed`, `uniform`, `normal`, `lognormal`, `step/histogram` (finalize exact set). |
| percentile inputs | `p50_ms`, `p95_ms`, `p99_ms` and/or min/max; runtime derives internal parameters, clamps to bounds. |
| `weight` | Relative selection weight for variants/outcomes (normalize; total > 0). |
| `seed` | Reproducible draws/selection for a config generation/scope. |
| `max_ms` guardrail | Platform-enforced cap; never unbounded waits. |

**Runtime rules:**
- Delays use **non-blocking timers** (no blocked OS threads per request); in-flight/waiting work still counts against pool connection/concurrency capacity.
- **Overhead baseline:** Mock Service measures processing overhead at `latency=0` (per profile/operation class) and exposes metrics. Load-test analysis must account for mock overhead; use **performance-isolated pools** for strict latency assertions.
- In-flight requests complete under the old config snapshot; new requests use the new snapshot after atomic swap.

## 5. Separate Mock Service runtime (execution layer)

Mock Service is a dedicated runtime/control plane component:
- **Stateless/shared runtime pods** in capacity pools (not 1 pod per mock definition/binding).
- **Mock operator** reconciles namespace component mock metadata (desired) to runtime config/endpoints/reservations (observed).
- **Admission/ledger** enforces finite capacity with ACID reservations.
- **Config delivery:** versioned snapshots, `generation`/`observed_generation`, atomic swap.
- **Exposure:** cluster-internal DNS/service endpoints preferred; access allowed only by NetworkPolicy and namespace dependency policy. Ingress/public exposure is opt-in and policy-controlled.

### 5.1 Capacity pools
- **Pool units (benchmark-derived):** `rps_units`, `concurrent_connections`, memory/CPU profile, safety factor (e.g., 60–70% of measured max), `benchmark_version`.
- **Classes:** `general` (functional deps) and `perf-isolated` (latency-sensitive performance tests). Binding/component can request pool class.
- **Admission:** insufficient capacity => `409 INSUFFICIENT_MOCK_CAPACITY` (MVP) or explicit queued admission if later designed with TTL/fairness.

## 6. ACID, async, idempotency, reconcile

Mutating control-plane operations are **consistent and async**:
1. Validate authz/schema/refs/latency/operation compatibility.
2. In one Postgres transaction: persist desired metadata/materialized intent reference, create/update **reservation** (with pool row lock / `SELECT ... FOR UPDATE` strategy), append outbox/state intent, store **idempotency** record.
3. Return `202`/status handle where apply is async.
4. **Level-triggered operator** converges runtime to desired generation; retries are idempotent.
5. **Reservation release** happens only after runtime/K8s resources/config/routes are confirmed removed. A reaper reconciles ledger vs runtime/K8s to detect leaks.

**Optimistic concurrency:** component/namespace updates use `version`/ETag/`If-Match` where applicable. Active runs: mock-affecting changes create new generation and follow namespace/run policy (block, snapshot-by-generation, or controlled rollout) — do not mutate behavior of an in-flight measured run implicitly.

## 7. Control-plane API (controller) and runtime boundary

Namespace/component CRUD remains the primary place to **define mock metadata**. The controller exposes control-plane/visibility APIs; `mock-service` runtime endpoints are cluster-internal data-plane endpoints (not listed as public `/api/loadtest/v1` routes):

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/mock-runtime/status` | Aggregate runtime/operator health (projection-backed). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/mock-status` | Materialized status of mock components/bindings derived from namespace service/database mock metadata (`generation`, `observed_generation`, endpoints, conditions, pool). |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/mocks/reconcile` | Request/trigger reconcile for namespace mock metadata (`202`, idempotent). Usually also driven by namespace apply. |
| `GET` | `/api/loadtest/v1/mock-pools` | List capacity pools/classes (projection-backed). |
| `GET` | `/api/loadtest/v1/mock-pools/{pool_id}` | Pool utilization/reservations/health. |
| `GET/PUT` | `/api/loadtest/v1/mock-pools/{pool_id}` (admin-guarded mutation; read may be ops/admin-scoped) | Manage pool profiles/capacity metadata (guarded; benchmark-versioned). Finalize path/role split in implementation. |

*Optional (if needed later):* internal read models for compiled mock bindings; avoid making standalone `/mocks` definitions the canonical source if metadata is already owned by namespace components. Snapshot/binding contract (`schema_version`, generation, compatibility/rollback) must be finalized before implementation.

**Status codes (key):** `200/201/202` success/accepted, `400/422` validation, `401/403/404` authz, `409` (`INSUFFICIENT_MOCK_CAPACITY`, `CONFLICT`, version/state), `429` quota.

## 7.1 Snapshot contract (draft)

Controller → `mock-service` (internal contract, not user-facing API):

```json
{
  "schema_version": "1",
  "namespace_id": "ns-1a2b3c4d",
  "k8s_namespace": "lt-env-ns-1a2b3c4d",
  "generation": 12,
  "issued_at": "2026-11-07T00:00:00Z",
  "pool_class": "general",
  "bindings": [
    {
      "binding_id": "mb-svc-payment",
      "kind": "service-openapi",
      "service_id": "svc-payment",
      "base_path": "/",
      "operations": [
        { "operation_id": "authorizePayment", "responses": [ { "name":"authorized", "weight":95, "latency":{"distribution":"lognormal","p50_ms":40,"p95_ms":120,"p99_ms":180}, "body":{"source":"schema_example"} } ] }
      ]
    }
  ]
}
```

**Rules:** `mock-service` rejects unsupported `schema_version` (reports error/`DEGRADED`), applies snapshots atomically, and reports `observed_generation`/health. Do not embed secrets; resolve refs via controller-approved mechanisms.

## 8. Integration with namespace readiness, load tests, and NetworkPolicy

- **Readiness gate:** namespace `ready_for_loadtest=true` only when required services/databases/ingress/dependencies/network policy **and required mock components** are ready. Mock metadata invalid or runtime not reconciled => `INVALID/DEGRADED`, and load-test create returns `409 NAMESPACE_NOT_READY`.
- **Load tests:** continue to target namespace service OpenAPI operations (`targets[]`) and include the k6 JS script. Mocks may satisfy dependencies, but **runner egress/NetworkPolicy must still be enforced** at the network layer; `targets[]` is not advisory. Runner gets no platform/K8s credentials.
- **NetworkPolicy:** allow SUT/runner access only to declared/approved mock endpoints and dependencies; Mock Service runtime endpoints follow dependency-derived policy. Database mock connectivity must fit the same namespace network model.

## 9. Security and validation

- Tenant isolation always; owner-only by default; **admin may manage across owners within the same tenant** (not cross-tenant), per platform rules. Never trust client-supplied owner/tenant IDs.
- Validate mock metadata size/complexity (operation count, body templates, regex/predicates), latency bounds, weights, and OpenAPI operation resolution.
- No raw secrets in mock metadata/responses; use secret refs.
- Metrics/logs redact sensitive payloads; status/diagnostics are authz-scoped.

## 10. Implementation phases (recommended)

1. **Finalize metadata schema:** `service.mock` and `database.mock` MVP fields, allowed latency distributions, operation/profile vocabularies, and 2–3 real examples.
2. **Benchmark Mock Service runtime:** `rps_per_core`, `conns_per_gb`, latency=0 overhead by profile; version benchmark data.
3. **Ledger + admission:** ACID reservation/idempotency/outbox and pool admission.
4. **Runtime + operator:** materialize namespace mock metadata -> runtime config, endpoint/DNS, atomic snapshot swap, reconcile, metrics.
5. **Namespace integration:** validation/apply/readiness hooks, NetworkPolicy exposure, runner egress alignment, load-test compatibility.

## 11. Open decisions

1. **Canonical metadata shape:** exact `service.mock` / `database.mock` schema and MVP operation/profile vocabulary (especially database/cloud adapters).
2. **Real vs mock mode:** per component, can a dependency be `real`, `mock`, or `hybrid`? How is ambiguity prevented at apply time?
3. **Binding materialization:** are runtime bindings persisted as explicit rows derived from namespace components, or computed/projection-only?
4. **Protocol scope:** HTTP/OpenAPI service mocks in MVP; database/cloud profiles supported in MVP vs staged.
5. **Stateful behavior:** stateless MVP; stateful/sessionful mock scenarios as extension.
6. **Pool selection/capacity request:** who sets `capacity_request` (user spec vs platform default vs admission estimate), and how are units normalized?
7. **Active-run behavior:** snapshot-by-generation for running load tests vs block mock-affecting apply.
8. **Endpoint addressing:** internal DNS/service naming convention and NetworkPolicy templates for mock endpoints.

# Namespace Management API (`/api/loadtest/v1/namespaces`)

This document defines the **namespace-first model** and the **detailed API contract** for namespace-centric environment management. A namespace is the top-level environment definition: **define a namespace (everything) → CRUD components → apply/validate → READY → define/load load tests**.

**Base path:** `/api/loadtest/v1`  
**Terminology (MVP locked):** API path uses `namespaces` and `namespace_id`, representing a platform **Environment**. **Mapping is 1:1:** one Environment = one Kubernetes Namespace (deterministic name/labels; components reconciled in that K8s Namespace). The name “namespace” in the API is a platform term; isolation/enforcement is implemented via the mapped Kubernetes Namespace and policies.
**Stack (MVP):** Go + go-chi, Swagger/OpenAPI, nginx edge auth (`conf/nginx`), Kubernetes orchestration via Go K8s API (no `kubectl`) where applicable.  
**Related:** [`docs/architecture.md`](architecture.md), [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md), [`docs/loadtest-manager-api.md`](loadtest-manager-api.md), [`docs/service-management-api.md`](service-management-api.md), [`docs/database-management-api.md`](database-management-api.md), [`docs/mock-service-api.md`](mock-service-api.md) (metadata in services/databases; separate Mock Service runtime with latency/capacity pools/ACID async reconcile), [`docs/admin-oidc-integration-api.md`](admin-oidc-integration-api.md), [`docs/k8s-orchestration.md`](k8s-orchestration.md) (how the desired spec is rendered/applied to K8s resources and reconciled).

## 1. Model (core idea)

- There is an API to **define an entire namespace** (desired environment state).
- A namespace contains manageable components: **services, databases, ingress** (and derived dependency/network policy).
- Each component supports **CRUD** within the namespace.
- **Each service definition includes its own OpenAPI/Swagger spec** (the contract of APIs that service exposes). This is required and used to target specific APIs in load tests.
- The namespace/environment must be **validated, applied, and become `READY`** before load tests can be defined. How `APPLYING` maps desired state to real K8s resources (render/SSA, component→resource mapping, reconciler order, ownership/prune) is defined in [`docs/k8s-orchestration.md`](k8s-orchestration.md).
- A **load test** is defined only against a **ready namespace**, must specify **which API(s)** it targets, and must include the **JS file (k6 script)** that defines the load test (see `docs/loadtest-manager-api.md`).

### Resource hierarchy

```text
Namespace (environment definition)
├─ services[]                # CRUD: /namespaces/{namespace_id}/services
│  ├─ deploy/runtime config
│  ├─ openapi (Swagger/OpenAPI)   # REQUIRED per service
│  ├─ database_bindings[]         # refs namespace database_id
│  ├─ dependencies[]              # refs namespace service_id -> drives NetworkPolicy
│  └─ readiness/status
├─ databases[]               # CRUD: /namespaces/{namespace_id}/databases
│  ├─ database_id, engine/operator, config, secret refs
│  └─ readiness/status
├─ ingress[]                 # CRUD: /namespaces/{namespace_id}/ingress
│  ├─ host/path/tls rules -> service_id/port
│  └─ readiness/status
├─ dependency graph (derived/validated)
└─ network policies (derived from dependencies) + readiness/status
```

**Hard gates/rules:**
1. **Service OpenAPI required:** validated/parsed; invalid/missing required spec blocks readiness and returns `422`.
2. **Readiness gate:** **only `ready_for_loadtest=true` Environments accept load-test definitions/runs** (else **`409 Conflict`**, `NAMESPACE_NOT_READY`, with readiness details).
3. **Dependencies → NetworkPolicy:** dependency declarations are validated and used to configure **NetworkPolicy per service** in the Environment’s mapped K8s Namespace (1:1 MVP): least privilege (declared dependency traffic + required platform allowances + load-test runner access as needed). Runner egress for load-test pods must be network-enforced from resolved `targets[]`/approved deps (not advisory).
4. **Load test required inputs:** `namespace_id` (ready) + explicit `targets[]` (APIs resolved from namespace service OpenAPI) + **JS load-test script**.

## 2. Authz, tenancy, conventions

- **POC auth (locked):** use **OIDC/nginx auth** (`conf/nginx`). nginx authenticates and reflects identity/permissions from OIDC/claims/introspection into trusted headers (`X-Tenant-Id`, `X-User-Id`, role/admin signals as available). **Go BFF still uses go-chi middleware** to read trusted headers, build canonical principal (`tenant_id`, `user_id`/principal, `is_admin`, roles), enforce RBAC/tenant isolation, and fail closed. **`abt_`** is a platform/service-account base token (CLI/automation). Target/prod keeps the same edge+middleware contract.
- **Tenant isolation always.** Owner-only by default; **admin** may manage across owners **within the same tenant** (visibility policy finalized in implementation).
- JSON + RFC3339 UTC, pagination (`page`,`limit`,`total`,`data`), consistent error envelope.
- Async operations may return `202` + `status_url`. Use idempotency for mutating `POST`s where specified.
- Never accept client-supplied `tenant_id`/`owner_user_id` for authorization. Secrets use references; never return raw secrets.

## 3. Namespace lifecycle & readiness

| Status | Description |
|---|---|
| `DRAFT` | Stored, not applied. |
| `VALIDATING` | Validation in progress (schema/OpenAPI/bindings/deps/dataset refs). |
| `INVALID` | Validation failed (see errors). |
| `APPLYING` | Reconciling/applying desired state (K8s resources, NetworkPolicy, mock runtime as applicable). Render/apply order and readiness criteria per component: [`docs/k8s-orchestration.md`](k8s-orchestration.md) §3–§4, §7. |
| `DATA_LOADING` | Required database/data restore or seed import is in progress (e.g., dump-based `pg_restore` Job, verified seed load). Namespace is not yet ready for load tests. |
| `READY` | Applied and required checks passed, including data-load/verify when configured. |
| `DEGRADED` | Was ready; component(s) unhealthy/unready or data integrity/check drift detected. |
| `DELETING` | Teardown in progress. |
| `DELETED` | Archived/removed. |

**Readiness payload (conceptual):**
```json
{
  "namespace_id": "ns-1a2b3c4d",
  "status": "READY",
  "ready_for_loadtest": true,
  "checked_at": "2026-11-06T07:05:00Z",
  "components": {
    "services": { "ready": 2, "total": 2, "blocking": [] },
    "databases": { "ready": 1, "total": 1, "blocking": [] },
    "data_load": { "required": true, "ready": 1, "total": 1, "blocking": [] },
    "ingress": { "ready": 1, "total": 1, "blocking": [] },
    "dependencies": { "resolved": true, "blocking": [] },
    "network_policy": { "applied": true, "blocking": [] },
    "mocks": { "required": false, "ready": 0, "total": 0, "blocking": [] }
  },
  "blocking_reasons": [],
  "ready": true
}
```

**Data loading / restore gate:** if a database component references a dataset or restore/seed source, the namespace remains not `ready_for_loadtest` while restore/import/verify is incomplete (`DATA_LOADING` or blocking reasons). Verify steps (MVP policy, configurable): checksum match, restore/import tool success, and basic schema/version compatibility check (e.g., `pg_restore --list` sanity and/or `schema_version` marker where applicable). Restore Jobs must set `activeDeadlineSeconds` (from `restore_policy.timeout` / environment limits) to prevent hung cluster resources. Runner/data egress to object storage is never granted by default; only controlled restore/staging jobs may access approved dataset refs with scoped short-lived credentials.

## 4. Namespace endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces` | Create namespace; optional full `spec` + `apply`. Returns `201` when stored without async apply, or `202` + `status_url` when apply/reconcile is started. `Idempotency-Key` supported. |
| `PUT` | `/api/loadtest/v1/namespaces/{namespace_id}` | Replace full desired definition (declarative). |
| `GET` | `/api/loadtest/v1/namespaces` | List namespaces. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}` | Get detail. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/readiness` | Readiness gate details. **Projection-backed:** served from `tbl_component_status` + `tbl_namespaces.blocking_reasons`, maintained by the Status Watcher (observed K8s state → PG): [`docs/namespace-status-watcher.md`](namespace-status-watcher.md). |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/validate` | Dry-run validate. |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/apply` | Async apply/reconcile (`202`). `Idempotency-Key`/version conflict policy required. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}` | Delete/archive (guard active refs/runs). |

**Create request (example):**
```json
{
  "name": "checkout-ephemeral",
  "slug": "checkout-ephemeral",
  "description": "Checkout flow environment",
  "visibility": "tenant",
  "apply": true,
  "spec": {
    "services": [],
    "databases": [],
    "ingress": [],
    "dependency_policy": { "mode": "dependency-derived", "defaults": { "deny_by_default": true } }
  }
}
```

## 5. Services CRUD (OpenAPI/Swagger required)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | Add service. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | List services. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Get service. |
| `PUT` / `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Update service. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Remove service (guard refs). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/openapi` | Get OpenAPI/Swagger spec. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/apis` | List parsed API operations (for load-test targets). |

**Service request (example):**
```json
{
  "service_id": "svc-order",
  "name": "order-service",
  "deploy": {
    "image": "registry.example/orders:1.2.3",
    "replicas": 1,
    "port": 8080,
    "health": { "path": "/health", "port": 8080 }
  },
  "openapi": {
    "format": "openapi",
    "version": "3.0.3",
    "source": "inline",
    "content_type": "application/yaml",
    "content": "openapi: 3.0.3\ninfo:\n  title: Order API\n  version: 1.0.0\npaths:\n  /orders:\n    post:\n      operationId: createOrder\n      responses:\n        '201': { description: created }\n"
  },
  "database_bindings": [
    { "database_id": "db-orders", "binding": "ORDERS_DB", "access": "readwrite" }
  ],
  "dependencies": [
    { "service_id": "svc-payment", "type": "http", "ports": [8080], "required": true, "purpose": "authorize payment" }
  ],
  "network_policy": { "mode": "dependency-derived" }
}
```

**OpenAPI rules:** `openapi` is required. Spec is validated (syntax/semantics as supported); parsing failures -> `422`. Parsed operations power `/apis` and load-test `targets[]` validation. Supported input forms (inline JSON/YAML, multipart upload, vetted ref) are finalized with guardrails.

## 6. Databases CRUD

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | Add database. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | List databases. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Get database. |
| `PUT` / `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Update database. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Remove database (guard bindings). |

**Database request (example):**
```json
{
  "database_id": "db-orders",
  "name": "orders-primary",
  "engine": "postgres",
  "operator": "cloudnative-pg",
  "version": "16",
  "config": { "storage_gb": 2, "node_count": 1 },
  "data_source": {
    "dataset_id": "ds-orders-seed-v3",
    "mode": "restore",
    "load_policy": "reuse"
  },
  "restore_policy": { "timeout": "30m", "parallel_jobs": 4 },
  "ephemeral": { "enabled": true, "mode": "per-loadtest" },
  "secret_ref": "secret://tenant/orders/db",
  "labels": { "domain": "orders" }
}
```

## 7. Ingress CRUD

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | Add ingress. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | List ingress. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Get ingress. |
| `PUT` / `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Update ingress. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Remove ingress. |

**Ingress request (example):**
```json
{
  "ingress_id": "ing-order",
  "name": "order-ingress",
  "class": "nginx",
  "hosts": [
    {
      "host_template": "order.{{namespace}}.loadtest.internal",
      "tls": { "enabled": false, "secret_ref": null },
      "rules": [
        { "path": "/", "path_type": "Prefix", "service_id": "svc-order", "port": 8080 }
      ]
    }
  ]
}
```
Ingress rules must target valid `service_id`/ports in the namespace.

## 8. Dependencies and NetworkPolicy

- Dependencies are declared on services and validated (known targets, allowed graph/policy constraints).
- **NetworkPolicy is derived from dependencies** for each service (least privilege). Allow only declared dependency traffic + required platform allowances (DNS/metrics/control as needed) + load-test runner access required for targeted runs.
- Dependent services must be linkable/communicable under the selected namespace model.
- Runner policy: for load-test runs, render an additional egress policy (runner → resolved target) and ingress policy (target ← runner) selected by label `loadtest.io/run-id`. NetworkPolicy is L3/L4 only (`service` + `port`); `operation_id`/`path` are validated and audited here but not enforced by NetworkPolicy. Full spec: [`docs/network-policy.md`](network-policy.md).

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/dependencies` | Resolved dependency graph. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/network-policies` | Rendered/applied NetworkPolicy status/refs. |

## 9. Load-test gate (integration)

Load-test create (`POST /api/loadtest/v1/namespaces/{namespace_id}/loadtests`, `docs/loadtest-manager-api.md`) **MUST**:
- reference `namespace_id` (path segment) that is `ready_for_loadtest=true` (else **`409 Conflict`**, `NAMESPACE_NOT_READY`);
- omit `targets[]` from the request body — targets are **derived server-side** from each service's parsed OpenAPI operations;
- include required **`script_id`** (validated, status `VALIDATED`) referencing a k6 script definition (see `docs/loadtest-manager-api.md`).

**Conceptual example:**
```json
POST /api/loadtest/v1/namespaces/ns-1a2b3c4d/loadtests
{
  "name": "checkout-api-stress",
  "script_id": "scr-7f2e9a1b",
  "load_config": { "vusers": 200, "duration": "10m", "timeout_minutes": 15 }
}
```

## 10. Validation and errors

**Idempotency (MVP minimum):** `POST /namespaces`, `POST /namespaces/{id}/apply`, `POST /loadtests`, `POST /loadtests/{id}/stop` (and other async mutations as added). Use `Idempotency-Key` scoped to tenant+principal+route.
**Concurrency:** `PUT`/apply should use optimistic concurrency (`version`/ETag/`If-Match`) to prevent lost updates; behavior while active load tests run must be defined (block, version, or controlled apply).
**Limits (define concrete values before implementation):** OpenAPI max size, script max size (ConfigMap 1 MiB for `configMap` staging; larger via approved ref + `volumeClaim` staging), validation Job timeout/resource limits, max vusers/duration/TTL, max components per Environment, namespace/environment TTL for ephemeral definitions.


| Condition | Typical status |
|---|---|
| Unauthorized/forbidden | `401` / `403` |
| Not found / not visible | `404` |
| Invalid schema/OpenAPI/binding/dependency/ingress/target/script | `422` |
| Conflict, namespace not ready for load test, invalid state transition, concurrent apply/version conflict | `409` |
| Quota/guardrail exceeded | `429` |

Validation should return actionable `details` (component, path, reason).

## 11. Mock and data-source integration
- **Mocking:** metadata lives in service/database components (`service.mock`, `database.mock`); execution/runtime is a separate Mock Service (`docs/mock-service-api.md`). Real vs mock mode must be unambiguous.
- **Dataset restore/import:** database components may reference managed datasets (`data_source.dataset_id`, `docs/database-management-api.md`). Restore/import is controlled by isolated Jobs (not runner pods), contributes to `DATA_LOADING`/readiness, and uses approved non-prod dataset refs only.

## 12. Decisions / remaining open items

1. **Locked (MVP):** 1 platform Environment = 1 Kubernetes Namespace (1:1). Per-run ephemeral K8s Namespace is not required for MVP unless later isolation needs justify it; runner egress/NetworkPolicy still enforced per run.
2. OpenAPI forms (inline JSON/YAML, upload, vetted ref) and validation limits; operation identifier format for `targets[]`.
3. JS script forms (inline/upload/approved ref such as S3/object ref), storage/staging (`configMap`/`volumeClaim`), async validation via **K8s Job + watcher/informer**, and guardrails.
4. Dataset restore MVP scope (dump-file default), schema/version compatibility checks, object-store credential model (IRSA/scoped short-lived/presigned), retention/owner handoff, and optional PII pattern scan on import.
5. Readiness criteria for `ready_for_loadtest` and degraded behavior (including `DATA_LOADING` completion/verify).
6. Dependency/NetworkPolicy baseline merge with cluster DNS/metrics/system rules; cross-K8s-namespace policy if any. Runner egress must be enforced to resolved targets (not advisory). Restore/staging job egress is separate and minimal.
7. Compatibility strategy for legacy/global service/resource endpoints.

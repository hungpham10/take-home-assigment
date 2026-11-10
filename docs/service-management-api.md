# Service Management API — Namespace-Scoped Service Components

This document defines **service components inside a namespace**. The platform model is namespace-first: services are not independent/global records; they are **components of a namespace definition** and are managed via Environment-scoped (API path: `/namespaces/{namespace_id}`) CRUD.

**Primary contracts:** [`docs/namespace-management-api.md`](namespace-management-api.md), [`docs/architecture.md`](architecture.md), [`docs/loadtest-manager-api.md`](loadtest-manager-api.md), [`docs/mock-service-api.md`](mock-service-api.md).

## 1. Purpose

A service represents an application/API under test within a namespace. Service management provides:
- service identity and deploy/runtime metadata;
- **required OpenAPI/Swagger contract** for the service’s APIs;
- optional **service mock metadata** (mocking behavior for API operations, including latency/variants), persisted with the service definition and executed/reconciled by the separate Mock Service;
- database bindings;
- dependency declarations (used for linking and NetworkPolicy);
- status/readiness contribution to the namespace `ready_for_loadtest` gate.

## 2. Scope and base path

**Base path:** `/api/loadtest/v1/namespaces/{namespace_id}/services`

Services are **tenant-scoped** through namespace ownership and namespace authz. Never accept client-supplied tenant/owner IDs for authorization.

## 3. Required rule: OpenAPI/Swagger per service

**MVP policy (clear default): every namespace service definition MUST include its own OpenAPI/Swagger spec** when created/updated. Rationale: the platform uses OpenAPI for validation, operation indexing, load-test `targets[]` resolution, and service mock operation mapping. If a future non-API workload type is introduced, define an explicit exception; do not leave this ambiguous.

The spec is used to:
- validate the service API contract,
- index operations for load-test targeting,
- resolve `service.mock.operations` when mock metadata is present,
- expose `GET .../openapi` and `GET .../apis`.

Invalid or missing required OpenAPI fails validation (`422`) and blocks namespace readiness.

## 4. API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | Create/add service component. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | List services in namespace. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Get service detail (may include mock metadata summary/status as policy allows). |
| `PUT` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Replace service definition. |
| `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Partially update service definition. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Remove service (guard references). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/openapi` | Retrieve service OpenAPI/Swagger. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/apis` | List parsed operations for target selection. |

## 5. Service definition (conceptual)

```json
{
  "service_id": "svc-order",
  "name": "order-service",
  "description": "Checkout/order API",
  "deploy": {
    "image": "registry.example/orders:1.2.3",
    "replicas": 1,
    "port": 8080,
    "health": { "path": "/health", "port": 8080 },
    "env": [{ "name": "LOG_LEVEL", "value": "info" }]
  },
  "config_map": { "LOG_FORMAT": "json" },
  "service_account": true,
  "hpa": { "min_replicas": 1, "max_replicas": 5, "cpu_target_percent": 70 },
  "openapi": {
    "format": "openapi",
    "version": "3.0.3",
    "source": "inline",
    "content_type": "application/yaml",
    "content": "openapi: 3.0.3\ninfo:\n  title: Order API\n  version: 1.0.0\npaths:\n  /orders:\n    post:\n      operationId: createOrder\n      responses:\n        '201': { description: created }\n"
  },
  "mock": {
    "enabled": true,
    "mode": "openapi-behavior",
    "provider": "mock-service",
    "pool_class": "general",
    "defaults": { "seed": 42 },
    "operations": [
      {
        "operation_id": "createOrder",
        "responses": [
          { "name": "created", "match": { "status": 201 }, "weight": 95, "latency": { "distribution": "lognormal", "p50_ms": 40, "p95_ms": 120, "p99_ms": 180 }, "body": { "source": "schema_example" } },
          { "name": "slow-timeout", "match": { "status": 503 }, "weight": 5, "latency": { "distribution": "fixed", "ms": 900 }, "body": { "source": "inline", "content": { "error": "upstream_timeout" } } }
        ]
      }
    ],
    "capacity_request": { "rps_units": 300, "concurrent_connections": 120 }
  },
  "database_bindings": [
    { "database_id": "db-orders", "binding": "ORDERS_DB", "access": "readwrite" }
  ],
  "dependencies": [
    { "service_id": "svc-payment", "type": "http", "ports": [8080], "required": true }
  ],
  "network_policy": { "mode": "dependency-derived" }
}
```

## 6. Service mock metadata

`mock` is optional metadata on the service definition (canonical storage in namespace service component). It describes **API mock behavior**; execution is delegated to the separate Mock Service (`docs/mock-service-api.md`).

- `enabled`: enable/disable mock behavior for this component/context (semantics finalized with namespace apply policy).
- `provider`: `mock-service` (MVP default).
- `pool_class`: e.g. `general` or `perf-isolated` (capacity/isolation class).
- `operations[]`: per OpenAPI operation variants with `match`, `status`, `weight`, `latency` (duration/distribution), headers/body behavior, and optional fault profiles.
- `capacity_request`: requested units (admission checked by Mock Service; insufficient => `409 INSUFFICIENT_MOCK_CAPACITY` during apply/reconcile path as designed).
- `defaults.seed`: reproducible selection/latency draws.

Mock metadata is validated against the service OpenAPI and platform guardrails. Namespace apply/readiness must make **real vs mock** behavior unambiguous.

## 7. Dependencies and NetworkPolicy

- `dependencies[]` declare which other namespace services this service requires (type, ports/protocol, purpose, required/optional).
- Dependencies are validated during namespace validate/apply. Missing/invalid dependencies block readiness.
- **NetworkPolicy is derived from dependencies** for each service (least privilege): allow declared dependency traffic + required platform allowances + load-test runner access needed for targeted runs.
- Dependent services/mocks must be linkable/communicable under the namespace deployment model.
- Runner policy: for load-test runs targeting this service, render an additional egress policy (runner → resolved target pod + port) and ingress policy (target ← runner) selected by label `loadtest.io/run-id`. NetworkPolicy is L3/L4 only (`service` + `port`); `operation_id`/`path` are validated and audited here but not enforced by NetworkPolicy. Full spec: [`docs/network-policy.md`](network-policy.md).

## 8. Relationship to load tests

- Namespace must be `ready_for_loadtest=true` before load tests run.
- Load tests select **target API(s)** from namespace services’ parsed OpenAPI operations (`service_id` + `operation_id` or `method`+`path`).
- Load tests must also include the required **JS script** definition (see `docs/loadtest-manager-api.md`).

## 9. Validation / errors

- Missing required `openapi`: `422`.
- Invalid OpenAPI syntax/semantics: `422` with details.
- Invalid `mock` metadata (unknown operation, bad latency/weights/size): `422`.
- Unknown `database_id`/dependency `service_id`: `422`.
- Authz/tenant violations: `401/403/404` per platform policy.
- Delete blocked by references/active runs: `409` (policy).
- Insufficient mock capacity during apply/reconcile path: `409` (`INSUFFICIENT_MOCK_CAPACITY`) per Mock Service policy.

## 10. Legacy note

Older global `/api/loadtest/v1/services` SUT-catalog ideas are superseded by Environment-scoped (API path: `/namespaces/{namespace_id}`) service components in this model. Keep compatibility aliases only if explicitly decided and documented.

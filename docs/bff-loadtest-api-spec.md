# BFF API Specification — Self-Service Ephemeral Load Testing Engine (MVP)

This document defines the **BFF REST/JSON API contract** for the platform. The design is **namespace-first** (clear model): define/manage a namespace environment, ensure it is **READY**, then define/run load tests that explicitly target APIs and provide a JS (k6) script.

- Stack (MVP, `AGENTS.MD`): **Go + go-chi**, **Swagger/OpenAPI**.
- Execution: **async/operator-driven** (Kubernetes operators, incl. k6-operator via Go K8s API, no `kubectl`), **TTL/cleanup**, force close.
- **Primary model:** [`docs/namespace-management-api.md`](namespace-management-api.md) — namespace-first model, component CRUD, readiness, and load-test gate.
- Related: [`docs/loadtest-manager-api.md`](loadtest-manager-api.md), [`docs/service-management-api.md`](service-management-api.md), [`docs/database-management-api.md`](database-management-api.md), [`docs/k6-operator-go-client.md`](k6-operator-go-client.md), [`docs/nginx-oidc-auth.md`](nginx-oidc-auth.md), [`docs/admin-oidc-integration-api.md`](admin-oidc-integration-api.md), [`docs/architecture.md`](architecture.md).

> **Authorization:** all APIs are **user-scoped**. Tenant isolation always. Owner-only by default; **`admin`** may access/manage across owners **within the same tenant**. Identity comes from trusted nginx headers (`X-Tenant-Id`, `X-User-Id`, normalized admin role) — never from client-supplied owner IDs.

## Common conventions

| Concern | Convention |
|---|---|
| Base path | `/api/loadtest/v1` |
| Format | `application/json; charset=utf-8` (multipart where file upload is explicitly supported) |
| Auth | OIDC/JWT/session enforced at nginx; backend enforces owner/admin + tenant authz. `401` unauthenticated, `403` forbidden, `404` not found (apply consistent disclosure policy). |
| IDs | Opaque, URL-safe (e.g., `ns-...`, `svc-...`, `db-...`, `ing-...`, `lt-...`). |
| Timestamps | RFC3339/ISO-8601 UTC. |
| Async | Long-running mutations may return `202 Accepted` with `status`/`status_url`; poll `GET .../{id}` or `/readiness`. |
| Pagination | `page` (1-based) + `limit`; response `total`, `page`, `limit`, `data`. |
| Errors | Consistent envelope: `{ "error": { "code": "...", "message": "...", "details": [...] } }`. `400` malformed; `422` validation (schema/OpenAPI/targets/script/bindings); `401` unauthenticated; `403` forbidden; `404` not found/not visible (apply chosen disclosure policy consistently); **`409` conflict/state/not-ready** (`NAMESPACE_NOT_READY`, active run blocks delete, duplicate idempotency conflict as policy defines); `429` quota/guardrail. |
| Ownership | Persist immutable `tenant_id`, `owner_user_id` on managed resources. |

## 1. Namespace — top-level environment definition (primary)

> **MVP mapping (locked):** “Namespace” in `/api/loadtest/v1/namespaces` means a platform **Environment**. **1 Environment = 1 Kubernetes Namespace** (deterministic name/labels). Components are reconciled in that mapped K8s Namespace; load-test runner resources are subject to run-scoped, network-enforced egress NetworkPolicy. **POC auth (locked):** OIDC/nginx auth (`conf/nginx`) reflects identity/permissions from OIDC/claims into trusted headers; Go go-chi middleware normalizes principal and enforces RBAC/fail-closed. `tenant_id` scopes org/multi-domain boundary; `abt_` is service-account/base token for CLI/automation.

A **namespace** defines the full environment needed for load testing: **services, databases, ingress**, and dependency-derived **NetworkPolicy**. Components are manageable via namespace-scoped CRUD. A namespace must be **READY** (`ready_for_loadtest=true`) before load tests can be created.

### 1.1 Define/manage namespace
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces` | Create namespace; optionally full `spec` (services/databases/ingress) and `apply`. `201` when stored without async apply; `202` when `apply=true`/reconcile triggers async work (include `status_url`). |
| `PUT` | `/api/loadtest/v1/namespaces/{namespace_id}` | Replace full namespace definition (declarative). |
| `GET` | `/api/loadtest/v1/namespaces` | List namespaces (`status`, `ready_for_loadtest`, `owner` admin-only, `q`, pagination). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}` | Namespace detail, component/readiness/policy summary. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/readiness` | Readiness gate status: `ready_for_loadtest`, blocking reasons, per-component status. |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/validate` | Dry-run validate definition (schema, OpenAPI, deps, DB bindings, ingress, NetworkPolicy). |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/apply` | Async apply/reconcile desired state (`202`). Support `Idempotency-Key`; concurrent apply uses conflict/version policy. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}` | Delete/archive (guard active refs/runs). |

### 1.2 Services CRUD (namespace-scoped) — **OpenAPI/Swagger required per service**
Each service **MUST include its own OpenAPI/Swagger spec** when defined/updated (validated and parsed). This is the contract used to select which APIs a load test targets.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | Add service + deploy config + **`openapi`** + DB bindings + dependencies. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services` | List services. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Get service definition/status. |
| `PUT/PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Update service; OpenAPI revalidated when changed. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}` | Remove service (guard refs). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/openapi` | Get service OpenAPI/Swagger. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/services/{service_id}/apis` | List parsed operations (for load-test API targeting). |

**Service `openapi` (required):** includes format/version and spec content or approved ref (e.g., inline JSON/YAML; multipart/ref forms finalized in implementation). Validation errors -> `422`.

### 1.3 Databases CRUD (namespace-scoped)
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | Add database (`database_id`, engine/operator, config, secret refs). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | List databases. |
| `GET/PUT/PATCH/DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Get/update/remove (guard bindings/instances). |

### 1.4 Ingress CRUD (namespace-scoped)
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | Add ingress rule targeting namespace service(s). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | List ingress definitions. |
| `GET/PUT/PATCH/DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Get/update/remove. |

### 1.5 Dependencies & NetworkPolicy (namespace-scoped, derived/managed)
Dependencies are declared on services and validated; **NetworkPolicy is configured from dependencies** for each service (least privilege). Dependent services must be linkable/communicable within the allowed namespace model.
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/dependencies` | Dependency graph/resolution. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/network-policies` | Effective/rendered NetworkPolicy status/refs. |

## 2. Load Tests — only after namespace is READY

A load test targets a **ready namespace**, explicitly declares **which API(s)** it affects, and provides the **JS file (k6 script)** that defines the test.

### 2.1 `POST /api/loadtest/v1/namespaces/{namespace_id}/loadtests` — Create & trigger (`201`)
Support `Idempotency-Key` to prevent duplicate runs on client retry.
**Preconditions:** `namespace_id` (path segment) required; namespace `ready_for_loadtest=true` else **`409 Conflict`** (`NAMESPACE_NOT_READY`). Targets are **derived server-side** from each service's parsed OpenAPI operations (not client-supplied). `script_id` (validated, status `VALIDATED`) is required. For approved remote/script refs, script must be `VALIDATED` per policy (async validation Job + K8s watcher) else `409 SCRIPT_NOT_VALIDATED`/`422` as finalized; inline scripts use guardrails/light validation.

**Request body (clear/required shape):**
```json
{
  "name": "checkout-api-stress",
  "script_id": "scr-7f2e9a1b",
  "load_config": { "vusers": 200, "duration": "10m", "timeout_minutes": 15 }
}
```
*(JS definition mandatory, referenced by `script_id`; must already be `VALIDATED`. Supported forms: `inline` (staged via `/namespaces/{id}/scripts`), managed upload, or approved `script_ref` (e.g., S3/object ref). k6-operator has no native S3 source; approved refs are validated and staged to `configMap`/`volumeClaim` before `TestRun`. Remote refs should require async validation via K8s Job + watcher/informer before execution; see `docs/loadtest-manager-api.md` §5.)*

**Security note:** targets are **derived server-side** from each namespace service's parsed OpenAPI operations and recorded for audit; a k6 JS script can make arbitrary requests beyond the derived list. Implementation **must** enforce **network-level runner egress** via run-scoped NetworkPolicy in the Environment's mapped K8s Namespace (1:1): default deny egress, allow only resolved targets/approved deps/mocks + required DNS/platform allowances. The run-scoped policy is the enforcement target — targets themselves are metadata/audit only. Runner uses least privilege, no platform/K8s credentials; apply script/size/resource guardrails and SSRF-safe resolution.

**Response `201 Created`:**
```json
{
  "test_id": "lt-8f92a10b",
  "namespace_id": "ns-1a2b3c4d",
  "status": "PROVISIONING",
  "created_at": "2026-11-06T07:10:00Z",
  "expires_at": "2026-11-06T07:25:00Z",
  "status_url": "/api/loadtest/v1/namespaces/ns-1a2b3c4d/loadtests/lt-8f92a10b"
}
```

### 2.2 Scripts (validation/staging, async)
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/scripts` | Register script metadata / upload / approved ref (namespace-scoped). Returns `script_id`, checksum/hash, status. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/scripts/{script_id}` | Script metadata + validation/execution readiness status (projection-backed). |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/scripts/{script_id}/validate` | Start async validation (`202`). Creates isolated **K8s Job**; status reconciled via **K8s watcher/informer**. Idempotent policy. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/scripts/{script_id}/validations/{validation_id}` | Validation job status/diagnostics (`PENDING_VALIDATION, VALIDATING, VALIDATED, INVALID, VALIDATION_FAILED`). |

### 2.3 Load test management
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/loadtests` | List (filters `service_id`, `status`, owner admin-only). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/loadtests/{id}` | Status, targets, resolved namespace/service snapshot, metrics summary. |
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/loadtests/{id}/stop` | Force close / teardown (idempotent policy). |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/loadtests/{id}/logs` | Execution logs (authz-scoped, redacted). |
| (inferred) | `GET .../{id}/events`, `/metrics`, `/artifacts`, `POST .../rerun`, `DELETE .../{id}` | Timeline, telemetry, rerun, cleanup per policy. |

## 3. Mock Service (metadata in components, separate runtime)
Mocking is split by responsibility (see **[`docs/mock-service-api.md`](mock-service-api.md)**):
- **Metadata (canonical definition):** namespace **services** and **databases** may declare `mock` metadata as part of component CRUD. Service mock metadata is behavior over the service’s required OpenAPI (operation variants, latency/duration, weights, seed). Database mock metadata defines data/cloud dependency behavior profiles (operation/latency/result/error metadata).
- **Runtime/execution (separate Mock Service):** a dedicated Mock Service/operator materializes component mock metadata into runnable behavior (pools, endpoints/DNS, config snapshots, latency injection, metrics) with benchmark-derived capacity admission.

Key points:
- **ACID async writes:** mutating control-plane actions persist intent + capacity reservation + idempotency in one transaction; operators reconcile desired vs runtime/K8s state (level-triggered, `generation/observed_generation`). **GETs are projection-backed/fast**.
- **Capacity-planned pools** (benchmark-derived units, admission control), GA-scalable shared runtime (not one pod per mock binding), mock overhead measured.
- **Readiness/NetworkPolicy:** required mock components contribute to namespace readiness (`409 NAMESPACE_NOT_READY` if not ready). Real vs mock mode must be unambiguous. Mock endpoints access follows dependency-derived NetworkPolicy; runner egress remains network-enforced.

Summary runtime/control endpoints (detail in `mock-service-api.md`): `GET /api/loadtest/v1/namespaces/{namespace_id}/mock-status` (namespace-scoped mock status projection), `POST /api/loadtest/v1/namespaces/{namespace_id}/mocks/reconcile` (`202`), `GET /api/loadtest/v1/mock-pools[/{pool_id}]` (pool capacity/utilization; mutations admin-guarded; finalize path split in implementation).

## 3.1 Datasets (managed non-prod data for restore)
| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/datasets` | Register/import non-prod dataset metadata + approved ref (`202` if async). `Idempotency-Key`. |
| `GET` | `/api/loadtest/v1/datasets` | List datasets (owner own; admin within tenant policy). |
| `GET` | `/api/loadtest/v1/datasets/{dataset_id}` | Dataset metadata/status (no raw data download). |
| `PATCH` | `/api/loadtest/v1/datasets/{dataset_id}` | Update metadata/policy/retention (owner/admin policy-limited). |
| `DELETE` | `/api/loadtest/v1/datasets/{dataset_id}` | Delete/archive (guard active refs). |
| `POST` | `/api/loadtest/v1/datasets/{dataset_id}/validate` | Async re-validate checksum/format/ref (`202`). |

Non-prod sources only; owner = Environment creator; `pii_classification` = `masked|synthetic|non_prod_raw`; MVP load policies `reuse`/`reset_from_source`; `snapshot_per_run`/PITR roadmap. See `docs/database-management-api.md`.

## 4. Platform / Admin / Integration
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health`, `/ready`, `/metrics` | Liveness/readiness/Prometheus (public/internal per nginx). |
| `GET` | `/api/loadtest/v1/me` | Principal context (tenant, user, is_admin). |
| `GET` | `/api/loadtest/v1/guardrails` | Effective limits (optionally namespace/service-aware). |
| `/api/admin/v1/*` | (separate base) | Tenant/auth-config, token introspection, account/role/OIDC integration (`docs/admin-oidc-integration-api.md`). |

## 5. Status models (summary)

- **Namespace:** `DRAFT, VALIDATING, INVALID, APPLYING, DATA_LOADING, READY, DEGRADED, DELETING, DELETED` + `ready_for_loadtest` (`DATA_LOADING` when required dataset restore/import/verify is incomplete). `DELETE` from `READY` clears `ready_for_loadtest` automatically via `fn_namespace_transition` (sql/0013) before moving to `DELETING` — not blocked by `chk_namespaces_ready_only_when_ready` (sql/0012).
- **Load test (canonical):**
| Status | Meaning |
|---|---|
| `QUEUED` | Accepted, awaiting capacity/scheduling. |
| `PROVISIONING` | Creating/preparing namespace-run resources (script source, policies, TestRun, deps as needed). |
| `RUNNING` | k6 execution active. |
| `COMPLETED` | Finished successfully per test/operator outcome. |
| `FAILED` | Validation/provisioning/execution failed. |
| `STOPPING` | Stop/cancel/teardown requested and in progress. |
| `STOPPED` | Stopped/canceled before normal completion. |
| `TIMED_OUT` | Exceeded configured timeout/duration limit. |
| `TIMEOUT_CLEANED` | Timed out and cleanup completed. |
| `CLEANUP_FAILED` | Run finished/stopped/timed out but cleanup left residual resources (ops/admin action required). |
| `CANCELED` | Reserved/deprecated for MVP; use `STOPPED` unless implementation explicitly distinguishes user cancel from operator stop. |

## 6. Notes / migration
- Legacy/global `/services` or `/resources/ephemeral-db` shapes in earlier docs are superseded by namespace-scoped component CRUD in this clear model; keep aliases only if explicitly decided.
- Every load test **must** have: **ready namespace + explicit API targets + JS script**.

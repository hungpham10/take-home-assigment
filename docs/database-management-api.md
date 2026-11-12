# Database & Ingress Management API — Namespace-Scoped Components

This document defines **database and ingress components inside a namespace**, plus namespace-level dependency/NetworkPolicy implications relevant to data/connectivity. The platform model is namespace-first: these are **components of a namespace definition**, managed by Environment-scoped (API path: `/namespaces/{namespace_id}`) CRUD.

**Primary contracts:** [`docs/namespace-management-api.md`](namespace-management-api.md), [`docs/service-management-api.md`](service-management-api.md), [`docs/mock-service-api.md`](mock-service-api.md), [`docs/architecture.md`](architecture.md), [`docs/loadtest-manager-api.md`](loadtest-manager-api.md).

## 1. Purpose

- Manage **databases** (`database_id`) that namespace services can bind to.
- Allow database components to carry optional **mock metadata** for data/cloud dependency mocking (latency/operation profiles), persisted with the database definition and executed/reconciled by the separate Mock Service.
- Manage **ingress** rules that route to namespace services.
- Ensure component definitions participate in namespace validation/apply/readiness.
- Support dependency-driven **NetworkPolicy** so services can reach allowed dependencies/databases/mocks and be reached by allowed callers/load-test runners according to policy.

## 2. Scope and base paths

- Databases: `/api/loadtest/v1/namespaces/{namespace_id}/databases`
- Ingress: `/api/loadtest/v1/namespaces/{namespace_id}/ingress`
- Related namespace views: `/dependencies`, `/network-policies`, `/readiness`, `/mock-status` (Mock Service projection)

All resources are tenant-scoped via namespace authz (owner-only; admin cross-owner within tenant). Never accept client-supplied tenant/owner IDs for authorization.

## 3. Databases CRUD

Each database is a namespace component with a stable `database_id`. Services reference databases through `database_bindings`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | Create/add database component. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/databases` | List databases. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Get database detail/status (may include mock metadata/status summary). |
| `PUT` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Replace database definition. |
| `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Partially update database definition. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/databases/{database_id}` | Remove database (guard bindings/instances). |

**Database definition (conceptual):**
```json
{
  "database_id": "db-orders",
  "name": "orders-primary",
  "engine": "postgres",
  "operator": "cloudnative-pg",
  "version": "16",
  "config": {
    "storage_gb": 2,
    "node_count": 1,
    "parameters": {}
  },
  "init_script_ref": "s3://seeds/orders.sql",
  "ephemeral": {
    "enabled": true,
    "mode": "per-loadtest",
    "teardown": "ttl"
  },
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
  },
  "secret_ref": "secret://tenant/orders/db",
  "labels": { "domain": "orders" }
}
```

**Rules:** `database_id` unique within namespace. Service `database_bindings` must reference existing/valid namespace databases. Secrets are references only. **Real vs mock behavior must be unambiguous** at apply/readiness time (do not allow ambiguous dual-active mode). Database mock metadata is stored in the database component and reconciled/executed by Mock Service.

## 4. Database mock metadata

`mock` is optional metadata on the database definition. It supports mocking data/cloud dependency behavior (e.g., connection/query latency, read/write result templates, transient failures) as a **behavior profile**, not full arbitrary emulation unless explicitly scoped.

- `profile`: adapter/profile identifier (e.g., Postgres-compatible/cloud dependency profile), validated and versioned.
- `operations[]`: operation/shape-based behavior with `latency`, optional `weight`, `match`, `result`, or `error`.
- `pool_class`/`capacity_request`: passed to Mock Service admission (finite capacity).
- `defaults.seed`: reproducibility.

MVP should keep the operation vocabulary small and explicit; expand adapters/profiles by implementation phases.

## 5. Ingress CRUD

Ingress defines how requests are routed to namespace services (host/path/TLS/rules). It is validated against namespace services/ports.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | Create/add ingress component. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress` | List ingress rules/resources. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Get ingress detail/status. |
| `PUT` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Replace ingress definition. |
| `PATCH` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Partially update ingress definition. |
| `DELETE` | `/api/loadtest/v1/namespaces/{namespace_id}/ingress/{ingress_id}` | Remove ingress. |

**Rules:** target `service_id`/port must exist in namespace. Host/path conflicts and policy violations fail validation (`422`/`409`).

## 6. NetworkPolicy and dependencies (connectivity)

- Service dependencies are declared in service definitions (`docs/service-management-api.md`).
- **NetworkPolicy is derived from dependency configuration** for each service (least privilege): allow declared dependency traffic + required platform allowances (DNS/metrics/control-plane as needed) + load-test runner access needed for targeted runs.
- Dependent services, database endpoints, and mock endpoints must be linkable/communicable according to the namespace deployment model and NetworkPolicy.
- Database connectivity (real or mock) must be compatible with NetworkPolicy/service bindings.

**Visibility endpoints (namespace):**
| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/dependencies` | Resolved dependency graph. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/network-policies` | Effective/rendered NetworkPolicy status/refs. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/readiness` | Includes dependency/network-policy/mock readiness. |
| `GET` | `/api/loadtest/v1/namespaces/{namespace_id}/mock-status` | Mock Service materialized status for service/database mock metadata. |

## 7. Readiness gate impact

Namespace `ready_for_loadtest=true` requires required components (including databases/ingress where applicable by policy, plus dependency resolution, NetworkPolicy application, and required mock components/bindings) to be valid and ready. Load tests are rejected when the namespace is not ready.

## 8. Validation / errors

- Unknown `service_id`/`database_id` binding, invalid engine/config/ref, ingress target/host conflict: `422`/`409` with details.
- Invalid `mock` metadata (unknown profile/op, bad latency/weights/size): `422`.
- Insufficient mock capacity during apply/reconcile path: `409` (`INSUFFICIENT_MOCK_CAPACITY`).
- Authz/tenant violations: `401/403/404` per platform policy.
- Delete blocked by bindings/active instances/runs: `409` (policy).


## 10. Managed datasets (`/api/loadtest/v1/datasets`) — data preparation for DB restore

Datasets are **managed non-prod data artifacts** (MVP default: `pg_dump` custom-format dump file on approved object storage) used to load/restore database components. **Production buckets/backups are not used as dataset sources.** Restore/import runs as an isolated Kubernetes Job (not a runner pod), contributes to namespace `DATA_LOADING`/readiness, and never grants S3/object-store egress to untrusted k6 runner pods.

### 10.1 Decisions (locked for design)
| Topic | Decision |
|---|---|
| Source | Non-prod only. No `prod_copy_approved` policy. |
| Restore time | Configurable (`restore_policy.timeout`, resources/parallel jobs as applicable). |
| Reuse mode (MVP) | `reuse` and `reset_from_source` only. `snapshot_per_run` is roadmap (phase after). |
| Owner | Dataset owned by the **Environment creator** (`owner_user_id`); tenant-scoped. Admin can view/audit/suspend metadata within tenant; admin is not the owner. |
| PII | Allowed classifications: `masked`, `synthetic`, `non_prod_raw`. Even `masked`/`non_prod_raw` should pass simple import-time PII pattern checks (email, VN phone patterns) as policy allows; non-prod is not automatically safe. |

### 10.2 Dataset schema (conceptual)

```json
{
  "dataset_id": "ds-orders-seed-v3",
  "tenant_id": "t-001",
  "owner_user_id": "u-creator",
  "name": "orders-seed-v3",
  "source_type": "dump_file",
  "engine": "postgres",
  "engine_version": "16",
  "location": { "provider": "s3", "bucket": "lt-datasets-nonprod", "key": "orders/seed-v3.dump", "region": "ap-southeast-1" },
  "checksum": { "algorithm": "sha256", "value": "..." },
  "size_bytes": 2147483648,
  "restore_format": "pg_dump_custom",
  "schema_version": "migration_20261101",
  "pii_classification": "masked",
  "restore_policy": { "timeout": "30m", "parallel_jobs": 4 },
  "status": "AVAILABLE",
  "retention_until": "2026-12-01T00:00:00Z",
  "created_at": "2026-11-07T00:00:00Z",
  "updated_at": "2026-11-07T00:00:00Z"
}
```

**Status (dataset):** `IMPORTING | AVAILABLE | INVALID | EXPIRED | SUSPENDED | DELETING | DELETED`.

**Roadmap (not MVP):** `snapshot_per_run`; PITR from **non-prod** CNPG backups as optional large-dataset path.

### 10.3 Dataset API (tenant-scoped, owner/admin)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/datasets` | Register/import dataset metadata + approved non-prod object ref (`202` if async validation/import). `Idempotency-Key` supported. |
| `GET` | `/api/loadtest/v1/datasets` | List datasets visible to caller (owner own; admin within tenant as policy allows). |
| `GET` | `/api/loadtest/v1/datasets/{dataset_id}` | Get dataset metadata/status. No raw data download by default. |
| `PATCH` | `/api/loadtest/v1/datasets/{dataset_id}` | Update mutable metadata/policy/retention (owner; admin policy-limited). |
| `DELETE` | `/api/loadtest/v1/datasets/{dataset_id}` | Delete/archive (guard active Environment refs; `409` if in use, unless force policy). |
| `POST` | `/api/loadtest/v1/datasets/{dataset_id}/validate` | Async re-validate object ref/checksum/format (`202`). |

**Authz rules:** owner can create/read/update/delete own datasets (subject to guards). Admin can read/audit/suspend within tenant; **admin does not become owner**. No cross-tenant access. Never trust client-supplied `tenant_id`/`owner_user_id`. Sharing datasets across users requires explicit future share mechanism (not default in MVP).

**Object storage safety:** only approved non-prod refs/buckets; checksum required; scoped short-lived credentials or presigned reads for controlled jobs; never expose raw credentials to runners; do not accept arbitrary URLs.

### 10.4 Database component `data_source` (restore binding)

Namespace database components may reference a dataset:

```json
{
  "database_id": "db-orders",
  "engine": "postgres",
  "version": "16",
  "data_source": {
    "dataset_id": "ds-orders-seed-v3",
    "mode": "restore",
    "load_policy": "reuse"
  },
  "restore_policy": { "timeout": "30m", "parallel_jobs": 4 }
}
```

- `mode` MVP: `restore` (dump-based) or approved seed/init flow; finalize enum.
- `load_policy` MVP: `reuse` (keep/attach compatible restored data when allowed) or `reset_from_source` (restore/import again from dataset). `snapshot_per_run` deferred.
- `restore_policy.timeout` is configurable but enforced by K8s Job `activeDeadlineSeconds` / platform max guardrails.

### 10.5 Restore flow (dump-file MVP)

1. Namespace apply reconciles database and sees `data_source.dataset_id`.
2. Platform validates dataset is `AVAILABLE`, tenant/owner-visible (or explicitly attached), engine/version compatible, and location approved non-prod.
3. Platform creates isolated **restore K8s Job** in the Environment’s mapped namespace (ADR-001) with least privilege, run/load-scoped labels, no K8s/platform creds, and **minimal egress allowlist** to approved object-store/DNS endpoints only. Job uses scoped short-lived/presigned access; runner pods get no S3 access.
4. Job performs restore (e.g., `pg_restore` for `pg_dump_custom`) using configurable parallel jobs/resources and hard timeout (`activeDeadlineSeconds`).
5. Verify: object checksum (if applicable), restore command success, format sanity (e.g., `pg_restore --list`), and `schema_version`/compatibility checks per policy.
6. Watcher/informer updates projection: namespace moves through `DATA_LOADING` and becomes `READY` only when required data-load checks pass; failures set `DEGRADED`/`INVALID` with blocking reasons.
7. Reaper cleans up completed/failed Jobs and temporary artifacts per TTL; owner handoff/orphan handling follows `retention_until` and platform policy.

**Diagram (MVP):** S3 non-prod dataset (approved ref) → controlled restore Job (IRSA/scoped/presigned, minimal egress) → DB in `lt-env-<namespace_id>` → verify/checksum/schema sanity → namespace `DATA_LOADING`→`READY`.


## 9. Legacy note

Older generic `/resources/ephemeral-db` and per-service-global DB ideas are superseded by Environment-scoped (API path: `/namespaces/{namespace_id}`) component management in this clear model. Keep compatibility aliases only if explicitly decided and documented.

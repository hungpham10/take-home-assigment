# Database Schema

Source of truth: the `sql/` migrations (conventions in `sql/README.md`). This document describes each table, index, and invariant.

## Migration 0001 — Admin/Auth domain

### tbl_tenants
Tenant (org boundary). `slug` unique; `status`: `ACTIVE|SUSPENDED|DELETED`.

### tbl_tenant_hosts
Maps hostname → tenant for nginx. `host` unique; `is_default`.

### tbl_tenant_auth_profiles
Per-tenant auth configuration whose schema matches the Lua expectations in `conf/nginx` (issuer, client_id, jwks_uri, `config JSONB`). Unique `(tenant_id, name)`. **Contains no secrets** — secrets come from the SOPS env (`docs/secrets-management.md`); migration 0007 strips the sensitive keys.

### tbl_users
`tenant_id` immutable. Unique `(tenant_id, email)`.

### tbl_user_external_identities
OIDC link: unique `(issuer, subject)`.

### tbl_user_roles
MVP roles: `admin` (cross-owner within the tenant) | `user` (owner-only). Unique `(user_id, role)`.

### tbl_access_tokens
Service tokens `abt_...`: **only the SHA-256 hash is stored** (`token_hash` unique), `prefix` = first 8 characters for fast lookup. `expires_at`, `revoked_at` nullable. The plaintext is returned only once at issue time.

### tbl_audit_log
Append-only. `actor JSONB` holds the principal from trusted headers. Indexes `(tenant_id, created_at DESC)` and `(object_type, object_id)`.

## Invariants
- Every mutation in the admin domain writes 1 row to `tbl_audit_log`.
- Tenants/users/tokens are never hard-deleted — use status/`revoked_at`.

## Migration 0002 — Status transitions (pattern)

- `tbl_status_transitions (object_type, from_status, to_status, allowed)` — shared transition map.
- `fn_is_transition_allowed(object_type, from, to)` — map check.
- `fn_status_transition(main_table, events_table, object_type, id, to_status, actor, reason, meta)` — generic template: FOR UPDATE lock → check map → UPDATE status+version → INSERT event, all within one caller transaction. Domain migrations create wrappers `fn_<object>_transition`.

## Migration 0003 — Namespace & component domain

### tbl_namespaces (current state)
- `status`: `DRAFT, VALIDATING, INVALID, APPLYING, DATA_LOADING, READY, DEGRADED, DELETING, DELETED` (CHECK constraint).
- `ready_for_loadtest BOOL` — the gate for creating a loadtest; `version` for ETag/If-Match. `fn_namespace_transition` (sql/0013) clears the flag automatically when leaving READY (target ≠ READY); `chk_namespaces_ready_only_when_ready` (sql/0012) is the backstop CHECK.
- TTL: `ttl_expires_at` (NULL = no TTL) + `cleanup_policy JSONB` — the reaper (Plan 13) reads a partial index.
- Unique `(tenant_id, name)`; indexes `(tenant_id, status)`, `(owner_user_id, status)`.
- Transitions only to `DELETED` (soft delete) — never hard delete.

### tbl_namespace_events
Append-only; index `(object_id, id)`; wrapper `fn_namespace_transition(...)`.

### Components (FK CASCADE to namespace, no state machine — audit instead)
Per-component runtime status lives in the **`tbl_component_status` projection** (migration `0011_component_status.sql`, plans/21; namespace-level `desired_generation` / `blocking_reasons` / `status_observed_at` in the same migration). Components themselves stay stateless — the watcher writes observed phases there, and `GET /readiness` reads that projection.
- `tbl_services`: `openapi_spec JSONB` (NULL → blocks readiness, 422), `spec_hash` (spec change → revalidate), `parsed_summary JSONB` (operations index → `targets[]`), unique `(namespace_id, name)`.
- `tbl_databases`: engine + `profile JSONB`.
- `tbl_ingress`: host + `paths JSONB`.
- `tbl_dependencies`: `type` (service|database|mock) + `ref_id` + `config` — the source for deriving NetworkPolicy egress (ADR-002).
- `tbl_network_policies`: resolved `spec` + `observed_generation`.

## Migration 0004 — Script & Loadtest domain

### tbl_scripts (current state)
- `status`: `PENDING_VALIDATION, VALIDATING, VALIDATED, INVALID, VALIDATION_FAILED`; `INVALID/VALIDATION_FAILED` can be revalidated (`→VALIDATING`).
- `size_bytes CHECK ≤ 1MiB` (ConfigMap limit); content is stored via `content_ref` (ConfigMap/staging), only `content_hash` is persisted.
- `tbl_script_validations`: result of each validation run (`(script_id, attempt)` unique, `errors JSONB` → 422).

### tbl_load_tests (current state)
- `status`: `QUEUED, PROVISIONING, RUNNING, COMPLETED, FAILED, STOPPING, STOPPED, TIMED_OUT, TIMEOUT_CLEANED, CLEANUP_FAILED` (`CANCELED` reserved).
- `load_config JSONB`, `targets JSONB` (required), `runner_type k8s|local`, `runner_ref`.
- `timeout_at` + a partial index for the reaper (TIMED_OUT); app gate: namespace `ready_for_loadtest` + script `VALIDATED` (409 otherwise).
- `tbl_loadtest_events` append-only; `fn_loadtest_transition`.

### Migration 0014 — load test results (plans/45)
Four additive, nullable columns on `tbl_load_tests`. No default and no CHECK on `status`, so the
change is metadata-only (no table rewrite) and a result can be attached to `COMPLETED`, `FAILED`
or `STOPPED` alike.

| Column | Type | Notes |
|---|---|---|
| `result_summary` | `JSONB` | parsed k6 summary (`pkgs/runner.Summary`); NULL = never collected |
| `thresholds_failed` | `BOOLEAN` | NULL when no `█ THRESHOLDS` block was parsed |
| `result_collected_at` | `TIMESTAMPTZ` | set on success **and** failure; the idempotency guard (`... IS NULL`) |
| `result_parse_error` | `TEXT` | why the summary is missing/partial (pod GC'd, empty output, timeout) |

Contract: NULL means "not collected", not "collected nothing" — see
[`docs/loadtest-manager-api.md`](loadtest-manager-api.md) §8. Covered by
`tests/sql/loadtest_results_test.sql` (columns exist, old rows read back NULL, JSONB round-trip,
a FAILED run may carry a parse error).


### tbl_test_events
Append-only runtime stream, unique `(load_test_id, seq)`, `type log|metric|status`.

## State transitions & events (detailed pattern in plans/04)
- Objects with a state machine (`tbl_namespaces`, `tbl_scripts`, `tbl_load_tests`, `tbl_datasets`) change state **only** through `fn_<object>_transition(...)`:
  `SELECT ... FOR UPDATE` → check `tbl_status_transitions` → `UPDATE status, version+1` → `INSERT tbl_<object>_events` — all in one transaction; an illegal transition → exception (app maps to `409/422`).
- `tbl_<object>` = current state, the read source for list/get (JOIN + index); `tbl_<object>_events` is append-only and only JOINed when viewing history.
- Transition map table: `tbl_status_transitions (object_type, from_status, to_status, allowed)` — seeded with every valid transition.

## Migration 0005 — Platform plumbing

- `tbl_idempotency_keys`: UNIQUE `(tenant_id, endpoint, key)` — Idempotency-Key middleware; the reaper cleans entries older than 24h.
- `tbl_outbox_events`: transactional outbox — written in the same transaction as the state change, dispatched by a worker; partial index on `dispatched_at IS NULL`.
- `tbl_reservation_ledger`: quota `vusers|concurrent_loadtests|mock_pool`, state `RESERVED→COMMITTED|RELEASED`; exceeding quota → 429.
- `tbl_datasets`: `pii_classification masked|synthetic|non_prod_raw`, restore_config JSONB; 7-state lifecycle via `fn_dataset_transition`.

> The controller MVP SQL schema is frozen at this migration. Any later change = a new migration + an update to this doc.

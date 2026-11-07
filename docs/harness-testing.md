# Harness Testing — Integrity, Safety & ACID (MVP)

This document defines **harness tests**: fault-injection and integrity tests that **prove** the load-testing platform remains **safe, consistent, and stable** under failures (crash, exception, network error, timeout, partial outage, retries, concurrency). These are **not** happy-path functional tests and **not** performance tests.

**Why:** `AGENTS.md` requires CodeGraph/no-missing-edge-cases quality gates and k6 perf gates. The control plane manages metadata, K8s/runtime resources, and untrusted execution (k6 JS, external refs). Correctness under failure is a release-blocking property.

**Primary related docs:** [`docs/architecture.md`](architecture.md), [`docs/database-schema.md`](database-schema.md), [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md), [`docs/k6-operator-go-client.md`](k6-operator-go-client.md), [`docs/mock-service-api.md`](mock-service-api.md), [`docs/nginx-oidc-auth.md`](nginx-oidc-auth.md).

## 1. Scope & principles

| Principle | Meaning |
|---|---|
| **Metadata ACID first** | PostgreSQL is the source of truth for desired state/projections/audit. State transitions, idempotency, reservation, and outbox writes must be atomic where the contract requires it. |
| **Fail closed** | Authn/authz/header trust, readiness gates, quota/capacity admission, target/script validation, and runner egress must deny/stop on ambiguity or missing data — never silently allow. |
| **At-least-once, idempotent effects** | HTTP retries, reconciler retries, operator watches, outbox dispatch, and client `Idempotency-Key` replays may duplicate delivery/work. Handlers must be idempotent and use deterministic keys/versions. |
| **Crash-only tolerance** | Any process (BFF, reconciler, worker, validation job watcher, mock-service operator) may die between steps. Recovery must converge from persisted state + level-triggered reconcile; no orphaned “phantom committed” state. |
| **Untrusted input/runtime isolation** | OpenAPI specs, JS scripts, S3 refs, user targets metadata, and runner-executed JS are untrusted. Harness tests prove validation, SSRF-safe resolution, and **network-enforced** runner egress (not advisory). |
| **Tenant/owner isolation under failure** | Error paths, retries, cache misses, idempotency replays, and admin flows must never leak or mutate cross-tenant/cross-owner data. |

## 2. Harness categories

### 2.1 SQL / metadata integrity (DB harness)
Proves invariants implemented in `sql/` and relied on by application code. Runs in CI against migrated Postgres (extend `tests/sql/*` pattern).

| ID | Fault / scenario | Expected invariant / proof |
|---|---|---|
| DB-TXN-01 | Illegal state transition (every state machine) | `fn_*_transition` raises `check_violation`; **no** status/version/event mutation (atomic rollback semantics). |
| DB-TXN-02 | Concurrent transition on same object | `SELECT ... FOR UPDATE` serializes; exactly one transition succeeds per legal step; version increments once per applied transition; no lost events, no duplicate terminal states. |
| DB-TXN-03 | Missing object transition | Deterministic `OBJECT_NOT_FOUND` error; no partial writes. |
| DB-TXN-04 | Domain CHECK violations (script >1MiB, bad enum/PII/resource type, zero/negative amounts) | Rejected by DB constraints. |
| DB-TXN-05 | Idempotency key uniqueness | Duplicate `(tenant_id, endpoint, key)` rejected (or application-defined conflict) — never two logical creates for same idempotency scope. |
| DB-TXN-06 | Outbox + state change atomicity | Simulate: state transition + outbox insert in one tx commits together; forced rollback leaves neither committed (application test with real tx boundary). |
| DB-TXN-07 | Reservation ledger integrity | `RESERVED→COMMITTED|RELEASED` only via allowed path; no negative/over-release under concurrent commits/releases; quota admission cannot overbook (app+DB test). |
| DB-TXN-08 | FK/tenant scoping invariants | Cross-tenant references impossible where modeled; deletes/soft-deletes preserve audit/event history rules. |
| DB-TXN-09 | Append-only/event sequencing | Events/test events remain append-only; `(load_test_id, seq)` unique; history never rewritten by normal API paths. |
| DB-TXN-10 | Readiness/loadtest gate data invariants | `ready_for_loadtest`/status combinations used by gate are consistent with transition rules; app must still enforce gate (defense in depth). |

### 2.2 API / application integrity harness
Runs as Go integration tests (`tests/integration/...` or `pkgs/...` integration) with real Postgres (testcontainers/compose), fake clock where useful, and injected fakes for K8s/runner/S3.

| ID | Fault / scenario | Expected behavior |
|---|---|---|
| API-IDEM-01 | Retry same mutating request with same `Idempotency-Key` + same payload | Same logical resource/response semantics; no duplicate namespace/script/loadtest/reservation side effects. |
| API-IDEM-02 | Same `Idempotency-Key`, different payload (if policy disallows) | `409` conflict or documented behavior; never silently apply second payload. |
| API-IDEM-03 | Client retries after timeout/5xx (unknown commit) | Safe: replay returns committed result or deterministic conflict; reconciler/outbox handles duplicate downstream intents. |
| API-CONC-01 | Concurrent create/apply/stop on same object | Optimistic concurrency (`version`/`If-Match`/ETag or state checks) prevents lost updates; `409/412` as specified. |
| API-CONC-02 | Mutate Environment while active runs exist | Behavior matches policy (block/guard/conflict) — never corrupt running run metadata or bypass cleanup/TTL. |
| API-AUTHZ-01 | Missing/stripped trusted headers on protected route | **Fail closed** `401`. |
| API-AUTHZ-02 | Cross-tenant/cross-owner access attempt | `403/404` per disclosure policy; no data mutation/read leakage. |
| API-AUTHZ-03 | Admin override within tenant only | Admin can act cross-owner in same tenant; cannot cross tenant; never trust client-supplied owner/tenant IDs. |
| API-VAL-01 | Invalid/missing OpenAPI, bad `targets[]`, script >1MiB, malformed refs | `400/422` with stable error envelope; readiness blocked; no runnable state created. |
| API-CRASH-01 | Crash after DB commit, before HTTP response | Client retry + idempotency/outbox converges; no duplicate logical effect. |
| API-CRASH-02 | Crash after enqueueing runtime intent, before runtime ACK | Reconciler redrives from desired/projection state; idempotent runtime apply. |
| API-OUTBOX-01 | Dispatcher delivers outbox event twice | Consumer/handler idempotent; no duplicate external side effects beyond tolerated no-op. |
| API-OUTBOX-02 | Dispatch fails transiently (network/5xx) | Retry with backoff; `attempts/next_retry_at` progress; event not lost; poison handling policy documented/tested. |

### 2.3 Runner / K8s / reconciliation harness
Uses **fake Kubernetes API client** / envtest-like harness where feasible, plus **LocalRunner** integration for real `k6` process behavior (Plan 13/14). NetworkPolicy enforcement tests require cluster/CNI-capable environment (tagged `//go:build harness_k8s` or E2E).

| ID | Fault / scenario | Expected behavior |
|---|---|---|
| RUN-IDEM-01 | `TestRun` create retried / “already exists” | Treated by deterministic run identity; no duplicate runs; status reconciled. |
| RUN-CRASH-01 | Controller crashes during `QUEUED→PROVISIONING→RUNNING` | Reconciler resumes from DB + observed `runner_ref`/status; no stuck permanent inconsistency without timeout/reaper path. |
| RUN-NET-01 | K8s API transient errors / timeout | Retry/backoff; state reflects uncertainty safely; no false `COMPLETED`. |
| RUN-NET-02 | Runner cannot reach disallowed destination | Blocked by **NetworkPolicy egress** (cluster harness); script may fail, but platform must not grant broad egress. `targets[]` metadata mismatch is audit/validation issue, not network permission. |
| RUN-STOP-01 | Stop requested while `RUNNING` | Idempotent `STOPPING→STOPPED`; duplicate stop requests safe. |
| RUN-TIMEOUT-01 | Run exceeds `timeout_at` / duration | Reaper transitions per model (`TIMED_OUT→TIMEOUT_CLEANED` or `CLEANUP_FAILED`); no leaked local process/TestRun beyond policy. |
| RUN-LEAK-01 | Cleanup fails after timeout/stop | `CLEANUP_FAILED` surfaced; reaper/operator retries cleanup idempotently; audit/ops signal emitted. |
| RUN-ISO-01 | Parallel runs in same Env/K8s namespace | Run-scoped labels/selectors/owner refs prevent cross-run interference; quota/limits enforced. |

### 2.4 External dependency / data-plane fault harness
| ID | Fault / scenario | Expected behavior |
|---|---|---|
| EXT-NET-01 | S3/object-store timeout, DNS failure, 5xx during dataset/script staging/validation ref fetch | Bounded retry/backoff; clear terminal status (`VALIDATION_FAILED`/dataset error) when exhausted; no partial “validated/ready” claim. |
| EXT-NET-02 | SSRF / non-approved ref / redirect to disallowed host | Rejected by validation/resolver policy; never fetched from untrusted internal metadata endpoints. |
| EXT-PARTIAL-01 | Partial upload / checksum mismatch / corrupted content | Rejected; `content_hash`/checksum verified; no state advance. |
| MOCK-NET-01 | mock-service unavailable/degraded | Namespace/mock readiness degrades per policy; **do not** mark load test meaningful-success if mock SLO/health contract is violated (documented). Controller remains source of truth. |
| MOCK-VER-01 | Snapshot `schema_version`/generation mismatch | mock-service rejects incompatible snapshot, reports `observed_generation`/error; controller projection shows degraded/conflict — no silent partial apply. |

### 2.5 Nginx / trust-boundary harness (integration)
| ID | Fault / scenario | Expected behavior |
|---|---|---|
| NGX-01 | Client sends forged `X-Tenant-Id`/`X-User-*` | nginx strips/overwrites at edge (config contract); backend also fails closed if boundary assumptions violated. |
| NGX-02 | Introspection/auth-config upstream timeout | nginx path fails safely (no anonymous privileged access); cached behavior explicit and TTL-bounded. |
| NGX-03 | Role propagation gap on specific auth path | Harness documents expected header set per path; backend canonical role normalization tested with fixtures. |

## 3. ACID mapping (what “ACID” means here)

| Property | Platform interpretation | Harness proof |
|---|---|---|
| **Atomicity** | A state transition + event append (+ required same-tx side records like outbox/idempotency decision/reservation step where designed) commit or rollback together. | DB-TXN-01/06, API-CRASH-01, integration tx-abort tests. |
| **Consistency** | Constraints, transition maps, tenant/ownership rules, readiness gates, quota/capacity rules, and non-negative/valid enums always hold. | DB-TXN-04/07/08/10, API-VAL/AUTHZ. |
| **Isolation** | Concurrent transitions/requests do not produce lost updates, double-spend quota, or interleaved illegal states. | DB-TXN-02, DB-TXN-07, API-CONC-*. |
| **Durability** | Once acknowledged committed, metadata survives process restart; recovery resumes from durable state. | API-CRASH-*, restart/reconcile integration tests. |

**Distributed caveat:** Kubernetes/runtime effects and external calls are **not** in the same DB transaction as Postgres commit. Harness tests must prove **transactional outbox / idempotent reconcile / compensating cleanup** semantics, not pretend global ACID across K8s+S3+DB.

## 4. Required fault-injection controls (test hooks)

Implementations should expose **test-only** controls (build tag `harness` or internal interfaces/fakes) — never enabled in production:

- Fake clock / controllable `time.Now` for TTL/timeout/reaper.
- Runner/K8s client interface with injected errors, delays, “already exists”, stale reads.
- Outbox dispatcher with duplicate delivery / crash-between-mark attempts.
- HTTP middleware test hook for idempotency store failures (verify fail-safe behavior: reject unsafe ambiguous write or documented retry-safe path).
- Network/ref resolver with DNS failure, timeout, redirect, disallowed host fixtures.
- Reconciler single-step/run-once mode for deterministic assertions.

## 5. Execution model & CI placement

| Layer | Location | Runs when | Blocking |
|---|---|---|---|
| SQL integrity harness | `tests/sql/*` (+ new harness cases) | CI on SQL/Go changes, local | **Yes** |
| Go unit + race (where practical) | `pkgs/...`, `cmds/...` | CI/local | **Yes** |
| Integration harness (Postgres + fakes) | `tests/integration/` or tagged packages | CI/local | **Yes** (MVP core cases) |
| LocalRunner + real k6 harness | `tests/integration/` | CI if k6 available / nightly | MVP subset blocking |
| K8s/NetworkPolicy/CNI harness | `tests/harness/k8s/` (`//go:build harness_k8s`) | cluster CI / manual / nightly | **Required before GA/K8s production claim**; optional in minimal CI if no cluster |
| k6 perf | `tests/perf/` | per `plans/14-local-stack-e2e.md` | **Yes** (merge gate) |
| E2E smoke | compose/Earthfile stack | CI/local | **Yes** |

**Reporting:** harness failures must emit object IDs, tenant scope, state/version, idempotency key (redacted if needed), and transition/event evidence — no secrets.

## 6. Release gate checklist

Before merge/release of controller/runner/mock control-plane changes:
- [ ] Relevant **SQL harness** cases pass (existing + new cases for touched state machines/invariants).
- [ ] Relevant **API/runner fault-injection** cases pass (idempotency, crash/retry, authz fail-closed, validation).
- [ ] No bypass of readiness/quota/network safety found in CodeGraph review.
- [ ] K8s/NetworkPolicy harness executed for changes affecting runner egress, TestRun lifecycle, or namespace isolation (or explicitly waived with risk note).
- [ ] `docs/` + `plans/` updated if invariants/behavior changed.

## 7. Non-goals (MVP harness)

- Proving global exactly-once across DB + K8s + S3 (use idempotency/convergence).
- Full chaos platform (Litmus/Chaos Mesh) in MVP CI — add later; start with deterministic fakes + targeted integration faults.
- Performance/load correctness under benchmark pressure (covered by k6 perf gate), except where perf causes integrity risk (timeouts, queue/backpressure). 

## 8. Implementation status (2026-11-07, plan-16 scope)

Implemented per `plans/16-harness-testing-integrity.md` "Implementation scope 2026-11-07":

- **SQL harness** — `tests/sql/harness_test.sql`, runs in the CI `sql-migrations` job
  (`for t in tests/sql/*_test.sql`): `DB-TXN-01..05`, `IDEM-CONFLICT-01`,
  `RESERVATION-01`, `TERMINAL-01`, `APPEND-01` (skips under superuser/owner —
  enforce with the app role).
- **Go harness** — `tests/harness/harness_test.go` (package `harness_test`),
  through the REAL server composition (`pkgs/testutil.NewServer`):
  `API-AUTHZ-01..04`, `API-IDEM-01`, `API-CONC-01`, `API-VAL-01`,
  `STATE-QUEUE-01`, `STATE-MEM-01`, `RUN-STOP`, `RUN-TIMEOUT`. Integration
  tests skip without `TEST_DATABASE_DSN`, hard-fail when `CI` is set.
- **K8s scaffold** — `tests/harness/k8s/egress_test.go`
  (`//go:build harness_k8s`), skip-guarded, compiles under the tag only.
- **Behavior fixes landed with the harness** (TDD):
  - `pkgs/script` queue-full now returns **429 (QUOTA_EXCEEDED)** and leaves
    the script status untouched (plan High-2; was mislabeled 422).
  - `pkgs/adminapi` `issueToken`/`listTokens` resolve the **target tenant from
    the target user's DB row**, not the caller's own `X-Tenant-Id` header —
    closes the cross-tenant admin token-issue hole surfaced by API-AUTHZ-03.
- **Invariant-break proof:** disabling the `RequireSelfOrAdmin` check in
  `issueToken` flips `TestAPIAUTHZ03AdminCrossOwnerMutationAndAudit` and
  `TestAPIAUTHZ03SelfOnlyTokenGate` to FAIL (verified 2026-11-07; check restored).

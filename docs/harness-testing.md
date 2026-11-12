# Harness & Integration Testing — Integrity, Safety, ACID & Functional Scenarios (MVP)

This document defines the **test strategy and scenario catalogue** of the load-testing platform. It has two complementary halves:

- **Harness tests (§1–§8):** fault-injection and integrity tests that **prove** the platform remains **safe, consistent, and stable** under failures (crash, exception, network error, timeout, partial outage, retries, concurrency).
- **Functional integration tests (§9–§15):** scenarios that prove the platform **behaves correctly per contract** (happy path, business rules, stable error codes, end-to-end lifecycles) across HTTP → handler → Postgres, fake K8s/runner, nginx, and kind.

Neither half is a performance test; perf has its own gate (§5, §12.4).

**Why:** `AGENTS.md` requires CodeGraph/no-missing-edge-cases quality gates and k6 perf gates. The control plane manages metadata, K8s/runtime resources, and untrusted execution (k6 JS, external refs). Correctness under failure is a release-blocking property.

**Primary related docs:** [`docs/architecture.md`](architecture.md), [`docs/database-schema.md`](database-schema.md), [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md), [`docs/k6-operator-go-client.md`](k6-operator-go-client.md), [`docs/mock-service-api.md`](mock-service-api.md), [`docs/nginx-oidc-auth.md`](nginx-oidc-auth.md), [`docs/mvp-status.md`](mvp-status.md) (what is implemented today), `sql/README.md`.

**Boundary between the two halves:** a scenario whose point is a **fault injected into the system** (crash between two steps, duplicate event, K8s timeout, concurrent writers) is a harness case (`DB-TXN-*`, `API-*`, `RUN-*`, `WATCH-*`, …). A scenario whose point is **correct behaviour of a feature** is a functional integration case (`INT-*`, `E2E-*`). When both apply, keep the ID in one place and cross-reference it from the other (see §11.6).

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

### 2.3.1 Namespace status watcher harness (observed → PG loop)
Design: `docs/namespace-status-watcher.md` (§3, §9). Uses fake informers/cache + fault-injected K8s client; implemented in plan 21.

| ID | Fault / scenario | Expected behavior |
|---|---|---|
| WATCH-01 | Informer events dropped mid-flight | Periodic resync converges to the correct component/namespace status. |
| WATCH-02 | Duplicate / reordered events | Result unchanged (worker recomputes from cache, never event payload). |
| WATCH-03 | Spec edited after previous READY | Old generation's READY does not count (generation fencing); status stays PROGRESSING until `observed_generation == desired_generation`. |
| WATCH-04 | Watcher crashes between component upsert and namespace transition | Restart recomputes and converges; no duplicate event / double transition. |
| WATCH-05 | Watcher stopped (no heartbeat) | `POST /loadtests` returns `NAMESPACE_NOT_READY` reason `STATUS_STALE` after staleness threshold; fail-closed. |
| WATCH-06 | Component flaps not-READY within grace window | No DEGRADED emitted (hysteresis); READY↔DEGRADED does not flap. |

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

Shared (non-fault) test infrastructure used by both harness and functional integration tests — composition root, identity fixtures, data fixtures, isolation rules — is in §9.

## 5. Execution model & CI placement

| Layer | Location | Runs when | Blocking |
|---|---|---|---|
| SQL integrity harness | `tests/sql/*` (+ new harness cases) | CI on SQL/Go changes, local | **Yes** |
| Go unit + race (where practical) | `pkgs/...`, `cmds/...` | CI/local | **Yes** |
| Integration harness (Postgres + fakes) | `tests/integration/` or tagged packages | CI/local | **Yes** (MVP core cases) |
| **Functional integration (`INT-*`)** — Postgres + fake K8s/runner, §11 | `pkgs/*/*_integration_test.go`, `tests/integration/` | CI `test`; local with `TEST_DATABASE_DSN` | **Yes** |
| LocalRunner + real k6 harness | `tests/integration/` | CI if k6 available / nightly | MVP subset blocking |
| K8s/NetworkPolicy/CNI harness | `tests/harness/k8s/` (`//go:build harness_k8s`) | cluster CI / manual / nightly | **Required before GA/K8s production claim**; optional in minimal CI if no cluster |
| **Nginx integration (`INT-NGX-*`)**, §12.1 | `tests/nginx/` (compose: nginx + controller + postgres + fake OIDC) | CI/nightly | **Yes** (subset); required for PRs touching `conf/nginx` |
| k6 perf | `tests/perf/` | per `plans/14-local-stack-e2e.md` | **Yes** (merge gate) |
| E2E smoke | compose/Earthfile stack, `tests/e2e-smoke.sh` | CI/local | **Yes** |

**Reporting:** harness failures must emit object IDs, tenant scope, state/version, idempotency key (redacted if needed), and transition/event evidence — no secrets.

## 6. Release gate checklist

Before merge/release of controller/runner/mock control-plane changes:
- [ ] Relevant **SQL harness** cases pass (existing + new cases for touched state machines/invariants).
- [ ] Relevant **API/runner fault-injection** cases pass (idempotency, crash/retry, authz fail-closed, validation).
- [ ] No bypass of readiness/quota/network safety found in CodeGraph review.
- [ ] K8s/NetworkPolicy harness executed for changes affecting runner egress, TestRun lifecycle, or namespace isolation (or explicitly waived with risk note).
- [ ] `docs/` + `plans/` updated if invariants/behavior changed.

Additional gate for **functional behaviour changes** (definition of done):
- [ ] At least one `INT-*` scenario added/updated per new behaviour, using the ID and domain table in §11.
- [ ] New route added to the route × principal matrix (`INT-MATRIX-01`); new transition covered by `INT-MATRIX-02`.
- [ ] New error branch has a stable error code and is asserted via `INT-MATRIX-03`.
- [ ] Status changes go only through `fn_*_transition` (`INT-LT-27` green).
- [ ] Failure semantics touched (retry, crash, concurrency) → harness case added/updated in §2.
- [ ] Column "status" in `docs/mvp-status.md` updated; known gaps in §15 updated.

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

---

# Part II — Functional integration tests

Sections §9–§15 below form the catalogue of **functional integration** scenarios (kept in sync with `mvp-status.md` and `k8s-orchestration.md`). The section numbers §1–§8 above are kept unchanged so that references from other documents (`k8s-orchestration.md`, `namespace-status-watcher.md`, `architecture.md`) do not break.

## 9. Principles & shared test infrastructure

### 9.1 Rules for choosing a layer
- Pure logic with no I/O → **unit**.
- Involves SQL, transactions, constraints, JOINs, or needs to verify DB state after a request → **integration**. Do not mock the DB at this layer; mocking the DB removes exactly what needs to be verified (transition functions, CHECKs, FKs).
- K8s: **fake client** by default (`pkgs/k8s` fake, with fault injection). Only go to kind when real cluster behaviour is needed (NetworkPolicy enforcement, CRDs, real informers).
- k6 binary: **fake k6** by default (shell script on `PATH`) for deterministic tests; real k6 only in nightly/e2e.

### 9.2 Composition
Every integration test (both harness and `INT-*`) goes through **the server's actual composition root** (`pkgs/testutil.NewServer`) — do not build your own router/middleware, do not re-create auth (plans/16). Tests that need a different configuration (`K8S_ENABLED`, master key, runner) pass it through `NewServer` options, not by forking the wiring.

```go
srv := testutil.NewServer(t, testutil.WithK8s(fakeK8s), testutil.WithRunner(fakeRunner),
                          testutil.WithClock(clk), testutil.WithMasterKey(key))
```

### 9.3 Identity fixtures
Each test creates its own data through helpers; no shared data between tests:

| Fixture | Description |
|---|---|
| `tenantA`, `tenantB` | Two tenants, with a random slug per test (`t-<rand>`) |
| `ownerA` | user with role `user`, tenant A |
| `otherA` | user with role `user`, tenant A (not the owner) |
| `adminA` | role `admin`, tenant A — role **stored in `tbl_user_roles`** (not just a header) |
| `adminB` | role `admin`, tenant B |
| `abtToken(user)` | real `abt_` token issued through the API |

Helper `srv.As(user).Do(req)` sets trusted headers (`X-Tenant-Id`, `X-User-Id`, `X-User-Role`) the same way nginx does; `srv.AsRaw(headers)` is for testing forged/missing headers.

### 9.4 Data & time isolation
- Migrations run **once per package** (`TestMain`) using the real migration runner.
- Each test uses its own tenant → `t.Parallel()` is safe, no truncation needed. Tests that need global state (reconciler scanning the whole table, reaper) use a **dedicated DB/schema** (`testutil.NewIsolatedDB`) and do not run in parallel.
- Do not use `time.Sleep`. Wait for asynchronous state with `testutil.Eventually(t, 5*time.Second, cond)`; control time (TTL, timeout, grace/stable window, staleness) with the **fake clock** (§4).

### 9.5 Data fixtures
```text
tests/fixtures/
  openapi/   orders_valid.yaml  orders_valid.json  payment_valid.yaml
             invalid_syntax.yaml  invalid_semantic.yaml  no_paths.yaml
             no_operation_id.yaml  oversize.yaml
  k6/        ok.js  syntax_error.js  import_forbidden.js  slow_30s.js  exit_nonzero.js  oversize_1mib.js
  fakebin/   k6    # shell stub: behaviour selected by env FAKE_K6_MODE=ok|inspect_fail|exit1|hang
```

### 9.6 Test-writing conventions
- Names: `TestINT_<DOMAIN>_<NN>_<Description>` — the ID matches the tables in §10–§12 so it can be grepped. Harness tests keep their original §2 IDs (`TestAPIAUTHZ03…`, `TestAPIIDEM01…`).
- Each integration test asserts **three layers** where possible: (1) HTTP status + error envelope `{error:{code,message,details}}`; (2) DB state (`status`, `version`); (3) event trail (`tbl_*_events`) in the right order, with the right actor.
- Table-driven for variants (role × route, OpenAPI formats…).
- Assert **stable error codes** (`NAMESPACE_NOT_READY`, `SCRIPT_NOT_VALIDATED`, `QUOTA_EXCEEDED`, `STATUS_STALE`), never assert message strings.
- Never log/assert plaintext secrets; secret tests check for the **absence** of the value in the DB, logs, and responses.
- Local without a DB → `t.Skip`; `CI=true` → **hard fail** (already in place, keep it).

## 10. Cross-cutting control matrix (written once, runs for every route)

Three "auto-generated" tests catch forgotten tests when a route/transition is added:

| ID | Content | Approach / Expected |
|---|---|---|
| `INT-MATRIX-01` | **Route × principal.** Use `chi.Walk` to enumerate every route of `/api/loadtest/v1` and `/api/admin/v1` (operator). For each route run 5 principals: no header, ownerA (ownerA's resource), otherA, adminA, adminB. | No header → 401; otherA/adminB → 404 (not 403, to avoid leaks); ownerA/adminA → not 401/404. A new route missing from the expectation table → test fails ("route has no declared policy"). Extends `API-AUTHZ-01..04`. |
| `INT-MATRIX-02` | **Transition coverage.** Read `tbl_status_transitions` (seed) for each `object_type`. | Every `allowed=true` pair has at least one test that traverses it (via API or `fn_*_transition`); every pair outside the map is rejected (409/exception). A new seed pair without a test → fail. Complements `DB-TXN-01`. |
| `INT-MATRIX-03` | **Error envelope.** Call representative 400/401/404/409/412/422/429 error branches. | Always matches the `{error:{code,message,details[]}}` schema; `Content-Type: application/json`; no stack trace/SQL/secret leakage. |

## 11. Integration scenarios by domain

Legend: ☐ = not yet confirmed to have a test (see §15.1); 🔜 = cannot be written yet because the feature does not exist.

### 11.1 Admin & Auth (`pkgs/adminapi`, `pkgs/authz`, `pkgs/secrets`)

| ID | Scenario | Expected |
|---|---|---|
| `INT-ADM-01` | `GET /tenant/{host}/id` for a mapped host | 200, body is a bare UUID (plain), not JSON |
| `INT-ADM-02` | Host does not exist | 404; nginx treats it as a tenant-resolution error |
| `INT-ADM-03` | `GET /tenant/{host}/auth-config?name=default` | 200, shape `{auth:{id,jwt_mode,oidc_*,...}}`; non-sensitive fields come from the DB, secrets from env `LTP_TENANT_<SLUG>_*` |
| `INT-ADM-04` | Slug containing a hyphen (`acme-co`) | Env key resolves to `ACME_CO`; the secret belongs to the right tenant, never bleeds into another tenant |
| `INT-ADM-05` | Missing env secret | 200 with an empty/null secret; log **WARN without the value**; no panic |
| `INT-ADM-06` | `name` does not exist | Clear 404, no secret leakage |
| `INT-ADM-07` | After migration 0007, the `config` of an old auth_profile contains sensitive keys | Keys are stripped; the auth-config response is still complete thanks to env |
| `INT-ADM-08` | Issue token `POST /users/{uid}/tokens` | 201, plaintext `abt_…` returned **once**; DB only holds `token_hash` + `prefix` (8 chars) + `token_encrypted` ≠ plaintext |
| `INT-ADM-09` | `GET /users/{uid}/tokens` | Metadata is complete, with **no** plaintext/hash/encrypted |
| `INT-ADM-10` | Introspect a valid token | `{active:true,user_id,tenant_id,roles,is_admin}` matching the DB |
| `INT-ADM-11` | Introspect: wrong token / expired / already revoked / same prefix but different hash | `{active:false}`; the caller cannot tell the reasons apart |
| `INT-ADM-12` | Revoke `POST /tokens/{id}/revoke` then introspect immediately | Inactive immediately (independent of the controller cache); audit written |
| `INT-ADM-13` | Reveal: self / admin in same tenant / admin in another tenant / another user | 200 with plaintext matching the issued token / 200 / 404 / 404; each reveal produces one `tbl_audit_log` row |
| `INT-ADM-14` | Reveal when `LTP_MASTER_KEY` is not configured | 503; no configuration information leaked |
| `INT-ADM-15` | Issue a token for a user in another tenant (tenant taken from the target user's DB row, not from the header) | 404/403; no row created (regression for the hole caught by `API-AUTHZ-03`) |
| `INT-ADM-16` | Any admin mutation | Exactly one `tbl_audit_log` row with `actor` JSONB = principal |
| `INT-AUTHZ-01` | `RequirePrincipalWithRole`: header `X-User-Role: admin` but the DB has no admin role | Must not be treated as admin (spoofing blocked) |
| `INT-AUTHZ-02` | User's admin role removed between two requests | The next request loses the privilege immediately (role read from DB, no long cache) |
| `INT-SEC-01` | Crypto: `Encrypt/Decrypt` roundtrip | Blob = `nonce(12)‖ciphertext`; encrypting the same plaintext twice yields different blobs |
| `INT-SEC-02` | Wrong master key / blob with one byte modified / blob shorter than 12 bytes | Decrypt returns an error, no panic, no garbage plaintext |
| `INT-SEC-03` | `LoadMasterKey` with a hex key of the wrong length/invalid | Clear startup error |

### 11.2 Namespace lifecycle (`pkgs/namespace`, `pkgs/store`)

**CRUD, idempotency, concurrency**

| ID | Scenario | Expected |
|---|---|---|
| `INT-NS-01` | `POST /namespaces` without `Idempotency-Key` | 4xx per contract (confirm 400), no row created |
| `INT-NS-02` | Valid create | 201, `status=DRAFT`, `owner_user_id` = `X-User-Id`; `tenant_id`/`owner_user_id` in the body are **ignored**; 1 initialization event |
| `INT-NS-03` | Replay with the same key + same payload | Same status/body as the first call; exactly 1 row (especially when the first call returned 202). Related to `API-IDEM-01` |
| `INT-NS-04` | Same key, different payload | 409 (per policy), the second payload is not applied. Related to `API-IDEM-02` |
| `INT-NS-05` | Duplicate `(tenant_id,name)` with a different key | 409; a different tenant with the same name → OK |
| `INT-NS-06` | `GET` returns `ETag`; `PATCH` with the correct `If-Match` | 200, `version+1`, new ETag |
| `INT-NS-07` | `PATCH` with a stale `If-Match` | 412, no data change. Related to `API-CONC-01` |
| `INT-NS-08` | `GET /namespaces` pagination (`page`,`limit`,`total`,`data`), filters `status`, `ready_for_loadtest`, `q` | Correct total/page; a regular user only sees their own |
| `INT-NS-09` | `owner=` filter | Admin can use it within the tenant; regular user: ignored or 403 per contract |
| `INT-NS-10` | Admin views/edits a namespace owned by another owner in the same tenant | 200 + cross-owner audit; admin in another tenant → 404 |
| `INT-NS-11` | `GET /events` | Transition history in the right order, append-only, with actor + reason |
| `INT-NS-12` | `DELETE` a namespace with no runs | Soft delete → `DELETED` (via `fn_namespace_transition`), row remains, subsequent `GET` returns 404 |
| `INT-NS-13` | `DELETE` while an active run exists | 409 |
| `INT-NS-14` | After `DELETED`: recreate with the same name | **409 `NAME_CONFLICT`** (the DELETED row remains, UNIQUE(tenant,name)) — locked by `TestINT_NS_14_DeletedReuse` (plans/22) |

**Validate → Apply → Ready**

| ID | Scenario | Expected |
|---|---|---|
| `INT-NS-20` | `validate` a namespace with no services | Per contract (INVALID or allow); locked by a test |
| `INT-NS-21` | `validate` when a service lacks its OpenAPI | `DRAFT→VALIDATING→INVALID`, 422 + checklist (component, path, reason) |
| `INT-NS-22` | Valid `validate` | `→VALIDATING→APPLYING` |
| `INT-NS-23` | `apply` from an unvalidated `DRAFT` / from `DELETED` / `DELETING` | 409 (illegal transition), status unchanged |
| `INT-NS-24` | **Legacy mode** (no cluster): `apply` | `APPLYING→DATA_LOADING→READY` synchronously, `ready_for_loadtest=true`, network_policies already resolved from dependencies |
| `INT-NS-25` | **K8S_ENABLED**: `apply` | 202 `APPLYING`; only `desired_generation++`; **does not** write `READY` (only the Watcher does) |
| `INT-NS-26` | `apply` with an `Idempotency-Key` replay | Does not bump `desired_generation` a second time |
| `INT-NS-27` | Edit a service spec while the namespace is `READY` | `ready_for_loadtest=false`, takes the revalidate branch (`→VALIDATING` / `DEGRADED→VALIDATING`) |
| `INT-NS-28` | Edit a service but the spec content is unchanged (same `spec_hash`) | `unchanged`, ready is **not** reset, generation is not bumped |
| `INT-NS-29` | `GET /readiness` in legacy mode | Computed on the fly: services, policies, `mock_capacity` hook |
| `INT-NS-30` | `GET /readiness` in K8s mode with a populated projection | Reads `tbl_component_status` + `blocking_reasons`, does not recompute |
| `INT-NS-31` | `GET /readiness` in K8s mode with an empty projection | Falls back to `readinessGates` (no 500, no fake READY) |
| `INT-NS-32` | `GET /dependencies`, `/network-policies` | Graph already resolved; per-service policy matches `dependencies[]`; default-deny present |
| `INT-NS-33` | Create-with-spec with a circular dependency (A→B→A) | 422 `dependency cycle: a -> b -> a` (DAG required); a valid DAG still returns 201 — `TestINT_NS_33_CircularDeps` |
| `INT-NS-07b` | Golden path in K8S mode: POST /apply → Reconciler → Watcher → `GET /readiness` ready:true | Runs on a real cluster (`tests/harness/watcher`, `//go:build harness_k8s`) — `TestWATCH07GoldenPath` |

### 11.3 Service, OpenAPI, Database, Dataset (`pkgs/serviceopenapi`, components)

| ID | Scenario | Expected |
|---|---|---|
| `INT-SVC-01` | Add a service with a valid OpenAPI 3.0 YAML | 201; `spec_hash`, `parsed_summary` present; `GET /apis` lists the right operations (`operationId` or method+path) |
| `INT-SVC-02` | The same spec as JSON | Parsed result equivalent to YAML |
| `INT-SVC-03` | Missing `openapi` | 422 |
| `INT-SVC-04` | Syntax error / semantic error (broken `$ref`, missing `info`) | 422 + `details` with positions; the service is **not** stored |
| `INT-SVC-05` | Spec without `paths` / without operations | Per contract (422 or allowed but with empty `targets`); locked by a test |
| `INT-SVC-06` | Operation without `operationId` | Still indexable by method+path |
| `INT-SVC-07` | Spec exceeds the size limit | 413/422 per guardrail, parser does not hang |
| `INT-SVC-08` | `PUT …/spec` with identical content | `unchanged`; different content → ready reset, `spec_hash` changes |
| `INT-SVC-09` | Dependency pointing to a non-existent `service_id` | 422 |
| `INT-SVC-10` | `database_bindings` pointing to a non-existent `database_id` | 422 |
| `INT-SVC-11` | Delete a service that another service depends on / that has runs | 409 |
| `INT-SVC-12` | Circular dependency (A→B→A) | Per policy (allowed because it is a call graph, or 422); locked by a test, NetworkPolicy still correct |
| `INT-SVC-13` | Duplicate `service_id` in a namespace | 409; duplicate in another namespace → OK |
| `INT-SVC-14` | Ingress pointing to a non-existent service/port; conflicting host/path | 422 / 409 |
| `INT-DB-01` | Add a valid database (engine postgres, cnpg) | 201; a binding afterwards is OK |
| `INT-DB-02` 🔜 | `data_source.dataset_id` does not exist / dataset not `AVAILABLE` / different tenant / engine version mismatch | 422 (or 409) with a clear reason |
| `INT-DB-03` | `secret_ref` | Only the reference is stored; the response never contains the secret value |
| `INT-DB-04` 🔜 | `mock.enabled` + real provisioning both enabled (ambiguous) | 422 (dual-active not allowed) — waiting on mock-service |
| `INT-DS-01` 🔜 | Create dataset (`Idempotency-Key`), `pii_classification` ∈ {masked, synthetic, non_prod_raw} | 201/202; any other value → 422/CHECK |
| `INT-DS-02` 🔜 | Dataset: owner owns it, admin in the same tenant views/suspends, admin **does not** become owner | As per `database-management-api.md` §10.3 |
| `INT-DS-03` 🔜 | Delete a dataset that a namespace references | 409 |
| `INT-DS-04` 🔜 | Location outside the approved non-prod bucket / arbitrary URL | 422 (SSRF-safe). Related to `EXT-NET-02` |

### 11.4 Script & Loadtest (`pkgs/script`, `pkgs/loadtest`, `pkgs/runner`)

**Script**

| ID | Scenario | Expected |
|---|---|---|
| `INT-SCR-01` | Upload `ok.js` ≤1MiB | `PENDING_VALIDATION`; worker moves it `VALIDATING→VALIDATED` (Eventually); `content_hash` correct |
| `INT-SCR-02` | `oversize_1mib.js` (>1MiB) | 413/422 at the API; CHECK `size_bytes ≤ 1MiB` is the second layer of defence |
| `INT-SCR-03` | `syntax_error.js` | `INVALID`; `GET …/validations` has `attempt=1`, `errors` JSONB contains the line/reason |
| `INT-SCR-04` | `POST …/validate` (revalidate) from `INVALID` | `→VALIDATING`, `attempt=2`, the previous result is kept in history |
| `INT-SCR-05` | Revalidate from `VALIDATED` (migration 0009) | Allowed; the previous state is not lost if validation fails at the infrastructure level |
| `INT-SCR-06` | Validator infrastructure failure (`FAKE_K6_MODE=inspect_fail` / missing binary) | `VALIDATION_FAILED` (distinct from `INVALID`), not counted as a bad script |
| `INT-SCR-07` | Queue worker full | 429 `QUOTA_EXCEEDED`, script status **unchanged** (= `STATE-QUEUE-01`, regression plan-16 High-2) |
| `INT-SCR-08` | Script belonging to another namespace or another tenant | 404 |
| `INT-SCR-09` | Path traversal when reading a staged script (`../`, symlink, `\x00`) | Blocked, nothing is read outside the staging directory |
| `INT-SCR-10` | Validate the same script concurrently | Only one attempt runs, no duplicate attempt created |

**Loadtest create (gates).** Common setup: namespace `READY` (legacy apply or fake K8s + Watcher), script `VALIDATED`, service with the `orders_valid.yaml` OpenAPI.

| ID | Scenario | Expected |
|---|---|---|
| `INT-LT-01` | Happy path: valid targets + VALIDATED script + load_config | 202 + `status_url`; run `QUEUED/PROVISIONING→RUNNING`; transition inside the tx; `Runner.Start` called exactly once with the right args/labels |
| `INT-LT-02` | Namespace not yet READY (DRAFT/APPLYING/DEGRADED/INVALID) | 409 `NAMESPACE_NOT_READY` + readiness details; **no** run created |
| `INT-LT-03` | `ready_for_loadtest=true` but `status_observed_at` older than `staleness_threshold` (fake clock) | 409 `NAMESPACE_NOT_READY` with reason `STATUS_STALE` (same gate as `WATCH-05`) |
| `INT-LT-04` | Script not yet `VALIDATED` (PENDING/VALIDATING/INVALID/VALIDATION_FAILED) | 409 `SCRIPT_NOT_VALIDATED` |
| `INT-LT-05` | Empty `targets` | 422 |
| `INT-LT-06` | Target: service does not exist / `operation_id` does not exist / method+path does not match the spec | 422 + `details` identifying which target |
| `INT-LT-07` | Target by `operation_id` and by method+path for the same operation | Both resolve; stored in a normalized form |
| `INT-LT-08` | `load_config` exceeding guardrails (vusers/duration/timeout) | 422 or 429 per policy; `GET /guardrails` matches the actual limits |
| `INT-LT-09` | Namespace of another tenant / of another owner (regular user) | 404 |
| `INT-LT-10` | Admin creates a run on a namespace owned by another owner in the same tenant | Allowed if policy permits; run `owner_user_id` per contract; audit |
| `INT-LT-11` | `Idempotency-Key` replay | No second run, no second `Runner.Start` call |
| `INT-LT-12` 🔜 | Quota 429 (`tbl_reservation_ledger`) | 429 `QUOTA_EXCEEDED` — not wired yet (`mvp-status.md` §7.5) |

**Loadtest lifecycle & reconciler**

| ID | Scenario | Expected |
|---|---|---|
| `INT-LT-20` | Successful run (fake k6 `ok`) | `RUNNING→COMPLETED`; events contiguous, `seq` without gaps |
| `INT-LT-21` | Failed run (`exit_nonzero`) | `FAILED`, reason recorded in the event |
| `INT-LT-22` | `POST /stop` while RUNNING | `STOPPING→STOPPED`; fake process/TestRun stopped; a second stop is idempotent 2xx (`RUN-STOP-01`) |
| `INT-LT-23` | `stop` on a run already in a terminal state (COMPLETED/STOPPED) | Per policy (2xx no-op or 409); locked by a test |
| `INT-LT-24` | Exceeding `timeout_at` (fake clock) | `TIMED_OUT→TIMEOUT_CLEANED`; cleanup failure → `CLEANUP_FAILED`, reaper retries idempotently (`RUN-TIMEOUT-01`, `RUN-LEAK-01`) |
| `INT-LT-25` | Reconciler finds the runner gone (process no longer exists, DB still RUNNING) | `FAILED` (runner-lost, plan-16 High) |
| `INT-LT-26` | Reconciler restarts while a run is RUNNING | Keeps tracking via `runner_ref`, does not create a new run (`RUN-CRASH-01`) |
| `INT-LT-27` | Reconciler only changes status via `fn_loadtest_transition` | No direct `UPDATE … SET status` (test + CI grep for `Raw` outside `pkgs/store/transitions.go`) |
| `INT-LT-28` | `GET /loadtests` filters `namespace_id`, `status`; `owner` only for admin | Correct tenant/owner scope |
| `INT-LT-29` | Namespace becomes `DEGRADED` while a run is RUNNING | The run is **not** stopped; `namespace_degraded_at` event; `GET` + events return `unreliable:true` (plans/22) |
| `INT-LT-30` | Namespace `DEGRADED` **before** the run starts | Not `unreliable` (the flag only applies to runs started before the degrade) |
| `INT-LT-31` | `/logs` | Secrets already redacted; only owner/admin in the same tenant |

### 11.5 Env Reconciler & Status Watcher (fake K8s client)

Uses the `pkgs/k8s` fake (records calls in order, simulates status). Real-cluster cases are in §12.2.

**Env Reconciler (desired → K8s)**

| ID | Scenario | Expected |
|---|---|---|
| `INT-ENV-01` | Apply a namespace with all components | Call order: Namespace → NetworkPolicy → CNPG Cluster → restore Job → mock binding → Deployment/Service → Ingress (`k8s-orchestration.md` §4.1) |
| `INT-ENV-02` | Re-apply with the same `spec_hash` | No-op (resources are not patched again) |
| `INT-ENV-03` | Change one service | Only that `component-id`'s resources are re-applied |
| `INT-ENV-04` | Remove a service from the spec | Resources carrying that `component-id` label are pruned; resources **without** the managed label are untouched |
| `INT-ENV-05` | Every resource created | Has all 5 labels (`managed-by`, `tenant-id`, `namespace-id`, `component-id`, `component-kind`); `fieldManager=loadtest-controller` |
| `INT-ENV-06` | Rendered manifests | No plaintext secrets; DB creds via `secretKeyRef`; `automountServiceAccountToken=false`; a dedicated ServiceAccount per service |
| `INT-ENV-07` | NetworkPolicy before workloads | No Deployment-create call precedes default-deny |
| `INT-ENV-08` | Delete an Environment | Deletes the K8s Namespace `lt-env-<id>`; does not delete resources one by one |
| `INT-ENV-09` | Reconciler does **not** write readiness to PG | After apply, `tbl_namespaces.status` does not become READY unless the Watcher has run |
| `INT-ENV-10` | `deploy.image` using `:latest` in the prod profile | Rejected at validate |

**Reconciler → Watcher loop.** Watcher fault cases are `WATCH-01..06` (§2.3.1); below are the functional cases:

| ID | Scenario | Expected |
|---|---|---|
| `INT-LOOP-01` | Apply a namespace without `data_source`; fake K8s reports the Deployment `Available`, and Ingress/NetworkPolicy complete | `APPLYING→READY`, `ready_for_loadtest=true`, `blocking_reasons` empty, `tbl_component_status` fully populated; **after that** `POST /loadtests` succeeds |
| `INT-LOOP-02` 🔜 | With `data_source`: Cluster Ready → restore Job running | `APPLYING→DATA_LOADING`; loadtest gate still 409; Job `Complete` + verify → `READY` |
| `INT-LOOP-03` 🔜 | Restore Job `Failed` / past `activeDeadlineSeconds` | `DATA_LOADING→INVALID`, `blocking_reasons` contains `restore_job_failed` / `restore_timeout` |
| `INT-LOOP-04` | Deployment `ProgressDeadlineExceeded` while APPLYING | `→INVALID`/`DEGRADED` per the aggregator; reason `crash_loop`/`image_pull_backoff` |
| `INT-LOOP-05` | READY, one Deployment drops below `replicas` for > 60s grace (fake clock) | `READY→DEGRADED`, `ready_for_loadtest=false`; stable for > 60s → `READY` |
| `INT-LOOP-06` | Past `apply_deadline` in APPLYING/DATA_LOADING | `DEGRADED`, reason `apply_deadline_exceeded` |
| `INT-LOOP-07` | Edit the spec while READY (`desired_generation` increases) | The old component stays `PROGRESSING` until `observed_generation` matches; `/readiness` does not report READY early (`WATCH-03`) |
| `INT-LOOP-08` | `network_policy`: one policy missing / `spec_hash` out of sync (drift) | Phase `FAILED`/`PROGRESSING` + reason, namespace not READY |
| `INT-LOOP-09` | Delete an Environment | `READY→DELETING`; the K8s Namespace disappears → Watcher writes `DELETED` |
| `INT-LOOP-10` | The 4 extra transitions from migration 0011 (`APPLYING→READY`, `APPLYING→DEGRADED`, `DATA_LOADING→INVALID`, `DEGRADED→READY`) | Each is traversed at least once (complements `INT-MATRIX-02`) |

| `INT-LOOP-11` | Watcher writes `status_observed_at` on every reconcile/resync | Heartbeat advances; stopping the Watcher → see `WATCH-05` / `INT-LT-03` |
| `INT-LOOP-12` | Event for a GVR in another namespace (label other than `managed-by`) | Ignored, not enqueued |

### 11.6 Duplicate mapping between harness and `INT-*`

| `INT-*` | Corresponding harness | Where it lives |
|---|---|---|
| `INT-NS-03/04` | `API-IDEM-01/02` | Harness is the primary source; `INT-*` only locks the response contract |
| `INT-NS-07` | `API-CONC-01` | Harness |
| `INT-ADM-15`, `INT-MATRIX-01` | `API-AUTHZ-01..04` | `INT-MATRIX-01` extends to all routes |
| `INT-SCR-07` | `STATE-QUEUE-01` | Harness |
| `INT-LT-03`, `INT-LOOP-11` | `WATCH-05` | Harness |
| `INT-LT-22/24/25/26` | `RUN-STOP-01`, `RUN-TIMEOUT-01`, `RUN-LEAK-01`, `RUN-CRASH-01` | Harness (fault) / `INT-*` (lifecycle happy path) |
| `INT-NGX-01/07` | `NGX-01/02` | §12.1 implements them for real on compose |
| `INT-K8S-04` | `RUN-NET-02` | K8s harness |

## 12. Out-of-process integration: Nginx, real K8s, E2E, Perf, Migration

### 12.1 Nginx / trust boundary (`tests/nginx/`)
Brings up `docker compose`: nginx (`conf/nginx`) + controller + postgres + a fake OIDC provider (static JWKS, pre-signed tokens). This is the only place that exercises **real header injection** — all other layers only simulate headers.

| ID | Scenario | Expected |
|---|---|---|
| `INT-NGX-01` | Client sends forged `X-Tenant-Id`/`X-User-Id`/`X-User-Role: admin` together with a regular user's valid token | The controller receives headers **set by nginx**, not the forged values; no admin privileges |
| `INT-NGX-02` | Valid Bearer JWT | 200; `X-Tenant-Id`, `X-User-Id` correct; `X-User-Role` per the claim |
| `INT-NGX-03` | Expired JWT / bad signature / wrong issuer | 401 at nginx, the request never reaches the controller |
| `INT-NGX-04` | `abt_` Bearer (introspected through the controller) | 200; `X-User-Id` from introspection; after revoke: 401 only after the cache TTL at most (~60s) — the test uses a short cache TTL |
| `INT-NGX-05` | **Role propagation gap** (known): `abt_` and OIDC sessions do not set `X-User-Role` | **Locks current behaviour**: admins are still normalized from the DB (`tbl_user_roles`), regular users do not become admin; update the test once nginx is aligned (`NGX-03`) |
| `INT-NGX-06` | `/api/admin/v1/tenant/*`, `/token/introspect` from an external client | Not reachable over the public channel (only via the trusted nginx channel) |
| `INT-NGX-07` | Controller upstream slow/down while nginx calls auth-config/introspect | Fails safe (no anonymous entry); cache is TTL-bounded |
| `INT-NGX-08` | `auth-config` cached for 600s; rotate the secret | Correct behaviour after the cache expires |
| `INT-NGX-09` | Method allowlist | `DELETE`/`OPTIONS` work for the routes that need them; unknown methods are blocked |

### 12.2 K8s harness (`//go:build harness_k8s`, kind/colima)
Kept in `tests/harness/k8s/`; besides `WATCH-01..06` and `RUN-NET-02`, it adds functional cases on a real cluster:

| ID | Scenario | Expected |
|---|---|---|
| `INT-K8S-01` | Real SSA apply: Namespace, Deployment, Service, Ingress, NetworkPolicy | Idempotent on re-apply; managed fields owned by `loadtest-controller` |
| `INT-K8S-02` | CNPG `Cluster` CR | Requires the operator + CRDs installed; Watcher reads the `Ready` condition correctly |
| `INT-K8S-03` | Real informer: delete a Deployment manually (drift) | Watcher detects it, namespace → `DEGRADED` after grace |
| `INT-K8S-04` | Default-deny + per-service policy | Runner pods cannot reach destinations outside the list (`RUN-NET-02`); the SUT can reach its declared dependencies |
| `INT-K8S-05` 🔜 | Real k6-operator `TestRun` | Requires the K8s runner (currently `newRunner()` always returns Local); `status.stage` → `fn_loadtest_transition` |
| `INT-K8S-06` | Controller ServiceAccount RBAC | Every verb in the RBAC table of `k8s-orchestration.md` §6 works; verbs outside the table are denied |

### 12.3 E2E smoke (`tests/e2e-smoke.sh`)
Currently PASSING: seed → namespace → service → validate → apply → script → loadtest → events.

| ID | Scenario | Environment |
|---|---|---|
| `E2E-01` | Main flow as above (legacy mode, Local runner) | `docker compose` |
| `E2E-02` | Main flow + negative paths: namespace not READY → loadtest 409; INVALID script → 409 | `docker compose` |
| `E2E-03` | Through nginx with an `abt_` token (issue → use → revoke → blocked) | compose + nginx |
| `E2E-04` 🔜 | K8s mode on kind: real apply → READY (Watcher) → run via the K8s runner | kind, once the K8s runner exists |

### 12.4 E2E full deploy (`tests/e2e-full-deploy.sh`)
End-to-end **K8s-mode** flow with real cluster (kind), CNPG operator, and Env Reconciler + Status Watcher loops. Verifies full production path.

| ID | Scenario | Environment |
|---|---|---|
| `E2E-FD-01` | **Full K8s deploy**: kind cluster → CNPG operator install → control-plane Postgres → controller (K8S_ENABLED=true) → namespace with database (CNPG) + service (echo-server) → validate → apply (202) → Env Reconciler applies → Status Watcher projects → READY → port-forward curl echo-server → delete namespace → verify K8s ns gone | kind + CNPG |
| `E2E-FD-02` | **Negative**: invalid spec → validate returns 422 | kind |
| `E2E-FD-03` | **HA simulation**: controller pod kill mid-apply → verify convergence on resync | kind |
| `E2E-FD-04` | **Full load-test lifecycle**: namespace READY → upload k6 script → poll `VALIDATED` → `POST /loadtests` (201) → assert run-scoped NetworkPolicy (`loadtest-run-{egress,ingress}-<id>`, `component-kind=run-policy`, egress `ports:[80]`, target selector = service name, 6565 rule) → poll `COMPLETED` → `started_at`/`finished_at` + events + `unreliable` | kind + CNPG + k6-operator |
| `E2E-FD-05` | **L3/L4 probe**: busybox pod labeled `loadtest.io/run-id=<id>`: runner→echo (port 80) ✅; runner→demo-db (5432) ❌; undeclared internet/metadata ❌ | kind + Calico |
| `E2E-FD-06` | **L3/L4 probe negative**: other-service→demo-db (5432) ❌ (no `database_bindings`); bound-service→demo-db ✅ (binding present) | kind + Calico |
| `E2E-FD-07` | **Cleanup**: `DELETE /namespaces/{id}` → 202 DELETING → K8s ns gone → run-scoped NetworkPolicy + ConfigMap gone | kind + CNPG |
| `E2E-FD-08` | **Script gate**: upload a script whose content starts with a NUL byte → validator marks it `INVALID`; `POST /loadtests` with that script → 409 `SCRIPT_NOT_VALIDATED` (independent of async validation timing) | kind |

**Prerequisites**: `kind`, `kubectl`, `docker`, `curl`, `python3`. **k6-operator** (`bundle.yaml`, pinned tag) + `grafana/k6` image must be available in kind (the install script lives in `tests/e2e-full-deploy.sh`). The controller runs with `RUNNER_MODE=k8s` for cases 04–07. Uses `KEEP_CLUSTER=1` to retain cluster for debugging.

**Flow** (from script):
1. Bootstrap kind cluster + pre-load images (CNPG postgres, echo-server, `grafana/k6`)
2. Install CNPG operator (`kubectl apply --server-side`)
3. Install k6-operator (`bundle.yaml`); wait for CRDs established + `k6-operator-controller-manager` ready
4. Start control-plane Postgres (Docker) + run migrations (`sql/*.sql`)
5. Build & start `loadtest-controller` with `K8S_ENABLED=true`, `RUNNER_MODE=k8s`, `K8S_API_URL` pointing to kind
6. Seed tenant/user fixture in PG
7. Create namespace with full spec (database: CNPG postgres:16 + service: echo-server)
8. `POST /validate` → `POST /apply` (202, async hand-off)
9. Poll `/readiness` (up to 12m — CNPG bootstrap)
10. Verify: CNPG Cluster Ready, Deployment Available, `tbl_component_status` projection
11. Port-forward `echo` service → curl returns JSON
12. Upload k6 script → poll `VALIDATED` → `POST /loadtests` → assert run-scoped NetworkPolicy
13. Poll loadtest `COMPLETED` → `started_at`/`finished_at` + events + `unreliable`
14. L3/L4 probes (runner→echo ✅, runner→DB ❌, other-service→DB ❌, bound-service→DB ✅)
15. `DELETE /namespaces/{id}` → 202 DELETING
16. Env Reconciler deletes K8s namespace `lt-env-<id>` → verify gone + run policies gone

**Run**: `./tests/e2e-full-deploy.sh` (or `KEEP_CLUSTER=1 ./tests/e2e-full-deploy.sh` to keep kind cluster)

### 12.5 Perf (`tests/perf/`)
Thresholds are already defined: `control-plane.js` (40 rps reads, p95 < 100ms), `admin.js` (80 rps introspect). **Never run yet** because k6 is not installed (`mvp-status.md` §7.2).

| ID | Work to do |
|---|---|
| `PERF-01` | Install k6, run both scripts against the compose stack, **record the baseline in the README** (p50/p95/p99, error rate, machine used) |
| `PERF-02` | Add a script for `/readiness` (projection-backed) and the `POST /loadtests` gate to catch JOIN-query regressions |
| `PERF-03` | Wire into CI/nightly with thresholds = baseline × the agreed tolerance factor |

### 12.6 Migration (owned by `pkgs/store`)
| ID | Scenario | Expected |
|---|---|---|
| `INT-MIG-01` | Migrate from an empty DB 0001→0011 | Succeeds; `tbl_schema_migrations` has all records, in the right order |
| `INT-MIG-02` | Run the runner again on an already-migrated DB | No-op, no error |
| `INT-MIG-03` | Upgrade from an old snapshot with data (before 0007) | Secrets are stripped, other data intact; old tokens can still be introspected |
| `INT-MIG-04` | Two instances run migrations concurrently | Only one wins (advisory lock), no double apply |
| `INT-MIG-05` | An applied migration file's content is edited (checksum mismatch) | Runner refuses, reports clearly |

## 13. Cross-references & quick ID index

Quick lookup table for IDs referenced across part I (harness, §1–§8) and part II (functional integration, §9–§15), so links do not break when new sections are added.

| ID | Meaning | Defined in |
|---|---|---|
| `DB-TXN-*` | SQL/metadata integrity faults | §2.1 |
| `API-*` | API/application integrity harness | §2.2 |
| `RUN-*` | Runner/K8s/reconciliation harness | §2.3 |
| `WATCH-01..06` | Namespace status watcher harness | §2.3.1 |
| `EXT-*/MOCK-*` | External dependency / data-plane faults | §2.4 |
| `NGX-*` | Nginx trust-boundary harness | §2.5 |
| `INT-ADM-*` | Admin & Auth integration | §11.1 |
| `INT-NS-*` | Namespace lifecycle integration | §11.2 |
| `INT-SVC-*`/`INT-DB-*`/`INT-DS-*` | Service/OpenAPI/Database/Dataset integration | §11.3 |
| `INT-SCR-*`/`INT-LT-*` | Script & Loadtest integration | §11.4 |
| `INT-ENV-*`/`INT-LOOP-*` | Env Reconciler & Status Watcher | §11.5 |
| `INT-MATRIX-01/02/03` | Cross-cutting matrix (route×principal, transitions, error envelope) | §10 |
| `INT-K8S-*` | K8s harness (cluster) | §12.2 |
| `INT-NGX-*` | Nginx integration (compose) | §12.1 |
| `E2E-*` | E2E smoke | §12.3 |
| `E2E-FD-*` | E2E full deploy | §12.4 |
| `PERF-*` | Perf | §12.5 |
| `INT-MIG-*` | Migration | §12.6 |

## 14. Running tests & CI

```bash
# Unit + integration + harness (requires Postgres 16)
docker compose up -d postgres
export TEST_DATABASE_DSN='postgres://…'
go test -race ./...                         # integration self-skips without a DSN (fails when CI=true)

# SQL
for t in tests/sql/*_test.sql; do psql "$TEST_DATABASE_DSN" -v ON_ERROR_STOP=1 -f "$t"; done

# A single scenario by ID
go test -race ./... -run 'TestINT_LT_03'

# K8s harness (requires kind/colima + CRDs)
go test -tags harness_k8s ./tests/harness/k8s/...

# E2E
docker compose up -d --build && ./tests/e2e-smoke.sh
```

| CI job | Runs | Notes |
|---|---|---|
| `lint` | golangci-lint v2 | |
| `test` | unit + integration (`INT-*`) + harness (fake K8s), `-race`, Postgres service | `CI=true` ⇒ skipping is not allowed |
| `sql-migrations` | migrate from empty + `tests/sql/*` | |
| `build` | image | |
| `watcher-harness` | `harness_k8s` on kind | |
| *(proposed)* `nginx-integration` | `tests/nginx/` on compose | mandatory for PRs touching `conf/nginx` |
| *(proposed)* `e2e` | `tests/e2e-smoke.sh` | |
| *(proposed, nightly)* `perf`, `k6-real` | k6 perf + Local runner with real k6 | |

## 15. Known gaps & coverage tracking

| Gap | Test impact | Action |
|---|---|---|
| K8s runner does not exist (`newRunner()` always Local) | `INT-K8S-05`, `E2E-04` | Write together with the K8s runner plan |
| k6 perf has never run | `PERF-01..03` | Install k6, run, record baseline |
| Outbox worker has no dispatcher | `API-OUTBOX-01/02` | Write together with the worker |
| Quota gate 429 not wired into loadtest | `INT-LT-12`, app side of `DB-TXN-07` | Write together with the feature |
| mock-service endpoints are still stubs | `INT-DB-04`, mock readiness, `MOCK-*` | Write once the contract is settled |
| Dataset/restore not implemented | `INT-DS-*`, `INT-DB-02`, `INT-LOOP-02/03` | Write together with the feature |
| Nginx role propagation not yet consistent | `INT-NGX-05` only locks current behaviour | Update when `conf/nginx` is adjusted |
| Edge behaviours not yet decided (empty namespace, circular dependency, stop on a terminal run, recreating a name after `DELETED`, status when `Idempotency-Key` is missing) | The "locked by a test" cases in §11 | Decide, document in the matching API doc, then replace the expectation with a concrete value |

### 15.1 Tracking which IDs already have tests
This catalogue has not yet been reconciled with the existing test files. The cheapest way to sync:

1. Name tests per `TestINT_<DOMAIN>_<NN>_…` (§9.6); for old tests, rename or add a `// covers: INT-NS-06` comment.
2. Add `scripts/test-coverage-map.sh`: `grep` the IDs in `docs/harness-testing.md` and in `*_test.go`, print IDs **without a test** and **orphan** IDs (present in code but not in the doc).
3. Run it in CI in warn mode; switch to failing once the catalogue is mostly covered.

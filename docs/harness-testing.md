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

# Phần II — Functional integration tests

Các mục §9–§15 bên dưới là catalogue kịch bản **functional integration** (viết bằng tiếng Việt, đồng bộ với `mvp-status.md` và `k8s-orchestration.md`). Số mục §1–§8 phía trên được giữ nguyên để không làm gãy tham chiếu từ các tài liệu khác (`k8s-orchestration.md`, `namespace-status-watcher.md`, `architecture.md`).

## 9. Nguyên tắc & hạ tầng test dùng chung

### 9.1 Quy tắc chọn tầng
- Logic thuần không cần I/O → **unit**.
- Có SQL, transaction, constraint, JOIN, hoặc cần verify trạng thái DB sau request → **integration**. Không mock DB ở tầng này; mock DB làm mất đúng thứ cần kiểm chứng (transition function, CHECK, FK).
- K8s: mặc định **fake client** (`pkgs/k8s` fake, có fault injection). Chỉ lên kind khi cần hành vi cluster thật (NetworkPolicy enforcement, CRD, informer thật).
- k6 binary: mặc định **fake k6** (script shell trong `PATH`) cho test deterministic; k6 thật chỉ ở nightly/e2e.

### 9.2 Composition
Mọi integration test (cả harness lẫn `INT-*`) đi qua **đúng composition root của server** (`pkgs/testutil.NewServer`) — không tự dựng router/middleware, không tái tạo auth (plans/16). Test cần cấu hình khác (`K8S_ENABLED`, master key, runner) truyền qua option của `NewServer`, không fork wiring.

```go
srv := testutil.NewServer(t, testutil.WithK8s(fakeK8s), testutil.WithRunner(fakeRunner),
                          testutil.WithClock(clk), testutil.WithMasterKey(key))
```

### 9.3 Fixtures danh tính
Mỗi test tự tạo dữ liệu qua helper, không dùng dữ liệu chung giữa test:

| Fixture | Mô tả |
|---|---|
| `tenantA`, `tenantB` | Hai tenant, slug ngẫu nhiên theo test (`t-<rand>`) |
| `ownerA` | user role `user`, tenant A |
| `otherA` | user role `user`, tenant A (không phải owner) |
| `adminA` | role `admin`, tenant A — role **ghi trong `tbl_user_roles`** (không chỉ header) |
| `adminB` | role `admin`, tenant B |
| `abtToken(user)` | token `abt_` thật qua API issue |

Helper `srv.As(user).Do(req)` set trusted headers (`X-Tenant-Id`, `X-User-Id`, `X-User-Role`) giống nginx; `srv.AsRaw(headers)` để test header giả/thiếu.

### 9.4 Cô lập dữ liệu & thời gian
- Migration chạy **một lần mỗi package** (`TestMain`) bằng migration runner thật.
- Mỗi test dùng tenant riêng → `t.Parallel()` được, không cần truncate. Test cần trạng thái toàn cục (reconciler quét toàn bảng, reaper) dùng **DB/schema riêng** (`testutil.NewIsolatedDB`) và không parallel.
- Không dùng `time.Sleep`. Chờ trạng thái bất đồng bộ bằng `testutil.Eventually(t, 5*time.Second, cond)`; thời gian (TTL, timeout, grace/stable window, staleness) điều khiển bằng **fake clock** (§4).

### 9.5 Fixtures dữ liệu
```text
tests/fixtures/
  openapi/   orders_valid.yaml  orders_valid.json  payment_valid.yaml
             invalid_syntax.yaml  invalid_semantic.yaml  no_paths.yaml
             no_operation_id.yaml  oversize.yaml
  k6/        ok.js  syntax_error.js  import_forbidden.js  slow_30s.js  exit_nonzero.js  oversize_1mib.js
  fakebin/   k6    # shell stub: hành vi chọn bằng env FAKE_K6_MODE=ok|inspect_fail|exit1|hang
```

### 9.6 Quy ước viết test
- Tên: `TestINT_<DOMAIN>_<NN>_<MoTa>` — ID khớp bảng ở §10–§12 để grep được. Test harness giữ ID gốc của §2 (`TestAPIAUTHZ03…`, `TestAPIIDEM01…`).
- Mỗi test integration assert **ba lớp** khi có thể: (1) HTTP status + error envelope `{error:{code,message,details}}`; (2) trạng thái DB (`status`, `version`); (3) event trail (`tbl_*_events`) đúng thứ tự, đúng actor.
- Table-driven cho các biến thể (role × route, định dạng OpenAPI…).
- Assert **mã lỗi ổn định** (`NAMESPACE_NOT_READY`, `SCRIPT_NOT_VALIDATED`, `QUOTA_EXCEEDED`, `STATUS_STALE`), không assert chuỗi message.
- Không log/assert plaintext secret; test secret kiểm tra **sự vắng mặt** của giá trị trong DB, log, response.
- Local không có DB → `t.Skip`; `CI=true` → **fail cứng** (đã có, giữ nguyên).

## 10. Ma trận kiểm soát chéo (viết một lần, chạy cho mọi route)

Ba test "sinh tự động" bắt lỗi quên test khi thêm route/transition:

| ID | Nội dung | Cách làm / Expected |
|---|---|---|
| `INT-MATRIX-01` | **Route × principal.** Dùng `chi.Walk` liệt kê mọi route của `/api/loadtest/v1` và `/api/admin/v1` (operator). Với mỗi route chạy 5 principal: không header, ownerA (resource của ownerA), otherA, adminA, adminB. | Không header → 401; otherA/adminB → 404 (không 403, chống leak); ownerA/adminA → không 401/404. Route mới chưa có trong bảng kỳ vọng → test fail ("route chưa khai báo policy"). Mở rộng từ `API-AUTHZ-01..04`. |
| `INT-MATRIX-02` | **Transition coverage.** Đọc `tbl_status_transitions` (seed) cho từng `object_type`. | Mỗi cặp `allowed=true` có ít nhất một test đi qua (qua API hoặc `fn_*_transition`); mỗi cặp ngoài map bị từ chối (409/exception). Cặp seed mới chưa có test → fail. Bổ sung cho `DB-TXN-01`. |
| `INT-MATRIX-03` | **Error envelope.** Gọi nhánh lỗi 400/401/404/409/412/422/429 đại diện. | Luôn đúng schema `{error:{code,message,details[]}}`; `Content-Type: application/json`; không lộ stack trace/SQL/secret. |

## 11. Kịch bản integration theo domain

Ký hiệu: ☐ = chưa xác nhận đã có test (xem §15.1); 🔜 = chưa viết được vì tính năng chưa có.

### 11.1 Admin & Auth (`pkgs/adminapi`, `pkgs/authz`, `pkgs/secrets`)

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-ADM-01` | `GET /tenant/{host}/id` host đã map | 200, body là UUID thuần (plain), không JSON |
| `INT-ADM-02` | Host không tồn tại | 404; nginx coi là lỗi resolve tenant |
| `INT-ADM-03` | `GET /tenant/{host}/auth-config?name=default` | 200, shape `{auth:{id,jwt_mode,oidc_*,...}}`; field không nhạy cảm từ DB, secret từ env `LTP_TENANT_<SLUG>_*` |
| `INT-ADM-04` | Slug có dấu gạch (`acme-co`) | Env key resolve `ACME_CO`; secret đúng tenant, không lẫn sang tenant khác |
| `INT-ADM-05` | Thiếu env secret | 200 với secret rỗng/null; log **WARN không chứa giá trị**; không panic |
| `INT-ADM-06` | `name` không tồn tại | 404 rõ ràng, không lộ secret |
| `INT-ADM-07` | Sau migration 0007, `config` của auth_profile cũ có key nhạy cảm | Key bị strip; response auth-config vẫn đầy đủ nhờ env |
| `INT-ADM-08` | Issue token `POST /users/{uid}/tokens` | 201, plaintext `abt_…` **một lần**; DB chỉ có `token_hash` + `prefix` (8 ký tự) + `token_encrypted` ≠ plaintext |
| `INT-ADM-09` | `GET /users/{uid}/tokens` | Metadata đủ, **không** có plaintext/hash/encrypted |
| `INT-ADM-10` | Introspect token hợp lệ | `{active:true,user_id,tenant_id,roles,is_admin}` đúng với DB |
| `INT-ADM-11` | Introspect: sai token / hết hạn / đã revoke / prefix trùng nhưng hash khác | `{active:false}`; không phân biệt lý do cho caller |
| `INT-ADM-12` | Revoke `POST /tokens/{id}/revoke` rồi introspect ngay | Inactive ngay (không phụ thuộc cache controller); audit ghi |
| `INT-ADM-13` | Reveal: self / admin cùng tenant / admin khác tenant / user khác | 200 plaintext khớp token đã issue / 200 / 404 / 404; mỗi lần reveal có 1 dòng `tbl_audit_log` |
| `INT-ADM-14` | Reveal khi chưa cấu hình `LTP_MASTER_KEY` | 503; không rò thông tin cấu hình |
| `INT-ADM-15` | Issue token cho user của tenant khác (tenant lấy từ DB của target user, không từ header) | 404/403; không tạo row (regression của lỗ hổng bắt bởi `API-AUTHZ-03`) |
| `INT-ADM-16` | Mọi mutation admin | Đúng 1 dòng `tbl_audit_log` với `actor` JSONB = principal |
| `INT-AUTHZ-01` | `RequirePrincipalWithRole`: header `X-User-Role: admin` nhưng DB không có role admin | Không được coi là admin (spoof bị chặn) |
| `INT-AUTHZ-02` | User bị gỡ role admin giữa hai request | Request sau mất quyền ngay (role đọc DB, không cache dài) |
| `INT-SEC-01` | Crypto: roundtrip `Encrypt/Decrypt` | Blob = `nonce(12)‖ciphertext`; hai lần encrypt cùng plaintext cho blob khác nhau |
| `INT-SEC-02` | Sai master key / blob bị sửa 1 byte / blob ngắn hơn 12 byte | Decrypt trả lỗi, không panic, không trả plaintext rác |
| `INT-SEC-03` | `LoadMasterKey` với hex sai độ dài/không hợp lệ | Lỗi khởi động rõ ràng |

### 11.2 Namespace lifecycle (`pkgs/namespace`, `pkgs/store`)

**CRUD, idempotency, concurrency**

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-NS-01` | `POST /namespaces` thiếu `Idempotency-Key` | 4xx theo contract (xác nhận 400), không tạo row |
| `INT-NS-02` | Create hợp lệ | 201, `status=DRAFT`, `owner_user_id` = `X-User-Id`; `tenant_id`/`owner_user_id` trong body bị **bỏ qua**; 1 event khởi tạo |
| `INT-NS-03` | Replay cùng key + cùng payload | Cùng status/body với lần đầu; đúng 1 row (đặc biệt khi lần đầu trả 202). Liên quan `API-IDEM-01` |
| `INT-NS-04` | Cùng key, payload khác | 409 (theo policy), không áp payload thứ hai. Liên quan `API-IDEM-02` |
| `INT-NS-05` | Trùng `(tenant_id,name)` với key khác | 409; tenant khác cùng name → OK |
| `INT-NS-06` | `GET` trả `ETag`; `PATCH` với `If-Match` đúng | 200, `version+1`, ETag mới |
| `INT-NS-07` | `PATCH` `If-Match` cũ | 412, không đổi dữ liệu. Liên quan `API-CONC-01` |
| `INT-NS-08` | `GET /namespaces` phân trang (`page`,`limit`,`total`,`data`), filter `status`, `ready_for_loadtest`, `q` | Đúng tổng/trang; user thường chỉ thấy của mình |
| `INT-NS-09` | Filter `owner=` | Admin dùng được trong tenant; user thường: bị bỏ qua hoặc 403 theo contract |
| `INT-NS-10` | Admin xem/sửa namespace của owner cùng tenant | 200 + audit cross-owner; admin tenant khác → 404 |
| `INT-NS-11` | `GET /events` | Lịch sử transition đúng thứ tự, append-only, có actor + reason |
| `INT-NS-12` | `DELETE` namespace không có run | Soft delete → `DELETED` (qua `fn_namespace_transition`), row còn, `GET` sau đó 404 |
| `INT-NS-13` | `DELETE` khi còn run active | 409 |
| `INT-NS-14` | Sau `DELETED`: tạo lại cùng name | **409 `NAME_CONFLICT`** (row DELETED còn, UNIQUE(tenant,name)) — khóa bằng `TestINT_NS_14_DeletedReuse` (plans/22) |

**Validate → Apply → Ready**

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-NS-20` | `validate` namespace không có service nào | Theo contract (INVALID hoặc cho qua); khóa bằng test |
| `INT-NS-21` | `validate` khi có service thiếu OpenAPI | `DRAFT→VALIDATING→INVALID`, 422 + checklist (component, path, reason) |
| `INT-NS-22` | `validate` hợp lệ | `→VALIDATING→APPLYING` |
| `INT-NS-23` | `apply` từ `DRAFT` chưa validate / từ `DELETED` / `DELETING` | 409 (transition không hợp lệ), không đổi trạng thái |
| `INT-NS-24` | **Legacy mode** (không cluster): `apply` | `APPLYING→DATA_LOADING→READY` đồng bộ, `ready_for_loadtest=true`, network_policies đã resolve từ dependencies |
| `INT-NS-25` | **K8S_ENABLED**: `apply` | 202 `APPLYING`; chỉ `desired_generation++`; **không** ghi `READY` (Watcher mới ghi) |
| `INT-NS-26` | `apply` với `Idempotency-Key` replay | Không bump `desired_generation` lần hai |
| `INT-NS-27` | Sửa spec service khi namespace `READY` | `ready_for_loadtest=false`, đi nhánh revalidate (`→VALIDATING` / `DEGRADED→VALIDATING`) |
| `INT-NS-28` | Sửa service nhưng nội dung spec không đổi (`spec_hash` giống) | `unchanged`, **không** reset ready, không bump generation |
| `INT-NS-29` | `GET /readiness` legacy mode | Tính on-the-fly: services, policies, `mock_capacity` hook |
| `INT-NS-30` | `GET /readiness` K8s mode, projection có dữ liệu | Đọc `tbl_component_status` + `blocking_reasons`, không tính lại |
| `INT-NS-31` | `GET /readiness` K8s mode, projection rỗng | Fallback về `readinessGates` (không 500, không báo READY giả) |
| `INT-NS-32` | `GET /dependencies`, `/network-policies` | Đồ thị đã resolve; policy per-service khớp `dependencies[]`; default-deny có mặt |
| `INT-NS-33` | Create-with-spec với dependency vòng (A→B→A) | 422 `dependency cycle: a -> b -> a` (DAG bắt buộc); DAG hợp lệ vẫn 201 — `TestINT_NS_33_CircularDeps` |
| `INT-NS-07b` | Golden path K8S mode: POST /apply → Reconciler → Watcher → `GET /readiness` ready:true | Chạy trên cluster thật (`tests/harness/watcher`, `//go:build harness_k8s`) — `TestWATCH07GoldenPath` |

### 11.3 Service, OpenAPI, Database, Dataset (`pkgs/serviceopenapi`, components)

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-SVC-01` | Thêm service với OpenAPI 3.0 YAML hợp lệ | 201; `spec_hash`, `parsed_summary` có; `GET /apis` liệt kê đúng operation (`operationId` hoặc method+path) |
| `INT-SVC-02` | Cùng spec dạng JSON | Kết quả parsed tương đương YAML |
| `INT-SVC-03` | Thiếu `openapi` | 422 |
| `INT-SVC-04` | Cú pháp sai / ngữ nghĩa sai (`$ref` hỏng, thiếu `info`) | 422 + `details` có vị trí; **không** lưu service |
| `INT-SVC-05` | Spec không có `paths` / không có operation | Theo contract (422 hoặc cho phép nhưng `targets` rỗng); khóa bằng test |
| `INT-SVC-06` | Operation không có `operationId` | Vẫn index được bằng method+path |
| `INT-SVC-07` | Spec vượt giới hạn kích thước | 413/422 theo guardrail, không treo parser |
| `INT-SVC-08` | `PUT …/spec` cùng nội dung | `unchanged`; khác nội dung → ready reset, `spec_hash` đổi |
| `INT-SVC-09` | Dependency trỏ `service_id` không tồn tại | 422 |
| `INT-SVC-10` | `database_bindings` trỏ `database_id` không tồn tại | 422 |
| `INT-SVC-11` | Xoá service đang được service khác phụ thuộc / đang có run | 409 |
| `INT-SVC-12` | Dependency vòng (A→B→A) | Theo policy (cho phép vì là đồ thị gọi, hoặc 422); khóa bằng test, NetworkPolicy vẫn đúng |
| `INT-SVC-13` | `service_id` trùng trong namespace | 409; trùng ở namespace khác → OK |
| `INT-SVC-14` | Ingress trỏ service/port không tồn tại; host/path xung đột | 422 / 409 |
| `INT-DB-01` | Thêm database hợp lệ (engine postgres, cnpg) | 201; binding hợp lệ sau đó OK |
| `INT-DB-02` 🔜 | `data_source.dataset_id` không tồn tại / dataset không `AVAILABLE` / khác tenant / engine version lệch | 422 (hoặc 409) với lý do rõ |
| `INT-DB-03` | `secret_ref` | Chỉ lưu reference; response không bao giờ có giá trị secret |
| `INT-DB-04` 🔜 | `mock.enabled` + real provisioning cùng bật (mơ hồ) | 422 (không cho dual-active) — chờ mock-service |
| `INT-DS-01` 🔜 | Tạo dataset (`Idempotency-Key`), `pii_classification` ∈ {masked, synthetic, non_prod_raw} | 201/202; giá trị khác → 422/CHECK |
| `INT-DS-02` 🔜 | Dataset: owner own, admin cùng tenant xem/suspend, admin **không** thành owner | Đúng như `database-management-api.md` §10.3 |
| `INT-DS-03` 🔜 | Xoá dataset đang được namespace tham chiếu | 409 |
| `INT-DS-04` 🔜 | Location không thuộc bucket non-prod được duyệt / URL tuỳ ý | 422 (SSRF-safe). Liên quan `EXT-NET-02` |

### 11.4 Script & Loadtest (`pkgs/script`, `pkgs/loadtest`, `pkgs/runner`)

**Script**

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-SCR-01` | Upload `ok.js` ≤1MiB | `PENDING_VALIDATION`; worker chuyển `VALIDATING→VALIDATED` (Eventually); `content_hash` đúng |
| `INT-SCR-02` | `oversize_1mib.js` (>1MiB) | 413/422 ở API; CHECK `size_bytes ≤ 1MiB` là lớp bảo vệ thứ hai |
| `INT-SCR-03` | `syntax_error.js` | `INVALID`; `GET …/validations` có `attempt=1`, `errors` JSONB có dòng/lý do |
| `INT-SCR-04` | `POST …/validate` (revalidate) từ `INVALID` | `→VALIDATING`, `attempt=2`, kết quả cũ giữ trong lịch sử |
| `INT-SCR-05` | Revalidate từ `VALIDATED` (migration 0009) | Cho phép; không mất trạng thái cũ nếu validate lỗi hạ tầng |
| `INT-SCR-06` | Validator lỗi hạ tầng (`FAKE_K6_MODE=inspect_fail` / binary thiếu) | `VALIDATION_FAILED` (khác `INVALID`), không tính là script sai |
| `INT-SCR-07` | Queue worker đầy | 429 `QUOTA_EXCEEDED`, trạng thái script **không đổi** (= `STATE-QUEUE-01`, regression plan-16 High-2) |
| `INT-SCR-08` | Script thuộc namespace khác hoặc tenant khác | 404 |
| `INT-SCR-09` | Path traversal khi đọc staged script (`../`, symlink, `\x00`) | Bị chặn, không đọc ngoài thư mục staging |
| `INT-SCR-10` | Validate đồng thời cùng một script | Chỉ một attempt chạy, không tạo attempt trùng |

**Loadtest create (gates).** Chuẩn bị chung: namespace `READY` (legacy apply hoặc fake K8s + Watcher), script `VALIDATED`, service có OpenAPI `orders_valid.yaml`.

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-LT-01` | Happy path: targets hợp lệ + script VALIDATED + load_config | 202 + `status_url`; run `QUEUED/PROVISIONING→RUNNING`; transition trong tx; `Runner.Start` gọi đúng 1 lần với args/labels đúng |
| `INT-LT-02` | Namespace chưa READY (DRAFT/APPLYING/DEGRADED/INVALID) | 409 `NAMESPACE_NOT_READY` + chi tiết readiness; **không** tạo run |
| `INT-LT-03` | `ready_for_loadtest=true` nhưng `status_observed_at` quá `staleness_threshold` (fake clock) | 409 `NAMESPACE_NOT_READY` lý do `STATUS_STALE` (cùng gate với `WATCH-05`) |
| `INT-LT-04` | Script chưa `VALIDATED` (PENDING/VALIDATING/INVALID/VALIDATION_FAILED) | 409 `SCRIPT_NOT_VALIDATED` |
| `INT-LT-05` | `targets` rỗng | 422 |
| `INT-LT-06` | Target: service không tồn tại / `operation_id` không tồn tại / method+path không khớp spec | 422 + `details` chỉ ra target nào |
| `INT-LT-07` | Target bằng `operation_id` và bằng `method+path` cho cùng operation | Cả hai resolve; lưu dạng chuẩn hoá |
| `INT-LT-08` | `load_config` vượt guardrail (vusers/duration/timeout) | 422 hoặc 429 theo policy; `GET /guardrails` khớp giới hạn thực |
| `INT-LT-09` | Namespace của tenant khác / của owner khác (user thường) | 404 |
| `INT-LT-10` | Admin tạo run trên namespace của owner cùng tenant | Cho phép nếu policy cho phép; `owner_user_id` của run theo contract; audit |
| `INT-LT-11` | `Idempotency-Key` replay | Không tạo run thứ hai, không gọi `Runner.Start` lần hai |
| `INT-LT-12` 🔜 | Quota 429 (`tbl_reservation_ledger`) | 429 `QUOTA_EXCEEDED` — chưa wire (`mvp-status.md` §7.5) |

**Loadtest lifecycle & reconciler**

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-LT-20` | Run thành công (fake k6 `ok`) | `RUNNING→COMPLETED`; events liên tục, `seq` không đứt |
| `INT-LT-21` | Run lỗi (`exit_nonzero`) | `FAILED`, lý do ghi trong event |
| `INT-LT-22` | `POST /stop` khi RUNNING | `STOPPING→STOPPED`; process/TestRun fake bị dừng; stop lần hai idempotent 2xx (`RUN-STOP-01`) |
| `INT-LT-23` | `stop` trên run đã terminal (COMPLETED/STOPPED) | Theo policy (2xx no-op hoặc 409); khóa bằng test |
| `INT-LT-24` | Vượt `timeout_at` (fake clock) | `TIMED_OUT→TIMEOUT_CLEANED`; cleanup lỗi → `CLEANUP_FAILED`, reaper retry idempotent (`RUN-TIMEOUT-01`, `RUN-LEAK-01`) |
| `INT-LT-25` | Reconciler mất runner (process không còn, DB vẫn RUNNING) | `FAILED` (runner-lost, plan-16 High) |
| `INT-LT-26` | Reconciler khởi động lại giữa lúc có run RUNNING | Tiếp tục theo dõi từ `runner_ref`, không tạo run mới (`RUN-CRASH-01`) |
| `INT-LT-27` | Reconciler chỉ đổi trạng thái qua `fn_loadtest_transition` | Không có `UPDATE … SET status` trực tiếp (test + grep CI cho `Raw` ngoài `pkgs/store/transitions.go`) |
| `INT-LT-28` | `GET /loadtests` filter `namespace_id`, `status`; `owner` chỉ admin | Đúng phạm vi tenant/owner |
| `INT-LT-29` | Namespace chuyển `DEGRADED` khi run đang RUNNING | Run **không** bị stop; event `namespace_degraded_at`; `GET` + events trả `unreliable:true` (plans/22) |
| `INT-LT-30` | Namespace `DEGRADED` **trước** khi run bắt đầu | Không `unreliable` (cờ chỉ cho run bắt đầu trước degrade) |
| `INT-LT-31` | `/logs` | Đã redact secret; chỉ owner/admin cùng tenant |

### 11.5 Env Reconciler & Status Watcher (fake K8s client)

Dùng `pkgs/k8s` fake (ghi lại lời gọi theo thứ tự, mô phỏng status). Case bám cluster thật ở §12.2.

**Env Reconciler (desired → K8s)**

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-ENV-01` | Apply namespace đủ thành phần | Thứ tự lời gọi: Namespace → NetworkPolicy → CNPG Cluster → restore Job → mock binding → Deployment/Service → Ingress (`k8s-orchestration.md` §4.1) |
| `INT-ENV-02` | Apply lại cùng `spec_hash` | No-op (không patch lại resource) |
| `INT-ENV-03` | Đổi một service | Chỉ resource của `component-id` đó được apply lại |
| `INT-ENV-04` | Xoá service khỏi spec | Resource có label `component-id` đó bị prune; resource **không** có label managed không bị đụng |
| `INT-ENV-05` | Mọi resource tạo ra | Đủ 5 label (`managed-by`, `tenant-id`, `namespace-id`, `component-id`, `component-kind`); `fieldManager=loadtest-controller` |
| `INT-ENV-06` | Manifest render | Không chứa plaintext secret; DB creds qua `secretKeyRef`; `automountServiceAccountToken=false`; ServiceAccount riêng mỗi service |
| `INT-ENV-07` | NetworkPolicy trước workload | Không có lời gọi tạo Deployment nào đứng trước default-deny |
| `INT-ENV-08` | Xoá Environment | Xoá K8s Namespace `lt-env-<id>`; không xoá từng resource lẻ |
| `INT-ENV-09` | Reconciler **không** ghi readiness vào PG | Sau apply, `tbl_namespaces.status` chưa sang READY nếu Watcher chưa chạy |
| `INT-ENV-10` | `deploy.image` dùng `:latest` ở profile prod | Bị từ chối ở validate |

**Vòng Reconciler → Watcher.** Case sự cố của Watcher là `WATCH-01..06` (§2.3.1); dưới đây là case functional:

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-LOOP-01` | Apply namespace không `data_source`; fake K8s báo Deployment `Available`, Ingress, NetworkPolicy đủ | `APPLYING→READY`, `ready_for_loadtest=true`, `blocking_reasons` rỗng, `tbl_component_status` đủ dòng; **sau đó** `POST /loadtests` thành công |
| `INT-LOOP-02` 🔜 | Có `data_source`: Cluster Ready → restore Job đang chạy | `APPLYING→DATA_LOADING`; gate loadtest vẫn 409; Job `Complete` + verify → `READY` |
| `INT-LOOP-03` 🔜 | Restore Job `Failed` / quá `activeDeadlineSeconds` | `DATA_LOADING→INVALID`, `blocking_reasons` có `restore_job_failed` / `restore_timeout` |
| `INT-LOOP-04` | Deployment `ProgressDeadlineExceeded` khi APPLYING | `→INVALID`/`DEGRADED` theo aggregator; reason `crash_loop`/`image_pull_backoff` |
| `INT-LOOP-05` | READY, một Deployment rớt dưới `replicas` > grace 60s (fake clock) | `READY→DEGRADED`, `ready_for_loadtest=false`; ổn định > stable 60s → `READY` |
| `INT-LOOP-06` | Quá `apply_deadline` ở APPLYING/DATA_LOADING | `DEGRADED`, reason `apply_deadline_exceeded` |
| `INT-LOOP-07` | Sửa spec khi READY (`desired_generation` tăng) | Component cũ chỉ `PROGRESSING` đến khi `observed_generation` khớp; `/readiness` không báo READY sớm (`WATCH-03`) |
| `INT-LOOP-08` | `network_policy`: thiếu một policy / `spec_hash` lệch (drift) | Phase `FAILED`/`PROGRESSING` + reason, namespace không READY |
| `INT-LOOP-09` | Xoá Environment | `READY→DELETING`; K8s Namespace biến mất → Watcher ghi `DELETED` |
| `INT-LOOP-10` | 4 transition bổ sung của migration 0011 (`APPLYING→READY`, `APPLYING→DEGRADED`, `DATA_LOADING→INVALID`, `DEGRADED→READY`) | Mỗi cái được đi qua ít nhất một lần (bổ sung cho `INT-MATRIX-02`) |

| `INT-LOOP-11` | Watcher ghi `status_observed_at` mỗi lần reconcile/resync | Heartbeat tăng; dừng Watcher → xem `WATCH-05` / `INT-LT-03` |
| `INT-LOOP-12` | Event của GVR thuộc namespace khác (label khác `managed-by`) | Bị bỏ qua, không enqueue |

### 11.6 Ánh xạ trùng lặp giữa harness và `INT-*`

| `INT-*` | Harness tương ứng | Giữ ở đâu |
|---|---|---|
| `INT-NS-03/04` | `API-IDEM-01/02` | Harness là nguồn chính; `INT-*` chỉ khoá contract response |
| `INT-NS-07` | `API-CONC-01` | Harness |
| `INT-ADM-15`, `INT-MATRIX-01` | `API-AUTHZ-01..04` | `INT-MATRIX-01` mở rộng sang toàn bộ route |
| `INT-SCR-07` | `STATE-QUEUE-01` | Harness |
| `INT-LT-03`, `INT-LOOP-11` | `WATCH-05` | Harness |
| `INT-LT-22/24/25/26` | `RUN-STOP-01`, `RUN-TIMEOUT-01`, `RUN-LEAK-01`, `RUN-CRASH-01` | Harness (fault) / `INT-*` (happy path của lifecycle) |
| `INT-NGX-01/07` | `NGX-01/02` | §12.1 triển khai thật trên compose |
| `INT-K8S-04` | `RUN-NET-02` | K8s harness |

## 12. Integration ngoài process: Nginx, K8s thật, E2E, Perf, Migration

### 12.1 Nginx / trust boundary (`tests/nginx/`)
Dựng `docker compose`: nginx (`conf/nginx`) + controller + postgres + một OIDC provider giả (JWKS tĩnh, token ký sẵn). Đây là nơi duy nhất kiểm **header injection thật** — các tầng khác chỉ giả lập header.

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-NGX-01` | Client gửi `X-Tenant-Id`/`X-User-Id`/`X-User-Role: admin` giả kèm token hợp lệ của user thường | Controller nhận header **do nginx set**, không phải giá trị giả; không có quyền admin |
| `INT-NGX-02` | Bearer JWT hợp lệ | 200; `X-Tenant-Id`, `X-User-Id` đúng; `X-User-Role` theo claim |
| `INT-NGX-03` | JWT hết hạn / sai chữ ký / sai issuer | 401 tại nginx, request không tới controller |
| `INT-NGX-04` | Bearer `abt_` (introspect qua controller) | 200; `X-User-Id` từ introspection; sau revoke: tối đa sau TTL cache (~60s) mới 401 — test dùng cache TTL ngắn |
| `INT-NGX-05` | **Role propagation gap** (đã biết): `abt_` và OIDC session không set `X-User-Role` | **Khóa hành vi hiện tại**: admin vẫn được chuẩn hoá từ DB (`tbl_user_roles`), user thường không thành admin; khi nginx được căn chỉnh thì cập nhật test (`NGX-03`) |
| `INT-NGX-06` | `/api/admin/v1/tenant/*`, `/token/introspect` từ client bên ngoài | Không truy cập được từ kênh public (chỉ kênh trusted nginx) |
| `INT-NGX-07` | Upstream controller chậm/chết khi nginx gọi auth-config/introspect | Fail an toàn (không cho vào ẩn danh); cache có TTL |
| `INT-NGX-08` | `auth-config` cache 600s; rotate secret | Hành vi đúng sau khi cache hết hạn |
| `INT-NGX-09` | Method allowlist | `DELETE`/`OPTIONS` hoạt động cho route cần; method lạ bị chặn |

### 12.2 K8s harness (`//go:build harness_k8s`, kind/colima)
Giữ ở `tests/harness/k8s/`; ngoài `WATCH-01..06` và `RUN-NET-02`, thêm case functional trên cluster thật:

| ID | Kịch bản | Expected |
|---|---|---|
| `INT-K8S-01` | SSA apply thật: Namespace, Deployment, Service, Ingress, NetworkPolicy | Idempotent khi apply lại; managed fields đúng `loadtest-controller` |
| `INT-K8S-02` | CNPG `Cluster` CR | Cần operator + CRD cài sẵn; Watcher đọc condition `Ready` đúng |
| `INT-K8S-03` | Informer thật: xoá Deployment bằng tay (drift) | Watcher phát hiện, namespace → `DEGRADED` sau grace |
| `INT-K8S-04` | Default-deny + per-service policy | Pod runner không tới được đích ngoài danh sách (`RUN-NET-02`); SUT gọi được dependency khai báo |
| `INT-K8S-05` 🔜 | `TestRun` k6-operator thật | Cần K8s runner (hiện `newRunner()` luôn trả Local); `status.stage` → `fn_loadtest_transition` |
| `INT-K8S-06` | RBAC ServiceAccount controller | Mọi verb trong bảng RBAC `k8s-orchestration.md` §6 hoạt động; verb ngoài bảng bị từ chối |

### 12.3 E2E smoke (`tests/e2e-smoke.sh`)
Hiện đã PASS: seed → namespace → service → validate → apply → script → loadtest → events.

| ID | Kịch bản | Môi trường |
|---|---|---|
| `E2E-01` | Luồng chính như trên (legacy mode, Local runner) | `docker compose` |
| `E2E-02` | Luồng chính + phủ định: namespace chưa READY → loadtest 409; script INVALID → 409 | `docker compose` |
| `E2E-03` | Đi qua nginx với `abt_` token (issue → dùng → revoke → bị chặn) | compose + nginx |
| `E2E-04` 🔜 | K8s mode trên kind: apply thật → READY (Watcher) → run qua K8s runner | kind, sau khi có K8s runner |

### 12.4 E2E full deploy (`tests/e2e-full-deploy.sh`)
End-to-end **K8s-mode** flow with real cluster (kind), CNPG operator, and Env Reconciler + Status Watcher loops. Verifies full production path.

| ID | Kịch bản | Môi trường |
|---|---|---|
| `E2E-FD-01` | **Full K8s deploy**: kind cluster → CNPG operator install → control-plane Postgres → controller (K8S_ENABLED=true) → namespace with database (CNPG) + service (echo-server) → validate → apply (202) → Env Reconciler applies → Status Watcher projects → READY → port-forward curl echo-server → delete namespace → verify K8s ns gone | kind + CNPG |
| `E2E-FD-02` | **Negative**: invalid spec → validate returns 422 | kind |
| `E2E-FD-03` | **HA simulation**: controller pod kill mid-apply → verify convergence on resync | kind |
| `E2E-FD-04` | **Full load-test lifecycle**: namespace READY → upload k6 script → poll `VALIDATED` → `POST /loadtests` (201) → assert run-scoped NetworkPolicy (`loadtest-run-{egress,ingress}-<id>`, `component-kind=run-policy`, egress `ports:[80]`, target selector = service name, 6565 rule) → poll `COMPLETED` → `started_at`/`finished_at` + events + `unreliable` | kind + CNPG + k6-operator |
| `E2E-FD-05` | **L3/L4 probe**: busybox pod gắn `loadtest.io/run-id=<id>`: runner→echo (port 80) ✅; runner→demo-db (5432) ❌; undeclared internet/metadata ❌ | kind + Calico |
| `E2E-FD-06` | **L3/L4 probe negative**: other-service→demo-db (5432) ❌ (không `database_bindings`); bound-service→demo-db ✅ (có binding) | kind + Calico |
| `E2E-FD-07` | **Cleanup**: `DELETE /namespaces/{id}` → 202 DELETING → K8s ns gone → run-scoped NetworkPolicy + ConfigMap gone | kind + CNPG |
| `E2E-FD-08` | **Script gate**: `POST /loadtests` với script chưa VALIDATED → 409 `SCRIPT_NOT_VALIDATED` | kind |

**Prerequisites**: `kind`, `kubectl`, `docker`, `curl`, `python3`. **k6-operator** (`bundle.yaml`, pinned tag) + `grafana/k6` image phải có sẵn trong kind (script cài trong `tests/e2e-full-deploy.sh`). Controller chạy với `RUNNER_MODE=k8s` cho các case 04–07. Uses `KEEP_CLUSTER=1` to retain cluster for debugging.

**Flow** (from script):
1. Bootstrap kind cluster + pre-load images (CNPG postgres, echo-server, `grafana/k6`)
2. Install CNPG operator (`kubectl apply --server-side`)
3. Install k6-operator (`bundle.yaml`); chờ CRD established + `k6-operator-controller-manager` ready
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
Ngưỡng đã định nghĩa: `control-plane.js` (40 rps đọc, p95 < 100ms), `admin.js` (80 rps introspect). **Chưa chạy lần nào** vì k6 chưa cài (`mvp-status.md` §7.2).

| ID | Việc cần làm |
|---|---|
| `PERF-01` | Cài k6, chạy cả hai script trên stack compose, **ghi baseline vào README** (p50/p95/p99, error rate, máy chạy) |
| `PERF-02` | Thêm script cho `/readiness` (projection-backed) và gate `POST /loadtests` để bắt hồi quy truy vấn JOIN |
| `PERF-03` | Gắn vào CI/nightly với ngưỡng = baseline × hệ số dung sai đã chốt |

### 12.6 Migration (thuộc `pkgs/store`)
| ID | Kịch bản | Expected |
|---|---|---|
| `INT-MIG-01` | Migrate từ DB rỗng 0001→0011 | Thành công; `tbl_schema_migrations` đủ bản ghi, đúng thứ tự |
| `INT-MIG-02` | Chạy lại runner trên DB đã migrate | No-op, không lỗi |
| `INT-MIG-03` | Nâng cấp từ snapshot cũ có dữ liệu (trước 0007) | Secrets bị strip, dữ liệu khác nguyên vẹn; token cũ vẫn introspect được |
| `INT-MIG-04` | Hai instance chạy migration đồng thời | Chỉ một thắng (advisory lock), không áp trùng |
| `INT-MIG-05` | File migration đã áp bị sửa nội dung (checksum lệch) | Runner từ chối, báo rõ |

## 13. Cross-references & quick ID index

Bảng tra cứu nhanh các ID tham chiếu xuyên phần I (harness, §1–§8) và phần II (functional integration, §9–§15), để không gãy liên kết khi thêm mục mới.

| ID | Ý nghĩa | Nơi định nghĩa |
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

## 14. Chạy test & CI

```bash
# Unit + integration + harness (cần Postgres 16)
docker compose up -d postgres
export TEST_DATABASE_DSN='postgres://…'
go test -race ./...                         # integration tự skip nếu không có DSN (CI=true thì fail)

# SQL
for t in tests/sql/*_test.sql; do psql "$TEST_DATABASE_DSN" -v ON_ERROR_STOP=1 -f "$t"; done

# Một kịch bản theo ID
go test -race ./... -run 'TestINT_LT_03'

# K8s harness (cần kind/colima + CRD)
go test -tags harness_k8s ./tests/harness/k8s/...

# E2E
docker compose up -d --build && ./tests/e2e-smoke.sh
```

| CI job | Chạy | Ghi chú |
|---|---|---|
| `lint` | golangci-lint v2 | |
| `test` | unit + integration (`INT-*`) + harness (fake K8s), `-race`, Postgres service | `CI=true` ⇒ không được skip |
| `sql-migrations` | migrate từ rỗng + `tests/sql/*` | |
| `build` | image | |
| `watcher-harness` | `harness_k8s` trên kind | |
| *(đề xuất)* `nginx-integration` | `tests/nginx/` trên compose | PR đụng `conf/nginx` bắt buộc |
| *(đề xuất)* `e2e` | `tests/e2e-smoke.sh` | |
| *(đề xuất, nightly)* `perf`, `k6-real` | k6 perf + Local runner với k6 thật | |

## 15. Khoảng trống đã biết & theo dõi coverage

| Khoảng trống | Ảnh hưởng test | Hành động |
|---|---|---|
| K8s runner chưa có (`newRunner()` luôn Local) | `INT-K8S-05`, `E2E-04` | Viết cùng plan K8s runner |
| k6 perf chưa chạy lần nào | `PERF-01..03` | Cài k6, chạy, ghi baseline |
| Outbox worker chưa có dispatcher | `API-OUTBOX-01/02` | Viết cùng worker |
| Quota gate 429 chưa wire vào loadtest | `INT-LT-12`, `DB-TXN-07` phía app | Viết cùng tính năng |
| mock-service endpoint còn stub | `INT-DB-04`, mock readiness, `MOCK-*` | Viết khi contract chốt |
| Dataset/restore chưa implement | `INT-DS-*`, `INT-DB-02`, `INT-LOOP-02/03` | Viết cùng tính năng |
| Role propagation nginx chưa đồng nhất | `INT-NGX-05` chỉ khóa hành vi hiện tại | Cập nhật khi chỉnh `conf/nginx` |
| Hành vi biên chưa chốt (namespace rỗng, dependency vòng, stop run đã terminal, tạo lại tên sau `DELETED`, status khi thiếu `Idempotency-Key`) | Các case "khóa bằng test" ở §11 | Chốt quyết định, ghi vào doc API tương ứng, rồi đổi kỳ vọng thành giá trị cụ thể |

### 15.1 Theo dõi ID đã có test
Catalogue này chưa đối chiếu với các test file hiện có. Cách đồng bộ rẻ nhất:

1. Đặt tên test theo `TestINT_<DOMAIN>_<NN>_…` (§9.6); test cũ thì đổi tên hoặc thêm comment `// covers: INT-NS-06`.
2. Thêm `scripts/test-coverage-map.sh`: `grep` các ID trong `docs/harness-testing.md` và trong `*_test.go`, in ID **chưa có test** và ID **mồ côi** (có trong code nhưng không có trong doc).
3. Chạy trong CI ở chế độ cảnh báo; chuyển thành fail khi catalogue được phủ phần lớn.

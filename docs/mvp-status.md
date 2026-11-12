# MVP Status — Integration Summary & Review

> Status after completing plans 01–15 + 3 hardening/refactor PRs.
> Use as the entry point for **integration testing** and **code review**.

## 1. Architecture built

```
nginx (conf/nginx — pre-existing, untouched)
  └─ /api/admin/v1    ─┐
  └─ /api/loadtest/v1 ─┴─► loadtest-controller (go-chi, :8080)
                             ├─ pkgs/authz          trusted headers → Principal, fail-closed, RBAC
                             ├─ pkgs/adminapi       tenant-id, auth-config, introspect abt_, issue/reveal/revoke token
                             ├─ pkgs/namespace      CRUD + Idempotency-Key + ETag + validate/apply/readiness
                             ├─ pkgs/serviceopenapi kin-openapi validate + targets[] index
                             ├─ pkgs/script         upload ≤1MiB + async validation workers (k6 inspect/structural)
                             ├─ pkgs/loadtest       gates 409/422 + lifecycle + reconciler (2s poll)
                             ├─ pkgs/runner         Runner interface: Local (k6 binary) | K8s (k6-operator TestRun via Go client, RUNNER_MODE=k8s)
                             ├─ pkgs/secrets        SOPS env resolver + AES-256-GCM field-level
                             └─ pkgs/store          GORM models + migration runner + Transition (single Raw bypass)
                                    ▼
                            PostgreSQL (13 migrations, tbl_schema_migrations tracked)
```

## 2. API surface (implemented)

### `/api/admin/v1` — nginx-internal (trusted channel, no principal)
| Method | Path | Notes |
|---|---|---|
| GET | `/tenant/{host}/id` | plain UUID (Lua `read_body` → header) |
| GET | `/tenant/{host}/auth-config?name=` | shape `{auth:{...}}`; secrets from `LTP_TENANT_<SLUG>_*` env |
| POST | `/token/introspect` | `{active,user_id,tenant_id,roles,is_admin}` — prefix lookup + constant-time hash |

### `/api/admin/v1` — operator (RequirePrincipal + RequireSelfOrAdmin)
| Method | Path | Notes |
|---|---|---|
| POST | `/users/{uid}/tokens` | issue `abt_` — plaintext once; encrypted at rest |
| GET | `/users/{uid}/tokens` | list metadata (no plaintext) |
| POST | `/tokens/{id}/revoke` | self-or-admin |
| POST | `/tokens/{id}/reveal` | decrypt AES-GCM (requires `LTP_MASTER_KEY`), audit-logged |

### `/api/loadtest/v1` — RequirePrincipal (fail-closed 401)
| Method | Path | Notes |
|---|---|---|
| POST | `/namespaces` | Idempotency-Key required, replay cached response |
| GET/PATCH/DELETE | `/namespaces[/{id}]` | owner-only; admin cross-owner within the same tenant; 404 to prevent leaks; ETag/If-Match → 412 |
| GET | `/namespaces/{id}/events` | transition history |
| POST | `/namespaces/{id}/services` | OpenAPI validate → 422 + reason; spec change → ready reset |
| PUT | `/namespaces/{id}/services/{svc}/spec` | same-content → unchanged |
| POST | `/namespaces/{id}/validate` | →VALIDATING→(INVALID+422 checklist \| APPLYING) |
| POST | `/namespaces/{id}/apply` | Idempotency-Key; resolve deps → network_policies. **K8S_ENABLED=true:** only bumps `desired_generation`, returns 202 APPLYING — Env Reconciler applies to K8s, Status Watcher projection closes the APPLYING→READY loop (plans/21). **No cluster (legacy):** keeps the sync branch `→DATA_LOADING→READY` for the MVP stack |
| GET | `/namespaces/{id}/readiness` | checklist gates (services, policies, mock_capacity hook) |
| POST | `/namespaces/{id}/scripts` | ≤1MiB; async validate → VALIDATED/INVALID |
| POST | `/namespaces/{id}/scripts/{sid}/validate` | revalidate |
| GET | `/namespaces/{id}/scripts/{sid}/validations` | attempts + errors JSONB |
| POST | `/namespaces/{id}/loadtests` | gates: NAMESPACE_NOT_READY/SCRIPT_NOT_VALIDATED → 409; empty targets → 422 |
| POST | `/namespaces/{id}/loadtests/{lt}/stop` | →STOPPING (reaper/reconciler completes) |
| GET | `/namespaces/{id}/loadtests/{lt}/events` | transition trail |

Health: `/health`, `/ready` (fail-closed DB), `/metrics` (prometheus).

## 3. DB schema (14 migrations — source of truth `sql/`)

| Migration | Contents |
|---|---|
| 0001 | tenants, tenant_hosts, auth_profiles, users, external_identities, user_roles, access_tokens (hash), audit_log |
| 0002 | **pattern**: `tbl_status_transitions` + `fn_is_transition_allowed` + `fn_status_transition` |
| 0003 | namespaces + events + `fn_namespace_transition` + services/databases/ingress/dependencies/network_policies |
| 0004 | scripts + events + validations, load_tests + events + `fn_loadtest_transition`, test_events |
| 0005 | idempotency_keys, outbox_events, reservation_ledger, datasets + events |
| 0006–0009 | additional seed transitions, strip secrets from the DB, token_encrypted, revalidate VALIDATED |
| 0010–0014 | namespace spec/components, component status, readiness invariants + transition clears readiness, **load test results** (plans/45) |

**Invariants verified by SQL tests (CI job sql-migrations):**
- Every status change goes through `fn_*_transition` (FOR UPDATE → check map → status+version+1 → event, 1 transaction)
- Events append-only; `tbl_<object>` is the read source (JOIN + index)
- 5 test suites: `tests/sql/{transition,namespace_transition,loadtest_transition,plumbing,loadtest_results}_test.sql`

## 4. Security checklist

- ✅ Fail-closed 401 when identity headers are missing; 404 (not 403) to prevent existence leaks
- ✅ `RequireSelfOrAdmin`: self ✓, admin within the same tenant ✓, admin from another tenant ✗, role spoof ✗
- ✅ `abt_` stores only the SHA-256 hash + prefix; plaintext encrypted with AES-256-GCM (`LTP_MASTER_KEY` from SOPS) for reveal
- ✅ Secrets are not stored in the DB (stripped by migration 0007); SOPS file-level (age) + field-level (pattern sops.rs)
- ✅ Auth-config response contains secrets — trusted channel only, warn log without the value
- ✅ Path traversal blocked when reading staged scripts
- ⚠️ Known gap (documented): nginx only sets `X-User-Role` on the Bearer-JWT path — RBAC additionally relies on `tbl_user_roles` server-side

## 5. Testing

| Layer | Contents | Where it runs |
|---|---|---|
| Unit | authz (6 cases), ETag, idempotency middleware, secrets resolver/crypto, serviceopenapi | `go test ./...` — no DB required |
| Integration (GORM + Postgres 16) | adminapi, namespace, script, loadtest, reconciler, store migration runner | CI job `test` (postgres service); local skip when no DB is available, **hard-fail when `CI=true`** |
| SQL | transition pattern, 4 suites | CI job `sql-migrations` |
| E2E | `tests/e2e-smoke.sh` — seed → ns → service → validate → apply → script → loadtest → events → delete namespace (**PASS verified**) | local: `docker compose up -d` + run the script |
| Perf | `tests/perf/control-plane.js` (40rps reads p95<100ms), `admin.js` (80rps introspect) | k6 run — **gate has never been run** (k6 binary not installed) |

Total: 17 test files, 12/12 packages pass with `-race`.

## 6. Running locally

```bash
docker compose up -d --build          # postgres + controller
./tests/e2e-smoke.sh                  # e2e pass/fail
earthly +controller-image             # build image (pattern opsense-rs)
```

CI (GitHub Actions): lint (golangci-lint v2) + test (`-race`, postgres service) + build + sql-migrations — **4/4 green on main**.

## 7. Gaps / remaining work

1. **k6 perf gate not yet run** — install k6 and run `tests/perf/*` at least once, record the numbers in the README.
2. **mock-service** — the binary exists at `cmds/mock-service` (separate data plane, does not use pkgs/controller); the mock-status/mock-pools endpoints are still stubs, metadata CRUD goes through namespace components.
3. **Outbox worker** — the table + outbox writes exist, but there is no dispatcher loop yet.
4. **Quota gate 429** — `tbl_reservation_ledger` has a schema, not yet wired into loadtest create.
   Per-run guardrails (vusers/duration/timeout caps → 422) **are** enforced at create
   (`docs/loadtest-manager-api.md` §5.4, plans/42); per-tenant quotas remain deferred.
5. **CodeGraph final review + bump version** (`scripts/bump.sh`) — per AGENTS.md before treating it as a release.
6. Plans 16 (harness-testing-integrity) — created by the user, not yet implemented.

### Phase 2 (plans 41–45)

| Plan | Scope | Status |
|------|-------|--------|
| 42 | loadtest caps — MAX_VUS / MAX_DURATION / MAX_TIMEOUT_MINUTES → 422 at create | implemented (`pkgs/loadtest/limits.go`, wired through server/testutil) |
| 43 | sample service — `examples/sample-service` Go SUT + openapi.yaml + Dockerfile + kind E2E wiring | implemented |
| 44 | developer CLI — `pkgs/ltpclient` + `cmds/ltp` (ns/script/run/report), exit codes, docs, e2e-cli | implemented |
| 45 | run results — migration 0014, k6 summary parse (local + pod log), API fields, `ltp run report` | implemented |
| 41 | submission docs — README (EN), WRITEUP.md, AI-USAGE.md, plans index, mvp-status refresh | **remaining deliverable** (this doc refresh) |

Merge order: 42 → 43 → 44 → 45 → 41 (plan 41 merges last so the README documents shipped CLI syntax).
Quota ledger (gap 4) stays deferred; demo video is still unplanned (plan 46, pending confirmation).

## 7b. Harness (plans/16 — PR #20/#21, 2026-11-07)
- Centralized composition root: `Mount(r chi.Router, d Deps)`, a single server.go configures middleware; tests via `pkgs/testutil.NewServer` (does not rebuild auth)
- High-severity fixes: loadtest create transition-in-tx + Runner.Start; queue-full → 429 (no getting stuck in VALIDATING); reconciler runner-lost → FAILED
- Role verified DB server-side (`RequirePrincipalWithRole`) — spoofed headers blocked
- Harness matrix: `tests/harness/` (AUTHZ-01..04, IDEM, CONC, VAL, STATE-*, RUN-*) + `tests/sql/harness_test.sql` (DB-TXN-01..05...) — **TDD proof: breaking an invariant → red**
- Bonus fix: cross-tenant token issue hole (caught by the harness)
- Recorded deviation: `POST /tokens/{id}/revoke` instead of `DELETE /users/{uid}/tokens/{tid}`; admin CRUD surface deferred

## 8. Areas to review carefully during code review

1. `pkgs/store/transitions.go` — the single Raw bypass point; every status change must go through here
2. `pkgs/store/models.go` — `BeforeCreate` UUID (GORM inserting '' overrides DEFAULT if missing); json tags
3. `pkgs/authz/selforadmin.go` + `pkgs/adminapi/handlers.go` — trust boundary, constant-time compare, secret handling
4. `pkgs/loadtest/reconciler.go` — transition-first (never UPDATE status directly), idempotent reap
5. `pkgs/namespace/handlers.go` — idempotency replay (captureWriter), ETag, 404-vs-403
6. `pkgs/secrets/crypto.go` — format nonce(12)||ciphertext, master key lifecycle
7. `sql/0002` — transition function template (uses `%I` dynamic SQL — review the injection surface)

### plans/22 — Watcher hardening (2026-11-08)

- `GET /readiness` **projection-backed in K8S mode**: reads `tbl_component_status` + `blocking_reasons` (falls back to readinessGates when the projection is empty); legacy mode keeps computing on the fly.
- Watcher event source: `k8s.Client.Watch` per GVK → workqueue dedup/rate-limited (client-go workqueue), workers call `ReconcileStatus`; polling resync kept as a safety net.
- `apply_deadline` enforcement: a namespace stuck in APPLYING/DATA_LOADING past the deadline → DEGRADED reason `apply_deadline_exceeded`; all 5 `WATCHER_*` knobs wired.
- DEGRADED-with-running-run: event `namespace_degraded_at` metadata; GET loadtest + events return `unreliable: true` when the namespace is DEGRADED after the run starts (the run is not stopped).
- Harness WATCH-01..06: fake-client cases (CI `test` job) + `//go:build harness_k8s` on kind (CI job `watcher-harness`) / colima locally.

### plans/25 — Codebase layering PH1 (2026-11-08)

- §5.4 Stage 1 ✅: extracted `pkgs/readiness` (Gates/ProjectionReport/BlockNameOf + dataset/ingress helpers) out of `pkgs/namespace` — handlers now call `readiness.*`; readiness is DESIRED-side logic, the watcher keeps the OBSERVED-side evaluators.
- §5.3 amendment: `B → D(pure helpers only)` (mockcontrol.DecodeMockDef, envreconciler render) + `E → F, httpx` — matches §5.2.1 "shared code is pure helpers only".
- Stage 2/3 keep the baseline (trigger not fired yet): split loadtest waits on the K8s runner/quota; ~55 gorm refs in handlers recorded in §5.4 to refactor once there is a second query consumer.

### plans/26 — Full deploy E2E + apply fully manages objects (2026-11-08)

- **Apply fully orchestrates K8s objects** per component (Go-render, keeping ADR-004): service → ConfigMap (large env) + Secret (Opaque, env via `valueFrom.secretKeyRef` — plaintext NEVER enters the manifest, fixing the docs §7:84 violation) + Deployment + Service + HPA (autoscaling/v2) + ServiceAccount; database → operator CR by the `operator` column (`cloudnative-pg` | `elastic`/ECK — unknown fails closed before apply). Prune extended to the 4 new kinds.
- **Full deploy E2E** (`tests/e2e-full-deploy.sh` + CI job `full-deploy-e2e`): create-with-spec in 1 call (database CNPG demo-db + echo-server) → validate → apply 202 → READY on kind → kubectl verify `cluster,hpa,cm,secret,deploy,svc` → port-forward curl echo returns JSON → DELETE namespace (202/DELETING) + verify the K8s namespace `lt-env-*` is deleted.
- **Remove fakeWatcher** (plans/26): the `pkgs/k8s` Fake keeps Apply/Get/List/Delete for unit fault-injection; the watch path is tested for real on kind (`TestWATCH08WatchEventsTriggerReconcile`). Fixed 2 bugs it exposed: consumeWatch tight-spin when the channel closes (starving reconcile), Tick stall when one failing namespace blocks the other namespaces (per-ns log-and-continue).
- **202 + status_url** for create-with-spec + `apply:true` in K8S mode (docs namespace-management-api.md:93) — apply/reconcile STARTS rather than completes; legacy keeps 201 READY.
- New tests: INT-NS-02b (202+Location), INT-NS-34 (combined flow), render ConfigMap/Secret/HPA/SA/ECK + unknown-operator fail-closed.

- **Hierarchical apply (plans/26 addendum)**: `Applier` interface (`Kind/Name/Apply/Children`) — applies the tree outside→in via `ApplyTree`; `applyNamespace` (K8S) calls `ApplyNow` right after the desired-state commit (best-effort, watcher + loop converge on their own). Tick batches 100 at a time + prioritizes `status_observed_at NULLS FIRST`; tests clean up namespaces after a run (a backlog of 1133 old namespaces once stalled Tick). Tests: INT-ENV-08 (apply order), INT-ENV-09 (aggregation all-children-ready).
- **Centralize state machines (plans/26, review feedback)**: `pkgs/store/{namespace,loadtest,script,dataset}_lifecycle.go` — status constants + typed wrappers + caller-owner table; every call-site migrated (loadtest 10, namespace 5, dataset 3, script 2); a **lint-transitions guard** in CI blocks raw `store.Transition` outside `pkgs/store/` — adding/changing a state means reviewing 1 file.

### plans/34 — AIO serve image + Dex compose + integration tests (2026-11-11)

- **Earthfile rewrite** (pattern `sources/opsense-rs/Earthfile`): `+build` 2 binaries (fast path `bin/` host, fallback go build); `+serve` AIO openresty-fat image (nginx + supervisor + controller **unix socket** `unix:/var/run/axum` + mock-service :8090 + alloy); `+mock-service` separate image (data-plane); `+integration-images` build local no-push.
- **Controller unix socket (TDD)**: `ADDR=unix:/var/run/axum` → `net.Listen("unix")` + rm stale socket + chmod 666; TCP unchanged. nginx `conf/nginx` UNCHANGED.
- **docker-compose**: postgres + valkey (noeviction, session lua) + dex v2.41.1 (conf/dex/config.dev.yaml, issuer localhost:5556/dex) + loadtest-serve (SOPS mount, healthcheck /health).
- **Integration test** pattern opsense-rs: `tests/integration/` (tag `integration`) — dex login flow Go port, health smoke via nginx, oauth flow via the gateway `/login`+`/callback`; skip when no stack is available, fail hard when `LTP_INTEGRATION=1`. `scripts/mint-dex-token.py` host CLI. Makefile `test-integration`/`test-integration-down`/`up`/`down`.
- Details: `docs/packaging-deployment.md`, `plans/34-aio-serve-image.md`.

### plans/36 — Deploy to Kubernetes (kind) + full in-cluster test (2026-11-11)

- **Manifest**: `deploy/k8s/` (base: ltp-system, mock-system, postgres StatefulSet, valkey, dex, mock-service, serve AIO Deployment/Service/SA/RBAC/SOPS secrets; overlays: `kind/`, `prod/` skeleton).
- **Two-tier secrets**: SOPS + age (`deploy/k8s/base/serve/secrets.enc.yaml`, safe to commit) mounted via ConfigMap + `SOPS_AGE_KEY_CONTENT` Secret → `scripts/release.sh` decrypts at boot; `pkgs/secrets` field-level AES-256-GCM (`LTP_MASTER_KEY`) for values in the DB.
- **CLI**: `loadtest-controller sops encrypt|decrypt` (hex blob, pattern `opsense-rs sops.rs`) — generate/verify `token_encrypted` when seeding SQL.
- **Test**: CI job `Full Deploy E2E (in-cluster)` — Earthly build → kind load → kustomize apply → `tests/e2e-full-deploy.sh DEPLOY_MODE=in-cluster` (bearer token from `scripts/mint-dex-token.py`) → `tests/integration` via port-forward.
- Details: `docs/k8s-deployment.md`, `plans/36-k8s-manifests.md`.

### plans/45 — Run results: collect, store, surface k6 summary (2026-11-12)

- **One parser for both backends**: `pkgs/runner/summary.go` `ParseSummary` reads k6's *text* summary
  (`█ THRESHOLDS` + `█ TOTAL RESULTS`, plus the older v0.4x inline layout). Text — not
  `--summary-export` — because the K8s path can only read the pod log; two formats would mean two
  parsers. Fixtures in `pkgs/runner/testdata/` are real captured k6 output (pass/threshold-fail/
  truncated/progress-only/empty/v0.49).
- **Runner carries the raw tail, not a parsed struct**: `runner.Status` gains `Summary []byte` +
  `SummaryErr`. Local captures stdout/stderr in a buffer and reads it *before* dropping the proc;
  K8s adds `k8s.Client.PodLogs` (typed clientset, `LimitBytes` 1 MiB) and reads the runner pod log on
  the terminal poll only.
- **Collection is transactional and idempotent**: `Reconciler.finish` takes the runner status and
  writes migration `0014`'s four columns in the same transaction as the terminal transition, guarded
  by `result_collected_at IS NULL`. A missing log keeps the terminal status and records
  `result_parse_error`; a timeout records its own reason. NULL = not collected (≠ collected nothing).
- **API + CLI**: `GET /loadtests/{id}` returns the results (`docs/loadtest-manager-api.md` §8);
  `ltp run report` renders RPS/p95/p99/avg/failure-rate/checks/thresholds and never prints zeros for
  an uncollected run (`docs/developer-cli.md`).
- **Tests**: parser table tests, local-runner capture (`-race`), K8s `PodLogs` (hit/missing/cap/fault),
  reconciler result tests against real Postgres, `tests/sql/loadtest_results_test.sql`, CLI report
  tests, and a `tests/e2e-full-deploy.sh` assertion that a kind run's `result_summary.http_reqs > 0`
  within 30s of `finished_at`.
- Details: `plans/45-run-results.md`.

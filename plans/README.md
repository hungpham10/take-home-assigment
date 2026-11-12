# Plans Index

Execution order: simple → complex. Each plan = 1 PR / feature branch, TDD, docs before code.

| # | Plan | Scope | Dependencies |
|---|------|-------|--------------|
| 01 | [repo-bootstrap](01-repo-bootstrap.md) | go.mod, Makefile, lint | — |
| 02 | [ci-pipeline](02-ci-pipeline.md) | GitHub Actions (lint/test/build/sql-migrations) | 01 |
| 03 | [sql-schema-foundation](03-sql-schema-foundation.md) | Conventions + migration 0001 admin/auth | 01 |
| 04 | [sql-events-pattern](04-sql-events-pattern.md) | `tbl_<object>` current-state (JOIN read) + `tbl_<object>_events` + transition function (transaction) | 03 |
| 05 | [sql-namespace-domain](05-sql-namespace-domain.md) | Migration 0002 namespace/components | 03, 04 |
| 06 | [sql-script-loadtest-domain](06-sql-script-loadtest-domain.md) | Migration 0003 scripts/loadtests | 04, 05 |
| 07 | [sql-platform-plumbing](07-sql-platform-plumbing.md) | Migration 0004 idempotency/outbox/ledger/datasets | 03, 04 |
| 08 | [controller-bootstrap](08-controller-bootstrap.md) | cmds/loadtest-controller skeleton | 01, 03–07 |
| 09 | [authz-admin-api](09-authz-admin-api.md) | pkgs/authz + /api/admin/v1 (matches nginx Lua) | 08 |
| 10 | [namespace-api](10-namespace-api.md) | CRUD + Idempotency-Key + ETag + transitions | 04, 05, 09 |
| 11 | [openapi-validation-readiness](11-openapi-validation-readiness.md) | Validate 422, targets[], /validate /apply | 10 |
| 12 | [scripts-api](12-scripts-api.md) | Script upload/validation async | 10 |
| 13 | [loadtest-runner](13-loadtest-runner.md) | Runner interface (K8s/local) + lifecycle | 11, 12 |
| 14 | [local-stack-e2e](14-local-stack-e2e.md) | docker-compose, Earthfile (pattern opsense-rs), e2e smoke, k6 perf gate (QPS target per API) | 08–13 |
| 15 | [sops-secrets](15-sops-secrets.md) | SOPS + age: encrypted secrets, out of DB, LTP_TENANT_<SLUG>_* resolver | 08–09 |
| 16 | [harness-testing-integrity](16-harness-testing-integrity.md) | Harness: ACID/fault matrix, composition root refactor, authz matrix | 03–14 |
| 17 | [service-management](17-service-management.md) | Full service API (service_id, deploy/mock/bindings, /openapi /apis) | 11, 16 |
| 18 | [namespace-database-management](18-namespace-database-management.md) | Namespace create-with-spec + databases/datasets/ingress/dependencies | 17 |
| 19 | [k8s-orchestration](19-k8s-orchestration.md) | Docs-only: docs/k8s-orchestration.md (render/SSA, mapping, reconciler, RBAC, mock wiring); code = plan 21 | 17, 18 |
| 20 | [namespace-status-watcher](20-namespace-status-watcher.md) | Docs-only: docs/namespace-status-watcher.md (observed→PG loop, tbl_component_status projection, generation fencing, WATCH-*); code = plan 21 | 17–19 |
| 21 | [k8s-reconciler-watcher-mock](21-k8s-reconciler-watcher-mock.md) | Code: pkgs/k8s adapter, pkgs/envreconciler, pkgs/statuswatcher (readiness projection), cmds/mock-service + mockcontrol, migration 0011, WATCH-01..06 (PRs A–E) | 19, 20 |
| 22 | [watcher-hardening-readiness-projection](22-watcher-hardening-readiness-projection.md) | Readiness projection-backed (K8S), Watch→workqueue event source, apply_deadline, DEGRADED-with-running-run, WATCH-01..06 (colima/kind CI) | 21 |
| 24 | [harness-coverage-ph1](24-harness-coverage-ph1.md) | Docs sync + DB-TXN-06..10 + API-IDEM-02/03 + RUN-NET-02/MOCK-VER-01 e2e + INT labeling | 22 |
| 25 | [codebase-layering-ph1](25-codebase-layering-ph1.md) | §5.4 Stage 1: extract pkgs/readiness from namespace; §5.3 amendment (B→D pure, E→httpx) | 22 |
| 26 | [full-deploy-e2e](26-full-deploy-e2e.md) | Full deploy E2E: database CNPG + echo-server via API → READY → curl proof; drop fakeWatcher, real watch path test on kind | 22, 25 |
| 27 | [k8s-runner-e2e](27-k8s-runner-e2e.md) | k6-operator TestRun per run, run-scoped NetworkPolicy probes (busybox), kind Calico enforcement | 21, 26 |
| 28 | [runner-status-decoupled](28-runner-status-decoupled.md) | Status() reads Job -1 conditions first, then stage, then runner=true pod fallback; rendering paused=false + initializer.disabled=true | 27 |
| 29 | [e2e-probe-during-run](29-e2e-probe-during-run.md) | L3/L4 probes from a busybox pod labeled loadtest.io/run-id while the run is alive (before run-scoped NP cleanup) | 27, 28 |
| 30 | [namespace-delete-readiness](30-namespace-delete-readiness.md) | Namespace delete 202/DELETING → K8s ns garbage-collected + run-scoped NP pruned | 21, 26 |
| 31 | [platform-networking-config](31-platform-networking-config.md) | K8S_NODE_CIDRS, K6_OPERATOR_NAMESPACE, MOCK_SERVICE_NAMESPACE/PORTS; FromEnv validation | 19, 26 |
| 34 | [aio-serve-image](34-aio-serve-image.md) | Earthfile +build/+serve AIO image (nginx + supervisor + controller unix socket + mock-service + alloy), Dex compose, integration tests | 15, 21 |
| 35 | [k8s-dns1035-run-prefix](35-k8s-dns1035-run-prefix.md) | Run-scoped K8s object names use `run-` prefix (DNS-1035 for UUIDs starting with a digit) | 27 |
| 36 | [k8s-manifests](36-k8s-manifests.md) | deploy/k8s base + overlays (kind/prod), two-tier secrets (SOPS + age), CLI sops encrypt/decrypt, CI in-cluster E2E | 34, 35 |
| 37 | [oidc-subject-mapping](37-oidc-subject-mapping.md) | Dex opaque sub → tbl_users.id via tbl_user_external_identities (plans/37) | 09, 34 |
| 38 | [version-management](38-version-management.md) | Tag-driven versioning, release pipeline hardening, release overlay | 01, 02 |
| 39 | [release-earthly-install](39-release-earthly-install.md) | Earthly install fail-fast + tag after images verified | 38 |
| 40 | [release-artifacts](40-release-artifacts.md) | Attach built binaries to GitHub Release | 38, 39 |

| 47 | [apiserver-ports-config](47-apiserver-ports-config.md) | K8S_APISERVER_PORTS env var for kube-apiserver egress ports in default-deny NP | 31 |

Sau 14: mock-service (phase sau), CodeGraph review, bump version, release.

## Phase 2 — submission deliverables (plans 41–45)

Authored 2026-11-12. Merge order is by dependency, **not** by number:
**42 → 43 → 44 → 45 → 41**. Plan 41 (docs) is written first but merged last so the
README documents shipped CLI syntax.

| # | Plan | Scope | Dependencies |
|---|------|-------|--------------|
| 41 | [submission-docs](41-submission-docs.md) | README (EN, 3 run paths + journey), WRITEUP.md, AI-USAGE.md, plans index, mvp-status refresh | **merge after 42–45** |
| 42 | [loadtest-caps](42-loadtest-caps.md) | MAX_VUS / MAX_DURATION / MAX_TIMEOUT_MINUTES guardrails → 422 at create; no quota ledger | — |
| 43 | [sample-service](43-sample-service.md) | examples/sample-service: Go SUT + openapi.yaml + Dockerfile + kind E2E wiring | — |
| 44 | [developer-cli](44-developer-cli.md) | pkgs/ltpclient + cmds/ltp (ns/script/run/report), exit codes, docs, e2e-cli | 42, 43 |
| 45 | [run-results](45-run-results.md) | migration 0014, k6 summary parse (local + pod log), API fields, ltp run report | 43, 44 |

Not yet planned: **demo video** (plan 46, pending confirmation) and **quota
ledger 429** (deferred; see plan 42 "Out of scope").

# WRITEUP — Load Testing Platform (MVP)

Design decisions and their costs, not a feature list. This is the narrative a
reviewer gets when they read the code and want to know *why* it was built this
way.

## Problem framing

Teams need to run load tests against their own services without asking platform
engineers for k6 scripts, clusters, or credentials. The platform treats a
**namespace (environment)** as the unit of isolation — not the test run. Inside
a namespace, services, databases, mocks, ingress, and dependencies are declared
once (create-with-spec); load tests (k6) run against declared targets only, and
NetworkPolicy enforces L3/L4 isolation while the run is alive.

**One-sentence thesis:** a namespace is a reproducible, network-isolated test
harness; a load test is a transactional state change on top of it.

## Architecture

```
nginx (API gateway, OIDC/JWT, tenant headers)
  └─ /api/loadtest/v1  ─► loadtest-controller (go-chi, :8080)
                              ├─ pkgs/authz        trusted headers → Principal, fail-closed RBAC
                              ├─ pkgs/adminapi     abt_ tokens, introspect, issue/reveal/revoke
                              ├─ pkgs/namespace    CRUD + Idempotency-Key + ETag + validate/apply/readiness
                              ├─ pkgs/serviceopenapi YAML→JSON OpenAPI index, targets[]
                              ├─ pkgs/script       ≤1 MiB upload, async k6 validation
                              ├─ pkgs/loadtest     409/422 gates, lifecycle, reconciler (2s poll)
                              ├─ pkgs/runner       Local (k6 binary) | K8s (k6-operator TestRun)
                              ├─ pkgs/secrets      SOPS env resolver + AES-256-GCM fields
                              └─ pkgs/store        GORM models, migration runner, single Raw bypass
                                         ▼
                                   PostgreSQL (14 migrations)
```

**The spine of the codebase:** desired-state in PostgreSQL + observed-state in a
projection, with generation fencing. Every status change goes through a single
transition function (`fn_*_transition`) inside one transaction:
`FOR UPDATE → check transition map → status + version + 1 → append event`. The
table is the read source; the event log is append-only. This replaces
imperative "apply-and-wait" loops that hang forever when a dependency stalls.

### Control plane vs data plane

- **Control plane** (`loadtest-controller`, `cmds/loadtest-controller`): API,
  state machines, reconciler, RBAC, secrets. Runs in `ltp-system` (in-cluster)
  or as a local binary (compose).
- **Data plane** (`mock-service`, `cmds/mock-service`): separate binary, does not
  import controller packages. Metadata CRUD goes through namespace components;
  mock pools/endpoints are stubs deferred post-MVP.
- **Gateway** (`conf/nginx`): OpenResty + Lua, OIDC introspection, tenant header
  injection, authz reflection. Untouched by platform code — routing changes live
  in `conf/nginx`, not in Go.

## Three decisions (alternative → why rejected → cost today)

### 1. Namespace (environment) is the unit of isolation, not the test run

**Alternative:** each load test gets its own namespace / cluster-scoped sandbox.
**Why rejected:** creates, deletes, and NetworkPolicy-reconciles a namespace per
run; slow, noisy, and makes cross-run comparison (same SUT, different script)
awkward. A namespace is cheaper to create once and reuse across many runs.
**Cost today:** namespace teardown cascades everything (services, DBs, policies);
a leaked namespace leaks its database (CNPG cluster) too. Namespace delete is
202 + asynchronous GC — developers must poll, not assume instant cleanup.

### 2. Desired-state in Postgres + observed-state projection, with generation fencing

**Alternative:** imperative apply-and-wait (controller calls K8s, blocks until
READY, retries on failure).
**Why rejected:** apply-and-wait hangs when CNPG initdb or a Deployment rollout
stalls; the controller thread is tied up per namespace; no durable record of
*why* it is stuck. Generation fencing (`desired_generation` vs
`observed_generation`) means a stale apply result can never overwrite a newer
desired state, and the watcher projection is the single source of truth for
`ready_for_loadtest`.
**Cost today:** eventual consistency — `apply` returns 202 APPLYING and the
developer must `ns wait`. The projection can lag the cluster by a reconcile
tick (2s default). Debugging requires correlating `tbl_component_status` with
`kubectl get`.

### 3. Manifests rendered in Go, applied via SSA/kustomize — not Helm charts

**Alternative:** Helm charts for serve, dex, postgres, valkey.
**Why rejected:** Helm adds a templating layer and a release lifecycle that the
platform already implements in Go (desired state + apply + prune). SSA
(server-side apply / owned-fields pruning) lets the controller manage exactly
the objects it renders and nothing else; kustomize handles environment
variants (kind vs prod overlays).
**Cost today:** the controller must implement render + diff + prune for each
K8s kind (Deployment, Service, ConfigMap, Secret, HPA, SA, NetworkPolicy,
CNPG Cluster) — that's real Go code that a Helm chart would have "for free."
The trade-off pays off because the same render path is reused for per-service
and run-scoped NetworkPolicies.

## Trade-offs taken under time pressure

| Trade-off | What was given up | Revisit |
|-----------|-------------------|---------|
| **Quota ledger deferred** (gap 5) | Per-tenant reservation/429; only per-run caps (vusers/duration/timeout → 422) at create | plan 42 out of scope; ledger schema exists (`tbl_reservation_ledger`) |
| **k6 text-summary parse** instead of a metrics pipeline | `--summary-export` JSON; one parser over the pod-log text tail keeps local and K8s identical | plan 45; parser is fixture-tested against real k6 output |
| **Single-tenant-per-host assumption** | Multi-tenant host isolation; `tbl_tenant_hosts` maps host→tenant but no host-level rate limiting | post-MVP |
| **mock-service metadata CRUD stubs** | Mock pools, dynamic mock endpoints, stateful mock assertions | data plane is a separate binary; stubs are clearly marked |
| **Outbox table without dispatcher** | Reliable event delivery to external systems | `tbl_outbox_events` exists; writes happen, no loop consumes them |
| **Single `serve` AIO image** (nginx + controller + mock-service + alloy in one container) | Independent scaling, separate image vuln surface | keeps local stack simple; K8s overlay can split later |

## What I would do next (priority order)

1. **Run the k6 perf gate once** (`tests/perf/control-plane.js`, `admin.js`) and record p95/QPS in the README. The gate has never been executed (k6 binary not installed in CI).
2. **CodeGraph final review** (no duplicated code, no missing logic, no missing edge cases) + `scripts/bump.sh` release tag — the AGENTS.md quality gate before `main`.
3. **Wire the quota ledger** into loadtest create (429 per-tenant reservation) — schema exists, gate is deferred.
4. **Implement the outbox dispatcher loop** so outbox events are actually consumed.
5. **Plan 46 demo video** (pending confirmation) — record the kind E2E journey.
6. **Split the AIO serve image** into independent Deployments if K8s scaling becomes necessary.

## Traceability

Every claim above cites a file or plan. No unmeasured numbers appear (the perf
gate is explicitly marked "never run"). Quota ledger and perf gate are listed as
gaps, not achievements.

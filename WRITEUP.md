# WRITEUP — Self-Service Ephemeral Load Testing Platform (MVP)

Design decisions, the alternatives I rejected, and what each choice costs today.
Status is stated honestly: what was run, what was only written, what is missing.

## 1. Context I assumed

No clarifying questions were possible, so these assumptions drive every decision below.

- **100+ services**, owned by product teams (the brief says ~50; I plan for growth past 100, where per-team manual setup clearly cannot scale). DevOps is the bottleneck today (ticket → provision → deploy → hand over → ticket to tear down).
- **Kubernetes is the common runtime** for most services. Services already ship as containers with an OpenAPI spec and a health endpoint, so a service can be described declaratively. Non-K8s services are out of scope.
- **Load testing is a stage inside CI/CD**, not an occasional manual activity. Pipelines trigger runs (PR, merge, nightly) and nobody watches them live, so the primary interface must be machine-friendly: deterministic exit codes, idempotent calls, result output a pipeline can gate on.
- **Many teams, bursty concurrency.** Runs cluster around merge-time and nightly schedules, so expect dozens of concurrent environments, short-lived (minutes, not hours). Cost is dominated by orphaned environments and pile-ups, not by the load itself.
- **Packaged for reuse as a GitHub Action** that any team drops into its workflow, and possibly **a SaaS product later**. So tenancy is designed in from the start (tenant headers, RBAC, tenant-scoped data) even though the MVP has one deployment.
- **Users are developers, not infra people.** They should never touch k6 clusters, kubeconfigs or cloud credentials.
- **Shared cloud, shared cluster.** Teams must not be able to hit each other's services or reach production.
- **Cloud: GCP.** I know its billing model well (per-node vCPU/memory on GKE Standard, per-pod requests on Autopilot, egress, persistent disks, label-based cost export), so I can reason about cost guardrails with confidence rather than guess. Nothing in the control plane is GCP-specific; only the provisioning layer (Terraform) is.
- **SaaS readiness is partial by design:** tenant identity and RBAC are enforced at the API and data layer, but host-level rate limiting, per-tenant billing and hard tenant isolation are not built (`tbl_tenant_hosts` maps host → tenant only).

## 2. Developer experience

**Thesis:** a namespace is a reproducible, network-isolated test harness; a load test is a transactional state change on top of it.

The flow is six CLI steps, with exit codes a pipeline can act on (`0` ok, `1` runtime, `2` usage, `3` state/precondition):

1. `ns create --spec` — declare services and databases once.
2. `ns apply` — returns `202 APPLYING`.
3. `ns wait` — polls until `ready_for_loadtest=true`.
4. `script push` — uploads k6 JS (≤1 MiB), waits for async validation.
5. `run start` — only allowed when the namespace is READY and the script is VALIDATED, within per-run caps.
6. `run watch` / `run report` — parsed result: requests, RPS, p95/p99/avg, failure %, checks, thresholds.

Design choices that reduce friction: the environment is declared once and reused across many runs (compare same SUT, different script); `run report` never prints zeros for an uncollected run, it prints the collection state or parse error instead; every mutating call takes an `Idempotency-Key` and uses ETags, so retries from CI are safe.

## 3. Guardrails

| Guardrail | Mechanism | Status |
|-----------|-----------|--------|
| **Authz / tenancy** | nginx does OIDC/JWT and injects tenant headers; `pkgs/authz` maps them to a Principal with fail-closed RBAC | Implemented, tested |
| **Network isolation** | Per-service and **run-scoped NetworkPolicy**: while a run is alive, the runner can reach declared targets only | Asserted in the kind E2E (Calico) |
| **Per-run cost caps** | vusers / duration / timeout validated at create → `422` | Implemented |
| **Safe gates** | `409`/`422` if namespace not READY or script not VALIDATED | Implemented |
| **Secrets** | SOPS env resolver + AES-256-GCM field encryption | Implemented |
| **Audit** | Single transition function per state machine; append-only event log; a CI lint (`lint-transitions.sh`) rejects status changes that bypass it | Implemented |
| **Per-tenant quota / budget** | Reservation ledger → `429` | **Not wired** (schema `tbl_reservation_ledger` exists) |
| **Enforced teardown (TTL)** | Auto-delete namespaces after a deadline | **Not implemented.** Teardown is `ns delete` (202 + async GC) |

**The honest gap:** the original problem includes teams forgetting to tear down. Today a namespace lives until someone deletes it, and a leaked namespace also leaks its CNPG database. Caps bound the cost of a *run* but not of an *idle environment*. A TTL janitor on the existing reconciler is the first thing I would add (section 7).

## 4. Decisions and alternatives

### 4.1 Environment provisioning: namespace (environment) as the unit of isolation

- **Alternatives:** namespace per test run; dedicated cluster per team; serverless (Cloud Run jobs); VMs.
- **Picked:** one namespace per environment, reused across runs.
- **Why:** per-run namespaces mean create/delete/NetworkPolicy-reconcile on every run: slow and noisy, and cross-run comparison gets awkward. Dedicated clusters give the strongest blast-radius isolation but cost minutes to start and money while idle, which is the exact failure mode we want to avoid. Serverless/VMs would re-implement service discovery and dependency wiring that Kubernetes gives us.
- **Cost:** namespace teardown cascades everything; isolation is L3/L4 policy, not a hard tenant boundary; noisy-neighbour effects on shared nodes are possible.

### 4.2 Control model: desired state in Postgres + observed projection with generation fencing

- **Alternative:** imperative apply-and-wait (call K8s, block until READY, retry).
- **Why rejected:** it hangs when CNPG initdb or a rollout stalls, ties up a controller thread per namespace, and leaves no durable record of why it is stuck. With `desired_generation` vs `observed_generation`, a stale apply result can never overwrite newer desired state.
- **Cost:** eventual consistency. `apply` returns 202, the developer must `ns wait`, and the projection can lag the cluster by one reconcile tick (2 s default).

### 4.3 Manifests: rendered in Go, applied via server-side apply / kustomize, not Helm

- **Why:** the platform already implements desired state + apply + prune in Go; Helm would add a second templating and release lifecycle. SSA lets the controller own exactly the objects it renders. The same render path serves per-service and run-scoped NetworkPolicies.
- **Cost:** render + diff + prune is written by hand for each kind (Deployment, Service, ConfigMap, Secret, HPA, SA, NetworkPolicy, CNPG Cluster).

### 4.4 Load generator: k6 (k6-operator on K8s, local binary for compose)

- **Alternatives:** Locust, Gatling, JMeter.
- **Why k6:** tests are plain JS that developers can write, a single binary with low resource overhead per virtual user, thresholds are first-class (pass/fail maps to exit codes), and k6-operator runs it as a Kubernetes-native `TestRun`.
- **Result collection:** I parse the k6 text-summary tail from the runner pod log with one parser, so local and K8s backends produce the identical summary. `--summary-export` JSON was rejected because the K8s path can only read pod logs.
- **Cost:** text parsing is more brittle than a metrics pipeline (mitigated by fixtures from real k6 output); no time-series metrics or dashboards yet.

### 4.5 Developer interface: REST API + CLI (`ltp`), with a GitHub Action as the intended front door

- **Alternatives:** web portal; raw `kubectl` + CRDs; a GitHub Action built first.
- **Why:** since load testing lives in CI/CD, the interface has to be scriptable first and pretty second. The REST API is the contract; `ltp` is a thin client over it whose exit codes (`0/1/2/3`) already let a pipeline fail on a bad run. A GitHub Action is then a wrapper around the same six commands, and a portal or SaaS UI can be added later without touching the control plane. `kubectl`/CRDs would expose cluster credentials to every team.
- **Status:** the CLI works end to end. **The GitHub Action itself is not built yet**; today a team would call `ltp` from a workflow step.
- **Cost:** no browsable run history or PR-comment summary yet; results live in `run report` output.

### 4.6 Dependencies

- **Databases:** a real PostgreSQL per namespace via CloudNativePG, declared in the namespace spec. Seeded, disposable, and lives and dies with the environment.
- **Other services:** declared in the same spec and deployed for real; the target list comes from the service's OpenAPI index.
- **Mocks (data plane, HTTP only):** a service in the namespace spec can declare `mock: {enabled, routes}`. The controller renders the routes as a versioned snapshot (a ConfigMap), an `ExternalName` Service pointing at `mock-service` in `mock-system`, and injects a `MOCK_<SERVICE>_URL` env var into the system under test; NetworkPolicy allows egress to the mock on ports 80/8090. `mock-service` is a separate binary (it does not import controller packages) that swaps snapshots atomically with generation fencing and serves each route from weighted variants, with latency injection (fixed or lognormal), error injection and static or templated bodies. This keeps the load test deterministic against slow or failing third parties (payment gateway, auth provider) without calling them.
- **Mock status:** **unit-tested only** (`pkgs/mockruntime`: snapshot swap, generation fencing, route serving, latency injection, health). It is deployed in the kind E2E but receives no traffic there, because the sample service has no mock dependency and no spec fixture declares a mock route. The path from namespace spec to a k6 request served by a mock variant has therefore never been run end to end.
- **Mock limits:** HTTP only, exact method + path matching, stateless. Capacity pools and admission, database mock profiles, gRPC and stateful mocks are specified but deferred. The OpenAPI/HTTP scope matches what k6 scripts call; a gRPC dependency would need an HTTP gateway in front of it or the deferred gRPC support.
- **Cost:** a real DB per namespace is heavier than a mock, but results are more faithful. HTTP third-party dependencies can be mocked; gRPC, TCP and database-level mocks have no answer yet.

### 4.7 IaC and cloud layout (GCP)

- **Intended layout:** Terraform modules provision the GKE cluster and its surroundings; everything inside the cluster (namespaces, policies, workloads) is rendered and applied by the controller, not by Terraform. This keeps Terraform to long-lived infrastructure and avoids two tools owning the same objects.
- **Alternatives:** Crossplane / Config Connector (cloud resources as K8s objects, but a larger operational surface for a single cluster); Pulumi (no advantage here over Terraform's ecosystem).
- **Honest status:** I did **not have time to apply the Terraform modules**. The prototype was only exercised on a standard Kubernetes cluster (kind, Calico CNI, CNPG, k6-operator). Anything GKE-specific is therefore unverified, notably: NetworkPolicy enforcement needs GKE Dataplane V2 or the network-policy add-on enabled; Workload Identity, node pool sizing and quotas are untested.

### 4.8 CI/CD: GitHub Actions

Jobs: lint (`golangci-lint`), test (`go test -race`), build, SQL migrations + invariant suites, integration on kind, and two full-deploy E2E runs. All must be green before merge. Chosen because the repo lives on GitHub; there is no deploy-to-GKE stage yet because the cloud path is unverified.

## 5. Trade-offs accepted

| Trade-off | Given up | Why acceptable now |
|-----------|----------|--------------------|
| Namespace isolation, not cluster isolation | Hard blast-radius boundary | Cheap and fast; L3/L4 policy covers the stated threat (cross-team traffic) |
| Eventual consistency | `apply` is not instant | Avoids hung controllers; progress is durable |
| k6 text parse | Rich metrics | One parser for both backends; fixture-tested |
| Quota ledger deferred | Per-tenant budget / `429` | Per-run caps exist; ledger schema ready |
| No TTL teardown | Automatic cost safety for idle environments | Time-box; first item next. Matters more under CI, where failed pipelines abandon environments |
| No queueing / global concurrency limit | Protection from a burst of CI-triggered runs | Per-run caps exist; the quota ledger is the planned fix |
| SaaS-ready tenancy only at API/data layer | Hard isolation between external customers | Fine for internal teams; a SaaS would need per-tenant clusters or sandboxing |
| Real DB per namespace | Cost and startup time | Fidelity of results |
| Outbox table without dispatcher | Reliable external events | Writes exist, nothing consumes them |
| Single all-in-one `serve` image (nginx + controller + mock-service + alloy) | Independent scaling, smaller vuln surface | Keeps local stack simple |
| Mock runtime: HTTP-only, stateless, not in the E2E | gRPC / TCP / DB mocks, capacity pools, stateful mocks, and end-to-end proof of the mock path | Covers the HTTP dependencies k6 scripts actually call; unit-tested, with the E2E gap stated in section 6 |

## 6. What was and was not verified

- **Verified:** unit tests with the race detector, SQL invariant suites, CLI E2E against the compose stack, and a full-deploy E2E on kind + Calico. The E2E asserts: create-with-spec → READY via the status-watcher projection → run-scoped NetworkPolicies present while the run is live → `result_collected_at` within 30 s of `finished_at` with `http_reqs > 0` → run-scoped policies garbage-collected after the terminal state → namespace deleted from the cluster, with every transition in the event log.
- **Not verified:** the mock data plane end to end (unit tests pass, but `e2e-full-deploy.sh` builds and deploys `mock-service` and sends it no traffic, with no assertions on the mock bindings or on mock response headers); Terraform apply and anything on GKE; the k6 perf gate for the control plane (`tests/perf/*`, never run, so this document quotes no performance numbers); the outbox dispatcher; the TTL/quota guardrails above.
- **Known test-environment limits:** CI produces a coverage profile but enforces no threshold. On a macOS dev machine, `httptest`-based tests in `pkgs/ltpclient` and `pkgs/mockruntime` fail to bind the IPv6 loopback, and the DB-backed integration packages skip without PostgreSQL. These are environment issues, not logic failures; the Linux CI run with Postgres is the gate for them.

## 7. What I would build next (one more week)

1. **TTL janitor** in the reconciler: default deadline on every namespace, extend-with-justification, auto-delete, plus an idle-environment alert. Closes the biggest gap versus the original problem.
2. **Ship the GitHub Action** wrapping the six `ltp` commands, with an always-run cleanup step (so failed pipelines still tear down), a PR comment with the run summary, and thresholds as the pass/fail gate.
3. **Add a mock-path E2E** to the full-deploy suite: a namespace spec with a mock-enabled service, a k6 script that hits the mock routes, and assertions on the binding objects and on the `X-Mock-Generation`, `X-Mock-Reason` and latency behaviour. This turns the dependency-handling story from "unit-tested" into "proven".
4. **Wire the quota ledger and a global concurrency limit** (per-tenant reservation → `429`, queue when the cluster is saturated), which CI bursts make necessary.
5. **Apply and fix the Terraform modules on a real GKE cluster** (Dataplane V2, Workload Identity, node pools), re-run the full-deploy E2E there (it is kind/Calico only today), then add a deploy stage to CI.
6. **Cost attribution:** label every namespace and pod with tenant and run, export billing by label, show cost per run in `run report`. This is also the foundation for SaaS billing.
7. **Close the CI quality gaps:** enforce a coverage threshold (the profile is already produced), install k6 in CI and run the control-plane perf gate (`tests/perf/*`) recording p95/QPS, and bind `httptest` servers to IPv4 loopback so the suite also runs on macOS dev machines.
8. **Outbox dispatcher** for reliable events; split the all-in-one image if scaling requires it.

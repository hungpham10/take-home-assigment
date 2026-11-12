# AI-USAGE — Load Testing Platform (MVP)

Which AI tools were used, how they were configured, what I asked them, and how every output was verified before it reached `main`. This is an audit trail of *how* the code was produced and checked, not a claim of sole authorship.

## 1. Tools used

| Tool | Role in this project |
|------|----------------------|
| **opencode** | AI coding assistant: code generation, test drafts, CI failure analysis |
| **Claude + notebook** | Ideation and architecture, design docs, test plans, drafts of README / WRITEUP / AI-USAGE. The notebook was loaded with three O'Reilly books as grounding material: *Fundamentals of Software Architecture* (Richards and Ford, 2020), *Building Microservices: Designing Fine-Grained Systems* (Newman) and *Designing Data-Intensive Applications* (Kleppmann) |
| **CodeGraph MCP** ([codegraph-rs](https://github.com/hungpham10/codegraph-rs)) | Local semantic code graph served over MCP: exploring the codebase (symbols, call chains) before edits, and reviewing source after changes |
| **Assistant** ([hungpham10/assistant](https://github.com/hungpham10/assistant)): my own terminal coding agent, a Rust rewrite based on OpenAI's Codex, with MCP, skills, hooks and `AGENTS.md` support | Used as a review agent: cross-checks the design docs, the unit tests and the actual implementation against each other and reports mismatches |
| **golangci-lint** (`errcheck`, `revive`) | CI gate (`golangci/golangci-lint-action@v7`, `--timeout 5m`) |
| **`go test -race`** | Race detector on every PR; caught a plain-read counter race in `cmds/ltp/cli_test.go` |
| **SQL invariant suites** (`tests/sql/*_test.sql`) | Break an invariant → red; the TDD proof for state machines |
| **Harness matrix** (`tests/harness/`) | AUTHZ-01..04, IDEM, CONC, VAL, STATE-*, RUN-* cases |
| **`lint-transitions.sh`** | CI lint: status changes must go through the `fn_*_transition` functions |
| **kind full-deploy E2E** (`tests/e2e-full-deploy.sh`) | End-to-end check on kind + Calico + CNPG + k6-operator; the final arbiter of AI-written orchestration code |
| **GitHub Actions CI** | Lint, test, build, SQL migrations, K8s integration, two full-deploy E2E jobs |

## 2. Agentic workflow and configuration

**Workflow rules live in `AGENTS.md`** at the repo root, loaded by the agent at the start of every session:

- **Plan before code:** design docs and a numbered plan (`plans/01`–`46`, indexed in `plans/README.md`) are written and agreed before any implementation.
- **TDD:** a failing test exists before the implementation; the agent may draft test bodies, but I decide which invariant is being proven.
- **Quality gates:** a numbered checklist the agent must satisfy before calling a change done. Gate #8 is a CodeGraph review for duplicated code, missing logic and missing edge cases.
- **Ask-first rules:** the agent must stop and ask before merging to `main`, before changing nginx routing, and before any non-backward-compatible SQL change.

**Before the plans: architecture grounding.** I loaded architecture books into a notebook tool: *Fundamentals of Software Architecture* (Mark Richards and Neal Ford, O'Reilly, 2020), *Building Microservices: Designing Fine-Grained Systems* (Sam Newman, O'Reilly) and *Designing Data-Intensive Applications* (Martin Kleppmann, O'Reilly). I used them together with my own Kubernetes, GCP and security experience to break the assignment down and set the minimum requirements the platform must meet. The AI's part was retrieving and summarising what the books say and cross-referencing it with the brief; deciding what counts as the minimum bar for isolation, cost control, teardown and security was mine. **The resulting requirements are written down in `plans/01-repo-bootstrap.md` through `plans/46-...` (indexed in `plans/README.md`), with architecture decisions in `docs/architecture.md` and detailed API specs in `docs/*-api.md`.**

**Per-plan loop:** plan → docs → failing test → implementation → lint + `-race` → CI → human review → merge.

**CodeGraph MCP:** set up by following the README of [`hungpham10/codegraph-rs`](https://github.com/hungpham10/codegraph-rs): install the binary, run `codegraph init` at the repo root to parse the Go source with tree-sitter into a local graph (SQLite under `.codegraph/` by default), and serve it to the agent over MCP with `codegraph serve --mcp`. The agent then queries symbols, callers, impact and call flows instead of grepping, which is how changes were kept on existing call chains rather than duplicating logic. It is also used to review source after a change.

**For design document and source code review without re-indexing:** use `codegraph_init index=false` (or the MCP tool `mcp__codegraph_codegraph_init` with `index: false`) to bind to the existing `.codegraph/` workspace. This loads the pre-built symbol graph instantly for:
- `codegraph_search_symbol` — find symbols by name/pattern across `pkgs/`, `cmds/`, `tests/`
- `codegraph_callers` / `codegraph_callees` — trace call chains up/down from a function
- `codegraph_flow` — get control-flow markers (LOOP, IF_TRUE, THROW) + callee names with line numbers
- `codegraph_impact` — find transitive dependents of a symbol (what breaks if this changes)
- `codegraph_context` — composed view: symbol + callers + callees + optional source
- `codegraph_symbol` — full symbol detail (signature, doc, annotations, file:line)

**Assistant (custom review agent):** uses CodeGraph MCP to cross-check the three artifacts for each plan:
1. **Design doc/plan** (e.g., `plans/26-full-deploy-e2e.md`, `docs/network-policy.md`)
2. **Unit test invariants** (e.g., `pkgs/envreconciler/component_policy_test.go`, `pkgs/loadtest/reconciler_internal_test.go`)
3. **Actual implementation** (e.g., `pkgs/envreconciler/component_runnerpolicy.go`, `pkgs/loadtest/reconciler.go`)

**Review prompt given to Assistant:** *"Given plan X and its linked docs, use CodeGraph to: (a) find the implementation symbols for each documented behavior; (b) find the unit tests asserting those behaviors; (c) report any: documented behavior with no test, test asserting behavior not in code, code path not covered by tests, duplicated logic across packages, missing edge cases in error handling. Output as a findings list with file:line references."*

Its output is a list of findings that I verify and fix; it does not merge or edit on its own.

## 3. Representative prompts and outcomes

### 3.1 Full-deploy E2E kind bootstrap (plan 26)
- *Prompt:* "Write a kind cluster bootstrap script that: creates a cluster with disableDefaultCNI, pinned node image kindest/node:v1.31.0, pre-loads CNPG postgres + k6-operator + SUT images, installs Calico CNI for NetworkPolicy enforcement, applies CNPG and k6-operator manifests from pinned GitHub release URLs, and exports KUBECONFIG. Must handle SKIP_BOOTSTRAP=1 and KEEP_CLUSTER=1 env vars."
- *Outcome:* **Accepted** with minor fixes (node image pin, Calico install ordering). Became `tests/e2e-full-deploy.sh` lines 1–120.

### 3.2 Full-deploy E2E platform deploy (plan 26)
- *Prompt:* "Apply the platform via kustomize (deploy/k8s/overlays/kind): ltp-system namespace, serve AIO deployment (nginx + controller unix socket + mock-service + alloy), mock-system namespace with mock-service, postgres StatefulSet, valkey, dex. Use SOPS-encrypted secrets from deploy/k8s/base/serve/secrets.enc.yaml decrypted at boot via scripts/release.sh. Verify all deployments roll out healthy."
- *Outcome:* **Accepted**. Became `tests/e2e-full-deploy.sh` lines 121–200.

### 3.3 Full-deploy E2E namespace create-with-spec + apply + wait (plan 26)
- *Prompt:* "Create namespace with spec declaring: 1 CNPG database (cloudnative-pg operator) + 1 service (sample-service, port 8080, OpenAPI spec from examples/sample-service/openapi.yaml). POST /namespaces with spec → 201, POST /validate → VALIDATING→APPLYING, POST /apply → 202 APPLYING. Poll GET /readiness until ready_for_loadtest=true (Status Watcher projection: all components observed_generation == desired_generation). Assert Env Reconciler created K8s objects in lt-env-<ns> namespace."
- *Outcome:* **Accepted**. Became `tests/e2e-full-deploy.sh` lines 201–350.

### 3.4 NetworkPolicy probe against the sample SUT (plan 29)
- *Prompt:* "Add a busybox probe pod labeled loadtest.io/run-id=<run-id> that curls the SUT endpoint (SUT_PROBE_PATH=/todos) while the run is alive. Assert: probe succeeds (200 + JSON) → NetworkPolicy egress/ingress allows runner→SUT. Probe from outside the run label → assert connection refused/timeout → NetworkPolicy denies. Clean up probe pod after assertion."
- *Outcome:* **Rejected** a probe of `http://echo:8080/`. The sample service returns 404 on `/`, so `wget` fails with an empty body, indistinguishable from a NetworkPolicy denial. Now probes a known-good path (`SUT_PROBE_PATH`, `/todos`).

### 3.5 CLI tests (plan 44)
- *Prompt:* "Write CLI integration tests for cmds/ltp against the running compose stack. Tests must cover: ns create → validate → apply → wait (exit 0), script push (VALIDATED, oversize rejected exit 3), run start (gates: 409 if namespace not READY/script not VALIDATED, 422 if caps exceeded), run watch (exit 0=COMPLETED, 3=FAILED/STOPPED), run report (renders RPS/p95/p99, never prints zeros for uncollected), ns delete (DELETING→DELETED). Use dev auth headers (X-Tenant-Id, X-User-Id, X-User-Role). Assert JSON output is single object (--json)."
- *Outcome:* **Fixed** a plain (non-atomic) counter read in otherwise working tests. `go test -race` flagged it in CI; replaced with atomic access.

## 4. Division of labour

| Work | Human | AI |
|------|-------|----|
| Plans / design decisions | ✅ Decided; minimum requirements derived by me from the brief, the architecture books and my K8s / GCP / security experience; docs + plans before code | Grounded summaries from the notebook; drafted text and structure |
| Production Go code (`pkgs/`, `cmds/`) | ✅ Human-reviewed before merge | Draft implementations |
| Unit tests (failing first) | ✅ Chose the invariants | Draft bodies |
| SQL migrations + invariant tests | ✅ Authored | Draft schemas |
| nginx / k8s / Dockerfile / Earthfile | ✅ Reviewed | Drafted |
| CI workflow (`.github/workflows/ci.yml`) | ✅ Owned | Drafted |
| E2E scripts (`tests/e2e-*.sh`) | ✅ Owned (kind bootstrap, `SUT_PORT`, probe path) | Drafted |
| Failure diagnosis (CI logs → root cause → fix) | ✅ Owned | Suggested candidates |
| Docs (`docs/`, `README.md`, `WRITEUP.md`, this file) | ✅ Owned; supplied the assumptions (GCP choice, Terraform status, CI/CD-first use, SaaS direction) | Drafted; checked the drafts against the assignment brief |

**Per-plan map**

| Plan | AI helped with | Human-owned |
|------|----------------|-------------|
| 42 (loadtest caps) | `pkgs/loadtest/limits.go` draft, 422 codes | Startup-validated config knobs, wiring, tests |
| 43 (sample service) | `examples/sample-service/main.go`, `openapi.yaml`, Dockerfile | `make build-sample`/`kind-demo`, `SUT_PORT` parameterization, CI wiring |
| 44 (developer CLI) | `pkgs/ltpclient` typed client, `cmds/ltp` subcommands | Exit-code contract, `run report` rendering, `tests/e2e-cli.sh` |
| 45 (run results) | `pkgs/runner/summary.go` parser, reconciler `collectResult`, 0014 migration | K8s `PodLogs` tail capture, idempotent `result_collected_at IS NULL`, `result_parse_error`, E2E assertion |
| 41 (submission docs) | Drafts of README / WRITEUP / AI-USAGE / plans index | Every claim traced to a file or plan; no unmeasured numbers |

## 5. How I validated AI output

1. **Failing test first:** every feature has a red test before the implementation.
2. **`go test -race`** in CI (caught the counter race above).
3. **golangci-lint** (`errcheck`, `revive`): unchecked `fmt.*` returns and unused params blocked merge until fixed.
4. **SQL invariant suites:** a broken transition pattern fails fast.
5. **`lint-transitions.sh`:** blocks status changes that bypass the transition functions.
6. **kind full-deploy E2E:** real cluster, real NetworkPolicy enforcement (Calico), real k6 run, with assertions on results, policy cleanup and event log.
7. **Green CI before merge:** lint, test, build, sql-migrations, integration-k8s and both full-deploy E2E jobs.
8. **Assistant cross-check** of docs ↔ unit tests ↔ implementation using CodeGraph MCP. This is AI reviewing AI-assisted work, so it complements the mechanisms above and never replaces them; its findings are checked by hand.
9. **CodeGraph final review** (duplicates / missing logic / missing edge cases): **planned, not yet run.** CodeGraph has been used for lookups and one test/E2E/doc review pass, which is not the same gate.

## 6. What is *not* covered by this validation

Stated so a reviewer does not assume otherwise:

- **Terraform modules** have never been applied, so none of the mechanisms above has exercised them. Treat them as unverified.
- **The mock data plane** is covered by unit tests only; the kind E2E deploys it but sends it no traffic, so the mock path is unproven end to end.
- **Nothing has been run on GKE;** all E2E evidence is kind + Calico.
- **The control-plane perf gate** (`tests/perf/*`) has never been executed.
- **The GitHub Action** front end does not exist yet; only the CLI does.
- **Local test runs on macOS** hit environment failures (`httptest` IPv6 binding) and skip DB-backed tests; CI on Linux with Postgres is the gate.

## 7. What I would not delegate

- **Architecture boundaries** (control plane vs data plane, routing in `conf/nginx` not in Go): decided before code.
- **TDD discipline:** the AI drafts test bodies; I decide what invariant is being proven.
- **CI failure diagnosis:** reading raw job logs and telling "policy denies" apart from "404 masquerades as deny" needs a human in the loop.
- **Merge-to-main decisions:** per `AGENTS.md`, ask before merging to `main`, before breaking nginx routing, before non-backward-compatible SQL.

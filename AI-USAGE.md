# AI-USAGE — Load Testing Platform (MVP)

Tooling, division of labour, and the review mechanism that made AI-generated
code reviewable rather than trusted. Nothing here is a claim of authorship; it
is an audit trail of *how* the code was produced and *how* it was verified.

## Tooling used

| Tool | Role in this project |
|------|----------------------|
| **AI chat (this conversation)** | Plan drafting, code generation, test writing, failure analysis, doc authoring |
| **CodeGraph MCP** | Symbol/call-chain lookup before edits; post-change diff review ("no duplicated code, no missing logic, no missing edge cases") |
| **golangci-lint** | `errcheck` + `revive` enforcement; CI gate (`golangci/golangci-lint-action@v7`, `args: --timeout 5m`) |
| **go test -race** | Race-detector enforcement on every PR; caught a plain-read counter race in `cmds/ltp/cli_test.go` |
| **SQL invariant suites** | `tests/sql/*_test.sql` — breaking an invariant → red; TDD proof |
| **Harness matrix** (`tests/harness/`) | AUTHZ-01..04, IDEM, CONC, VAL, STATE-*, RUN-* cases — TDD: mutate an invariant, watch it go red |

## Division of labour

| Work | Human | AI |
|------|-------|----|
| Plans / design decisions | ✅ (AGENTS.MD workflow: docs + plans before code) | Draft text, structure |
| Production Go code (`pkgs/`, `cmds/`) | ✅ Reviewed every line | Draft implementations |
| Unit tests (TDD, failing first) | ✅ Wrote the failing tests | Draft bodies |
| SQL migrations + invariant tests | ✅ Authored | Draft schemas |
| nginx / k8s / Dockerfile / Earthfile | ✅ Reviewed | Drafted |
| CI workflow (`.github/workflows/ci.yml`) | ✅ Owned | Drafted |
| E2E scripts (`tests/e2e-*.sh`) | ✅ Owned (kind bootstrap, SUT_PORT, probe path) | Drafted |
| Failure diagnosis (CI logs → root cause → fix) | ✅ Owned | Suggested candidates |
| Docs (`docs/`, `README.md`, this file) | ✅ Owned | Drafted |

## Per-plan AI assistance map

| Plan | What AI helped with | What was human-owned |
|------|---------------------|----------------------|
| 42 (loadtest caps) | `pkgs/loadtest/limits.go` draft, 422 codes | Startup-validated config knobs, wiring, tests |
| 43 (sample service) | `examples/sample-service/main.go`, `openapi.yaml`, Dockerfile | `make build-sample`/`kind-demo`, SUT_PORT parameterization, CI wiring |
| 44 (developer CLI) | `pkgs/ltpclient` typed client, `cmds/ltp` subcommands | Exit-code contract, `run report` rendering, `tests/e2e-cli.sh` |
| 45 (run results) | `pkgs/runner/summary.go` parser, reconciler `collectResult`, 0014 migration | K8s `PodLogs` tail capture, idempotent `result_collected_at IS NULL`, `result_parse_error`, step 11a assertion |
| 41 (submission docs) | Drafts of README/WRITEUP/AI-USAGE/plans index | Every claim traced to a file/plan; no unmeasured numbers |

## Three examples of AI output that was **rejected**

1. **"Use `--summary-export` JSON for the k6 parser"** — AI suggested parsing k6's `--summary-export` JSON output. Rejected because the K8s path can only read the runner pod *log* (stdout/stderr), so local and K8s would need two parsers with two failure modes. One parser over the text summary tail (`pkgs/runner/summary.go`) keeps `runner.Status.Summary` identical for both backends (plans/45 §2).

2. **"Silence `kind load` failures with `2>/dev/null || true`"** — the first CI bootstrap draft swallowed image-load errors, producing `ImagePullBackOff` that took 10+ minutes to diagnose. Rejected: the SUT image load is now fatal (`fail "SUT image not found locally"`), and CI no longer pre-creates a conflicting kind cluster — the e2e script owns cluster bootstrap.

3. **"Probe the SUT on `/`"** — the L3/L4 runner→echo probe used `http://echo:8080/`, but sample-service returns 404 on `/`, making `wget` exit non-zero with an empty body — indistinguishable from a NetworkPolicy denial. Rejected: probe `http://echo:8080/todos` (`SUT_PROBE_PATH`), a known-good path that returns a body.

## What I would **not** delegate

- **Architecture boundaries** (control plane vs data plane, nginx routing in `conf/nginx` not in Go) — these define the system and were decided before code.
- **TDD discipline** (failing test first, then green) — the AI can draft test bodies, but the human decides what invariant is being proven.
- **CI failure diagnosis** — reading raw job logs, correlating timestamps, and distinguishing "policy denies" from "404 masquerades as deny" requires the human in the loop.
- **Merge-to-main decisions** — per AGENTS.md: ask before merging to `main`, before breaking nginx routing, before non-backward-compatible SQL changes.

## Review mechanism (what made AI output safe)

1. **Failing test first** — every feature has a red test before the implementation.
2. **`go test -race`** — CI runs with the race detector; plain atomic reads caught in `cli_test.go`.
3. **golangci-lint** (`errcheck`, `revive`) — unchecked `fmt.*` returns and unused params blocked merge until fixed.
4. **SQL invariant suites** — `tests/sql/*_test.sql` run in CI; a broken transition pattern fails fast.
5. **CodeGraph final review** — post-change graph check for duplicated code, missing logic, missing edge cases (AGENTS.md quality gate #8).
6. **Green CI before merge** — all jobs (lint, test, build, sql-migrations, integration-k8s, both full-deploy E2E) must pass on the feature branch.

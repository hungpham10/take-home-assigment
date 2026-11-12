# Load Testing Platform (MVP)

Self-service ephemeral load-testing platform. Namespace-first: one **Environment**
holds services/databases/ingress/dependencies; a k6 load test runs only when the
namespace is `READY`. Built with Go + go-chi, PostgreSQL, nginx, Docker Compose,
and kind/k6-operator for the full K8s path.

## What it does

| Piece | Role |
|-------|------|
| `cmds/loadtest-controller` | Control-plane BFF: namespace/component CRUD, OpenAPI validation, script/loadtest lifecycle, RBAC. DB: PostgreSQL. |
| `cmds/mock-service` | Data-plane mock runtime (stub metadata CRUD; pools/service endpoints deferred). |
| `cmds/ltp` | Developer CLI: namespace → script → run → report (exit codes 0/1/2/3). |
| `examples/sample-service` | Deterministic SUT demo (latency via `/slow`, error rate via `/flaky`). |
| `conf/nginx` | API gateway/routing (OIDC/JWT/tenant headers). |
| `sql/` | PostgreSQL migrations (single source of truth). |
| `deploy/k8s/` | K8s manifests (serve, dex, postgres, valkey, mock-system; overlays for kind/prod). |
| `tests/` | Unit, SQL invariant, CLI E2E, and full-deploy E2E (kind). |

## Quickstart

### Path A — local stack (compose)

```bash
make build                       # bin/loadtest-controller, bin/mock-service, bin/ltp
make up                          # docker compose up -d --build  (postgres + controller + dex + valkey)
./tests/e2e-smoke.sh             # ns → service → validate → apply → script → loadtest → delete
```

### Path B — kind (recommended full E2E)

Bootstraps its own kind cluster (pinned `kindest/node:v1.31.0`, Calico CNI, CNPG,
k6-operator), loads the sample-service image, and runs the complete
create-with-spec → READY → load test → results → cleanup journey:

```bash
make build-sample                # docker build -f examples/sample-service/Dockerfile -t ltp/sample-service:dev .
make kind-demo                   # ./tests/e2e-full-deploy.sh   (kind + CNPG + k6-operator)
```

`make kind-demo` is parameterised with `SUT_IMAGE`/`SUT_PORT`/`SUT_HEALTH_PATH` (default:
`ltp/sample-service:dev` on `8080`). It asserts run-scoped NetworkPolicies while the run
is live, verifies `result_collected_at` + `result_summary.http_reqs > 0` (plans/45), then
deletes the namespace and tears down the cluster unless `KEEP_CLUSTER=1`.

## Developer journey — six `ltp` commands

Requires `make build` and the stack reachable at `http://localhost:8080`.
Locally, nginx is not terminating OIDC, so pass dev-auth headers directly:

```bash
export LTP_SERVER=http://localhost:8080
TENANT=<uuid> USERID=<uuid>

# 1. namespace (spec = services + databases as raw JSON)
bin/ltp --tenant $TENANT --user $USERID ns create --name demo --slug demo \
  --spec tests/fixtures/ns-demo.json

# 2. apply (202 APPLYING on K8s; 200 READY on legacy)
bin/ltp --tenant $TENANT --user $USERID ns apply <ns-id>

# 3. wait — polls /readiness until ready_for_loadtest=true (exits 3 on timeout)
bin/ltp --tenant $TENANT --user $USERID ns wait <ns-id>

# 4. script push — uploads k6 JS, waits for VALIDATED (exits 3 on >1 MiB)
bin/ltp --tenant $TENANT --user $USERID script push <ns-id> tests/perf/sample-service.js

# 5. run start — gates on READY + VALIDATED + per-run caps (vusers/duration/timeout)
bin/ltp --tenant $TENANT --user $USERID run start <ns-id> --script <script-id> \
  --name demo-run --vus 20 --duration 1m --timeout 10

# 6. run watch — exits 0 on COMPLETED, 3 on any other terminal state
bin/ltp --tenant $TENANT --user $USERID run watch <ns-id> <lt-id>

# 7. run report — parsed k6 summary: http_reqs, RPS, p95/p99/avg, failure %, checks, thresholds
bin/ltp --tenant $TENANT --user $USERID run report <ns-id> <lt-id>

# cleanup
bin/ltp --tenant $TENANT --user $USERID ns delete <ns-id>
```

Global flags go before the subcommand: `--server`, `--token` (deployed path, `abt_`
bearer), `--tenant/--user/--role` (local dev-auth path), `--json` (one JSON document,
machine-readable), `--timeout`, `--wait-timeout`, `--poll`. Exit codes: `0` ok,
`1` runtime/transport, `2` usage, `3` state/precondition (409/422, run not COMPLETed).

Sample `run report` output:

```
run        7f3c9a1e-4b2d-4c8e-9f0a-1234567890ab
name       demo-run
status     COMPLETED
started    2026-11-12T06:02:17Z
finished   2026-11-12T06:02:28Z
collected  2026-11-12T06:02:28Z
  http_reqs     886,389   RPS 13207.09
  duration      p95 0.6ms   p99 0ms   avg 0.3ms
  failed        0.00%   checks 0/0 failed
  thresholds    OK
  ✓ http_req_failed rate<0.01
```

`run report` never prints zeros for an uncollected run; it prints the
`result_collected_at` state and any `result_parse_error` instead.

## `make` targets

| Target | What it does |
|--------|--------------|
| `make build` | Build `bin/loadtest-controller`, `bin/mock-service`, `bin/ltp` |
| `make build-sample` | Build `ltp/sample-service:dev` SUT image |
| `make up` / `make down` | Compose stack up/down |
| `make test` | `go test ./... -race` |
| `make lint` | `golangci-lint run` |
| `make version` / `make version-check` | Print / verify the build banner vs git tags |
| `make kind-demo` | Full kind E2E (sample SUT + CNPG + k6-operator) |
| `make test-cli` | CLI E2E (`tests/e2e-cli.sh`) against the compose stack |
| `make k8s-render` | Render a K8s overlay with the real image tag |

## Docs index

- `docs/` — architecture, API contracts, K8s orchestration, NetworkPolicy, RBAC, secrets, versioning, packaging.
- `plans/README.md` — plan index (01→46); Phase 2 deliverables are plans 41–45.
- `docs/mvp-status.md` — integration summary, gaps, and review checklist.
- `WRITEUP.md` — design decisions, trade-offs, and what comes next.
- `AI-USAGE.md` — tooling, division of labour, and rejected AI output.

## Known gaps (no code yet)

| Gap | Plan | Status |
|-----|------|--------|
| Quota ledger 429 (per-tenant reservation) | — | deferred; per-run caps (vusers/duration/timeout → 422) enforced at create (plan 42) |
| k6 perf gate never run (`tests/perf/*`) | — | install k6, run, record numbers |
| Outbox dispatcher loop | — | table + writes exist, no dispatcher yet |
| CodeGraph final review + `scripts/bump.sh` release | — | per AGENTS.md before release |
| Demo video | plan 46 | pending confirmation |

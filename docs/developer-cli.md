# Developer CLI — `ltp`

`ltp` is the developer-facing entry point to the self-service flow:
**namespace → script → run → report**, over the same BFF API that
`docs/loadtest-manager-api.md` documents. It replaces the six `curl` payloads a
developer would otherwise have to learn before their first load test.

- Source: `cmds/ltp` (flags, output, exit codes) + `pkgs/ltpclient` (typed client).
- Design/implementation plan: `plans/44-developer-cli.md`.
- Install: `make build` → `bin/ltp` (and `make install`-style copy to `$PATH`, or
  run `bin/ltp` directly).

> The client is a plain Go package too (`github.com/hungpham/loadtest-platform/pkgs/ltpclient`),
> so Go programs can embed the same flow without shelling out.

## Quick start

```bash
make build                       # bin/ltp, bin/loadtest-controller, bin/mock-service
export LTP_SERVER=http://localhost:8080

bin/ltp ns create --name demo --slug demo --spec tests/fixtures/ns-demo.json
bin/ltp ns apply  <ns-id>
bin/ltp ns wait   <ns-id>                # polls /readiness until READY

bin/ltp script push <ns-id> tests/perf/sample-service.js
bin/ltp run start <ns-id> --script <script-id> --vus 20 --duration 1m
bin/ltp run watch <ns-id> <lt-id>        # exits 3 unless the run COMPLETED
bin/ltp run report <ns-id> <lt-id>       # k6 summary: RPS, p95/p99, thresholds
bin/ltp ns delete <ns-id>
```

Every id in the transcript above is printed by the previous command, and every
command prints the literal next command it enables — you never have to guess.

## Global flags

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--server URL` | `LTP_SERVER` | `http://localhost:8080` | BFF base URL (no trailing slash) |
| `--token TOKEN` | `LTP_TOKEN` | – | `abt_` automation token → `Authorization: Bearer …` |
| `--tenant ID` | `LTP_TENANT` | – | dev auth only → `X-Tenant-Id` |
| `--user ID` | `LTP_USER` | – | dev auth only → `X-User-Id` |
| `--role ROLE` | `LTP_ROLE` | `user` | dev auth only → `X-User-Role` |
| `--json` | – | off | one JSON document on stdout, nothing else |
| `--timeout DURATION` | – | `30s` | per-HTTP-request timeout |
| `--wait-timeout DURATION` | – | `10m` | deadline for `ns wait` / `run watch` |
| `--poll DURATION` | – | `2s` | poll interval for `ns wait` / `run watch` |

Global flags go **before** the subcommand.

**Authentication.** In a deployed environment `--token` (or `LTP_TOKEN`) is the
only supported path: nginx validates the `abt_` token and injects the identity
headers. The `--tenant/--user/--role` flags exist for local runs where nginx is
not terminating OIDC; they set the same headers directly. Never mix the two —
`ltp` sends a bearer token *and* identity headers if both are configured.

## Commands

### Namespaces

```
ltp ns create   --name NAME [--slug SLUG] [--description TEXT] [--spec file.json]
ltp ns list
ltp ns show     <ns-id>
ltp ns validate <ns-id>
ltp ns apply    <ns-id>
ltp ns wait     <ns-id>
ltp ns delete   <ns-id>
```

- `create` takes the namespace spec (services, databases, mocks) as raw JSON
  (`--spec`), validated locally for syntax only; semantic validation is the
  server's job (`validate`).
- `apply` returns `202 APPLYING` on the Kubernetes path and `200 READY` on the
  legacy in-process path. The CLI prints what the server returned — it does not
  pretend the apply finished.
- `wait` polls `/readiness` until `ready_for_loadtest` is true. On timeout it
  prints the last status and **every** `blocking_reasons` entry to stderr, then
  exits `3`; it never hangs past `--wait-timeout`.
- `delete` returns as soon as the namespace is `DELETING`; the reconciler
  finishes the teardown (watch the API or `ns show` to follow it).

### Scripts

```
ltp script push <ns-id> <file.js> [--name NAME]
ltp script show <ns-id> <script-id>
```

- The 1 MiB ConfigMap limit is enforced **client-side**: an oversized file is
  rejected with exit `3` before any request is made.
- `push` does not return until validation is terminal (`VALIDATED` /
  `INVALID` / `VALIDATION_FAILED`). A non-validated script exits `3`, so a
  following `run start` is never rejected by the `SCRIPT_NOT_VALIDATED` gate.

### Runs

```
ltp run start  <ns-id> --script <script-id> [--name NAME] [--vus 20] [--duration 2m] [--timeout 15]
ltp run list   <ns-id>
ltp run watch  <ns-id> <lt-id>
ltp run stop   <ns-id> <lt-id>
ltp run report <ns-id> <lt-id>
```

- `--vus`, `--duration` and `--timeout` (minutes) are validated server-side
  against the operator caps from plan 42 (`MAX_VUS`, `MAX_DURATION`,
  `MAX_TIMEOUT_MINUTES`). Exceeding one returns `422` with a distinct code
  (`VUS_LIMIT_EXCEEDED`, `DURATION_LIMIT_EXCEEDED`, `TIMEOUT_LIMIT_EXCEEDED`),
  which the CLI surfaces verbatim and maps to exit `3`.
- `watch` polls until a terminal status. `COMPLETED` → exit `0`; every other
  terminal status (`FAILED`, `STOPPED`, `TIMED_OUT`, …) → exit `3`.
- `report` prints the collected k6 summary: request count + RPS, p95/p99/avg
  latency, failure rate, checks, and every threshold with ✓/✗.

```text
$ ltp run report demo-1 lt-9f3c
run        lt-9f3c
name       demo
status     COMPLETED
started    2026-11-12T10:04:11Z
finished   2026-11-12T10:06:13Z
collected  2026-11-12T10:06:15Z
  http_reqs     12,480   RPS 41.60
  duration      p95 82ms   p99 141ms   avg 61.20ms
  failed        0.31%   checks 39/12,480 failed
  thresholds    FAILED
                ✓ http_req_duration p(95)<200
                ✗ http_req_failed rate<0.01
```

`report` never prints zeros for a run whose summary was not collected. It
distinguishes three cases on stderr:

| API state | Report says |
|---|---|
| `result_collected_at` null | `result summary not collected yet (run is not terminal, …)` |
| `result_collected_at` set, `result_summary` null | `result summary unavailable: <result_parse_error>` |
| `result_summary.parse_error` set | the numbers are printed, plus `warning: k6 summary was partial: …` |

`report` exits `0` in all three cases — it is a reader, not a gate. Use
`run watch` for pass/fail (exit `3`), or inspect `thresholds_failed` under
`--json`. Note that `unreliable: true` (namespace `DEGRADED` after start) is
printed as a warning and is independent of the numbers.


## Exit codes

| Code | Meaning | Typical causes |
|---|---|---|
| `0` | success | – |
| `1` | runtime/transport failure | BFF down, 5xx, unreachable host, malformed response |
| `2` | usage error | unknown command/action, missing argument, missing `--script`, bad flag value |
| `3` | state/precondition failure | `409`, `422`, run ended not-`COMPLETED`, script not validated, wait timed out |

The split exists so scripts can tell "my request was legal but the platform is
not ready" (`3`) apart from "the platform broke" (`1`) without parsing messages.

```bash
if ! bin/ltp run start "$NS" --script "$SC" --vus 9999; then
  echo "run rejected (exit $?)"   # exit 3, VUS_LIMIT_EXCEEDED
fi
```

## Machine-readable output

`--json` writes exactly one JSON document to stdout — no progress lines, no
banner — so it pipes into `jq` directly. Errors always go to stderr, whatever
the mode.

```bash
bin/ltp --json ns show "$NS" | jq -r .status
bin/ltp --json run list "$NS" | jq -r '.[] | "\(.id)\t\(.status)"'
bin/ltp --json run report "$NS" "$LT" | jq '.result_summary.http_reqs'
```

## Scripting a CI gate

```bash
#!/bin/sh
set -eu
NS=$(bin/ltp --json ns create --name ci --slug ci --spec ns.json | jq -r .id)
trap 'bin/ltp ns delete "$NS" >/dev/null 2>&1 || true' EXIT
bin/ltp ns apply "$NS" >/dev/null
bin/ltp ns wait  "$NS" >/dev/null
SC=$(bin/ltp --json script push "$NS" tests/perf/sample-service.js | jq -r .id)
LT=$(bin/ltp --json run start "$NS" --script "$SC" --vus 10 --duration 30s | jq -r .id)
bin/ltp run watch "$NS" "$LT"     # exit 3 fails the gate
```

`tests/e2e-cli.sh` is this script against a real stack, with assertions.

## Verification

```bash
go test ./pkgs/ltpclient/... ./cmds/ltp/... -race
tests/e2e-cli.sh                    # needs the compose stack + k6 runner
bin/ltp --help                      # must list every flag documented above
```

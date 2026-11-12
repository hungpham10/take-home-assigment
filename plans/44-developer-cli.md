# Plan 44 — `ltp`: developer CLI for the self-service flow

**Status:** implemented (see "Deviations" at the end)
**Branch:** `feat/44-developer-cli`
**Depends on:** 42 (error codes), 43 (sample SUT for the demo)
**Type:** new `cmds/ltp` + `pkgs/ltpclient` + TDD
**Merge order:** third

## Problem

Every documented interaction with the platform is a raw `curl` against the BFF. `docs/mvp-status.md`
states the API surface exists; `tests/e2e-smoke.sh` is the only "client". That makes the
self-service story six payload shapes a developer must learn before their first run, and it makes
the request in the brief — a developer-facing way to use the platform — unmet.

Note: `pkgs/httpx` is a **server-side** response writer (it is not an HTTP client), so there is
nothing to reuse for outbound calls. The CLI is a real gap, not a packaging exercise.

## Design

Two layers, because the client is reusable and the CLI is presentation:

```
pkgs/ltpclient   — typed Go client over /api/loadtest/v1 (exported, unit-tested, no CLI types)
cmds/ltp         — flag parsing, table/JSON output, polling, exit codes
```

### Command surface (fixed here; plan 41 documents exactly these)

```
ltp ns create   --name demo --slug demo [--spec file.json]
ltp ns list
ltp ns show     <ns-id>
ltp ns validate <ns-id>
ltp ns apply    <ns-id>            # 202 + status_url; prints the follow-up command
ltp ns wait     <ns-id>            # poll /readiness until READY; on failure print blocking_reasons
ltp ns delete   <ns-id>            # 202 DELETING

ltp script push <ns-id> <file.js>  # enforces 1 MiB client-side, then polls validations
ltp script show <ns-id> <script-id>

ltp run start   <ns-id> --script <sid> [--vus 20] [--duration 2m] [--timeout 15] [--name demo]
ltp run watch   <ns-id> <lt-id>    # poll status; exit 3 if terminal status is not COMPLETED
ltp run stop    <ns-id> <lt-id>
ltp run report  <ns-id> <lt-id>    # plan 45; until then prints "results not collected"
```

Global flags: `--server` (env `LTP_SERVER`, default `http://localhost:8080`), `--token` (env
`LTP_TOKEN`, sent as `Authorization: Bearer`, enables the `abt_` automation path), `--json`
(machine output, one JSON object per command), `--timeout` (per-request, default 30s).

Exit codes: `0` success, `1` API/runtime error, `2` usage error, `3` state/gate failure
(`NAMESPACE_NOT_READY`, `SCRIPT_NOT_VALIDATED`, `VUS_LIMIT_EXCEEDED`, run not `COMPLETED`).
Exit 3 exists so CI/scripts can distinguish "your request was legal but the platform is not ready"
from "the platform broke".

### Client design (`pkgs/ltpclient`)

- `type Client struct { BaseURL string; Token string; HTTP *http.Client }`, one method per endpoint.
- `type Error struct { Status int; Code, Message string }` implementing `error`; **unknown codes
  are surfaced verbatim**, never swallowed, so new server codes need no client release.
- `Idempotency-Key: <uuid v4>` generated per mutating call (`ns create`, `ns apply`, `script push`,
  `run start`, `run stop`) — required by the server (`idempotency.RequireKey`).
- No retries on mutations. `ns wait`/`run watch` do bounded exponential backoff
  (1s → 5s, deadline `--wait-timeout`, default 10m) with an injected `sleep`/`now` for tests.
- Zero new module dependencies (stdlib `net/http`, `encoding/json`, `flag`). `spf13/pflag` is in
  `go.mod` only as an indirect transitive dep; taking it direct is not worth it for 12 flags.
  Nested subcommands are parsed with a small `switch` on `os.Args`, matching the existing
  `sops` subcommand style in `cmds/loadtest-controller/main.go:50-58`.

## Steps (TDD)

1. **Client tests first** (`pkgs/ltpclient/client_test.go`, `httptest`):
   - happy path per method (assert method, path, body, auth header, `Idempotency-Key` present);
   - 401 → `*Error{Code:"UNAUTHORIZED"}`;
   - 409 `NAMESPACE_NOT_READY` → error carrying `Code` verbatim;
   - 422 `VUS_LIMIT_EXCEEDED` (from plan 42) → message surfaced unchanged;
   - 202 + `status_url` response preserved for `ns apply`;
   - malformed JSON error body → still a useful error, never a panic.
2. **CLI tests** (`cmds/ltp/cli_test.go`, golden output):
   - flag parsing: missing `--script` → exit 2 with usage; unknown subcommand → exit 2;
   - `ns wait` transitions DRAFT→APPLYING→READY against a scripted fake → exit 0;
   - `ns wait` hits `blocking_reasons` → exit 3 and prints each reason;
   - `run watch` terminal `FAILED` → exit 3; `COMPLETED` → exit 0;
   - `--json` emits exactly one object on stdout and nothing on stdout for progress (progress → stderr).
3. Implement client, then CLI, keeping `main.go` thin (config + dispatch).
4. **Docs**: new `docs/developer-cli.md` (install via `make build`, full command reference, exit
   codes, the six-command journey rendered as a runnable block); add `ltp` to `Makefile`
   (`build` produces `bin/ltp` too) and to `Earthfile +build` + `+serve` (so the AIO image ships it).
5. **E2E**: `tests/e2e-cli.sh` — compose stack up → `ltp ns create` → `apply` → `wait` →
   `script push` → `run start` → `run watch` → assert exit 0. Wire into `make test-cli`
   (not into CI initially; flip to a CI job once it is stable twice).

## Verification

- [x] `go test ./pkgs/ltpclient/... ./cmds/ltp/... -race`
- [x] `ltp --help` and `ltp <cmd> --help` list every flag documented in `docs/developer-cli.md`
      (plan 41 diffs the two).
- [x] `tests/e2e-cli.sh` exists and is wired as `make test-cli`; it runs against `docker compose up -d`
      (not yet a CI job — see Deviations).
- [x] `ltp run start --vus 9999` → non-zero exit with the plan-42 code surfaced verbatim
      (exit 3 when the namespace exists, i.e. the 422 path; a 404 namespace is exit 1).
- [x] `ltp --json ns show <id> | jq .status` works (asserted by `TestJSONOutputIsOneObject` and the
      single-object check in `tests/e2e-cli.sh`).

## Out of scope

- A web UI. The CLI is the chosen developer surface; a UI would duplicate the same endpoints with
  more surface area.
- Admin/token commands (`abt_` issue/revoke) — operators keep using `curl`; blast radius stays small.
- Shell completion generation (cheap follow-up, not needed for the deliverable).
- `run report` content: command exists as a stub here, implemented in plan 45.

## Risks

| Risk | Mitigation |
|---|---|
| CLI drifts from server routes | Client tests assert exact paths built from one route constant per endpoint |
| Polling hides a stuck namespace | `wait` prints the last status + `blocking_reasons` on timeout, exit 3 |
| Adds a binary to the AIO image | `+serve` copies `bin/ltp` into `/usr/local/bin`; image size impact is one static binary |

## Deviations recorded during implementation

1. **`--poll DURATION` global flag** (default `2s`). The design hardcoded 1s→5s backoff; a single
   knob is simpler, testable (`TestPollIntervalFlagIsHonoured`) and lets the E2E keep a short
   deadline without sleeping for minutes. `--wait-timeout` remains the deadline.
2. **Dev-auth flags `--tenant` / `--user` / `--role`** (env `LTP_TENANT` / `LTP_USER` / `LTP_ROLE`).
   The design only had `--token`, but nginx terminates OIDC in front of the BFF: against the local
   compose stack there is no nginx, so the `abt_` path cannot be exercised end-to-end. The flags set
   exactly the headers nginx would inject (`X-Tenant-Id`, `X-User-Id`, `X-User-Role`) and are
   documented as development-only; `--token` stays the deployed path.
3. **Flags may follow positionals.** `flag.Parse` stops at the first non-flag argument, so
   `ltp run start <ns> --script <sc>` (the natural word order, and the one the design's own command
   table uses) silently lost its flags. `reorderForFlags` moves known flags ahead of positionals;
   both orders now work.
4. **`--version` / `version`** delegate to `pkgs/version.PrintIfRequested`. This is what the
   Earthfile version gate probes on every binary, so it is a build-system requirement, not feature
   creep. It works before any flag parsing or network access.
5. **Two create-response shapes.** `POST /namespaces` returns the namespace flat when no spec is
   given, and `{namespace, services, databases, ingress}` when a spec is. `CreateNamespaceWithSpec`
   accepts both and errors if neither carries an id; `--json ns create` therefore emits
   `{"namespace":{…},"services":[…],"databases":[…]}` (the server's shape, documented in
   `docs/developer-cli.md`).
6. **Earthfile `+build` was refactored, not extended.** The host-binary fast path was two hardcoded
   `sig_ctrl`/`mock_ok` blocks; adding a third binary triplicated it, so it is now one loop over
   `loadtest-controller mock-service ltp` with identical semantics (ELF check + version match).
   `+serve` copies `ltp` to `/usr/local/bin/ltp` so it is available inside the AIO image.
7. **`tests/e2e-cli.sh` is not a CI job yet** (as planned). It needs the compose stack plus a local
   k6 runner; it runs via `make test-cli`, and flips to CI once green twice.
8. **`run report` was delivered as a stub in this plan** (it said "not collected yet" on stderr
   rather than printing zeros) and was completed by plan 45, which also added the results JSON
   contract. See `plans/45-run-results.md`.

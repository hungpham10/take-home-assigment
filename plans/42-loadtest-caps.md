# Plan 42 — Load-test guardrails: vuser / duration / timeout caps

**Status:** implemented (2026-11-12)
**Branch:** `feat/42-loadtest-caps`
**Depends on:** — (independent)
**Type:** backend + TDD
**Merge order:** first

## Problem

`POST /api/loadtest/v1/namespaces/{id}/loadtests` accepts any `load_config` the client sends.
`defaultLoadConfig` (`pkgs/loadtest/handlers.go:275-281`) fills

```go
map[string]any{"vusers": 200, "duration": "10m", "timeout_minutes": 15}
```

and then overlays the request. There is **no upper bound and no parse validation**:

- `{"vusers": 50000}` creates a run that will exhaust the cluster (each VU is a goroutine in a
  k6 pod; 50k VUs is roughly 50k connections).
- `{"duration": "banana"}` is stored verbatim and passed to `k6 --duration banana` at
  `pkgs/runner/runner.go:87`, which fails **after** the DB row is created and provisioned —
  the operator sees FAILED with a k6 error instead of a 422 at submit time.
- `{"timeout_minutes": 1, "duration": "10m"}` produces a run that is guaranteed to be reaped as
  `TIMED_OUT` before it can finish (`pkgs/loadtest/reconciler.go:119` reaps at `timeout_at`).

Only the script size is capped (`size_bytes <= 1048576` in `sql/0004_script_loadtest.sql:19`).
`docs/loadtest-manager-api.md:43` already promises "Apply guardrails (vusers/duration/TTL/
concurrency/quotas)" — the vuser/duration part is unimplemented.

Quota enforcement (`tbl_reservation_ledger` → 429) is **explicitly deferred**; see Out of scope.

## Design

### Config knobs (`pkgs/config`)

| Env | Default | Meaning |
|---|---|---|
| `MAX_VUS` | `500` | hard ceiling per load test |
| `MAX_DURATION` | `30m` | hard ceiling for `config.duration` |
| `MAX_TIMEOUT_MINUTES` | `60` | hard ceiling for `config.timeout_minutes` |

Parsed in `FromEnv` with the existing `envInt` pattern; `MAX_DURATION` needs a new
`envDuration` helper (`time.ParseDuration`, error on invalid) mirroring `envCIDRs`.
Startup fails closed on an unparseable value — the same posture as `RUNNER_MODE`.

### Validation (`pkgs/loadtest/validate.go`, new file)

```go
type Limits struct { MaxVUs, MaxTimeoutMinutes int; MaxDuration time.Duration }

// ValidateLoadConfig returns a platform code + message; empty code = OK.
func ValidateLoadConfig(cfg map[string]any, l Limits) (code, msg string)
```

Rules, in order:

1. `vusers` missing or non-numeric → default 200 (existing behaviour). `< 1` → `VALIDATION_FAILED`
   "vusers must be >= 1".
2. `vusers > MaxVUs` → `VUS_LIMIT_EXCEEDED` "vusers 50000 exceeds the platform limit of 500".
3. `duration` unparseable by `time.ParseDuration` → `INVALID_DURATION`
   (accepts `30s`, `10m`, `1h30m`; k6 accepts the same syntax).
4. `duration < 10s` → `VALIDATION_FAILED` (typo guard: `"1"` must not become a 1ns run).
5. `duration > MaxDuration` → `DURATION_LIMIT_EXCEEDED`.
6. `timeout_minutes < 1` → `VALIDATION_FAILED`; `> MaxTimeoutMinutes` → `TIMEOUT_LIMIT_EXCEEDED`.
7. `timeout_minutes` must exceed `duration` (else the reaper kills a healthy run) →
   `TIMEOUT_TOO_SHORT` "timeout 1m must be greater than duration 10m".

Every code is added to `statusByCode` in `pkgs/httpx/httpx.go` mapping to **422**. (Distinct codes,
not the generic `VALIDATION_FAILED`, so the CLI in plan 44 can print a specific remedy.)

### Wiring

`loadtest.Deps` gains `Limits Limits`; `create` calls `ValidateLoadConfig` **before** the
transaction and before any DB write, so a rejected request costs one struct parse. `server.go`
passes `config.Config` values through. The reconciler is untouched — validation happens at the
edge, so nothing downstream can observe an out-of-range config.

## Steps (TDD)

1. **Failing tests first** in `pkgs/loadtest/validate_test.go` (table-driven, 12 cases):
   at-limit passes, over-limit fails, `vusers: 0`/`-1`/`"abc"` fail, `duration` `"30s"`/`"1h30m"`
   parse, `"banana"` fails, `"1"` fails, `duration > max` fails, `timeout <= duration` fails,
   timeout over max fails, empty config gets defaults and passes.
   Plus a handler test in `loadtest_test.go` asserting **422 + code** and that **no row is
   inserted** (`COUNT(*) == 0`) — the "reject before write" invariant.
2. `config` test for `MAX_VUS`/`MAX_DURATION`/`MAX_TIMEOUT_MINUTES`: defaults, overrides, and
   invalid values failing `FromEnv`.
3. Implement `validate.go`, `envDuration`, `Limits` wiring, `statusByCode` entries until green.
4. Update docs (below), then `go test ./... -race` + `golangci-lint run`.

## Docs

- `docs/loadtest-manager-api.md`: §5 guardrails table gains the three limits + the 422 code list;
  §3 error-code table gains the five new codes.
- `docs/architecture.md`: env/config section lists the three knobs.
- `docs/mvp-status.md`: gap #5 becomes "caps done (plan 42), quota ledger deferred".
- `deploy/k8s/base/serve/*` + `docker-compose.yml`: add the three env vars with defaults commented,
  so operators can see the knobs exist.

## Verification

- `curl -X POST .../loadtests -d '{"load_config":{"vusers":501}}'` → 422 `VUS_LIMIT_EXCEEDED`,
  `psql -c 'select count(*) from tbl_load_tests'` unchanged.
- `{"vusers":200,"duration":"10m","timeout_minutes":15}` → 201 as today (no regression).
- `MAX_VUS=10` in the compose env → the default 200 is rejected (proves the knob is live).

## Out of scope

- **Quota / reservation ledger (429)** — `tbl_reservation_ledger` stays unused; documented as a
  known gap. Wiring reserve→commit→release touches create + reconciler + reaper and deserves its
  own plan.
- Per-tenant (as opposed to per-test) limits: requires a tenant-limits table; not needed for MVP.
- Concurrency cap (max simultaneous RUNNING tests per namespace) — cheap to add later in the same
  `validate.go`, but out of scope here.

## Risks

| Risk | Mitigation |
|---|---|
| Existing tests/E2E create runs with configs above the new default | Defaults (500/30m/60m) are 2.5× the documented defaults; E2E uses 1–20 VUs |
| New 422 codes break clients that match on `VALIDATION_FAILED` | Grep first; the CLI (44) is the only new client and is taught the specific codes |

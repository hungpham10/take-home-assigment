# Plan 45 — Run results: collect, store, surface k6 summary

**Status:** implemented (kind E2E green on both `kind + CNPG` and `in-cluster` full-deploy jobs)
**Branch:** `feat/45-run-results`
**Depends on:** 44 (CLI `run report`), 43 (deterministic thresholds fixture)
**Type:** backend + migration + CLI + TDD
**Merge order:** fourth

## Problem

A load test today ends in a status and nothing else. `GET /loadtests/{id}`
(`pkgs/loadtest/handlers.go:280+`) returns `status`, `started_at`, `finished_at`,
`unreliable`, and `targets` — but no measurements. Concretely:

- `tbl_load_tests` (`sql/0004_script_loadtest.sql:74-96`) has **no result columns** (verified).
- `runner.Local.Status` (`pkgs/runner/runner.go:116-146`) discards k6's stdout entirely
  (`cmd.Stdout` is never set), so the summary is lost even in dev.
- `k8s.Client` (`pkgs/k8s/client.go:75-82`) exposes Apply/Get/Delete/List/Watch — **no pod logs**,
  so the K8s path cannot read the summary either.
- `docs/loadtest-manager-api.md:99-102` already specifies the surface
  (`result_summary`, `thresholds_failed`, `result_collected_at`) and says it is deferred.

The platform can tell you *that* a run finished, not *whether it passed*. For a load- testing
product that is the central output, and it is also what makes the demo in plan 41 meaningful.

## Design

### 1. Runner contract: carry the raw tail, not a parsed struct

`runner.Status` gains two fields:

```go
type Status struct {
    State string
    Msg   string
    Missing bool
    // Summary is the raw end-of-run k6 output tail (stdout for local, pod
    // log for k8s). Empty when the backend has nothing to offer.
    Summary    []byte
    SummaryErr string // set when retrieval failed (e.g. pod already GC'd)
}
```

Rationale: the runner should not know the result schema (it is a k6-version concern), and the
reconciler must be able to store a run as `FAILED` even when the summary is unavailable. Both
implementations fill it only on a **terminal** poll, so cost is one extra read per run.

- **Local**: set `cmd.Stdout = &p.out` (bytes.Buffer) in `Start`; on the terminal `Status` return
  `p.out.Bytes()`. The proc is removed from the table after `Status` returns today
  (`runner.go:138-141`) — read the buffer **before** removal.
- **K8s**: extend the interface with `PodLogs(ctx, ns, name string, tailBytes int64) ([]byte, error)`
  implemented on `DynamicClient` via a typed `kubernetes.Interface` (client-go is already a
  dependency; add the clientset to the struct and construct it in `NewClient`). `Fake` gains
  `PodLogsFn`/`Logs map[string][]byte` + a `Faults.PodLogsErr`.
  Pod selection: same label selector `Status` already uses (`loadtest.io/run-id=<ref>`,
  `runner=true`), container `k6`, `TailLines`/`LimitBytes` capped at 1 MiB.

### 2. Parser: `pkgs/runner/summary.go` (one implementation, both modes)

```go
type Summary struct {
    HTTPReqs int; RPS float64
    P95, P99, Avg float64 // milliseconds
    ErrorRate float64     // 0..1
    Checks, ChecksFailed int
    Thresholds []Threshold // {Name string; OK bool}
    ThresholdsFailed bool
    ParseError string
}
func ParseSummary(raw []byte) Summary // never panics, never returns error
```

Parse k6's end-of-run text report (the format printed by `k6 run`: `http_req_duration........:
avg=… p(95)=…`, `http_reqs…: N  RPS`, `http_req_failed…: X%`, `checks…`, and the
`█ THRESHOLDS` block with `✓`/`✗` lines). Text — not `--summary-export` — because the K8s path
can only read the pod log, and using two formats for the two backends would be duplicated logic.

Tolerance rules (each is a test):
- unknown/absent metric lines → zero value, no error;
- `THRESHOLDS` present with any `✗` → `ThresholdsFailed = true`;
- summary box present but truncated mid-line → parse what is complete, `ParseError` mentions it;
- nothing parseable (crash, empty log) → `ParseError = "no k6 summary found"`.

### 3. Storage: migration `0014_loadtest_results.sql`

```sql
ALTER TABLE tbl_load_tests
    ADD COLUMN result_summary     JSONB,
    ADD COLUMN thresholds_failed  BOOLEAN,
    ADD COLUMN result_collected_at TIMESTAMPTZ,
    ADD COLUMN result_parse_error TEXT;
```

Additive and nullable → backwards compatible (AGENTS.md gate respected; no data rewrite, no
transition-table change). `store.LoadTest` gains the four fields with `json:",omitempty"` on the
optional ones; the `result_summary` blob is the parsed `Summary` plus `k6_version` when detectable.

### 4. Collection in the reconciler

`Reconciler.finish(ctx, lt, to, actor, reason)` gains the `runner.Status` argument. Inside the
existing transaction, after `store.LTFinish`, it updates the four columns **only when
`result_collected_at IS NULL`** (idempotent: a retried tick never double-writes and never
overwrites a good summary with a later empty one). Retrieval failure still sets
`result_collected_at = now()` + `result_parse_error`, so the API can distinguish
"collected, nothing to report" from "never collected".

`reapTimeouts` (`reconciler.go:114-140`) records no summary: a killed run has no trustworthy
result — it sets `result_collected_at` + `result_parse_error = "run timed out before summary"`.

### 5. API + CLI surface

`GET /loadtests/{lt}` returns the new fields (omitted when null). `unreliable: true` (DEGRADED
after start) keeps its meaning and now sits next to the numbers — a consumed result must check both.
`ltp run report <ns> <lt>` renders:

```
run demo-1  COMPLETED  2026-11-12T10:04:11Z  (unreliable: no)
  http_reqs     12,480   RPS 41.6
  duration p95  82ms     p99 141ms   avg 61ms
  failed        0.31%    checks 12,480/12,480
  thresholds    ✓ p(95)<200  ✓ http_req_failed<0.01

result_summary JSON is at .result_summary (--json for machine output)
```

## Steps (TDD)

1. **Parser tests first** with real captured fixtures in `pkgs/runner/testdata/`:
   `pass.txt` (k6 v1.x, all thresholds green), `fail.txt` (one `✗`), `truncated.txt`,
   `empty.txt`, `v0_49.txt` (older layout). 20+ table assertions on parsed numbers.
2. **Local runner test**: `Local` with a fake binary (`sh -c 'echo …; exit 0'`) → terminal `Status`
   carries `Summary`; the buffer is not lost when the proc is removed; `-race` clean.
3. **k8s.Fake** test for `PodLogs` (hit, missing pod → `SummaryErr`, size cap).
4. **Reconciler tests**: COMPLETED run stores summary + `thresholds_failed=true`; FAILED run with
   no log stores `result_parse_error`; second tick leaves `result_collected_at` unchanged;
   TIMED_OUT sets its own reason. Use `store.LTFinish` path, assert via a real Postgres test
   (existing INT-* harness) since columns are involved.
5. **Migration test**: `tests/sql/` gains a case that 0014 is idempotent (`ADD COLUMN IF NOT EXISTS`)
   and that old rows read back with NULL result fields.
6. Wire `ltp run report` (from plan 44's stub) with a golden-output test.
7. Docs, then lint + `go test ./... -race`.

## Docs

- `docs/loadtest-manager-api.md` §8: replace "deferred" with the implemented contract + the
  `ParseError` / `result_collected_at` semantics and the `unreliable` caveat.
- `docs/database-schema.md`: migration 0014 columns.
- `docs/developer-cli.md`: `run report` output + `--json` shape.
- `docs/mvp-status.md`: results gap closed; note that a metrics **pipeline** (Prometheus/OTel
  remote-write) remains future work.

## Verification

- [x] `go test ./pkgs/runner/... ./pkgs/loadtest/... -race` (and full `go test ./...` green).
- [x] Local runner capture: `Local` with a fake k6 binary returns the summary tail on terminal
      status (including a threshold-failing exit) and is `-race` clean (`pkgs/runner/capture_test.go`).
- [x] K8s `PodLogs`: fake covers hit, missing pod, size cap (tail), and injected failure
      (`pkgs/k8s/client_test.go`); K8s runner attaches the log only on terminal polls and keeps the
      terminal status when the pod/log is unavailable (`pkgs/runner/k8s_test.go`).
- [x] Reconciler: stores summary + `thresholds_failed`, records retrieval/parse failure without
      failing the run, and is idempotent on retry (`result_collected_at IS NULL` guard); timeout
      records its own reason. Tests run against real Postgres.
- [x] Migration: `sql/0014_loadtest_results.sql` additive/nullable; `tests/sql/loadtest_results_test.sql`
      verifies columns, NULL-on-old-rows, JSONB round-trip, and FAILED+parse-error storage (rolled back).
- [x] `ltp run report` renders the summary, thresholds (✓/✗), and never prints zeros for an
      uncollected/unavailable run; `--json` emits the run payload unchanged (`cmds/ltp/cli_test.go`).
- [x] K8s end-to-end: `SUT_IMAGE=… tests/e2e-full-deploy.sh` reaches `result_collected_at` within
      30s of `finished_at` with `result_summary.http_reqs > 0` (step 11a). CI run 43197156840:
      `Full Deploy E2E (kind + CNPG)` green in 7m39s and `Full Deploy E2E (in-cluster)` green in
      9m42s on branch `feat/45-run-results` (PR #43). Full kind path (create-with-spec → READY →
      run → results → cleanup) confirmed.
- [ ] Pod deleted before collection → terminal status kept, `result_parse_error` set, API 200 (never
      500). Covered by unit/reconciler tests and the K8s "missing pod keeps terminal state" case; not
      yet exercised as a manual in-cluster probe.
- [ ] Local compose end-to-end with `tests/perf/sample-service-fail.js` (threshold fail) — not run in
      this pass (relies on a running stack + k6).

## Out of scope

- Time-series metrics (Prometheus/OTel remote-write, per-second samples). MVP stores one summary
  blob; a metrics pipeline is a separate plan and a different storage choice.
- `GET /loadtests/{id}/logs` (live streaming) — still unimplemented; not required for results.
- Threshold policy management (platform-enforced SLOs). The script owns thresholds; the platform
  reports them.
- Quota ledger (deferred in plan 42).

## Risks

| Risk | Mitigation |
|---|---|
| k6 changes its text summary format | Fixtures for two versions; `ParseError` degrades gracefully instead of failing the run; a future switch to `--summary-export` only touches `ParseSummary` callers |
| Runner pod GC'd before the reconciler polls | Terminal status is observed first (Job conditions), log read is best-effort; `SummaryErr` recorded |
| 1 MiB log tail truncated | k6 prints the summary at the end; tail is what we need. Assert the parser finds the summary in `truncated.txt` |
| Migration on a large table | Four nullable columns, no default rewrite → metadata-only in PG 11+ |

## Deviations recorded during implementation

1. **`Threshold.Metric` added** to the parsed/stored/wire threshold shape (`pkgs/runner.Summary`,
   `ltpclient.RunThreshold`) so the report can render `"✓ http_req_duration p(95)<200"` rather than
   only the expression; it is `omitempty` and the parser accepts layouts without a group line.
2. **`k8s.Client` grew `PodLogs`** (and `DynamicClient` now holds a typed clientset). The fake gained
   `Logs` + `Faults.PodLogsErr` and returns the tail capped to `tailBytes`, matching the real reader.
3. **`Reconciler.finish` signature changed** to `(ctx, lt, st runner.Status, to, actor, reason)` so
   collection happens in the same transaction as the terminal transition; `reapTimeouts` records a
   "timed out before summary" parse error via `collectResult`.
4. **YAML OpenAPI normalization (bugfix, unblocks kind E2E).** `tbl_services.openapi_spec` is JSONB
   but OpenAPI is often YAML; the sample service and kind E2E send `openapi.yaml`. `serviceopenapi.Normalize`
   converts YAML→JSON (JSON passthrough for hash stability) and is applied in `pkgs/namespace/spec.go`
   and `pkgs/namespace/services.go`.
5. **kind E2E image/Calico pins.** `tests/e2e-full-deploy.sh` pins `KIND_NODE_IMAGE`/`CALICO_VERSION`
   (overridable) because kind 0.33's default v1.37/Debian-13 node broke calico-node `mount-bpffs`
   (`libpcap.so.0.8` missing) with the previously pinned Calico manifest.
6. **E2E results assertion added.** Step 11a in `tests/e2e-full-deploy.sh` polls the API for
   `result_collected_at` and `result_summary.http_reqs > 0` (≤30s) instead of only checking terminal
   timestamps/events.
7. **Compose/local fail-threshold E2E and the manual "delete runner pod before collection" probe were
   not executed** in this pass; the former needs a live stack+k6, the latter a targeted in-cluster
   action. Both behaviors are covered by automated tests.

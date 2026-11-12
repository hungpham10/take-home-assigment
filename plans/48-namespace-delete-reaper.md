# Plan 48 — Namespace delete reaper (DELETING → DELETED auto-completion)

## Problem

`DELETE /namespaces/{id}` marks the row `DELETING` and `envreconciler.Tick`
deletes the K8s Namespace — but nothing ever completes the DB transition
`DELETING → DELETED`. `store.NSToDeleted` exists (`pkgs/store/namespace_lifecycle.go:90`,
"reaper" is named in the caller-owner table) yet has zero callers. Consequences:

- Real state deleted but DB stuck at `DELETING` forever (name can't be reused,
  zombie rows, misleading reporting).
- A K8s namespace stuck in `Terminating` (finalizer) retries forever silently —
  no deadline, no escalation surface.
- Existing `DELETING` branch in `Tick` also bypasses the HA leader gate
  (`reconciler.go:185` vs `:191`) — standbys delete K8s namespaces too.

## Non-goals

- No force-removal of finalizers (operator action required for stuck namespaces).
- No new status: DELETING stays DELETING until the K8s namespace is actually
  gone (level-triggered truth; `fn_namespace_transition` has no `DELETING → DEGRADED`
  seed and adding one would drop the env from `LoadEnvs` — the reaper loop).
- No watch on Namespace GVK — poll tick is sufficient (delete completion is rare).

## Design

Reaper lives in `envreconciler.Tick`, replacing the bare DELETING branch with
`reapDeleting(ctx, env)`:

1. Leader gate first (fixes the bypass bug).
2. `Get` the K8s Namespace:
   - **NotFound** → real state deleted → `store.NSToDeleted` (actor
     `envreconciler-reaper`, reason "k8s namespace gone"). Desync heals within
     one tick, no events needed.
   - **Terminating** (`DeletionTimestamp` set) → if past
     `DeletionDeadline` (default 10m from `deleting_since`) → log WARN once per
     namespace (`delete deadline exceeded — operator intervention required`),
     keep retrying. Otherwise wait.
   - **Active** (not terminating) → `DeleteEnv` as today.
3. `DeleteEnv` failure → return error; next tick retries (at-least-once, unchanged).

### Status writes behind an interface

`envreconciler` must not depend on `pkgs/store`'s DB directly (existing boundary:
"Reconciler never writes status back to PG" — relaxed deliberately, scoped to the
delete terminal transition only). New:

```go
type DeleteStatusSink interface {
    MarkDeleted(ctx context.Context, namespaceID, reason string) error
}
```

Wired in `cmds/loadtest-controller/orchestration.go` over `store.NSToDeleted`.
Sink nil → legacy behavior (delete-only, no DB write) so existing tests/harness
keep working.

### deleting_since

`Env` gains `DeletingSince time.Time`, populated by `LoadEnvs` via the
watcher's `pastApplyDeadline` pattern: last `to_status='DELETING'` row in
`tbl_namespace_events`, fallback `tbl_namespaces.updated_at`. No schema change.

## Config

`ENVR_DELETING_DEADLINE` (duration, default `10m`, negative disables)
in `pkgs/config`, passed through `envreconciler.Options.DeletionDeadline`.

## Files

- `plans/48-namespace-delete-reaper.md` (this), `docs/k8s-orchestration.md` (§delete
  flow + §HA note), `docs/namespace-status-watcher.md` §5 (reaper line now real).
- `pkgs/envreconciler/reconciler.go` — `DeleteStatusSink`, `Options.DeletionDeadline`,
  `reapDeleting`, leader gate, stuck-warn-once map.
- `pkgs/envreconciler/render.go` — `Env.DeletingSince`.
- `pkgs/envreconciler/db.go` — populate `DeletingSince` in `LoadEnvs`.
- `pkgs/config` — `ENVR_DELETING_DEADLINE`.
- `cmds/loadtest-controller/orchestration.go` — wire sink + deadline.

## TDD cases (`pkgs/envreconciler/reconciler_test.go`)

1. Namespace NotFound + sink → sink called with MarkDeleted, no DeleteEnv error.
2. Namespace active (no DeletionTimestamp) → DeleteEnv called, sink NOT called.
3. Terminating within deadline → no sink call, no error.
4. Terminating past deadline → warn logged (once), status stays DELETING.
5. DeleteEnv error → error surfaces (retry next tick), sink not called.
6. Sink nil → legacy behavior (DeleteEnv only).
7. Standby (isLeader=false) → no DeleteEnv, no sink call.

Integration harness: `LoadEnvs` returns `DeletingSince` from events table.

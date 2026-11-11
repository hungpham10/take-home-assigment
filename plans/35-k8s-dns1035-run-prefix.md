# Plan 35 — K8s resource names must be DNS-1035 compliant (prefix `run-`)

## Goal
Fix Full Deploy E2E failure where k6-operator TestRun stalls because k8s runner
renders resource names directly from `LT_ID` (a UUID that can start with a digit,
e.g. `3c1cb1c9-...`). Kubernetes object names must match DNS-1035:
`[a-z]([-a-z0-9]*[a-z0-9])?` — must start with a lowercase letter.

Failing CI error:
```
Service "3c1cb1c9-...-service-1" is invalid: metadata.name: Invalid value:
"3c1cb1c9-...-service-1": a DNS-1035 label must ... start with an alphabetic character
```

## Root cause
`pkgs/runner/k8s.go` uses `spec.ID` / `ref` (= `LT_ID`) directly as K8s object names:
- ConfigMap `name = spec.ID + "-script"` (Provision, line 146)
- TestRun   `name = ref` (Start, line 182)
- ConfigMap ref in TestRun spec `name = ref + "-script"` (Start, line 199)
- Job name  `jobName = ref + "-1"` (Status, line 282)
- Get/List/Delete TestRun by `ref` (Status/Stop, lines 273/283/295/345)

`pkgs/envreconciler/component_runnerpolicy.go:280` deletes ConfigMap
`runID + "-script"` — same prefix problem on cleanup.

Labels (`loadtest.io/run-id: <uuid>`) are fine — label values allow a leading
digit. Only object `metadata.name` is constrained.

## Fix
Add a small helper that prefixes object names with `run-` so they always start
with a lowercase letter, applied consistently everywhere a run-scoped K8s
object is named:

```
runObjectName(ref) = "run-" + ref
```

Affected:
- `pkgs/runner/k8s.go`: Provision ConfigMap name, Start TestRun name +
  script ConfigMap ref, Status Job name, Status/Get/List/Delete TestRun by
  `runObjectName(ref)`.
- `pkgs/envreconciler/component_runnerpolicy.go`: delete ConfigMap
  `runObjectName(runID) + "-script"`.
  (envreconciler cannot import runner — import cycle: runner imports envreconciler
  for `EnvNamespaceName`/`PlatformNet`. Define an equivalent one-liner helper
  there with a comment explaining why, to avoid duplicated logic sprawl.)

## Tests (TDD)
Failing test first in `pkgs/runner/k8s_test.go`:
- `TestK8sTestRunRunnerProvisionStartWithNumericRef`: Provision + Start with
  `ID = "3c1cb1c9-bc06-4d88-ad0f-d6a5cadb4b5f"` (digit-leading UUID from CI),
  assert:
  - ConfigMap created as `run-<id>-script` (not `<id>-script`)
  - TestRun created as `run-<id>`
  - TestRun `spec.script.configMap.name == run-<id>-script`
- Existing tests keep using letter-leading refs (`run-123`, `run-x`, …) and
  still pass (prefix `run-` keeps them DNS-1035).

## Docs
- `docs/loadtest-manager-api.md §5`: script source name is `run-<test_id>-script`
  (not `<test_id>-script`); K8s naming section mentions `run-` prefix for
  DNS-1035.
- `docs/k6-operator-go-client.md`: Option 1 sample shows deterministic
  run-scoped names with `run-` prefix.

## Scope / boundaries
- Do NOT change `loadtest.io/run-id` label value (still the raw UUID).
- Do NOT change `nsByRef` map key (still `spec.ID`/LT_ID).
- UDS between nginx and controller untouched.
- No schema change; no SQL migration.

## Definition of done
- `go test ./pkgs/runner/...` green (new numeric-ref test + existing tests).
- `go test ./...` green.
- `golangci-lint run` 0 issues.
- `sh -n tests/e2e-full-deploy.sh` OK.
- Commit + push on `feat/34-aio-serve-image` (PR #37); wait for CI green
  (Full Deploy E2E passes past `create-with-spec` and k6 run to COMPLETED),
  then squash-and-merge PR #37.

# Plan 43 — `examples/sample-service`: a real SUT instead of echo-server

**Status:** implemented (2026-11-12)
**Branch:** `feat/43-sample-service`
**Depends on:** — (independent; used by 44 CLI demo and 45 result fixtures)
**Type:** new example + build/E2E wiring
**Merge order:** second

## Problem

The platform can only be demonstrated against `ealen/echo-server:latest`
(`tests/e2e-full-deploy.sh:289`). That is a third-party image whose OpenAPI spec is a single
hand-written `GET /` (`{"title":"Echo","version":"1"}`), so:

- The demo has nothing meaningful to load test: no latency, no error path, no thresholds to fail.
- Plan 45 needs a run that produces a **passing and a failing threshold** to prove result
  collection; echo-server cannot produce either deterministically.
- A reviewer cannot tell "does the platform actually test my service?" from the demo.
- The OpenAPI → `targets[]` extraction (`pkgs/serviceopenapi`) is exercised with one trivial path,
  so multi-operation indexing is untested in E2E.

## Design

A tiny Go service, no third-party deps, that is *deliberately a plausible SUT*:

```
examples/sample-service/
├── main.go        # GET /healthz, GET /todos, POST /todos, GET /slow
├── openapi.yaml   # 4 operations, one target-worthy each
├── Dockerfile     # distroless/static, ~5 MB, matches Earthfile patterns
└── README.md      # how to run it and what each endpoint is for
```

Behaviour, chosen so load-test results are meaningful:

| Endpoint | Behaviour | Why |
|---|---|---|
| `GET /healthz` | always 200, no allocation | k8s readiness probe |
| `GET /todos` | serves an in-memory slice, JSON | the "fast" happy path |
| `POST /todos` | appends under `sync.Mutex`, 201 | write-path exercise |
| `GET /slow?ms=50` | sleeps `ms` (default 50, max 2000) | deterministic latency → p95 assertions |
| `GET /flaky?rate=0.1` | returns 500 with probability `rate` | deterministic-ish error rate → threshold failure |

Flags/env: `-addr` (`:8080`), `-latency-ms`, `-error-rate`. A `/flaky` call at rate 1.0 gives a
guaranteed failing `http_req_failed` threshold — the fixture plan 45 needs.

`openapi.yaml` is 3.0.3, and must validate under `kin-openapi` (that is the acceptance test).
Targets: `listTodos`, `createTodo`, `slow`, `flaky`.

## Steps

1. **Write the service** with a small unit test (`main_test.go`, `httptest`): `/healthz` 200,
   `/todos` round-trip, `/slow` honours `ms` and clamps at 2000, `/flaky?rate=1` returns 500,
   `/todos` serialises concurrent `POST`s (run with `-race`).
2. **Write `openapi.yaml`** and a test asserting `serviceopenapi.ValidateAndIndex` accepts it and
   yields exactly 4 targets with the expected operation IDs. This test is what keeps the example
   honest if the indexer changes.
3. **Dockerfile** (`golang:1.26-alpine` build → `gcr.io/distroless/static`), mirroring the existing
   multi-binary Dockerfile style used by `cmds/*`.
4. **Build wiring**: `Makefile` target `build-sample`; `Earthfile` target `+sample-service`
   (so kind can `kind load` it without a registry); **do not** touch the `+serve` image.
5. **E2E wiring** in `tests/e2e-full-deploy.sh`: add a `SUT_IMAGE` variable defaulting to
   `ealen/echo-server:latest`, and a new `SUT_IMAGE=ltp/sample-service:dev` path that
   (a) loads the image into kind, (b) creates the service with `openapi.yaml` content,
   (c) asserts `targets[]` length is 4, (d) curls `GET /todos` after apply.
   The existing assertions on echo's JSON body stay behind the default branch so the script keeps
   working without a sample build.
6. **Docs**: `README.md` inside the example; one row in `docs/mvp-status.md` §5 (E2E matrix);
   link the example from `docs/architecture.md` "how a developer uses this".

## Tests / verification

- [ ] `go test ./examples/... -race`
- [ ] `go vet ./examples/...`; `golangci-lint run ./examples/...`
- [ ] `docker build -t ltp/sample-service:dev examples/sample-service && docker run -p 8080:8080 …`
      → `curl localhost:8080/todos` returns `[]`.
- [ ] `k6 run tests/perf/sample-service.js --vus 5 --duration 10s` against the local container
      (new tiny script, no thresholds) → no errors.
- [ ] `SUT_IMAGE=ltp/sample-service:dev tests/e2e-full-deploy.sh` reaches READY and the
      post-apply curl returns the todo list.

## Out of scope

- A database for the sample (in-memory only, explicitly stated in its README).
- Multi-service sample topology (one service is enough to prove the flow).
- Publishing the image to a registry — kind loads it locally; CI builds it only on demand.

## Implementation notes (2026-11-12)

Deviations from the design above, recorded per AGENTS.md:

1. **No Earthfile target.** `+build`/`+serve` are the release path; adding a third binary there
   introduces risk for no deliverable value. Instead: `examples/sample-service/Dockerfile`
   (build context = repo root) + `make build-sample`. CI does not build it in the lint/test jobs;
   the kind E2E job builds and loads it locally.
2. **kind is the reference path; there is no compose variant.** `tests/e2e-full-deploy.sh`
   defaults to `SUT_IMAGE=ltp/sample-service:dev` (loaded into the kind node by the script) and
   passes the example's real `openapi.yaml` as the service spec, so the platform's target index
   is exercised with 5 operations. The historical `ealen/echo-server` demo remains reachable by
   overriding the five `SUT_*` variables. `make kind-demo` = build image + full E2E.
3. **5 indexed operations, not 4** — `/healthz` is included so the spec is a complete description
   of the service (and the container probe has a target).
4. **Deployment is via the namespace spec, not a bespoke manifest.** No platform special-casing:
   the sample is a generic service component image, which is what makes it a valid demo.
5. **Container healthcheck is the binary probing itself** (`-probe <url>`), because the image is
   distroless and has neither shell nor curl — and Kubernetes exec probes would need a shell.
6. Added the two k6 fixtures the results plan needs: `tests/perf/sample-service.js` (green
   thresholds) and `tests/perf/sample-service-fail.js` (guaranteed failing threshold via
   `/flaky?rate=1`).

## Risks

| Risk | Mitigation |
|---|---|
| Adds a new CI build path that could slow CI | Sample image is built only in the `full-deploy-e2e` job, not in `lint`/`test` |
| `/flaky` timing makes E2E flaky | E2E uses `rate=0`/`rate=1` only (deterministic); the random path is unit-tested with a fixed seed |
| Example drifts from the OpenAPI indexer contract | The `ValidateAndIndex` assertion test in step 2 fails on drift |

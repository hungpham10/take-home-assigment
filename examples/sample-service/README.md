# Sample service — system under test for the load-testing platform

A deliberately small HTTP service used by the platform demo and E2E tests. Stdlib only, state in
memory, no auth. It exists because `ealen/echo-server` cannot produce deterministic latency or
error rates, which are what make a load-test *result* meaningful.

## Run locally

```bash
go run ./examples/sample-service -addr :8095
curl localhost:8095/todos
```

## Endpoints

| Endpoint | Behaviour | Used for |
|---|---|---|
| `GET /healthz` | `200 ok`, no allocation | readiness / container probe |
| `GET /todos` | in-memory list as JSON | fast happy path |
| `POST /todos` | `{"text":"..."}` → `201`, mutex-guarded append | write path / contention |
| `GET /slow?ms=50` | sleeps `ms` (clamped to 2000) | deterministic p95 assertions |
| `GET /flaky?rate=0.1` | `500` with probability `rate` (0..1) | threshold pass/fail fixtures |

Flags: `-addr`, `-latency-ms` (default latency for `/slow`), `-error-rate` (default for
`/flaky`), `-probe <url>` (self healthcheck; distroless images have no shell).

`rate=0` and `rate=1` are deterministic, so E2E uses only those two values.

## Container

The Dockerfile needs the repository root as build context (Go module lives there), and the image
is distroless (no shell, no curl):

```bash
make build-sample        # docker build -f examples/sample-service/Dockerfile -t ltp/sample-service:dev .
```

Healthcheck works without curl — the binary probes itself:

```bash
docker run --rm --entrypoint /usr/local/bin/sample-service ltp/sample-service:dev \
  -probe http://127.0.0.1:8080
```

## Run it on kind (the reference path)

`tests/e2e-full-deploy.sh` defaults to this image: it loads `ltp/sample-service:dev` into the kind
node, creates a namespace whose service spec uses `openapi.yaml`, waits for READY, curls
`/todos` through a port-forward, runs a k6 load test against the service, then deletes the
namespace and verifies the K8s namespace is gone.

```bash
make kind-demo            # build-sample + full kind E2E (bootstraps kind/CNPG/k6-operator)
```

Or step by step:

```bash
make build-sample
SUT_IMAGE=ltp/sample-service:dev SUT_PORT=8080 \
SUT_HEALTH_PATH=/healthz SUT_PROBE_PATH=/todos SUT_K6_URL=http://echo:8080/todos \
  tests/e2e-full-deploy.sh
```

The platform renders Deployment/Service/HPA from the namespace spec, so nothing platform-side is
special-cased for this image. The historical `ealen/echo-server` demo is still available:

```bash
SUT_IMAGE=ealen/echo-server:latest SUT_PORT=80 SUT_HEALTH_PATH=/ \
SUT_PROBE_PATH=/ SUT_K6_URL=http://echo:80/ tests/e2e-full-deploy.sh
```

## Tests

```bash
go test ./examples/... -race
```

`TestOpenAPIIndexesFourTargets` validates `openapi.yaml` through `pkgs/serviceopenapi` (the same
indexer the control plane uses), so the spec and the platform cannot drift apart silently.

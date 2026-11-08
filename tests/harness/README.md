# Harness Tests (placeholder)

This directory is reserved for **integrity/ACID/fault-tolerance harness tests** (crash, exception, network faults, retries, concurrency) as defined in:

- [`docs/harness-testing.md`](../../docs/harness-testing.md)
- [`plans/16-harness-testing-integrity.md`](../../plans/16-harness-testing-integrity.md)

## Layout (planned)

```text
tests/
  sql/                 # DB invariants/state-machine harness (CI, migrated Postgres)
  harness/
    README.md
    k8s/               # //go:build harness_k8s — NetworkPolicy/egress/runner isolation (cluster)
    watcher/           # //go:build harness_k8s — WATCH-01..06 (plans/22, real cluster)
  integration/         # Go integration harness (Postgres + fakes; phased)
  perf/                # k6 performance gates (separate from integrity harness)
```

## Status Watcher harness (WATCH-01..06, plans/22)

Run against a real cluster — colima locally, kind in CI (job `watcher-harness` in `.github/workflows/ci.yml`):

```sh
# local (colima)
colima start --kubernetes
export TEST_DATABASE_DSN=postgres://ltp:ltp@localhost:5432/ltp?sslmode=disable
export KUBECONFIG=~/.colima/_lima/colima/sshconfig  # hoặc: colima kubernetes config > kubeconfig
go test -tags harness_k8s -v ./tests/harness/watcher/...
```

`TestWATCH00Setup` seeds tenant/namespace/service rows vào Postgres và apply
deployment thật lên cluster qua `pkgs/envreconciler`; các case WATCH-01..06
chạy trên đó. Client build từ `KUBECONFIG` (TLS certs — bắt buộc cho
colima/kind vì cert self-signed) hoặc bare `K8S_API_URL` fallback.
Fake-client counterparts (không cần cluster) nằm ở `pkgs/statuswatcher/`
và chạy trong job `test`.

CI: job `integration-colima` trong `.github/workflows/ci.yml` cài colima qua
Homebrew trên ubuntu runner (qemu, không KVM — start 3-6 phút, retry x2) và
chạy cùng harness. Nếu colima flaky trên runner, đổi sang kind chỉ cần 1
step: `uses: helm/kind-action@v1` + export KUBECONFIG của kind.

## Rules
- Harness tests prove **safety under failure**, not happy paths.
- Use deterministic fakes/hooks (clock, runner, K8s adapter, outbox duplicate delivery, ref resolver faults). No production-only backdoors.
- Never log secrets/tokens/PII; redact idempotency keys in artifacts if sensitive.
- K8s/NetworkPolicy cases require a real cluster/CNI; tag with `harness_k8s` and run in cluster-capable CI/manual gate until automated.

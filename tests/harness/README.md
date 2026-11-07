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
  integration/         # Go integration harness (Postgres + fakes; phased)
  perf/                # k6 performance gates (separate from integrity harness)
```

## Rules
- Harness tests prove **safety under failure**, not happy paths.
- Use deterministic fakes/hooks (clock, runner, K8s adapter, outbox duplicate delivery, ref resolver faults). No production-only backdoors.
- Never log secrets/tokens/PII; redact idempotency keys in artifacts if sensitive.
- K8s/NetworkPolicy cases require a real cluster/CNI; tag with `harness_k8s` and run in cluster-capable CI/manual gate until automated.

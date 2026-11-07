# Plan 16 — Harness Testing: Integrity, ACID & Fault Tolerance

## Mục tiêu
Thiết lập **harness test** chứng minh metadata/control-plane **an toàn và ổn định** dưới lỗi: crash, exception, network lỗi, timeout, retry, concurrency, partial outage. Harness bổ sung (không thay thế) unit/integration/E2E/k6 perf; tập trung vào **ACID/invariant/idempotency/fail-closed/recovery**.

## Dependencies
- Nền tảng: Plan 03–07 (SQL/schema/invariants), Plan 08–13 (controller/runner/reconciler/idempotency/outbox/authz).
- Tham chiếu: `docs/harness-testing.md` (contract), `tests/sql/*` (pattern hiện có), `plans/14-local-stack-e2e.md`.

## Deliverables

### Docs (đã/đồng bộ)
- `docs/harness-testing.md` — taxonomy, ID matrix, ACID mapping, fault-injection controls, CI gates.
- Link từ `docs/architecture.md` / `plans/README.md` nếu cần (update khi implement).

### SQL harness (mở rộng `tests/sql/`)
- Bổ sung case cho invariant chưa cover: idempotency request-hash/conflict policy (theo impl), outbox+state same-tx rollback proof (qua app tx test bổ trợ), reservation concurrent commit/release, terminal-state/event append-only theo domain, cross-tenant FK/scoping nơi áp dụng.
- Giữ pattern transaction + `RAISE EXCEPTION`/`DO $$` như harness hiện có; chạy trong CI `sql-migrations`.

### Go application harness
- `pkgs/*` interfaces/fakes cho clock, runner, K8s client adapter, outbox dispatcher, ref resolver (test-only; build tag `harness` hoặc injected interfaces).
- Integration harness (Postgres thật): `API-IDEM-*`, `API-CONC-*`, `API-AUTHZ-*` fail-closed, `API-VAL-*`, `API-CRASH-*`/outbox duplicate delivery simulation.
- Runner harness: fake K8s (`already exists`, timeout, stale status), LocalRunner process crash/timeout/stop idempotency; assert transition events + no false completion.

### K8s/NetworkPolicy harness (cluster-capable, phased)
- `tests/harness/k8s/` (`//go:build harness_k8s`): verify run-scoped egress **NetworkPolicy default deny** + allowed targets/deps/mocks/DNS only; runner has no K8s/platform creds. Chạy khi có cluster/CNI; bắt buộc trước claim production runner isolation.

### CI/reporting
- Job/unit targets: `test-sql-harness`, `test-harness` (race where practical), optional `test-harness-k8s`.
- Harness failure output: IDs/tenant/state/version/idempotency key (redacted) — no secrets.

## Verification (acceptance)
- TDD cho mỗi harness case mới: case fail khi invariant bị phá (hoặc mô phỏng fault) và pass khi behavior đúng.
- SQL harness xanh; API/runner harness xanh trên core matrix (`DB-TXN-01..05`, `API-IDEM-01`, `API-AUTHZ-01/02`, `API-CONC-01`, `RUN-STOP/TIMEOUT/IDEM` tối thiểu).
- K8s egress harness có bằng chứng chạy (CI cluster hoặc manual artifact) cho thay đổi chạm runner/network isolation.

## Ghi chú / rủi ro
- **Distributed ACID caveat:** không claim ACID xuyên DB+K8s+S3; chứng minh bằng outbox + idempotent reconcile + cleanup.
- External S3/nginx chaos trong MVP dùng fake/resolver fault injection; full network chaos (tc/Chaos Mesh) là phase sau.
- Mock-service runtime harness phụ thuộc snapshot contract finalization (architecture open item).

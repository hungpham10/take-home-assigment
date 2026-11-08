# Plan 24 — Phased harness coverage PH1: docs sync, DB-TXN-06..10, fault-injection, INT labeling

## Goal
Audit coverage (2026-11-08) xác nhận: go test xanh nhưng danh mục INT chỉ ~24/~130 IDs; DB-TXN-06..10, API-IDEM-02/03, RUN-NET-02, MOCK-VER-01 e2e còn doc-only; docs drift (namespace-status-watcher.md stale, INT-DS-01 đánh 🔜 dù test đã có). PH1 đóng các gap giá trị cao nhất.

## Dependencies
- Plans/21/22 (watcher + harness WATCH-01..06 trên kind).
- `tests/sql/harness_test.sql` (DB-TXN-01..05 pattern).
- `docs/harness-testing.md` §11 catalog.

## Deliverables
1. **Docs sync**: namespace-status-watcher.md (WATCH implemented), harness-testing.md (INT-DS-01 un-🔜, §14 gap update), plans/README row.
2. **DB-TXN-06..10** SQL tests: outbox atomicity, reservation ledger, FK/tenant scoping, append-only events, readiness invariant (CHECK nếu thiếu → migration 0012).
3. **API-IDEM-02/03**: in-flight duplicate 409; replay response của lần đầu 202.
4. **RUN-NET-02** egress thật trên kind (conditional skip khi CNI không enforce); **MOCK-VER-01** e2e (schema_version reject + generation mismatch → PROGRESSING); **RUN-CRASH-01** gán nhãn WATCH-04.
5. **INT labeling**: adminapi → INT-ADM-*, script → INT-SCR-*, envreconciler → INT-ENV-*, statuswatcher → INT-LOOP-01/04/05 (rename, không đổi logic) + §14.1 mapping.

## Out of scope (plans/25+)
INT-LT-12 quota, INT-DS-02..04, INT-LOOP-02/03 (restore Job), EXT-*/NGX-*, k6 gate, K8s k6 runner, API-OUTBOX-* (cần outbox dispatcher).

## Verification
- go test -race ./... + golangci-lint; psql harness_test.sql xanh trong CI; kind egress PASS/conditional-skip; PR mới chờ review.

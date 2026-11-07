# Plans Index

Thứ tự thực thi từ đơn giản → phức tạp. Mỗi plan = 1 PR/feature branch, TDD, docs trước code.

| # | Plan | Phạm vi | Dependencies |
|---|------|---------|--------------|
| 01 | [repo-bootstrap](01-repo-bootstrap.md) | go.mod, Makefile, lint | — |
| 02 | [ci-pipeline](02-ci-pipeline.md) | GitHub Actions (lint/test/build/sql-migrations) | 01 |
| 03 | [sql-schema-foundation](03-sql-schema-foundation.md) | Quy ước + migration 0001 admin/auth | 01 |
| 04 | [sql-events-pattern](04-sql-events-pattern.md) | `tbl_<object>` current-state (đọc bằng JOIN) + `tbl_<object>_events` + transition function (transaction) | 03 |
| 05 | [sql-namespace-domain](05-sql-namespace-domain.md) | Migration 0002 namespace/components | 03, 04 |
| 06 | [sql-script-loadtest-domain](06-sql-script-loadtest-domain.md) | Migration 0003 scripts/loadtests | 04, 05 |
| 07 | [sql-platform-plumbing](07-sql-platform-plumbing.md) | Migration 0004 idempotency/outbox/ledger/datasets | 03, 04 |
| 08 | [controller-bootstrap](08-controller-bootstrap.md) | cmds/loadtest-controller skeleton | 01, 03–07 |
| 09 | [authz-admin-api](09-authz-admin-api.md) | pkgs/authz + /api/admin/v1 (khớp nginx Lua) | 08 |
| 10 | [namespace-api](10-namespace-api.md) | CRUD + Idempotency-Key + ETag + transitions | 04, 05, 09 |
| 11 | [openapi-validation-readiness](11-openapi-validation-readiness.md) | Validate 422, targets[], /validate /apply | 10 |
| 12 | [scripts-api](12-scripts-api.md) | Script upload/validation async | 10 |
| 13 | [loadtest-runner](13-loadtest-runner.md) | Runner interface (K8s/local) + lifecycle | 11, 12 |
| 14 | [local-stack-e2e](14-local-stack-e2e.md) | docker-compose, Earthfile (pattern opsense-rs), e2e smoke, k6 perf gate (bảng QPS target per API) | 08–13 |

Sau 14: mock-service (phase sau), CodeGraph review, bump version, release.

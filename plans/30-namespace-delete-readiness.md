# Plan 30 — Namespace delete clears `ready_for_loadtest` (READY→DELETING)

## Mục tiêu

Sửa lỗi `DELETE namespace` trả về `409 INVALID_TRANSITION` khi namespace đang `READY` + `ready_for_loadtest=true` (CI run 42772143419, job 129682199303, `07:03:54`). Root cause:

```
READY (ready_for_loadtest=true) ──► fn_namespace_transition(..., 'DELETING')
  fn_status_transition chỉ UPDATE status, version → không đụng ready_for_loadtest
  → CHECK (NOT ready_for_loadtest OR status='READY') bị vi phạm
  → PG check_violation (23514) → deleteNamespace map mọi error → 409 INVALID_TRANSITION
```

Không phải lỗi transition map (`READY→DELETING` đã seed ở sql/0003:73), mà là **invariant không được duy trì trên đường DELETE**. Status Watcher có xử lý (clear flag trước DEGRADED) nhưng `NSToDeleting` không.

## Quyết định (đã duyệt)

1. **sql/0013_namespace_transition_clears_readiness.sql**: `CREATE OR REPLACE FUNCTION fn_namespace_transition` thành plpgsql: nếu `p_to_status <> 'READY'` thì `UPDATE tbl_namespaces SET ready_for_loadtest=false` **trước** khi gọi `fn_status_transition`. Cùng statement → atomic, cùng rollback. Hợp lệ mọi caller (handler, watcher, reaper, test). `CREATE OR REPLACE` giữ signature, chỉ đổi `LANGUAGE sql` → `plpgsql` (không cần đổi caller).
2. **pkgs/namespace/handlers.go** `deleteNamespace`: phân biệt lỗi DB — `check_violation` (23514) có prefix `INVALID_TRANSITION` → 409 (giữ); `OBJECT_NOT_FOUND` (P0001) → 409; còn lại → **500 INTERNAL + slog.Error**. Không còn mask lỗi PG thật.
3. TDD: SQL test N6/N7 (READY+ready=true → DELETING/DELETED thành công + flag=false; UPDATE thẳng status khi ready=true vẫn raise check_violation), Go test INT-NS-15 (DELETE sau `/apply` → 202, DB DELETING+ready=false), test mapping lỗi không phải transition → 500.
4. Không nới CHECK, không tăng timeout, không đụng `sources/`.

## Acceptance criteria

- [ ] `tests/sql/namespace_transition_test.sql`: N6 READY(ready=true)→DELETING thành công, `ready_for_loadtest=false`; →DELETED thành công. N7 backstop: UPDATE status='DEGRADED' khi ready=true → check_violation.
- [ ] `pkgs/namespace/lifecycle_test.go`: `TestINT_NS_15_DeleteFromReady` — create → `/validate` → `/apply` (READY+ready=true) → `DELETE` = 202; DB `status='DELETING'` + `ready_for_loadtest=false`.
- [ ] `pkgs/namespace/handlers_test.go`: lỗi DB không phải transition → 500 `INTERNAL` (không 409).
- [ ] `go build ./...` ✅, `go test ./...` ✅, `golangci-lint run ./...` 0 issues.
- [ ] `sh -n tests/e2e-full-deploy.sh` ✅; chạy `tests/sql/*_test.sql` trên PG local xanh.
- [ ] Push `feat/28-runner-status-decoupled` (PR #33), CI Full Deploy E2E xanh tới bước `delete namespace` → 202 (không còn 409), rồi squash-merge.

## Không làm trong plan này

- Không xóa/thay đổi `sql/0012_readiness_invariant.sql` (nó là backstop, giữ nguyên).
- Không sửa `pkgs/statuswatcher/watcher.go` (đã xử lý đúng: clear flag trước DEGRADED, và `readyForLoadtest` chỉ true khi `ns.Status=="READY"`).
- Không thêm flag env test-only.

## Files thay đổi

- `docs/namespace-status-watcher.md` §5 (đã làm)
- `docs/database-schema.md` migration 0003 (đã làm)
- `docs/bff-loadtest-api-spec.md` §5 (đã làm)
- `plans/30-namespace-delete-readiness.md` (này)
- `sql/0013_namespace_transition_clears_readiness.sql` (mới)
- `tests/sql/namespace_transition_test.sql` (thêm N6, N7)
- `pkgs/namespace/lifecycle_test.go` (thêm TestINT_NS_15)
- `pkgs/namespace/handlers_test.go` (thêm test mapping)
- `pkgs/namespace/handlers.go` (deleteNamespace error mapping)

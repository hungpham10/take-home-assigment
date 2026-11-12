# Plan 02 — CI Pipeline (GitHub Actions)

## Mục tiêu
Thiết lập CI trên GitHub Actions: mọi PR phải xanh trước khi review/merge (quality gate theo AGENTS.md).

## Dependencies
- Plan 01 (cần go.mod/Makefile).

## Deliverables
- `.github/workflows/ci.yml` với các job:
  1. **lint**: `golangci-lint-action`.
  2. **test**: `go test ./... -race -coverprofile`, upload coverage (artifact).
  3. **build**: build `cmds/loadtest-controller` (và sau này `mock-service`).
  4. **sql-migrations**: postgres service container → áp dụng tuần tự toàn bộ `sql/*.sql` vào DB sạch, đảm bảo migration chạy từ đầu không lỗi.
- Trigger: `pull_request` + `push` vào `main`.

## Verification
- Mở PR thử → cả 4 job xanh.
- Cố tình break 1 test → job test đỏ.

## Ghi chú
- Job `sql-migrations` có thể tạm skip nếu chưa có file nào trong `sql/` (dùng `if: hashFiles('sql/*.sql') != ''`).

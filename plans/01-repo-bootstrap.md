# Plan 01 — Repo Bootstrap

## Mục tiêu
Khởi tạo nền tảng Go cho workspace (hiện chưa có `go.mod`, Makefile rỗng) để mọi task sau có chỗ đặt code và chạy test.

## Dependencies
Không (bắt đầu trước tiên).

## Deliverables
- `go.mod` (module, ví dụ `github.com/hungpham/loadtest-platform`), Go version ổn định (1.23+).
- `Makefile`: targets `test`, `lint`, `build`, `tidy`, `run-controller`.
- `.golangci.yml`: bộ linter cơ bản (errcheck, govet, staticcheck, revive).
- `README.MD`: mô tả ngắn repo + cách chạy test/lint.
- `.editorconfig` (tùy chọn).

## Verification
- `go build ./...` xanh.
- `make test` / `make lint` chạy được (kể cả khi chưa có test nào).

## Ghi chú
- Không viết code nghiệp vụ trong plan này.

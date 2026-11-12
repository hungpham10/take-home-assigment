# Plan 38 — Quản lý version (tag-driven) cho release pipeline

## Bối cảnh

`.github/workflows/release.yml` vừa được thêm nhưng chưa có cơ chế version nào
đứng sau nó. Kiểm tra trạng thái repo:

| # | Vấn đề | Bằng chứng |
|---|---|---|
| 1 | Release fail ngay bước bump | `version.txt` không tồn tại, `git tag` = 0 ⇒ `bump.sh`: `Error: No version.txt found and no git tags available.` |
| 2 | `bump.sh` là bản copy từ project Rust | sync `Cargo.toml`/`Cargo.lock` (không tồn tại), tự `git add/commit/push`, tag bị comment out |
| 3 | `earthly … +all` **thiếu `--push`** | `earthly --help`: `--push` mặc định `false` ⇒ workflow "xanh" nhưng không có image nào trên ghcr |
| 4 | Version không có trong runtime | không `-ldflags`, không `--version`, `/health` = `{"status":"ok"}`, manifest k8s hardcode `:local` |
| 5 | Workflow thiếu an toàn | không set `user.name/email` (annotated tag cần), không `concurrency`, không guard tag trùng |

## Quyết định

- **Git tag là nguồn sự thật duy nhất** (`vMAJOR.MINOR.PATCH`, annotated). Không tạo
  `version.txt`.
- Version build = `git describe --tags --always --dirty --match 'v[0-9]*'`, bỏ prefix
  `v`; `dev` khi không có `.git`/tag.
- `scripts/bump.sh` chỉ **tính** version từ tag (patch/minor/major/`--set`/`--print`),
  không git side-effect. Git ops thuộc về workflow.
- Release workflow hỗ trợ 2 chế độ: `bump=patch|minor|major` (tính từ tag mới nhất)
  và `bump=none` + `version=X.Y.Z` (chỉ định, dùng cho bootstrap và protected branch).
- Version được nhúng vào binary qua `-ldflags` (nguồn duy nhất `pkgs/version`), surface
  ở `--version`, `GET /version`, `GET /health`, startup log.
- Image: push `:<version>` **và** `:latest`; binary linux/amd64 đính kèm GitHub Release.
- Thêm overlay `deploy/k8s/overlays/release` (ghcr + tag thật) render bằng
  `scripts/k8s-render.sh`; overlay `kind` giữ `:local`.

## Thay đổi

| File | Nội dung |
|---|---|
| `pkgs/version/version.go` (mới) | `Version/Commit/BuildDate` (ldflags), `Get()` fallback `runtime/debug.ReadBuildInfo`, `String()` |
| `pkgs/version/version_test.go` (mới) | injected thắng fallback, fallback khi rỗng, format `String()` |
| `cmds/loadtest-controller/main.go` | `--version`/`-version` trước `config.FromEnv()`; log version lúc start |
| `cmds/mock-service/main.go` | `--version`; log version lúc start |
| `pkgs/server/server.go` | `GET /version`; thêm `version` vào `/health` |
| `pkgs/server/server_test.go` | test `/version` + `/health` có version |
| `Makefile` | `VERSION`/`GIT_COMMIT`/`BUILD_DATE`/`LDFLAGS`, target `version`, `version-check`, `k8s-render`; `build` dùng LDFLAGS |
| `Earthfile` | `ARG VERSION=dev` trong `build` + `go build -ldflags`; fast-path guard so `--version`; `SAVE IMAGE --push …:<ver> …:latest` |
| `.github/workflows/ci.yml` | thay 2 step `go build` bằng `make build`; thêm job `version` |
| `.github/workflows/release.yml` | input `none`+`version`, `concurrency`, git identity, guard tag, `--push`, verify manifest, attach binary, step summary |
| `deploy/k8s/docker/*.Dockerfile` | `RUN /app/loadtest-controller --version` (hiện version trong build log) |
| `deploy/k8s/overlays/release/` (mới) | overlay ghcr + `imagePullPolicy: Always`, `newTag: dev` placeholder |
| `scripts/k8s-render.sh` (mới) | render overlay với tag thật (temp copy + sed `newTag`) ra `bin/k8s/<overlay>.yaml` |
| `scripts/bump.sh` | viết lại: tính từ tag, không Cargo, không git |
| `scripts/bump_test.sh` (mới) | sandbox git repo: 0 tag, patch/minor/major, `--set`, tag rác, prerelease, sai định dạng, downgrade |
| `docs/versioning.md` (mới) | quy tắc, runbook, verify, xử lý sự cố |
| `docs/packaging-deployment.md` | bảng image/tag + `:latest` + overlay release |
| `AGENTS.md` | Commands: `make version`, `make version-check`, `./scripts/bump.sh patch` |

## Không thay đổi

- Không sửa `conf/nginx/**`, `sql/**`, `sources/**`.
- Không đổi tên image (`loadtest-platform-{serve,mock-service}`), không đổi registry.
- Overlay `kind` giữ nguyên `:local` + `IfNotPresent`.

## Definition of done

- `go test ./pkgs/version/... ./pkgs/server/...` xanh.
- `./scripts/bump_test.sh` pass toàn bộ case (0 tag → fail có hướng dẫn; `--set`;
  patch/minor/major; tag rác bị bỏ qua; downgrade bị chặn).
- `make version` in version từ tag; `make build && ./bin/loadtest-controller --version`
  in `v<version> (<commit>, <date>)`.
- `make k8s-render VERSION=<x>` tạo manifest với `ghcr.io/...:x`; `kubectl apply --dry-run=client` OK.
- `earthly --push` được dùng trong release.yml; bước verify `imagetools inspect` fail
  nếu push không thật sự xảy ra.
- CI xanh 7/7 job + job `version`; E2E in-cluster assert `GET /version`.

## Bootstrap

Repo hiện 0 tag ⇒ release đầu tiên chạy `bump=none`, `version=0.1.0`.

## Fix log

| # | Triệu chứng | Nguyên nhân | Sửa |
|---|---|---|---|
| 1 | (điền khi implement) | | |

# Versioning & Release (plan 38)

Nguồn sự thật duy nhất của version là **git tag** dạng `vMAJOR.MINOR.PATCH`
(annotated). Không có file `version.txt`; không có version nào được gõ tay trong
manifest, Dockerfile hay workflow.

```
git tag v0.1.1
   │
   ├─► git describe --tags  ──► Go binary (ldflags)  ──► --version / GET /version / /health
   ├─► --build-arg VERSION  ──► Earthly              ──► ghcr.io/.../serve:0.1.1 + :latest
   ├─► --build-arg VERSION  ──► Dockerfile (k8s)     ──► loadtest-platform-serve:0.1.1
   └─► git describe         ──► scripts/k8s-render.sh ──► overlay release newTag=0.1.1
```

## 1. Quy tắc

| Việc | Quy tắc |
|---|---|
| Tag | `v<major>.<minor>.<patch>`, annotated (`git tag -a`), không có prerelease/build metadata trong MVP |
| Bump | `patch` = fix, `minor` = feature tương thích, `major` = breaking |
| Nguồn version build | `git describe --tags --always --dirty --match 'v[0-9]*'`, bỏ prefix `v` |
| Commit chưa tag | `0.1.1-3-gabc1234` (số commit sau tag + short SHA) — hợp lệ để trace, **không** dùng để release |
| Không có `.git` (Docker build tay) | `dev` |
| Release | Chỉ từ tag; `:latest` luôn trỏ release mới nhất |

Không bump version trong cùng PR feature. Version chỉ sinh ra ở bước release
(`Releases → Run workflow`), trừ release đầu tiên (bootstrap, xem §4).

## 2. Đọc version ở runtime

```bash
# binary
./bin/loadtest-controller --version
# loadtest-controller v0.1.1 (1ead5d4, 2026-11-12T09:00:00Z)
./bin/mock-service --version

# HTTP (sau nginx hoặc trực tiếp trong pod)
curl -s localhost:8080/version
# {"version":"0.1.1","commit":"1ead5d4","build_date":"...","go_version":"go1.26.x"}
curl -s localhost:8080/health
# {"status":"ok","version":"0.1.1"}
```

`/version` là **runtime truth**: nếu pod chạy image cũ, `/version` trả version cũ
bất kể tag trong manifest/Helm values nói gì. Dùng nó để verify deploy:

```bash
kubectl -n ltp-system exec deploy/serve -c controller -- \
  wget -qO- http://127.0.0.1:8080/version   # hoặc curl qua nginx
```

Startup log cũng in `version` + `commit` (`slog.Info("starting", …)`), nên
`kubectl logs` đủ để biết pod đang chạy bản nào.

## 3. Release runbook

Điều kiện: CI xanh trên branch sẽ release, quyền `contents: write` +
`packages: write`.

### 3.1 Release thường (bump từ tag gần nhất)

```bash
gh workflow run release.yml --ref <branch> -f bump=patch   # hoặc minor|major
```

Workflow:
1. `./scripts/bump.sh patch` → tính version kế tiếp từ tag mới nhất (không ghi file).
2. Fail nếu `v<next>` đã tồn tại, hoặc HEAD đã được tag.
3. `earthly --push --build-arg VERSION=<next> … +all` → push `:<next>` và `:latest`.
4. Verify `docker buildx imagetools inspect` cho cả 2 image.
5. `git tag -a v<next>` + push tag (không commit gì ⇒ không push vào branch).
6. Tạo GitHub Release + đính binary `loadtest-controller`, `mock-service` (linux/amd64).

**Thứ tự có chủ đích (plans/39):** tag được push **sau** khi image đã verify. Nếu
build/verify fail thì không có tag nào được tạo ⇒ không bao giờ tồn tại tag trỏ tới
một release không có artifact. Toolchain (`earthly`) được cài và kiểm tra
(`earthly --version`) ngay đầu job, fail-fast.

**Thu hồi tag của một lần release fail:** chỉ khi **chưa** có artifact nào được
publish (không có image trong ghcr, không có GitHub Release):

```bash
git push origin --delete refs/tags/v0.1.0 && git tag -d v0.1.0
```


### 3.2 Release version chỉ định (`bump=none`)

Dùng khi version đã được chốt ở nơi khác (release PR, khách hàng yêu cầu số cụ thể)
hoặc để bootstrap:

```bash
gh workflow run release.yml --ref <branch> -f bump=none -f version=0.1.0
```

`version` bắt buộc, phải đúng `X.Y.Z` và tag chưa tồn tại.

### 3.3 Bootstrap (repo chưa có tag nào)

`bump=patch` sẽ fail có chủ đích ("chưa có tag nào"). Release đầu tiên:

```bash
gh workflow run release.yml -f bump=none -f version=0.1.0
```

## 4. Deploy một bản release lên cluster

```bash
make k8s-render                       # render overlay release với tag mới nhất
kubectl apply -f bin/k8s/release.yaml # ghcr.io/<owner>/loadtest-platform/*:<version>
```

`make k8s-render VERSION=0.1.1` để chọn tag cụ thể. Overlay `kind` vẫn dùng
image `:local` + `IfNotPresent` cho dev; overlay `release` dùng ghcr +
`Always`. Nếu package để private, tạo `imagePullSecret` (ví dụ `ghcr-pull`) và
thêm `imagePullSecrets` vào `deploy/k8s/overlays/release/kustomization.yaml`.

## 5. Khi release fail giữa đường

| Trạng thái | Cách xử lý |
|---|---|
| Fail **trước** khi push tag (bump/validate) | Sửa nguyên nhân, chạy lại workflow — không có gì bị ghi |
| Tag đã push, image build/push fail | Sửa workflow → chạy lại **job build** trên đúng tag (`Releases → …` hoặc `gh run rerun`). **Không** bump tiếp: version đã công bố |
| Image đã push, GitHub Release fail | Chạy lại job tạo release; nếu không được, tạo release tay trỏ đúng tag |
| Nhầm tag (sai số) | Xoá tag remote + local **chỉ khi** chưa ai dùng: `git push --delete origin v0.1.1 && git tag -d v0.1.1`; image đã push phải xoá/ghi đè có chủ đích |

Không bao giờ "sửa" version bằng cách bump lần nữa cho cùng một thay đổi — mỗi tag
là một hợp đồng đã công bố.

## 6. Kiểm tra version trong CI

- Job `version`: `./scripts/bump_test.sh` (sandbox git repo) + `--version` của binary
  khớp `git describe` + không có tag trùng với HEAD.
- E2E in-cluster (`run version`): assert `GET /version` trả đúng version mà image
  được build.

## 7. Liên quan

- `plans/38-version-management.md` — thiết kế + fix log.
- `docs/packaging-deployment.md` §1 — bảng image/tag.
- `.github/workflows/release.yml`, `scripts/bump.sh`, `pkgs/version`.

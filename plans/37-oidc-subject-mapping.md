# Plan 37 — Ánh xạ OIDC subject (`sub`) → `tbl_users.id` ở biên tin cậy

## Bối cảnh (root cause)

Trong E2E in-cluster (plans/36) lần đầu chạy được tới controller, request bearer
fail với:

```
ERROR: invalid input syntax for type uuid: "CiQxMTExMTExMS0xMTExLTQxMTEtODExMS0xMTExMTExMTExMTESBWxvY2Fs"
```

Đó là `X-User-Id` do nginx set từ `id_token.sub`. Hai sự thật đã kiểm chứng:

1. **Dex v2.41 luôn trả `sub` dạng opaque**: `base64url(protobuf{1: userID, 2: connectorID})`
   (`server/oauth2.go:genSubject`, `internal.IDTokenSubject`) — không có config để tắt.
   Ví dụ trên decode ra `{1:"11111111-1111-4111-8111-111111111111", 2:"local"}`.
2. Schema này yêu cầu `X-User-Id` là **UUID** của `tbl_users.id`
   (`tbl_namespaces.owner_user_id UUID REFERENCES tbl_users(id)`), và đã có sẵn bảng
   `tbl_user_external_identities (user_id, issuer, subject, UNIQUE(issuer, subject))`
   đúng cho việc ánh xạ này — nhưng **chưa có code nào dùng**.

Hệ quả: platform chỉ chạy được khi client tự gửi UUID (legacy header mode / access token);
đường OIDC thật (đường duy nhất trên production) chưa bao giờ hoạt động.

Ghi chú: đặt `staticPasswords[].userID` = UUID (plans/36) là **không đủ** — `sub` vẫn bị
bọc protobuf. Nó chỉ giúp `tbl_users.id` cố định/tiên đoán được.

## Quyết định

- **Không** sửa `conf/nginx/**`: nginx tiếp tục set `X-User-Id` = `sub` (đúng
  `docs/admin-oidc-integration-api.md` §2.2: "Authenticated subject (`sub`)").
- Controller **resolve** identity tại middleware `authz.RequirePrincipalWithRole(db)`:
  - `X-User-Id` là UUID hợp lệ → dùng nguyên (backward compatible với legacy header
    mode, access token `abt_`, và toàn bộ test hiện có).
  - Không phải UUID → tra `tbl_user_external_identities` join `tbl_users` **trong tenant
    của request** (`uei.subject = ? AND u.tenant_id = ?::uuid`), lấy `u.id` làm principal.
  - Không tra được → **fail closed 401** `identity not recognized` (không tự provision;
    provisioning là việc của admin API, ngoài scope plan này).
- Bảng mapping do admin/seed tạo; E2E đọc `sub` từ token vừa mint rồi seed mapping
  (không hardcode chuỗi protobuf của Dex).

## Thay đổi

| File | Nội dung |
|---|---|
| `pkgs/authz/identity.go` (mới) | `resolveIdentity(db, tenantID, userID)`: UUID pass-through hoặc tra `tbl_user_external_identities` (tenant-scoped); trả `ErrUnknownIdentity` khi không thấy |
| `pkgs/authz/authz.go` | `RequirePrincipalWithRole` gọi `resolveIdentity` trước khi tra role; lỗi → 401 |
| `pkgs/store/models.go` | thêm model `UserExternalIdentity` (`tbl_user_external_identities`) |
| `pkgs/authz/authz_db_test.go` | test: subject opaque → resolve đúng UUID + role DB; subject lạ → 401; UUID giữ nguyên |
| `tests/e2e-full-deploy.sh` | decode `sub` từ bearer token, seed `tbl_user_external_identities(issuer, subject)` trỏ user UUID cố định |
| `docs/admin-oidc-integration-api.md` | mô tả rõ hợp đồng `X-User-Id`: UUID hoặc external subject + quy tắc resolve |
| `docs/k8s-deployment.md` | cập nhật mục "Định danh tenant/user trong chế độ bearer" (bỏ giả định `sub` = UUID) |
| `plans/36-k8s-manifests.md` | Fix log #5: đổi nguyên nhân/sửa theo plan này |

## Definition of done

- `go test ./pkgs/authz/... ./pkgs/store/...` xanh (gồm test DB mới).
- `tests/e2e-full-deploy.sh` trong chế độ in-cluster: `create-with-spec` trả 201 và
  toàn bộ lifecycle (validate → apply 202 → COMPLETED → DELETE) chạy hết.
- Local: compose + Dex + mint token → `GET/POST /api/loadtest/v1/namespaces` không còn
  500/`invalid input syntax for type uuid`.
- Không thay đổi `conf/nginx/**`, không migration mới.

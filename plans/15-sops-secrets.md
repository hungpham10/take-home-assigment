# Plan 15 — SOPS Secret Management

## Mục tiêu
Mọi dữ liệu nhạy cảm (client_secret, session_secret, jwt_secret, DB password...) **không bao giờ** nằm plaintext trong repo hay DB — mã hoá bằng SOPS + age, decrypt lúc boot theo pattern `scripts/release.sh` sẵn có.

## Dependencies
- Plan 08–09 (controller + adminapi auth-config).

## Thiết kế

### 1. File & quy ước (khớp .gitignore hiện có)
- Plaintext: `env/secrets.<ENV>.env` — **gitignored**, chỉ nằm trên máy dev / secret store.
- Encrypted: `env/secrets.<ENV>.enc.yaml` — **commit được** (SOPS encrypt từng value, giữ key结构).
- `.sops.yaml`: `creation_rules` cho `env/**` → age public key dev (chỉ public key được commit; private key `age-key.txt` đã gitignored).
- Runtime: `scripts/release.sh` decrypt (`sops -d --output-type dotenv`) + export env vars từ `SOPS_AGE_KEY_CONTENT` — **không đổi release.sh**, chỉ bổ sung mounting `env/ → /app/secrets` ở compose (Plan 14).

### 2. Tách secrets khỏi DB
- **Nguyên tắc:** `tbl_tenant_auth_profiles.config` (JSONB) chỉ giữ field **không nhạy cảm** (jwt_mode, issuer, client_id, jwks_uri, oidc_expected_alg). Secret lấy theo **naming convention từ env**:
  - `LTP_TENANT_<SLUG>_OIDC_CLIENT_SECRET`
  - `LTP_TENANT_<SLUG>_SESSION_SECRET`
  - `LTP_TENANT_<SLUG>_JWT_SECRET`
- Migration `0007`: xoá các key nhạy cảm (`oidc_client_secret`, `session_secret`, `jwt_secret`) khỏi `config` JSONB (dọn dữ liệu cũ nếu có).
- `pkgs/secrets.Resolver`: resolve secret theo slug; inject vào `adminapi.Deps` để `GET /tenant/{host}/auth-config` merge DB (non-secret) + env (secret). Không tìm thấy secret → trả config thiếu field + **WARN log không log value** (khớp fallback env vars của nginx Lua).
- `pkgs/config`: đọc `DATABASE_DSN`, `OIDC_SESSION_SECRET`... từ env đã decrypt; thêm `ValidateSecrets` warn khi thiếu.

### 3. Ops flow
- Tạo key: `age-keygen` → `age-key.txt` (local, gitignored). `SOPS_AGE_RECIPIENTS` trong `.sops.yaml`.
- Encrypt: `sops -e env/secrets.dev.env > env/secrets.dev.enc.yaml` (output-type dotenv-compatible YAML).
- Decrypt test: `SOPS_AGE_KEY_CONTENT=$(cat age-key.txt) ./scripts/release.sh` — verify vars export.
- CI: **không có age key** → integration test không phụ thuộc secret (resolver unit test dùng env var set trực tiếp).

## Deliverables
- `.sops.yaml`, `env/secrets.dev.enc.yaml` (encrypted, commit), `env/secrets.dev.env.example` (template, commit được)
- `sql/0007_strip_auth_profile_secrets.sql`
- `pkgs/secrets/` (resolver + tests), cập nhật `pkgs/adminapi` auth-config merge
- `docs/secrets-management.md` + cập nhật `docs/database-schema.md`, `plans/README.md`

## Verification
- `sops -d env/secrets.dev.enc.yaml` ra đúng vars; plaintext không xuất hiện trong `git status`/grep repo.
- Unit test resolver (env injection); integration test auth-config: secret từ env, không còn trong DB.
- CI xanh (không cần age key).

# Secrets Management (SOPS + age)

Mọi secret (client_secret, session_secret, jwt_secret, DB DSN...) **không nằm plaintext** trong repo và **không nằm trong DB**.

## Layout

| File | Commit? | Vai trò |
|---|---|---|
| `env/secrets.<ENV>.yaml` | ❌ gitignored | plaintext, chỉ trên máy dev/secret store |
| `env/secrets.<ENV>.enc.yaml` | ✅ | SOPS-encrypted (AES256-GCM, age recipient trong `.sops.yaml`) |
| `.sops.yaml` | ✅ | creation rules + age **public** key |
| `age-key.txt` | ❌ gitignored | age private key — phân phối ngoài repo |
| `env/secrets.dev.yaml.example` | ✅ | template các key convention |

## Flow

1. **Encrypt:** `sops -e env/secrets.dev.yaml > env/secrets.dev.enc.yaml`
2. **Runtime decrypt:** `scripts/release.sh` — nếu có `SOPS_AGE_KEY_CONTENT` thì `sops -d --output-type dotenv` → export env vars; không có key → fallback env vars thường (dev).
3. **Consume:** controller đọc env:
   - `DATABASE_DSN`, `OIDC_SESSION_SECRET` — runtime của controller
   - `LTP_TENANT_<SLUG>_<SECRET>` — secret per tenant (`pkgs/secrets.Resolver`, slug sanitize `acme-co → ACME_CO`):
     `OIDC_CLIENT_SECRET`, `SESSION_SECRET`, `JWT_SECRET`

## DB không chứa secret

`tbl_tenant_auth_profiles.config` (JSONB) chỉ giữ field không nhạy cảm (`jwt_mode`, `issuer`, `client_id`, `jwks_uri`, `oidc_expected_alg`). Migration `0007` strip các key nhạy cảm khỏi dữ liệu cũ. `GET /api/admin/v1/tenant/{host}/auth-config` merge: DB (non-secret) + env (secret); thiếu secret → WARN log **không kèm value** (nginx Lua có fallback env vars riêng).

## Security rules
- Private age key chỉ qua `SOPS_AGE_KEY_CONTENT` (secret store) — không file, không log.
- Response auth-config chứa secret: chỉ trusted nginx channel, không log body.
- CI **không có age key**: test không phụ thuộc secret; resolver test bằng env injection.
- Rotate secret: sửa plaintext → re-encrypt → commit `.enc` mới; nginx cache 600s tự hết hạn.

## Mã hoá at rest (field-level, pattern `sops.rs`)

Bên cạnh SOPS file-level, các giá trị nhạy cảm **phải recoverable trong DB** (vd `abt_` plaintext cho admin reveal) được mã hoá **AES-256-GCM** bằng `LTP_MASTER_KEY` (từ SOPS env): blob = `nonce(12) || ciphertext` — pattern `opsense-rs/crates/opsense-mlib/src/sops.rs`.

- `pkgs/secrets/crypto.go` — `Encrypt`/`Decrypt`/`LoadMasterKey`/`GenerateMasterKey`
- `tbl_access_tokens.token_encrypted` (migration 0008) — plaintext đã mã hoá; `token_hash` vẫn là đường xác thực (introspect)
- `POST /api/admin/v1/tokens/{id}/reveal` — self-or-admin (audit-logged); 503 khi chưa cấu hình `LTP_MASTER_KEY`
- Sinh key: `openssl rand -hex 32`

## Nguồn tham khảo
- `scripts/release.sh` (decrypt_secrets) — pattern có sẵn của repo.
- `plans/15-sops-secrets.md` — thiết kế chi tiết.

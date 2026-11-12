-- 0007_strip_auth_profile_secrets.sql — tách secrets khỏi DB (plans/15)
--
-- Nguyên tắc SOPS: secret KHÔNG nằm trong DB. tbl_tenant_auth_profiles.config
-- (JSONB) chỉ giữ field không nhạy cảm; secret lấy từ env đã SOPS-decrypt
-- theo naming convention LTP_TENANT_<SLUG>_* (pkgs/secrets).
-- Migration dọn các key nhạy cảm nếu dữ liệu cũ còn lưu.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0007_strip_auth_profile_secrets.sql')
    ON CONFLICT (filename) DO NOTHING;

UPDATE tbl_tenant_auth_profiles
SET config = config - 'oidc_client_secret' - 'session_secret' - 'jwt_secret';

COMMIT;

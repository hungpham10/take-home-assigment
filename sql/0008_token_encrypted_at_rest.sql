-- 0008_token_encrypted_at_rest.sql — abt_ plaintext mã hoá at rest (plans/15)
--
-- Mirrors opsense-rs: sops.rs (AES-256-GCM nonce||ciphertext) + sys_token_map.
-- token_hash giữ nguyên cho introspect; token_encrypted cho phép admin reveal
-- plaintext (pkgs/secrets.Encrypt/Decrypt với LTP_MASTER_KEY từ SOPS env).

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0008_token_encrypted_at_rest.sql')
    ON CONFLICT (filename) DO NOTHING;

ALTER TABLE tbl_access_tokens ADD COLUMN IF NOT EXISTS token_encrypted BYTEA;

COMMENT ON COLUMN tbl_access_tokens.token_encrypted IS
  'AES-256-GCM (LTP_MASTER_KEY) encrypted plaintext — enables admin reveal; NULL when master key not configured at issue time';

COMMIT;

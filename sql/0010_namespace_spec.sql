-- 0010_namespace_spec.sql — Namespace create-with-spec (plans/17 + plans/18)
--
-- slug/visibility cho namespace; opaque *_id (svc-/db-/ing-) unique per
-- namespace; runtime metadata JSONB cho services/databases/ingress;
-- dataset ownership/checksum/engine metadata cho restore flow.
-- Tất cả ADD COLUMN IF NOT EXISTS → idempotent khi re-run.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0010_namespace_spec.sql')
    ON CONFLICT (filename) DO NOTHING;

-- ------------------------------------------------------------ tbl_namespaces
ALTER TABLE tbl_namespaces ADD COLUMN IF NOT EXISTS slug TEXT;
-- Unique slug per platform (nullable slug vẫn hợp lệ cho namespace cũ).
CREATE UNIQUE INDEX IF NOT EXISTS uq_namespaces_slug
    ON tbl_namespaces(slug) WHERE slug IS NOT NULL;
ALTER TABLE tbl_namespaces ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'tenant'
    CHECK (visibility IN ('tenant','public'));

-- -------------------------------------------------------------- tbl_services
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS service_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_services_service_id
    ON tbl_services(namespace_id, service_id) WHERE service_id IS NOT NULL;
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS deploy JSONB NOT NULL DEFAULT '{}';
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS mock JSONB NOT NULL DEFAULT '{}';
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS database_bindings JSONB NOT NULL DEFAULT '[]';
ALTER TABLE tbl_services ADD COLUMN IF NOT EXISTS dependencies JSONB NOT NULL DEFAULT '[]';

-- ------------------------------------------------------------- tbl_databases
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS database_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_databases_database_id
    ON tbl_databases(namespace_id, database_id) WHERE database_id IS NOT NULL;
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS operator TEXT NOT NULL DEFAULT 'cloudnative-pg';
-- engine_version (không dùng cột `version` — đó là INT optimistic-concurrency
-- counter từ 0003); docs/database-management-api.md dùng "version":"16" dạng
-- string → cột riêng engine_version TEXT.
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS engine_version TEXT;
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS config JSONB DEFAULT '{}';
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS init_script_ref TEXT;
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS data_source JSONB DEFAULT '{}';
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS restore_policy JSONB DEFAULT '{}';
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS ephemeral JSONB DEFAULT '{}';
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS mock JSONB DEFAULT '{}';
ALTER TABLE tbl_databases ADD COLUMN IF NOT EXISTS secret_ref TEXT;

-- --------------------------------------------------------------- tbl_ingress
ALTER TABLE tbl_ingress ADD COLUMN IF NOT EXISTS ingress_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ingress_ingress_id
    ON tbl_ingress(namespace_id, ingress_id) WHERE ingress_id IS NOT NULL;
ALTER TABLE tbl_ingress ADD COLUMN IF NOT EXISTS class TEXT NOT NULL DEFAULT 'nginx';
ALTER TABLE tbl_ingress ADD COLUMN IF NOT EXISTS hosts JSONB NOT NULL DEFAULT '[]';

-- -------------------------------------------------------------- tbl_datasets
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS owner_user_id UUID REFERENCES tbl_users(id);
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS checksum JSONB;
-- App-level: checksum (sha256) là REQUIRED khi POST /datasets/{id}/validate.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_datasets_checksum') THEN
        ALTER TABLE tbl_datasets ADD CONSTRAINT chk_datasets_checksum
            CHECK (checksum IS NULL OR checksum::text <> '') NOT VALID;
    END IF;
END $$;
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS engine TEXT;
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS engine_version TEXT;
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS location JSONB DEFAULT '{}';
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS restore_format TEXT;
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS schema_version TEXT;
ALTER TABLE tbl_datasets ADD COLUMN IF NOT EXISTS retention_until TIMESTAMPTZ;

COMMIT;

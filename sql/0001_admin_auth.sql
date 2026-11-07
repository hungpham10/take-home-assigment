-- 0001_admin_auth.sql — Admin/Auth domain
-- Tables: tenants, tenant_hosts, tenant_auth_profiles, users,
--         user_external_identities, user_roles, access_tokens, audit_log.
-- Convention: see sql/README.md.

BEGIN;

-- migration tracking (idempotent guard for manual runs)
CREATE TABLE IF NOT EXISTS tbl_schema_migrations (
    id         SERIAL PRIMARY KEY,
    filename   TEXT NOT NULL UNIQUE,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO tbl_schema_migrations (filename) VALUES ('0001_admin_auth.sql')
    ON CONFLICT (filename) DO NOTHING;

-- shared trigger: auto-update updated_at
CREATE OR REPLACE FUNCTION fn_touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------- tenants
CREATE TABLE tbl_tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    status     TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','SUSPENDED','DELETED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_tenants_touch BEFORE UPDATE ON tbl_tenants
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ----------------------------------------------------------- tenant_hosts
CREATE TABLE tbl_tenant_hosts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tbl_tenants(id) ON DELETE CASCADE,
    host       TEXT NOT NULL UNIQUE,
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tenant_hosts_tenant ON tbl_tenant_hosts(tenant_id);
CREATE TRIGGER trg_tenant_hosts_touch BEFORE UPDATE ON tbl_tenant_hosts
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ---------------------------------------------------- tenant_auth_profiles
-- Shape matches what conf/nginx Lua parses for OIDC/JWT auth at the edge.
CREATE TABLE tbl_tenant_auth_profiles (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tbl_tenants(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    issuer       TEXT NOT NULL,
    client_id    TEXT,
    jwks_uri     TEXT,
    config       JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);
CREATE INDEX idx_tenant_auth_profiles_tenant ON tbl_tenant_auth_profiles(tenant_id);
CREATE TRIGGER trg_tenant_auth_profiles_touch BEFORE UPDATE ON tbl_tenant_auth_profiles
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ----------------------------------------------------------------- users
CREATE TABLE tbl_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tbl_tenants(id),  -- immutable
    email         TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    owner_user_id UUID REFERENCES tbl_users(id),             -- self on bootstrap
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);
CREATE INDEX idx_users_tenant ON tbl_users(tenant_id);
CREATE TRIGGER trg_users_touch BEFORE UPDATE ON tbl_users
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- -------------------------------------------------- user_external_identities
CREATE TABLE tbl_user_external_identities (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES tbl_users(id) ON DELETE CASCADE,
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);
CREATE INDEX idx_uei_user ON tbl_user_external_identities(user_id);

-- ------------------------------------------------------------ user_roles
CREATE TABLE tbl_user_roles (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES tbl_users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('admin','user')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, role)
);
CREATE INDEX idx_user_roles_user ON tbl_user_roles(user_id);

-- ---------------------------------------------------------- access_tokens
-- Platform service-account tokens (`abt_...`). Plaintext never stored:
-- token_hash = SHA-256, prefix = first 8 chars for lookup.
CREATE TABLE tbl_access_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES tbl_users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_access_tokens_user ON tbl_access_tokens(user_id);
CREATE INDEX idx_access_tokens_prefix ON tbl_access_tokens(prefix);
CREATE TRIGGER trg_access_tokens_touch BEFORE UPDATE ON tbl_access_tokens
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- -------------------------------------------------------------- audit_log
CREATE TABLE tbl_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   UUID,
    actor       JSONB NOT NULL DEFAULT '{}',
    action      TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id   TEXT,
    details     JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_log_tenant_time ON tbl_audit_log(tenant_id, created_at DESC);
CREATE INDEX idx_audit_log_object ON tbl_audit_log(object_type, object_id);

COMMIT;

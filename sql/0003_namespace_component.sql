-- 0003_namespace_component.sql — Namespace-first domain (plans/05)
--
-- tbl_namespaces = current state (nguồn đọc; status chuyển qua
-- fn_namespace_transition -> fn_status_transition).
-- Component tables FK CASCADE về namespace; chỉ namespace có state machine.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0003_namespace_component.sql')
    ON CONFLICT (filename) DO NOTHING;

-- ----------------------------------------------------------- tbl_namespaces
CREATE TABLE tbl_namespaces (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tbl_tenants(id),  -- immutable
    owner_user_id      UUID NOT NULL REFERENCES tbl_users(id),    -- immutable
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'DRAFT'
                       CHECK (status IN ('DRAFT','VALIDATING','INVALID','APPLYING',
                                         'DATA_LOADING','READY','DEGRADED',
                                         'DELETING','DELETED')),
    ready_for_loadtest BOOLEAN NOT NULL DEFAULT false,
    version            INT NOT NULL DEFAULT 1,          -- optimistic concurrency / ETag
    ttl_expires_at     TIMESTAMPTZ,                     -- NULL = không TTL; reaper (Plan 13)
    cleanup_policy     JSONB NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);
CREATE INDEX idx_namespaces_tenant_status ON tbl_namespaces(tenant_id, status);
CREATE INDEX idx_namespaces_owner ON tbl_namespaces(owner_user_id, status);
CREATE INDEX idx_namespaces_ttl ON tbl_namespaces(ttl_expires_at)
    WHERE ttl_expires_at IS NOT NULL;
CREATE TRIGGER trg_namespaces_touch BEFORE UPDATE ON tbl_namespaces
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ------------------------------------------------------ tbl_namespace_events
CREATE TABLE tbl_namespace_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL REFERENCES tbl_namespaces(id),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_namespace_events_object ON tbl_namespace_events(object_id, id);
REVOKE UPDATE, DELETE ON tbl_namespace_events FROM PUBLIC;

-- Wrapper transition cho namespace (pattern plans/04)
CREATE FUNCTION fn_namespace_transition(
    p_id UUID, p_to_status TEXT,
    p_actor JSONB DEFAULT '{}', p_reason TEXT DEFAULT NULL, p_meta JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
    SELECT * FROM fn_status_transition('tbl_namespaces', 'tbl_namespace_events',
        'namespace', p_id, p_to_status, p_actor, p_reason, p_meta);
$$ LANGUAGE sql;

-- Seed transition map namespace (không cho nhảy cóc trạng thái)
INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('namespace', 'DRAFT',        'VALIDATING'),
    ('namespace', 'VALIDATING',   'INVALID'),
    ('namespace', 'VALIDATING',   'APPLYING'),
    ('namespace', 'INVALID',      'VALIDATING'),
    ('namespace', 'APPLYING',     'DATA_LOADING'),
    ('namespace', 'APPLYING',     'INVALID'),
    ('namespace', 'DATA_LOADING', 'READY'),
    ('namespace', 'DATA_LOADING', 'DEGRADED'),
    ('namespace', 'READY',        'DEGRADED'),
    ('namespace', 'DEGRADED',     'VALIDATING'),
    ('namespace', 'READY',        'DELETING'),
    ('namespace', 'DEGRADED',     'DELETING'),
    ('namespace', 'INVALID',      'DELETING'),
    ('namespace', 'DELETING',     'DELETED');

-- ------------------------------------------------------------ tbl_services
CREATE TABLE tbl_services (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id   UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    openapi_spec   JSONB,        -- spec document; NULL -> chặn readiness (422)
    spec_hash      TEXT,         -- SHA-256 của spec, đổi spec -> revalidate
    parsed_summary JSONB NOT NULL DEFAULT '{}',  -- operations index -> targets[]
    version        INT NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name)
);
CREATE INDEX idx_services_namespace ON tbl_services(namespace_id);
CREATE TRIGGER trg_services_touch BEFORE UPDATE ON tbl_services
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ----------------------------------------------------------- tbl_databases
CREATE TABLE tbl_databases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    engine       TEXT NOT NULL DEFAULT 'postgres',
    profile      JSONB NOT NULL DEFAULT '{}',
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name)
);
CREATE INDEX idx_databases_namespace ON tbl_databases(namespace_id);
CREATE TRIGGER trg_databases_touch BEFORE UPDATE ON tbl_databases
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ------------------------------------------------------------- tbl_ingress
CREATE TABLE tbl_ingress (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    host         TEXT NOT NULL,
    paths        JSONB NOT NULL DEFAULT '[]',
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name)
);
CREATE INDEX idx_ingress_namespace ON tbl_ingress(namespace_id);
CREATE TRIGGER trg_ingress_touch BEFORE UPDATE ON tbl_ingress
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- -------------------------------------------------------- tbl_dependencies
-- Nguồn suy ra NetworkPolicy egress của runner (ADR-002)
CREATE TABLE tbl_dependencies (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    type         TEXT NOT NULL CHECK (type IN ('service','database','mock')),
    ref_id       UUID,          -- id của service/database/mock
    config       JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_dependencies_namespace ON tbl_dependencies(namespace_id);
CREATE TRIGGER trg_dependencies_touch BEFORE UPDATE ON tbl_dependencies
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ---------------------------------------------------- tbl_network_policies
CREATE TABLE tbl_network_policies (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id        UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    resolved_from_deps  BOOLEAN NOT NULL DEFAULT false,
    spec                JSONB NOT NULL DEFAULT '{}',   -- egress spec đã resolve
    observed_generation BIGINT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_network_policies_namespace ON tbl_network_policies(namespace_id);
CREATE TRIGGER trg_network_policies_touch BEFORE UPDATE ON tbl_network_policies
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

COMMIT;

-- 0005_platform_plumbing.sql — Idempotency, Outbox, Quota ledger, Datasets (plans/07)
--
-- Sau migration này, schema SQL cho MVP controller được coi là CHỐT.
-- Mọi thay đổi sau phải qua migration mới + update docs/database-schema.md.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0005_platform_plumbing.sql')
    ON CONFLICT (filename) DO NOTHING;

-- ------------------------------------------------------ tbl_idempotency_keys
-- Bắt buộc trên POST /namespaces, /apply, /loadtests, /loadtests/{id}/stop.
-- Replay trả cached response; UNIQUE (tenant_id, endpoint, key).
CREATE TABLE tbl_idempotency_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    endpoint       TEXT NOT NULL,
    key            TEXT NOT NULL,
    request_hash   TEXT NOT NULL,
    response_status INT,
    response_body  JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, endpoint, key)
);
-- Reaper dọn key cũ (>24h)
CREATE INDEX idx_idempotency_created ON tbl_idempotency_keys(created_at);

-- ----------------------------------------------------------- tbl_outbox_events
-- Transactional outbox: ghi CÙNG transaction với state change; worker dispatch.
CREATE TABLE tbl_outbox_events (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT NOT NULL,
    aggregate_id   UUID NOT NULL,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    attempts       INT NOT NULL DEFAULT 0,
    next_retry_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Partial index: chỉ scan các event chưa dispatch
CREATE INDEX idx_outbox_pending ON tbl_outbox_events(id)
    WHERE dispatched_at IS NULL;
CREATE INDEX idx_outbox_retry ON tbl_outbox_events(next_retry_at)
    WHERE dispatched_at IS NULL;

-- ------------------------------------------------------- tbl_reservation_ledger
-- Quota/capacity: RESERVED -> COMMITTED (run bắt đầu) | RELEASED (hủy/kết thúc).
-- Quá hạn -> 429.
CREATE TABLE tbl_reservation_ledger (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    load_test_id  UUID REFERENCES tbl_load_tests(id),
    resource_type TEXT NOT NULL CHECK (resource_type IN ('vusers','concurrent_loadtests','mock_pool')),
    amount        INT NOT NULL CHECK (amount > 0),
    state         TEXT NOT NULL DEFAULT 'RESERVED' CHECK (state IN ('RESERVED','COMMITTED','RELEASED')),
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_reservation_tenant_state ON tbl_reservation_ledger(tenant_id, resource_type, state);
CREATE TRIGGER trg_reservation_touch BEFORE UPDATE ON tbl_reservation_ledger
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ---------------------------------------------------------------- tbl_datasets
-- Non-prod only; pii_classification import-time check (app-level).
CREATE TABLE tbl_datasets (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    namespace_id       UUID REFERENCES tbl_namespaces(id),
    name               TEXT NOT NULL,
    source_ref         TEXT NOT NULL,      -- S3 pg_dump custom-format file
    size_bytes         BIGINT NOT NULL DEFAULT 0,
    pii_classification TEXT NOT NULL
                       CHECK (pii_classification IN ('masked','synthetic','non_prod_raw')),
    status             TEXT NOT NULL DEFAULT 'IMPORTING'
                       CHECK (status IN ('IMPORTING','AVAILABLE','INVALID','EXPIRED',
                                         'SUSPENDED','DELETING','DELETED')),
    restore_config     JSONB NOT NULL DEFAULT '{}', -- vd {timeout:"30m", parallel_jobs:4}
    version            INT NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);
CREATE INDEX idx_datasets_tenant_status ON tbl_datasets(tenant_id, status);
CREATE TRIGGER trg_datasets_touch BEFORE UPDATE ON tbl_datasets
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ---------------------------------------------------------- tbl_dataset_events
CREATE TABLE tbl_dataset_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL REFERENCES tbl_datasets(id),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_dataset_events_object ON tbl_dataset_events(object_id, id);
REVOKE UPDATE, DELETE ON tbl_dataset_events FROM PUBLIC;

CREATE FUNCTION fn_dataset_transition(
    p_id UUID, p_to_status TEXT,
    p_actor JSONB DEFAULT '{}', p_reason TEXT DEFAULT NULL, p_meta JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
    SELECT * FROM fn_status_transition('tbl_datasets', 'tbl_dataset_events',
        'dataset', p_id, p_to_status, p_actor, p_reason, p_meta);
$$ LANGUAGE sql;

INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('dataset', 'IMPORTING', 'AVAILABLE'),
    ('dataset', 'IMPORTING', 'INVALID'),
    ('dataset', 'AVAILABLE', 'SUSPENDED'),
    ('dataset', 'AVAILABLE', 'EXPIRED'),
    ('dataset', 'AVAILABLE', 'DELETING'),
    ('dataset', 'SUSPENDED', 'AVAILABLE'),
    ('dataset', 'SUSPENDED', 'DELETING'),
    ('dataset', 'EXPIRED',   'DELETING'),
    ('dataset', 'DELETING',  'DELETED');

COMMIT;

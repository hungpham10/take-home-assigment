-- 0011_component_status.sql — Status Watcher projection (plans/20 + plans/21 PR A)
--
-- tbl_component_status = projection per-component do Status Watcher ghi
-- (observed K8s -> PG). GET /readiness đọc từ đây thay vì tính on-the-fly.
-- tbl_namespaces thêm desired_generation (Env Reconciler bump), blocking_reasons
-- (nguồn cho GET /readiness) và status_observed_at (watcher heartbeat -> gate
-- STATUS_STALE fail-closed).
-- Seed transition delta: các nhánh APPLYING→READY/DEGRADED, DATA_LOADING→INVALID,
-- DEGRADED→READY còn thiếu ở 0003 (docs/namespace-status-watcher.md §2).
-- Tất cả IF NOT EXISTS / ON CONFLICT → idempotent khi re-run.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0011_component_status.sql')
    ON CONFLICT (filename) DO NOTHING;

-- ------------------------------------------------------ tbl_component_status
CREATE TABLE IF NOT EXISTS tbl_component_status (
    id                   BIGSERIAL PRIMARY KEY,
    namespace_id         UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    component_type       TEXT NOT NULL CHECK (component_type IN
        ('service','database','data_load','ingress','network_policy','mock')),
    component_id         TEXT NOT NULL,          -- svc-.../db-.../ing-... ('-' cho namespace-level)
    required             BOOLEAN NOT NULL DEFAULT true,
    desired_generation   BIGINT NOT NULL DEFAULT 0,
    observed_generation  BIGINT NOT NULL DEFAULT 0,
    phase                TEXT NOT NULL CHECK (phase IN
        ('PENDING','PROGRESSING','READY','FAILED','UNKNOWN')),
    reason               TEXT,                   -- vd image_pull_backoff, STATUS_STALE
    message              TEXT,
    last_transition_at   TIMESTAMPTZ,
    observed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, component_type, component_id)
);
CREATE INDEX IF NOT EXISTS idx_component_status_ns
    ON tbl_component_status(namespace_id, component_type);
CREATE INDEX IF NOT EXISTS idx_component_status_stale
    ON tbl_component_status(observed_at)
    WHERE phase IN ('PENDING','PROGRESSING','UNKNOWN');

-- ------------------------------------------------------------- tbl_namespaces
ALTER TABLE tbl_namespaces ADD COLUMN IF NOT EXISTS desired_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tbl_namespaces ADD COLUMN IF NOT EXISTS blocking_reasons   JSONB;
ALTER TABLE tbl_namespaces ADD COLUMN IF NOT EXISTS status_observed_at TIMESTAMPTZ;

-- --------------------------------------------- seed transition delta (0003)
-- Bổ sung các nhánh còn thiếu cho vòng watcher; ON CONFLICT giữ idempotent.
INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('namespace', 'APPLYING',     'READY'),      -- namespace không có data_source
    ('namespace', 'APPLYING',     'DEGRADED'),   -- component FAILED vĩnh viễn trong apply
    ('namespace', 'DATA_LOADING', 'INVALID'),    -- restore failure cứng
    ('namespace', 'DEGRADED',     'READY')       -- recovery ổn định > stable window
ON CONFLICT (object_type, from_status, to_status) DO NOTHING;

COMMIT;

-- 0004_script_loadtest.sql — Script validation & Loadtest lifecycle (plans/06)
--
-- tbl_scripts / tbl_load_tests = current state; status chuyển qua
-- fn_script_transition / fn_loadtest_transition (wrapper fn_status_transition).

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0004_script_loadtest.sql')
    ON CONFLICT (filename) DO NOTHING;

-- -------------------------------------------------------------- tbl_scripts
-- Giới hạn 1MiB (ConfigMap limit): size_bytes CHECK <= 1048576.
CREATE TABLE tbl_scripts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id  UUID NOT NULL REFERENCES tbl_namespaces(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    content_ref   TEXT NOT NULL,        -- ConfigMap / staging ref, không lưu content
    content_hash  TEXT NOT NULL,        -- SHA-256
    size_bytes    INT  NOT NULL CHECK (size_bytes <= 1048576),
    status        TEXT NOT NULL DEFAULT 'PENDING_VALIDATION'
                  CHECK (status IN ('PENDING_VALIDATION','VALIDATING','VALIDATED',
                                    'INVALID','VALIDATION_FAILED')),
    version       INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name)
);
CREATE INDEX idx_scripts_namespace_status ON tbl_scripts(namespace_id, status);
CREATE TRIGGER trg_scripts_touch BEFORE UPDATE ON tbl_scripts
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- -------------------------------------------------------- tbl_script_events
CREATE TABLE tbl_script_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL REFERENCES tbl_scripts(id),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_script_events_object ON tbl_script_events(object_id, id);
REVOKE UPDATE, DELETE ON tbl_script_events FROM PUBLIC;

CREATE FUNCTION fn_script_transition(
    p_id UUID, p_to_status TEXT,
    p_actor JSONB DEFAULT '{}', p_reason TEXT DEFAULT NULL, p_meta JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
    SELECT * FROM fn_status_transition('tbl_scripts', 'tbl_script_events',
        'script', p_id, p_to_status, p_actor, p_reason, p_meta);
$$ LANGUAGE sql;

INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('script', 'PENDING_VALIDATION', 'VALIDATING'),
    ('script', 'VALIDATING',         'VALIDATED'),
    ('script', 'VALIDATING',         'INVALID'),
    ('script', 'VALIDATING',         'VALIDATION_FAILED'),
    ('script', 'INVALID',            'VALIDATING'),       -- revalidate sau khi sửa
    ('script', 'VALIDATION_FAILED',  'VALIDATING');

-- ------------------------------------------------------ tbl_script_validations
CREATE TABLE tbl_script_validations (
    id           BIGSERIAL PRIMARY KEY,
    script_id    UUID NOT NULL REFERENCES tbl_scripts(id) ON DELETE CASCADE,
    attempt      INT  NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('VALIDATED','INVALID','VALIDATION_FAILED')),
    errors       JSONB NOT NULL DEFAULT '[]',   -- chi tiết lỗi -> trả 422
    validated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (script_id, attempt)
);

-- -------------------------------------------------------------- tbl_load_tests
CREATE TABLE tbl_load_tests (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace_id  UUID NOT NULL REFERENCES tbl_namespaces(id),
    script_id     UUID NOT NULL REFERENCES tbl_scripts(id),
    name          TEXT NOT NULL,
    created_by    UUID NOT NULL REFERENCES tbl_users(id),
    load_config   JSONB NOT NULL DEFAULT '{}',  -- vd {vusers:200, duration:"10m", timeout_minutes:15}
    targets       JSONB NOT NULL,               -- bắt buộc, resolve từ parsed OpenAPI
    runner_type   TEXT NOT NULL DEFAULT 'local' CHECK (runner_type IN ('k8s','local')),
    runner_ref    TEXT,                         -- TestRun name / local job id
    status        TEXT NOT NULL DEFAULT 'QUEUED'
                  CHECK (status IN ('QUEUED','PROVISIONING','RUNNING','COMPLETED',
                                    'FAILED','STOPPING','STOPPED','TIMED_OUT',
                                    'TIMEOUT_CLEANED','CLEANUP_FAILED')),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    timeout_at    TIMESTAMPTZ,                  -- reaper: TIMED_OUT khi quá mốc
    version       INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_loadtests_namespace_status ON tbl_load_tests(namespace_id, status);
CREATE INDEX idx_loadtests_timeout ON tbl_load_tests(timeout_at)
    WHERE status IN ('QUEUED','PROVISIONING','RUNNING','STOPPING');
CREATE INDEX idx_loadtests_runner ON tbl_load_tests(runner_type, runner_ref);
CREATE TRIGGER trg_loadtests_touch BEFORE UPDATE ON tbl_load_tests
    FOR EACH ROW EXECUTE FUNCTION fn_touch_updated_at();

-- ------------------------------------------------------ tbl_loadtest_events
CREATE TABLE tbl_loadtest_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL REFERENCES tbl_load_tests(id),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_loadtest_events_object ON tbl_loadtest_events(object_id, id);
REVOKE UPDATE, DELETE ON tbl_loadtest_events FROM PUBLIC;

CREATE FUNCTION fn_loadtest_transition(
    p_id UUID, p_to_status TEXT,
    p_actor JSONB DEFAULT '{}', p_reason TEXT DEFAULT NULL, p_meta JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
    SELECT * FROM fn_status_transition('tbl_load_tests', 'tbl_loadtest_events',
        'loadtest', p_id, p_to_status, p_actor, p_reason, p_meta);
$$ LANGUAGE sql;

-- Lifecycle loadtest (CANCELED reserved, không dùng)
INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('loadtest', 'QUEUED',          'PROVISIONING'),
    ('loadtest', 'QUEUED',          'STOPPING'),
    ('loadtest', 'PROVISIONING',    'RUNNING'),
    ('loadtest', 'PROVISIONING',    'FAILED'),
    ('loadtest', 'PROVISIONING',    'STOPPING'),
    ('loadtest', 'RUNNING',         'COMPLETED'),
    ('loadtest', 'RUNNING',         'FAILED'),
    ('loadtest', 'RUNNING',         'STOPPING'),
    ('loadtest', 'RUNNING',         'TIMED_OUT'),
    ('loadtest', 'STOPPING',        'STOPPED'),
    ('loadtest', 'STOPPING',        'CLEANUP_FAILED'),
    ('loadtest', 'TIMED_OUT',       'TIMEOUT_CLEANED'),
    ('loadtest', 'TIMED_OUT',       'CLEANUP_FAILED'),
    ('loadtest', 'CLEANUP_FAILED',  'TIMEOUT_CLEANED'),
    ('loadtest', 'CLEANUP_FAILED',  'STOPPED');

-- ------------------------------------------------------------- tbl_test_events
-- Stream event runtime (log/metric/status) — append-only, cold data.
CREATE TABLE tbl_test_events (
    id           BIGSERIAL PRIMARY KEY,
    load_test_id UUID NOT NULL REFERENCES tbl_load_tests(id) ON DELETE CASCADE,
    seq          BIGINT NOT NULL,
    type         TEXT NOT NULL CHECK (type IN ('log','metric','status')),
    payload      JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (load_test_id, seq)
);
REVOKE UPDATE, DELETE ON tbl_test_events FROM PUBLIC;

COMMIT;

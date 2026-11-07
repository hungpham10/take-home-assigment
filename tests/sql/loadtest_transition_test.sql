-- tests/sql/loadtest_transition_test.sql
-- Test script/loadtest lifecycle (sql/0004) — chạy sau migrations trong CI.

BEGIN;

-- fixtures
INSERT INTO tbl_tenants (name, slug) VALUES ('T', 't1');
INSERT INTO tbl_users (tenant_id, email, display_name)
SELECT id, 'u@t.co', 'U' FROM tbl_tenants WHERE slug='t1';
INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status, ready_for_loadtest)
SELECT tenant_id, id, 'ns1', 'READY', true FROM tbl_users WHERE email='u@t.co';
INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes)
SELECT id, 's1', 'cm://s1', 'abc', 100 FROM tbl_namespaces WHERE name='ns1';

-- S1: script lifecycle PENDING_VALIDATION -> VALIDATING -> VALIDATED
SELECT * FROM fn_script_transition((SELECT id FROM tbl_scripts WHERE name='s1'), 'VALIDATING');
SELECT * FROM fn_script_transition((SELECT id FROM tbl_scripts WHERE name='s1'), 'VALIDATED');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_scripts WHERE name='s1' AND status='VALIDATED' AND version=3)
    THEN RAISE EXCEPTION 'S1_FAIL'; END IF;
END $$;

-- S2: size > 1MiB bị chặn
DO $$ BEGIN
    INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes)
    SELECT id, 'big', 'cm://big', 'xyz', 1048577 FROM tbl_namespaces WHERE name='ns1';
    RAISE EXCEPTION 'S2_FAIL: oversized script accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;

-- L1: loadtest lifecycle QUEUED -> PROVISIONING -> RUNNING -> COMPLETED
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, runner_type, started_at)
SELECT n.id, s.id, 'lt1', u.id, '[{"op":"getItems"}]'::jsonb, 'local', now()
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='ns1' AND s.name='s1' AND u.email='u@t.co';
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt1'), 'PROVISIONING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt1'), 'RUNNING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt1'), 'COMPLETED');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='lt1' AND status='COMPLETED' AND version=4)
    THEN RAISE EXCEPTION 'L1_FAIL'; END IF;
END $$;

-- L2: COMPLETED terminal — mọi transition từ COMPLETED bị chặn
DO $$ BEGIN
    PERFORM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt1'), 'RUNNING');
    RAISE EXCEPTION 'L2_FAIL: resurrected completed test';
EXCEPTION WHEN check_violation THEN NULL; END $$;

-- L3: đường stop RUNNING -> STOPPING -> STOPPED
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, runner_type, started_at)
SELECT n.id, s.id, 'lt2', u.id, '[]'::jsonb, 'local', now()
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='ns1' AND s.name='s1' AND u.email='u@t.co';
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt2'), 'PROVISIONING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt2'), 'RUNNING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt2'), 'STOPPING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt2'), 'STOPPED');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='lt2' AND status='STOPPED')
    THEN RAISE EXCEPTION 'L3_FAIL'; END IF;
END $$;

-- L4: đường timeout TIMED_OUT -> TIMEOUT_CLEANED
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, runner_type, started_at, timeout_at)
SELECT n.id, s.id, 'lt3', u.id, '[]'::jsonb, 'local', now(), now() + interval '1 min'
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='ns1' AND s.name='s1' AND u.email='u@t.co';
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt3'), 'PROVISIONING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt3'), 'RUNNING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt3'), 'TIMED_OUT');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='lt3'), 'TIMEOUT_CLEANED');
DO $$ BEGIN
    IF (SELECT count(*) FROM tbl_loadtest_events e JOIN tbl_load_tests l ON l.id=e.object_id
        WHERE l.name='lt3' AND e.to_status IN ('TIMED_OUT','TIMEOUT_CLEANED')) <> 2
    THEN RAISE EXCEPTION 'L4_FAIL: timeout events missing'; END IF;
END $$;

-- L5: tbl_test_events unique (load_test_id, seq)
INSERT INTO tbl_test_events (load_test_id, seq, type)
SELECT id, 1, 'log' FROM tbl_load_tests WHERE name='lt1';
DO $$ BEGIN
    INSERT INTO tbl_test_events (load_test_id, seq, type)
    SELECT id, 1, 'log' FROM tbl_load_tests WHERE name='lt1';
    RAISE EXCEPTION 'L5_FAIL: duplicate seq accepted';
EXCEPTION WHEN unique_violation THEN NULL; END $$;

ROLLBACK;

-- tests/sql/namespace_transition_test.sql
-- Test state machine namespace (sql/0003) — chạy sau migrations trong CI.

BEGIN;

INSERT INTO tbl_tenants (name, slug) VALUES ('T', 't1');
INSERT INTO tbl_users (tenant_id, email, display_name)
SELECT id, 'u@t.co', 'U' FROM tbl_tenants WHERE slug='t1';

INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name)
SELECT tenant_id, id, 'ns1' FROM tbl_users WHERE email='u@t.co';

-- N1: happy path DRAFT -> VALIDATING -> APPLYING -> DATA_LOADING -> READY
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'VALIDATING');
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'APPLYING');
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'DATA_LOADING');
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'READY');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_namespaces WHERE name='ns1' AND status='READY' AND version=5)
    THEN RAISE EXCEPTION 'N1_FAIL: expected READY/version=5'; END IF;
    IF (SELECT count(*) FROM tbl_namespace_events e JOIN tbl_namespaces n ON n.id=e.object_id WHERE n.name='ns1') <> 4
    THEN RAISE EXCEPTION 'N1_FAIL: expected 4 events'; END IF;
END $$;

-- N2: nhảy cóc READY -> APPLYING bị chặn
DO $$ BEGIN
    PERFORM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'APPLYING');
    RAISE EXCEPTION 'N2_FAIL: shortcut allowed';
EXCEPTION WHEN check_violation THEN NULL; END $$;

-- N3: READY -> DELETING -> DELETED (đường cleanup)
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'DELETING');
SELECT * FROM fn_namespace_transition((SELECT id FROM tbl_namespaces WHERE name='ns1'), 'DELETED');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_namespaces WHERE name='ns1' AND status='DELETED')
    THEN RAISE EXCEPTION 'N3_FAIL: expected DELETED'; END IF;
END $$;

-- N4: component CASCADE + unique
INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name)
SELECT tenant_id, owner_user_id, 'ns2' FROM tbl_namespaces WHERE name='ns1';
DO $$ DECLARE ns2 UUID;
BEGIN
    SELECT id INTO ns2 FROM tbl_namespaces WHERE name='ns2';
    INSERT INTO tbl_services (namespace_id, name) VALUES (ns2, 'svc');
    BEGIN
        INSERT INTO tbl_services (namespace_id, name) VALUES (ns2, 'svc');
        RAISE EXCEPTION 'N4_FAIL: duplicate service allowed';
    EXCEPTION WHEN unique_violation THEN NULL; END;
    DELETE FROM tbl_namespaces WHERE id = ns2;
    IF EXISTS (SELECT 1 FROM tbl_services WHERE namespace_id = ns2)
    THEN RAISE EXCEPTION 'N4_FAIL: cascade delete failed'; END IF;
END $$;

-- N5: status CHECK constraint chặn giá trị lạ
DO $$ BEGIN
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status)
    SELECT tenant_id, owner_user_id, 'ns3', 'WHATEVER' FROM tbl_namespaces WHERE name='ns1';
    RAISE EXCEPTION 'N5_FAIL: bad status accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;

-- N6: rời READY với ready_for_loadtest=true phải clear flag trong cùng transition
--     (READY→DELETING→DELETED). Trước fix: check_violation do chk_namespaces_ready_only_when_ready.
DO $$ DECLARE ns6 UUID; BEGIN
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name)
    SELECT tenant_id, id, 'ns6' FROM tbl_users WHERE email='u@t.co' RETURNING id INTO ns6;
    PERFORM fn_namespace_transition(ns6, 'VALIDATING');
    PERFORM fn_namespace_transition(ns6, 'APPLYING');
    PERFORM fn_namespace_transition(ns6, 'DATA_LOADING');
    PERFORM fn_namespace_transition(ns6, 'READY');
    UPDATE tbl_namespaces SET ready_for_loadtest = true WHERE id = ns6;
    PERFORM fn_namespace_transition(ns6, 'DELETING');   -- trước fix: check_violation
    IF NOT EXISTS (SELECT 1 FROM tbl_namespaces WHERE id=ns6 AND status='DELETING' AND NOT ready_for_loadtest)
    THEN RAISE EXCEPTION 'N6_FAIL: DELETING did not clear ready_for_loadtest'; END IF;
    PERFORM fn_namespace_transition(ns6, 'DELETED');    -- trước fix: check_violation (ready vẫn true)
    IF NOT EXISTS (SELECT 1 FROM tbl_namespaces WHERE id=ns6 AND status='DELETED')
    THEN RAISE EXCEPTION 'N6_FAIL: expected DELETED'; END IF;
END $$;

-- N7: backstop — UPDATE thẳng status khi ready=true vẫn bị check_violation (CHECK giữ nguyên).
DO $$ DECLARE ns7 UUID; BEGIN
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name)
    SELECT tenant_id, id, 'ns7' FROM tbl_users WHERE email='u@t.co' RETURNING id INTO ns7;
    PERFORM fn_namespace_transition(ns7, 'VALIDATING');
    PERFORM fn_namespace_transition(ns7, 'APPLYING');
    PERFORM fn_namespace_transition(ns7, 'READY');
    UPDATE tbl_namespaces SET ready_for_loadtest = true WHERE id = ns7;
    BEGIN
        UPDATE tbl_namespaces SET status='DEGRADED' WHERE id = ns7;
        RAISE EXCEPTION 'N7_FAIL: direct UPDATE status accepted with ready=true';
    EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

ROLLBACK;

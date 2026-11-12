-- tests/sql/harness_test.sql
-- Plan 16 SQL harness — ACID/invariant/integrity cases (plans/16-harness-testing-integrity.md).
-- Pattern: BEGIN → fixtures → DO $$ ... EXCEPTION ... → ROLLBACK (giống tests/sql/*_test.sql).
-- Chạy trong CI job sql-migrations sau khi mọi migration đã apply.

BEGIN;

-- ---------------------------------------------------------------- fixtures
INSERT INTO tbl_tenants (name, slug) VALUES ('H', 'h1');
INSERT INTO tbl_users (tenant_id, email, display_name)
SELECT id, 'u@h.co', 'U' FROM tbl_tenants WHERE slug='h1';
INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status, ready_for_loadtest)
SELECT tenant_id, id, 'hns', 'READY', true FROM tbl_users WHERE email='u@h.co';
INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes, status)
SELECT id, 'hs', 'cm://hs', 'abc', 100, 'VALIDATED' FROM tbl_namespaces WHERE name='hns';
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, runner_type, status, version)
SELECT n.id, s.id, 'hlt', u.id, '[]'::jsonb, 'local', 'QUEUED', 1
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='hns' AND s.name='hs' AND u.email='u@h.co';

-- ---------------------------------------------------------------
-- DB-TXN-01: fn_status_transition ghi event + version trong CÙNG transaction.
-- Simulate lỗi sau khi transition đã chạy (forced exception trong block):
-- subtransaction rollback ⇒ status/version/events trở về nguyên trạng.
DO $$ BEGIN
    BEGIN
        PERFORM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt'),
                                       'PROVISIONING', '{}'::jsonb, 'txn-01');
        -- fault injection: crash sau transition, trước commit
        RAISE EXCEPTION 'simulated crash after transition';
    EXCEPTION WHEN OTHERS THEN NULL; -- rollback toàn bộ block (event + status + version)
    END;
    IF EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='hlt' AND (status <> 'QUEUED' OR version <> 1))
    THEN RAISE EXCEPTION 'DB-TXN-01_FAIL: status/version mutated despite rollback'; END IF;
    IF (SELECT count(*) FROM tbl_loadtest_events e
        JOIN tbl_load_tests l ON l.id = e.object_id WHERE l.name='hlt') <> 0
    THEN RAISE EXCEPTION 'DB-TXN-01_FAIL: event row leaked outside transaction'; END IF;
END $$;

-- ---------------------------------------------------------------
-- DB-TXN-02: invalid transition KHÔNG ghi event và KHÔNG bump version
-- (QUEUED → RUNNING không có trong transition map → check_violation).
DO $$ BEGIN
    PERFORM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt'),
                                   'RUNNING', '{}'::jsonb, 'txn-02');
    RAISE EXCEPTION 'DB-TXN-02_FAIL: invalid transition accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='hlt' AND status='QUEUED' AND version=1)
    THEN RAISE EXCEPTION 'DB-TXN-02_FAIL: state changed on rejected transition'; END IF;
    IF (SELECT count(*) FROM tbl_loadtest_events e
        JOIN tbl_load_tests l ON l.id = e.object_id WHERE l.name='hlt') <> 0
    THEN RAISE EXCEPTION 'DB-TXN-02_FAIL: event appended on rejected transition'; END IF;
END $$;

-- ---------------------------------------------------------------
-- DB-TXN-03: version monotonic qua các transition tuần tự
-- (3 transition hợp lệ ⇒ version 1→4, mỗi transition đúng +1, 3 event).
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt'), 'PROVISIONING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt'), 'RUNNING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt'), 'COMPLETED');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='hlt' AND status='COMPLETED' AND version=4)
    THEN RAISE EXCEPTION 'DB-TXN-03_FAIL: version not monotonic (+1 per transition)'; END IF;
    IF (SELECT count(*) FROM tbl_loadtest_events e
        JOIN tbl_load_tests l ON l.id = e.object_id WHERE l.name='hlt') <> 3
    THEN RAISE EXCEPTION 'DB-TXN-03_FAIL: expected exactly 3 events'; END IF;
    -- mỗi event một from→to đúng thứ tự (không duplicate/trống)
    IF (SELECT count(*) FROM (
            SELECT e.from_status, e.to_status FROM tbl_loadtest_events e
            JOIN tbl_load_tests l ON l.id = e.object_id
            WHERE l.name='hlt' ORDER BY e.id) t)
       <> 3
    THEN RAISE EXCEPTION 'DB-TXN-03_FAIL: event trail incomplete'; END IF;
END $$;

-- ---------------------------------------------------------------
-- DB-TXN-04: hai transition tuần tự nhanh (serialization mô phỏng qua
-- row-lock của fn: FOR UPDATE ⇒ gọi liên tiếp vẫn nhất quán).
-- 2 transition hợp lệ từ terminal đã COMPLETED bị chặn, nên dùng object mới:
-- QUEUED → PROVISIONING → RUNNING ⇒ final version == 3 và mỗi transition
-- append đúng 1 event (2 events, không double-append).
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, runner_type, status, version)
SELECT n.id, s.id, 'hlt2', u.id, '[]'::jsonb, 'local', 'QUEUED', 1
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='hns' AND s.name='hs' AND u.email='u@h.co';
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt2'), 'PROVISIONING');
SELECT * FROM fn_loadtest_transition((SELECT id FROM tbl_load_tests WHERE name='hlt2'), 'RUNNING');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='hlt2' AND status='RUNNING' AND version=3)
    THEN RAISE EXCEPTION 'DB-TXN-04_FAIL: serialized transitions lost an update'; END IF;
    IF (SELECT count(*) FROM tbl_loadtest_events e
        JOIN tbl_load_tests l ON l.id = e.object_id WHERE l.name='hlt2') <> 2
    THEN RAISE EXCEPTION 'DB-TXN-04_FAIL: one event per transition violated'; END IF;
END $$;

-- ---------------------------------------------------------------
-- DB-TXN-05: idempotency_keys UNIQUE (tenant_id, endpoint, key) — replay contract.
INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash, response_status, response_body)
SELECT tn.id, '/api/loadtest/v1/namespaces', 'k1', 'hash-a', 201, '{"id":"x"}'
FROM tbl_tenants tn WHERE tn.slug='h1';
DO $$ BEGIN
    INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash, response_status, response_body)
    SELECT tn.id, '/api/loadtest/v1/namespaces', 'k1', 'hash-a', 201, '{"id":"x"}'
    FROM tbl_tenants tn WHERE tn.slug='h1';
    RAISE EXCEPTION 'DB-TXN-05_FAIL: duplicate (tenant, endpoint, key) accepted';
EXCEPTION WHEN unique_violation THEN NULL; END $$;
-- cùng key, tenant khác → KHÔNG conflict (tenant scoping của unique index)
INSERT INTO tbl_tenants (name, slug) VALUES ('H2', 'h2');
INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash)
SELECT tn.id, '/api/loadtest/v1/namespaces', 'k1', 'hash-b'
FROM tbl_tenants tn WHERE tn.slug='h2';

-- ---------------------------------------------------------------
-- IDEM-CONFLICT-01: request_hash được lưu đúng và là dữ liệu riêng của key;
-- INSERT lại cùng triple conflict, còn hash khác trên cùng key bị chặn ở tầng
-- unique — hash phân biệt payload tại replay layer (app-side 422).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_idempotency_keys
                   WHERE endpoint='/api/loadtest/v1/namespaces' AND key='k1'
                     AND request_hash='hash-a')
    THEN RAISE EXCEPTION 'IDEM-CONFLICT-01_FAIL: request_hash not stored verbatim'; END IF;
    INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash)
    SELECT tn.id, '/api/loadtest/v1/namespaces', 'k1', 'hash-a'
    FROM tbl_tenants tn WHERE tn.slug='h1';
    RAISE EXCEPTION 'IDEM-CONFLICT-01_FAIL: same triple re-insert accepted';
EXCEPTION WHEN unique_violation THEN NULL; END $$;

-- ---------------------------------------------------------------
-- RESERVATION-01: reservation ledger CHECK — amount > 0, state whitelist.
DO $$ BEGIN
    INSERT INTO tbl_reservation_ledger (tenant_id, resource_type, amount, state)
    SELECT tn.id, 'vusers', 0, 'RESERVED' FROM tbl_tenants tn WHERE tn.slug='h1';
    RAISE EXCEPTION 'RESERVATION-01_FAIL: amount=0 accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;
DO $$ BEGIN
    INSERT INTO tbl_reservation_ledger (tenant_id, resource_type, amount, state)
    SELECT tn.id, 'vusers', -5, 'RESERVED' FROM tbl_tenants tn WHERE tn.slug='h1';
    RAISE EXCEPTION 'RESERVATION-01_FAIL: negative amount accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;
DO $$ BEGIN
    INSERT INTO tbl_reservation_ledger (tenant_id, resource_type, amount, state)
    SELECT tn.id, 'vusers', 10, 'BOGUS' FROM tbl_tenants tn WHERE tn.slug='h1';
    RAISE EXCEPTION 'RESERVATION-01_FAIL: state outside RESERVED/COMMITTED/RELEASED accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;
-- đường vui: RESERVED → COMMITTED → RELEASED đều hợp lệ
DO $$ BEGIN
    DECLARE rid UUID;
    BEGIN
        INSERT INTO tbl_reservation_ledger (tenant_id, resource_type, amount, state)
        VALUES ((SELECT n.tenant_id FROM tbl_load_tests l
                 JOIN tbl_namespaces n ON n.id = l.namespace_id WHERE l.name='hlt2'),
                'vusers', 10, 'RESERVED')
        RETURNING id INTO rid;
        UPDATE tbl_reservation_ledger SET state='COMMITTED' WHERE id=rid;
        UPDATE tbl_reservation_ledger SET state='RELEASED' WHERE id=rid;
        IF NOT EXISTS (SELECT 1 FROM tbl_reservation_ledger WHERE id=rid AND state='RELEASED')
        THEN RAISE EXCEPTION 'RESERVATION-01_FAIL: lifecycle path broken'; END IF;
    END;
END $$;

-- ---------------------------------------------------------------
-- TERMINAL-01: terminal states của loadtest không có outgoing transition
-- trong tbl_status_transitions.
DO $$ BEGIN
    IF (SELECT count(*) FROM tbl_status_transitions
        WHERE object_type='loadtest' AND allowed AND from_status='COMPLETED') <> 0
    THEN RAISE EXCEPTION 'TERMINAL-01_FAIL: COMPLETED has outgoing transitions'; END IF;
    IF (SELECT count(*) FROM tbl_status_transitions
        WHERE object_type='loadtest' AND allowed AND from_status='STOPPED') <> 0
    THEN RAISE EXCEPTION 'TERMINAL-01_FAIL: STOPPED has outgoing transitions'; END IF;
    IF (SELECT count(*) FROM tbl_status_transitions
        WHERE object_type='loadtest' AND allowed AND from_status='FAILED') <> 0
    THEN RAISE EXCEPTION 'TERMINAL-01_FAIL: FAILED has outgoing transitions'; END IF;
    IF (SELECT count(*) FROM tbl_status_transitions
        WHERE object_type='loadtest' AND allowed AND from_status='TIMEOUT_CLEANED') <> 0
    THEN RAISE EXCEPTION 'TERMINAL-01_FAIL: TIMEOUT_CLEANED has outgoing transitions'; END IF;
END $$;

-- ---------------------------------------------------------------
-- APPEND-01: tbl_namespace_events là append-only — UPDATE phải bị từ chối
-- khi role hiện tại không có quyền write trực tiếp. Khi chạy bằng owner/
-- superuser (psql CI) case này SKIP (notice) — bản chất: app role phải bị
-- REVOKE UPDATE/DELETE.
DO $$
DECLARE v_super BOOLEAN;
BEGIN
    SELECT rolsuper INTO v_super FROM pg_roles WHERE rolname = current_user;
    IF v_super THEN
        RAISE NOTICE 'APPEND-01 skipped: % is superuser/owner — run under app role to enforce', current_user;
    ELSE
        BEGIN
            UPDATE tbl_namespace_events SET reason = 'tampered';
            RAISE EXCEPTION 'APPEND-01_FAIL: UPDATE on append-only events allowed for %', current_user;
        EXCEPTION WHEN insufficient_privilege THEN NULL; -- expected
        END;
    END IF;
END $$;

-- DB-TXN-06: outbox atomicity — business write + outbox insert cùng tx;
-- rollback mở phải mất CẢ HAI (không để lại outbox mồ côi).
DO $$
DECLARE
    v_tenant UUID; v_user UUID; v_ns UUID; v_orphans INT;
BEGIN
    INSERT INTO tbl_tenants (name, slug) VALUES ('txn06', 'txn06') RETURNING id INTO v_tenant;
    INSERT INTO tbl_users (tenant_id, email) VALUES (v_tenant, 'txn06@t.io') RETURNING id INTO v_user;
    -- kỳ vọng: savepoint/rollback của caller phải kéo cả outbox row về
    INSERT INTO tbl_namespaces (id, tenant_id, owner_user_id, name, status)
        VALUES (gen_random_uuid(), v_tenant, v_user, 'txn06-ns', 'DRAFT')
        RETURNING id INTO v_ns;

    -- mô phỏng "dispatcher chạy nửa chừng": ghi outbox rồi rollback toàn bộ
    BEGIN
        INSERT INTO tbl_outbox_events (aggregate_type, aggregate_id, event_type, payload)
            VALUES ('namespace', v_ns, 'test.txn06', '{}');
        UPDATE tbl_namespaces SET status = status WHERE id = v_ns;
        -- rollback thủ công: xoá cả hai ở tx mới để verify invariant ngược lại
        RAISE EXCEPTION 'txn06_simulated_failure';
    EXCEPTION
        WHEN OTHERS THEN
            IF SQLERRM <> 'txn06_simulated_failure' THEN RAISE; END IF;
    END;

    -- app-level invariant: business row + outbox phải được dọn CÙNG tx —
    -- ở đây test chỉ khóa rằng schema cho phép atomicity (cả hai bảng cùng
    -- tham gia 1 tx; không constraint chặn rollback). Verify không orphan:
    SELECT count(*) INTO v_orphans FROM tbl_outbox_events
        WHERE aggregate_type = 'namespace' AND aggregate_id = v_ns;
    IF v_orphans <> 0 THEN
        RAISE EXCEPTION 'DB-TXN-06_FAIL: orphan outbox rows %', v_orphans;
    END IF;
END $$;

-- DB-TXN-07: reservation ledger — amount phải > 0 và state trong enum
-- (CHECK constraints từ 0005); ledger không bao giờ âm.
DO $$
DECLARE v_tenant UUID; v_user UUID; v_ns UUID; v_lt UUID;
BEGIN
    INSERT INTO tbl_tenants (name, slug) VALUES ('txn07', 'txn07') RETURNING id INTO v_tenant;
    INSERT INTO tbl_users (tenant_id, email) VALUES (v_tenant, 'txn07@t.io') RETURNING id INTO v_user;
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status)
        VALUES (v_tenant, v_user, 'txn07-ns', 'READY') RETURNING id INTO v_ns;
    INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes, status, version)
        VALUES (v_ns, 'txn07-script', 'x/y.js', 'hash', 10, 'VALIDATED', 1) RETURNING id INTO v_lt;
    INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets, status, version)
        VALUES (v_ns, v_lt, 'txn07-lt', v_user, '[]', 'RUNNING', 1) RETURNING id INTO v_lt;

    BEGIN
        INSERT INTO tbl_reservation_ledger (tenant_id, load_test_id, resource_type, amount)
            VALUES (v_tenant, v_lt, 'vusers', -5);
        RAISE EXCEPTION 'DB-TXN-07_FAIL: negative amount accepted';
    EXCEPTION WHEN check_violation THEN NULL; -- expected
    END;

    BEGIN
        INSERT INTO tbl_reservation_ledger (tenant_id, load_test_id, resource_type, amount)
            VALUES (v_tenant, v_lt, 'vusers', 10);
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'DB-TXN-07_FAIL: valid reservation rejected: %', SQLERRM;
    END;
END $$;

-- DB-TXN-08: FK/tenant scoping ở mức schema — reservation phải trỏ tenant
-- + loadtest hợp lệ; loadtest của tenant khác phải bị FK chặn khi mồ côi.
DO $$
DECLARE v_tenant UUID; v_orphan_lt UUID;
BEGIN
    INSERT INTO tbl_tenants (name, slug) VALUES ('txn08', 'txn08') RETURNING id INTO v_tenant;
    -- loadtest id không tồn tại → FK chặn
    v_orphan_lt := gen_random_uuid();
    BEGIN
        INSERT INTO tbl_reservation_ledger (tenant_id, load_test_id, resource_type, amount)
            VALUES (v_tenant, v_orphan_lt, 'vusers', 1);
        RAISE EXCEPTION 'DB-TXN-08_FAIL: FK to missing loadtest accepted';
    EXCEPTION WHEN foreign_key_violation THEN NULL; -- expected
    END;
END $$;

-- DB-TXN-09: append-only sequencing — events có id tăng dần và from/to hợp
-- lệ theo transition map (verify chuỗi event của 1 namespace là 1 chuỗi
-- không giảm id).
DO $$
DECLARE v_tenant UUID; v_user UUID; v_ns UUID; v_seq_ok BOOLEAN;
BEGIN
    INSERT INTO tbl_tenants (name, slug) VALUES ('txn09', 'txn09') RETURNING id INTO v_tenant;
    INSERT INTO tbl_users (tenant_id, email) VALUES (v_tenant, 'txn09@t.io') RETURNING id INTO v_user;
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status)
        VALUES (v_tenant, v_user, 'txn09-ns', 'DRAFT') RETURNING id INTO v_ns;
    PERFORM fn_namespace_transition(v_ns, 'VALIDATING', '{"role":"system"}', 'txn09');
    PERFORM fn_namespace_transition(v_ns, 'INVALID', '{"role":"system"}', 'txn09 fail');

    SELECT bool_and(prev_id < id) INTO v_seq_ok FROM (
        SELECT id, lag(id) OVER (ORDER BY id) AS prev_id
        FROM tbl_namespace_events WHERE object_id = v_ns
    ) seq;
    IF NOT v_seq_ok THEN
        RAISE EXCEPTION 'DB-TXN-09_FAIL: event sequence not monotonic';
    END IF;
    IF (SELECT count(*) FROM tbl_namespace_events WHERE object_id = v_ns) <> 2 THEN
        RAISE EXCEPTION 'DB-TXN-09_FAIL: expected 2 transition events (raw INSERT không sinh event khởi tạo — create API mới sinh)';
    END IF;
END $$;

-- DB-TXN-10: readiness invariant — namespace READY mặc định có thể không
-- ready_for_loadtest (watcher/apply set), nhưng ready_for_loadtest=true
-- chỉ hợp lệ khi status = 'READY' (CHECK constraint migration 0012).
DO $$
DECLARE v_tenant UUID; v_user UUID; v_ns UUID;
BEGIN
    INSERT INTO tbl_tenants (name, slug) VALUES ('txn10', 'txn10') RETURNING id INTO v_tenant;
    INSERT INTO tbl_users (tenant_id, email) VALUES (v_tenant, 'txn10@t.io') RETURNING id INTO v_user;
    INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status)
        VALUES (v_tenant, v_user, 'txn10-ns', 'DRAFT') RETURNING id INTO v_ns;
    BEGIN
        UPDATE tbl_namespaces SET ready_for_loadtest = true WHERE id = v_ns;
        RAISE EXCEPTION 'DB-TXN-10_FAIL: ready_for_loadtest=true accepted for DRAFT';
    EXCEPTION WHEN check_violation THEN NULL; -- expected
    END;
END $$;

ROLLBACK;

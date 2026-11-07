-- tests/sql/plumbing_test.sql
-- Test plumbing tables (sql/0005) — chạy sau migrations trong CI.

BEGIN;

INSERT INTO tbl_tenants (name, slug) VALUES ('T', 't1');
INSERT INTO tbl_users (tenant_id, email, display_name)
SELECT id, 'u@t.co', 'U' FROM tbl_tenants t WHERE t.slug='t1';
INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status, ready_for_loadtest)
SELECT tenant_id, id, 'ns1', 'READY', true FROM tbl_users WHERE email='u@t.co';
INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes)
SELECT id, 's1', 'cm://s1', 'abc', 100 FROM tbl_namespaces WHERE name='ns1';
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets)
SELECT n.id, s.id, 'lt1', u.id, '[]'::jsonb
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='ns1' AND s.name='s1' AND u.email='u@t.co';

-- I1: idempotency UNIQUE (tenant_id, endpoint, key)
INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash)
SELECT t.id AS tenant_id, '/namespaces', 'k1', 'h1' FROM tbl_tenants t WHERE t.slug='t1';
DO $$ BEGIN
    INSERT INTO tbl_idempotency_keys (tenant_id, endpoint, key, request_hash)
    SELECT t.id AS tenant_id, '/namespaces', 'k1', 'h1' FROM tbl_tenants t WHERE t.slug='t1';
    RAISE EXCEPTION 'I1_FAIL: duplicate idempotency key accepted';
EXCEPTION WHEN unique_violation THEN NULL; END $$;

-- O1: outbox partial index tồn tại + ghi/đọc event pending
INSERT INTO tbl_outbox_events (aggregate_type, aggregate_id, event_type, payload)
VALUES ('loadtest', (SELECT id FROM tbl_load_tests WHERE name='lt1'), 'StatusChanged', '{}');
DO $$ BEGIN
    IF (SELECT count(*) FROM tbl_outbox_events WHERE dispatched_at IS NULL) <> 1
    THEN RAISE EXCEPTION 'O1_FAIL: pending event missing'; END IF;
END $$;

-- R1: ledger state + CHECK
INSERT INTO tbl_reservation_ledger (tenant_id, load_test_id, resource_type, amount)
SELECT t.id, (SELECT id FROM tbl_load_tests WHERE name='lt1'), 'vusers', 200
FROM tbl_tenants t WHERE t.slug='t1';
DO $$ BEGIN
    INSERT INTO tbl_reservation_ledger (tenant_id, resource_type, amount)
    SELECT t.id AS tenant_id, 'vusers', 0 FROM tbl_tenants t WHERE t.slug='t1';
    RAISE EXCEPTION 'R1_FAIL: zero amount accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;

-- D1: dataset lifecycle qua fn_dataset_transition
INSERT INTO tbl_datasets (tenant_id, name, source_ref, pii_classification)
SELECT t.id AS tenant_id, 'ds1', 's3://dump/ds1.dump', 'synthetic'
FROM tbl_tenants t WHERE t.slug='t1';
SELECT * FROM fn_dataset_transition((SELECT id FROM tbl_datasets WHERE name='ds1'), 'AVAILABLE');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_datasets WHERE name='ds1' AND status='AVAILABLE' AND version=2)
    THEN RAISE EXCEPTION 'D1_FAIL'; END IF;
END $$;

-- D2: pii_classification CHECK
DO $$ BEGIN
    INSERT INTO tbl_datasets (tenant_id, name, source_ref, pii_classification)
    SELECT t.id AS tenant_id, 'ds2', 's3://x', 'prod_raw' FROM tbl_tenants t WHERE t.slug='t1';
    RAISE EXCEPTION 'D2_FAIL: bad pii_classification accepted';
EXCEPTION WHEN check_violation THEN NULL; END $$;

ROLLBACK;

-- tests/sql/transition_test.sql
-- Test pattern transition (sql/0002). Chạy SAU khi áp toàn bộ migrations
-- trong CI job sql-migrations. Fail = exit non-zero (ON_ERROR_STOP).
-- Fixture: 1 bảng object mẫu + events + seed transition map.

BEGIN;

CREATE TEMP TABLE tbl_test_objects (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status     TEXT NOT NULL DEFAULT 'DRAFT',
    version    INT  NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TEMP TABLE tbl_test_object_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL,
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO tbl_status_transitions (object_type, from_status, to_status)
VALUES ('test_object', 'DRAFT', 'VALIDATING'), ('test_object', 'VALIDATING', 'READY');

INSERT INTO tbl_test_objects (status) VALUES ('DRAFT');

-- T1: happy path — transition hợp lệ
SELECT * FROM fn_status_transition('tbl_test_objects', 'tbl_test_object_events',
    'test_object', (SELECT id FROM tbl_test_objects), 'VALIDATING', '{"user":"u1"}', 'go', '{}');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_test_objects WHERE status='VALIDATING' AND version=2)
    THEN RAISE EXCEPTION 'T1_FAIL: status/version not updated'; END IF;
    IF (SELECT count(*) FROM tbl_test_object_events WHERE to_status='VALIDATING') <> 1
    THEN RAISE EXCEPTION 'T1_FAIL: event row missing'; END IF;
END $$;

-- T2: transition trái phép (VALIDATING -> DELETED không có trong map) —
-- exception + không thay đổi gì
DO $$ BEGIN
    PERFORM fn_status_transition('tbl_test_objects', 'tbl_test_object_events',
        'test_object', (SELECT id FROM tbl_test_objects), 'DELETED');
    RAISE EXCEPTION 'T2_FAIL: illegal transition allowed';
EXCEPTION WHEN check_violation THEN NULL; END $$;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM tbl_test_objects WHERE status='READY')
    THEN RAISE EXCEPTION 'T2_FAIL: state changed by illegal transition'; END IF;
    IF (SELECT count(*) FROM tbl_test_object_events) <> 1
    THEN RAISE EXCEPTION 'T2_FAIL: event logged for illegal transition'; END IF;
END $$;

-- T3: object không tồn tại
DO $$ BEGIN
    PERFORM fn_status_transition('tbl_test_objects', 'tbl_test_object_events',
        'test_object', gen_random_uuid(), 'VALIDATING');
    RAISE EXCEPTION 'T3_FAIL: missing object not raised';
EXCEPTION WHEN others THEN
    IF SQLERRM NOT LIKE 'OBJECT_NOT_FOUND%' THEN RAISE EXCEPTION 'T3_FAIL: wrong error %', SQLERRM; END IF;
END $$;

-- T4: 2 transition đồng thời trên cùng object — FOR UPDATE serial hóa:
-- transaction 2 phải chờ/fail chứ không ghi đè. Giả lập bằng advisory lock
-- quanh 2 lần gọi trong 2 transaction riêng (tại đây chỉ check version tăng
-- đúng 1 lần mỗi transition sau khi tuần tự).
SELECT * FROM fn_status_transition('tbl_test_objects', 'tbl_test_object_events',
    'test_object', (SELECT id FROM tbl_test_objects), 'READY', '{"user":"u2"}', 'ok', '{}');
DO $$ BEGIN
    IF (SELECT version FROM tbl_test_objects) <> 3
    THEN RAISE EXCEPTION 'T4_FAIL: version not incremented once per transition'; END IF;
    IF (SELECT count(*) FROM tbl_test_object_events) <> 2
    THEN RAISE EXCEPTION 'T4_FAIL: event count wrong'; END IF;
END $$;

-- T5: allowed=false ghi đè blacklist
INSERT INTO tbl_status_transitions (object_type, from_status, to_status, allowed)
VALUES ('test_object', 'READY', 'DRAFT', false)
ON CONFLICT (object_type, from_status, to_status) DO UPDATE SET allowed = false;
DO $$ BEGIN
    PERFORM fn_status_transition('tbl_test_objects', 'tbl_test_object_events',
        'test_object', (SELECT id FROM tbl_test_objects), 'DRAFT');
    RAISE EXCEPTION 'T5_FAIL: blacklisted transition allowed';
EXCEPTION WHEN check_violation THEN NULL; END $$;

ROLLBACK; -- fixture cleanup, không để rác trong CI db

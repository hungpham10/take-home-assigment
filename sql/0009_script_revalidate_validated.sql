-- 0009_script_revalidate_validated.sql — cho phép re-validate script VALIDATED
-- (upload spec mới / đổi content hash cần chạy lại k6 inspect).

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0009_script_revalidate_validated.sql')
    ON CONFLICT (filename) DO NOTHING;

INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('script', 'VALIDATED', 'VALIDATING')
ON CONFLICT (object_type, from_status, to_status) DO NOTHING;

COMMIT;

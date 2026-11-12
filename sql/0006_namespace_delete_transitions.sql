-- 0006_namespace_delete_transitions.sql — cho phép soft-delete từ mọi trạng thái
-- không terminal (docs: DELETE namespace từ DRAFT/INVALID/READY/DEGRADED).
-- 0003 chỉ seed DELETING từ READY/DEGRADED/INVALID.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0006_namespace_delete_transitions.sql')
    ON CONFLICT (filename) DO NOTHING;

INSERT INTO tbl_status_transitions (object_type, from_status, to_status) VALUES
    ('namespace', 'DRAFT', 'DELETING')
ON CONFLICT (object_type, from_status, to_status) DO NOTHING;

COMMIT;

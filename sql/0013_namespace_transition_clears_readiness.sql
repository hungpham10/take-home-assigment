-- 0013_namespace_transition_clears_readiness.sql — plans/30
--
-- Root cause (CI run 42772143419): READY(ready_for_loadtest=true) → DELETING
-- vi phạm CHECK chk_namespaces_ready_only_when_ready (sql/0012), vì
-- fn_status_transition chỉ UPDATE status+version — không đụng ready_for_loadtest.
-- deleteNamespace sau đó map TẤT CẢ error → 409 INVALID_TRANSITION, mask lỗi DB thật.
--
-- Sửa: fn_namespace_transition (plpgsql) clear ready_for_loadtest=FALSE khi rời
-- READY (target <> 'READY') TRƯỚC khi gọi fn_status_transition, cùng statement
-- ⇒ atomic, cùng rollback. CHECK 0012 giữ nguyên làm backstop.
--
-- CREATE OR REPLACE giữ signature (caller không đổi); chỉ đổi LANGUAGE sql → plpgsql.
-- Idempotent (ghi tbl_schema_migrations, ON CONFLICT).

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0013_namespace_transition_clears_readiness.sql')
    ON CONFLICT (filename) DO NOTHING;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'fn_namespace_transition') THEN
        RAISE EXCEPTION 'fn_namespace_transition missing (run 0003 first)';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION fn_namespace_transition(
    p_id UUID, p_to_status TEXT,
    p_actor JSONB DEFAULT '{}', p_reason TEXT DEFAULT NULL, p_meta JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
BEGIN
    -- Rời READY sang bất kỳ trạng thái nào ⇒ clear readiness trong cùng transition.
    -- ready_for_loadtest=false luôn thỏa mãn CHECK (NOT ready_for_loadtest OR status='READY').
    IF p_to_status <> 'READY' THEN
        UPDATE tbl_namespaces n
           SET ready_for_loadtest = false
         WHERE n.id = p_id
           AND n.ready_for_loadtest;
    END IF;

    RETURN QUERY SELECT * FROM fn_status_transition(
        'tbl_namespaces', 'tbl_namespace_events', 'namespace',
        p_id, p_to_status, p_actor, p_reason, p_meta);
END;
$$ LANGUAGE plpgsql;

COMMIT;

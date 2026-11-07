-- 0002_status_transitions.sql — Pattern quản lý chuyển đổi trạng thái (plans/04)
--
-- Mọi object có state machine (namespace, script, loadtest, dataset) chuyển
-- trạng thái CHỈ qua fn_<object>_transition(...):
--   1. SELECT ... FOR UPDATE row current-state  -> lock, chống race
--   2. fn_is_transition_allowed(...)            -> check transition map
--   3. UPDATE status, version = version + 1     -> optimistic concurrency
--   4. INSERT tbl_<object>_events               -> append-only log
-- Tất cả trong 1 transaction của caller; exception -> rollback toàn bộ.
--
-- tbl_<object> = current state (nguồn đọc cho list/get, JOIN + index).
-- tbl_<object>_events = log, chỉ JOIN khi xem lịch sử/timeline.
-- App không bao giờ UPDATE status trực tiếp; exception map 409/422.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0002_status_transitions.sql')
    ON CONFLICT (filename) DO NOTHING;

-- ------------------------------------------------- tbl_status_transitions
-- Transition map chung. Seed transition hợp lệ đi kèm migration của từng
-- domain (0003+). `allowed = false` dùng để ghi đè blacklist rõ ràng.
CREATE TABLE tbl_status_transitions (
    id          BIGSERIAL PRIMARY KEY,
    object_type TEXT NOT NULL,
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    allowed     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (object_type, from_status, to_status)
);
CREATE INDEX idx_status_transitions_object ON tbl_status_transitions(object_type, from_status);

-- ------------------------------------------------- fn_is_transition_allowed
-- true chỉ khi có row (object_type, from, to) với allowed = true.
CREATE OR REPLACE FUNCTION fn_is_transition_allowed(
    p_object_type TEXT,
    p_from TEXT,
    p_to TEXT
) RETURNS BOOLEAN AS $$
    SELECT EXISTS (
        SELECT 1 FROM tbl_status_transitions
         WHERE object_type = p_object_type
           AND from_status = p_from
           AND to_status   = p_to
           AND allowed
    );
$$ LANGUAGE sql STABLE;

-- ------------------------------------------------- fn_status_transition
-- Template dùng chung: object_type + 2 tham số bảng động (bảng chính và bảng
-- events). Dùng format %I để an toàn identifier; các domain migration 0003+
-- có thể tạo wrapper fn_<object>_transition gọi hàm này.
--
-- p_object_id     : id row trong bảng chính
-- p_to_status     : trạng thái đích
-- p_actor         : principal từ trusted headers (JSONB)
-- p_reason        : lý do transition (nullable)
-- p_meta          : metadata bổ sung (JSONB)
-- RETURNS TABLE(id UUID, status TEXT, version INT)
CREATE OR REPLACE FUNCTION fn_status_transition(
    p_main_table  TEXT,     -- vd 'tbl_namespaces'
    p_events_table TEXT,    -- vd 'tbl_namespace_events'
    p_object_type TEXT,     -- vd 'namespace'
    p_object_id   UUID,
    p_to_status   TEXT,
    p_actor       JSONB DEFAULT '{}',
    p_reason      TEXT DEFAULT NULL,
    p_meta        JSONB DEFAULT '{}'
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
DECLARE
    v_from    TEXT;
    v_version INT;
    v_sql     TEXT;
BEGIN
    -- 1. Lock row current-state (FOR UPDATE chống 2 transition đồng thời)
    -- Lưu ý: FOUND không đáng tin sau EXECUTE...INTO...USING với FOR UPDATE
    -- (đã verify trên PG16) — check giá trị NULL thay vì FOUND.
    v_sql := format(
        'SELECT status, version FROM %I WHERE id = $1 FOR UPDATE', p_main_table);
    EXECUTE v_sql INTO v_from, v_version USING p_object_id;
    IF v_from IS NULL THEN
        RAISE EXCEPTION 'OBJECT_NOT_FOUND % %', p_object_type, p_object_id;
    END IF;

    -- 2. Check transition map
    IF NOT fn_is_transition_allowed(p_object_type, v_from, p_to_status) THEN
        RAISE EXCEPTION 'INVALID_TRANSITION % % -> %', p_object_type, v_from, p_to_status
            USING ERRCODE = 'check_violation';
    END IF;

    -- 3. Update current-state + version (optimistic concurrency)
    v_sql := format(
        'UPDATE %I SET status = $1, version = version + 1
          WHERE id = $2 RETURNING id, status, version', p_main_table);
    RETURN QUERY EXECUTE v_sql USING p_to_status, p_object_id;

    -- 4. Ghi event (append-only) — cùng transaction
    v_sql := format(
        'INSERT INTO %I (object_id, from_status, to_status, reason, actor, metadata)
         VALUES ($1, $2, $3, $4, $5, $6)', p_events_table);
    EXECUTE v_sql USING p_object_id, v_from, p_to_status, p_reason, p_actor, p_meta;
END;
$$ LANGUAGE plpgsql;

COMMIT;

# Plan 04 — Pattern `tbl_<object>_events` + DB Transaction Transition

## Mục tiêu
Chuẩn hóa cách quản lý chuyển đổi trạng thái: mọi state change phải đi qua **DB function trong 1 transaction** — kiểm tra transition hợp lệ, cập nhật status + version, ghi event — đảm bảo không bao giờ có trạng thái không nhất quán hoặc event mất.

## Dependencies
- Plan 03 (quy ước + migrations đã có nền).

## Thiết kế pattern chuẩn

### Bảng events (mẫu, mỗi object có state machine 1 bảng riêng)
```sql
CREATE TABLE tbl_<object>_events (
    id          BIGSERIAL PRIMARY KEY,
    object_id   UUID NOT NULL REFERENCES tbl_<object>(id),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    actor       JSONB NOT NULL DEFAULT '{}',   -- principal từ trusted headers
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_<object>_events_object ON tbl_<object>_events(object_id, id);
-- append-only:
REVOKE UPDATE, DELETE ON tbl_<object>_events FROM app_role;
```

### DB function transition (mẫu)
```sql
CREATE FUNCTION fn_<object>_transition(
    p_id UUID, p_to_status TEXT, p_actor JSONB, p_reason TEXT DEFAULT NULL
) RETURNS TABLE (id UUID, status TEXT, version INT) AS $$
DECLARE v_from TEXT; v_version INT;
BEGIN
    SELECT status, version INTO v_from, v_version
      FROM tbl_<object> WHERE id = p_id FOR UPDATE;   -- lock row

    IF NOT FOUND THEN RAISE EXCEPTION 'OBJECT_NOT_FOUND'; END IF;

    -- kiểm tra transition hợp lệ theo tbl_status_transitions (Plan 07)
    IF NOT fn_is_transition_allowed('<object>', v_from, p_to_status) THEN
        RAISE EXCEPTION 'INVALID_TRANSITION % -> %', v_from, p_to_status;
    END IF;

    UPDATE tbl_<object> SET status = p_to_status, version = version + 1
     WHERE id = p_id RETURNING version INTO v_version;

    INSERT INTO tbl_<object>_events(object_id, from_status, to_status, reason, actor)
    VALUES (p_id, v_from, p_to_status, p_reason, p_actor);

    RETURN QUERY SELECT p_id, p_to_status, v_version;
END $$ LANGUAGE plpgsql;
```
- Gọi trong transaction của app: rollback toàn bộ nếu exception.
- App map exception → HTTP: `INVALID_TRANSITION`/`OBJECT_NOT_FOUND` → `409`; sai dữ liệu → `422`.
- Status hiện tại + version là nguồn thật; API không bao giờ `UPDATE status` trực tiếp.

### Transition map
- Bảng chung `tbl_status_transitions (object_type, from_status, to_status, allowed BOOL)` (tạo ở Plan 07) hoặc hardcode trong function — quyết định khi implement; ưu tiên bảng chung để dễ audit.

## Current-state table (`tbl_<object>`) & chiến lược đọc

- `tbl_<object>` là **bảng trạng thái cuối (current state)** — nguồn đọc duy nhất cho status/version hiện tại. Events là append-only log, **không dùng làm nguồn đọc trạng thái**.
- Truy vấn đọc (list/get, readiness, list loadtests...) đọc trực tiếp `tbl_<object>`, tối ưu bằng JOIN + index:
  - `tbl_load_tests JOIN tbl_namespaces` (lọc tenant/owner), `tbl_scripts JOIN tbl_namespaces`, v.v.
  - Index hỗ trợ: `(tenant_id, status)`, `(namespace_id, status)`, index trên mọi FK.
- Events chỉ JOIN khi xem lịch sử/timeline (`GET /loadtests/{id}/events`, audit) — dữ liệu cold, index `(object_id, id)`.

## Deliverables
- `sql/` helper migration: `tbl_status_transitions` + `fn_is_transition_allowed` (nếu chọn bảng chung).
- `docs/database-schema.md` bổ sung section "State transitions & events".
- `tests/sql/transition_test.sql`: script test (chạy trong CI job sql-migrations) — happy path, transition trái phép → exception + không thay đổi, version+1, đúng 1 event row mỗi lần.

## Verification
- Chạy test SQL: gọi `fn_*_transition` hợp lệ → status đổi + version+1 + 1 event; gọi trái phép → exception, mọi thay đổi rollback.
- 2 transaction đồng thời transition cùng object → 1 thành công, 1 fail (đảm bảo nhờ `FOR UPDATE`).

## Ghi chú
- Pattern này áp dụng cho namespace (Plan 05), script/loadtest (Plan 06), dataset (Plan 07).

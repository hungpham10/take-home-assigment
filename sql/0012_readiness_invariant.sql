-- 0012_readiness_invariant.sql — DB-TXN-10 (plans/24)
--
-- Invariant readiness: ready_for_loadtest=true chỉ hợp lệ khi status='READY'
-- (theo docs/namespace-status-watcher.md §5 — chỉ watcher/legacy-apply ghi
-- ready_for_loadtest, và chúng chỉ ghi khi aggregate quyết định READY).
-- NOT VALID: dữ liệu cũ sẵn có không bị revoke, chỉ chặn ghi mới sai.

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0012_readiness_invariant.sql')
    ON CONFLICT (filename) DO NOTHING;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_namespaces_ready_only_when_ready') THEN
        ALTER TABLE tbl_namespaces ADD CONSTRAINT chk_namespaces_ready_only_when_ready
            CHECK (NOT ready_for_loadtest OR status = 'READY') NOT VALID;
    END IF;
END $$;

COMMIT;

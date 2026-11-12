-- 0014_loadtest_results.sql — k6 summary collection (plans/45)
--
-- A load test used to end in a status and nothing else: the platform could say
-- *that* a run finished, not whether it passed. The reconciler now stores the
-- parsed end-of-run summary here, in the same transaction as the terminal
-- status transition.
--
-- Why nullable and additive:
--   * old runs (and runs whose pod log is already gone) legitimately have no
--     result — NULL means "not collected", not "collected nothing";
--   * four nullable columns with no default are a metadata-only change in
--     PostgreSQL 11+ (no table rewrite, no backfill);
--   * no CHECK constraint ties these columns to `status`, so a result can be
--     written for COMPLETED, FAILED or STOPPED alike.
--
-- Idempotent: safe to re-run (ADD COLUMN IF NOT EXISTS + the migrations ledger).

BEGIN;

INSERT INTO tbl_schema_migrations (filename) VALUES ('0014_loadtest_results.sql')
    ON CONFLICT (filename) DO NOTHING;

ALTER TABLE tbl_load_tests
    ADD COLUMN IF NOT EXISTS result_summary      JSONB,
    ADD COLUMN IF NOT EXISTS thresholds_failed   BOOLEAN,
    ADD COLUMN IF NOT EXISTS result_collected_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS result_parse_error  TEXT;

COMMENT ON COLUMN tbl_load_tests.result_summary IS
  'parsed k6 summary (pkgs/runner.Summary) — NULL when never collected';
COMMENT ON COLUMN tbl_load_tests.thresholds_failed IS
  'true when any k6 threshold was violated — NULL when no summary was parsed';
COMMENT ON COLUMN tbl_load_tests.result_collected_at IS
  'when collection ran (success OR failure); the idempotency key for repeated reconciler ticks';
COMMENT ON COLUMN tbl_load_tests.result_parse_error IS
  'why the summary is missing/partial (pod GC''d, no k6 output, timeout, …)';

COMMIT;

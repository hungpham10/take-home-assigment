-- tests/sql/loadtest_results_test.sql
-- Migration 0014 (plans/45): result columns are additive, nullable, and do not
-- constrain the status machine. Run after migrations in CI.
--
-- Pattern: BEGIN → fixtures → assertions → ROLLBACK.

BEGIN;

DO $$
DECLARE
    missing TEXT;
BEGIN
    SELECT string_agg(c, ', ')
      INTO missing
      FROM unnest(ARRAY['result_summary', 'thresholds_failed',
                        'result_collected_at', 'result_parse_error']) AS c
     WHERE NOT EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_name = 'tbl_load_tests' AND column_name = c
     );
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION '0014 columns missing: %', missing;
    END IF;
END $$;

-- ------------------------------------------------------------------ fixtures
INSERT INTO tbl_tenants (name, slug) VALUES ('R', 'r1');
INSERT INTO tbl_users (tenant_id, email, display_name)
SELECT id, 'r@r.co', 'R' FROM tbl_tenants WHERE slug='r1';
INSERT INTO tbl_namespaces (tenant_id, owner_user_id, name, status, ready_for_loadtest)
SELECT tenant_id, id, 'rns', 'READY', true FROM tbl_users WHERE email='r@r.co';
INSERT INTO tbl_scripts (namespace_id, name, content_ref, content_hash, size_bytes, status)
SELECT id, 'rs', 'cm://rs', 'abc', 100, 'VALIDATED' FROM tbl_namespaces WHERE name='rns';

-- RES-01: a run inserted without results reads back as NULL (old rows / not
-- collected), never as an empty JSON object.
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets,
                            runner_type, status, version)
SELECT n.id, s.id, 'old-run', u.id, '[]'::jsonb, 'local', 'COMPLETED', 1
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='rns' AND s.name='rs' AND u.email='r@r.co';

DO $$
DECLARE
    collected TIMESTAMPTZ;
    summary   JSONB;
    failed    BOOLEAN;
    perr      TEXT;
BEGIN
    SELECT result_collected_at, result_summary, thresholds_failed, result_parse_error
      INTO collected, summary, failed, perr
      FROM tbl_load_tests WHERE name='old-run';
    IF collected IS NOT NULL OR summary IS NOT NULL OR failed IS NOT NULL OR perr IS NOT NULL THEN
        RAISE EXCEPTION 'RES-01: fresh row has non-NULL results (collected=%, summary=%, failed=%, err=%)',
            collected, summary, failed, perr;
    END IF;
END $$;

-- RES-02: a collected result can be attached to a COMPLETED run and is readable
-- back verbatim (JSONB round-trip, threshold flag, timestamp).
UPDATE tbl_load_tests
   SET result_summary = '{"http_reqs":12480,"rps":41.6,"p95_ms":82,"thresholds_failed":true}'::jsonb,
       thresholds_failed = true,
       result_collected_at = now(),
       result_parse_error = NULL
 WHERE name = 'old-run';

DO $$
DECLARE
    reqs BIGINT;
    failed BOOLEAN;
    collected TIMESTAMPTZ;
BEGIN
    SELECT (result_summary->>'http_reqs')::BIGINT, thresholds_failed, result_collected_at
      INTO reqs, failed, collected
      FROM tbl_load_tests WHERE name='old-run';
    IF reqs <> 12480 OR NOT failed OR collected IS NULL THEN
        RAISE EXCEPTION 'RES-02: result round-trip failed (reqs=%, failed=%, collected=%)',
            reqs, failed, collected;
    END IF;
END $$;

-- RES-03: results do not constrain the status machine — a FAILED run can carry
-- a summary and a parse error at the same time (partial log before a crash).
INSERT INTO tbl_load_tests (namespace_id, script_id, name, created_by, targets,
                            runner_type, status, version, result_parse_error,
                            result_collected_at)
SELECT n.id, s.id, 'partial-run', u.id, '[]'::jsonb, 'local', 'FAILED', 1,
       'summary unavailable: runner pod not found', now()
FROM tbl_namespaces n, tbl_scripts s, tbl_users u
WHERE n.name='rns' AND s.name='rs' AND u.email='r@r.co';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM tbl_load_tests WHERE name='partial-run'
                    AND result_summary IS NULL AND result_parse_error IS NOT NULL) THEN
        RAISE EXCEPTION 'RES-03: FAILED run with a parse error was not stored';
    END IF;
END $$;

ROLLBACK;

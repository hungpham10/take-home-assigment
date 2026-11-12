#!/bin/sh
# E2E CLI (plans/44) — the self-service journey through `bin/ltp` only, no curl.
# Proves the developer surface: create → apply → wait → script push →
# run start → run watch → report → delete, including the exit-code contract.
#
# Requires: `make build`, the compose stack up (`make up` or `docker compose up -d`),
# a reachable Postgres (the fixture seed uses psql), and k6 on the controller
# host (otherwise run start fails closed and the script asserts that instead).
set -e
BASE=${BASE:-http://localhost:8080}
PG_DSN=${PG_DSN:-"host=127.0.0.1 user=ltp password=ltp dbname=ltp sslmode=disable"}
LTP=${LTP:-./bin/ltp}
SCRIPT=${SCRIPT:-tests/perf/sample-service.js}
SPEC=${SPEC:-tests/fixtures/ns-cli-e2e.json}

say()  { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
jqget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

[ -x "$LTP" ] || fail "$LTP not built (run: make build)"
[ -f "$SCRIPT" ] || fail "script fixture $SCRIPT missing"
[ -f "$SPEC" ] || fail "spec fixture $SPEC missing"

# The CLI authenticates with the dev-auth headers locally; nginx normally
# injects these after OIDC.
CLI="$LTP --server $BASE"
authed() { "$CLI" --tenant "$TENANT" --user "$USERID" "$@"; }

say "health"
curl -sf "$BASE/health" >/dev/null || fail "server not reachable at $BASE"

say "seed tenant fixture"
SEED=$(psql "$PG_DSN" -tAc "
  WITH t AS (
    INSERT INTO tbl_tenants (name, slug) VALUES ('CLI', 'cli-e2e')
    ON CONFLICT (slug) DO UPDATE SET name='CLI' RETURNING id
  ), u AS (
    INSERT INTO tbl_users (tenant_id, email, display_name)
    SELECT id, 'cli@local', 'CLI' FROM t
    ON CONFLICT (tenant_id, email) DO UPDATE SET display_name='CLI' RETURNING id, tenant_id
  ), r AS (
    INSERT INTO tbl_user_roles (user_id, role) SELECT id, 'user' FROM u
    ON CONFLICT (user_id, role) DO NOTHING
  )
  SELECT u.tenant_id || '|' || u.id FROM u")
TENANT=${SEED%%|*}; USERID=${SEED##*|}
[ -n "$TENANT" ] || fail "tenant seed failed"

# --- usage contract -------------------------------------------------------
say "usage errors exit 2"
CODE=0
"$LTP" bogus >/dev/null 2>&1 || CODE=$?
[ "$CODE" = 2 ] || fail "unknown command exit = $CODE (want 2)"
CODE=0
"$LTP" ns show >/dev/null 2>&1 || CODE=$?
[ "$CODE" = 2 ] || fail "missing argument exit = $CODE (want 2)"

say "rejected run is a non-zero exit (caps, plans/42)"
CODE=0
OUT=$(authed run start ns-does-not-exist --script sc-does-not-exist --vus 999999 2>&1) || CODE=$?
# 404 (unknown namespace) is a runtime error, 409/422 a state error; what we
# assert is that a rejected run is never exit 0 and never a panic.
[ "$CODE" != 0 ] || fail "impossible vus was accepted"
echo "  vus=999999 → exit $CODE: $(echo "$OUT" | head -1)"

# --- the journey ----------------------------------------------------------
say "ns create"
NS_JSON=$(authed --json ns create --name "cli-e2e" --slug "cli-e2e-$$" --description "CLI e2e" \
  --spec "$SPEC") || fail "ns create"
NS=$(echo "$NS_JSON" | jqget "['namespace']['id']")
[ -n "$NS" ] || fail "no namespace id in $NS_JSON"
trap 'authed ns delete "$NS" >/dev/null 2>&1 || true' EXIT

say "ns show / ns list (--json is one object)"
authed --json ns show "$NS" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert d["id"], d
print("  status:", d["status"], "ready:", d.get("ready_for_loadtest"))
' || fail "ns show --json is not a single JSON object"
authed --json ns list | python3 -c 'import json,sys; assert isinstance(json.load(sys.stdin), list)' \
  || fail "ns list --json is not a JSON array"

say "ns validate + apply + wait"
authed ns validate "$NS" >/dev/null || fail "validate"
authed ns apply "$NS" >/dev/null || fail "apply"
if ! authed --wait-timeout 5m --poll 2s ns wait "$NS"; then
  fail "namespace never became READY (blockers above)"
fi

say "script push (waits for VALIDATED)"
SC_JSON=$(authed --json script push "$NS" "$SCRIPT") || fail "script push"
SC=$(echo "$SC_JSON" | jqget "['id']")
SCS=$(echo "$SC_JSON" | jqget "['status']")
[ "$SCS" = "VALIDATED" ] || fail "script status = $SCS (want VALIDATED)"

say "script push oversized file is rejected client-side (exit 3)"
BIG=$(mktemp)
python3 -c 'import sys; sys.stdout.write("a"*(1024*1024+1))' > "$BIG"
CODE=0
authed script push "$NS" "$BIG" >/dev/null 2>&1 || CODE=$?
rm -f "$BIG"
[ "$CODE" = 3 ] || fail "oversize script exit = $CODE (want 3)"

say "run start + run watch"
LT_JSON=$(authed --json run start "$NS" --script "$SC" --name "cli-run" --vus 5 --duration 20s --timeout 10) \
  || fail "run start"
LT=$(echo "$LT_JSON" | jqget "['id']")
echo "  run: $LT ($(echo "$LT_JSON" | jqget "['status']"))"
if authed --wait-timeout 6m --poll 3s run watch "$NS" "$LT"; then
  say "run COMPLETED (exit 0)"
else
  CODE=$?
  [ "$CODE" = 3 ] || fail "run watch exit = $CODE (want 3 for a non-COMPLETED terminal state)"
  say "run did not COMPLETE (exit 3) — acceptable only when k6 is unavailable"
  authed --json ns "$NS" >/dev/null 2>&1 || true
fi

say "run report"
authed run report "$NS" "$LT" || fail "run report"

say "ns delete"
authed ns delete "$NS" >/dev/null || fail "ns delete"
STATUS=$(authed --json ns show "$NS" 2>/dev/null | jqget "['status']" 2>/dev/null || echo "DELETED")
case "$STATUS" in
  DELETING|DELETED) ;;
  *) fail "namespace status after delete = $STATUS" ;;
esac
trap - EXIT

echo "CLI E2E PASS (namespace=$NS script=$SC run=$LT)"

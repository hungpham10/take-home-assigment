#!/bin/sh
# E2E smoke — namespace→service→validate→apply→script→loadtest→delete (POSIX sh).
# Yêu cầu: controller tại ${BASE:-http://localhost:8080}; psql trỏ DB (PG_DSN).
set -e
BASE=${BASE:-http://localhost:8080}
PG_DSN=${PG_DSN:-"host=127.0.0.1 user=ltp password=ltp dbname=ltp sslmode=disable"}

say()  { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
jqget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

say "health"
curl -sf "$BASE/health" >/dev/null || fail "health"

say "seed tenant fixture"
SEED=$(psql "$PG_DSN" -tAc "
  WITH t AS (
    INSERT INTO tbl_tenants (name, slug) VALUES ('Smoke', 'smoke')
    ON CONFLICT (slug) DO UPDATE SET name='Smoke' RETURNING id
  ), u AS (
    INSERT INTO tbl_users (tenant_id, email, display_name)
    SELECT id, 'smoke@local', 'Smoke' FROM t
    ON CONFLICT (tenant_id, email) DO UPDATE SET display_name='Smoke' RETURNING id, tenant_id
  ), r AS (
    INSERT INTO tbl_user_roles (user_id, role) SELECT id, 'user' FROM u
    ON CONFLICT (user_id, role) DO NOTHING
  ), h AS (
    INSERT INTO tbl_tenant_hosts (tenant_id, host) SELECT tenant_id, 'smoke.local' FROM u
    ON CONFLICT (host) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
  )
  SELECT tenant_id || '|' || id FROM u")
TENANT=${SEED%%|*}; USERID=${SEED##*|}
[ -n "$TENANT" ] || fail "seed"

say "create namespace"
NS=$(curl -sf -X POST "$BASE/api/loadtest/v1/namespaces" -H "Idempotency-Key: smoke-ns-$(date +%s)" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H "X-User-Role: user" \
  -H 'Content-Type: application/json' \
  -d '{"name":"smoke-env","description":"e2e"}' | jqget "['id']") || fail "create ns"

say "add service (OpenAPI)"
SPEC='{"openapi":"3.0.0","info":{"title":"W","version":"1"},"paths":{"/widgets":{"get":{"operationId":"listWidgets","responses":{"200":{"description":"ok"}}}}}}'
curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/services" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H "X-User-Role: user" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"widgets\",\"spec\":$SPEC}" >/dev/null || fail "create service"

say "validate + apply"
curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/validate" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "validate"
curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/apply" -H "Idempotency-Key: smoke-apply" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "apply"
STATUS=$(curl -sf "$BASE/api/loadtest/v1/namespaces/$NS" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" | jqget "['status']")
[ "$STATUS" = "READY" ] || fail "namespace status = $STATUS (want READY)"

say "upload script"
SC=$(curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/scripts" -H "Idempotency-Key: smoke-sc" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H 'Content-Type: application/json' \
  -d '{"name":"smoke.js","content":"export default function(){}"}' | jqget "['id']") || fail "upload script"
SCS=$(curl -sf "$BASE/api/loadtest/v1/namespaces/$NS/scripts/$SC" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" | jqget "['status']")
[ "$SCS" = "VALIDATED" ] || fail "script status = $SCS (want VALIDATED)"

say "create loadtest"
# Runner.Start cần k6 binary; không có k6 → 500 + FAILED là path hợp lệ (fail-closed).
LTCODE=$(curl -s -o /tmp/lt-resp.json -w "%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/loadtests" \
  -H "Idempotency-Key: smoke-lt" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-run\",\"script_id\":\"$SC\"}")
case "$LTCODE" in
  201) LT=$(jqget "['id']" < /tmp/lt-resp.json); say "loadtest $LT created" ;;
  500) say "loadtest 500 (k6 binary missing — FAILED path, acceptable without k6)" ;;
  *)   fail "create loadtest unexpected code $LTCODE" ;;
esac

say "readiness + events"
curl -sf "$BASE/api/loadtest/v1/namespaces/$NS/readiness" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "readiness"
if [ -n "${LT:-}" ]; then
  curl -sf "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT/events" \
    -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "events"
fi

say "delete namespace (cleanup sau khi sài xong)"
DELCODE=$(curl -s -o /tmp/smoke-del.json -w "%{http_code}" -X DELETE "$BASE/api/loadtest/v1/namespaces/$NS" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H "X-User-Role: user")
[ "$DELCODE" = "202" ] || fail "delete namespace: $DELCODE $(cat /tmp/smoke-del.json 2>/dev/null || true)"
DELSTATUS=$(jqget "['status']" < /tmp/smoke-del.json 2>/dev/null || echo "?")
[ "$DELSTATUS" = "DELETING" ] || fail "delete status = $DELSTATUS (want DELETING)"
# loadOwned ẩn namespace DELETED (404) nhưng vẫn trả DELETING; reaper (nếu có) có thể hoàn tất → chấp nhận 404.
GETCODE=$(curl -s -o /tmp/smoke-get.json -w "%{http_code}" "$BASE/api/loadtest/v1/namespaces/$NS" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID")
case "$GETCODE" in
  200)
    GSTATUS=$(jqget "['status']" < /tmp/smoke-get.json 2>/dev/null || echo "?")
    [ "$GSTATUS" = "DELETING" ] || fail "namespace status after delete = $GSTATUS (want DELETING)"
    ;;
  404) ;; # reaper đã chuyển DELETED
  *) fail "get after delete: $GETCODE" ;;
esac

echo "SMOKE PASS (namespace=$NS script=$SC loadtest=${LT:-none} deleted=$DELSTATUS)"

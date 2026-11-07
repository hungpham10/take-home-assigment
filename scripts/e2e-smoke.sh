#!/bin/sh
# E2E smoke — namespace→service→validate→apply→script→loadtest flow (POSIX sh).
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
LT=$(curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/loadtests" -H "Idempotency-Key: smoke-lt" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-run\",\"script_id\":\"$SC\"}" | jqget "['id']") || fail "create loadtest"
say "loadtest $LT created"

say "readiness + events"
curl -sf "$BASE/api/loadtest/v1/namespaces/$NS/readiness" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "readiness"
curl -sf "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT/events" \
  -H "X-Tenant-Id: $TENANT" -H "X-User-Id: $USERID" >/dev/null || fail "events"

echo "SMOKE PASS (namespace=$NS script=$SC loadtest=$LT)"

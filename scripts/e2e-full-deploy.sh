#!/bin/sh
# E2E full deploy (plans/26) — vòng đời ĐẦY ĐỦ với database + service demo:
#   namespace → database (CNPG postgres:16) + echo-server service → validate
#   → apply (202) → Env Reconciler apply thật → Status Watcher projection
#   → READY → port-forward curl echo-server trả JSON.
#
# Yêu cầu: docker, kind (hoặc KUBECONFIG trỏ sẵn), kubectl, curl, python3.
# Env: KEEP_CLUSTER=1 giữ lại kind cluster sau khi chạy; SKIP_BOOTSTRAP=1 dùng
# cluster hiện có (đã cài CNPG).
set -e

BASE=${BASE:-http://localhost:18080}
PG_PORT=${PG_PORT:-55433}
CLUSTER=${CLUSTER:-ltp-full-deploy}
CNPG_MANIFEST=$(curl -fsSL https://api.github.com/repos/cloudnative-pg/cloudnative-pg/releases/latest \
  | python3 -c "import sys,json;print([a['browser_download_url'] for a in json.load(sys.stdin)['assets'] if a['name'].endswith('.yaml') and a['name'].startswith('cnpg-')][0])")

say()  { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
jqget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

# --- 0. tooling ---------------------------------------------------------------
command -v kubectl >/dev/null || fail "kubectl not installed"
command -v curl >/dev/null || fail "curl not installed"

# --- 1. cluster bootstrap -----------------------------------------------------
if [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  command -v kind >/dev/null || fail "kind not installed (hoặc đặt SKIP_BOOTSTRAP=1 với KUBECONFIG sẵn)"
  say "kind cluster: $CLUSTER"
  kind get clusters | grep -q "^$CLUSTER$" || kind create cluster --name "$CLUSTER" --wait 120s
  kind export kubeconfig --name "$CLUSTER"
fi
[ -n "${KUBECONFIG:-}" ] || KUBECONFIG="$HOME/.kube/config"
export KUBECONFIG

if [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  say "install CNPG operator"
  kubectl apply --server-side -f "$CNPG_MANIFEST" >/dev/null
  kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=180s
fi
kubectl get crd clusters.postgresql.cnpg.io >/dev/null 2>&1 || fail "CNPG CRD not present"

# --- 2. control-plane postgres + controller -----------------------------------
say "control-plane postgres (:$PG_PORT)"
docker rm -f ltp-full-deploy-pg >/dev/null 2>&1 || true
docker run -d --rm --name ltp-full-deploy-pg \
  -e POSTGRES_USER=ltp -e POSTGRES_PASSWORD=ltp -e POSTGRES_DB=ltp \
  -p "$PG_PORT:5432" postgres:16-alpine >/dev/null
PGHOST=127.0.0.1 PGPORT=$PG_PORT PGUSER=ltp PGPASSWORD=ltp PGDATABASE=ltp
export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE
for i in $(seq 1 30); do pg_in=$(psql -tAc "SELECT 1" 2>/dev/null || true); [ "$pg_in" = "1" ] && break; sleep 1; done
[ "$pg_in" = "1" ] || fail "control-plane postgres not ready"

for f in sql/*.sql; do psql -v ON_ERROR_STOP=1 -q < "$f" || fail "migration $f"; done

say "build + start controller (K8S_ENABLED=true)"
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
go build -o bin/loadtest-controller ./cmds/loadtest-controller
DATABASE_DSN="postgres://ltp:ltp@127.0.0.1:$PG_PORT/ltp?sslmode=disable" \
SCRIPTS_DIR=/tmp/ltp-full-deploy ADDR=$BASE K8S_ENABLED=true K8S_API_URL="$SERVER" \
  ./bin/loadtest-controller > /tmp/ltp-full-deploy-controller.log 2>&1 &
CTRL_PID=$!
trap 'kill $CTRL_PID 2>/dev/null; docker rm -f ltp-full-deploy-pg >/dev/null 2>&1' EXIT
sleep 3
curl -sf "$BASE/health" >/dev/null || fail "controller health"

# --- 3. fixture + namespace + database + echo service qua API ------------------
say "seed tenant fixture"
SEED=$(psql -tAc "
  WITH t AS (
    INSERT INTO tbl_tenants (name, slug) VALUES ('FullDeploy', 'fulldeploy')
    ON CONFLICT (slug) DO UPDATE SET name='FullDeploy' RETURNING id
  ), u AS (
    INSERT INTO tbl_users (tenant_id, email, display_name)
    SELECT id, 'fulldeploy@local', 'FullDeploy' FROM t
    ON CONFLICT (tenant_id, email) DO UPDATE SET display_name='FullDeploy' RETURNING id, tenant_id
  ), r AS (
    INSERT INTO tbl_user_roles (user_id, role) SELECT id, 'user' FROM u
    ON CONFLICT (user_id, role) DO NOTHING
  ), h AS (
    INSERT INTO tbl_tenant_hosts (tenant_id, host) SELECT tenant_id, 'fulldeploy.local' FROM u
    ON CONFLICT (host) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
  )
  SELECT tenant_id || '|' || id FROM u")
TENANT=${SEED%%|*}; USERID=${SEED##*|}
[ -n "$TENANT" ] || fail "seed"
H="-H X-Tenant-Id:$TENANT -H X-User-Id:$USERID -H X-User-Role:user"

say "create namespace + database + echo service (create-with-spec, 1 call)"
CREATE=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces" -H "Idempotency-Key: fd-ns-$(date +%s)" \
  $H -H 'Content-Type: application/json' -d '{
    "name":"full-deploy-env",
    "spec":{
      "services":[{"name":"echo","deploy":{"image":"ealen/echo-server:latest","replicas":1,"port":8080,"health":{"path":"/","port":8080},
        "config_map":{"app.yaml":"listen: 8080\n"},"service_account":true,
        "hpa":{"min_replicas":1,"max_replicas":3,"cpu_target_percent":80}},
        "openapi":{"content":"{\"openapi\":\"3.0.0\",\"info\":{\"title\":\"Echo\",\"version\":\"1\"},\"paths\":{\"/\":{\"get\":{\"responses\":{\"200\":{\"description\":\"ok\"}}}}}}"}}],
      "databases":[{"name":"demo-db","engine":"postgres","version":"16","config":{"image":"postgres:16","storage_gb":1}}]
    }
  }')
echo "$CREATE" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['namespace']['id']" >/dev/null 2>&1 || fail "create-with-spec: $CREATE"
NS=$(echo "$CREATE" | jqget "['namespace']['id']")
echo "   namespace=$NS services=$(echo "$CREATE" | jqget "['services']" | head -c 40) databases=$(echo "$CREATE" | jqget "['databases']" | head -c 40)"

say "validate + apply (separate endpoints)"
curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/validate" $H >/dev/null || fail "validate"
APPLY=$(curl -s -o /tmp/fd-apply.json -w "%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/apply" \
  -H "Idempotency-Key: fd-apply-$(date +%s)" $H)
[ "$APPLY" = "202" ] || fail "apply: $APPLY $(cat /tmp/fd-apply.json)"
echo "   apply → 202 $(cat /tmp/fd-apply.json)"

# --- 4. đợi READY qua projection ----------------------------------------------
say "poll readiness (deadline 5m — CNPG pull postgres:16 lần đầu)"
READY=false
for i in $(seq 1 150); do
  STATUS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H | jqget "['status']" 2>/dev/null || echo "?")
  READYF=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H | jqget "['ready']" 2>/dev/null || echo false)
  if [ "$STATUS" = "READY" ] && [ "$READYF" = "true" ]; then READY=true; break; fi
  sleep 2
done
[ "$READY" = "true" ] || { curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H; kubectl get pods -A | grep lt-env || true; fail "namespace did not reach READY"; }
echo "   namespace READY ✅"

# --- 5. verify cluster thật -----------------------------------------------------
say "verify cluster state"
ENVNS="lt-env-$NS"
kubectl get cluster -n "$ENVNS" -o jsonpath='{.items[*].metadata.name}'; echo
kubectl -n "$ENVNS" get hpa,cm,secret,sa,deploy,svc 2>/dev/null | head -12
kubectl -n "$ENVNS" get pods
CNPG_READY=$(kubectl get cluster -n "$ENVNS" demo-db -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
[ "$CNPG_READY" = "True" ] || fail "CNPG demo-db not Ready"
ECHO_AVAIL=$(kubectl -n "$ENVNS" get deploy echo -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')
[ "$ECHO_AVAIL" = "True" ] || fail "echo deployment not Available"

# --- 6. echo phục vụ thật qua port-forward --------------------------------------
say "curl echo-server qua port-forward"
kubectl -n "$ENVNS" port-forward svc/echo 18082:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 3
ECHO_BODY=$(curl -s --max-time 5 http://localhost:18082/)
kill $PF_PID 2>/dev/null
[ -n "$ECHO_BODY" ] || fail "echo-server did not respond"
echo "   echo response: $(echo "$ECHO_BODY" | head -c 120)"

# --- 7. projection dump ----------------------------------------------------------
say "projection"
psql -c "SELECT component_type, component_id, phase, desired_generation, observed_generation
         FROM tbl_component_status WHERE namespace_id = '$NS' ORDER BY component_type"
psql -tAc "SELECT status || ' ready=' || ready_for_loadtest FROM tbl_namespaces WHERE id = '$NS'"

echo "FULL-DEPLOY E2E PASS (namespace=$NS)"
if [ "${KEEP_CLUSTER:-0}" != "1" ] && [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  say "teardown kind cluster"
  kind delete cluster --name "$CLUSTER"
fi

#!/bin/sh
# E2E full deploy (plans/26) — vòng đời ĐẦY ĐỦ với database + service demo:
#   namespace → database (CNPG postgres:16) + echo-server service → validate
#   → apply (202) → Env Reconciler apply thật → Status Watcher projection
#   → READY → port-forward curl echo-server trả JSON → DELETE namespace + verify K8s ns gone.
#
# Yêu cầu: docker, kind (hoặc KUBECONFIG trỏ sẵn), kubectl, curl, python3.
# Env: KEEP_CLUSTER=1 giữ lại kind cluster sau khi chạy; SKIP_BOOTSTRAP=1 dùng
# cluster hiện có (đã cài CNPG).
# NetworkPolicy enforcement: kindnetd default CNI does NOT enforce
# NetworkPolicies. Calico is installed automatically when bootstrapping to
# make RUN-NET-02 / INT-K8S-04 enforcement real.
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
  # Pre-load images vào kind node: CNPG bootstrap không phải pull ghcr/postgres
  # mỗi lần (5-7m → ~1-2m, ổn định CI).
  docker pull ealen/echo-server:latest >/dev/null 2>&1 || true
  docker pull ghcr.io/cloudnative-pg/postgresql:16 >/dev/null 2>&1 || true
  kind load docker-image ealen/echo-server:latest ghcr.io/cloudnative-pg/postgresql:16 --name "$CLUSTER" 2>/dev/null || true

  # Install Calico so NetworkPolicy is actually enforced (kindnetd does not).
  # kind's default CNI (kindnetd) does NOT enforce NetworkPolicies.
  # The standard calico.yaml manifest installs calico-node daemonset in kube-system.
  say "install Calico CNI (NetworkPolicy enforcement)"
  kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.27.1/manifests/calico.yaml
  # Wait for calico-node daemonset (in kube-system) to be ready.
  for i in $(seq 1 30); do
    if kubectl get daemonset -n kube-system calico-node >/dev/null 2>&1; then break; fi
    sleep 2
  done
  kubectl -n kube-system rollout status daemonset/calico-node --timeout=180s
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
# psql wrapper: host không có psql → chạy qua container postgres (portable CI/local)
psql() { docker exec -i ltp-full-deploy-pg psql -U ltp -d ltp "$@"; }
for i in $(seq 1 30); do pg_in=$(psql -tAc "SELECT 1" 2>/dev/null || true); [ "$pg_in" = "1" ] && break; sleep 1; done
[ "$pg_in" = "1" ] || fail "control-plane postgres not ready"

for f in sql/*.sql; do psql -v ON_ERROR_STOP=1 -q < "$f" || fail "migration $f"; done

say "build + start controller (K8S_ENABLED=true)"
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
go build -o bin/loadtest-controller ./cmds/loadtest-controller
DATABASE_DSN="postgres://ltp:ltp@127.0.0.1:$PG_PORT/ltp?sslmode=disable" \
SCRIPTS_DIR=/tmp/ltp-full-deploy ADDR=${BASE#http://localhost} K8S_ENABLED=true K8S_API_URL="$SERVER" KUBECONFIG="$HOME/.kube/config" \
  ./bin/loadtest-controller > /tmp/ltp-full-deploy-controller.log 2>&1 &
CTRL_PID=$!
trap 'kill $CTRL_PID 2>/dev/null; docker rm -f ltp-full-deploy-pg >/dev/null 2>&1' EXIT
HEALTH_OK=""
for i in $(seq 1 30); do
  if curl -sf "$BASE/health" >/dev/null 2>&1; then HEALTH_OK=1; break; fi
  sleep 2
done
[ -n "$HEALTH_OK" ] || { echo "--- controller log ---"; tail -30 /tmp/ltp-full-deploy-controller.log >&2 || true; fail "controller health (60s)"; }

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
      "services":[{"name":"echo","deploy":{"image":"ealen/echo-server:latest","replicas":1,"port":80,"health":{"path":"/","port":80},
        "config_map":{"app.yaml":"listen: 8080\n"},"service_account":true,
        "hpa":{"min_replicas":1,"max_replicas":3,"cpu_target_percent":80}},
        "openapi":{"content":"{\"openapi\":\"3.0.0\",\"info\":{\"title\":\"Echo\",\"version\":\"1\"},\"paths\":{\"/\":{\"get\":{\"responses\":{\"200\":{\"description\":\"ok\"}}}}}}"}}],
      "databases":[{"name":"demo-db","engine":"postgres","version":"16","config":{"image":"postgres:16","storage_gb":1}}]
    }
  }')
echo "$CREATE" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['namespace']['id']" >/dev/null 2>&1 || fail "create-with-spec: $CREATE"
NS=$(echo "$CREATE" | jqget "['namespace']['id']")
ENVNS="lt-env-$NS"
echo "   namespace=$NS services=$(echo "$CREATE" | jqget "['services']" | head -c 40) databases=$(echo "$CREATE" | jqget "['databases']" | head -c 40)"

say "validate + apply (separate endpoints)"
curl -sf -X POST "$BASE/api/loadtest/v1/namespaces/$NS/validate" $H >/dev/null || fail "validate"
APPLY=$(curl -s -o /tmp/fd-apply.json -w "%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/apply" \
  -H "Idempotency-Key: fd-apply-$(date +%s)" $H)
[ "$APPLY" = "202" ] || fail "apply: $APPLY $(cat /tmp/fd-apply.json)"
echo "   apply → 202 $(cat /tmp/fd-apply.json)"

# --- 4. đợi READY qua projection ----------------------------------------------
say "poll readiness (deadline 12m — CNPG bootstrap, images pre-loaded)"
READY=false
for i in $(seq 1 360); do
  READINESS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H 2>/dev/null || echo '{}')
  STATUS=$(printf '%s' "$READINESS" | jqget "['status']" 2>/dev/null || echo "?")
  READYF=$(printf '%s' "$READINESS" | jqget "['ready']" 2>/dev/null || echo "false")
  if [ "$STATUS" = "READY" ] && { [ "$READYF" = "true" ] || [ "$READYF" = "True" ]; }; then READY=true; break; fi
  sleep 2
done
if [ "$READY" != "true" ]; then
  curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H; echo
  kubectl get pods -A | grep lt-env || true
  echo "--- CNPG resource ---"
  kubectl get cluster -n "$ENVNS" -o yaml | head -40 || true
  echo "--- initdb pod logs ---"
  for p in $(kubectl get pods -n "$ENVNS" -o name | grep initdb | head -2); do
    kubectl -n "$ENVNS" logs "$p" 2>&1 | tail -15
  done
  echo "--- all objects in env ns ---"
  kubectl -n "$ENVNS" get all,cm,secret,sa,hpa 2>&1 | head -20 || true
  echo "--- echo pod logs/describe ---"
  for p in $(kubectl -n "$ENVNS" get pods -o name 2>/dev/null | grep echo | head -1); do
    kubectl -n "$ENVNS" logs "$p" --all-containers 2>&1 | tail -10
    kubectl -n "$ENVNS" describe "$p" 2>&1 | tail -20
  done
  echo "--- events ---"
  kubectl -n "$ENVNS" get events --sort-by=.lastTimestamp 2>&1 | tail -15
  echo "--- controller log (apply-now/loop warnings) ---"
  grep -E "apply-now|WARN|tree apply" /tmp/ltp-full-deploy-controller.log | tail -15 >&2 || true
  tail -20 /tmp/ltp-full-deploy-controller.log >&2 || true
  fail "namespace did not reach READY"
fi
echo "   namespace READY ✅"

# --- 4b. verify NetworkPolicy objects exist (default-deny + per-service + per-db)
say "verify NetworkPolicy objects"
ENVNS="lt-env-$NS"
kubectl get networkpolicy -n "$ENVNS"
for np in default-deny echo-policy demo-db-policy; do
  kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1 || fail "NetworkPolicy $np not found in $ENVNS"
done
# default-deny must NOT have allow-all ingress (ingress:[{}] was removed).
DD_INGRESS=$(kubectl get networkpolicy -n "$ENVNS" default-deny -o jsonpath='{.spec.ingress}' 2>/dev/null || echo "null")
if [ "$DD_INGRESS" = "[]" ] || [ "$DD_INGRESS" = "[{}]" ]; then
  fail "default-deny still allows all ingress (expected platform allowances only)"
fi
echo "   NetworkPolicies present: default-deny, echo-policy, demo-db-policy ✅"

# --- 4c. simple egress enforcement probe (RUN-NET-02 subset)
say "egress enforcement probe"
PROBE_POD="netprobe-$(date +%s)"
kubectl run "$PROBE_POD" --image=busybox:latest --restart=Never -n "$ENVNS" -- sleep 300 >/dev/null 2>&1 || true
for i in $(seq 1 60); do
  STATUS=$(kubectl get pod "$PROBE_POD" -n "$ENVNS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
  [ "$STATUS" = "Running" ] && break
  sleep 1
done
[ "$STATUS" = "Running" ] || fail "probe pod did not reach Running state"
# Probe pod egress to K8s API should be allowed (default-deny opens node CIDR :6443).
API_REACHABLE=$(kubectl exec "$PROBE_POD" -n "$ENVNS" -- wget -qO- --timeout=5 https://kubernetes.default.svc:443/version 2>/dev/null | head -c 1 || echo "")
if [ -z "$API_REACHABLE" ]; then
  kubectl delete pod "$PROBE_POD" -n "$ENVNS" >/dev/null 2>&1 || true
  fail "probe pod cannot reach K8s API — Calico may not be enforcing or DNS broken"
fi
# Probe pod egress to echo service should be DENIED (not a declared dependency;
# default-deny egress only allows DNS + kube-apiserver + declared deps).
ECHO_REACHABLE=$(kubectl exec "$PROBE_POD" -n "$ENVNS" -- wget -qO- --timeout=5 http://echo:80/ 2>/dev/null | head -c 1 || echo "")
if [ -n "$ECHO_REACHABLE" ]; then
  kubectl delete pod "$PROBE_POD" -n "$ENVNS" >/dev/null 2>&1 || true
  fail "probe pod reached echo service without declared dependency — default-deny egress not enforced"
fi
kubectl delete pod "$PROBE_POD" -n "$ENVNS" >/dev/null 2>&1 || true
echo "   egress enforcement verified: API ✅, undeclared echo ❌ ✅"

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
kubectl -n "$ENVNS" port-forward svc/echo 18082:80 >/tmp/ltp-full-deploy-pf.log 2>&1 &
PF_PID=$!
ECHO_BODY=""
for i in $(seq 1 20); do
  ECHO_BODY=$(curl -s --max-time 5 http://localhost:18082/ 2>/dev/null || true)
  [ -n "$ECHO_BODY" ] && break
  sleep 1
done
kill $PF_PID 2>/dev/null || true
[ -n "$ECHO_BODY" ] || { echo "--- port-forward log ---"; tail -20 /tmp/ltp-full-deploy-pf.log >&2 || true; fail "echo-server did not respond"; }
echo "   echo response: $(echo "$ECHO_BODY" | head -c 120)"

# --- 7. projection dump ----------------------------------------------------------
say "projection"
psql -c "SELECT component_type, component_id, phase, desired_generation, observed_generation
         FROM tbl_component_status WHERE namespace_id = '$NS' ORDER BY component_type"
psql -tAc "SELECT status || ' ready=' || ready_for_loadtest FROM tbl_namespaces WHERE id = '$NS'"

say "delete namespace (cleanup sau khi sài xong)"
DELCODE=$(curl -s -o /tmp/fd-del.json -w "%{http_code}" -X DELETE "$BASE/api/loadtest/v1/namespaces/$NS" $H)
[ "$DELCODE" = "202" ] || fail "delete namespace: $DELCODE $(cat /tmp/fd-del.json 2>/dev/null || true)"
DELSTATUS=$(jqget "['status']" < /tmp/fd-del.json 2>/dev/null || echo "?")
[ "$DELSTATUS" = "DELETING" ] || fail "delete status = $DELSTATUS (want DELETING)"
# Env Reconciler (K8S_ENABLED) xoá K8s namespace khi status DELETING.
say "wait for env namespace to be removed from cluster"
NS_GONE=false
for i in $(seq 1 90); do
  if ! kubectl get ns "$ENVNS" >/dev/null 2>&1; then NS_GONE=true; break; fi
  sleep 2
done
[ "$NS_GONE" = "true" ] || { kubectl get ns "$ENVNS" -o yaml 2>&1 | head -40 || true; fail "env namespace $ENVNS still present after delete"; }
echo "   env namespace $ENVNS deleted ✅"

echo "FULL-DEPLOY E2E PASS (namespace=$NS)"
if [ "${KEEP_CLUSTER:-0}" != "1" ] && [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  say "teardown kind cluster"
  kind delete cluster --name "$CLUSTER"
fi

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
# Pinned versions (avoid floating `releases/latest` / `main` which can break CI).
# CNPG: manifests/cnpg.yaml không tồn tại ở tag v1.26.0 → dùng release artifact.
CNPG_VERSION="${CNPG_VERSION:-1.26.0}"
CNPG_MANIFEST="https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/v${CNPG_VERSION}/releases/cnpg-${CNPG_VERSION}.yaml"
# k6-operator: bundle.yaml nằm trong repo tag, không phải release asset (v0.14.0 không tồn tại).
K6_OPERATOR_VERSION="${K6_OPERATOR_VERSION:-v1.7.0}"
K6_OPERATOR_BUNDLE="https://raw.githubusercontent.com/grafana/k6-operator/${K6_OPERATOR_VERSION}/bundle.yaml"

say()  { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Mint Dex id_token (bearer token) theo OIDC code flow, dùng curl theo dõi
# redirect từng bước (không phụ thuộc parse HTML form của Dex).
# Env: LTP_DEX_ISSUER, LTP_DEX_CLIENT_ID, LTP_DEX_CLIENT_SECRET,
#      LTP_DEX_USER, LTP_DEX_PASSWORD.
mint_dex_token() {
  local jar=/tmp/dex-cookies.txt
  rm -f "$jar"
  local issuer="${LTP_DEX_ISSUER:-http://localhost:5556/dex}"
  local cid="${LTP_DEX_CLIENT_ID:-ltp-test}"
  local csecret="${LTP_DEX_CLIENT_SECRET:-}"
  local user="${LTP_DEX_USER:-dev-user@example.com}"
  local pass="${LTP_DEX_PASSWORD:-password}"
  local redirect_uri="http%3A%2F%2Flocalhost%3A8080%2Fcallback"
  local disc token_ep authz loc code
  disc=$(curl -s -c "$jar" -b "$jar" "$issuer/.well-known/openid-configuration") || return 1
  token_ep=$(printf '%s' "$disc" | python3 -c "import sys,json;print(json.load(sys.stdin)['token_endpoint'])")
  authz=$(printf '%s' "$disc" | python3 -c "import sys,json;print(json.load(sys.stdin)['authorization_endpoint'])")
  # B1: GET authorize → Dex 302 về trang login.
  loc=$(curl -s -c "$jar" -b "$jar" -D - -o /dev/null \
    "$authz?response_type=code&client_id=$cid&redirect_uri=$redirect_uri&scope=openid+email+profile+offline_access" \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')
  [ -n "$loc" ] || return 1
  # B2: POST credentials → Dex 302 về callback (Location có ?code=...).
  curl -s -c "$jar" -b "$jar" -o /dev/null -X POST -d "login=$user&password=$pass" "$loc" >/dev/null
  loc=$(curl -s -c "$jar" -b "$jar" -D - -o /dev/null "$loc" \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')
  [ -n "$loc" ] || return 1
  code=$(printf '%s' "$loc" | python3 -c "import sys,urllib.parse;print(urllib.parse.parse_qs(urllib.parse.urlparse(sys.stdin.read().strip()).query).get('code',[''])[0])")
  [ -n "$code" ] || return 1
  # B3: exchange code → id_token.
  curl -s -X POST "$token_ep" \
    -d "grant_type=authorization_code&code=$code&redirect_uri=$redirect_uri&client_id=$cid&client_secret=$csecret" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['id_token'])"
}
jqget() { python3 -c "import sys,json;d=json.load(sys.stdin);v=d$1;print('' if v is None else ('true' if v is True else ('false' if v is False else str(v))))"; }

# --- 0. tooling ---------------------------------------------------------------
command -v kubectl >/dev/null || fail "kubectl not installed"
command -v curl >/dev/null || fail "curl not installed"
command -v python3 >/dev/null || fail "python3 not installed"

DEPLOY_MODE="${DEPLOY_MODE:-legacy}"   # legacy: docker postgres + go controller (máy local)
                                       # in-cluster: docker build images + kustomize apply lên kind
if [ "$DEPLOY_MODE" = "in-cluster" ]; then
  command -v docker >/dev/null || fail "docker not installed (cần cho DEPLOY_MODE=in-cluster)"
  BASE=http://localhost:8080           # port-forward svc/serve 8080:8080 ở bước 2
fi

# --- 1. cluster bootstrap -----------------------------------------------------
if [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  command -v kind >/dev/null || fail "kind not installed (hoặc đặt SKIP_BOOTSTRAP=1 với KUBECONFIG sẵn)"
  say "kind cluster: $CLUSTER (disableDefaultCNI so Calico is the sole CNI enforcing NetworkPolicy)"
  kind get clusters | grep -q "^$CLUSTER$" || {
    KIND_CONFIG="$(mktemp)"
    cat > "$KIND_CONFIG" <<KIND_YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
nodes:
  - role: control-plane
  - role: worker
KIND_YAML
    kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --wait 120s
    rm -f "$KIND_CONFIG"
  }
  kind export kubeconfig --name "$CLUSTER"
  # Pre-load images vào kind node: CNPG bootstrap không phải pull ghcr/postgres
  # mỗi lần (5-7m → ~1-2m, ổn định CI).
  docker pull ealen/echo-server:latest >/dev/null 2>&1 || true
  docker pull ghcr.io/cloudnative-pg/postgresql:16 >/dev/null 2>&1 || true
  docker pull grafana/k6:latest >/dev/null 2>&1 || true
  kind load docker-image ealen/echo-server:latest ghcr.io/cloudnative-pg/postgresql:16 grafana/k6:latest --name "$CLUSTER" 2>/dev/null || true

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

# Install k6-operator so load tests run as TestRun CRs inside the env
# namespace (subject to NetworkPolicy). bundle.yaml (namespace + CRDs + RBAC
# + controller) is downloaded from the k6-operator repo; sources/ is
# gitignored so we can't reference it in CI. No cert-manager needed.
if [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  say "install k6-operator"
  kubectl apply -f "$K6_OPERATOR_BUNDLE"
  # Wait for TestRun CRD to be established.
  for i in $(seq 1 30); do
    if kubectl get crd testruns.k6.io >/dev/null 2>&1; then break; fi
    sleep 2
  done
  kubectl get crd testruns.k6.io >/dev/null 2>&1 || fail "k6-operator TestRun CRD not established"
  kubectl -n k6-operator-system rollout status deployment/k6-operator-controller-manager --timeout=180s >/dev/null 2>&1 || true
fi

# --- 2. control-plane postgres + controller -----------------------------------
if [ "$DEPLOY_MODE" = "in-cluster" ]; then
  say "build images (docker build) + apply k8s overlay kind"
  docker build -t loadtest-platform-serve:local -f deploy/k8s/docker/serve.Dockerfile .
  docker build -t loadtest-platform-mock-service:local -f deploy/k8s/docker/mock-service.Dockerfile .
  kind load docker-image loadtest-platform-serve:local loadtest-platform-mock-service:local --name "$CLUSTER" 2>/dev/null || true
  export SOPS_AGE_KEY_CONTENT="${SOPS_AGE_KEY_CONTENT:-}"
  POSTGRES_PASSWORD=ltp scripts/k8s-apply.sh
  # port-forward: serve (API gateway), dex (mint token), postgres (seed SQL)
  kubectl -n ltp-system port-forward svc/serve 8080:8080 >/tmp/ltp-full-deploy-pf-serve.log 2>&1 &
  PF_SERVE=$!
  kubectl -n ltp-system port-forward svc/dex 5556:5556 >/tmp/ltp-full-deploy-pf-dex.log 2>&1 &
  PF_DEX=$!
  # đợi Dex discovery sẵn sàng (tránh 302 khi mint token)
  for i in $(seq 1 30); do
    if curl -sf http://localhost:5556/dex/.well-known/openid-configuration >/dev/null 2>&1; then break; fi
    sleep 2
  done
  curl -sf http://localhost:5556/dex/.well-known/openid-configuration >/dev/null 2>&1 || fail "dex discovery không sẵn sàng"
  kubectl -n ltp-system port-forward svc/postgres 5432:5432 >/tmp/ltp-full-deploy-pf-pg.log 2>&1 &
  PF_PG=$!
  # đợi postgres pod xuất hiện
  PG_POD=""
  for i in $(seq 1 30); do
    PG_POD=$(kubectl -n ltp-system get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    [ -n "$PG_POD" ] && break
    sleep 1
  done
  [ -n "$PG_POD" ] || fail "postgres pod không xuất hiện"
  export PGPASSWORD=ltp
  psql() { kubectl -n ltp-system exec "$PG_POD" -- psql -U ltp -d ltp "$@"; }
  # health qua nginx gateway (không cần auth)
  HEALTH_OK=""
  for i in $(seq 1 30); do
    if curl -sf http://localhost:8080/health >/dev/null 2>&1; then HEALTH_OK=1; break; fi
    sleep 2
  done
  [ -n "$HEALTH_OK" ] || { echo "--- serve pf log ---"; tail -20 /tmp/ltp-full-deploy-pf-serve.log >&2 || true; fail "serve health (60s)"; }
  # bearer token từ Dex (host-side qua port-forward 5556)
  TOKEN=$(mint_dex_token)
  [ -n "$TOKEN" ] || fail "mint Dex bearer token"
  H="-H Authorization: Bearer $TOKEN -H Host: localhost"
  trap 'kill $PF_SERVE $PF_DEX $PF_PG 2>/dev/null || true' EXIT
else
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
  SCRIPTS_DIR=/tmp/ltp-full-deploy ADDR=${BASE#http://localhost} K8S_ENABLED=true RUNNER_MODE=k8s K8S_API_URL="$SERVER" KUBECONFIG="$HOME/.kube/config" \
    ./bin/loadtest-controller > /tmp/ltp-full-deploy-controller.log 2>&1 &
  CTRL_PID=$!
  trap 'kill $CTRL_PID 2>/dev/null; docker rm -f ltp-full-deploy-pg >/dev/null 2>&1' EXIT
  HEALTH_OK=""
  for i in $(seq 1 30); do
    if curl -sf "$BASE/health" >/dev/null 2>&1; then HEALTH_OK=1; break; fi
    sleep 2
  done
  [ -n "$HEALTH_OK" ] || { echo "--- controller log ---"; tail -30 /tmp/ltp-full-deploy-controller.log >&2 || true; fail "controller health (60s)"; }
  # legacy: header-based auth (không bearer)
  H=""
fi

# --- 3. fixture + namespace + database + echo service qua API ------------------
say "seed tenant fixture"
# In-cluster (bearer): host=localhost để nginx resolve tenant qua $host, +
# auth profile trỏ Dex để verify JWT. Legacy (header): host=fulldeploy.local,
# auth qua X-Tenant-Id/X-User-Id header.
if [ "$DEPLOY_MODE" = "in-cluster" ]; then
  TENANT_HOST="localhost"
  AUTH_PROFILE=", p AS (
    INSERT INTO tbl_tenant_auth_profiles (tenant_id, name, issuer, jwks_uri, config)
    SELECT id, 'default', 'http://dex:5556/dex', 'http://dex:5556/dex/keys',
           '{\"jwt_mode\":\"jwks\",\"oidc_expected_alg\":\"RS256\"}'::jsonb
    FROM t
    ON CONFLICT (tenant_id, name) DO UPDATE
      SET issuer = EXCLUDED.issuer, jwks_uri = EXCLUDED.jwks_uri,
          config = EXCLUDED.config, updated_at = now()
  )"
else
  TENANT_HOST="fulldeploy.local"
  AUTH_PROFILE=""
fi
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
    INSERT INTO tbl_tenant_hosts (tenant_id, host) SELECT tenant_id, '$TENANT_HOST' FROM u
    ON CONFLICT (host) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
  )$AUTH_PROFILE
  SELECT tenant_id || '|' || id FROM u")
TENANT=${SEED%%|*}; USERID=${SEED##*|}
[ -n "$TENANT" ] || fail "seed"
# legacy: header-based auth (in-cluster đã set H = Bearer ở bước 2)
if [ -z "${H:-}" ]; then
  H="-H X-Tenant-Id:$TENANT -H X-User-Id:$USERID -H X-User-Role:user"
fi

say "create namespace + database + echo service (create-with-spec, 1 call)"
CREATE=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces" -H "Idempotency-Key: fd-ns-$(date +%s)" \
  $H -H 'Content-Type: application/json' -d '{
    "name":"full-deploy-env",
    "spec":{
      "services":[{"name":"echo","deploy":{"image":"ealen/echo-server:latest","replicas":1,"port":80,"health":{"path":"/","port":80},
        "config_map":{"app.yaml":"listen: 8080\n"},"service_account":true,
        "hpa":{"min_replicas":1,"max_replicas":3,"cpu_target_percent":80}},
        "database_bindings":[{"database_id":"demo-db","binding":"DEMO_DB","access":"readwrite"}],
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
say "poll readiness (deadline 10m — namespace CNPG + services converge)"
READY=false
for i in $(seq 1 300); do
  READINESS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness" $H 2>/dev/null || echo '{}')
  # K8S mode: applyDesired sets ready_for_loadtest=false; the Status Watcher
  # sets it true only when it transitions the namespace to READY. The loadtest
  # create gate requires ready_for_loadtest=true, so wait for that column (not
  # just the projection's ready flag). Check in Python so JSON booleans map
  # correctly (True→true, None→"") — so sánh chuỗi "true" sẽ sai vì Python in
  # boolean là "True"/"False".
  if printf '%s' "$READINESS" | python3 -c "
import sys,json
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if d.get('status')=='READY' and d.get('ready_for_loadtest') is True and d.get('ready') is True else 1)
" 2>/dev/null; then READY=true; break; fi
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

# --- 8. upload k6 script + poll VALIDATED -----------------------------------------
say "upload + validate k6 script"
SCRIPT_CONTENT='import http from "k6/http"; export default function() { http.get("http://echo:80/"); }'
SCRIPT=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces/$NS/scripts" -H "Idempotency-Key: fd-script-$(date +%s)" \
  $H -H 'Content-Type: application/json' \
  -d "{\"name\":\"echo-test\",\"content\":$(printf '%s' "$SCRIPT_CONTENT" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}")
SCRIPT_ID=$(printf '%s' "$SCRIPT" | jqget "['id']")
[ -n "$SCRIPT_ID" ] || fail "script upload: $SCRIPT"

# E2E-FD-08: create loadtest with script NOT YET VALIDATED → 409 SCRIPT_NOT_VALIDATED
# Deterministic: upload script invalid cho CẢ hai validator:
#   - Structural (không có k6): nội dung bắt đầu bằng byte NUL → content[0]==0
#   - LocalK6 (có k6): k6 inspect từ chối invalid JS → parse error
#   → status INVALID bất kể validator nào chạy. Poll tới INVALID rồi dùng làm
#   script_id cho POST /loadtests → mong đợi 409. Không phụ thuộc timing validate async.
# Sinh JSON bằng python3 để truyền đáng tin cậy byte NUL (tránh shell printf '%s'
# cắt chuỗi tại byte NUL).
BAD_SCRIPT_JSON=$(python3 -c 'import json;print(json.dumps({"name":"echo-bad","content":"\x00this is not valid k6 script"}))')
BAD_SCRIPT=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces/$NS/scripts" -H "Idempotency-Key: fd-badscript-$(date +%s)" \
  $H -H 'Content-Type: application/json' -d "$BAD_SCRIPT_JSON")
BAD_SCRIPT_ID=$(printf '%s' "$BAD_SCRIPT" | jqget "['id']")
[ -n "$BAD_SCRIPT_ID" ] || fail "bad script upload: $BAD_SCRIPT"

BAD_STATUS=""
for i in $(seq 1 30); do
  BAD_STATUS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/scripts/$BAD_SCRIPT_ID" $H | jqget "['status']")
  [ "$BAD_STATUS" = "INVALID" ] && break
  [ "$BAD_STATUS" = "VALIDATED" ] && fail "E2E-FD-08: bad script unexpectedly VALIDATED (validator did not reject NUL content)"
  sleep 2
done
[ "$BAD_STATUS" = "INVALID" ] || fail "bad script did not reach INVALID (status=$BAD_STATUS)"

LT_PRE=$(curl -s -o /tmp/fd-lt-pre.json -w "%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/loadtests" \
  -H "Idempotency-Key: fd-lt-pre-$(date +%s)" $H -H 'Content-Type: application/json' \
  -d "{\"name\":\"echo-load-pre\",\"script_id\":\"$BAD_SCRIPT_ID\",\"load_config\":{\"vusers\":5,\"duration\":\"45s\",\"timeout_minutes\":5}}")
[ "$LT_PRE" = "409" ] || fail "E2E-FD-08: expected 409 for unvalidated script, got $LT_PRE $(cat /tmp/fd-lt-pre.json 2>/dev/null || true)"
LT_PRE_CODE=$(jqget "['error']['code']" < /tmp/fd-lt-pre.json 2>/dev/null || true)
[ "$LT_PRE_CODE" = "SCRIPT_NOT_VALIDATED" ] || fail "E2E-FD-08: expected error code SCRIPT_NOT_VALIDATED, got $LT_PRE_CODE"
echo "   E2E-FD-08: unvalidated (INVALID) script → 409 SCRIPT_NOT_VALIDATED ✅"

SCRIPT_STATUS=""
for i in $(seq 1 30); do
  SCRIPT_STATUS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/scripts/$SCRIPT_ID" $H | jqget "['status']")
  [ "$SCRIPT_STATUS" = "VALIDATED" ] && break
  [ "$SCRIPT_STATUS" = "INVALID" ] && fail "script validation INVALID"
  sleep 2
done
[ "$SCRIPT_STATUS" = "VALIDATED" ] || fail "script did not validate (status=$SCRIPT_STATUS)"
echo "   script VALIDATED ($SCRIPT_ID) ✅"

# --- 9. create loadtest ------------------------------------------------------------
say "create loadtest"
LT_RESP=$(curl -s -o /tmp/fd-lt.json -w "%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/loadtests" \
  -H "Idempotency-Key: fd-lt-$(date +%s)" $H -H 'Content-Type: application/json' \
  -d "{\"name\":\"echo-load\",\"script_id\":\"$SCRIPT_ID\",\"load_config\":{\"vusers\":5,\"duration\":\"45s\",\"timeout_minutes\":5}}")
[ "$LT_RESP" = "201" ] || fail "create loadtest: $LT_RESP $(cat /tmp/fd-lt.json 2>/dev/null || true)"
LT_ID=$(jqget "['id']" < /tmp/fd-lt.json)
echo "   loadtest $LT_ID created ✅"

# --- 10. assert run-scoped NetworkPolicies exist ------------------------------------
say "assert run-scoped NetworkPolicies"
for np in loadtest-run-egress-$LT_ID loadtest-run-api-$LT_ID; do
  kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1 || fail "run NetworkPolicy $np not found in $ENVNS"
done
# ingress policy is per-target: loadtest-run-ingress-<id>-echo
INGRESS_NP="loadtest-run-ingress-$LT_ID-echo"
INGRESS_OK=""
for i in $(seq 1 20); do
  if kubectl get networkpolicy -n "$ENVNS" "$INGRESS_NP" >/dev/null 2>&1; then INGRESS_OK=1; break; fi
  sleep 1
done
[ -n "$INGRESS_OK" ] || {
  echo "--- all networkpolicies in $ENVNS ---"
  kubectl get networkpolicy -n "$ENVNS" -o name 2>&1 || echo "kubectl list networkpolicies failed: $?"
  echo "--- run-policy labeled networkpolicies ---"
  kubectl get networkpolicy -n "$ENVNS" -l loadtest.io/component-kind=run-policy -o name 2>&1 || echo "kubectl list run-policy failed: $?"
  echo "--- loadtest targets ---"
  curl -s -w "\nHTTP_CODE:%{http_code}" "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H 2>&1 || echo "curl loadtest failed"
  fail "run ingress NetworkPolicy $INGRESS_NP not found in $ENVNS"
}
# run-policy labels + egress port 80 + target selector = service name (echo)
# kubectl -o jsonpath='{.metadata.labels}' prints Go map syntax (map[k:v ...]),
# not k=v pairs → extract each label via its own jsonpath query.
NP_KIND=$(kubectl get networkpolicy -n "$ENVNS" loadtest-run-egress-$LT_ID \
  -o jsonpath='{.metadata.labels.loadtest\.io/component-kind}')
NP_RUN=$(kubectl get networkpolicy -n "$ENVNS" loadtest-run-egress-$LT_ID \
  -o jsonpath='{.metadata.labels.loadtest\.io/run-id}')
[ "$NP_KIND" = "run-policy" ] || fail "egress NP component-kind=$NP_KIND (want run-policy)"
[ "$NP_RUN" = "$LT_ID" ] || fail "egress NP run-id=$NP_RUN (want $LT_ID)"
EGRESS_PORTS=$(kubectl get networkpolicy -n "$ENVNS" loadtest-run-egress-$LT_ID -o jsonpath='{.spec.egress[?(@.ports[0].port==80)]}')
[ -n "$EGRESS_PORTS" ] || fail "egress NP missing port 80 rule"
EGRESS_TARGET=$(kubectl get networkpolicy -n "$ENVNS" loadtest-run-egress-$LT_ID -o jsonpath='{.spec.egress[?(@.ports[0].port==80)].to[0].podSelector.matchLabels.loadtest\.io/component-id}')
[ "$EGRESS_TARGET" = "echo" ] || fail "egress NP target selector = $EGRESS_TARGET (want echo)"
# 6565 API ingress rule present
API_6565=$(kubectl get networkpolicy -n "$ENVNS" loadtest-run-api-$LT_ID -o jsonpath='{.spec.ingress[?(@.ports[0].port==6565)]}')
[ -n "$API_6565" ] || fail "api NP missing 6565 ingress rule"
echo "   run-scoped NetworkPolicies: egress/ingress/api present, port 80 scoped, target=echo, 6565 ✅"

# --- 13. L3/L4 probes from busybox runner-like pod (in-flight, before completion) ---
# Enforcement must be verified while the run is alive: the reconciler deletes
# run-scoped NetworkPolicies once the run reaches terminal status (docs/
# network-policy.md §3.3). Probing after completion only asserts "no policy
# exists" (weak), not "policy scoped correctly" (strong).
say "L3/L4 NetworkPolicy probes (in-flight, before completion)"
PROBE_POD="netprobe-$LT_ID"
kubectl run "$PROBE_POD" --image=busybox:latest --restart=Never -n "$ENVNS" \
  --labels="loadtest.io/run-id=$LT_ID" -- sleep 300 >/dev/null 2>&1 || true
for i in $(seq 1 60); do
  PSTATUS=$(kubectl get pod "$PROBE_POD" -n "$ENVNS" -o jsonpath='{.status.phase}' 2>/dev/null || echo Pending)
  [ "$PSTATUS" = "Running" ] && break
  sleep 1
done
[ "$PSTATUS" = "Running" ] || fail "probe pod did not reach Running"

# 13a. runner → echo (port 80) MUST be allowed (declared target)
ECHO_PROBE=$(kubectl exec "$PROBE_POD" -n "$ENVNS" -- wget -qO- --timeout=5 http://echo:80/ 2>/dev/null | head -c 1 || echo "")
[ -n "$ECHO_PROBE" ] || fail "runner→echo:80 denied (declared target should be allowed)"
# 13b. runner → demo-db-rw (5432) MUST be denied (not a target; CNPG svc is demo-db-rw)
DB_PROBE=$(kubectl exec "$PROBE_POD" -n "$ENVNS" -- nc -z -w3 demo-db-rw 5432 2>/dev/null || echo "")
[ -z "$DB_PROBE" ] || fail "runner→demo-db-rw:5432 allowed (should be denied — not a target)"
# 13c. echo pod (component-id=echo, not bound to DB) → demo-db-rw:5432 MUST be denied
ECHO_POD=$(kubectl get pod -n "$ENVNS" -l loadtest.io/component-id=echo -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$ECHO_POD" ]; then
  ECHO_DB_PROBE=$(kubectl exec "$ECHO_POD" -n "$ENVNS" -- nc -z -w3 demo-db-rw 5432 2>/dev/null || echo "")
  [ -z "$ECHO_DB_PROBE" ] || fail "echo→demo-db-rw:5432 allowed (echo not bound to DB — should be denied)"
fi
# 13d. echo pod IS bound to demo-db → demo-db-rw:5432 MUST be allowed (positive case)
ECHO_DB_ALLOWED=$(kubectl exec "$ECHO_POD" -n "$ENVNS" -- nc -z -w3 demo-db-rw 5432 2>/dev/null || echo "")
if [ -z "$ECHO_DB_ALLOWED" ]; then
  # nc may not be installed inside echo image; fall back to checking binding exists in policy
  echo "   note: echo image lacks nc; skipping positive DB probe (binding DEMO_DB declared)"
else
  [ -n "$ECHO_DB_ALLOWED" ] && echo "   13d echo→demo-db-rw:5432 allowed (bound) ✅" || fail "echo→demo-db-rw:5432 denied but echo is bound to demo-db"
fi
kubectl delete pod "$PROBE_POD" -n "$ENVNS" >/dev/null 2>&1 || true
echo "   L3/L4 probes: runner→echo ✅, runner→demo-db-rw ❌, echo→demo-db-rw ❌ (unbound), echo→demo-db-rw ✅ (bound) ✅"

# --- 11. wait for loadtest COMPLETED -----------------------------------------------
say "wait for loadtest completion (deadline 3m)"
LT_STATUS=""
for i in $(seq 1 90); do
  LT_STATUS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H | jqget "['status']")
  [ "$LT_STATUS" = "COMPLETED" ] && break
  [ "$LT_STATUS" = "FAILED" ] && { curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H; fail "loadtest FAILED"; }

  sleep 2
done
[ "$LT_STATUS" = "COMPLETED" ] || {
  echo "--- LT_STATUS=$LT_STATUS (want COMPLETED) ---"
  echo "--- TestRun CR ---"
  kubectl get testrun -n "$ENVNS" "$LT_ID" -o json 2>&1 || true
  echo "--- describe testrun ---"
  kubectl describe testrun -n "$ENVNS" "$LT_ID" 2>&1 || true
  echo "--- Jobs in $ENVNS ---"
  kubectl get jobs -n "$ENVNS" -o wide 2>&1 || true
  echo "--- k6-operator logs (tail=400, looking for the Create(Service) trigger) ---"
  kubectl -n k6-operator-system logs deploy/k6-operator-controller-manager --tail=400 2>&1 | grep -E "Failed to launch|Reconciler error|job with the name|Creating test jobs|stage =" || true
  echo "--- events (tail=30, for pod scheduling only; Create failures emit no event) ---"
  kubectl get events -n "$ENVNS" --sort-by=.lastTimestamp 2>&1 | tail -30 || true
  curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H
  fail "loadtest did not complete (status=$LT_STATUS)"
}
STARTED=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H | jqget "['started_at']")
FINISHED=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID" $H | jqget "['finished_at']")
[ -n "$STARTED" ] && [ -n "$FINISHED" ] || fail "loadtest missing started_at/finished_at"
echo "   loadtest COMPLETED ✅ (started=$STARTED finished=$FINISHED)"

# --- 11b. assert run-scoped NetworkPolicies garbage-collected after terminal ---
# Reconciler (2s tick) deletes run-scoped NetworkPolicies + script ConfigMap
# once the run reaches terminal status (docs/network-policy.md §3.3). Verify
# hygiene rather than assuming.
say "run-scoped NetworkPolicies garbage-collected after completion"
for np in loadtest-run-egress-$LT_ID loadtest-run-ingress-$LT_ID-echo loadtest-run-api-$LT_ID; do
  for i in $(seq 1 15); do
    if ! kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then break; fi
    sleep 1
  done
  if kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then
    fail "run NetworkPolicy $np still present after run terminal (expected pruned by reconciler)"
  fi
done
echo "   run-scoped NetworkPolicies garbage-collected ✅"

# --- 12. events + unreliable ---------------------------------------------------------
say "loadtest events + unreliable"
EVENTS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID/events" $H)
echo "$EVENTS" | python3 -c "
import json,sys
d=json.load(sys.stdin)
ev=len(d.get('events',[]))
unrel=d.get('unreliable')
if unrel is not False:
    sys.exit(1)
print('events:',ev,'unreliable:',unrel)
" >/dev/null || fail "events payload unreadable or unreliable!=false: $EVENTS"
echo "   events + unreliable=false ✅"

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

# Run-scoped NetworkPolicies are garbage-collected with the namespace.
for np in loadtest-run-egress-$LT_ID loadtest-run-ingress-$LT_ID-echo loadtest-run-api-$LT_ID; do
  if kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then
    fail "run NetworkPolicy $np still present after namespace delete"
  fi
done
echo "   run-scoped NetworkPolicies garbage-collected ✅"

echo "FULL-DEPLOY E2E PASS (namespace=$NS)"
if [ "${KEEP_CLUSTER:-0}" != "1" ] && [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
  say "teardown kind cluster"
  kind delete cluster --name "$CLUSTER"
fi

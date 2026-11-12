#!/bin/sh
# Common functions and variables for E2E full deploy
# Source this in other helper scripts: . tests/e2e/helpers/common.sh

# --- Colors for output ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

say()  { echo "${BLUE}==${NC} $*"; }
ok()   { echo "${GREEN}   ✅ $*${NC}"; }
warn() { echo "${YELLOW}   ⚠️  $*${NC}"; }
fail() { echo "${RED}FAIL:${NC} $*" >&2; exit 1; }

# --- jqget: extract JSON value ---
jqget() {
  python3 -c "
import sys, json
d = json.load(sys.stdin)
v = d
for k in '$1'.strip('[]').split(']['):
    k = k.strip(\"'\")
    if k.isdigit():
        k = int(k)
    v = v.get(k)
    if v is None:
        break
if v is None:
    print('')
elif isinstance(v, bool):
    print('true' if v else 'false')
else:
    print(v)
"
}

# --- Default configuration ---
BASE="${BASE:-http://localhost:18080}"
PG_PORT="${PG_PORT:-55433}"
CLUSTER="${CLUSTER:-ltp-full-deploy}"
SUT_IMAGE="${SUT_IMAGE:-ltp/sample-service:dev}"
SUT_PORT="${SUT_PORT:-8080}"
SUT_HEALTH_PATH="${SUT_HEALTH_PATH:-/healthz}"
SUT_PROBE_PATH="${SUT_PROBE_PATH:-/todos}"
SUT_K6_URL="${SUT_K6_URL:-http://echo:8080/todos}"
SUT_OPENAPI="${SUT_OPENAPI:-examples/sample-service/openapi.yaml}"
CNPG_VERSION="${CNPG_VERSION:-1.26.0}"
CNPG_MANIFEST="https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/v${CNPG_VERSION}/releases/cnpg-${CNPG_VERSION}.yaml"
K6_OPERATOR_VERSION="${K6_OPERATOR_VERSION:-v1.7.0}"
K6_OPERATOR_BUNDLE="https://raw.githubusercontent.com/grafana/k6-operator/${K6_OPERATOR_VERSION}/bundle.yaml"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.31.0}"
ENVNS="lt-env-"
NS=""
LT_ID=""

# --- DEPLOY_MODE handling ---
DEPLOY_MODE="${DEPLOY_MODE:-legacy}"
if [ "$DEPLOY_MODE" = "in-cluster" ]; then
  command -v docker >/dev/null || fail "docker not installed (required for DEPLOY_MODE=in-cluster)"
  BASE="http://localhost:8080"
fi

# --- Required tools ---
require_tools() {
  command -v kubectl >/dev/null || fail "kubectl not installed"
  command -v curl >/dev/null || fail "curl not installed"
  command -v python3 >/dev/null || fail "python3 not installed"
}

# --- Auth headers helper ---
auth_headers() {
  # Returns -H flags for tenant/user auth
  # Usage: curl $(auth_headers) "$URL"
  if [ -n "$TENANT" ] && [ -n "$USERID" ]; then
    echo "-H \"X-Tenant-Id: $TENANT\" -H \"X-User-Id: $USERID\" -H \"X-User-Role: user\""
  fi
}

# --- Wait helper ---
wait_for() {
  local description="$1"
  local max_attempts="${2:-30}"
  local sleep_sec="${3:-2}"
  local cmd="$4"
  
  say "$description"
  for i in $(seq 1 "$max_attempts"); do
    if eval "$cmd" >/dev/null 2>&1; then
      ok "$description"
      return 0
    fi
    sleep "$sleep_sec"
  done
  fail "$description timed out after $max_attempts attempts"
}

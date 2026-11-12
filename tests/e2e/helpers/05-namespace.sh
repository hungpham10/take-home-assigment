#!/bin/sh
# 05-namespace.sh — Namespace CRUD operations
# Usage: . tests/e2e/helpers/common.sh && create_namespace_spec

create_namespace_spec() {
  require_tools
  
  say "Creating namespace spec (database + service)"
  
  # Build the namespace spec JSON
  cat > /tmp/ns-spec.json <<SPEC
{
  "name": "e2e-full-deploy",
  "slug": "e2e-full-deploy-$(date +%s)",
  "description": "E2E full deploy test namespace",
  "databases": [
    {
      "name": "test-db",
      "operator": "cloudnative-pg",
      "engine": "postgresql",
      "engine_version": "16",
      "storage_gb": 1,
      "instance_size": "dev",
      "high_availability": false
    }
  ],
  "services": [
    {
      "name": "sample-service",
      "port": 8080,
      "openapi": $(python3 -c "
import yaml, json, sys
with open('$SUT_OPENAPI') as f:
    spec = yaml.safe_load(f)
print(json.dumps(spec))
")
    }
  ],
  "ingress": [],
  "dependencies": []
}
SPEC
  
  say "POST /api/loadtest/v1/namespaces (create with spec)"
  NS_RESPONSE=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: e2e-ns-create-$(date +%s)" \
    -d @/tmp/ns-spec.json)
  
  NS=$(echo "$NS_RESPONSE" | jqget "['namespace']['id']")
  [ -n "$NS" ] || fail "Failed to create namespace: $NS_RESPONSE"
  
  ENVNS="lt-env-$NS"
  ok "Namespace created: $NS (env: $ENVNS)"
}

validate_namespace() {
  say "Validating namespace: $NS"
  curl -s -X POST "$BASE/api/loadtest/v1/namespaces/$NS/validate" \
    -H "Idempotency-Key: e2e-ns-validate-$(date +%s)" >/dev/null
  ok "Namespace validated"
}

apply_namespace() {
  say "Applying namespace: $NS (async, returns 202)"
  APPLY_RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "$BASE/api/loadtest/v1/namespaces/$NS/apply" \
    -H "Idempotency-Key: e2e-ns-apply-$(date +%s)")
  
  HTTP_CODE=$(echo "$APPLY_RESPONSE" | tail -1)
  [ "$HTTP_CODE" = "202" ] || fail "Apply failed: $APPLY_RESPONSE"
  
  ok "Namespace apply accepted (202)"
}

wait_namespace_ready() {
  say "Waiting for namespace to become READY"
  
  for i in $(seq 1 60); do
    READY_RESPONSE=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/readiness")
    READY=$(echo "$READY_RESPONSE" | jqget "['ready_for_loadtest']")
    STATUS=$(echo "$READY_RESPONSE" | jqget "['status']")
    
    if [ "$READY" = "true" ]; then
      ok "Namespace READY (status: $STATUS)"
      return 0
    fi
    
    # Show component status for debugging
    if [ $((i % 10)) -eq 0 ]; then
      say "Still waiting... (status: $STATUS)"
      echo "$READY_RESPONSE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for comp in d.get('components', []):
    print('  {}: {} (desired={}, observed={})'.format(
        comp.get('component_type'), comp.get('phase'), 
        comp.get('desired_generation'), comp.get('observed_generation')))
" 2>/dev/null || true
    fi
    sleep 5
  done
  
  fail "Namespace never became READY"
}

#!/bin/sh
# 09-cleanup.sh — Delete namespace and verify K8s cleanup
# Usage: . tests/e2e/helpers/common.sh && delete_namespace && verify_k8s_cleanup

delete_namespace() {
  require_tools
  
  say "Deleting namespace: $NS"
  
  DEL_RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/api/loadtest/v1/namespaces/$NS" \
    -H "Idempotency-Key: e2e-ns-delete-$(date +%s)")
  
  DEL_CODE=$(echo "$DEL_RESPONSE" | tail -1)
  [ "$DEL_CODE" = "202" ] || fail "Delete namespace failed: $DEL_RESPONSE"
  
  DEL_STATUS=$(echo "$DEL_RESPONSE" | head -1 | jqget "['status']")
  [ "$DEL_STATUS" = "DELETING" ] || fail "Delete status = $DEL_STATUS (want DELETING)"
  
  ok "Namespace delete accepted (202 DELETING)"
}

verify_k8s_cleanup() {
  say "Waiting for K8s namespace to be removed from cluster"
  
  for i in $(seq 1 90); do
    if ! kubectl get ns "$ENVNS" >/dev/null 2>&1; then
      ok "K8s namespace $ENVNS deleted"
      break
    fi
    sleep 2
  done
  
  if kubectl get ns "$ENVNS" >/dev/null 2>&1; then
    kubectl get ns "$ENVNS" -o yaml 2>&1 | head -40 || true
    fail "K8s namespace $ENVNS still present after delete"
  fi
  
  # Verify run-scoped NetworkPolicies also gone
  for np in loadtest-run-egress-$LT_ID loadtest-run-ingress-$LT_ID-echo loadtest-run-api-$LT_ID; do
    if kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then
      fail "Run NetworkPolicy $np still present after namespace delete"
    fi
  done
  
  ok "Run-scoped NetworkPolicies garbage-collected with namespace"
}

teardown_cluster() {
  if [ "${KEEP_CLUSTER:-0}" != "1" ] && [ "${SKIP_BOOTSTRAP:-0}" != "1" ]; then
    say "Tearing down kind cluster"
    kind delete cluster --name "$CLUSTER"
    ok "Cluster torn down"
  else
    warn "Cluster preserved (KEEP_CLUSTER=1 or SKIP_BOOTSTRAP=1)"
  fi
}

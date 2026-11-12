#!/bin/sh
# 08-verify.sh — Verify results, events, NetworkPolicy cleanup
# Usage: . tests/e2e/helpers/common.sh && verify_results && verify_cleanup

verify_results() {
  require_tools
  
  say "Verifying results collected (result_collected_at, result_summary)"
  
  for i in $(seq 1 30); do
    LT_RESPONSE=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID")
    RESULT_COLLECTED=$(echo "$LT_RESPONSE" | jqget "['result_collected_at']")
    
    if [ -n "$RESULT_COLLECTED" ] && [ "$RESULT_COLLECTED" != "null" ]; then
      # Parse the result summary
      echo "$LT_RESPONSE" | python3 -c "
import json, sys, base64
d = json.load(sys.stdin)
raw = d.get('result_summary')
if isinstance(raw, str):
    try:
        raw = base64.b64decode(raw)
    except Exception:
        raw = None
if isinstance(raw, (bytes, bytearray)):
    try:
        raw = raw.decode()
    except Exception:
        raw = None
s = json.loads(raw) if isinstance(raw, str) and raw else (raw if isinstance(raw, dict) else {})
if int(s.get('http_reqs') or 0) <= 0:
    print('result_summary:', json.dumps(s)[:400], file=sys.stderr)
    sys.exit(3)
print('http_reqs:', s['http_reqs'], 'rps:', s.get('rps'), 'p95_ms:', s.get('p95_ms'),
      'thresholds_failed:', d.get('thresholds_failed'))
" > /tmp/verify-result.txt 2>/tmp/verify-result.err && \
      ok "Results collected: $(cat /tmp/verify-result.txt)" && return 0
    fi
    sleep 1
  done
  
  echo "--- loadtest payload ---"
  curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID"
  echo "--- assertion error ---"
  cat /tmp/verify-result.err 2>/dev/null || true
  fail "Results not collected within 30s"
}

verify_networkpolicy_cleanup() {
  say "Verifying run-scoped NetworkPolicies garbage-collected"
  
  for np in loadtest-run-egress-$LT_ID loadtest-run-ingress-$LT_ID-echo loadtest-run-api-$LT_ID; do
    for i in $(seq 1 15); do
      if ! kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then break; fi
      sleep 1
    done
    if kubectl get networkpolicy -n "$ENVNS" "$np" >/dev/null 2>&1; then
      fail "Run NetworkPolicy $np still present after run terminal"
    fi
  done
  
  ok "Run-scoped NetworkPolicies garbage-collected"
}

verify_events() {
  say "Verifying loadtest events and unreliable flag"
  
  EVENTS=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID/events")
  echo "$EVENTS" | python3 -c "
import json, sys
d = json.load(sys.stdin)
ev = len(d.get('events', []))
unrel = d.get('unreliable')
if unrel is not False:
    sys.exit(1)
print('events:', ev, 'unreliable:', unrel)
" >/dev/null || fail "Events payload unreadable or unreliable!=false: $EVENTS"
  
  ok "Events verified (unreliable=false)"
}

dump_projection() {
  say "Dumping projection state"
  
  # This requires psql access to the database
  # In kind, we can exec into the postgres pod
  kubectl exec -n ltp-system postgresql-test-db-0 -- \
    psql -U ltp -d ltp -c "
    SELECT component_type, component_id, phase, desired_generation, observed_generation
    FROM tbl_component_status WHERE namespace_id = '$NS' ORDER BY component_type;
  " 2>/dev/null || warn "Could not query projection (psql not available in pod)"
  
  kubectl exec -n ltp-system postgresql-test-db-0 -- \
    psql -U ltp -d ltp -tAc "
    SELECT status || ' ready=' || ready_for_loadtest FROM tbl_namespaces WHERE id = '$NS';
  " 2>/dev/null || warn "Could not query namespace status"
}

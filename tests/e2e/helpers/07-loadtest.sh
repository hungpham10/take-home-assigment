#!/bin/sh
# 07-loadtest.sh — Load test lifecycle
# Usage: . tests/e2e/helpers/common.sh && start_loadtest && watch_loadtest

start_loadtest() {
  require_tools
  
  say "Starting load test"
  
  LT_RESPONSE=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces/$NS/loadtests" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: e2e-lt-start-$(date +%s)" \
    -d "{
      \"script_id\": \"$SCRIPT_ID\",
      \"name\": \"e2e-loadtest-$(date +%s)\",
      \"vus\": 5,
      \"duration\": \"30s\",
      \"timeout\": \"5m\"
    }")
  
  LT_ID=$(echo "$LT_RESPONSE" | jqget "['id']")
  LT_STATUS=$(echo "$LT_RESPONSE" | jqget "['status']")
  
  [ -n "$LT_ID" ] || fail "Failed to start load test: $LT_RESPONSE"
  
  ok "Load test started: $LT_ID (status: $LT_STATUS)"
}

watch_loadtest() {
  say "Watching load test: $LT_ID"
  
  for i in $(seq 1 120); do
    LT_RESPONSE=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/loadtests/$LT_ID")
    LT_STATUS=$(echo "$LT_RESPONSE" | jqget "['status']")
    
    case "$LT_STATUS" in
      COMPLETED)
        ok "Load test COMPLETED"
        return 0
        ;;
      FAILED|STOPPED|TIMED_OUT|TIMEOUT_CLEANED)
        fail "Load test ended with status: $LT_STATUS"
        ;;
      RUNNING|PROVISIONING)
        echo "  Status: $LT_STATUS (poll $i/120)"
        ;;
      *)
        echo "  Status: $LT_STATUS (poll $i/120)"
        ;;
    esac
    sleep 5
  done
  
  fail "Load test timed out (10 minutes)"
}

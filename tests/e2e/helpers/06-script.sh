#!/bin/sh
# 06-script.sh — Script upload and validation
# Usage: . tests/e2e/helpers/common.sh && upload_script

upload_script() {
  require_tools
  
  SCRIPT_FILE="${1:-tests/perf/sample-service.js}"
  [ -f "$SCRIPT_FILE" ] || fail "Script file not found: $SCRIPT_FILE"
  
  say "Uploading k6 script: $SCRIPT_FILE"
  
  SCRIPT_RESPONSE=$(curl -s -X POST "$BASE/api/loadtest/v1/namespaces/$NS/scripts" \
    -H "Content-Type: application/javascript" \
    -H "Idempotency-Key: e2e-script-push-$(date +%s)" \
    --data-binary @"$SCRIPT_FILE")
  
  SCRIPT_ID=$(echo "$SCRIPT_RESPONSE" | jqget "['id']")
  SCRIPT_STATUS=$(echo "$SCRIPT_RESPONSE" | jqget "['status']")
  
  [ -n "$SCRIPT_ID" ] || fail "Failed to upload script: $SCRIPT_RESPONSE"
  
  say "Waiting for script validation: $SCRIPT_ID"
  for i in $(seq 1 30); do
    VAL_RESPONSE=$(curl -s "$BASE/api/loadtest/v1/namespaces/$NS/scripts/$SCRIPT_ID/validations")
    LATEST_STATUS=$(echo "$VAL_RESPONSE" | python3 -c "
import json, sys
d = json.load(sys.stdin)
if d.get('attempts'):
    print(d['attempts'][-1].get('status', ''))
")
    if [ "$LATEST_STATUS" = "VALIDATED" ]; then
      ok "Script validated: $SCRIPT_ID"
      return 0
    elif [ "$LATEST_STATUS" = "INVALID" ]; then
      fail "Script validation failed: $VAL_RESPONSE"
    fi
    sleep 2
  done
  
  fail "Script validation timed out"
}

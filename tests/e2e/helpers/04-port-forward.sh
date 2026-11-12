#!/bin/sh
# 04-port-forward.sh — Port-forward serve service for API access
# Usage: . tests/e2e/helpers/common.sh && start_port_forward
# Returns PID of port-forward in PF_PID variable

PF_PID=""

start_port_forward() {
  say "Starting port-forward: svc/serve 8080:8080 (namespace ltp-system)"
  
  # Kill any existing port-forward on 8080
  pkill -f "port-forward.*serve.*8080" 2>/dev/null || true
  
  kubectl port-forward -n ltp-system svc/serve 8080:8080 > /tmp/port-forward.log 2>&1 &
  PF_PID=$!
  
  # Wait for port-forward to be ready
  wait_for "Port-forward ready" 30 1 \
    "curl -sf http://localhost:8080/health >/dev/null 2>&1"
  
  BASE="http://localhost:8080"
  ok "Port-forward active (PID: $PF_PID)"
}

stop_port_forward() {
  if [ -n "$PF_PID" ]; then
    say "Stopping port-forward (PID: $PF_PID)"
    kill $PF_PID 2>/dev/null || true
    wait $PF_PID 2>/dev/null || true
    PF_PID=""
  fi
}

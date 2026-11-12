#!/bin/sh
# 02-install-operators.sh — Install CNPG and k6-operator
# Usage: . tests/e2e/helpers/common.sh && install_operators

install_operators() {
  require_tools
  
  # --- CNPG (CloudNativePG) ---
  say "Installing CNPG operator v$CNPG_VERSION"
  kubectl apply -f "$CNPG_MANIFEST"
  
  wait_for "CNPG operator ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/cnpg-controller-manager -n cnpg-system --timeout=30s 2>/dev/null"
  
  ok "CNPG operator installed"
  
  # --- k6-operator ---
  say "Installing k6-operator v$K6_OPERATOR_VERSION"
  kubectl apply -f "$K6_OPERATOR_BUNDLE"
  
  wait_for "k6-operator ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/k6-operator-controller-manager -n k6-operator-system --timeout=30s 2>/dev/null"
  
  ok "k6-operator installed"
}

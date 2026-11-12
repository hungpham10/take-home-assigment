#!/bin/sh
# 03-deploy-platform.sh — Deploy platform via kustomize
# Usage: . tests/e2e/helpers/common.sh && deploy_platform

deploy_platform() {
  require_tools
  
  say "Deploying platform via kustomize (deploy/k8s/overlays/kind)"
  
  # Apply the base + kind overlay
  kubectl apply -k deploy/k8s/overlays/kind
  
  # Wait for deployments
  wait_for "serve deployment ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/serve -n ltp-system --timeout=30s 2>/dev/null"
  
  wait_for "mock-service deployment ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/mock-service -n mock-system --timeout=30s 2>/dev/null"
  
  wait_for "postgres StatefulSet ready" 120 10 \
    "kubectl wait --for=condition=Ready pod -l app=postgres -n ltp-system --timeout=30s 2>/dev/null"
  
  wait_for "valkey ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/valkey -n ltp-system --timeout=30s 2>/dev/null"
  
  wait_for "dex ready" 60 5 \
    "kubectl wait --for=condition=Available deployment/dex -n ltp-system --timeout=30s 2>/dev/null"
  
  ok "Platform deployed"
}

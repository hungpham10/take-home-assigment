#!/bin/sh
# E2E Full Deploy — Main orchestrator using modular helpers
# 
# Usage:
#   ./tests/e2e-full-deploy.sh                 # Full run (bootstrap + deploy + test + cleanup)
#   SKIP_BOOTSTRAP=1 ./tests/e2e-full-deploy.sh # Use existing cluster
#   KEEP_CLUSTER=1 ./tests/e2e-full-deploy.sh   # Preserve cluster after run
#   DEPLOY_MODE=in-cluster ./tests/e2e-full-deploy.sh # Build images + kustomize
#
# Individual phases can be run by sourcing helpers:
#   . tests/e2e/helpers/common.sh
#   . tests/e2e/helpers/01-cluster-bootstrap.sh
#   bootstrap_cluster
#   . tests/e2e/helpers/02-install-operators.sh
#   install_operators
#   ...

set -e

# Source all helpers
. tests/e2e/helpers/common.sh
. tests/e2e/helpers/01-cluster-bootstrap.sh
. tests/e2e/helpers/02-install-operators.sh
. tests/e2e/helpers/03-deploy-platform.sh
. tests/e2e/helpers/04-port-forward.sh
. tests/e2e/helpers/05-namespace.sh
. tests/e2e/helpers/06-script.sh
. tests/e2e/helpers/07-loadtest.sh
. tests/e2e/helpers/08-verify.sh
. tests/e2e/helpers/09-cleanup.sh

# Trap cleanup on exit
cleanup() {
  stop_port_forward
  teardown_cluster
}
trap cleanup EXIT INT TERM

main() {
  say "=== E2E FULL DEPLOY START ==="
  say "Cluster: $CLUSTER | Mode: $DEPLOY_MODE | SUT: $SUT_IMAGE"
  
  # Phase 1: Cluster bootstrap
  bootstrap_cluster
  preload_images
  
  # Phase 2: Install operators
  install_operators
  
  # Phase 3: Deploy platform (in-cluster mode builds images first)
  if [ "$DEPLOY_MODE" = "in-cluster" ]; then
    say "Building images for in-cluster deployment"
    make build
    make build-sample
    # Load images into kind
    kind load docker-image loadtest-platform-serve:local --name "$CLUSTER"
    kind load docker-image loadtest-platform-mock-service:local --name "$CLUSTER"
    kind load docker-image ltp/sample-service:dev --name "$CLUSTER"
  fi
  deploy_platform
  
  # Phase 4: Port-forward for API access
  start_port_forward
  
  # Phase 5: Namespace lifecycle
  create_namespace_spec
  validate_namespace
  apply_namespace
  wait_namespace_ready
  
  # Phase 6: Script upload
  upload_script "tests/perf/sample-service.js"
  
  # Phase 7: Load test
  start_loadtest
  watch_loadtest
  
  # Phase 8: Verify results
  verify_results
  verify_networkpolicy_cleanup
  verify_events
  dump_projection
  
  # Phase 9: Cleanup
  delete_namespace
  verify_k8s_cleanup
  
  say "=== E2E FULL DEPLOY PASS (namespace=$NS) ==="
}

main "$@"

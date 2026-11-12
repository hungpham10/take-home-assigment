#!/bin/sh
# 01-cluster-bootstrap.sh — Create kind cluster with Calico CNI
# Usage: . tests/e2e/helpers/common.sh && bootstrap_cluster
# Env: CLUSTER, KIND_NODE_IMAGE, SKIP_BOOTSTRAP

bootstrap_cluster() {
  require_tools
  
  if [ "${SKIP_BOOTSTRAP:-0}" = "1" ]; then
    say "SKIP_BOOTSTRAP=1: using existing cluster"
    kind export kubeconfig --name "$CLUSTER"
    return 0
  fi
  
  command -v kind >/dev/null || fail "kind not installed"
  
  say "Creating kind cluster: $CLUSTER (disableDefaultCNI for Calico)"
  
  if kind get clusters | grep -q "^$CLUSTER$"; then
    say "Deleting existing cluster $CLUSTER"
    kind delete cluster --name "$CLUSTER"
  fi
  
  KIND_CONFIG="$(mktemp)"
  cat > "$KIND_CONFIG" <<KIND_YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
nodes:
  - role: control-plane
  - role: worker
KIND_YAML
  
  kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" \
    --config "$KIND_CONFIG" --wait 300s
  rm -f "$KIND_CONFIG"

  # Wait for control-plane node to be Ready (kind --wait may time out
  # on slow CI runners; the cluster exists but isn't usable yet).
  wait_for "control-plane node Ready" 60 5 \
    "kubectl get nodes | grep -q 'control-plane.* Ready'"

  kind export kubeconfig --name "$CLUSTER"
  ok "Cluster created and kubeconfig exported"
  
  # Install Calico CNI for NetworkPolicy enforcement
  install_calico
}

install_calico() {
  say "Installing Calico CNI (NetworkPolicy enforcement)"
  
  # Calico operator
  kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.29.1/manifests/tigera-operator.yaml
  
  # Calico custom resources
  cat <<EOF | kubectl apply -f -
apiVersion: operator.tigera.io/v1
kind: Installation
metadata:
  name: default
spec:
  calicoNetwork:
    ipPools:
    - blockSize: 26
      cidr: 10.244.0.0/16
      encapsulation: VXLANCrossSubnet
      natOutgoing: Enabled
      nodeSelector: all()
---
apiVersion: operator.tigera.io/v1
kind: APIServer
metadata:
  name: default
spec: {}
EOF
  
  # Wait for Calico to be ready
  wait_for "Calico pods ready" 60 5 \
    "kubectl wait --for=condition=Ready pods -l k8s-app=calico-node -n calico-system --timeout=30s 2>/dev/null"
  
  ok "Calico CNI installed"
}

# Pre-load images into kind nodes
preload_images() {
  say "Pre-loading images into kind nodes"
  
  # CNPG postgres image
  kind load docker-image ghcr.io/cloudnative-pg/postgres:16.2 --name "$CLUSTER" 2>/dev/null || true
  
  # SUT image
  kind load docker-image "$SUT_IMAGE" --name "$CLUSTER" 2>/dev/null || \
    warn "SUT image $SUT_IMAGE not found locally, will pull from registry"
  
  ok "Images pre-loaded"
}

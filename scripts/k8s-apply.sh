#!/bin/bash
# scripts/k8s-apply.sh — áp dụng deploy/k8s/overlays/kind lên cluster hiện tại
# (KUBECONFIG đã export). Tạo secret không commit (postgres-credentials,
# sops-age-key) rồi rollout status. Dùng cho CI job và local dev.
#
# Env tuỳ chọn:
#   POSTGRES_PASSWORD (mặc định ltp)
#   SOPS_AGE_KEY_CONTENT (bắt buộc — toàn bộ file age keys.txt, dùng để tạo
#                        K8s Secret sops-age-key cho release.sh giải mã boot)
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVERLAY="$BASE_DIR/deploy/k8s/overlays/kind"

fail() { echo "FAIL: $*" >&2; exit 1; }

command -v kubectl >/dev/null || fail "kubectl not installed"
[ -n "${KUBECONFIG:-}" ] || KUBECONFIG="$HOME/.kube/config"
export KUBECONFIG

# --- 1. guard: UDS path trong conf/nginx phải là FILE socket /var/run/axum ---
# Controller bind `unix:/var/run/axum` (conf/supervisor/ltp.conf) và pkgs/listener
# tự mkdir parent + rm stale socket. Nếu path đó bị tạo thành THƯ MỤC
# (mkdir trong entrypoint, emptyDir mount vào /var/run/axum, hay ai đó đổi sang
# /var/run/axum/controller.sock) thì controller không bind được và mọi request
# /api/* chết trước handler (plans/36 Fix log #2). Lỗi gần như vô hình trong log.
BAD_SOCKET=$(grep -rn 'unix:/var/run/axum/' "$BASE_DIR/conf/nginx" || true)
if [ -n "$BAD_SOCKET" ]; then
  echo "$BAD_SOCKET" >&2
  fail "unix socket phải là /var/run/axum (file), không phải thư mục (xem docs/nginx-oidc-auth.md)"
fi

# --- 2. apply overlay kind (tạo namespace ltp-system, mock-system, rồi các workload) ---
echo "== apply k8s overlay kind =="
kubectl apply -k "$OVERLAY"

# --- 3. secret postgres-credentials (control-plane DB, không commit) ---
# Namespace ltp-system đã tồn tại sau bước apply.
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-ltp}"
kubectl -n ltp-system create secret generic postgres-credentials \
  --from-literal=POSTGRES_USER=ltp \
  --from-literal=POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
  --from-literal=POSTGRES_DB=ltp \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# --- 4. secret sops-age-key (release.sh giải mã secrets.enc.yaml tại boot) ---
if [ -z "${SOPS_AGE_KEY_CONTENT:-}" ]; then
  fail "SOPS_AGE_KEY_CONTENT chưa set (toàn bộ nội dung file age keys.txt). Xem docs/k8s-deployment.md §1."
fi
kubectl -n ltp-system create secret generic sops-age-key \
  --from-literal=SOPS_AGE_KEY_CONTENT="$SOPS_AGE_KEY_CONTENT" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# --- 5. rollout status ---
echo "== rollout status =="
for res in "sts/postgres -n ltp-system" \
           "deploy/valkey -n ltp-system" \
           "deploy/dex -n ltp-system" \
           "deploy/serve -n ltp-system" \
           "deploy/mock-service -n mock-system"; do
  if ! kubectl rollout status $res --timeout=180s; then
    echo "--- dump logs for failed resource: $res ---"
    case "$res" in
      deploy/serve*) kubectl -n ltp-system logs deploy/serve --all-containers --tail=100 2>&1 || true ;;
      deploy/dex*)   kubectl -n ltp-system logs deploy/dex   --all-containers --tail=80  2>&1 || true ;;
      deploy/valkey*)kubectl -n ltp-system logs deploy/valkey --all-containers --tail=80  2>&1 || true ;;
    esac
    fail "rollout $res"
  fi
done

echo "== apply xong =="
kubectl -n ltp-system get pods

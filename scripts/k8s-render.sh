#!/usr/bin/env bash
#
# k8s-render.sh — render a kustomize overlay with the real image tag
# (docs/versioning.md, plans/38).
#
# Usage:
#   ./scripts/k8s-render.sh [kind|release] [version]
#
#   kind    → deploy/k8s/overlays/kind, tag defaults to "local"
#   release → deploy/k8s/overlays/release, tag defaults to the latest git tag
#             (ghcr.io images); fails if no tag exists and no version is given.
#
# Environment:
#   K8S_VERSION   fallback version when the positional arg is empty
#   IMAGE_PREFIX  override the ghcr image prefix for the release overlay
#                 (default: ghcr.io/hungpham10/loadtest-platform)
#
# Output: bin/k8s/<overlay>.yaml (stdout prints the path).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$SCRIPT_DIR"

OVERLAY="${1:-release}"
VERSION="${2:-${K8S_VERSION:-}}"

case "$OVERLAY" in
  kind|release) ;;
  *) echo "k8s-render: unknown overlay '$OVERLAY' (expected kind|release)" >&2; exit 2 ;;
esac

SRC_ROOT="deploy/k8s"
[ -d "$SRC_ROOT/overlays/$OVERLAY" ] || { echo "k8s-render: overlay not found: $SRC_ROOT/overlays/$OVERLAY" >&2; exit 1; }

if [ -z "$VERSION" ]; then
  if [ "$OVERLAY" = "release" ]; then
    VERSION="$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null | sed 's/^v//' || true)"
    if [ -z "$VERSION" ]; then
      echo "k8s-render: no release tag found; pass a version explicitly (e.g. make k8s-render VERSION=0.1.0)" >&2
      exit 1
    fi
  else
    VERSION="local"
  fi
fi

case "$VERSION" in
  ""|*[[:space:]]*) echo "k8s-render: invalid version '$VERSION'" >&2; exit 1 ;;
esac

WORK="$(mktemp -d)"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# Copy the whole k8s tree so relative `resources: ../../base` stays valid.
mkdir -p "$WORK/deploy"
cp -R "$SRC_ROOT" "$WORK/deploy/k8s"
KUST="$WORK/deploy/k8s/overlays/$OVERLAY/kustomization.yaml"

sed_inplace() {
  local expr="$1" file="$2"
  if sed --version >/dev/null 2>&1; then
    sed -i "$expr" "$file"
  else
    sed -i '' "$expr" "$file"
  fi
}

sed_inplace "s|newTag: .*|newTag: ${VERSION}|" "$KUST"

if [ "$OVERLAY" = "release" ] && [ -n "${IMAGE_PREFIX:-}" ]; then
  sed_inplace "s|newName: ghcr.io/hungpham10/loadtest-platform/serve|newName: ${IMAGE_PREFIX}/serve|" "$KUST"
  sed_inplace "s|newName: ghcr.io/hungpham10/loadtest-platform/mock-service|newName: ${IMAGE_PREFIX}/mock-service|" "$KUST"
fi

OUT_DIR="bin/k8s"
mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/${OVERLAY}.yaml"

if command -v kubectl >/dev/null 2>&1; then
  kubectl kustomize "$WORK/deploy/k8s/overlays/$OVERLAY" > "$OUT"
elif command -v kustomize >/dev/null 2>&1; then
  kustomize build "$WORK/deploy/k8s/overlays/$OVERLAY" > "$OUT"
else
  echo "k8s-render: kubectl or kustomize is required" >&2
  exit 1
fi

echo "$OUT"

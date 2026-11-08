#!/bin/sh
# lint-transitions — state transitions chỉ được gọi trong pkgs/store/
# (single source of truth: pkgs/store/*_lifecycle.go — plans/26).
# Vi phạm: thêm trạng thái mới sẽ phải săn call-site thủ công (rủi ro bỏ sót).
set -e
VIOLATIONS=$(grep -rn "store\.Transition(\|store\.TransitionMeta(" \
  pkgs cmds tests --include='*.go' 2>/dev/null \
  | grep -v "_test.go" | grep -v "pkgs/store/" || true)
if [ -n "$VIOLATIONS" ]; then
  echo "VIOLATION: raw store.Transition outside pkgs/store/ — dùng wrapper trong pkgs/store/*_lifecycle.go:" >&2
  echo "$VIOLATIONS" >&2
  exit 1
fi
echo "lint-transitions: OK (raw transitions chỉ trong pkgs/store/)"

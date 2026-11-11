#!/bin/bash
# Entry shim (plans/34): tạo thư mục unix socket cho nginx upstream
# (conf/nginx/http.conf: server unix:/var/run/axum), source release.sh
# (SOPS decrypt + prepare + boot) rồi exec supervisord.
set -e
mkdir -p /var/run/axum
[ -f /app/scripts/release.sh ] && . /app/scripts/release.sh || true
exec "$@"

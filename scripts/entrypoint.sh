#!/bin/sh
# Entry shim: decrypt SOPS secrets rồi exec controller (App chỉ có env vars).
set -e
[ -f /app/scripts/release.sh ] && . /app/scripts/release.sh || true
exec "$@"

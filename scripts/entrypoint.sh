#!/bin/bash
# Entry shim (plans/34): source release.sh (SOPS decrypt + prepare + boot rồi
# exec supervisord).
#
# /var/run/axum là FILE unix socket do controller tự tạo (pkgs/listener:
# mkdir parent + rm stale + chmod 666). KHÔNG mkdir path này ở đây — tạo nó
# thành thư mục sẽ làm `os.Remove(path)` fail (EBUSY/EISDIR) và controller
# không bind được, nginx upstream `unix:/var/run/axum` sẽ Connection refused.
set -e
[ -f /app/scripts/release.sh ] && . /app/scripts/release.sh || true
exec "$@"

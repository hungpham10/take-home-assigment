#!/bin/bash

######################################################################
# @author      : Hung Nguyen Xuan Pham (hung0913208@gmail.com)
# @file        : release
# @created     : Tuesday Aug 13, 2024 22:19:39 +07
# @description : Entrypoint tích hợp SOPS giải mã đa môi trường
#                (plans/34: AIO serve image — không chạy SQL ở đây,
#                controller tự migrate qua embedded SQL embed.go)
######################################################################

function decrypt_secrets() {
  local ENV=${APP_ENV:-dev}
  local ENCRYPTED_FILE="${ENCRYPTED_FILE:-/app/secrets/secrets.${ENV}.enc.yaml}"

  if [ -z "$SOPS_AGE_KEY_CONTENT" ]; then
    echo "--- [SOPS] Not found SOPS_AGE_KEY_CONTENT. Ignore decryption. ---"
    return
  fi

  if [ -f "$ENCRYPTED_FILE" ]; then
    local decrypted_content
    decrypted_content=$(SOPS_AGE_KEY_FILE=<(echo "$SOPS_AGE_KEY_CONTENT") sops -d --output-type dotenv "$ENCRYPTED_FILE" 2>/dev/null)

    if [ -n "$decrypted_content" ]; then
      while IFS= read -r line || [ -n "$line" ]; do
        [[ -z "$line" || "$line" =~ ^# ]] && continue

        if [ "${DEBUG}" = "true" ]; then
          echo "$line"
        fi

        export "$line"
      done <<< "$decrypted_content"
    else
      echo "Error: [SOPS] Decrypt failed." >&2
      exit 1
    fi

    unset SOPS_AGE_KEY_CONTENT
    export SOPS_AGE_KEY_CONTENT=""
  else
    echo "--- [SOPS] Not found $ENCRYPTED_FILE, will use default environment variables ---"
  fi
}

# prepare — plans/34: SQL KHÔNG chạy ở đây (controller tự migrate qua embed.go).
# Chỉ đợi PostgreSQL sẵn sàng nếu được cấu hình, để supervisord không phải
# restart controller liên tục trong lúc DB chưa lên.
function prepare() {
  if [[ ${DISABLE_AUTO_INIT_DATABASE} = "true" ]]; then
    return
  fi

  if [ -n "$POSTGRES_HOST" ]; then
    export PGPASSWORD="${POSTGRES_PASSWORD:-}"
    for i in {1..30}; do
      if pg_isready -h "$POSTGRES_HOST" -p "${POSTGRES_PORT:-5432}" -U "${POSTGRES_USER:-postgres}" >/dev/null 2>&1; then
        echo "--- PostgreSQL is ready ---"
        unset PGPASSWORD
        return
      fi
      echo "Waiting for PostgreSQL to be ready ($i/30)..."
      sleep 1
    done
    echo "Error: PostgreSQL is not available after 30s." >&2
    unset PGPASSWORD
    exit 1
  fi
  echo "--- No POSTGRES_HOST configured, skip DB wait (controller will fail fast if DSN is set) ---"
}

function localtonet() {
  if [ -z "${DOTNET_SYSTEM_GLOBALIZATION_INVARIANT:-}" ]; then
    export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1
  fi

  if [ -n "${LOCALTONET:-}" ]; then
    set -x
    screen -S "localtonet.pid" -dm localtonet authtoken "$LOCALTONET"
    set +x
  fi
}

function boot() {
  local cmd=$1
  local nginx_conf="${NGINX_DIR}/http.d/default.conf"

  # Placeholders của conf/nginx (plans/34):
  #   %%NGINX_LOG%%    — access_log format (http.conf)
  #   %%HTTP_SERVER%%  — server_name (www.conf)
  #   %%TOR_SERVER%%   — legacy alias, dev = HTTP_SERVER
  #   %%FORCE_SSL%%    — chỉ tồn tại khi template có HTTPS; dev = off
  if [ "${HTTP_PROTOCOL}" = "https" ]; then
    sed -i "s/%%FORCE_SSL%%/on/g" "$nginx_conf"
  else
    sed -i '/HTTPS/d; /HTTP_X_FORWARDED_PROTO/d; /HTTP_X_FORWARDED_PORT/d' "$nginx_conf"
    sed -i "s/%%FORCE_SSL%%/off/g" "$nginx_conf"
    HTTP_PROTOCOL="http"
  fi

  sed -i "s/%%NGINX_LOG%%/$NGINX_LOG/g" "${NGINX_DIR}/nginx.conf"
  sed -i "s|%%HTTP_SERVER%%|$HTTP_SERVER|g; s|%%TOR_SERVER%%|$HTTP_SERVER|g" "$nginx_conf"

  shift
  exec "$cmd" "$@"
}

function main() {
  local cmd=$1
  shift

  decrypt_secrets

  prepare

  localtonet

  boot "$cmd" "$@"
}

main "$@"

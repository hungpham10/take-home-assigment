# AIO serve image: OpenResty + supervisor + controller + mock-service + alloy.
# Dùng cho kind local (imagePullPolicy: IfNotPresent) và CI.
FROM openresty/openresty:1.27.1.2-4-bookworm-fat

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    supervisor curl gettext-base git gnupg2 postgresql-client ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor -o /etc/apt/keyrings/grafana.gpg && \
    echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" \
      > /etc/apt/sources.list.d/grafana.list && \
    apt-get update && apt-get install -y --no-install-recommends alloy && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://github.com/getsops/sops/releases/download/v3.9.4/sops-v3.9.4.linux.amd64 \
    -o /usr/local/bin/sops && chmod +x /usr/local/bin/sops

# Lua-resty modules
RUN cd /tmp && \
    git clone --depth 1 https://github.com/zmartzone/lua-resty-openidc.git && cp -av lua-resty-openidc/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-openidc && \
    git clone --depth 1 https://github.com/fffonion/lua-resty-openssl.git && cp -av lua-resty-openssl/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-openssl && \
    git clone --depth 1 https://github.com/jkeys089/lua-resty-hmac.git && cp -av lua-resty-hmac/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-hmac && \
    git clone --depth 1 https://github.com/cdbattags/lua-resty-jwt.git && cp -av lua-resty-jwt/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-jwt && \
    git clone --depth 1 https://github.com/bungle/lua-resty-session.git && cp -av lua-resty-session/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-session && \
    git clone --depth 1 https://github.com/ledgetech/lua-resty-http.git && cp -av lua-resty-http/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-http && \
    git clone --depth 1 https://github.com/openresty/lua-resty-redis.git && cp -av lua-resty-redis/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-redis && \
    git clone --depth 1 https://github.com/anvouk/lua-resty-jwt-verification.git && cp -av lua-resty-jwt-verification/lib/resty/* /usr/local/openresty/lualib/resty/ && rm -rf lua-resty-jwt-verification && \
    git clone --depth 1 https://github.com/hamishforbes/lua-ffi-zlib.git && cp -av lua-ffi-zlib/lib/ffi-zlib.lua /usr/local/openresty/lualib/ && rm -rf lua-ffi-zlib

RUN useradd nginx && \
    # /var/run/axum là FILE socket (không phải thư mục) — controller tự tạo
    # parent dir + socket (pkgs/listener). Tạo sẵn path dạng thư mục ở đây sẽ
    # làm bind/remove stale socket fail.
    mkdir -p /var/log/nginx /app/secrets /app/scripts && \
    chown -R nginx:nginx /var/log/nginx

RUN mkdir -p /etc/supervisor/conf.d
COPY conf/supervisor/ltp.conf /etc/supervisor/conf.d/ltp.conf
COPY conf/nginx/http.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY conf/nginx/www.conf /usr/local/openresty/nginx/conf/http.d/default.conf
COPY conf/nginx/map /usr/local/openresty/nginx/conf/map.d
COPY conf/nginx/vhost /usr/local/openresty/nginx/conf/http.d/vhost
COPY conf/config.alloy /etc/alloy/config.alloy
COPY conf/dex/config.dev.yaml /etc/dex/config.dev.yaml

COPY scripts/nginx.sh /app/nginx.sh
COPY scripts/alloy.sh /app/alloy.sh
COPY scripts/release.sh /app/scripts/release.sh
COPY scripts/entrypoint.sh /app/entrypoint.sh

ARG VERSION=local
ARG GIT_COMMIT=unknown
COPY bin/loadtest-controller /app/loadtest-controller
COPY bin/mock-service /app/mock-service
RUN chmod +x /app/*.sh /app/loadtest-controller /app/mock-service
# Fail-fast evidence in the build log: which version got baked in (plans/38).
RUN /app/loadtest-controller --version && /app/mock-service --version

ENTRYPOINT ["/app/entrypoint.sh", "/usr/bin/supervisord", "-n"]
EXPOSE 8080

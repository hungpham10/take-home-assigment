VERSION 0.8

# -----------------------------------------------------------------------
# Global config — override khi gọi, e.g. `earthly +all --VERSION=v0.1.0`
# Pattern: sources/opsense-rs/Earthfile (multi-stage, prebuilt-binary fast path)
# Docs: docs/packaging-deployment.md | Plan: plans/34-aio-serve-image.md
# -----------------------------------------------------------------------
ARG --global REGISTRY=ghcr.io
ARG --global IMAGE_PREFIX=loadtest-platform
# Version = git tag (docs/versioning.md). "dev" = no release information.
ARG --global VERSION=dev
ARG --global GIT_COMMIT=unknown
ARG --global BUILD_DATE=

# -----------------------------------------------------------------------
# build — build 2 Go binary (fast path: binary từ host nếu có, fallback go build)
# -----------------------------------------------------------------------
build:
    FROM golang:1.26-bookworm
    # Version comes from the git tag (docs/versioning.md). CI/Makefile pass it
    # explicitly; `dev` means "no release information available".
    ARG VERSION=$VERSION
    ARG GIT_COMMIT=$GIT_COMMIT
    ARG BUILD_DATE=$BUILD_DATE
    WORKDIR /src
    # CI stage binaries theo bin/ trước khi gọi; --if-exists ⇒ không fail nếu thiếu
    COPY --if-exists bin/ /src/host-bin/
    COPY . /src/code/
    # Fast path chỉ nhận ELF (CI stage linux binary vào bin/); Mach-O darwin
    # từ `make build` trên host phải bị loại — không chạy được trong container.
    # Ngoài ELF, binary phải báo ĐÚNG version của build này: binary cũ trong
    # bin/ (build từ commit khác) sẽ bị rebuild thay vì đóng gói nhầm.
    RUN sh -c '\
      set -e && \
      mkdir -p /out && \
      ldflags="-X github.com/hungpham/loadtest-platform/pkgs/version.Version=$VERSION -X github.com/hungpham/loadtest-platform/pkgs/version.Commit=$GIT_COMMIT" && \
      if [ -n "$BUILD_DATE" ]; then ldflags="$ldflags -X github.com/hungpham/loadtest-platform/pkgs/version.BuildDate=$BUILD_DATE"; fi && \
      expected="$VERSION" && \
      case "$VERSION" in dev) ;; *) expected="v$VERSION" ;; esac && \
      use_host=1 && \
      for b in loadtest-controller mock-service ltp; do \
        src="/src/host-bin/$b"; \
        sig=$(head -c4 "$src" 2>/dev/null | od -An -tx1); \
        v=$("$src" --version 2>/dev/null || true); \
        ok=0; \
        case "$sig" in *"7f 45 4c 46"*) case "$v" in "$expected"|"$expected "*) ok=1 ;; esac ;; esac; \
        if [ "$ok" != 1 ]; then use_host=0; echo "--> $b: host binary unusable (version ${v:-none}), rebuilding"; fi; \
      done && \
      if [ "$use_host" = 1 ]; then \
        echo "--> Using pre-built host binaries"; \
        cp /src/host-bin/loadtest-controller /src/host-bin/mock-service /src/host-bin/ltp /out/; \
      else \
        echo "--> Building inside Earthly (VERSION=$VERSION)..."; \
        cd /src/code && \
        for b in loadtest-controller mock-service ltp; do \
          go build -ldflags "$ldflags" -o "/out/$b" "./cmds/$b"; \
        done; \
      fi && \
      chmod +x /out/loadtest-controller /out/mock-service /out/ltp && \
      /out/loadtest-controller --version && /out/mock-service --version && /out/ltp --version && \
      ls -la /out/'
    SAVE ARTIFACT /out/loadtest-controller AS LOCAL bin/loadtest-controller-earthly
    SAVE ARTIFACT /out/mock-service AS LOCAL bin/mock-service-earthly
    SAVE ARTIFACT /out/ltp AS LOCAL bin/ltp-earthly

# -----------------------------------------------------------------------
# serve — AIO image: OpenResty (nginx) + supervisor + controller (unix socket)
#         + mock-service (TCP :8090) + alloy. Entrypoint: SOPS → prepare → supervisord
# -----------------------------------------------------------------------
serve:
    FROM openresty/openresty:1.27.1.2-4-bookworm-fat
    ARG VERSION=$VERSION
    ARG GIT_COMMIT=$GIT_COMMIT
    ARG BUILD_DATE=$BUILD_DATE

    # Runtime deps tách nhiều RUN nhỏ (blob ≤ ~80MB — blob lớn qua local
    # registry pullping của Earthly trên colima/docker-29 hay bị EOF giữa dòng).
    RUN apt-get update && \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            supervisor curl gettext-base git gnupg2 && \
        apt-get clean && rm -rf /var/lib/apt/lists/*
    RUN apt-get update && \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            postgresql-client ca-certificates && \
        apt-get clean && rm -rf /var/lib/apt/lists/*
    RUN mkdir -p /etc/apt/keyrings && \
        curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor -o /etc/apt/keyrings/grafana.gpg && \
        echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" \
            > /etc/apt/sources.list.d/grafana.list && \
        apt-get update && apt-get install -y --no-install-recommends alloy && \
        apt-get clean && rm -rf /var/lib/apt/lists/*
    # sops không có package trên bookworm — lấy binary release chính thức
    RUN curl -fsSL https://github.com/getsops/sops/releases/download/v3.9.4/sops-v3.9.4.linux.$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') \
            -o /usr/local/bin/sops && chmod +x /usr/local/bin/sops

    # Lua-resty modules — mỗi repo một RUN (blob nhỏ, cache theo từng lib)
    RUN cd /tmp && \
        git clone --depth 1 https://github.com/zmartzone/lua-resty-openidc.git && \
        cp -av lua-resty-openidc/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-openidc && \
        git clone --depth 1 https://github.com/fffonion/lua-resty-openssl.git && \
        cp -av lua-resty-openssl/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-openssl
    RUN cd /tmp && \
        git clone --depth 1 https://github.com/jkeys089/lua-resty-hmac.git && \
        cp -av lua-resty-hmac/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-hmac && \
        git clone --depth 1 https://github.com/cdbattags/lua-resty-jwt.git && \
        cp -av lua-resty-jwt/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-jwt
    RUN cd /tmp && \
        git clone --depth 1 https://github.com/bungle/lua-resty-session.git && \
        cp -av lua-resty-session/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-session && \
        git clone --depth 1 https://github.com/ledgetech/lua-resty-http.git && \
        cp -av lua-resty-http/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-http
    RUN cd /tmp && \
        git clone --depth 1 https://github.com/openresty/lua-resty-redis.git && \
        cp -av lua-resty-redis/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-redis && \
        git clone --depth 1 https://github.com/anvouk/lua-resty-jwt-verification.git && \
        cp -av lua-resty-jwt-verification/lib/resty/* /usr/local/openresty/lualib/resty/ && \
        rm -rf lua-resty-jwt-verification

    RUN cd /tmp && \
        git clone --depth 1 https://github.com/hamishforbes/lua-ffi-zlib.git && \
        cp -av lua-ffi-zlib/lib/ffi-zlib.lua /usr/local/openresty/lualib/ && \
        rm -rf lua-ffi-zlib

    RUN useradd nginx && \
        # /var/run/axum is a unix socket FILE created by controller (pkgs/listener);
        # creating it as a directory here breaks bind on startup.
        mkdir -p /var/log/nginx /app/secrets /app/scripts && \
        chown -R nginx:nginx /var/log/nginx

    # Supervisor + nginx + alloy + dex configs
    RUN mkdir -p /etc/supervisor/conf.d
    COPY conf/supervisor/ltp.conf /etc/supervisor/conf.d/ltp.conf
    COPY conf/nginx/http.conf     /usr/local/openresty/nginx/conf/nginx.conf
    COPY conf/nginx/www.conf      /usr/local/openresty/nginx/conf/http.d/default.conf
    COPY conf/nginx/map           /usr/local/openresty/nginx/conf/map.d
    COPY conf/nginx/vhost         /usr/local/openresty/nginx/conf/http.d/vhost
    COPY conf/config.alloy        /etc/alloy/config.alloy
    # Bake để consistent với compose service — Dex thật chạy ở container dex riêng
    COPY conf/dex/config.dev.yaml /etc/dex/config.dev.yaml

    # Helper scripts + entrypoint
    COPY scripts/nginx.sh      /app/nginx.sh
    COPY scripts/alloy.sh      /app/alloy.sh
    COPY scripts/release.sh    /app/scripts/release.sh
    COPY scripts/entrypoint.sh /app/entrypoint.sh

    # Binaries lấy từ artifact của build (propagate version args so the fast-path
    # guard in +build can compare against THIS image's VERSION).
    COPY (+build/loadtest-controller --VERSION=$VERSION --GIT_COMMIT=$GIT_COMMIT --BUILD_DATE=$BUILD_DATE) /app/loadtest-controller
    COPY (+build/mock-service        --VERSION=$VERSION --GIT_COMMIT=$GIT_COMMIT --BUILD_DATE=$BUILD_DATE) /app/mock-service
    # Developer CLI (plans/44) ships in the image so `kubectl exec` has it too.
    COPY (+build/ltp                 --VERSION=$VERSION --GIT_COMMIT=$GIT_COMMIT --BUILD_DATE=$BUILD_DATE) /usr/local/bin/ltp
    RUN chmod +x /app/*.sh /app/loadtest-controller /app/mock-service /usr/local/bin/ltp

    ENTRYPOINT ["/app/entrypoint.sh", "/usr/bin/supervisord", "-n"]
    EXPOSE 8080
    SAVE IMAGE --push ${REGISTRY}/${IMAGE_PREFIX}/serve:${VERSION}
    SAVE IMAGE --push ${REGISTRY}/${IMAGE_PREFIX}/serve:latest
    SAVE IMAGE loadtest-platform-serve:${VERSION}
    SAVE IMAGE loadtest-platform-serve:latest

# -----------------------------------------------------------------------
# mock-service — data-plane unit riêng (k8s deployment mock-service scale ngang)
# -----------------------------------------------------------------------
mock-service:
    FROM debian:bookworm-slim
    ARG VERSION=$VERSION
    ARG GIT_COMMIT=$GIT_COMMIT
    ARG BUILD_DATE=$BUILD_DATE

    RUN apt-get update && \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            ca-certificates wget && \
        apt-get clean && rm -rf /var/lib/apt/lists/*

    COPY (+build/mock-service --VERSION=$VERSION --GIT_COMMIT=$GIT_COMMIT --BUILD_DATE=$BUILD_DATE) /app/mock-service
    RUN chmod +x /app/mock-service

    ENV ADDR=:8090
    EXPOSE 8090
    ENTRYPOINT ["/app/mock-service"]
    SAVE IMAGE --push ${REGISTRY}/${IMAGE_PREFIX}/mock-service:${VERSION}
    SAVE IMAGE --push ${REGISTRY}/${IMAGE_PREFIX}/mock-service:latest
    SAVE IMAGE loadtest-platform-mock-service:${VERSION}
    SAVE IMAGE loadtest-platform-mock-service:latest

# -----------------------------------------------------------------------
# test — chạy unit tests trong container (CI-fallback khi runner không có Go)
# -----------------------------------------------------------------------
test:
    FROM golang:1.26-bookworm
    WORKDIR /src
    COPY . .
    RUN go test ./... -race

# -----------------------------------------------------------------------
# all — build & push cả 2 images
# -----------------------------------------------------------------------
all:
    BUILD +serve
    BUILD +mock-service

# -----------------------------------------------------------------------
# multi — build & push cả 2 platform dưới MỘT tag (Earthly 0.8.x: BUILD --platform)
# -----------------------------------------------------------------------
multi:
    BUILD --platform linux/amd64 --platform linux/arm64 +all

# -----------------------------------------------------------------------
# integration-images — build 2 images locally (no registry push)
# -----------------------------------------------------------------------
integration-images:
    ARG VERSION=local
    ARG GIT_COMMIT=unknown
    ARG BUILD_DATE=
    BUILD --build-arg VERSION=${VERSION} --build-arg GIT_COMMIT=${GIT_COMMIT} --build-arg BUILD_DATE=${BUILD_DATE} +serve
    BUILD --build-arg VERSION=${VERSION} --build-arg GIT_COMMIT=${GIT_COMMIT} --build-arg BUILD_DATE=${BUILD_DATE} +mock-service

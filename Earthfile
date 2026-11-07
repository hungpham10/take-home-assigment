VERSION 0.8

# -----------------------------------------------------------------------
# Global config — override khi gọi, e.g. `earthly +controller-image --VERSION=v0.1.0`
# Pattern: sources/opsense-rs/Earthfile (multi-stage, prebuilt-binary fast path)
# -----------------------------------------------------------------------
ARG --global REGISTRY=ghcr.io
ARG --global IMAGE_PREFIX=hungpham10/loadtest-platform
ARG --global VERSION=latest

# -----------------------------------------------------------------------
# build — build Go binaries (fast path: binary từ host nếu có, fallback go build)
# -----------------------------------------------------------------------
build:
    FROM golang:1.23-bookworm
    WORKDIR /src
    COPY --if-exists bin/ /src/host-bin/
    COPY . /src/code/
    RUN sh -c '\
      mkdir -p /out && \
      if [ -f /src/host-bin/loadtest-controller ]; then \
        echo "--> Using pre-built host binary"; \
        cp /src/host-bin/loadtest-controller /out/; \
      else \
        echo "--> Building inside Earthly..."; \
        cd /src/code && go build -o /out/loadtest-controller ./cmds/loadtest-controller; \
      fi && \
      chmod +x /out/loadtest-controller && \
      /out/loadtest-controller --help 2>/dev/null; test $? -ne 0 || true'
    SAVE ARTIFACT /out/loadtest-controller AS LOCAL bin/loadtest-controller-earthly

# -----------------------------------------------------------------------
# controller-image — runtime image tối giản (binary + sql migrations + scripts)
# -----------------------------------------------------------------------
controller-image:
    FROM +build
    FROM alpine:3.20
    RUN apk add --no-cache ca-certificates tzdata
    COPY +build/loadtest-controller /app/loadtest-controller
    # sql/ + scripts/ + env/ mount từ compose; embed đã chứa sql/ trong binary
    ENTRYPOINT ["/app/loadtest-controller"]
    SAVE IMAGE --push $REGISTRY/$IMAGE_PREFIX/controller:$VERSION

# -----------------------------------------------------------------------
# test — chạy unit tests trong container (CI-fallback khi runner không có Go)
# -----------------------------------------------------------------------
test:
    FROM golang:1.23-bookworm
    WORKDIR /src
    COPY . .
    RUN go test ./... -race

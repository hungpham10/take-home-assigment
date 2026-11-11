FROM debian:bookworm-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates wget && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
# Version is injected into the binary by `make build` (ldflags, plans/38);
# the ARGs keep the build log self-describing.
ARG VERSION=local
ARG GIT_COMMIT=unknown
COPY bin/mock-service /app/mock-service
RUN chmod +x /app/mock-service
RUN /app/mock-service --version
ENV ADDR=:8090
EXPOSE 8090
ENTRYPOINT ["/app/mock-service"]

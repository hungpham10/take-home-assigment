FROM debian:bookworm-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates wget && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
ARG VERSION=local
COPY bin/mock-service /app/mock-service
RUN chmod +x /app/mock-service
ENV ADDR=:8090
EXPOSE 8090
ENTRYPOINT ["/app/mock-service"]

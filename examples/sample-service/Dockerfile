# Sample service image (plans/43).
# Build context is the REPOSITORY ROOT because the Go module lives there:
#   docker build -f examples/sample-service/Dockerfile -t ltp/sample-service:dev .
# The service itself imports only the standard library, so the copied tree stays
# tiny (no pkgs/, no sources/).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY examples/sample-service ./examples/sample-service
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/sample-service ./examples/sample-service

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/sample-service /usr/local/bin/sample-service
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sample-service"]

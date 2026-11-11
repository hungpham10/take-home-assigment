.PHONY: test lint build fmt tidy clean version version-check k8s-render test-integration test-integration-down up down logs

EARTHLY ?= earthly
COMPOSE ?= docker-compose
LTP_TAG ?= local

# ---------------------------------------------------------------------
# Version (docs/versioning.md) — git tag is the single source of truth.
# `git describe` yields "0.1.1" on a tagged commit and "0.1.1-3-gabc1234"
# after it. A tree without .git fall back to "dev" so the binary never
# claims a release it was not built for.
# ---------------------------------------------------------------------
MODULE     := github.com/hungpham/loadtest-platform
# Single source of truth for the resolution rules: scripts/bump.sh --print
# (tag → "0.1.1", after tag → "0.1.1-3-gabc1234", no tag/no git → "dev").
VERSION    ?= $(shell ./scripts/bump.sh --print 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X $(MODULE)/pkgs/version.Version=$(VERSION) \
           -X $(MODULE)/pkgs/version.Commit=$(GIT_COMMIT) \
           -X $(MODULE)/pkgs/version.BuildDate=$(BUILD_DATE)

test:
	go test ./... -race

lint-transitions:
	@./scripts/lint-transitions.sh

lint:
	golangci-lint run

version:
	@echo $(VERSION)

# version-check — CI gate: bump logic + binary banner must agree with git.
version-check:
	@./scripts/bump_test.sh
	@go test ./pkgs/version/... -count=1
	@echo ">>> version: $(VERSION) (commit $(GIT_COMMIT))"

build:
	go build -ldflags "$(LDFLAGS)" -o bin/loadtest-controller ./cmds/loadtest-controller
	go build -ldflags "$(LDFLAGS)" -o bin/mock-service ./cmds/mock-service

# k8s-render — render an overlay with the real image tag (default: release).
#   make k8s-render                      # overlay release, tag = latest git tag
#   make k8s-render OVERLAY=kind K8S_VERSION=local
k8s-render:
	@./scripts/k8s-render.sh $(or $(OVERLAY),release) $(K8S_VERSION)

fmt:
	go fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/

# ---------------------------------------------------------------------
# Integration stack (plans/34 — pattern opsense-rs)
# ---------------------------------------------------------------------
integration-images:
	$(EARTHLY) --build-arg VERSION=$(LTP_TAG) \
	           --build-arg GIT_COMMIT=$(GIT_COMMIT) \
	           --build-arg BUILD_DATE=$(BUILD_DATE) +integration-images

up:
	@# macOS colima không mount được ~/Desktop (TCC) → stage 2 file bind-mount sang ~/.ltp-run
	@mkdir -p $(HOME)/.ltp-run && cp conf/dex/config.dev.yaml $(HOME)/.ltp-run/ && cp env/secrets.dev.enc.yaml $(HOME)/.ltp-run/
	LTP_DEX_CONFIG=$(HOME)/.ltp-run/config.dev.yaml \
	LTP_SECRETS=$(HOME)/.ltp-run/secrets.dev.enc.yaml \
	LTP_TAG=$(LTP_TAG) \
	$(COMPOSE) up -d --wait --wait-timeout 180

down:
	$(COMPOSE) down

down-v:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f --tail=100

# test-integration — build images local → compose up --wait → go test
# (tag `integration`). Suite tự skip khi stack không reachable, trừ khi
# LTP_INTEGRATION=1 (CI) → fail hard.
test-integration:
	@echo ">>> Building images (serve + mock-service) via Earthly"
	$(MAKE) integration-images
	@echo ">>> Compose up + wait for healthy"
	@$(MAKE) up
	@echo ">>> Run integration suite (nginx gateway + Dex + valkey + postgres)"
	LTP_INTEGRATION=1 \
	LTP_BASE_URL=http://127.0.0.1:8080 \
	LTP_DEX_ISSUER=http://127.0.0.1:5556/dex \
	LTP_PG_DSN=postgres://ltp:ltp@127.0.0.1:5432/ltp?sslmode=disable \
	go test -tags integration ./tests/integration/... -count=1 -v

test-integration-down:
	$(COMPOSE) down -v


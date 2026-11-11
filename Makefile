.PHONY: test lint build fmt tidy clean test-integration test-integration-down up down logs

EARTHLY ?= earthly
COMPOSE ?= docker-compose
LTP_TAG ?= local

test:
	go test ./... -race

lint-transitions:
	@./scripts/lint-transitions.sh

lint:
	golangci-lint run

build:
	go build -o bin/loadtest-controller ./cmds/loadtest-controller
	go build -o bin/mock-service ./cmds/mock-service

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
	$(EARTHLY) --build-arg VERSION=$(LTP_TAG) +integration-images

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


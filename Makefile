.PHONY: help up down proto proto-gen proto-lint proto-breaking proto-roundtrip sdk-go-test sdk-py-test sink-gcs-test sink-gcs-build-local sink-gcs-build lint test clean gateway-build gateway-build-local gateway-test gateway-migrate

COMPOSE := docker compose -f deploy/docker-compose.yml
BUILD_VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' | sort

up: ## Start local infra (NATS + Postgres + MinIO)
	$(COMPOSE) up -d

down: ## Stop local infra
	$(COMPOSE) down

proto: ## Run buf generate (outputs to proto/gen/)
	buf generate

proto-gen: proto ## Regenerate proto + channel-type codegen; CI diffs must be clean
	go run ./tools/genchanneltypes/
	@echo "==> Checking for codegen drift..."
	git diff --exit-code sdk-go/channeltypes.go sdk-py/mio/channeltypes.py || \
		(echo "ERROR: channeltypes drift detected — commit the generated files"; exit 1)

sdk-go-test: ## Run sdk-go unit tests (no live NATS needed)
	cd sdk-go && go test ./... -v

sdk-py-test: ## Run sdk-py unit tests (no live NATS needed)
	cd sdk-py && uv run pytest tests/ -v -m "not integration"

proto-lint: ## Run buf lint (STANDARD ruleset)
	buf lint

proto-breaking: ## Run buf breaking check against main branch (WIRE_JSON ruleset)
	buf breaking --against ".git#branch=main"

proto-roundtrip: ## Run Go+Python proto round-trip test (pipes bytes; both must print OK)
	@echo "==> Go half: marshal + subject-token validator"
	go run ./tools/proto-roundtrip/

lint: ## Run buf lint + go vet
	buf lint
	go vet ./...

test: ## Run go tests
	go test ./...

clean: ## Remove generated proto output and stop infra (wipes volumes)
	rm -rf proto/gen
	$(COMPOSE) down -v

gateway-build-local: ## Build gateway Docker image locally (no push)
	docker build -f gateway/Dockerfile -t mio/gateway:dev .

gateway-build: ## Build gateway Docker image with version tag (no push)
	docker build -f gateway/Dockerfile \
		--build-arg BUILD_VERSION=$(BUILD_VERSION) \
		-t mio/gateway:$(BUILD_VERSION) .

gateway-test: ## Run gateway unit tests (no live NATS/Postgres needed)
	cd gateway && go test ./internal/... -v -count=1

gateway-migrate: ## Run database migrations manually via gateway CLI
	cd gateway && MIO_MIGRATE_ON_START=true go run ./cmd/gateway/

sink-gcs-test: ## Run sink-gcs unit tests (no live NATS/MinIO needed)
	cd sink-gcs && go test ./internal/... -v

sink-gcs-build-local: ## Build sink-gcs Docker image locally (no push)
	docker build -f sink-gcs/Dockerfile -t mio/sink-gcs:dev .

sink-gcs-build: ## Build sink-gcs Docker image with version tag (no push)
	docker build -f sink-gcs/Dockerfile \
		--build-arg BUILD_VERSION=$(BUILD_VERSION) \
		-t mio/sink-gcs:$(BUILD_VERSION) .

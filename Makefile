.PHONY: help up down proto proto-lint proto-breaking proto-roundtrip lint test clean gateway-build gateway-build-local

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

BINARY      := proxy
PKG         := ./cmd/proxy
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the proxy binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

.PHONY: run
run: ## Run the proxy locally (no Redis needed: REDIS_ADDR=disabled)
	REDIS_ADDR=$${REDIS_ADDR:-disabled} LOG_LEVEL=debug go run $(PKG)

.PHONY: devredis
devredis: ## Run an in-process Redis for local dev (no Docker, no install)
	go run ./cmd/devredis

.PHONY: dev
dev: ## Run the proxy against the dev Redis (start `make devredis` first)
	REDIS_ADDR=127.0.0.1:6379 LOG_LEVEL=debug go run ./cmd/proxy

.PHONY: test
test: ## Run the full test suite
	go test ./... -count=1

.PHONY: test-verbose
test-verbose: ## Run the test suite with per-test output
	go test ./... -count=1 -v

.PHONY: test-race
test-race: ## Run the tests under the race detector (needs a C toolchain)
	CGO_ENABLED=1 go test ./... -count=1 -race

.PHONY: test-redis
test-redis: ## Run the Redis tests against a real server instead of miniredis
	TEST_REDIS_ADDR=$${TEST_REDIS_ADDR:-localhost:6379} go test ./internal/limiter/ -count=1 -v

.PHONY: cover
cover: ## Produce coverage.html
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: bench
bench: ## Run the benchmarks
	go test ./internal/proxy/ -run '^$$' -bench . -benchmem -benchtime 2s

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format the tree
	gofmt -w .

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: up
up: ## Start Redis + proxy with docker compose
	docker compose up --build -d
	@echo "dashboard: http://localhost:8080/_rl/dashboard"

.PHONY: cluster
cluster: ## Start two proxy instances sharing one Redis
	docker compose --profile cluster up --build -d
	@echo "instance A: http://localhost:8080/_rl/dashboard"
	@echo "instance B: http://localhost:8081/_rl/dashboard"

.PHONY: down
down: ## Stop the compose stack
	docker compose down -v

.PHONY: logs
logs: ## Tail the proxy logs
	docker compose logs -f proxy

.PHONY: load
load: ## Run the load script against a running proxy
	./scripts/test_load.sh

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out coverage.html

.DEFAULT_GOAL := help
BIN := bin/ledgerd

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the ledgerd binary
	go build -trimpath -o $(BIN) ./cmd/ledgerd

.PHONY: run
run: ## Run ledgerd locally
	go run ./cmd/ledgerd

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open a coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format code
	gofmt -s -w .

.PHONY: up
up: ## Start local dependencies (Postgres)
	docker compose up -d

.PHONY: down
down: ## Stop local dependencies
	docker compose down

.PHONY: help build test bench fmt lint vet tidy clean run docker-build snapshot check

BINARY_NAME := arc-mcp
GO          := go
MAIN_PATH   := ./cmd/arc-mcp

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary to ./$(BINARY_NAME)
	$(GO) build -o $(BINARY_NAME) $(MAIN_PATH)

run: ## Run arc-mcp with ARC_URL/ARC_TOKEN from env
	$(GO) run $(MAIN_PATH)

test: ## Run all tests with race detector
	$(GO) test -race -coverprofile=coverage.out ./...

bench: ## Run benchmarks
	$(GO) test -bench=. -benchmem ./...

fmt: ## Format Go code
	$(GO) fmt ./...
	gofmt -s -w .

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint (requires install)
	golangci-lint run

tidy: ## Tidy go.mod
	$(GO) mod tidy

clean: ## Remove build artifacts
	rm -f $(BINARY_NAME) coverage.out

check: ## goreleaser config sanity check
	goreleaser check

snapshot: ## goreleaser local snapshot build (no publish)
	goreleaser release --clean --snapshot --skip=sign,publish

docker-build: ## Build the arc-mcp Docker image locally
	docker build -t arc-mcp:local .

.DEFAULT_GOAL := help

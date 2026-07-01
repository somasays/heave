# Gateway developer commands. `make check` is the one gate; the pre-commit hook
# and CI both call the same script so there is a single source of truth.

.DEFAULT_GOAL := check

.PHONY: check
check: ## Run the full fail-fast verification (fmt, arch, vet, build, test)
	@bash scripts/check.sh

.PHONY: fmt
fmt: ## Auto-format all Go code
	@gofmt -w .

.PHONY: build
build: ## Build the gateway binary into ./bin
	@go build -o bin/gateway ./cmd/gateway

.PHONY: run
run: build ## Build and run against config.yaml
	@./bin/gateway -config config.yaml

.PHONY: test
test: ## Run tests only
	@go test ./...

.PHONY: hooks
hooks: ## Install the versioned git hooks (pre-commit + commit-msg)
	@git config core.hooksPath .githooks
	@chmod +x .githooks/* scripts/*.sh
	@echo "git hooks installed (core.hooksPath=.githooks)"

.PHONY: help
help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

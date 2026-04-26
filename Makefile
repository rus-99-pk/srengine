# Переменные по умолчанию, если они не заданы в окружении
export KUBECONFIG ?= ~/.kube/config
export NAMESPACES ?= default,production
export OLLAMA_URL ?= http://localhost:11434

.PHONY: help run run-kind run-kind-full cluster-reset cluster-reset-full cluster-down test test-scenario build clean

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

run: ## Run agent locally against the current kubeconfig context
	./scripts/start.sh local

run-kind: ## Start a local kind cluster, create namespaces, and run the agent
	./scripts/start.sh kind

run-kind-full: ## Start kind cluster, install Prometheus, and run the agent
	./scripts/start.sh kind-full

cluster-reset: ## Delete and recreate the kind cluster, then run the agent
	./scripts/start.sh kind-reset

cluster-reset-full: ## Delete and recreate the kind cluster with Prometheus, then run the agent
	./scripts/start.sh kind-reset-full

cluster-down: ## Delete the local kind cluster
	./scripts/start.sh kind-down

test: ## Run all integration tests
	./scripts/test.sh

test-scenario: ## Run a specific integration test (e.g., make test-scenario SCENARIO=test-high-memory)
	@if [ -z "$(SCENARIO)" ]; then \
		echo "Error: SCENARIO is not set. Usage: make test-scenario SCENARIO=test-high-memory"; \
		exit 1; \
	fi
	./scripts/test.sh $(SCENARIO)

sync-prompt:
	cp internal/agent/prompt.txt helm/srengine/files/prompt.txt

build: ## Build the SREngine binary
	go build -o bin/agent ./cmd/agent

clean: ## Clean go test cache and binaries
	go clean -testcache
	rm -rf bin/
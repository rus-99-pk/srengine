BINARY   := srengine
IMAGE    := ghcr.io/rus-99-pk/srengine
TAG      := latest

.PHONY: build run test test-integration lint docker-build helm-lint

build:
	go build -o $(BINARY) ./cmd/agent

run:
	go run ./cmd/agent

test:
	go test ./internal/...

test-integration:
	INTEGRATION=1 go test ./tests/integration/... -v -timeout=300s

lint:
	golangci-lint run ./...

docker-build:
	docker build -t $(IMAGE):$(TAG) .

helm-lint:
	helm lint ./helm/srengine

kind-setup:
	kind create cluster --name srengine-test
	kubectl apply -f tests/scenarios/
	@echo "Waiting for pods to start failing..."
	sleep 30

kind-teardown:
	kind delete cluster --name srengine-test

.PHONY: help build test test-race cover lint vet fmt tidy docker-build e2e clean

BINARY ?= bin/harvester-antiaffinity
IMAGE ?= ghcr.io/jaevans/harvester-antiaffinity
TAG ?= dev
GO ?= go

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the binary
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/harvester-antiaffinity

test: ## Run unit tests with the race detector
	$(GO) test -race ./...

cover: ## Run unit tests with coverage report
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format code
	$(GO) fmt ./...

tidy: ## Tidy go modules
	$(GO) mod tidy

docker-build: ## Build the container image
	docker build --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .

e2e: ## Run the kind + KubeVirt end-to-end test (KEEP_CLUSTER=1 to keep it)
	./hack/e2e.sh

clean: ## Remove build artifacts
	rm -rf bin dist coverage.out .e2e

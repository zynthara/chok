.DEFAULT_GOAL := all

# ==============================================================================
# Variables
# ==============================================================================

ROOT_DIR   := $(shell pwd)
OUTPUT_DIR := $(ROOT_DIR)/_output
BIN_DIR    := $(OUTPUT_DIR)/bin

GO         := go
GOFLAGS    := -trimpath

# Version metadata. `git describe` falls back to "dev" outside a git
# checkout. The same VERSION/GIT_HASH/BUILD_TIME variables are read by
# version.go via ldflags — keep the path in sync with version package.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_HASH   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS    := -s -w \
	-X github.com/zynthara/chok/version.version=$(VERSION) \
	-X github.com/zynthara/chok/version.gitHash=$(GIT_HASH) \
	-X github.com/zynthara/chok/version.buildTime=$(BUILD_TIME)

# ==============================================================================
# Targets
# ==============================================================================

all: tidy lint test build ## tidy + lint + test + build

##@ Build

.PHONY: build
build: ## Build the chok CLI into _output/bin/chok
	@echo "==> Building chok ($(VERSION))..."
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/chok ./cmd/chok
	@echo "==> Binary: $(BIN_DIR)/chok"

.PHONY: install
install: ## Install the chok CLI into GOPATH/bin
	@echo "==> Installing chok ($(VERSION)) to $$(go env GOPATH)/bin..."
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' ./cmd/chok

##@ Test

.PHONY: test
test: ## Run the full unit test suite with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## Generate a coverage report at _output/coverage.html
	@mkdir -p $(OUTPUT_DIR)
	$(GO) test -race -coverprofile=$(OUTPUT_DIR)/coverage.out ./...
	$(GO) tool cover -html=$(OUTPUT_DIR)/coverage.out -o $(OUTPUT_DIR)/coverage.html
	@echo "==> Coverage report: $(OUTPUT_DIR)/coverage.html"

.PHONY: smoke
smoke: build ## Boot examples/blog as a self-check
	@echo "==> Smoke testing examples/blog..."
	@( cd examples/blog && $(GO) run ./cmd/blog & \
	   PID=$$!; sleep 3; kill $$PID 2>/dev/null; wait $$PID 2>/dev/null; \
	   echo "==> blog start-up smoke OK" )

##@ Lint & Format

.PHONY: lint
lint: ## Run golangci-lint over the whole tree
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed; skipping" && exit 0)
	golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format code (gofmt + go mod tidy)
	gofmt -s -w .
	$(GO) mod tidy

##@ Release

.PHONY: snapshot
snapshot: ## Run goreleaser locally without publishing (output under dist/)
	@which goreleaser > /dev/null || (echo "goreleaser not installed: brew install goreleaser" && exit 1)
	goreleaser release --snapshot --clean --skip=publish

.PHONY: tag
tag: ## Print the release-please workflow
	@echo "Release workflow:"
	@echo "  1. push to main; release-please opens / updates the release PR"
	@echo "  2. review + merge the PR; the merge creates the git tag"
	@echo "  3. the tag push triggers goreleaser to build and publish a GitHub Release"

##@ Clean

.PHONY: clean
clean: ## Remove build outputs
	@echo "==> Cleaning..."
	@rm -rf $(OUTPUT_DIR) dist/

##@ Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

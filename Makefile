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
	-X github.com/zynthara/chok/v2/version.version=$(VERSION) \
	-X github.com/zynthara/chok/v2/version.gitHash=$(GIT_HASH) \
	-X github.com/zynthara/chok/v2/version.buildTime=$(BUILD_TIME)

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

.PHONY: test-pg
test-pg: ## Run the store/db packages against Postgres (set CHOK_TEST_PG_DSN)
	CHOK_TEST_DRIVER=postgres $(GO) test -race -count=1 ./store/... ./db/...

.PHONY: test-mysql
test-mysql: ## Run the migration partial-DDL test against MySQL (set CHOK_TEST_MYSQL_DSN)
	$(GO) test -race -count=1 ./db -run TestApplyMigrations_MySQLPartialDDL

.PHONY: cover
cover: ## Generate a coverage report at _output/coverage.html
	@mkdir -p $(OUTPUT_DIR)
	$(GO) test -race -coverprofile=$(OUTPUT_DIR)/coverage.out ./...
	$(GO) tool cover -html=$(OUTPUT_DIR)/coverage.out -o $(OUTPUT_DIR)/coverage.html
	@echo "==> Coverage report: $(OUTPUT_DIR)/coverage.html"

# Build first, then signal the binary directly: `go run ... & kill -INT $$!`
# signals the go-run wrapper and orphans the app (M4 verification bite).
.PHONY: smoke
smoke: ## Boot examples/blog briefly as a self-check
	@echo "==> Smoke testing examples/blog..."
	@mkdir -p $(BIN_DIR)
	@$(GO) build $(GOFLAGS) -o $(BIN_DIR)/blog-smoke ./examples/blog
	@( BLOG_CONFIG=examples/blog/chok.yaml \
	   BLOG_HTTP_ADDR=127.0.0.1:18080 \
	   BLOG_HTTP_DRAIN_DELAY=100ms \
	   BLOG_DB_SQLITE_PATH=$(OUTPUT_DIR)/blog-smoke.db \
	   $(BIN_DIR)/blog-smoke & \
	   PID=$$!; ok=0; \
	   for i in $$(seq 1 30); do \
	     curl -sf -m 1 http://127.0.0.1:18080/healthz >/dev/null 2>&1 && { ok=1; break; }; \
	     sleep 0.5; \
	   done; \
	   kill -INT $$PID 2>/dev/null; wait $$PID; rc=$$?; \
	   rm -f $(OUTPUT_DIR)/blog-smoke.db; \
	   if [ $$ok -eq 1 ] && [ $$rc -eq 0 ]; then \
	     echo "==> blog smoke OK (healthz 200, clean SIGINT exit)"; \
	   else \
	     echo "==> blog smoke FAILED (healthz_ok=$$ok exit=$$rc)"; exit 1; \
	   fi )

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
	@which syft > /dev/null || (echo "syft not installed (SBOM generation): brew install syft" && exit 1)
	goreleaser release --snapshot --clean --skip=publish

.PHONY: tag
tag: ## Print the manual release workflow
	@echo "Release workflow (manual since v2) — canonical runbook: CONTRIBUTING.md, 'Releasing':"
	@echo "  1. full suite green: make test && go vet ./..."
	@echo "  2. one release commit 'chore(release): vX.Y.Z — <punchline>': promote the"
	@echo "     CHANGELOG.md Unreleased entry, add the docs/changelog.md note, bump .apidiff-baseline"
	@echo "  3. git tag vX.Y.Z[-pre.N] && git push origin main vX.Y.Z"
	@echo "  4. the tag push triggers goreleaser (binaries + SBOMs + checksums -> GitHub Release)"

##@ Clean

.PHONY: clean
clean: ## Remove build outputs
	@echo "==> Cleaning..."
	@rm -rf $(OUTPUT_DIR) dist/

##@ Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

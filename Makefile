# chok — the gates every change must pass. CI runs `make verify` and
# nothing else, so a green local `make verify` predicts a green CI run.
#
# The database lanes read their DSNs from the environment; this Makefile
# never loads .env on its own:
#
#   set -a; source .env; set +a
#   make verify            # or just: make test-pg test-mysql

.DEFAULT_GOAL := help
.NOTPARALLEL:

GO            ?= go
GOFMT         ?= gofmt
GOLANGCI_LINT ?= golangci-lint
# Keep in step with the golangci-lint-action pin in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := 2.15.0

# Packages that open a real database rerun on Postgres and MySQL. A
# directory joins both lanes as soon as it exists (db: WP4, store and
# choktest: WP5, identity: WP6, jobs: WP7, telemetry: WP8, for its
# Instrument tests; cmd/chok: WP9, whose scaffold and migrate tests run
# on the lane's database; examples/chatter: WP10, whose acceptance test
# runs the app on choktest.DB); there is no per-test selection.
DB_PKG_DIRS := db store choktest identity jobs telemetry cmd/chok examples/chatter
DB_PKGS     := $(strip $(foreach d,$(DB_PKG_DIRS),$(if $(wildcard $(d)),./$(d)/...)))

# Tracked plus untracked-but-not-ignored Go files, so ignored scratch
# space (.private/ and friends) never reaches gofmt.
GO_FILES = $(shell git ls-files --cached --others --exclude-standard -- '*.go')

##@ Gates (make verify runs them in this order)

.PHONY: verify
verify: tidy-check fmt-check vet lint fix-check test test-pg test-mysql budget smoke ## Run every gate below
	@echo "==> verify: all gates passed"

.PHONY: tidy-check
tidy-check: ## go.mod and go.sum are tidy (go mod tidy -diff is empty)
	$(GO) mod tidy -diff

.PHONY: fmt-check
fmt-check: ## Every Go file is gofmt -s clean
	@files='$(GO_FILES)'; [ -z "$$files" ] && exit 0; \
	out=$$($(GOFMT) -s -l $$files); \
	if [ -n "$$out" ]; then echo "gofmt -s needed (make fmt):"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint (blocking)
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo "golangci-lint not found; install it with:"; \
	  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)"; exit 1; }
	@v=$$($(GOLANGCI_LINT) version --short 2>/dev/null); [ "$$v" = "$(GOLANGCI_LINT_VERSION)" ] || \
	  echo "warning: golangci-lint $$v here, CI pins $(GOLANGCI_LINT_VERSION)"
	$(GOLANGCI_LINT) run ./...

.PHONY: fix-check
fix-check: ## go fix has nothing to rewrite (go fix -diff is empty)
	@out=$$($(GO) fix -diff ./... 2>&1); rc=$$?; \
	if [ $$rc -ne 0 ] || [ -n "$$out" ]; then echo "go fix -diff is not empty (go fix ./...):"; echo "$$out"; exit 1; fi

# The SQLite lane is hermetic: the driver and both DSNs are blanked so an
# exported .env cannot pull Postgres or MySQL into it.
.PHONY: test
test: ## Full suite on SQLite with the race detector
	CHOK_TEST_DRIVER= CHOK_TEST_PG_DSN= CHOK_TEST_MYSQL_DSN= $(GO) test -race -count=1 ./...

.PHONY: test-pg
test-pg: ## DB-backed packages on Postgres (needs CHOK_TEST_PG_DSN)
	@[ -n "$$CHOK_TEST_PG_DSN" ] || { echo "test-pg: CHOK_TEST_PG_DSN is not set (set -a; source .env; set +a)"; exit 1; }
	@if [ -z "$(DB_PKGS)" ]; then echo "test-pg: no DB-backed packages yet ($(DB_PKG_DIRS)); nothing to run"; \
	else echo "CHOK_TEST_DRIVER=postgres $(GO) test -race -count=1 $(DB_PKGS)"; \
	  CHOK_TEST_DRIVER=postgres $(GO) test -race -count=1 $(DB_PKGS); fi

.PHONY: test-mysql
test-mysql: ## DB-backed packages on MySQL (needs CHOK_TEST_MYSQL_DSN)
	@[ -n "$$CHOK_TEST_MYSQL_DSN" ] || { echo "test-mysql: CHOK_TEST_MYSQL_DSN is not set (set -a; source .env; set +a)"; exit 1; }
	@if [ -z "$(DB_PKGS)" ]; then echo "test-mysql: no DB-backed packages yet ($(DB_PKG_DIRS)); nothing to run"; \
	else echo "CHOK_TEST_DRIVER=mysql $(GO) test -race -count=1 $(DB_PKGS)"; \
	  CHOK_TEST_DRIVER=mysql $(GO) test -race -count=1 $(DB_PKGS); fi

.PHONY: budget
budget: ## API surface and config key budgets (limits in hack/budget.txt)
	@bash hack/apibudget.sh
	@echo
	@bash hack/configkeys.sh

.PHONY: smoke
smoke: ## Boot examples/blog on a random port, probe /healthz, SIGINT it, expect exit 0 within 3s
	@bash hack/smoke.sh

##@ Helpers

.PHONY: apidiff
apidiff: ## Incompatible public API changes since the last release (advisory in CI while chok is at v0)
	@bash hack/apidiff.sh

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## gofmt -s -w every Go file
	@files='$(GO_FILES)'; [ -z "$$files" ] || $(GOFMT) -s -w $$files

.PHONY: help
help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } \
	  /^[a-zA-Z_-]+:.*## / { printf "  \033[36m%-11s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

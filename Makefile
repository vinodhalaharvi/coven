# coven — multi-agent Go package monitor
#
# Standard targets. Run `make help` for a summary.

GO          ?= go
GOFLAGS     ?=
RACE        ?= -race
TIMEOUT     ?= 360s
BIN_DIR     ?= bin
DEMO_BIN    := $(BIN_DIR)/coven

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the demo binary.
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(DEMO_BIN) ./cmd/demo
	@echo "built $(DEMO_BIN)"

.PHONY: test
test: ## Run the full test suite with -race.
	$(GO) test $(GOFLAGS) $(RACE) -timeout $(TIMEOUT) ./...

.PHONY: test-short
test-short: ## Run tests excluding the long integration tests.
	$(GO) test $(GOFLAGS) $(RACE) -timeout 60s -short $(shell $(GO) list ./... | grep -v /integration)

.PHONY: test-count
test-count: ## Print test counts per package.
	@total=0; \
	for pkg in $$($(GO) list ./...); do \
		count=$$($(GO) test -list '.*' "$$pkg" 2>/dev/null | grep -c '^Test'); \
		printf "  %-58s %3d\n" "$$pkg" "$$count"; \
		total=$$((total + count)); \
	done; \
	printf "\n  %-58s %3d\n" "TOTAL" "$$total"

.PHONY: vet
vet: ## Run go vet on all packages.
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go files.
	$(GO) fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum.
	$(GO) mod tidy

.PHONY: check
check: vet test ## Run vet + the full test suite.

.PHONY: race
race: ## Run tests under -race three times to check for flakes.
	$(GO) test $(RACE) -count=3 -timeout 600s ./...

.PHONY: demo
demo: build ## Build and run the demo against testdata/miniproject.
	$(DEMO_BIN) -root testdata/miniproject -tick 300ms -debounce 100ms

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR)

.PHONY: install
install: ## Install the demo binary to GOBIN.
	$(GO) install ./cmd/demo

.PHONY: lines
lines: ## Show line counts for source vs tests.
	@echo "source (excl tests, testdata):"
	@find . -name '*.go' -not -name '*_test.go' -not -path './testdata/*' | xargs wc -l | tail -1
	@echo "tests:"
	@find . -name '*_test.go' -not -path './testdata/*' | xargs wc -l | tail -1

.PHONY: ci
ci: vet test ## Target run by CI.

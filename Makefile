# AegiSheet build tasks.
#
# Everything here uses only the Go toolchain, so a clean checkout with Go
# installed can build, test and vet without any further setup.

BINARY      := aegisheet
CMD         := ./cmd/aegisheet
BIN_DIR     := bin

# Version metadata is injected at link time so that every log line and report can
# be traced back to an exact build. Falls back to the Go module metadata when the
# checkout has no tags.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG         := github.com/Redpatronus/AegiSheet/internal/buildinfo
LDFLAGS     := -s -w \
	-X $(PKG).version=$(VERSION) \
	-X $(PKG).commit=$(COMMIT) \
	-X $(PKG).date=$(BUILD_DATE)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "built $(BIN_DIR)/$(BINARY) $(VERSION)"

.PHONY: install
install: ## Install the binary into GOPATH/bin
	go install -trimpath -ldflags '$(LDFLAGS)' $(CMD)

.PHONY: test
test: ## Run the tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run the tests and open a coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format the source
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt -s -w:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy: ## Tidy and verify the module graph
	go mod tidy
	go mod verify

.PHONY: check
check: fmt-check vet test ## Everything CI runs

.PHONY: run
run: build ## Build, then run against conf/xls_report.ini
	$(BIN_DIR)/$(BINARY) -config conf/xls_report.ini

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BIN_DIR) coverage.out

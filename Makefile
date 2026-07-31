# AegiSheet build tasks.
#
# Everything here uses only the Go toolchain, so a clean checkout with Go
# installed can build, test and vet without any further setup.

BINARY      := aegisheet
CMD         := ./cmd/aegisheet
BIN_DIR     := bin
IMAGE       := aegisheet

# Tool versions, kept in step with .github/workflows/go.yml so that a local run
# and a CI run reach the same verdict. `go run tool@version` needs no bootstrap
# step and no global installation.
GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
GOSEC         := github.com/securego/gosec/v2/cmd/gosec@v2.28.0
GOVULNCHECK   := golang.org/x/vuln/cmd/govulncheck@v1.6.0
TRIVY_IMAGE   := aquasec/trivy:0.72.0

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

.PHONY: lint
lint: ## Run golangci-lint with the repository configuration
	go run $(GOLANGCI_LINT) run --timeout=5m

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply the fixes it can make safely
	go run $(GOLANGCI_LINT) run --timeout=5m --fix

.PHONY: vuln
vuln: ## Check dependencies against the Go vulnerability database
	go run $(GOVULNCHECK) ./...

.PHONY: gosec
gosec: ## Run the gosec security scanner
	go run $(GOSEC) -quiet -fmt=text ./...

.PHONY: security
security: vuln gosec ## Every source-level security check

.PHONY: check
check: fmt-check vet lint test security ## Everything CI runs, except the image build

.PHONY: docker
docker: ## Build the container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
	@echo "built $(IMAGE):$(VERSION)"

.PHONY: docker-scan
docker-scan: docker ## Build the image, then scan it for known vulnerabilities
	@# The image is exported and scanned from a tarball rather than through the
	@# Docker socket. Mounting the socket into the scanner would hand a
	@# third-party container root-equivalent control of the host daemon, which is
	@# a steep price for reading one image.
	@mkdir -p $(BIN_DIR) "$$HOME/.cache/trivy"
	docker save $(IMAGE):$(VERSION) -o $(BIN_DIR)/image.tar
	docker run --rm \
		-v "$(CURDIR)/$(BIN_DIR):/scan:ro" \
		-v "$$HOME/.cache/trivy:/root/.cache/trivy" \
		$(TRIVY_IMAGE) image \
			--input /scan/image.tar \
			--exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL
	@rm -f $(BIN_DIR)/image.tar

.PHONY: docker-smoke
docker-smoke: docker ## Build the image and verify it starts and stays unprivileged
	docker run --rm $(IMAGE):$(VERSION) -version
	@user=$$(docker image inspect $(IMAGE):$(VERSION) --format '{{.Config.User}}'); \
	if [ "$$user" != "65532:65532" ]; then \
		echo "the image runs as '$$user', expected the unprivileged 65532:65532"; exit 1; \
	fi
	@echo "image runs as an unprivileged user"

.PHONY: run
run: build ## Build, then run against conf/xls_report.ini
	$(BIN_DIR)/$(BINARY) -config conf/xls_report.ini

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BIN_DIR) coverage.out gosec.sarif

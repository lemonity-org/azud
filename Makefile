# Azud Makefile

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go parameters
GOCMD = go
GOBUILD = $(GOCMD) build
GOTEST = $(GOCMD) test
GOGET = $(GOCMD) get
GOMOD = $(GOCMD) mod
GOFMT = gofmt
GOLINT = golangci-lint

# Build parameters
BINARY_NAME = azud
BUILD_DIR = bin
CMD_DIR = cmd

# LDFLAGS for version injection
LDFLAGS = -ldflags "-X github.com/lemonity-org/azud/pkg/version.Version=$(VERSION) \
	-X github.com/lemonity-org/azud/pkg/version.Commit=$(COMMIT) \
	-X github.com/lemonity-org/azud/pkg/version.BuildDate=$(BUILD_DATE)"

# Platforms for cross-compilation
PLATFORMS = darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all build clean test lint fmt deps help install release verify run dev version security-lint test-coverage

# Default target
all: deps build

## build: Build the azud binary for current platform
build:
	@printf '  BUILD  %s\n' "$(BINARY_NAME)"
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)/$(BINARY_NAME)
	@printf '  OK     %s\n' "$(BUILD_DIR)/$(BINARY_NAME)"

## install: Install azud to GOPATH/bin
install:
	@printf '  BUILD  Install %s\n' "$(BINARY_NAME)"
	$(GOBUILD) $(LDFLAGS) -o $(GOPATH)/bin/$(BINARY_NAME) ./$(CMD_DIR)/$(BINARY_NAME)
	@printf '  OK     %s\n' "$(GOPATH)/bin/$(BINARY_NAME)"

## release: Build for all platforms
release:
	@printf '  BUILD  Release matrix\n'
	@mkdir -p $(BUILD_DIR)/release
	@set -e; for platform in $(PLATFORMS); do \
		GOOS=$${platform%/*} GOARCH=$${platform#*/} \
		$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/release/$(BINARY_NAME)-$${platform%/*}-$${platform#*/}$$([ "$${platform%/*}" = "windows" ] && echo ".exe") \
		./$(CMD_DIR)/$(BINARY_NAME); \
		printf '  OK     $(BINARY_NAME) / %s\n' "$${platform}"; \
	done

## clean: Remove build artifacts
clean:
	@printf '  CLEAN  Build artifacts\n'
	@rm -rf $(BUILD_DIR)
	@rm -f $(BINARY_NAME)
	@printf '  OK     Clean complete\n'

## test: Run tests
test:
	@printf '  TEST   Race and coverage suite\n'
	$(GOTEST) -v -race -cover ./...

## test-coverage: Run tests with coverage report
test-coverage:
	@printf '  TEST   Coverage report\n'
	$(GOTEST) -v -race -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@printf '  OK     coverage.html\n'

## lint: Run linter
lint:
	@printf '  CHECK  golangci-lint\n'
	$(GOLINT) run ./...

## security-lint: Run security linter
security-lint:
	@printf '  CHECK  Security gates\n'
	./scripts/security-lint.sh

## fmt: Format code
fmt:
	@printf '  FMT    Go source\n'
	$(GOFMT) -s -w .

## deps: Download dependencies
deps:
	@printf '  FETCH  Go modules\n'
	$(GOMOD) download
	$(GOMOD) tidy

## verify: Verify dependencies
verify:
	@printf '  CHECK  Go modules\n'
	$(GOMOD) verify

## run: Build and run azud
run: build
	./$(BUILD_DIR)/$(BINARY_NAME) $(ARGS)

## help: Show this help message
help:
	@printf 'AZUD / DEVELOPMENT TARGETS\n'
	@printf '%s\n' '--------------------------------------------------------'
	@printf 'USAGE\n  make <target>\n\nTARGETS\n'
	@grep -E '^## ' Makefile | sed 's/## /  /'

# Development helpers

## dev: Run in development mode with hot reload (requires air)
dev:
	@if command -v air > /dev/null; then \
		air; \
	else \
		printf '  ERROR  air not found\n  INFO   Install: go install github.com/cosmtrek/air@latest\n'; \
		exit 1; \
	fi

## version: Show version information
version:
	@printf 'AZUD / BUILD METADATA\n'
	@printf '%s\n' '--------------------------------------------------------'
	@printf '  VERSION  %s\n' "$(VERSION)"
	@printf '  COMMIT   %s\n' "$(COMMIT)"
	@printf '  BUILT    %s\n' "$(BUILD_DATE)"

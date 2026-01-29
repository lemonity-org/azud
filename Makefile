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
PROXY_BINARY_NAME = azud-proxy
BUILD_DIR = bin
CMD_DIR = cmd

# LDFLAGS for version injection
LDFLAGS = -ldflags "-X github.com/adriancarayol/azud/pkg/version.Version=$(VERSION) \
	-X github.com/adriancarayol/azud/pkg/version.Commit=$(COMMIT) \
	-X github.com/adriancarayol/azud/pkg/version.BuildDate=$(BUILD_DATE)"

# Platforms for cross-compilation
PLATFORMS = darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all build build-all clean test lint fmt deps help install

# Default target
all: deps build

## build: Build the azud binary for current platform
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)/$(BINARY_NAME)
	@echo "Built $(BUILD_DIR)/$(BINARY_NAME)"

## build-proxy: Build the azud-proxy binary
build-proxy:
	@echo "Building $(PROXY_BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(PROXY_BINARY_NAME) ./$(CMD_DIR)/$(PROXY_BINARY_NAME)
	@echo "Built $(BUILD_DIR)/$(PROXY_BINARY_NAME)"

## build-all: Build both azud and azud-proxy
build-all: build build-proxy

## install: Install azud to GOPATH/bin
install:
	@echo "Installing $(BINARY_NAME)..."
	$(GOBUILD) $(LDFLAGS) -o $(GOPATH)/bin/$(BINARY_NAME) ./$(CMD_DIR)/$(BINARY_NAME)
	@echo "Installed to $(GOPATH)/bin/$(BINARY_NAME)"

## release: Build for all platforms
release:
	@echo "Building release binaries..."
	@mkdir -p $(BUILD_DIR)/release
	@for platform in $(PLATFORMS); do \
		GOOS=$${platform%/*} GOARCH=$${platform#*/} \
		$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/release/$(BINARY_NAME)-$${platform%/*}-$${platform#*/}$$([ "$${platform%/*}" = "windows" ] && echo ".exe") \
		./$(CMD_DIR)/$(BINARY_NAME); \
		echo "Built $(BINARY_NAME) for $${platform}"; \
	done

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@rm -f $(BINARY_NAME) $(PROXY_BINARY_NAME)
	@echo "Clean complete"

## test: Run tests
test:
	@echo "Running tests..."
	$(GOTEST) -v -race -cover ./...

## test-coverage: Run tests with coverage report
test-coverage:
	@echo "Running tests with coverage..."
	$(GOTEST) -v -race -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## lint: Run linter
lint:
	@echo "Running linter..."
	$(GOLINT) run ./...

## fmt: Format code
fmt:
	@echo "Formatting code..."
	$(GOFMT) -s -w .

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy

## verify: Verify dependencies
verify:
	@echo "Verifying dependencies..."
	$(GOMOD) verify

## run: Build and run azud
run: build
	./$(BUILD_DIR)/$(BINARY_NAME) $(ARGS)

## docker-build: Build Docker image for azud-proxy
docker-build:
	@echo "Building Docker image..."
	docker build -t azud-proxy:$(VERSION) -f Dockerfile.proxy .

## docker-push: Push Docker image
docker-push:
	@echo "Pushing Docker image..."
	docker push azud-proxy:$(VERSION)

## help: Show this help message
help:
	@echo "Azud - Deploy web apps anywhere"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' Makefile | sed 's/## /  /'

# Development helpers

## dev: Run in development mode with hot reload (requires air)
dev:
	@if command -v air > /dev/null; then \
		air; \
	else \
		echo "air not found. Install with: go install github.com/cosmtrek/air@latest"; \
		exit 1; \
	fi

## version: Show version information
version:
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build Date: $(BUILD_DATE)"

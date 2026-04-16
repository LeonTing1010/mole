.PHONY: all build install clean test run help

# Variables
BINARY_NAME=mole
INSTALL_DIR=/usr/local/bin
GO=go
GOFLAGS=-ldflags="-s -w"

# Default target
all: build

# Build the binary
build:
	@echo "🔨 Building $(BINARY_NAME)..."
	$(GO) build $(GOFLAGS) -o $(BINARY_NAME) .
	@echo "✅ Build complete: $(BINARY_NAME)"

# Install to system
install: build
	@echo "📦 Installing to $(INSTALL_DIR)..."
	@cp $(BINARY_NAME) $(INSTALL_DIR)/
	@chmod +x $(INSTALL_DIR)/$(BINARY_NAME)
	@echo "✅ Installed to $(INSTALL_DIR)/$(BINARY_NAME)"

# Uninstall from system
uninstall:
	@echo "🗑️  Uninstalling..."
	@rm -f $(INSTALL_DIR)/$(BINARY_NAME)
	@echo "✅ Uninstalled"

# Clean build artifacts
clean:
	@echo "🧹 Cleaning..."
	@rm -f $(BINARY_NAME)
	@rm -f /tmp/mole-hiddify-config.json
	@echo "✅ Cleaned"

# Run tests
test:
	@echo "🧪 Running tests..."
	$(GO) test -v ./...

# Run the application
run: build
	./$(BINARY_NAME)

# Development mode with hot reload
dev:
	@echo "👀 Watching for changes..."
	@which air > /dev/null || (echo "Installing air..." && go install github.com/cosmtrek/air@latest)
	air

# Format code
fmt:
	@echo "📝 Formatting code..."
	$(GO) fmt ./...

# Run linter
lint:
	@echo "🔍 Running linter..."
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run

# Download dependencies
deps:
	@echo "📥 Downloading dependencies..."
	$(GO) mod download
	$(GO) mod tidy

# Update dependencies
update:
	@echo "🔄 Updating dependencies..."
	$(GO) get -u ./...
	$(GO) mod tidy

# Show help
help:
	@echo "Available targets:"
	@echo "  make build     - Build the binary"
	@echo "  make install   - Build and install to $(INSTALL_DIR)"
	@echo "  make uninstall - Remove from $(INSTALL_DIR)"
	@echo "  make clean     - Clean build artifacts"
	@echo "  make test      - Run tests"
	@echo "  make run       - Build and run"
	@echo "  make dev       - Run with hot reload"
	@echo "  make fmt       - Format code"
	@echo "  make lint      - Run linter"
	@echo "  make deps      - Download dependencies"
	@echo "  make update    - Update dependencies"
	@echo "  make help      - Show this help"

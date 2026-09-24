.PHONY: all build clean test install

# Build variables
BINARY_DIR=bin
SERVER_BINARY=$(BINARY_DIR)/devboxd
CLIENT_BINARY=$(BINARY_DIR)/devbox

# Go variables
GOFLAGS=-ldflags="-s -w"

all: build

# Build both binaries
build:
	@echo "Building devboxd..."
	@mkdir -p $(BINARY_DIR)
	go build $(GOFLAGS) -o $(SERVER_BINARY) ./cmd/devboxd
	@echo "Building devbox..."
	go build $(GOFLAGS) -o $(CLIENT_BINARY) ./cmd/devbox

# Install binaries to /usr/local/bin
install: build
	@echo "Installing binaries..."
	sudo cp $(SERVER_BINARY) /usr/local/bin/
	sudo cp $(CLIENT_BINARY) /usr/local/bin/
	@echo "✓ Binaries installed to /usr/local/bin/"

# Run tests
test:
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf $(BINARY_DIR)
	find . -name "*.test" -delete

# Run the server (for development)
run-server: build
	./$(SERVER_BINARY) --config devboxd.yaml.example --db devboxd-dev.db

# Generate code (if needed)
generate:
	go generate ./...

# Format code
fmt:
	go fmt ./...

# Lint code (requires golangci-lint)
lint:
	golangci-lint run

# Cross-compile for common platforms
build-all:
	@echo "Cross-compiling for multiple platforms..."
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/devboxd-linux-amd64 ./cmd/devboxd
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/devbox-linux-amd64 ./cmd/devbox
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/devboxd-darwin-amd64 ./cmd/devboxd
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/devbox-darwin-amd64 ./cmd/devbox
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -o $(BINARY_DIR)/devboxd-darwin-arm64 ./cmd/devboxd
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -o $(BINARY_DIR)/devbox-darwin-arm64 ./cmd/devbox
	@echo "✓ Cross-compilation complete"

# Show help
help:
	@echo "Available targets:"
	@echo "  make build      - Build both binaries"
	@echo "  make test       - Run tests"
	@echo "  make install    - Install binaries to /usr/local/bin"
	@echo "  make clean      - Remove build artifacts"
	@echo "  make fmt        - Format code"
	@echo "  make lint       - Run linter"
	@echo "  make build-all  - Cross-compile for multiple platforms"

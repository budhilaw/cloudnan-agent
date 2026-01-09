BINARY_NAME=malangpanel-agent
VERSION?=0.1.0
BUILD_TIME=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}"

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

.PHONY: all build clean test proto deps

all: deps proto build

# Download dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Generate protobuf code
proto:
	@echo "Generating protobuf code..."
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/agent.proto

# Build for current platform
build:
	$(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/agent

# Build for Linux (for deployment)
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-arm64 ./cmd/agent

# Build all platforms
build-all: build-linux
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-arm64 ./cmd/agent

# Run tests
test:
	$(GOTEST) -v ./...

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf bin/

# Install to system (Linux)
install: build-linux
	sudo cp bin/$(BINARY_NAME)-linux-amd64 /usr/local/bin/$(BINARY_NAME)
	sudo chmod +x /usr/local/bin/$(BINARY_NAME)
	sudo mkdir -p /etc/malangpanel
	sudo cp config.example.yaml /etc/malangpanel/agent.yaml

# Install systemd service
install-service:
	sudo cp scripts/malangpanel-agent.service /etc/systemd/system/
	sudo systemctl daemon-reload
	sudo systemctl enable malangpanel-agent

# Start service
start:
	sudo systemctl start malangpanel-agent

# Stop service
stop:
	sudo systemctl stop malangpanel-agent

# View logs
logs:
	sudo journalctl -u malangpanel-agent -f

# Development run
dev:
	$(GOBUILD) $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/agent
	./bin/$(BINARY_NAME) --config config.example.yaml

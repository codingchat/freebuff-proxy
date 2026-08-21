.PHONY: all build build-proxy test test-race lint dev-proxy clean

BINARY_NAME=freebuff-proxy
BIN_DIR=bin

all: build

build-proxy:
	go build -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/freebuff-proxy

build: build-proxy

test:
	env -u AUTH_TOKENS go test ./...

test-race:
	env -u AUTH_TOKENS go test -race ./...

lint:
	go vet ./...
	golangci-lint run ./...

dev-proxy:
	go run ./cmd/freebuff-proxy

clean:
	rm -rf $(BIN_DIR)

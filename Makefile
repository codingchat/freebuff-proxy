.PHONY: all build build-ui build-proxy test test-race lint dev-ui dev-proxy clean

BINARY_NAME=freebuff-proxy
BIN_DIR=bin

all: build

build-ui:
	cd frontend && npm run build

build-proxy:
	go build -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/freebuff-proxy

build: build-ui build-proxy

test:
	env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./...

test-race:
	env -u AUTH_TOKENS -u ADMIN_TOKEN go test -race ./...

lint:
	go vet ./...
	golangci-lint run ./...

dev-ui:
	cd frontend && npm run dev

dev-proxy:
	go run ./cmd/freebuff-proxy

clean:
	rm -rf $(BIN_DIR)

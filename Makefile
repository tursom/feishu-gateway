.PHONY: build test check run
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/feishu-gateway ./cmd/gateway

test:
	go test -race ./...

check:
	go vet ./...

test-short:
	go test ./...

run:
	AUTH_MODE=local HOST=127.0.0.1 PORT=8787 PUBLIC_ORIGIN=http://127.0.0.1:8787 go run ./cmd/gateway

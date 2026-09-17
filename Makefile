.PHONY: build test check run
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/feishu-gateway ./cmd/gateway

test:
	go test -race ./...

check:
	go vet ./...

run:
	go run ./cmd/gateway

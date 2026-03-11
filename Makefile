.PHONY: build test vet

build:
	go build -o bin/astradns-agent ./cmd/agent/

test:
	go test ./...

vet:
	go vet ./...

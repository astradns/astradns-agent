.PHONY: build test vet

build:
	mkdir -p bin
	go build -o bin/astradns-agent ./cmd/agent/

test:
	go test ./...

vet:
	go vet ./...

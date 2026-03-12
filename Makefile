.PHONY: build test vet fmt lint check integration-test e2e-test clean

GO ?= go
BINARY := bin/astradns-agent

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) ./cmd/agent/

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: fmt vet

check: test vet

integration-test:
	$(GO) test -tags=integration ./test/integration/...

e2e-test:
	$(GO) test -tags=e2e ./test/e2e/...

clean:
	rm -rf bin

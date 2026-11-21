VERSION  ?= $(shell git describe --tags --match 'v*' --always)  # binary tags only; sdk-v* tags version the Python SDK
COMMIT   ?= $(shell git rev-parse --short HEAD)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
BIN      := $(HOME)/go/bin/mesh7

.PHONY: build install test lint deadcode clean release-dry

build:
	go build -ldflags "$(LDFLAGS)" -o mesh7 ./cmd/mesh7

install:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mesh7
	@echo "installed $(BIN)"

test:
	go test ./... -race -count=1

lint: deadcode
	@which golangci-lint > /dev/null 2>&1 || echo "install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
	golangci-lint run ./...

# Functions nothing reaches, counting the tests as entry points. Any output fails the target.
deadcode:
	@which deadcode > /dev/null 2>&1 || { echo "install: go install golang.org/x/tools/cmd/deadcode@latest"; exit 1; }
	@out=$$(deadcode -test ./...); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi; echo "deadcode: nothing unreachable"

clean:
	rm -f mesh7
	go clean -cache

release-dry:
	goreleaser release --snapshot --clean

BINARY := kwatch
BIN_DIR := bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test vet fmt lint lint-examples clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) .

install:
	go install -ldflags "$(LDFLAGS)" .

test:
	go test -v -race -count=1 ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "not gofmt'd:"; gofmt -l .; exit 1)

lint: vet fmt

# Validate examples/*.yaml against Kubernetes schemas. Requires kubeconform
# (https://github.com/yannh/kubeconform) — not part of `make lint` since it's an
# external binary, not a Go toolchain dependency.
lint-examples:
	kubeconform -strict -summary examples/*.yaml

clean:
	rm -rf $(BIN_DIR)

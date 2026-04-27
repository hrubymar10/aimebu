BINARY  := aimebu-$(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m)
# Version is taken from `git describe`. Falls back to "v0.0.0-dev" outside a
# tagged commit (or outside a git checkout entirely, e.g. brew's tarball).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install clean run fmt

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/aimebu

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/aimebu

clean:
	rm -f aimebu-*

run: build
	./$(BINARY) server serve

fmt:
	gofmt -s -w .

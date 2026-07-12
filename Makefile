.PHONY: build install install-memory test vet clean

# Default to git describe so locally-built binaries advertise a real version
# string; the updater treats literal "dev" as "skip polling," so a missing
# -ldflags here means Toot never announces new releases. Override with
# `make build VERSION=vX.Y.Z` if you need an explicit tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# build: produce ./otto and ./otto-memory in the repo for local testing.
# Both binaries must be built together: otto-memory is the MCP stdio server
# wired into mcp.json that provides memory_add, memory_replace, session_search,
# and the inter-agent messaging tools; rebuilding only otto leaves the MCP
# server stale.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o ./otto ./cmd/otto
	go build -ldflags "-X main.version=$(VERSION)" -o ./otto-memory ./cmd/otto-memory

# install-memory: deploy otto-memory to ~/.local/bin so the MCP stdio server
# that mcp.json references is consistent with the installed otto binary.
install-memory:
	go build -ldflags "-X main.version=$(VERSION)" -o $(HOME)/.local/bin/otto-memory ./cmd/otto-memory

# install: deploy directly to ~/.local/bin where launchd / systemd run from.
# Use this — NOT `go install` — when iterating, because `go install` silently
# writes to $GOBIN (~/go/bin by default) which is a different file than the one
# the running service uses. install-memory is chained here so both binaries are
# always deployed together; deploying otto alone while leaving an old otto-memory
# can cause subtle protocol mismatches in the MCP tool layer.
install: install-memory
	go build -ldflags "-X main.version=$(VERSION)" -o $(HOME)/.local/bin/otto ./cmd/otto

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

clean:
	rm -f ./otto ./otto-memory coverage.out

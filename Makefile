.PHONY: build install test test-integration vet clean

# Default to git describe so locally-built binaries advertise a real version
# string; the updater treats literal "dev" as "skip polling," so a missing
# -ldflags here means Toot never announces new releases. Override with
# `make build VERSION=vX.Y.Z` if you need an explicit tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# build: produce ./otto in the repo for local testing.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o ./otto ./cmd/otto

# install: deploy directly to ~/.local/bin/otto where launchd / systemd run
# from. Use this — NOT `go install` — when iterating, because `go install`
# silently writes to $GOBIN (~/go/bin by default) which is a different file
# than the one the running service uses.
install:
	go build -ldflags "-X main.version=$(VERSION)" -o $(HOME)/.local/bin/otto ./cmd/otto

test:
	go test ./...

test-integration:
	INTEGRATION=1 go test ./... -run Integration -v

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

clean:
	rm -f ./otto ./google-mcp coverage.out

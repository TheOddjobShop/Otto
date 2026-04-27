.PHONY: build test test-integration vet clean

build:
	go build -o ./otto ./cmd/otto

test:
	go test ./...

test-integration:
	INTEGRATION=1 go test ./... -run Integration -v

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

clean:
	rm -f ./otto ./google-mcp coverage.out

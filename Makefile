.PHONY: all build test test-race cover lint fmt vet run-cli tidy clean

# Default target: format, vet, lint and test.
all: fmt vet lint test

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

# Run the example bot locally in CLI mode (no credentials required).
run-cli:
	go run ./examples/v1 cli

tidy:
	go mod tidy

clean:
	rm -f coverage.txt

.PHONY: all build test test-race cover lint fmt vet vuln run-cli tidy clean

# Default target: format, vet, lint and race-test. The lifecycle code is
# concurrency-heavy, so the pre-commit gate runs the race detector.
all: fmt vet lint test-race

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

# Scan the dependency graph for known vulnerabilities.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Run the example bot locally in CLI mode (no credentials required).
# _examples is its own module, so run it from that directory.
run-cli:
	cd _examples && go run ./v1 cli

tidy:
	go mod tidy

clean:
	rm -f coverage.txt

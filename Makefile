.PHONY: all build test test-race cover lint fmt vet vet-examples vuln run-cli tidy clean

# Default target: format, vet, lint and race-test. The lifecycle code is
# concurrency-heavy, so the pre-commit gate runs the race detector.
all: fmt vet vet-examples lint test-race

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

# golangci-lint's fmt runs every formatter lint enforces (gofmt + goimports),
# so make fmt can never pass while make lint fails on formatting.
fmt:
	golangci-lint fmt ./...

vet:
	go vet ./...

# _examples is its own module, so the root ./... never checks it (CI vets it
# the same way).
vet-examples:
	cd _examples && go vet ./...

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

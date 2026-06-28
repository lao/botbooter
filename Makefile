.PHONY: all build test test-race cover bench soak endurance lint fmt vet run-cli tidy clean

# Default target: format, vet, lint and test.
all: fmt vet lint test

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Dispatch benchmarks with allocation stats. No -race: the detector distorts
# time/op and allocs/op. Use -cpu to see the parallel benchmark scale with cores.
bench:
	go test -bench=. -benchmem -run='^$$' -cpu=1,4,8 ./internal/core/

# Race-focused run of the load/soak/overload tests. These also run in test-race;
# this target isolates them for a quick concurrency check.
soak:
	go test -race -count=1 -run 'Soak|Overload|ConcurrentReads' ./...

# Gated real-platform endurance smokes; skipped unless the BOTBOOTER_{SLACK,DISCORD}_*
# env vars are exported. Timeout must exceed the configured endurance duration.
endurance:
	go test -count=1 -timeout 15m -run 'Endurance' ./...

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
	rm -f coverage.out

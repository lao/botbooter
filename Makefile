.PHONY: all build test test-race test-examples cover bench soak endurance lint fmt fmt-examples vet vet-examples vuln run-cli publish tidy clean

# Default target: format, vet, lint and race-test. The lifecycle code is
# concurrency-heavy, so the pre-commit gate runs the race detector.
all: fmt fmt-examples vet vet-examples lint test-race test-examples

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

# _examples is its own module, so the root ./... never tests it. Examples are
# mostly wiring exercised by vet, but pure helpers (env parsing) carry tests.
test-examples:
	cd _examples && go test ./...

cover:
	go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1

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

# golangci-lint's fmt runs every formatter lint enforces (gofmt + goimports),
# so make fmt can never pass while make lint fails on formatting.
fmt:
	golangci-lint fmt ./...

# _examples is its own module, so the root ./... never reaches it (gofmt -w .
# used to walk into it by path).
fmt-examples:
	cd _examples && golangci-lint fmt ./...

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
	cd _examples && go run ./basic cli

# Publish a new version of the module: check the release preconditions, run
# the full pre-commit gate, then create an annotated tag and push it. Go
# consumers pick the tag up through the module proxy; the final go list warms
# proxy.golang.org so the version resolves immediately (its failure is
# non-fatal — the tag is already live and the proxy fetches on demand).
# Publishing is one-way: once the proxy caches a version it can never be
# re-tagged — a bad release must be burned and retracted, not reissued.
# VERSION reaches the shell as an environment variable, never as recipe text,
# so a value with shell metacharacters is inert data, not command source.
# Usage: make publish VERSION=v0.4.0
publish: export PUBLISH_VERSION := $(VERSION)
publish:
	@[ -n "$$PUBLISH_VERSION" ] || { echo "usage: make publish VERSION=vX.Y.Z"; exit 1; }
	@printf '%s' "$$PUBLISH_VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$' || { echo "VERSION must be canonical semver like v0.4.0 (no leading zeros, no pre-release)"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$$PUBLISH_VERSION" >/dev/null || { echo "tag $$PUBLISH_VERSION already exists: git push origin $$PUBLISH_VERSION to finish an interrupted publish, or git tag -d $$PUBLISH_VERSION to retry"; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || { echo "working tree not clean"; exit 1; }
	@git fetch -q origin main
	@git merge-base --is-ancestor HEAD origin/main || { echo "HEAD is not on origin/main; merge and push first"; exit 1; }
	$(MAKE) all
	@[ -z "$$(git status --porcelain)" ] || { echo "make all modified files (fmt); commit them and retry"; exit 1; }
	git tag -a "$$PUBLISH_VERSION" -m "$$PUBLISH_VERSION"
	git push origin "$$PUBLISH_VERSION"
	-GOPROXY=https://proxy.golang.org go list -m github.com/lao/botbooter@"$$PUBLISH_VERSION"

tidy:
	go mod tidy

clean:
	rm -f coverage.txt

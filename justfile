# resy-snipe task runner. Install: brew install just
# Run `just --list` to see available recipes.

set shell := ["bash", "-cu"]
set dotenv-load := false

# Default recipe — run when `just` is invoked with no argument.
default: help

# Show available recipes.
help:
    @just --list

# Build the resy-snipe binary into ./bin/.
build:
    mkdir -p bin
    go build -o bin/resy-snipe ./cmd/resy-snipe

# Build with the race detector and full debug info.
build-debug:
    mkdir -p bin
    go build -race -gcflags="all=-N -l" -o bin/resy-snipe ./cmd/resy-snipe

# Run the resy-snipe binary, forwarding any extra arguments.
run *ARGS:
    go run ./cmd/resy-snipe {{ARGS}}

# Run the full test suite with the race detector.
test:
    go test -race ./...

# Run a single package's tests, e.g. `just test-pkg ./internal/engine/...`.
test-pkg PKG:
    go test -race {{PKG}}

# Run tests N times to surface flakes (concurrency tests, timing-sensitive code).
test-flake N="10":
    go test -race -count={{N}} ./...

# Lint: go vet + golangci-lint when present. golangci-lint config lives at
# .golangci.yml (see beads-49u).
lint:
    go vet ./...
    @if command -v golangci-lint >/dev/null 2>&1; then \
        golangci-lint run ./...; \
    else \
        echo "golangci-lint not installed — skipping. brew install golangci-lint"; \
    fi

# Format every .go file with gofmt and goimports if available.
fmt:
    gofmt -s -w .
    @if command -v goimports >/dev/null 2>&1; then \
        goimports -w .; \
    fi

# Tidy go.mod / go.sum.
tidy:
    go mod tidy

# Project-defined "is the codebase healthy" gates from CLAUDE.md.
gates:
    @echo "=> go vet" && go vet ./...
    @echo "=> go build" && go build ./...
    @echo "=> go test -race" && go test -race ./...
    @echo "=> time.Now() outside internal/clock" && \
        ! (grep -rn 'time\.Now()' --include='*.go' | grep -v internal/clock | grep -v _test.go)
    @echo "=> ioutil." && ! grep -rn 'ioutil\.' --include='*.go'
    @echo "=> map[string]interface{}" && \
        ! grep -rn 'map\[string\]interface{}' --include='*.go'
    @echo "=> json.RawMessage outside internal/store" && \
        ! (grep -rn 'json\.RawMessage' --include='*.go' | grep -v internal/store)
    @echo "all gates green"

# Wipe build artifacts.
clean:
    rm -rf bin

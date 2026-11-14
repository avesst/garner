# garner build and check recipes; `just --list` shows them.

version := `git describe --tags --always --dirty 2>/dev/null || echo devel`

# Check, then build.
all: check build

# Formatting, vet and unit tests: what CI runs.
check: fmt-check vet test

# Fail if any Go file needs gofmt.
fmt-check:
    #!/bin/sh
    out="$(gofmt -l .)"
    if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi

vet:
    go vet ./...

test:
    go test ./...

# Static binary: no cgo, so it runs on any Linux distribution.
build:
    CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version={{version}}' -o bin/garner ./cmd/garner

clean:
    rm -rf bin

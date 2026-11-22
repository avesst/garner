# garner build and check recipes; `just --list` shows them.

# GoReleaser from PATH, or GORELEASER=/path/to/goreleaser. CI pins
# goreleaser_version; its source needs a newer Go than garner, so it is
# not run with go run.
goreleaser := env("GORELEASER", "goreleaser")
goreleaser_version := "2.19.0"

# List recipes.
default:
    @just --list --unsorted --justfile {{justfile()}}

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
    go vet -tags e2e ./e2e

test:
    go test ./...

# Real apt, dnf and container clients; needs Docker and network access.
e2e:
    go test -tags e2e -count=1 -v ./e2e

# Static binary: no cgo, so it runs on any Linux distribution. go build
# stamps the version from git: the tag, or a semver pseudo-version.
build:
    CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o bin/garner ./cmd/garner

# .deb and .rpm packages (amd64) in dist/, as a release builds them, with a
# snapshot version such as 1.2.4-dev.<commit time>+<hash>.
packages:
    @{{goreleaser}} --version | grep -q 'GitVersion: *{{goreleaser_version}}$' || \
        echo "warning: CI uses GoReleaser {{goreleaser_version}}" >&2
    {{goreleaser}} release --snapshot --clean --skip=publish

# Install the packages from dist/ in Debian, Ubuntu, AlmaLinux and Fedora
# containers; needs Docker.
test-packages:
    GARNER_PACKAGES={{justfile_directory()}}/dist go test -tags e2e -count=1 -v -run TestPackages ./e2e

clean:
    rm -rf bin dist build

.PHONY: build run test clean release release-current

VERSION ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/platform-bridge-wecom ./cmd/bridge

run:
	go run ./cmd/bridge

test:
	go test ./...

clean:
	go clean
	rm -rf ./bin ./dist

# Cross-compile all platforms (win/mac/linux × amd64/arm64) into dist/<version>/
release:
	VERSION=$(VERSION) bash scripts/build-release.sh

# Only build the host platform (quick local smoke test of the release script)
release-current:
	VERSION=$(VERSION) bash scripts/build-release.sh $$(go env GOOS)/$$(go env GOARCH)

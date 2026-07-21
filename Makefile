GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
export GOTOOLCHAIN := local

PACKAGE := github.com/bullionbear/strategon

# --dirty: local uncommitted changes get a -dirty suffix (ops version check)
# --always: fall back to short commit when no tag matches
# || echo dev: still build when git is unavailable
VERSION := $(shell git describe --tags --always --dirty --match='v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || echo "dev")
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
# -u: UTC, avoid builder timezone skew
BUILD_TIMESTAMP := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -X '$(PACKAGE)/internal/buildinfo.Version=$(VERSION)' \
           -X '$(PACKAGE)/internal/buildinfo.CommitHash=$(COMMIT_HASH)' \
           -X '$(PACKAGE)/internal/buildinfo.BuildTime=$(BUILD_TIMESTAMP)'

.PHONY: all proto lint breaking generate build test tidy tools web-build release

all: generate build test

## tools: install buf and protoc plugins pinned to go.mod-compatible versions
tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.47.2
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.16.2
	go install github.com/sudorandom/protoc-gen-connect-openapi@v0.25.7

## lint: enforce proto style
lint:
	buf lint

## breaking: gate against breaking proto changes vs main
breaking:
	buf breaking --against '.git#branch=main'

## generate: regenerate Go + connect-go + Connect-ES TS + OpenAPI from proto.
## Needs web/node_modules (protoc-gen-es). OpenAPI lands in web/static/openapi.json
## for the SPA docs page and Docker FE build.
generate proto:
	@test -x web/node_modules/.bin/protoc-gen-es || (cd web && pnpm install --frozen-lockfile)
	buf generate

## build: compile all packages with buildinfo ldflags; also emit bin/ binaries
build:
	go build -ldflags "$(LDFLAGS)" ./...
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/controlplane ./cmd/controlplane
	go build -ldflags "$(LDFLAGS)" -o bin/agent ./cmd/agent

## test: run the full Go test suite
test:
	go test ./...

## web-build: build the SPA and stage it where //go:embed picks it up.
## Order matters: this must run before any go build that ships a
## UI, or an empty dist/ gets embedded. Requires web/static/openapi.json
## from `make generate`.
web-build:
	cd web && pnpm install --frozen-lockfile
	@test -f web/static/openapi.json || (echo "missing web/static/openapi.json — run 'make generate' first" && exit 1)
	cd web && pnpm build
	rm -rf internal/webassets/dist
	cp -r web/build internal/webassets/dist
	touch internal/webassets/dist/.gitkeep

## release: UI-embedded, statically linked CP + agent for both architectures
release: web-build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/controlplane-linux-amd64 ./cmd/controlplane
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/controlplane-linux-arm64 ./cmd/controlplane
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/agent-linux-arm64 ./cmd/agent

## web-install / web-check / web-dev: frontend helpers
web-install:
	cd web && pnpm i

web-check:
	cd web && pnpm run check

web-dev:
	cd web && pnpm run dev -- --host 127.0.0.1 --port 5173

## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

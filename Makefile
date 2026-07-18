GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
export GOTOOLCHAIN := local

.PHONY: all proto lint breaking generate build test tidy tools

all: generate build test

## tools: install buf and protoc plugins pinned to go.mod-compatible versions
tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.47.2
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.16.2

## lint: enforce proto style (PROTOCOL.md discipline)
lint:
	buf lint

## breaking: gate against breaking proto changes vs main (PROTOCOL.md §9)
breaking:
	buf breaking --against '.git#branch=main'

## generate: regenerate Go + connect-go + Connect-ES TS from proto
generate proto:
	buf generate

## build: compile all packages and command binaries
build:
	go build ./...

## test: run the full Go test suite
test:
	go test ./...

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

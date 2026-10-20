#!/usr/bin/env bash
# Builds holocron for this machine's OS and cross-compiles holocron.exe into the repository root.
# gofmt, vet and tests run first; a build that completes is a build that passed.
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.local/go/bin:$PATH"
test -z "$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "gofmt: files above need formatting"; exit 1; }
go vet ./...
go test -p 1 ./...   # one package at a time: the web and real-process suites are heavy
version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=$version -X holocron/internal/web.version=$version" -o holocron.exe ./cmd/holocron
echo "built $(pwd)/holocron.exe ($version)"

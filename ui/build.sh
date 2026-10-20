#!/usr/bin/env bash
# Cross-compile camui.exe for Windows from WSL.
#
# The web app now lives in internal/web (package web) and ./cmd/camui is its thin main, both in
# the repository's one Go module; this script is kept at its old path with its old interface for
# cam.ps1, the PowerShell harness and the docs. holocron.exe (build-holocron.sh) contains the same
# code as its web role.
#
# The NVR machine needs no Go toolchain - the binary is self-contained and the
# UI is embedded, so there is nothing to deploy beside it. See docs/go-toolchain.md
# for the one-time toolchain install.
#
# Usage:  ui/build.sh [output]        default output: <repo>/camui.exe
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
out="${1:-$repo/camui.exe}"

if ! command -v go >/dev/null 2>&1; then
    if [ -x "$HOME/.local/go/bin/go" ]; then
        export PATH="$HOME/.local/go/bin:$PATH"
    else
        echo "go not found. See docs/go-toolchain.md" >&2
        exit 1
    fi
fi

version="$(git -C "$repo" describe --always --dirty 2>/dev/null || echo unknown)"
version="${version}-$(date -u +%Y%m%d)"

cd "$repo"
unformatted="$(gofmt -l internal/web cmd/camui)"
if [ -n "$unformatted" ]; then
    echo "gofmt found unformatted files:" >&2
    echo "$unformatted" >&2
    exit 1
fi
go vet ./internal/web/... ./cmd/camui
go test ./internal/web/...

# CGO_ENABLED=0 keeps the binary free of any C runtime dependency; -s -w drops
# the symbol table and DWARF data, which this binary has no use for.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
    -trimpath \
    -ldflags "-s -w -X holocron/internal/web.version=${version}" \
    -o "$out" ./cmd/camui

echo "built $out"
ls -l "$out"

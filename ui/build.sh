#!/usr/bin/env bash
# Cross-compile camui.exe for Windows from WSL.
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

cd "$here"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
    echo "gofmt found unformatted files:" >&2
    echo "$unformatted" >&2
    exit 1
fi
go vet ./...
go test ./...

# CGO_ENABLED=0 keeps the binary free of any C runtime dependency; -s -w drops
# the symbol table and DWARF data, which this binary has no use for.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${version}" \
    -o "$out" .

echo "built $out"
ls -l "$out"

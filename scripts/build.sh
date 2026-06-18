#!/usr/bin/env sh
# Bootstrap build — produces the prebuilt `facet` binaries.
#
# This is the ONE-TIME host step. End users never run this; they just use the
# shipped `./facet` binary. When Facet self-hosts, this script is what gets
# replaced by `facet build` compiling Facet itself, and the bootstrap host is
# dropped entirely.
set -e
cd "$(dirname "$0")/.."

flags='-trimpath -ldflags=-s -w'

# native binary for this machine
go build -trimpath -ldflags="-s -w" -o facet ./cmd/facet
chmod +x facet
echo "built ./facet"

# shipped cross-platform binaries
mkdir -p dist
build() { GOOS="$1" GOARCH="$2" go build -trimpath -ldflags="-s -w" -o "dist/$3" ./cmd/facet; echo "  dist/$3"; }
build linux   amd64 facet-linux-amd64
build linux   arm64 facet-linux-arm64
build darwin  amd64 facet-macos-intel
build darwin  arm64 facet-macos-apple
build windows amd64 facet-windows-amd64.exe
echo "done"

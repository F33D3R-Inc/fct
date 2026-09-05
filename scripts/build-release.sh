#!/usr/bin/env bash
# Cross-compile the facet toolchain for every supported platform. The whole
# toolchain is pure Go (no CGO), so these binaries run anywhere with nothing to
# install. Usage: scripts/build-release.sh [version]
#
# These binaries are also the *base* images for application releases. `facet
# build --release` packages an app by copying a facet binary and appending the
# app's compiled graph to it (cmd/facet/release.go), so cross-building an app is
# a base swap and needs no Go toolchain and no compiler on the builder:
#
#   facet build --release app.fct --base dist/facet-linux-arm64 -o dist/myapp-arm64
#
# A darwin base is the one exception: appending invalidates the Mach-O signature,
# so the artifact is re-signed (codesign, a macOS tool) and must therefore be
# produced on a Mac.
set -euo pipefail

VERSION="${1:-dev}"
OUT="dist"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

rm -rf "$OUT"
mkdir -p "$OUT"

platforms=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

for p in "${platforms[@]}"; do
  os="${p%/*}"
  arch="${p#*/}"
  name="facet-${os}-${arch}"
  [ "$os" = "windows" ] && name="${name}.exe"
  echo "building $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X facet/internal/registry.ToolchainVersion=${VERSION}" \
    -o "$OUT/$name" ./cmd/facet
done

echo
echo "release ${VERSION} built into ${OUT}/:"
ls -1 "$OUT"
echo
echo "each of these is also a --base for packaging an app as one binary:"
echo "  facet build --release app.fct --base ${OUT}/facet-linux-amd64 -o ${OUT}/myapp"

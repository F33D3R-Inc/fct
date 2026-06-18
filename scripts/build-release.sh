#!/usr/bin/env bash
# Cross-compile the facet toolchain for every supported platform. The whole
# toolchain is pure Go (no CGO), so these binaries run anywhere with nothing to
# install. Usage: scripts/build-release.sh [version]
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
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$OUT/$name" ./cmd/facet
done

echo
echo "release ${VERSION} built into ${OUT}/:"
ls -1 "$OUT"

#!/usr/bin/env bash
# Generate a CycloneDX Software Bill of Materials for the facet module. Pure Go
# tooling fetched on demand (no install step), so it is reproducible anywhere.
#
# Usage: scripts/sbom.sh [output.json]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
OUT="${1:-dist/sbom.cdx.json}"
mkdir -p "$(dirname "$OUT")"

# cyclonedx-gomod walks the module graph (lib/pq, x/crypto) and emits a CycloneDX
# SBOM with each dependency, its version, and license metadata.
go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.7.0 \
  mod -json -licenses -output "$OUT" .

echo "wrote $OUT"

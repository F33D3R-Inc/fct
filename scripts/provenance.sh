#!/usr/bin/env bash
# Emit a SLSA v1.0 provenance statement (in-toto) over the release artifacts: an
# attestation of what was built, from which source, by which workflow. The CI job
# then keyless-signs it with cosign so it is verifiable against the Sigstore
# transparency log. Reproducible: pure shell, no extra tooling.
#
# Usage: scripts/provenance.sh [dist-dir]
set -euo pipefail

DIST="${1:-dist}"
OUT="$DIST/provenance.slsa.json"
builder="https://github.com/${GITHUB_REPOSITORY:-local/facet}/.github/workflows/release.yml@${GITHUB_REF:-refs/heads/main}"

subjects=""
for f in "$DIST"/facet-* "$DIST"/sbom.cdx.json; do
  [ -f "$f" ] || continue
  name="$(basename "$f")"
  digest="$(sha256sum "$f" | cut -d' ' -f1)"
  subjects="${subjects}{\"name\":\"${name}\",\"digest\":{\"sha256\":\"${digest}\"}},"
done
subjects="[${subjects%,}]"

cat > "$OUT" <<EOF
{
  "_type": "https://in-toto.io/Statement/v1",
  "predicateType": "https://slsa.dev/provenance/v1",
  "subject": ${subjects},
  "predicate": {
    "buildDefinition": {
      "buildType": "https://facet.dev/release/v1",
      "externalParameters": {
        "ref": "${GITHUB_REF:-}",
        "repository": "${GITHUB_REPOSITORY:-}",
        "workflow": ".github/workflows/release.yml"
      },
      "resolvedDependencies": [
        { "uri": "git+https://github.com/${GITHUB_REPOSITORY:-local/facet}", "digest": { "gitCommit": "${GITHUB_SHA:-}" } }
      ]
    },
    "runDetails": {
      "builder": { "id": "${builder}" },
      "metadata": {
        "invocationId": "${GITHUB_RUN_ID:-}",
        "startedOn": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      }
    }
  }
}
EOF

echo "wrote $OUT"

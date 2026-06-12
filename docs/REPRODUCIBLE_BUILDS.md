# Reproducible Builds & Release Verification

Every `fct` release is built by the public `release.yml` workflow with a
deterministic recipe, signed keylessly via Sigstore, attested with SLSA build
provenance, and shipped with an SPDX SBOM. This page is the recipe and the
three verification commands.

## What a release contains

| Artifact | What it is |
|---|---|
| `fct-<version>-<os>-<arch>[.exe]` | the CLI, one per platform |
| `checksums.txt` | sha256 of every artifact (including the SBOM) |
| `checksums.txt.sig`, `checksums.txt.pem` | keyless cosign signature + certificate over `checksums.txt` |
| `fct-<version>.sbom.spdx.json` | SPDX SBOM for the release (FA is dependency-free: Go stdlib only) |
| build provenance | SLSA provenance as a GitHub artifact attestation (stored by GitHub, not a release file) |

## Verify a download

1. **Checksum** — the binary you downloaded is the one that was checksummed:

   ```sh
   sha256sum -c checksums.txt --ignore-missing
   ```

2. **Signature** — `checksums.txt` was produced by *this repo's release
   workflow* (Sigstore keyless: the certificate binds the signature to the
   workflow's OIDC identity, logged in the public Rekor transparency log):

   ```sh
   cosign verify-blob checksums.txt \
     --signature checksums.txt.sig \
     --certificate checksums.txt.pem \
     --certificate-identity-regexp 'https://github.com/F33D3R-Inc/fct/\.github/workflows/release\.yml@refs/tags/v.*' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```

3. **Provenance (SLSA)** — the binary was built from this repo at that tag by
   GitHub Actions, not on someone's laptop:

   ```sh
   gh attestation verify fct-<version>-linux-amd64 --repo F33D3R-Inc/fct
   ```

Together: the checksum proves integrity, the signature proves the checksums
came from the release workflow, the provenance proves the build itself.

## Rebuild it yourself (bit-for-bit)

The build is deterministic: same toolchain + same recipe ⇒ same bytes.

1. Read the exact toolchain and settings out of the released binary:

   ```sh
   go version -m fct-<version>-linux-amd64   # prints e.g. go1.26.x, -trimpath, CGO_ENABLED=0, …
   ```

2. With that exact Go version installed, from the tag:

   ```sh
   git checkout <version>
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOFLAGS=-buildvcs=false \
     go build -trimpath -ldflags="-s -w -buildid= -X main.version=<version>" -o fct-rebuilt ./cmd/fct
   sha256sum fct-rebuilt fct-<version>-linux-amd64   # identical
   ```

What makes it deterministic: `-trimpath` (no absolute paths), `-buildid=`
(no content-addressed build id), `-buildvcs=false` (no VCS stamping),
`CGO_ENABLED=0` (no host toolchain), and zero third-party dependencies
(`go.mod` has no requirements, so there is no dependency resolution to drift).

## Supply-chain policy (CI)

- **Actions pinned to commit SHAs** — every third-party action in
  `.github/workflows/` is pinned to a full commit hash (the tag is a comment),
  so a moved tag in a compromised action repo cannot change what we run.
- **`govulncheck` on every push/PR** (`ci.yml`) — scans the module and the Go
  stdlib against the Go vulnerability database, call-graph-aware.
- **Dependency review on every PR** — flags any newly introduced dependency
  with known vulnerabilities. FA is intentionally dependency-free; this gate
  keeps the cost of *staying* dependency-free visible in review.
- **SBOM per release** — consumers can feed it to their own scanners.

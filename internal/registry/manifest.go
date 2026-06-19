package registry

import (
	"encoding/json"
	"fmt"
)

// Manifest is a facet's `facet.json` — the small metadata file at a published
// repo's root. Only a handful of fields matter to the toolchain; unknown keys
// are ignored so the format can grow without breaking older toolchains.
type Manifest struct {
	// Name is the canonical import path, e.g. "github.com/owner/repo". It MUST
	// equal the repo it is fetched from; a mismatch is rejected on fetch.
	Name string `json:"name"`
	// Version SHOULD equal the git tag the manifest is published under.
	Version string `json:"version"`
	// Main is the entry file used when the repo root is imported (no subpath).
	Main string `json:"main"`
	// Description is free-form human text.
	Description string `json:"description"`
	// Facet is a semver range naming the minimum toolchain (e.g. ">=1.4.0").
	Facet string `json:"facet"`
	// License is an SPDX identifier.
	License string `json:"license"`
}

// ParseManifest decodes a facet.json document. An empty or malformed document is
// an error; a valid one with unknown extra keys is accepted.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("invalid facet.json: %w", err)
	}
	return &m, nil
}

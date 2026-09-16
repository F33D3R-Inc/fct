#!/usr/bin/env bash
# Regenerates the Rust/Go/TS wire types from schema/facetql_wire.fct — the one
# command per repo SCHEMA_IDL_SCOPE.md §4 item 4 asks for. Run this after
# editing the schema; CI (ci.yml's wire-drift job) fails if the committed
# output in schema/generated/ doesn't match what this script produces, so no
# one hand-edits a generated file and no one forgets to regenerate after a
# schema change.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
go run ./cmd/schemagen schema/facetql_wire.fct schema/generated/facetql_wire wire

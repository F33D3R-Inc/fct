// Package selfhost holds the parts of the toolchain written in fct. Its Go
// files are the parity tests that hold each of them to the reference it
// replaces, plus this one: the FacetQL engine's sources, embedded so the facet
// binary can run the engine (`facet facetql`) with nothing else installed.
package selfhost

import "embed"

// Engine is the self-hosted FacetQL — the server (EngineEntry) and the
// operator CLI (CLIEntry) and every module they import, exactly the import
// closures `facet facetql` compiles. The list is
// explicit so the binary carries the engine and not the rest of this
// directory; cmd/facet's TestFacetQLEmbedCoversEngineImports fails the moment
// an import is added that the list does not name.
//
//go:embed fqcli.fct fqcli_text.fct facetql_cli.fct fqserver.fct fqhttp.fct fqtx.fct fqquery.fct fqengine.fct fqindex.fct fqheap.fct catalog.fct binary.fct wal.fct transaction.fct core_types.fct index_text.fct fqjson.fct aes_gcm.fct reference.fct fqbtree.fct pager.fct page.fct checkpoint.fct unquote.fct http_server.fct http_client.fct fabric_json.fct
var Engine embed.FS

// EngineEntry is the server's entry module within Engine.
const EngineEntry = "fqserver.fct"

// CLIEntry is the operator CLI's entry module within Engine: every
// `facetql` command but start.
const CLIEntry = "fqcli.fct"

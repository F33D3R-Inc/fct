// Package runtime embeds the fixed Facet Architecture client runtime so that a
// server can serve it without shipping a separate static file. The runtime is
// the same for every FA application — it contains no application logic — so it
// is embedded once here and reused.
package runtime

import _ "embed"

//go:embed fa-runtime.js
var Script []byte

// ContentType is the MIME type to serve Script with.
const ContentType = "application/javascript; charset=utf-8"

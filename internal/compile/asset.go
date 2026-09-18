package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"facet/internal/ast"
)

// `asset from "path"` is the general, any-file-type counterpart of `css from
// "path.css"`: parsed with no filesystem access (see ast.Asset), resolved
// here relative to the file that named it, exactly like every CSSFile above.
//
// It cannot reuse collectModules' own `for _, ref := range app.CSSFiles`
// loop, because an asset reference is not collected into one flat list the
// parser fills in top to bottom — it is a plain expression, so it can appear
// anywhere an expression can: inside a component's content, a proc body, a
// policy condition, nested inside a call's arguments. There is no fixed
// enumeration of "the places an expression lives" to hand-maintain here (the
// language has grown that list once already, for ast.WalkNodes, and its own
// doc says explicitly that a node the walk cannot see is a node some pass
// handles in silence — exactly the failure mode this must not have). So
// resolveAssets finds every *ast.Asset reachable from the freshly parsed file
// by walking the whole value with reflection instead: complete by
// construction, and never drifts out of sync with internal/ast as new node or
// expression kinds are added.
//
// textAssetExt names the file types resolved by inlining raw content — the
// same treatment `css from` already gives a stylesheet, generalized to any
// text file. Anything not listed here (an image, a font, a PDF, ...) is
// assumed binary/static and copied into the app as a content-addressed asset
// instead (see resolveOneAsset).
var textAssetExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true,
	".xml": true, ".html": true, ".htm": true, ".yaml": true, ".yml": true,
	".toml": true, ".ini": true, ".graphql": true, ".sql": true, ".json": true,
	// .json is inlined as raw text rather than parsed into a real map/array
	// `.fct` value. Parsing it into a genuine ast.MapLit/ListLit is a clean
	// enough fit for the language's existing literal support that it is worth
	// doing later — a map/list literal is proc-only today (checkNoIndex), so
	// the value would only ever be usable inside a `proc` body, not in a
	// component's content the way an image reference is — but it did not fit
	// this pass; raw text is the honest fallback until then.
}

// mergeAssets folds src's bundled binary/static assets into dst, keyed by
// content hash so a file contributed by two different modules or composition
// layers (an imported facet and its host app, a wireframe and a brick) lands
// once. Called everywhere joinStylesheets/joinCSS is, for the same reason: an
// asset a facet bundles ships with the facet.
func mergeAssets(dst, src *ast.App) {
	for name, blob := range src.Assets {
		if dst.Assets == nil {
			dst.Assets = map[string]ast.AssetBlob{}
		}
		dst.Assets[name] = blob
	}
}

// resolveAssets finds every *ast.Asset reachable from app (an entire freshly
// parsed file, before it is merged with any other module) and resolves it in
// place, relative to dir — the directory of the .fct file that declared it.
// Binary/static payloads it reads are also recorded on app.Assets, keyed by
// content hash, for ir.Build to carry into the compiled graph.
func resolveAssets(app *ast.App, dir string) error {
	var first error
	walkAssets(reflect.ValueOf(app), func(a *ast.Asset) {
		if first != nil || a.Resolved {
			return
		}
		if err := resolveOneAsset(app, dir, a); err != nil {
			first = err
		}
	})
	return first
}

// resolveOneAsset reads one asset reference's file and fills in Kind/Val (and,
// for a binary/static file, app.Assets) — the same "read once, fold the
// result in" shape collectModules already gives `css from`.
func resolveOneAsset(app *ast.App, dir string, a *ast.Asset) error {
	p := a.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("%d: asset from %q: %w", a.Line, a.Path, err)
	}
	ext := strings.ToLower(filepath.Ext(a.Path))
	if textAssetExt[ext] {
		a.Kind, a.Val, a.Resolved = "text", string(data), true
		return nil
	}
	// Binary/static: bundled as a content-addressed asset, referenced by the
	// URL it will be served from (runtime/staticassets.go). Naming it by a
	// hash of its own bytes is what makes two files that reference the same
	// image — even from different directories — bundle exactly one copy, and
	// what makes the URL cacheable forever: it only ever changes when the
	// bytes do.
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:12]) + ext
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	if app.Assets == nil {
		app.Assets = map[string]ast.AssetBlob{}
	}
	app.Assets[name] = ast.AssetBlob{Bytes: data, ContentType: ct}
	a.Kind, a.Val, a.Resolved = "text", "/assets/"+name, true
	return nil
}

// walkAssets visits every *ast.Asset reachable from v — a struct, slice,
// array, map, pointer or interface, at any depth — calling visit on each one
// it finds. It never descends into an already-visited *ast.Asset's own Val
// field (Val is `any` and, once resolved, never holds another Asset), so
// there is no risk of looping through a value it just set.
func walkAssets(v reflect.Value, visit func(*ast.Asset)) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return
		}
		if a, ok := v.Interface().(*ast.Asset); ok {
			visit(a)
			return
		}
		walkAssets(v.Elem(), visit)
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		walkAssets(v.Elem(), visit)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanInterface() {
				walkAssets(f, visit)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkAssets(v.Index(i), visit)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkAssets(v.MapIndex(k), visit)
		}
	}
}

package fa

import (
	"bytes"
	"html/template"
)

// RootMountID is the Playground's content mount — the facet-id of the <main>
// element that holds page content. App facets render inside it; navigation and
// full-page SSE updates retarget this node.
//
// The Playground is the base canvas every facet sits on (see README.md):
//
//	Playground (this document)
//	  └ wireframe facets (nav, sidebars)
//	      └ template facets (cards, headers)
//	          └ composites → atomics
const RootMountID = "fa:root"

// ShellOptions configures the Playground document chrome. Zero value is valid.
type ShellOptions struct {
	Title    string        // <title>; defaults to "FA App"
	Lang     string        // <html lang>; defaults to "en"
	Theme    string        // <body data-theme>; omitted if empty
	CSS      template.CSS  // inline <style> (trusted)
	HeadHTML template.HTML // extra <head> markup — stylesheets, meta (trusted)
}

type shellData struct {
	Key      string
	Title    string
	Lang     string
	Theme    string
	CSS      template.CSS
	HeadHTML template.HTML
	Content  template.HTML
	MountID  string
}

var shellTmpl = template.Must(template.New("fa.shell").Parse(
	`<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="fa-key" content="{{.Key}}">
<title>{{.Title}}</title>
{{- if .CSS}}
<style>{{.CSS}}</style>
{{- end}}
{{- if .HeadHTML}}
{{.HeadHTML}}
{{- end}}
</head>
<body{{if .Theme}} data-theme="{{.Theme}}"{{end}}>
<main data-facet-id="{{.MountID}}">{{.Content}}</main>
<script src="/fa-runtime.js"></script>
</body>
</html>
`))

// renderShell builds the Playground document around content, embedding the
// signing key so the client can verify pushed events.
func renderShell(key string, content template.HTML, opts ShellOptions) template.HTML {
	d := shellData{
		Key: key, Content: content, MountID: RootMountID,
		Title: opts.Title, Lang: opts.Lang, Theme: opts.Theme,
		CSS: opts.CSS, HeadHTML: opts.HeadHTML,
	}
	if d.Title == "" {
		d.Title = "FA App"
	}
	if d.Lang == "" {
		d.Lang = "en"
	}
	var buf bytes.Buffer
	_ = shellTmpl.Execute(&buf, d)
	return template.HTML(buf.String())
}

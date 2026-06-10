package fa

import (
	"html/template"
	"strings"
	"testing"
)

func TestPlaygroundWrapsContent(t *testing.T) {
	app := New([]byte("{}"))
	page := string(app.Page(
		template.HTML(`<button data-facet-id="LikeButton:post:p1">x</button>`),
		ShellOptions{Title: "My App", Theme: "dark"},
	))

	for _, want := range []string{
		`<!doctype html>`,
		`<meta name="fa-key" content="` + app.Key() + `"`, // signing key embedded
		`<title>My App</title>`,
		`data-theme="dark"`,
		`<main data-facet-id="fa:root">`,                        // the Playground content mount
		`<button data-facet-id="LikeButton:post:p1">x</button>`, // content lives inside
		`<script src="/fa-runtime.js">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Playground page missing %q\n---\n%s", want, page)
		}
	}
}

func TestPlaygroundDefaults(t *testing.T) {
	app := New([]byte("{}"))
	page := string(app.Page("", ShellOptions{}))
	if !strings.Contains(page, "<title>FA App</title>") {
		t.Error("expected default title")
	}
	if !strings.Contains(page, `<html lang="en">`) {
		t.Error("expected default lang")
	}
}

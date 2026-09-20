package integration

import (
	"regexp"
	"strings"
	"testing"
)

const probeIfApp = `app Grid:
    state level: int = 3 @client
    component StatusCell(n: int):
        if n == 1:
            text "one"
        if n == 2:
            text "two"
        if n == 3:
            text "three"
        if n == 4:
            text "four"
    view Home at "/":
        row class "x-grid":
            use StatusCell(level)
            text "actions"
        if level > 99:
            text "never"
`

var probeOpen = regexp.MustCompile(`<([a-z]+)(\s[^>]*)?>`)

func TestZZProbeIf(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, probeIfApp)
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	t.Logf("markup: %s", markup)
	elems := len(probeOpen.FindAllString(markup, -1))
	t.Logf("server elements: %d", elems)
	run, _ := runClient(t, page, nil)
	t.Logf("client nodes: %d", run.Nodes)
	t.Logf("client text: %q server text: %q", run.Text, serverText(page))
	t.Logf("empty divs: %d", strings.Count(markup, "<div></div>"))
	for _, at := range run.Attrs {
		t.Logf("  attr %v", at)
	}
}

package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeModules(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "main.fct")
}

const routeMain = `import "works.fct"

app Main:
    view Home at "/":
        text "x"
`

const routeWorks = `app Works:
    type WorkDTO:
        id: int
    type Ping:
        n: int
    entity Work:
        id: int
    action getWork(id: int) -> WorkDTO:
        return WorkDTO{id: id}
    action hook(n: int):
        emit Ping{n: n}
    action react():
        emit Ping{n: 0}
    api GET "/api/v2/works/{id}" -> getWork
    stream "/api/v2/pings": Ping
    webhook "/hooks/ping" -> hook
    on hook -> react
    contract "/api/v2/contract"
`

// An imported module's api routes, streams, webhooks, triggers and contract
// route reach the merged app, next to the domain they belong to.
func TestImportedRoutesMerge(t *testing.T) {
	g, err := File(writeModules(t, map[string]string{"main.fct": routeMain, "works.fct": routeWorks}))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.APIs) != 1 || g.APIs[0].Path != "/api/v2/works/{id}" {
		t.Fatalf("apis = %+v", g.APIs)
	}
	if len(g.Streams) != 1 || len(g.Webhooks) != 1 || len(g.Triggers) != 1 || g.Contract == nil {
		t.Fatalf("streams %d webhooks %d triggers %d contract %v", len(g.Streams), len(g.Webhooks), len(g.Triggers), g.Contract)
	}
}

// Two modules claiming one route is an error naming both files.
func TestImportedRouteCollisions(t *testing.T) {
	cases := map[string]string{
		"api":      `    api GET "/api/v2/works/{id}" -> getWork2`,
		"stream":   `    stream "/api/v2/pings": Ping2`,
		"webhook":  `    webhook "/hooks/ping" -> hook2`,
		"contract": `    contract "/api/v2/other"`,
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			other := `app Other:
    type WorkDTO2:
        id: int
    type Ping2:
        n: int
    action getWork2(id: int) -> WorkDTO2:
        return WorkDTO2{id: id}
    action hook2(n: int):
        emit Ping2{n: n}
` + decl + "\n"
			main := strings.Replace(routeMain, `import "works.fct"`, "import \"works.fct\"\nimport \"other.fct\"", 1)
			_, err := File(writeModules(t, map[string]string{"main.fct": main, "works.fct": routeWorks, "other.fct": other}))
			if err == nil || !strings.Contains(err.Error(), "other.fct") || !strings.Contains(err.Error(), "works.fct") {
				t.Fatalf("want a collision naming both files, got %v", err)
			}
		})
	}
}

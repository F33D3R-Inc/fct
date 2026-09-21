package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func loadQueryApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("query.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/query.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestQueryPaginationDemo(t *testing.T) {
	ts := loadQueryApp(t)
	d := postJSON(t, ts, "runDemoQueryPagination")

	cases := map[string]string{
		"ascPage1Result":  "10:1,10:3|next=true",
		"ascPage2Result":  "20:2,30:0|next=true",
		"ascPage3Result":  "40:4|next=false",
		"descPage1Result": "40:4,30:0,20:2|next=true",
		"descPage2Result": "10:3,10:1|next=false",
		"addrPage1Result": "30:0,10:1,20:2|next=true",
		"addrPage2Result": "10:3,40:4|next=false",
	}
	for field, want := range cases {
		if got, _ := d[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

func TestQueryExtraScenarios(t *testing.T) {
	ts := loadQueryApp(t)

	cases := []struct {
		name string
		want string
	}{
		{"absentSortsLastAsc", "1:2,5:0,0:1"},
		{"absentSortsLastDesc", "0:1,5:0,1:2"},
		{"textOrderingAsc", "apple,banana,cherry"},
		{"offsetAtEnd", "|next=false"},
		{"offsetPastEnd", "|next=false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := postJSON(t, ts, "runQueryExtraScenario", c.name)
			got, _ := d["extraScenarioResult"].(string)
			if got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		})
	}
}

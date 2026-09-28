package runtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"facet/internal/compile"
)

// A column holds its declared type from the first write, so a row reads the
// same from the working set as it does after the store hands it back on a
// restart: an int column given nothing, a fraction, numeric text or a bool
// through an untyped value (fromJson) holds 0, the truncated int, the number
// and 1/0; a nullable column keeps nothing; a column the add does not name
// holds its empty value; a set converts the same way.
func TestColumnsHoldTheirTypeAcrossARestart(t *testing.T) {
	g, err := compile.String(`app C:
    entity Row:
        id: int
        n: int
        f: float
        b: bool
        s: text
        opt: int?
        left: text
        when: date
    action put(n: text, f: text, b: text, s: text, opt: text):
        add Row { n: fromJson(n), f: fromJson(f), b: fromJson(b), s: fromJson(s), opt: fromJson(opt) }
    action reset(id: int, n: text):
        set Row(id).n = fromJson(n)
    view V at "/":
        text "x"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, args := range [][]any{
		{"null", "null", "null", "null", "null"},
		{"3.7", "2", "1", "12", "5"},
		{`"12"`, `"2.5"`, `"yes"`, "true", `"9"`},
		{"true", "false", "0", "4.5", "null"},
	} {
		if _, err := srv.Run("ada", "member", true, "put", args); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.Run("ada", "member", true, "reset", []any{2, "9.9"}); err != nil {
		t.Fatal(err)
	}
	want := []record{
		{"id": 1, "n": 0, "f": 0.0, "b": false, "s": "", "opt": nil, "left": "", "when": 0},
		{"id": 2, "n": 9, "f": 2.0, "b": true, "s": "12", "opt": 5, "left": "", "when": 0},
		{"id": 3, "n": 12, "f": 2.5, "b": true, "s": "true", "opt": 9, "left": "", "when": 0},
		{"id": 4, "n": 1, "f": 0.0, "b": false, "s": "4.5", "opt": nil, "left": "", "when": 0},
	}
	ent, _ := srv.entityByName("Row")
	rows := srv.EntityRows("Row")
	for i, r := range rows {
		live := r.(record)
		if !reflect.DeepEqual(live, want[i]) {
			t.Errorf("row %d in the working set = %#v, want %#v", i+1, live, want[i])
		}
		// What a store hands back after a restart: the row as it is written
		// (rowNode's encoding), decoded as a load decodes it (nodeRecord).
		node, err := rowNode(ent, live)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		json.Unmarshal([]byte(node.Data), &raw)
		back, err := nodeRecord(ent, fqNode{Address: node.Address, Data: node.Data})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, live) {
			t.Errorf("row %d after a restart = %#v, before = %#v", i+1, back, live)
		}
	}
}

package runtime

import (
	"reflect"
	"testing"

	"facet/internal/compile"
)

// Ordering by a float field sorts by the float, not its integer part: 0.9,
// 0.5 and 0.1 all truncate to 0, and used to come back in insertion order.
func TestOrderByFloatField(t *testing.T) {
	g, err := compile.String(`app O:
    entity R:
        id: int
        b: text
        sim: float
    action seedRows:
        add R { b: "a", sim: 0.1 }
        add R { b: "c", sim: 0.9 }
        add R { b: "b", sim: 0.5 }
        add R { b: "d", sim: 2.0 }
    action top -> [text]:
        return list(r.b in R where true by sim desc limit 3)
    action bottom -> [text]:
        return list(r.b in R where true by sim)
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
	if _, err := srv.RunValue("ada", "member", true, "seedRows", nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]any{"top": {"d", "c", "b"}, "bottom": {"a", "b", "c", "d"}} {
		got, err := srv.RunValue("ada", "member", true, name, nil)
		if err != nil || !reflect.DeepEqual(plainValue(got), want) {
			t.Errorf("%s = %v, %v; want %v", name, got, err, want)
		}
	}
	if !lessVal(0.1, 0.9) || lessVal(0.9, 0.1) || !lessVal(1, 1.5) || !lessVal(1.5, 2) || lessVal(2, 2.0) {
		t.Error("lessVal mis-orders floats")
	}
}

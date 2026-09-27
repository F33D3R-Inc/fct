package runtime

import (
	"strings"
	"testing"

	"facet/internal/compile"
)

// A run joins the caller's transaction: its writes land with the caller's,
// its requires and checks apply, its failure — or a failure after it in the
// caller — undoes both, its return value binds, and its return ends only it.
func TestRunActionSemantics(t *testing.T) {
	g, err := compile.String(`
app R:
    entity Acct:
        id: int
        owner: text
        bal: int
    entity Log:
        id: int
        note: text
    policy member:
        actor != "guest"
    action credit(who: text, n: int) -> int:
        requires member
        check n > 0 "amount must be positive"
        if !exists(a in Acct where a.owner == who):
            add Acct { owner: who, bal: 0 }
        set a in Acct where a.owner == who:
            bal = a.bal + n
        return sum(a.bal in Acct where a.owner == who)
        add Log { note: "unreachable after return" }
    action pay(to: text, n: int):
        add Log { note: "before" }
        let after = run credit(to, n)
        add Log { note: "after " + after }
    action payThenFail(to: text, n: int):
        add Log { note: "before" }
        run credit(to, n)
        check false "the caller refuses after the run"
    view Home at "/":
        text "{count(Log)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	logs := func() []string {
		var out []string
		for _, r := range srv.EntityRows("Log") {
			out = append(out, toStr(r.(record)["note"]))
		}
		return out
	}
	if _, err := srv.Run("ada", "member", true, "pay", []any{"bob", 5}); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if got := strings.Join(logs(), "|"); got != "before|after 5" {
		t.Errorf("logs = %q: the callee's return value must bind and its return must end only the callee", got)
	}
	if _, err := srv.Run("ada", "member", true, "pay", []any{"bob", 0}); err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Errorf("a failing check in the callee must fail the caller, got %v", err)
	}
	if n := len(logs()); n != 2 {
		t.Errorf("the caller's write before the failed run must be undone: %d logs", n)
	}
	if _, err := srv.Run("ada", "member", true, "payThenFail", []any{"bob", 7}); err == nil {
		t.Errorf("payThenFail must fail")
	}
	if b := toInt(srv.EntityRows("Acct")[0].(record)["bal"]); b != 5 {
		t.Errorf("a failure after the run must undo the callee's writes too: balance = %d, want 5", b)
	}
	if _, err := srv.Run("guest", "guest", false, "pay", []any{"bob", 3}); err == nil || !strings.Contains(err.Error(), "forbidden: member") {
		t.Errorf("the callee's requires must apply, got %v", err)
	}
}

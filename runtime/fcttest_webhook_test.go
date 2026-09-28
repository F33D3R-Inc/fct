package runtime

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A sidecar `webhook` step drives the app's real webhook handler: a signed
// delivery runs the action, a forged or unsigned one is refused (403), and a
// retried delivery with the same Idempotency-Key replays without re-running.
func TestSidecarWebhookStep(t *testing.T) {
	g, err := compile.String(`
app W:
    entity Paid:
        id: int
        ref: text
    action confirm(ref: text) @internal:
        check ref != "" "ref required"
        add Paid { ref: ref }
    webhook "/hooks/pay" -> confirm
    view Home at "/":
        text "{count(Paid)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	suite := `{"tests":[{"name":"webhook","steps":[
	  {"webhook":"/hooks/pay","body":{"ref":"a"},"forge":true,"status":403},
	  {"webhook":"/hooks/pay","body":{"ref":"a"},"unsigned":true,"status":403},
	  {"expect":"count(Paid)","equals":0},
	  {"webhook":"/hooks/pay","body":{"ref":"a"},"idemKey":"evt-1"},
	  {"webhook":"/hooks/pay","body":{"ref":"a"},"idemKey":"evt-1"},
	  {"expect":"count(Paid)","equals":1},
	  {"webhook":"/hooks/pay","body":{"ref":""},"status":422,"fails":"ref required"},
	  {"webhook":"/hooks/nope","body":{}}
	]}]}`
	var out bytes.Buffer
	pass, fail, err := RunTests(g, []byte(suite), &out)
	if err != nil {
		t.Fatal(err)
	}
	if pass != 0 || fail != 1 || !strings.Contains(out.String(), `no webhook is declared at "/hooks/nope"`) {
		t.Fatalf("want every step but the undeclared path to hold:\n%s", out.String())
	}
}

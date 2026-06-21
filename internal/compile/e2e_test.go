package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// The canonical sealed-field app: a DM whose body is @e2e. The client seals the
// body before sending, the authority stores ciphertext, a reader opens it.
const e2eApp = `app A:
    auth
    entity DM:
        id: int
        to: text
        body: text @e2e
    action send(to: text, body: text):
        add DM { to: to, body: body }
    view Home at "/":
        box:
            for m in DM:
                text "{m.body}"
`

// anyE2ESeg reports whether any text/badge node in the tree carries a sealed seg.
func anyE2ESeg(nodes []ir.Node) bool {
	for _, n := range nodes {
		for _, s := range n.Segs {
			if s.E2E {
				return true
			}
		}
		if anyE2ESeg(n.Children) {
			return true
		}
	}
	return false
}

func TestE2EFieldAndSeal(t *testing.T) {
	g := mustCompile(t, e2eApp)
	// The field is marked @e2e in the IR.
	dm, _ := find(g.Entities, func(e ir.Entity) bool { return e.Name == "DM" })
	var sealed bool
	for _, f := range dm.Fields {
		if f.Name == "body" {
			sealed = f.E2E
		}
	}
	if !sealed {
		t.Fatalf("DM.body should be @e2e in the IR: %+v", dm.Fields)
	}
	// The action that writes it publishes `body` as a seal param, so the client
	// encrypts that argument before POSTing.
	send, _ := find(g.Actions, func(a ir.Action) bool { return a.Name == "send" })
	if len(send.Seal) != 1 || send.Seal[0] != "body" {
		t.Fatalf("send should seal the `body` param, got Seal=%v", send.Seal)
	}
	// The interpolation that reads it is marked sealed so the renderers placeholder
	// + open it.
	if !anyE2ESeg(g.Pages[0].View) {
		t.Errorf("the `{m.body}` interpolation should be a sealed (E2E) seg")
	}
}

func TestE2EFromNonParamRejected(t *testing.T) {
	// Writing the sealed field from a literal (anything the authority computes)
	// would let the server see plaintext — rejected. Only a sealed param is allowed.
	src := strings.Replace(e2eApp, `add DM { to: to, body: body }`, `add DM { to: to, body: "x" }`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "must be written") {
		t.Fatalf("expected an @e2e-from-non-param error, got %v", err)
	}
}

func TestE2ESealParamUsedElsewhereRejected(t *testing.T) {
	src := strings.Replace(e2eApp,
		`add DM { to: to, body: body }`,
		"check len(body) > 0 \"empty\"\n        add DM { to: to, body: body }", 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "cannot also be read") {
		t.Fatalf("expected a sealed-param-reused error, got %v", err)
	}
}

func TestE2EInAttributeRejected(t *testing.T) {
	src := strings.Replace(e2eApp, `text "{m.body}"`, `image "{m.body}"`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("expected a sealed-in-attribute error, got %v", err)
	}
}

func TestE2EInMetadataRejected(t *testing.T) {
	src := `app A:
    entity DM:
        id: int
        body: text @e2e
    view Home at "/":
        meta title "{DM(1).body}"
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("expected a sealed-in-metadata error, got %v", err)
	}
}

func TestE2ECombinedExpressionRejected(t *testing.T) {
	// A sealed value can't be concatenated — it must stand alone to be opened whole.
	src := strings.Replace(e2eApp, `text "{m.body}"`, `text "re: {m.body} end"`, 1)
	if _, err := String(src); err != nil {
		// standing in a literal-wrapped interpolation is fine; the seg splits cleanly.
		t.Fatalf("a sealed value beside literals is fine (separate segs): %v", err)
	}
	src = strings.Replace(e2eApp, `text "{m.body}"`, `text "{m.body + m.to}"`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "stand alone") {
		t.Fatalf("expected a stand-alone error for a concatenated sealed value, got %v", err)
	}
}

func TestSecretAndE2EConflictRejected(t *testing.T) {
	src := strings.Replace(e2eApp, `body: text @e2e`, `body: text @secret @e2e`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "cannot be both") {
		t.Fatalf("expected a @secret+@e2e conflict error, got %v", err)
	}
}

func TestE2ENonTextRejected(t *testing.T) {
	src := strings.Replace(e2eApp, `body: text @e2e`, `body: int @e2e`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "must be text") {
		t.Fatalf("expected an @e2e-must-be-text error, got %v", err)
	}
}

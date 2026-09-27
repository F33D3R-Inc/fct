package compile

import (
	"strings"
	"testing"
)

// `type`/`message` exist solely to describe a wire protocol as FCT source
// (SCHEMA_IDL_SCOPE.md Tier C) so Rust/Go/TS clients can be generated from one
// schema instead of hand-copied. This is the language-construct half of that
// feature; the codegen backends are tested separately.

const wireApp = `app A:
    type Coordinate:
        x: int
        y: int
        z: int
        q: int
    type Expr:
        kind: text
        val: json?
        obj: Expr?
        l: Expr?
        r: Expr?
        args: [Expr]?
    message TxOp:
        | insert_node(address: text, kind: text, at: Coordinate, data: json)
        | delete_node(address: text)
        | clear_kind(kind: text)
        | delete_where(kind: text, where: Expr?)
    view Home at "/":
        box:
            text "hi"
`

func TestTypeAndMessageCompile(t *testing.T) {
	g := mustCompile(t, wireApp)
	if len(g.Types) != 2 {
		t.Fatalf("expected 2 types, got %+v", g.Types)
	}
	if len(g.Messages) != 1 || g.Messages[0].Name != "TxOp" {
		t.Fatalf("expected message TxOp, got %+v", g.Messages)
	}
}

// A field naming another declared type is a Ref, boxed by codegen — never
// inlined. This is what makes Coordinate-in-a-message and self-referential
// Expr both representable.
func TestWireFieldRefFlag(t *testing.T) {
	g := mustCompile(t, wireApp)
	var exprType, coordType = -1, -1
	for i, ty := range g.Types {
		if ty.Name == "Expr" {
			exprType = i
		}
		if ty.Name == "Coordinate" {
			coordType = i
		}
	}
	if exprType < 0 || coordType < 0 {
		t.Fatalf("expected Coordinate and Expr types, got %+v", g.Types)
	}
	for _, f := range g.Types[exprType].Fields {
		switch f.Name {
		case "obj", "l", "r":
			if !f.Ref {
				t.Errorf("Expr.%s should be Ref (self-reference), got %+v", f.Name, f)
			}
		case "args":
			if !f.Ref || !f.List {
				t.Errorf("Expr.args should be Ref+List (list of self-reference), got %+v", f)
			}
		case "kind":
			if f.Ref || f.Type != "text" {
				t.Errorf("Expr.kind should be a plain text field, got %+v", f)
			}
		case "val":
			if f.Ref || f.Type != "json" || !f.Optional {
				t.Errorf("Expr.val should be an optional json field, got %+v", f)
			}
		}
	}
	for _, msg := range g.Messages {
		if msg.Name != "TxOp" {
			continue
		}
		if len(msg.Variants) != 4 {
			t.Fatalf("expected 4 TxOp variants, got %+v", msg.Variants)
		}
		var insertNode, deleteWhere bool
		for _, v := range msg.Variants {
			for _, f := range v.Fields {
				if v.Name == "insert_node" && f.Name == "at" {
					insertNode = f.Ref && f.Type == "Coordinate"
				}
				if v.Name == "delete_where" && f.Name == "where" {
					deleteWhere = f.Ref && f.Type == "Expr" && f.Optional
				}
			}
		}
		if !insertNode {
			t.Error("TxOp.insert_node.at should be a Ref to Coordinate")
		}
		if !deleteWhere {
			t.Error("TxOp.delete_where.where should be an optional Ref to Expr")
		}
	}
}

func TestMessageVariantMustBeLowercase(t *testing.T) {
	src := `app A:
    message TxOp:
        | InsertNode(address: text)
    view Home at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("expected a lowercase-discriminant error, got %v", err)
	}
}

func TestMessageDuplicateVariant(t *testing.T) {
	src := `app A:
    message TxOp:
        | delete_node(address: text)
        | delete_node(address: text)
    view Home at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "two variants") {
		t.Fatalf("expected a duplicate-variant error, got %v", err)
	}
}

func TestTypeUnknownFieldType(t *testing.T) {
	src := `app A:
    type Thing:
        x: NotDeclared
    view Home at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected an unknown-type error, got %v", err)
	}
}

func TestTypeNameCollisionWithMessage(t *testing.T) {
	src := `app A:
    type Thing:
        x: int
    message Thing:
        | a(x: int)
    view Home at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "redeclared") {
		t.Fatalf("expected a redeclaration error, got %v", err)
	}
}

// Record's flatness is untouched by this feature: a record field still may
// not name another record. This test exists to pin that Record and Type stay
// deliberately different constructs, not to re-test Record itself.
func TestRecordStaysFlatDespiteType(t *testing.T) {
	src := `app A:
    type Thing:
        x: int
    record Holder:
        t: Thing
    view Home at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("expected a record-must-be-flat error, got %v", err)
	}
}

// `auth <scheme> basic <id> <secret>` and `body form` are validated like
// the bearer clause: the credential binds text parameters that are not path
// parameters, a GET carries no form, and a form never doubles a wire body.
func TestAPIBasicAndFormValidation(t *testing.T) {
	base := `app A:
    type T:
        x: text
    action ex(client_id: text, client_secret: text, code: text) -> T:
        return T{x: code}
    action get1(id: int) -> T:
        return T{x: "" + id}
    %s
    view Home at "/":
        text "hi"
`
	ok := `api POST "/api/token" -> ex auth dev_client basic client_id client_secret:
        body form`
	if _, err := String(strings.Replace(base, "%s", ok, 1)); err != nil {
		t.Fatalf("a basic + form route must compile: %v", err)
	}
	cases := []struct{ decl, want string }{
		{`api POST "/api/token" -> ex auth dev_client basic client_id nope`, `basic credential parameter "nope" must be a text parameter`},
		{`api POST "/api/token" -> ex auth dev_client basic client_id client_id`, `bind to two different parameters`},
		{`api POST "/api/token" -> ex auth bearer basic client_id client_secret`, `reserved`},
		{`api GET "/api/one/{id}" -> get1:
        body form`, "a GET carries no request body"},
		{`api POST "/api/token" -> ex auth dev_client basic client_id client_secret:
        body form
        body T`, "two different request bodies"},
		{`api POST "/api/token" -> ex auth dev_client bearer`, "api auth clause is"},
	}
	for _, c := range cases {
		_, err := String(strings.Replace(base, "%s", c.decl, 1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s\n  err = %v, want %q", c.decl, err, c.want)
		}
	}
}

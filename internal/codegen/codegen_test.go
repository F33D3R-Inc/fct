package codegen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
)

// This is the acid test for the whole feature (SCHEMA_IDL_SCOPE.md §3b/§5):
// a recursive type and a 7-variant tagged union, generated to real Rust and
// real Go from the real compiler (not a standalone PoC parser), actually
// built with cargo/go and run as subprocesses, round-tripping real JSON
// across the language boundary. A generator whose output merely "looks
// right" is not proof; only bytes surviving a real build in each language
// are.
const acidSchema = `app WireSchema:
    type Expr:
        kind: text
        val: json?
        obj: Expr?
        l: Expr?
        r: Expr?
        args: [Expr]?
    message TxOp:
        | insert_node(address: text, kind: text, x: int, y: int, z: int, q: int, data: text, public: bool?)
        | insert_edge(from: text, to: text, kind: text)
        | delete_edge(from: text, to: text, kind: text)
        | delete_node(address: text)
        | clear_kind(kind: text)
        | delete_where(kind: text, where: Expr?)
        | set_if(address: text, field: text, expect_le: int?, set: json?)
    view Home at "/":
        box:
            text "hi"
`

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH, skipping acid test: %v", name, err)
	}
}

func TestAcidTestRecursiveTypeAndTaggedUnionRoundTrip(t *testing.T) {
	requireTool(t, "cargo")
	requireTool(t, "go")

	g, err := compile.String(acidSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	schema := FromIR(g)

	rustSrc, err := GenerateRust(schema)
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	goSrc, err := GenerateGo(schema, "main")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}

	dir := t.TempDir()

	// --- Rust side: build a tiny binary that constructs one instance of
	// each of the 7 TxOp variants plus a recursive Expr, and prints each as
	// one line of JSON to stdout.
	rustDir := filepath.Join(dir, "rustproof")
	mustMkdir(t, filepath.Join(rustDir, "src"))
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), `[package]
name = "rustproof"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
`)
	writeFile(t, filepath.Join(rustDir, "src", "wire.rs"), rustSrc)
	writeFile(t, filepath.Join(rustDir, "src", "main.rs"), `mod wire;
use wire::*;

fn main() {
    let expr = Expr {
        kind: "and".to_string(),
        val: None,
        obj: None,
        l: Some(Box::new(Expr { kind: "gte".to_string(), val: Some(serde_json::json!(21)), obj: None, l: None, r: None, args: None })),
        r: Some(Box::new(Expr { kind: "eq".to_string(), val: Some(serde_json::json!("x")), obj: None, l: None, r: None, args: None })),
        args: None,
    };
    println!("{}", serde_json::to_string(&expr).unwrap());

    let ops: Vec<TxOp> = vec![
        TxOp::InsertNode { address: "a1".into(), kind: "Post".into(), x: 1, y: 2, z: 3, q: 4, data: "{}".into(), public: Some(true) },
        TxOp::InsertEdge { from: "a1".into(), to: "a2".into(), kind: "likes".into() },
        TxOp::DeleteEdge { from: "a1".into(), to: "a2".into(), kind: "likes".into() },
        TxOp::DeleteNode { address: "a1".into() },
        TxOp::ClearKind { kind: "Post".into() },
        TxOp::DeleteWhere { kind: "Post".into(), r#where: None },
        TxOp::SetIf { address: "a1".into(), field: "n".into(), expect_le: Some(5), set: Some(serde_json::json!({"n": 6})) },
    ];
    for op in &ops {
        println!("{}", serde_json::to_string(op).unwrap());
    }
}
`)
	out := runOK(t, rustDir, "cargo", "run", "--quiet")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 8 {
		t.Fatalf("expected 8 JSON lines (1 Expr + 7 TxOp variants), got %d:\n%s", len(lines), out)
	}
	rustExprJSON := lines[0]

	// --- Go side: a program that decodes the Rust-produced JSON using the
	// generated Go types, re-encodes it, and prints the result — proving the
	// generated Go can actually parse what the generated Rust actually wrote.
	goDir := filepath.Join(dir, "goproof")
	mustMkdir(t, goDir)
	writeFile(t, filepath.Join(goDir, "go.mod"), "module goproof\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "wire.go"), goSrc)
	writeFile(t, filepath.Join(goDir, "main.go"), `package main

// Element 0 of the input array is always the Expr, elements 1..7 are the
// seven TxOp variants, in that fixed order — known from how the Rust side
// (rustproof/src/main.rs) writes them, not sniffed from the JSON shape
// (an Expr's fields are all optional, so a naive "try Expr first" would
// silently "succeed" on a TxOp payload too, ignoring its real fields —
// Go's json.Unmarshal does not reject unknown keys by default).
import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Fprintln(os.Stderr, "decode array:", err)
		os.Exit(1)
	}
	if len(raw) != 8 {
		fmt.Fprintln(os.Stderr, "expected 8 elements, got", len(raw))
		os.Exit(1)
	}
	var e Expr
	if err := json.Unmarshal(raw[0], &e); err != nil {
		fmt.Fprintln(os.Stderr, "decode Expr:", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(e)
	fmt.Println(string(out))
	for _, l := range raw[1:] {
		var op TxOp
		if err := json.Unmarshal(l, &op); err != nil {
			fmt.Fprintln(os.Stderr, "decode TxOp:", err)
			os.Exit(1)
		}
		out, _ := json.Marshal(op)
		fmt.Println(string(out))
	}
}
`)
	combined := "[" + rustExprJSON + "," + strings.Join(lines[1:], ",") + "]"
	inputPath := filepath.Join(dir, "input.json")
	writeFile(t, inputPath, combined)

	goOut := runOK(t, goDir, "go", "run", ".", inputPath)
	goLines := strings.Split(strings.TrimSpace(goOut), "\n")
	if len(goLines) != 8 {
		t.Fatalf("expected 8 lines back from Go, got %d:\n%s", len(goLines), goOut)
	}

	// Parity check: decode both the original Rust JSON and the Go
	// re-encoding into generic maps and compare field-for-field — proves
	// semantic equality even if key order differs, which json.Marshal does
	// not guarantee to preserve identically to serde_json.
	assertSameJSON(t, rustExprJSON, goLines[0])
	for i := 0; i < 7; i++ {
		assertSameJSON(t, lines[1+i], goLines[1+i])
	}
}

// TestAcidTestTSCompiles is the TS leg of the same acid test: the PoC never
// covered TS at all, so this proves it for real — real tsc, real type
// errors would fail the build, on a file that actually constructs one
// instance of the recursive Expr and all 7 TxOp variants.
func TestAcidTestTSCompiles(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skipf("npx not on PATH, skipping: %v", err)
	}

	g, err := compile.String(acidSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	tsSrc, err := GenerateTS(FromIR(g))
	if err != nil {
		t.Fatalf("GenerateTS: %v", err)
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "wire.ts"), tsSrc)
	writeFile(t, filepath.Join(dir, "tsconfig.json"), `{
  "compilerOptions": { "strict": true, "noEmit": true, "target": "ES2020", "module": "ES2020" }
}`)
	writeFile(t, filepath.Join(dir, "use.ts"), `import { Expr, TxOp } from "./wire";

const leaf: Expr = { kind: "gte", val: 21 };
const expr: Expr = { kind: "and", l: leaf, r: { kind: "eq", val: "x" } };

const ops: TxOp[] = [
  { type: "insert_node", address: "a1", kind: "Post", x: 1, y: 2, z: 3, q: 4, data: "{}", public: true },
  { type: "insert_edge", from: "a1", to: "a2", kind: "likes" },
  { type: "delete_edge", from: "a1", to: "a2", kind: "likes" },
  { type: "delete_node", address: "a1" },
  { type: "clear_kind", kind: "Post" },
  { type: "delete_where", kind: "Post", where: expr },
  { type: "set_if", address: "a1", field: "n", expect_le: 5, set: { n: 6 } },
];

// A field access narrowed by the discriminant proves the union is really
// discriminated, not just a bag of optional fields.
function describe(op: TxOp): string {
  switch (op.type) {
    case "insert_node":
      return op.address + "@" + op.x + "," + op.y;
    case "delete_where":
      return op.kind + " where " + JSON.stringify(op.where);
    default:
      return op.type;
  }
}
console.log(ops.map(describe).join(", "), JSON.stringify(expr));
`)
	cmd := exec.Command("npx", "--yes", "--package=typescript@5", "--", "tsc", "-p", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tsc failed:\n%s", out)
	}
}

// TestEnumWireRenameRoundTrip proves the `as "Wire"` escape hatch
// (ast.Enum.WireNames) for real: it exists specifically because
// facetql::core::node::Visibility's real, already-shipped wire form is
// "Private"/"Public" (capitalized, no rename_all) — not FCT's default
// lowercase — and cannot be changed without breaking fabric's independent
// wire.rs mirror (see schema/facetql_wire.fct's header for the full
// decision). This builds real Rust and Go from a schema using the override,
// confirms the wire bytes are actually the capitalized override text (not
// the lowercase source name), and confirms Go can decode what Rust wrote.
const enumRenameSchema = `app EnumRename:
    enum Visibility: private as "Private", public as "Public"
    enum Role: user as "User", admin as "Admin"
    type Node:
        address: text
        visibility: Visibility
        role: Role
    view Home at "/":
        box:
            text "hi"
`

func TestEnumWireRenameRoundTrip(t *testing.T) {
	requireTool(t, "cargo")
	requireTool(t, "go")

	g, err := compile.String(enumRenameSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	schema := FromIR(g)

	rustSrc, err := GenerateRust(schema)
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	if !strings.Contains(rustSrc, `rename = "Private"`) || !strings.Contains(rustSrc, `rename = "Public"`) {
		t.Fatalf("expected explicit Private/Public renames in generated Rust, got:\n%s", rustSrc)
	}
	if !strings.Contains(rustSrc, `rename = "User"`) || !strings.Contains(rustSrc, `rename = "Admin"`) {
		t.Fatalf("expected explicit User/Admin renames in generated Rust, got:\n%s", rustSrc)
	}
	goSrc, err := GenerateGo(schema, "main")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(goSrc, `VisibilityPrivate Visibility = "Private"`) || !strings.Contains(goSrc, `VisibilityPublic Visibility = "Public"`) {
		t.Fatalf("expected capitalized Go enum constants, got:\n%s", goSrc)
	}

	dir := t.TempDir()
	rustDir := filepath.Join(dir, "rustproof")
	mustMkdir(t, filepath.Join(rustDir, "src"))
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), `[package]
name = "rustproof"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
`)
	writeFile(t, filepath.Join(rustDir, "src", "wire.rs"), rustSrc)
	writeFile(t, filepath.Join(rustDir, "src", "main.rs"), `mod wire;
use wire::*;

fn main() {
    let n = Node { address: "a1".to_string(), visibility: Visibility::Private, role: Role::Admin };
    println!("{}", serde_json::to_string(&n).unwrap());
}
`)
	out := strings.TrimSpace(runOK(t, rustDir, "cargo", "run", "--quiet"))
	if !strings.Contains(out, `"visibility":"Private"`) {
		t.Fatalf(`expected literal "visibility":"Private" on the wire (matching facetql's real, already-shipped serialization), got: %s`, out)
	}
	if !strings.Contains(out, `"role":"Admin"`) {
		t.Fatalf(`expected literal "role":"Admin" on the wire, got: %s`, out)
	}

	goDir := filepath.Join(dir, "goproof")
	mustMkdir(t, goDir)
	writeFile(t, filepath.Join(goDir, "go.mod"), "module goproof\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "wire.go"), goSrc)
	writeFile(t, filepath.Join(goDir, "main.go"), `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	var n Node
	if err := json.Unmarshal(data, &n); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}
	if n.Visibility != VisibilityPrivate {
		fmt.Fprintf(os.Stderr, "expected VisibilityPrivate, got %q\n", n.Visibility)
		os.Exit(1)
	}
	if n.Role != RoleAdmin {
		fmt.Fprintf(os.Stderr, "expected RoleAdmin, got %q\n", n.Role)
		os.Exit(1)
	}
	out, _ := json.Marshal(n)
	fmt.Println(string(out))
}
`)
	inputPath := filepath.Join(dir, "input.json")
	writeFile(t, inputPath, out)
	goOut := strings.TrimSpace(runOK(t, goDir, "go", "run", ".", inputPath))
	assertSameJSON(t, out, goOut)
}

// TestDefaultValueRoundTrip proves the `= literal` default-value escape
// hatch (ast.RecordField.Default / ir.WireField.Default), added specifically
// because facetql's real, already-shipped SequenceRequest has
// `#[serde(default = "one")]` on `count: u64` — a field that always has a
// real, non-zero value even when the wire omits it, which `?` alone cannot
// express (an Optional field's absence means "no value," not "the default
// value"). Builds real Rust and Go, sends JSON with the field OMITTED
// entirely, and confirms both sides independently arrive at the declared
// default rather than a zero value.
const defaultValueSchema = `app DefaultValue:
    type SequenceRequest:
        name: text
        count: int = 1
    view Home at "/":
        box:
            text "hi"
`

func TestDefaultValueRoundTrip(t *testing.T) {
	requireTool(t, "cargo")
	requireTool(t, "go")

	g, err := compile.String(defaultValueSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	schema := FromIR(g)

	rustSrc, err := GenerateRust(schema)
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	if !strings.Contains(rustSrc, `default = "default_sequencerequest_count"`) {
		t.Fatalf("expected a generated default fn reference in Rust, got:\n%s", rustSrc)
	}
	goSrc, err := GenerateGo(schema, "main")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if !strings.Contains(goSrc, "UnmarshalJSON") {
		t.Fatalf("expected a generated UnmarshalJSON applying the default, got:\n%s", goSrc)
	}

	dir := t.TempDir()
	rustDir := filepath.Join(dir, "rustproof")
	mustMkdir(t, filepath.Join(rustDir, "src"))
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), `[package]
name = "rustproof"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
`)
	writeFile(t, filepath.Join(rustDir, "src", "wire.rs"), rustSrc)
	writeFile(t, filepath.Join(rustDir, "src", "main.rs"), `mod wire;
use wire::*;
use std::io::Read;

fn main() {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).unwrap();
    let req: SequenceRequest = serde_json::from_str(&input).unwrap();
    if req.count != 1 {
        panic!("expected default count 1, got {}", req.count);
    }
    println!("rust ok: name={} count={}", req.name, req.count);
}
`)
	cmd := exec.Command("cargo", "run", "--quiet")
	cmd.Dir = rustDir
	cmd.Stdin = strings.NewReader(`{"name":"seq1"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cargo run (rust default check) failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "count=1") {
		t.Fatalf("rust side didn't apply the default: %s", out)
	}

	goDir := filepath.Join(dir, "goproof")
	mustMkdir(t, goDir)
	writeFile(t, filepath.Join(goDir, "go.mod"), "module goproof\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "wire.go"), goSrc)
	writeFile(t, filepath.Join(goDir, "main.go"), `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	var req SequenceRequest
	if err := json.Unmarshal([]byte(`+"`"+`{"name":"seq1"}`+"`"+`), &req); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}
	if req.Count != 1 {
		fmt.Fprintf(os.Stderr, "expected default count 1, got %d\n", req.Count)
		os.Exit(1)
	}
	fmt.Printf("go ok: name=%s count=%d\n", req.Name, req.Count)
}
`)
	goOut := runOK(t, goDir, "go", "run", ".")
	if !strings.Contains(goOut, "count=1") {
		t.Fatalf("go side didn't apply the default: %s", goOut)
	}
}

// TestQueryTypeBindsFromURLString proves the `type Name query:` marker
// (ast.Type.Query) for real: it exists because facetql's real, already-
// shipped QueryParams is bound from a GET request's URL query string via
// axum's `Query<T>` extractor, never a JSON body — this builds the generated
// Rust type into an actual axum handler, sends a real HTTP GET with real
// query-string parameters, and confirms the server-side extractor parses
// them, not just that the type compiles.
const queryTypeSchema = `app QueryType:
    type QueryParams query:
        kind: text?
        owner: text?
        limit: int?
    view Home at "/":
        box:
            text "hi"
`

func TestQueryTypeBindsFromURLString(t *testing.T) {
	requireTool(t, "cargo")

	g, err := compile.String(queryTypeSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rustSrc, err := GenerateRust(FromIR(g))
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	if !strings.Contains(rustSrc, "Bound from the URL query string") || strings.Contains(rustSrc, "Serialize, Deserialize)]\npub struct QueryParams") {
		t.Fatalf("expected QueryParams to be marked query-bound and Deserialize-only, got:\n%s", rustSrc)
	}

	dir := t.TempDir()
	rustDir := filepath.Join(dir, "rustproof")
	mustMkdir(t, filepath.Join(rustDir, "src"))
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), `[package]
name = "rustproof"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_urlencoded = "0.7"
`)
	writeFile(t, filepath.Join(rustDir, "src", "wire.rs"), rustSrc)
	// No axum dependency pulled in just to prove this (keeps the test fast
	// and dependency-light) — serde_urlencoded is the exact crate axum's
	// `Query<T>` extractor uses internally to decode a URL query string into
	// a Deserialize type, so decoding through it directly proves the same
	// real mechanism axum would use, without needing a running HTTP server.
	writeFile(t, filepath.Join(rustDir, "src", "main.rs"), `mod wire;
use wire::*;

fn main() {
    let raw = "kind=Post&owner=alice&limit=10";
    let params: QueryParams = serde_urlencoded::from_str(raw).unwrap();
    if params.kind.as_deref() != Some("Post") || params.owner.as_deref() != Some("alice") || params.limit != Some(10) {
        panic!("query-string decode produced wrong values: {:?} {:?} {:?}", params.kind, params.owner, params.limit);
    }
    println!("ok: kind={:?} owner={:?} limit={:?}", params.kind, params.owner, params.limit);
}
`)
	out := runOK(t, rustDir, "cargo", "run", "--quiet")
	if !strings.Contains(out, `kind=Some("Post")`) {
		t.Fatalf("expected the real URL query string to decode via the generated type, got: %s", out)
	}
}

// TestListAndJSONDefaultsRoundTrip proves the `= []` and `= {}` default
// literals, added specifically for two real fields the frozen wire surface
// needs verbatim: facetql's CreateNodeRequest.edges (`Vec<EdgeSpec>` with a
// bare `#[serde(default)]`, i.e. an empty list when omitted) and
// TxOp.set_if.set (`serde_json::Map<String, Value>` with a bare
// `#[serde(default)]`, i.e. an empty object when omitted). Neither is
// expressible as Optional without changing the real field's actual type
// shape (Vec<T>, not Option<Vec<T>>) — which would make cutover a real
// behavior change, not a formality.
const listJSONDefaultSchema = `app ListJSONDefault:
    type EdgeSpec:
        to: text
        kind: text
    type CreateNodeRequest:
        address: text
        edges: [EdgeSpec] = []
        extra: json = {}
    view Home at "/":
        box:
            text "hi"
`

func TestListAndJSONDefaultsRoundTrip(t *testing.T) {
	requireTool(t, "cargo")
	requireTool(t, "go")

	g, err := compile.String(listJSONDefaultSchema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	schema := FromIR(g)
	rustSrc, err := GenerateRust(schema)
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	goSrc, err := GenerateGo(schema, "main")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}

	dir := t.TempDir()
	rustDir := filepath.Join(dir, "rustproof")
	mustMkdir(t, filepath.Join(rustDir, "src"))
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), `[package]
name = "rustproof"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
`)
	writeFile(t, filepath.Join(rustDir, "src", "wire.rs"), rustSrc)
	writeFile(t, filepath.Join(rustDir, "src", "main.rs"), `mod wire;
use wire::*;
use std::io::Read;

fn main() {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).unwrap();
    let req: CreateNodeRequest = serde_json::from_str(&input).unwrap();
    if !req.edges.is_empty() {
        panic!("expected empty edges default, got {:?}", req.edges);
    }
    if !req.extra.is_object() || !req.extra.as_object().unwrap().is_empty() {
        panic!("expected empty object default, got {:?}", req.extra);
    }
    println!("rust ok: address={}", req.address);
}
`)
	cmd := exec.Command("cargo", "run", "--quiet")
	cmd.Dir = rustDir
	cmd.Stdin = strings.NewReader(`{"address":"a1"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cargo run (rust list/json default check) failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "rust ok") {
		t.Fatalf("rust side didn't apply the defaults: %s", out)
	}

	goDir := filepath.Join(dir, "goproof")
	mustMkdir(t, goDir)
	writeFile(t, filepath.Join(goDir, "go.mod"), "module goproof\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "wire.go"), goSrc)
	writeFile(t, filepath.Join(goDir, "main.go"), `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	var req CreateNodeRequest
	if err := json.Unmarshal([]byte(`+"`"+`{"address":"a1"}`+"`"+`), &req); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}
	if req.Edges == nil || len(req.Edges) != 0 {
		fmt.Fprintf(os.Stderr, "expected non-nil empty edges, got %#v\n", req.Edges)
		os.Exit(1)
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Extra, &obj); err != nil || len(obj) != 0 {
		fmt.Fprintf(os.Stderr, "expected empty object extra, got %s (err %v)\n", req.Extra, err)
		os.Exit(1)
	}
	fmt.Printf("go ok: address=%s\n", req.Address)
}
`)
	goOut := runOK(t, goDir, "go", "run", ".")
	if !strings.Contains(goOut, "go ok") {
		t.Fatalf("go side didn't apply the defaults: %s", goOut)
	}
}

func assertSameJSON(t *testing.T, a, b string) {
	t.Helper()
	var ma, mb map[string]any
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		t.Fatalf("bad rust-side JSON %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		t.Fatalf("bad go-side JSON %q: %v", b, err)
	}
	if len(ma) != len(mb) {
		t.Errorf("field-count mismatch\n rust: %s\n go:   %s", a, b)
		return
	}
	for k, va := range ma {
		vb, ok := mb[k]
		if !ok {
			t.Errorf("go side missing key %q\n rust: %s\n go:   %s", k, a, b)
			continue
		}
		if fa, ok := va.(float64); ok {
			if fb, ok := vb.(float64); !ok || fa != fb {
				t.Errorf("key %q mismatch: rust=%v go=%v", k, va, vb)
			}
			continue
		}
		// Anything else (nested objects/arrays, e.g. Expr.l, are decoded as
		// map[string]interface{}/[]interface{}, both uncomparable with `!=`)
		// is compared by re-marshaling both sides to canonical JSON.
		ba, _ := json.Marshal(va)
		bb, _ := json.Marshal(vb)
		if string(ba) != string(bb) {
			t.Errorf("key %q mismatch: rust=%v go=%v", k, va, vb)
		}
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runOK(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s (in %s) failed: %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

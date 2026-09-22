package selfhost

import (
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// Ground truth for every expectation below is facetql/src/storage/
// transaction.rs and facetql/src/storage/commit.rs themselves, plus the
// constants and messages they reach into: btree.rs `MAX_KEY_LEN` (1024),
// index.rs `MAX_INDEX_NAME_LEN` (64) / `MAX_INDEX_VALUE_LEN` (512) and
// the `check_*` messages, text.rs `GRAM_LEN` (3) / `MAX_TEXT_VALUE_LEN`
// (16384), wal.rs's three `AtomicU64::new(1)` counters and its
// `STANDALONE_TRANSACTION_ID` guards, and checkpoint.rs's fence set.
// Neither Rust file has a `#[cfg(test)]` module, so every case is a
// hand trace of the source: the key lengths are computed from the key
// encodings in index.rs (`component` = 2-byte length prefix + bytes;
// history key = component(address) + 8; kind/owner key = component +
// raw address; edge keys = component + component + raw endpoint) and
// the frame traces from the exact order commit.rs allocates ids and
// appends records.

func compileTransactionApp(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.File(filepath.Join("transaction.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/transaction.fct: %v", err)
	}
	return g
}

// A fresh runtime per case: an action reports a state cell only when it
// changed from the previous value, so cases whose answers coincide must
// not share a server.
func freshTransactionApp(t *testing.T, g *ir.IR) *httptest.Server {
	t.Helper()
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func boolSliceOrEmpty(t *testing.T, v any) []bool {
	t.Helper()
	if v == nil {
		return []bool{}
	}
	return boolSliceFromAny(t, v)
}

func assertBoolSlice(t *testing.T, got, want []bool, name string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}

// The driver scripts: one operation per line, fields tab-separated.
func opLine(fields ...string) string { return strings.Join(fields, "\t") }
func script(lines ...string) string  { return strings.Join(lines, "\n") }

const maxKeyLen = 1024

// ---------------------------------------------------------------
// Operation::validate, rule by rule
// ---------------------------------------------------------------

func TestTransactionOperationValidate(t *testing.T) {
	g := compileTransactionApp(t)
	rep := strings.Repeat

	cases := []struct {
		name     string
		ops      string
		declared string
		wantOk   bool
		wantErr  string
	}{
		// --- Archive -> check_history_keys ---
		{
			name:   "archive: admissible keys pass",
			ops:    opLine("archive", "h", "3", "h", "K", "alice", "{}", "private"),
			wantOk: true,
		},
		{
			// history key = 2 + 1015 + 8 = 1025 > MAX_KEY_LEN, checked before
			// the archived node's own keys (whose primary key, 1015, would pass).
			name:    "archive: the history key is measured with its fixed 8-byte version suffix",
			ops:     opLine("archive", rep("h", 1015), "1", rep("h", 1015), "K", "o", "{}", "private"),
			wantErr: "the history index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// A history entry carries a whole node, held to the same
			// admissibility as a live one.
			name:    "archive: the archived node's keys are checked too",
			ops:     opLine("archive", "h", "1", rep("n", 1025), "K", "o", "{}", "private"),
			wantErr: "the primary index key would be 1025 bytes; the maximum is 1024",
		},

		// --- Insert -> check_node_keys, check_data_keys, check_text_keys ---
		{
			name:   "insert: admissible keys and no declared index pass",
			ops:    opLine("insert", "a", "K", "alice", `{"title":"x"}`, "public"),
			wantOk: true,
		},
		{
			name:    "insert: an empty address is an empty primary key",
			ops:     opLine("insert", "", "K", "alice", "{}", "public"),
			wantErr: "the primary index key would be empty",
		},
		{
			name:    "insert: primary key over MAX_KEY_LEN",
			ops:     opLine("insert", rep("a", maxKeyLen+1), "K", "o", "{}", "private"),
			wantErr: "the primary index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// primary 1022 passes; kind key = 2 + 1 + 1022 = 1025 fails.
			name:    "insert: the composite kind key is what has the bound",
			ops:     opLine("insert", rep("a", 1022), "K", "o", "{}", "private"),
			wantErr: "the kind index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// kind "" -> kind key 1023; owner "ab" -> 2 + 2 + 1021 = 1025.
			name:    "insert: the owner key is checked after the kind key",
			ops:     opLine("insert", rep("a", 1021), "", "ab", "{}", "private"),
			wantErr: "the owner index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// encoded = rank(1) + 510 + terminator(2) = 513 > MAX_INDEX_VALUE_LEN.
			name:     "insert: a declared data index bounds the encoded value at 512",
			ops:      opLine("insert", "p1", "Post", "alice", `{"title":"`+rep("x", 510)+`"}`, "public"),
			declared: opLine("data", "by_title", "Post", "title", "false"),
			wantErr:  "field 'title' is 513 bytes encoded, over the 512-byte maximum for index 'by_title'",
		},
		{
			// 1 + 509 + 2 = 512 is not over; key = 512 + len("p1") = 514.
			name:     "insert: exactly 512 encoded bytes is admissible",
			ops:      opLine("insert", "p1", "Post", "alice", `{"title":"`+rep("x", 509)+`"}`, "public"),
			declared: opLine("data", "by_title", "Post", "title", "false"),
			wantOk:   true,
		},
		{
			name:     "insert: a data index over another kind is not consulted",
			ops:      opLine("insert", "p1", "Other", "alice", `{"title":"`+rep("x", 600)+`"}`, "public"),
			declared: opLine("data", "by_title", "Post", "title", "false"),
			wantOk:   true,
		},
		{
			// value 500 -> encoded 503 (under 512); key = 503 + 522 = 1025.
			// Node keys first: primary 522, kind 528, owner 525, history 532 — all pass.
			name:     "insert: the data key (value + address) is measured against MAX_KEY_LEN",
			ops:      opLine("insert", rep("p", 522), "Post", "alice", `{"title":"`+rep("x", 500)+`"}`, "public"),
			declared: opLine("data", "by_title", "Post", "title", "false"),
			wantErr:  "the 'by_title' index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// A composite value: rank + compact serialisation + terminator.
			// `["a","b"]` is 9 bytes -> 12; the key is 12 + 2.
			name:     "insert: a nested array value is encoded by its serialised form",
			ops:      opLine("insert", "p1", "Post", "alice", `{"tags": [ "a" , "b" ]}`, "public"),
			declared: opLine("data", "by_tags", "Post", "tags", "false"),
			wantOk:   true,
		},
		{
			// Malformed data decodes to "every field absent" (serde's
			// `from_str(..).ok()` is None): absent encodes to 3 bytes.
			name:     "insert: malformed data is absent for every declared index, never refused",
			ops:      opLine("insert", "p1", "Post", "alice", `{"title": "x`, "public"),
			declared: opLine("data", "by_title", "Post", "title", "false"),
			wantOk:   true,
		},
		{
			name:     "insert: a declared text index bounds the text at 16384 bytes",
			ops:      opLine("insert", "p1", "Post", "alice", `{"body":"`+rep("y", 16385)+`"}`, "public"),
			declared: opLine("text", "ft", "Post", "body"),
			wantErr:  "field 'body' holds 16385 bytes of text, over the 16384-byte maximum for the inverted index 'ft'. Every byte is a posting, so the bound is a bound on how much index writing one request may impose.",
		},
		{
			name:     "insert: exactly 16384 bytes of text is admissible",
			ops:      opLine("insert", "p1", "Post", "alice", `{"body":"`+rep("y", 16384)+`"}`, "public"),
			declared: opLine("text", "ft", "Post", "body"),
			wantOk:   true,
		},
		{
			// indexed_text is `as_str`: a number has no postings, so it is
			// skipped, not refused.
			name:     "insert: only a JSON string is text-indexed",
			ops:      opLine("insert", "p1", "Post", "alice", `{"body": 12345}`, "public"),
			declared: opLine("text", "ft", "Post", "body"),
			wantOk:   true,
		},
		{
			name:     "insert: the text-indexed field may be absent",
			ops:      opLine("insert", "p1", "Post", "alice", `{"other": "z"}`, "public"),
			declared: opLine("text", "ft", "Post", "body"),
			wantOk:   true,
		},
		{
			// Both loops run, data first: the data index passes (1+1+2+2 = 6),
			// then the text index refuses.
			name:     "insert: data keys are checked before text keys",
			ops:      opLine("insert", "p1", "Post", "alice", `{"title":"t","body":"`+rep("y", 16385)+`"}`, "public"),
			declared: script(opLine("data", "by_title", "Post", "title", "false"), opLine("text", "ft", "Post", "body")),
			wantErr:  "field 'body' holds 16385 bytes of text, over the 16384-byte maximum for the inverted index 'ft'. Every byte is a posting, so the bound is a bound on how much index writing one request may impose.",
		},

		// --- Delete -> check_key_admissible("primary", address) ---
		{
			name:    "delete: an empty address",
			ops:     opLine("delete", ""),
			wantErr: "the primary index key would be empty",
		},
		{
			name:    "delete: an oversized address",
			ops:     opLine("delete", rep("d", 1025)),
			wantErr: "the primary index key would be 1025 bytes; the maximum is 1024",
		},
		{
			name:   "delete: MAX_KEY_LEN itself is admissible",
			ops:    opLine("delete", rep("d", 1024)),
			wantOk: true,
		},

		// --- InsertEdge / DeleteEdge -> check_edge_keys ---
		{
			name:   "insert_edge: admissible",
			ops:    opLine("insert_edge", "a", "b", "follows", "alice"),
			wantOk: true,
		},
		{
			// out key = (2 + 1010) + (2 + 1) + 10 = 1025.
			name:    "insert_edge: the outgoing-edge composite key is bounded",
			ops:     opLine("insert_edge", rep("f", 1010), rep("t", 10), "k", "alice"),
			wantErr: "the outgoing-edge index key would be 1025 bytes; the maximum is 1024",
		},
		{
			name:    "delete_edge: the identity is held to the same keys",
			ops:     opLine("delete_edge", rep("f", 1010), rep("t", 10), "k"),
			wantErr: "the outgoing-edge index key would be 1025 bytes; the maximum is 1024",
		},
		{
			// (2 + 1009) + 3 + 10 = 1024: admissible.
			name:   "delete_edge: exactly MAX_KEY_LEN is admissible",
			ops:    opLine("delete_edge", rep("f", 1009), rep("t", 10), "k"),
			wantOk: true,
		},

		// --- InsertUser / RevokeUser: no key to admit ---
		{
			name:   "insert_user: never refused",
			ops:    opLine("insert_user", "", "", ""),
			wantOk: true,
		},
		{
			name:   "revoke_user: never refused",
			ops:    opLine("revoke_user", ""),
			wantOk: true,
		},

		// --- CreateIndex -> IndexDef::validate ---
		{
			name:   "create_index: a valid definition",
			ops:    opLine("create_index", rep("n", 64), "Post", "title", "true"),
			wantOk: true,
		},
		{
			name:    "create_index: an empty name",
			ops:     opLine("create_index", "", "Post", "title", "false"),
			wantErr: "index name must be 1..=64 bytes",
		},
		{
			name:    "create_index: a 65-byte name",
			ops:     opLine("create_index", rep("n", 65), "Post", "title", "false"),
			wantErr: "index name must be 1..=64 bytes",
		},
		{
			name:    "create_index: the name is a filename suffix, so a dot is a path",
			ops:     opLine("create_index", "a.b", "Post", "title", "false"),
			wantErr: "index name may contain only letters, digits, '_' and '-'",
		},
		{
			name:    "create_index: a non-ASCII name fails the alphabet",
			ops:     opLine("create_index", "né", "Post", "title", "false"),
			wantErr: "index name may contain only letters, digits, '_' and '-'",
		},
		{
			name:    "create_index: an empty kind",
			ops:     opLine("create_index", "by_title", "", "title", "false"),
			wantErr: "index kind must not be empty",
		},
		{
			name:    "create_index: an empty field",
			ops:     opLine("create_index", "by_title", "Post", "", "false"),
			wantErr: "index field must not be empty",
		},
		{
			name:   "drop_index: never refused",
			ops:    opLine("drop_index", ""),
			wantOk: true,
		},

		// --- CreateReference -> ReferenceDef::validate (reference.fct) ---
		{
			name:   "create_reference: a valid definition",
			ops:    opLine("create_reference", "comment_post", "Comment", "post", "Post", "false", "", "cascade"),
			wantOk: true,
		},
		{
			name:    "create_reference: the name alphabet",
			ops:     opLine("create_reference", "a/b", "Comment", "post", "Post", "false", "", "cascade"),
			wantErr: "reference name may contain only letters, digits, '_' and '-'",
		},
		{
			name:    "create_reference: an empty kind",
			ops:     opLine("create_reference", "r", "", "post", "Post", "false", "", "cascade"),
			wantErr: "reference kind must not be empty",
		},
		{
			name:    "create_reference: a field pointing a kind at itself",
			ops:     opLine("create_reference", "self", "Reply", "parent", "Reply", "true", "parent", "restrict"),
			wantErr: `reference "self" points Reply.parent at itself`,
		},
		{
			name:   "create_reference: a tree through the parent's address is fine",
			ops:    opLine("create_reference", "tree", "Reply", "parent", "Reply", "false", "", "cascade"),
			wantOk: true,
		},
		{
			name:   "drop_reference: never refused",
			ops:    opLine("drop_reference", "x"),
			wantOk: true,
		},

		// --- CreateTextIndex -> TextIndexDef::validate ---
		{
			name:   "create_text_index: a valid definition",
			ops:    opLine("create_text_index", "ft-body", "Post", "body"),
			wantOk: true,
		},
		{
			name:    "create_text_index: a space is not in the alphabet",
			ops:     opLine("create_text_index", "ft body", "Post", "body"),
			wantErr: "index name may contain only letters, digits, '_' and '-'",
		},
		{
			name:    "create_text_index: an empty field",
			ops:     opLine("create_text_index", "ft", "Post", ""),
			wantErr: "index field must not be empty",
		},
		{
			name:   "drop_text_index: never refused",
			ops:    opLine("drop_text_index", "ft"),
			wantOk: true,
		},

		// --- the batch rule and the degrade path ---
		{
			name:    "a batch is refused at its first inadmissible operation",
			ops:     script(opLine("insert", "a", "K", "o", "{}", "public"), opLine("delete", ""), opLine("delete", rep("d", 2000))),
			wantErr: "the primary index key would be empty",
		},
		{
			name:    "an unknown operation tag degrades to a refusal",
			ops:     opLine("bogus", "x"),
			wantErr: "unknown operation 'bogus'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshTransactionApp(t, g)
			d := postJSON(t, ts, "runValidate", c.ops, c.declared)
			gotOk := boolField(d["validateOk"])
			gotErr := textField(d["validateErr"], "")
			if gotOk != c.wantOk {
				t.Errorf("validateOk = %v, want %v (err %q)", gotOk, c.wantOk, gotErr)
			}
			if gotErr != c.wantErr {
				t.Errorf("validateErr = %q, want %q", gotErr, c.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------
// The JSON reader behind check_data_keys / check_text_keys
// ---------------------------------------------------------------

func TestTransactionJsonReader(t *testing.T) {
	g := compileTransactionApp(t)

	cases := []struct {
		name     string
		data     string
		field    string
		wantOk   bool
		wantKind string
		wantText string
		wantNum  float64
		wantBool bool
		wantLen  int // len(encode_order_value(value))
	}{
		{name: "a string", data: `{"a": "x"}`, field: "a", wantOk: true, wantKind: "text", wantText: "x", wantLen: 4},
		{
			// 1.5 is 0x3FF8000000000000; non-negative, so only the sign bit
			// flips: BF F8 00 00 00 00 00 00. append_escaped turns each 0x00
			// into 0x00 0xFF, so the body is 2 + 12 bytes: 1 + 14 + 2.
			name: "a number (its zero bytes are escaped)", data: `{"a": 1.5}`, field: "a",
			wantOk: true, wantKind: "num", wantNum: 1.5, wantLen: 17,
		},
		{
			// -2000.0 is 0xC09F400000000000; negative, so every bit flips:
			// 3F 60 BF FF FF FF FF FF — no zero byte to escape: 1 + 8 + 2.
			name: "an exponent number with surrounding whitespace", data: "  {\"n\": -2e3}\n", field: "n",
			wantOk: true, wantKind: "num", wantNum: -2000, wantLen: 11,
		},
		{name: "true", data: `{"a": true}`, field: "a", wantOk: true, wantKind: "bool", wantBool: true, wantLen: 4},
		// `u8::from(false)` is 0x00, which append_escaped doubles: 1 + 2 + 2.
		{name: "false (its payload byte is escaped)", data: `{"a":false}`, field: "a", wantOk: true, wantKind: "bool", wantBool: false, wantLen: 5},
		{name: "null", data: `{"a": null}`, field: "a", wantOk: true, wantKind: "null", wantLen: 3},
		{name: "a missing key is absent", data: `{"a": "x"}`, field: "b", wantOk: true, wantKind: "absent", wantLen: 3},
		{name: "an empty object", data: `{}`, field: "a", wantOk: true, wantKind: "absent", wantLen: 3},
		{name: "the last duplicate key wins", data: `{"a": "x", "a": "y"}`, field: "a", wantOk: true, wantKind: "text", wantText: "y", wantLen: 4},
		{
			// q " b \ c / d é(2 bytes) 😀(4 bytes) = 13 bytes -> 1 + 13 + 2.
			name: "escapes, \\u and a surrogate pair are decoded",
			data: `{"a": "q\"b\\c\/dé😀"}`, field: "a",
			wantOk: true, wantKind: "text", wantText: "q\"b\\c/dé\U0001F600", wantLen: 16,
		},
		{
			// a NUL inside a value is escaped to 0x00 0xFF by append_escaped:
			// 1 + (1 + 2 + 1) + 2.
			name: "a NUL byte in a string is escaped in the key",
			data: `{"s": "a\u0000b"}`, field: "s",
			wantOk: true, wantKind: "text", wantText: "a\x00b", wantLen: 7,
		},
		{
			// compact form `["a",{"b":1}]` is 13 bytes -> 1 + 13 + 2.
			name: "a nested composite keeps its compact serialisation",
			data: `{"tags": [ "a" , {"b": 1} ]}`, field: "tags",
			wantOk: true, wantKind: "composite", wantText: `["a",{"b":1}]`, wantLen: 16,
		},
		{name: "an unterminated string is malformed", data: `{"a": "x`, field: "a", wantOk: false, wantKind: "absent", wantLen: 3},
		{name: "trailing garbage is malformed", data: `{"a": "x"} trailing`, field: "a", wantOk: false, wantKind: "absent", wantLen: 3},
		{name: "a trailing comma is malformed", data: `{"a": "x", }`, field: "a", wantOk: false, wantKind: "absent", wantLen: 3},
		{name: "a top-level array has no fields", data: `[1, 2]`, field: "0", wantOk: false, wantKind: "absent", wantLen: 3},
		{name: "a bare scalar has no fields", data: `"x"`, field: "a", wantOk: false, wantKind: "absent", wantLen: 3},
		{name: "empty input has no fields", data: ``, field: "a", wantOk: false, wantKind: "absent", wantLen: 3},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshTransactionApp(t, g)
			d := postJSON(t, ts, "runJsonField", c.data, c.field)
			if got := boolField(d["jsonOk"]); got != c.wantOk {
				t.Errorf("jsonOk = %v, want %v", got, c.wantOk)
			}
			if got := textField(d["jsonKind"], ""); got != c.wantKind {
				t.Errorf("jsonKind = %q, want %q", got, c.wantKind)
			}
			if got := textField(d["jsonText"], ""); got != c.wantText {
				t.Errorf("jsonText = %q, want %q", got, c.wantText)
			}
			if got := boolField(d["jsonBool"]); got != c.wantBool {
				t.Errorf("jsonBool = %v, want %v", got, c.wantBool)
			}
			wantBits := int(int64(math.Float64bits(c.wantNum)))
			if got := intField(d["jsonNumBits"], 0); got != wantBits {
				t.Errorf("jsonNumBits = %d, want %d (%v)", got, wantBits, c.wantNum)
			}
			if got := intField(d["jsonEncodedLen"], 0); got != c.wantLen {
				t.Errorf("jsonEncodedLen = %d, want %d", got, c.wantLen)
			}
		})
	}
}

// ---------------------------------------------------------------
// Transaction::commit over StagedCommit: ids, ordering, both failure
// classes, and what recovery would find in the log afterwards
// ---------------------------------------------------------------

type commitWant struct {
	ok        bool
	errKind   string
	err       string
	sequence  int
	stage     string
	durable   bool
	applied   []int
	tags      []string
	seqs      []int
	txIds     []int
	opIds     []int
	payloads  []string
	fences    []int
	ceiling   int
	counters  []int
	skipOpIds bool
}

func TestTransactionCommitPipeline(t *testing.T) {
	g := compileTransactionApp(t)

	twoOps := script(
		opLine("insert", "a", "K", "alice", "{}", "public"),
		opLine("insert_edge", "a", "b", "follows", "alice"),
	)

	cases := []struct {
		name     string
		ops      string
		declared string
		failAt   string
		counters [3]int // nextSequence, nextOperationId, nextTransactionId
		want     commitWant
	}{
		{
			name:     "an empty transaction opens no frame and returns sequence 0",
			ops:      "",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: true, sequence: 0, stage: "empty", durable: false,
				applied: []int{}, tags: []string{}, seqs: []int{}, txIds: []int{}, opIds: []int{}, payloads: []string{},
				fences: []int{}, ceiling: -1, counters: []int{1, 1, 1},
			},
		},
		{
			// open: tx 1, BEGIN seq 1/op 1, fence {1}; stage: 2, 3; COMMIT 4;
			// apply both; settle releases the fence and reports 4.
			name:     "two operations: BEGIN, both mutations in order, COMMIT, settled",
			ops:      twoOps,
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: true, sequence: 4, stage: "settled", durable: true,
				applied: []int{0, 1},
				tags:    []string{"begin", "insert", "insert_edge", "commit"},
				seqs:    []int{1, 2, 3, 4}, txIds: []int{1, 1, 1, 1}, opIds: []int{1, 2, 3, 4},
				payloads: []string{"", "a", "a->b:follows", ""},
				fences:   []int{}, ceiling: -1, counters: []int{5, 5, 2},
			},
		},
		{
			// A lone operation still goes through the frame here: the
			// "exactly one => standalone" rule is the engine's apply_atomic,
			// not Transaction::commit.
			name:     "a single operation is framed too",
			ops:      opLine("insert", "a", "K", "alice", "{}", "public"),
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: true, sequence: 3, stage: "settled", durable: true,
				applied: []int{0},
				tags:    []string{"begin", "insert", "commit"},
				seqs:    []int{1, 2, 3}, txIds: []int{1, 1, 1}, opIds: []int{1, 2, 3},
				payloads: []string{"", "a", ""},
				fences:   []int{}, ceiling: -1, counters: []int{4, 4, 2},
			},
		},
		{
			name:     "the three counters are independent and start wherever the process left them",
			ops:      twoOps,
			counters: [3]int{10, 100, 7},
			want: commitWant{
				ok: true, sequence: 13, stage: "settled", durable: true,
				applied: []int{0, 1},
				tags:    []string{"begin", "insert", "insert_edge", "commit"},
				seqs:    []int{10, 11, 12, 13}, txIds: []int{7, 7, 7, 7}, opIds: []int{100, 101, 102, 103},
				payloads: []string{"", "a", "a->b:follows", ""},
				fences:   []int{}, ceiling: -1, counters: []int{14, 104, 8},
			},
		},
		{
			name: "an archive precedes the insert that supersedes it, in the staged order",
			ops: script(
				opLine("archive", "h", "3", "h", "K", "o", `{"v":1}`, "private"),
				opLine("insert", "h", "K", "o", `{"v":2}`, "private"),
			),
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: true, sequence: 4, stage: "settled", durable: true,
				applied: []int{0, 1},
				tags:    []string{"begin", "archive", "insert", "commit"},
				seqs:    []int{1, 2, 3, 4}, txIds: []int{1, 1, 1, 1}, opIds: []int{1, 2, 3, 4},
				payloads: []string{"", "h@3", "h", ""},
				fences:   []int{}, ceiling: -1, counters: []int{5, 5, 2},
			},
		},
		{
			name: "every resolved mutation variant lowers to its own WAL record",
			ops: script(
				opLine("archive", "h", "1", "h", "K", "o", "{}", "private"),
				opLine("insert", "h", "K", "o", "{}", "private"),
				opLine("delete", "d"),
				opLine("insert_edge", "a", "b", "k", "o"),
				opLine("delete_edge", "a", "b", "k"),
				opLine("insert_user", "hash1", "bob", "user"),
				opLine("revoke_user", "hash2"),
				opLine("create_index", "by_x", "K", "x", "false"),
				opLine("drop_index", "by_y"),
				opLine("create_reference", "r", "Comment", "post", "Post", "false", "", "cascade"),
				opLine("drop_reference", "r2"),
				opLine("create_text_index", "ft", "K", "body"),
				opLine("drop_text_index", "ft2"),
			),
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: true, sequence: 15, stage: "settled", durable: true,
				applied: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
				tags: []string{"begin", "archive", "insert", "delete", "insert_edge", "delete_edge", "insert_user", "revoke_user",
					"create_index", "drop_index", "create_reference", "drop_reference", "create_text_index", "drop_text_index", "commit"},
				seqs:  []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
				txIds: []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
				opIds: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
				payloads: []string{"", "h@1", "h", "d", "a->b:k", "a->b:k", "hash1", "hash2",
					"by_x", "by_y", "r", "r2", "ft", "ft2", ""},
				fences: []int{}, ceiling: -1, counters: []int{16, 16, 2},
			},
		},
		{
			// Admissibility runs for the whole batch before the frame is
			// opened: nothing is logged and no id is allocated.
			name:     "an inadmissible operation rejects the batch before anything is logged",
			ops:      script(opLine("insert", "a", "K", "o", "{}", "public"), opLine("delete", "")),
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "InvalidInput", err: "the primary index key would be empty",
				sequence: 0, stage: "rejected", durable: false,
				applied: []int{}, tags: []string{}, seqs: []int{}, txIds: []int{}, opIds: []int{}, payloads: []string{},
				fences: []int{}, ceiling: -1, counters: []int{1, 1, 1},
			},
		},
		{
			// wal::begin allocated a sequence and an operation id before the
			// failed append, and open() allocated the transaction id before
			// that; no frame exists, so no fence and no ABORT.
			name:     "BEGIN fails: ids are burned, no fence, no ABORT",
			ops:      twoOps,
			failAt:   "begin",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "Io", err: "injected I/O failure at begin",
				sequence: 0, stage: "open_failed", durable: false,
				applied: []int{}, tags: []string{}, seqs: []int{}, txIds: []int{}, opIds: []int{}, payloads: []string{},
				fences: []int{}, ceiling: -1, counters: []int{2, 2, 2},
			},
		},
		{
			// Crash before COMMIT: the frame is dropped — fence released,
			// best-effort ABORT (seq 4, after the burned 3). Recovery sees
			// BEGIN, one mutation, ABORT and no COMMIT: discarded.
			name:     "a staged append fails: the frame is dropped with an ABORT and no COMMIT",
			ops:      twoOps,
			failAt:   "stage:1",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "Io", err: "injected I/O failure at stage:1",
				sequence: 0, stage: "stage_failed", durable: false,
				applied: []int{},
				tags:    []string{"begin", "insert", "abort"},
				seqs:    []int{1, 2, 4}, txIds: []int{1, 1, 1}, opIds: []int{1, 2, 4},
				payloads: []string{"", "a", ""},
				fences:   []int{}, ceiling: -1, counters: []int{5, 5, 2},
			},
		},
		{
			name:     "the COMMIT append fails: every staged record survives but the frame is aborted",
			ops:      twoOps,
			failAt:   "commit",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "Io", err: "injected I/O failure at commit",
				sequence: 0, stage: "commit_failed", durable: false,
				applied: []int{},
				tags:    []string{"begin", "insert", "insert_edge", "abort"},
				seqs:    []int{1, 2, 3, 5}, txIds: []int{1, 1, 1, 1}, opIds: []int{1, 2, 3, 5},
				payloads: []string{"", "a", "a->b:follows", ""},
				fences:   []int{}, ceiling: -1, counters: []int{6, 6, 2},
			},
		},
		{
			// The best-effort ABORT may itself fail; its numbers are burned
			// and the missing COMMIT alone makes recovery discard the frame.
			name:     "a failed ABORT after a failed stage is ignored",
			ops:      twoOps,
			failAt:   "stage:0,abort",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "Io", err: "injected I/O failure at stage:0",
				sequence: 0, stage: "stage_failed", durable: false,
				applied: []int{},
				tags:    []string{"begin"},
				seqs:    []int{1}, txIds: []int{1}, opIds: []int{1},
				payloads: []string{""},
				fences:   []int{}, ceiling: -1, counters: []int{4, 4, 2},
			},
		},
		{
			// Crash after COMMIT: durably committed, NOT rolled back. The
			// frame is dropped unsettled, so the fence stays and the
			// checkpoint ceiling is pinned below BEGIN (1 - 1 = 0); no ABORT.
			// Recovery finds BEGIN … COMMIT and replays the batch.
			name:     "apply fails after COMMIT: committed, unsettled, fence kept",
			ops:      twoOps,
			failAt:   "apply:1",
			counters: [3]int{1, 1, 1},
			want: commitWant{
				ok: false, errKind: "Other", err: "injected apply failure at operation 1",
				sequence: 0, stage: "apply_failed", durable: true,
				applied: []int{0},
				tags:    []string{"begin", "insert", "insert_edge", "commit"},
				seqs:    []int{1, 2, 3, 4}, txIds: []int{1, 1, 1, 1}, opIds: []int{1, 2, 3, 4},
				payloads: []string{"", "a", "a->b:follows", ""},
				fences:   []int{1}, ceiling: 0, counters: []int{5, 5, 2},
			},
		},
		{
			name:     "apply fails on the first operation: nothing applied, still committed",
			ops:      twoOps,
			failAt:   "apply:0",
			counters: [3]int{5, 5, 3},
			want: commitWant{
				ok: false, errKind: "Other", err: "injected apply failure at operation 0",
				sequence: 0, stage: "apply_failed", durable: true,
				applied: []int{},
				tags:    []string{"begin", "insert", "insert_edge", "commit"},
				seqs:    []int{5, 6, 7, 8}, txIds: []int{3, 3, 3, 3}, opIds: []int{5, 6, 7, 8},
				payloads: []string{"", "a", "a->b:follows", ""},
				fences:   []int{5}, ceiling: 4, counters: []int{9, 9, 4},
			},
		},
		{
			// next_transaction_id handed out STANDALONE_TRANSACTION_ID:
			// wal::begin refuses before allocating a sequence.
			name:     "transaction id 0 is reserved for standalone operations",
			ops:      twoOps,
			counters: [3]int{1, 1, 0},
			want: commitWant{
				ok: false, errKind: "InvalidInput", err: "transaction ID 0 is reserved for standalone operations",
				sequence: 0, stage: "open_failed", durable: false,
				applied: []int{}, tags: []string{}, seqs: []int{}, txIds: []int{}, opIds: []int{}, payloads: []string{},
				fences: []int{}, ceiling: -1, counters: []int{1, 1, 1},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshTransactionApp(t, g)
			d := postJSON(t, ts, "runCommit", c.ops, c.declared, c.failAt, c.counters[0], c.counters[1], c.counters[2])
			w := c.want
			if got := boolField(d["commitOk"]); got != w.ok {
				t.Errorf("commitOk = %v, want %v", got, w.ok)
			}
			if got := textField(d["commitErrKind"], ""); got != w.errKind {
				t.Errorf("commitErrKind = %q, want %q", got, w.errKind)
			}
			if got := textField(d["commitErr"], ""); got != w.err {
				t.Errorf("commitErr = %q, want %q", got, w.err)
			}
			if got := intField(d["commitSequence"], 0); got != w.sequence {
				t.Errorf("commitSequence = %d, want %d", got, w.sequence)
			}
			if got := textField(d["commitStage"], ""); got != w.stage {
				t.Errorf("commitStage = %q, want %q", got, w.stage)
			}
			if got := boolField(d["commitDurable"]); got != w.durable {
				t.Errorf("commitDurable = %v, want %v", got, w.durable)
			}
			assertIntSlice(t, intSlice(t, d["commitApplied"]), w.applied, "commitApplied")
			assertTextSlice(t, textSlice(t, d["walTags"]), w.tags, "walTags")
			assertIntSlice(t, intSlice(t, d["walSequences"]), w.seqs, "walSequences")
			assertIntSlice(t, intSlice(t, d["walTransactionIds"]), w.txIds, "walTransactionIds")
			assertIntSlice(t, intSlice(t, d["walOperationIds"]), w.opIds, "walOperationIds")
			assertTextSlice(t, textSlice(t, d["walPayloads"]), w.payloads, "walPayloads")
			assertIntSlice(t, intSlice(t, d["walFences"]), w.fences, "walFences")
			if got := intField(d["walCeiling"], 0); got != w.ceiling {
				t.Errorf("walCeiling = %d, want %d", got, w.ceiling)
			}
			assertIntSlice(t, intSlice(t, d["walCountersAfter"]), w.counters, "walCountersAfter")
		})
	}
}

// ---------------------------------------------------------------
// StagedCommit's own guards, reached directly through the frame script
// ---------------------------------------------------------------

func TestCommitStagedFrameGuards(t *testing.T) {
	g := compileTransactionApp(t)

	cases := []struct {
		name      string
		script    string
		failAt    string
		wantOks   []bool
		wantKinds []string
		wantErrs  []string
		wantSeqs  []int
		wantTags  []string
		wantWalSq []int
		wantTxIds []int
		wantFence []int
		wantCount []int
	}{
		{
			name:      "the happy path: open, stage, commit, settle",
			script:    "open;stage insert;commit;settle",
			wantOks:   []bool{true, true, true, true},
			wantKinds: []string{"", "", "", ""},
			wantErrs:  []string{"", "", "", ""},
			wantSeqs:  []int{1, 2, 3, 3},
			wantTags:  []string{"begin", "insert", "commit"},
			wantWalSq: []int{1, 2, 3},
			wantTxIds: []int{1, 1, 1},
			wantFence: []int{},
			wantCount: []int{4, 4, 2},
		},
		{
			name:      "commit twice",
			script:    "open;commit;commit",
			wantOks:   []bool{true, true, false},
			wantKinds: []string{"", "", "InvalidInput"},
			wantErrs:  []string{"", "", "transaction already committed"},
			wantSeqs:  []int{1, 2, 0},
			wantTags:  []string{"begin", "commit"},
			wantWalSq: []int{1, 2},
			wantTxIds: []int{1, 1},
			wantFence: []int{1},
			wantCount: []int{3, 3, 2},
		},
		{
			name:      "stage after commit",
			script:    "open;commit;stage insert",
			wantOks:   []bool{true, true, false},
			wantKinds: []string{"", "", "InvalidInput"},
			wantErrs:  []string{"", "", "cannot stage a mutation after the frame has committed"},
			wantSeqs:  []int{1, 2, 0},
			wantTags:  []string{"begin", "commit"},
			wantWalSq: []int{1, 2},
			wantTxIds: []int{1, 1},
			wantFence: []int{1},
			wantCount: []int{3, 3, 2},
		},
		{
			// The guard fires before any id is allocated: counters untouched.
			name:      "control records cannot be staged",
			script:    "open;stage begin;stage commit;stage abort",
			wantOks:   []bool{true, false, false, false},
			wantKinds: []string{"", "InvalidInput", "InvalidInput", "InvalidInput"},
			wantErrs: []string{"",
				"transaction control records cannot be staged as mutations",
				"transaction control records cannot be staged as mutations",
				"transaction control records cannot be staged as mutations"},
			wantSeqs:  []int{1, 0, 0, 0},
			wantTags:  []string{"begin"},
			wantWalSq: []int{1},
			wantTxIds: []int{1},
			wantFence: []int{1},
			wantCount: []int{2, 2, 2},
		},
		{
			// settle(self) on an uncommitted frame: the error, and — the
			// frame having been consumed — the drop-guard: fence released,
			// ABORT appended.
			name:      "settle before commit",
			script:    "open;settle",
			wantOks:   []bool{true, false},
			wantKinds: []string{"", "InvalidInput"},
			wantErrs:  []string{"", "cannot settle a transaction that has not committed"},
			wantSeqs:  []int{1, 0},
			wantTags:  []string{"begin", "abort"},
			wantWalSq: []int{1, 2},
			wantTxIds: []int{1, 1},
			wantFence: []int{},
			wantCount: []int{3, 3, 2},
		},
		{
			name:      "dropping an uncommitted frame releases the fence and aborts",
			script:    "open;stage insert;drop",
			wantOks:   []bool{true, true, true},
			wantKinds: []string{"", "", ""},
			wantErrs:  []string{"", "", ""},
			wantSeqs:  []int{1, 2, 0},
			wantTags:  []string{"begin", "insert", "abort"},
			wantWalSq: []int{1, 2, 3},
			wantTxIds: []int{1, 1, 1},
			wantFence: []int{},
			wantCount: []int{4, 4, 2},
		},
		{
			name:      "dropping a committed, unsettled frame keeps the fence and writes nothing",
			script:    "open;commit;drop",
			wantOks:   []bool{true, true, true},
			wantKinds: []string{"", "", ""},
			wantErrs:  []string{"", "", ""},
			wantSeqs:  []int{1, 2, 0},
			wantTags:  []string{"begin", "commit"},
			wantWalSq: []int{1, 2},
			wantTxIds: []int{1, 1},
			wantFence: []int{1},
			wantCount: []int{3, 3, 2},
		},
		{
			// A failed staged append burns its sequence (2) and operation id;
			// the frame is still open, so the fence still stands.
			name:      "a failed stage burns its ids and leaves the frame open",
			script:    "open;stage delete;stage delete",
			failAt:    "stage:0",
			wantOks:   []bool{true, false, true},
			wantKinds: []string{"", "Io", ""},
			wantErrs:  []string{"", "injected I/O failure at stage:0", ""},
			wantSeqs:  []int{1, 2, 3},
			wantTags:  []string{"begin", "delete"},
			wantWalSq: []int{1, 3},
			wantTxIds: []int{1, 1},
			wantFence: []int{1},
			wantCount: []int{4, 4, 2},
		},
		{
			// Two open frames: two transaction ids, two fences, sorted.
			name:      "two frames register two fences",
			script:    "open;open",
			wantOks:   []bool{true, true},
			wantKinds: []string{"", ""},
			wantErrs:  []string{"", ""},
			wantSeqs:  []int{1, 2},
			wantTags:  []string{"begin", "begin"},
			wantWalSq: []int{1, 2},
			wantTxIds: []int{1, 2},
			wantFence: []int{1, 2},
			wantCount: []int{3, 3, 3},
		},
		{
			name:      "a step with no frame is the driver's own error",
			script:    "commit",
			wantOks:   []bool{false},
			wantKinds: []string{"Driver"},
			wantErrs:  []string{"no open frame"},
			wantSeqs:  []int{0},
			wantTags:  []string{},
			wantWalSq: []int{},
			wantTxIds: []int{},
			wantFence: []int{},
			wantCount: []int{1, 1, 1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshTransactionApp(t, g)
			d := postJSON(t, ts, "runFrameScript", c.script, c.failAt)
			assertBoolSlice(t, boolSliceOrEmpty(t, d["frameOks"]), c.wantOks, "frameOks")
			assertTextSlice(t, textSlice(t, d["frameErrKinds"]), c.wantKinds, "frameErrKinds")
			assertTextSlice(t, textSlice(t, d["frameErrs"]), c.wantErrs, "frameErrs")
			assertIntSlice(t, intSlice(t, d["frameSequences"]), c.wantSeqs, "frameSequences")
			assertTextSlice(t, textSlice(t, d["frameWalTags"]), c.wantTags, "frameWalTags")
			assertIntSlice(t, intSlice(t, d["frameWalSequences"]), c.wantWalSq, "frameWalSequences")
			assertIntSlice(t, intSlice(t, d["frameWalTransactionIds"]), c.wantTxIds, "frameWalTransactionIds")
			assertIntSlice(t, intSlice(t, d["frameFences"]), c.wantFence, "frameFences")
			assertIntSlice(t, intSlice(t, d["frameCounters"]), c.wantCount, "frameCounters")
		})
	}
}

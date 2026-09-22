package selfhost

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// Every expectation below is taken from facetql/src/database.rs itself:
// the literal exit codes (1/4/5, EXIT_ENGINE_POISONED = 6), the
// BROADCAST_CAPACITY/EVENT_REPLAY_CAPACITY constant (1024), the two word
// lists in `integrity_failure`, the four non-access `io::ErrorKind`s in
// `is_access_failure`, the `Display` sentences, and the ten Rust unit
// tests in `audience_tests` and `event_feed_tests`, translated one for
// one. The WAL operation-id counter (`AtomicU64::new(1)`, fetch_add
// returning the pre-increment value) is modelled by the `counter`
// argument: `EventFeed::new` mints `earliest_resumable` from it.

func compileDatabaseApp(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.File(filepath.Join("database.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/database.fct: %v", err)
	}
	return g
}

// A fresh runtime per case: an action reports a state cell as a delta
// only when it CHANGED from the session's previous value, so reusing one
// server across cases whose answers coincide would hide a result.
func freshDatabaseApp(t *testing.T, g *ir.IR) *httptest.Server {
	t.Helper()
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func textField(v any, deflt string) string {
	s, ok := v.(string)
	if !ok {
		return deflt
	}
	return s
}

func boolField(v any) bool {
	b, _ := v.(bool)
	return b
}

func textSlice(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return []string{}
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list, got %#v", v)
	}
	out := make([]string, len(raw))
	for i, x := range raw {
		s, ok := x.(string)
		if !ok {
			t.Fatalf("expected text at index %d, got %#v", i, x)
		}
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------
// DatabaseError: loading / recovering / exit_code / Display
// ---------------------------------------------------------------

func TestDatabaseErrorClassification(t *testing.T) {
	g := compileDatabaseApp(t)

	cases := []struct {
		name     string
		loading  bool
		kind     string
		detail   string
		wantKind string
		wantPhs  string
		wantAuth bool
		wantExit int
		wantMsg  string
	}{
		{
			name:    "loading: a missing file is a storage (access) failure",
			loading: true, kind: "NotFound", detail: "no such file or directory",
			wantKind: "Storage", wantPhs: "loading the storage files", wantAuth: false, wantExit: 1,
			wantMsg: "storage is unreachable while loading the storage files: no such file or directory",
		},
		{
			name:    "loading: a decrypt failure is integrity with the wrong-key hint first",
			loading: true, kind: "InvalidData", detail: "AEAD decrypt failed for page 3",
			wantKind: "Integrity", wantPhs: "", wantAuth: true, wantExit: 4,
			wantMsg: "a stored record failed authentication: AEAD decrypt failed for page 3 — most likely the wrong FACETQL_MASTER_KEY, otherwise the file is corrupt",
		},
		{
			name:    "loading: a checksum failure is integrity without the key hint",
			loading: true, kind: "InvalidData", detail: "bad checksum at offset 12",
			wantKind: "Integrity", wantPhs: "", wantAuth: false, wantExit: 4,
			wantMsg: "a stored record failed its integrity check: bad checksum at offset 12",
		},
		{
			// `loading` has evaluated no WAL semantics, so a lifecycle-sounding
			// message is still Integrity, with `unwrap_or(false)`.
			name:    "loading: anything non-access is integrity, authentication defaults false",
			loading: true, kind: "InvalidData", detail: "transaction 3 COMMIT after ABORT",
			wantKind: "Integrity", wantPhs: "", wantAuth: false, wantExit: 4,
			wantMsg: "a stored record failed its integrity check: transaction 3 COMMIT after ABORT",
		},
		{
			name:    "recovering: a lifecycle violation is WalRecovery",
			loading: false, kind: "InvalidData", detail: "transaction 3 COMMIT after ABORT",
			wantKind: "WalRecovery", wantPhs: "", wantAuth: false, wantExit: 5,
			wantMsg: "the write-ahead log describes a history that cannot be replayed: transaction 3 COMMIT after ABORT",
		},
		{
			name:    "recovering: permission denied is storage with the recovery phase",
			loading: false, kind: "PermissionDenied", detail: "permission denied",
			wantKind: "Storage", wantPhs: "recovering the write-ahead log", wantAuth: false, wantExit: 1,
			wantMsg: "storage is unreachable while recovering the write-ahead log: permission denied",
		},
		{
			name:    "recovering: a truncated frame is a verification failure",
			loading: false, kind: "UnexpectedEof", detail: "truncated frame at 4096",
			wantKind: "Integrity", wantPhs: "", wantAuth: false, wantExit: 4,
			wantMsg: "a stored record failed its integrity check: truncated frame at 4096",
		},
		{
			name:    "recovering: MASTER_KEY in the detail is an authentication failure (case-insensitive)",
			loading: false, kind: "Other", detail: "FACETQL_MASTER_KEY rejected",
			wantKind: "Integrity", wantPhs: "", wantAuth: true, wantExit: 4,
			wantMsg: "a stored record failed authentication: FACETQL_MASTER_KEY rejected — most likely the wrong FACETQL_MASTER_KEY, otherwise the file is corrupt",
		},
		{
			// The inverted match: a kind nobody anticipated is an access failure.
			name:    "recovering: an unanticipated ErrorKind is treated as an access failure",
			loading: false, kind: "SomeFutureKind", detail: "format version 9 unsupported",
			wantKind: "Storage", wantPhs: "recovering the write-ahead log", wantAuth: false, wantExit: 1,
			wantMsg: "storage is unreachable while recovering the write-ahead log: format version 9 unsupported",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshDatabaseApp(t, g)
			d := postJSON(t, ts, "runDatabaseError", c.loading, c.kind, c.detail)
			if got := textField(d["errKind"], ""); got != c.wantKind {
				t.Errorf("errKind = %q, want %q", got, c.wantKind)
			}
			if got := textField(d["errPhase"], ""); got != c.wantPhs {
				t.Errorf("errPhase = %q, want %q", got, c.wantPhs)
			}
			if got := boolField(d["errAuthentication"]); got != c.wantAuth {
				t.Errorf("errAuthentication = %v, want %v", got, c.wantAuth)
			}
			if got := intField(d["errExitCode"], 0); got != c.wantExit {
				t.Errorf("errExitCode = %d, want %d", got, c.wantExit)
			}
			if got := textField(d["errMessage"], ""); got != c.wantMsg {
				t.Errorf("errMessage = %q, want %q", got, c.wantMsg)
			}
			if got := intField(d["exitEnginePoisoned"], 0); got != 6 {
				t.Errorf("EXIT_ENGINE_POISONED = %d, want 6", got)
			}
		})
	}
}

func TestDatabaseIsAccessFailure(t *testing.T) {
	g := compileDatabaseApp(t)
	cases := map[string]bool{
		// the four kinds the storage layer raises for content already read
		"InvalidData":   false,
		"UnexpectedEof": false,
		"InvalidInput":  false,
		"Other":         false,
		// everything the OS hands back for reaching the file
		"NotFound":           true,
		"PermissionDenied":   true,
		"StorageFull":        true,
		"ReadOnlyFilesystem": true,
		"Interrupted":        true,
		"":                   true,
	}
	for kind, want := range cases {
		ts := freshDatabaseApp(t, g)
		d := postJSON(t, ts, "runIsAccessFailure", kind)
		if got := boolField(d["accessFailure"]); got != want {
			t.Errorf("is_access_failure(%q) = %v, want %v", kind, got, want)
		}
	}
}

func TestDatabaseIntegrityFailureClass(t *testing.T) {
	g := compileDatabaseApp(t)
	// 0 = None, 1 = Some(false) verification, 2 = Some(true) authentication.
	cases := []struct {
		detail string
		want   int
	}{
		{"", 0},
		{"transaction 3 COMMIT after ABORT", 0},
		{"sequence 10 after 12", 0},
		{"decrypt failed", 2},
		{"AEAD DECRYPT tag mismatch", 2}, // to_ascii_lowercase
		{"authentication tag invalid", 2},
		{"wrong master key", 2},
		{"FACETQL_MASTER_KEY unset", 2},
		{"checksum mismatch", 1},
		{"Crc32 mismatch in frame 4", 1},
		{"page 7 is corrupt", 1},
		{"could not deserialize record", 1},
		{"invalid record length", 1},
		{"frame is not valid hex", 1},
		{"bad magic bytes", 1},
		{"truncated tail", 1},
		{"unsupported format version 3", 1},
		// AUTHENTICATION is consulted before VERIFICATION
		{"corrupt frame after decrypt", 2},
		{"checksum ok but authentication failed", 2},
	}
	for _, c := range cases {
		ts := freshDatabaseApp(t, g)
		d := postJSON(t, ts, "runIntegrityFailure", c.detail)
		if got := intField(d["integrityClass"], -1); got != c.want {
			t.Errorf("integrity_failure(%q) = %d, want %d", c.detail, got, c.want)
		}
	}
}

// ---------------------------------------------------------------
// Audience: the three `audience_tests`
// ---------------------------------------------------------------

func TestDatabaseAudience(t *testing.T) {
	g := compileDatabaseApp(t)
	type nodeCase struct {
		name      string
		isPublic  bool
		nodeOwner string
		requester string
		isAdmin   bool
		want      bool
	}
	cases := []nodeCase{
		// private_node_events_do_not_reach_other_identities
		{"private: the owner sees its own node", false, "alice", "alice", false, true},
		{"private: an admin reads everything already", false, "alice", "root", true, true},
		{"private: another identity must not learn a private node exists", false, "alice", "bob", false, false},
		// public_node_events_reach_everyone
		{"public: owner", true, "alice", "alice", false, true},
		{"public: bob", true, "alice", "bob", false, true},
		{"public: admin", true, "alice", "root", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := freshDatabaseApp(t, g)
			d := postJSON(t, ts, "runAudienceForNode", c.isPublic, c.nodeOwner, c.requester, c.isAdmin)
			if got := boolField(d["admits"]); got != c.want {
				t.Errorf("admits = %v, want %v", got, c.want)
			}
		})
	}
	// explicit_broadcasts_reach_everyone
	ts := freshDatabaseApp(t, g)
	d := postJSON(t, ts, "runAudienceEveryone", "anyone", false)
	if !boolField(d["admits"]) {
		t.Errorf("Audience::Everyone must admit anyone")
	}
}

// ---------------------------------------------------------------
// EventFeed: the seven `event_feed_tests`, plus hand-traced cases
// ---------------------------------------------------------------

type feedRun struct {
	seqs            []int
	subOk           bool
	subRequested    int
	subEarliest     int
	subRetained     int
	feedEarliest    int
	feedRetained    int
	feedNext        int
	visibleCount    int
	backlogSeqs     []int
	backlogPayloads []string
	visibleSeqs     []int
	subMessage      string
	replayCapacity  int
}

func runFeed(t *testing.T, g *ir.IR, script string, counter, capacity int, hasAfter bool, after int, owner string, isAdmin bool) feedRun {
	t.Helper()
	ts := freshDatabaseApp(t, g)
	d := postJSON(t, ts, "runFeedScript", script, counter, capacity, hasAfter, after, owner, isAdmin)
	return feedRun{
		seqs:            intSlice(t, d["seqs"]),
		subOk:           boolField(d["subOk"]),
		subRequested:    intField(d["subRequested"], -1),
		subEarliest:     intField(d["subEarliest"], -1),
		subRetained:     intField(d["subRetained"], -1),
		feedEarliest:    intField(d["feedEarliest"], -1),
		feedRetained:    intField(d["feedRetained"], -1),
		feedNext:        intField(d["feedNext"], -1),
		visibleCount:    intField(d["visibleCount"], -1),
		backlogSeqs:     intSlice(t, d["backlogSeqs"]),
		backlogPayloads: textSlice(t, d["backlogPayloads"]),
		visibleSeqs:     intSlice(t, d["visibleSeqs"]),
		subMessage:      textField(d["subMessage"], ""),
		replayCapacity:  intField(d["replayCapacity"], 0),
	}
}

func assertTextSlice(t *testing.T, got, want []string, name string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// The counter a fresh process would hand EventFeed::new after recovery
// advanced it — any value works; 7 makes it visibly a parameter rather
// than the AtomicU64::new(1) initial value.
const feedCounter = 7

// `floor(&feed)`: subscribe(Some(0)) is refused and its `earliest` is the
// position EventFeed::new minted.
func TestDatabaseEventFeedFloor(t *testing.T) {
	g := compileDatabaseApp(t)
	r := runFeed(t, g, "", feedCounter, 0, true, 0, "", false)
	if r.subOk {
		t.Fatalf("position 0 predates this feed and must be refused")
	}
	if r.subRequested != 0 || r.subEarliest != feedCounter || r.subRetained != 0 {
		t.Errorf("refusal = {requested %d earliest %d retained %d}, want {0 %d 0}", r.subRequested, r.subEarliest, r.subRetained, feedCounter)
	}
	if r.replayCapacity != 1024 {
		t.Errorf("EVENT_REPLAY_CAPACITY = %d, want 1024", r.replayCapacity)
	}
	if r.feedNext != feedCounter+1 {
		t.Errorf("EventFeed::new must consume one counter position: next = %d, want %d", r.feedNext, feedCounter+1)
	}
}

// positions_increase_with_publication_order
func TestDatabaseEventFeedPositionsIncrease(t *testing.T) {
	g := compileDatabaseApp(t)
	r := runFeed(t, g, "e:a;e:b", feedCounter, 0, false, 0, "", false)
	if len(r.seqs) != 2 || r.seqs[1] <= r.seqs[0] {
		t.Fatalf("seqs = %v, want two strictly increasing positions", r.seqs)
	}
	assertIntSlice(t, r.seqs, []int{feedCounter + 1, feedCounter + 2}, "seqs")
}

// a_resume_replays_exactly_what_came_after
func TestDatabaseEventFeedResumeReplaysExactlyWhatCameAfter(t *testing.T) {
	g := compileDatabaseApp(t)
	script := "e:first;e:second;e:third"

	all := runFeed(t, g, script, feedCounter, 0, true, feedCounter, "", false)
	if !all.subOk {
		t.Fatalf("resume from the start refused: %+v", all)
	}
	assertTextSlice(t, all.backlogPayloads, []string{"first", "second", "third"}, "backlog from start")

	afterSecond := all.seqs[1]
	tail := runFeed(t, g, script, feedCounter, 0, true, afterSecond, "", false)
	if !tail.subOk {
		t.Fatalf("resume mid-stream refused: %+v", tail)
	}
	assertTextSlice(t, tail.backlogPayloads, []string{"third"}, "backlog after second")
	assertIntSlice(t, tail.backlogSeqs, []int{all.seqs[2]}, "backlog seqs after second")
}

// opening_without_a_position_replays_nothing
func TestDatabaseEventFeedOpenWithoutPositionReplaysNothing(t *testing.T) {
	g := compileDatabaseApp(t)
	r := runFeed(t, g, "e:already published", feedCounter, 0, false, 0, "", false)
	if !r.subOk {
		t.Fatalf("open at the live edge refused: %+v", r)
	}
	if len(r.backlogSeqs) != 0 || len(r.backlogPayloads) != 0 {
		t.Errorf("backlog = %v, want empty", r.backlogPayloads)
	}
	if r.feedRetained != 1 {
		t.Errorf("the event is still retained for a later resume: retained = %d, want 1", r.feedRetained)
	}
}

// a_resume_from_before_the_horizon_is_refused — the real 1024-deep ring
// with EVENT_REPLAY_CAPACITY + 1 emits.
func TestDatabaseEventFeedResumeBeforeHorizonIsRefused(t *testing.T) {
	g := compileDatabaseApp(t)
	script := "r:1025:event "

	refusal := runFeed(t, g, script, feedCounter, 0, true, feedCounter, "", false)
	if refusal.subOk {
		t.Fatalf("the oldest event has been evicted; this cannot be served")
	}
	if refusal.subRequested != feedCounter {
		t.Errorf("requested = %d, want %d", refusal.subRequested, feedCounter)
	}
	if refusal.subEarliest <= feedCounter {
		t.Errorf("the refusal must name a position that IS still available: earliest = %d", refusal.subEarliest)
	}
	// Exactly one event (seq counter+1) was evicted, so the horizon is its position.
	if refusal.subEarliest != feedCounter+1 {
		t.Errorf("earliest = %d, want %d (the evicted event's own position)", refusal.subEarliest, feedCounter+1)
	}
	if refusal.subRetained != 1024 {
		t.Errorf("retained = %d, want EVENT_REPLAY_CAPACITY (1024)", refusal.subRetained)
	}
	if len(refusal.seqs) != 1025 {
		t.Fatalf("emitted %d events, want 1025", len(refusal.seqs))
	}
	wantMsg := fmt.Sprintf("cannot resume the event feed from %d: the oldest position still available is %d (1024 of at most 1024 events retained, in memory only — nothing published before this process started is retained at all). The events between those two positions are gone from the feed and this server will not pretend otherwise by starting from the live edge. Reconcile from a full read instead.", feedCounter, feedCounter+1)
	if refusal.subMessage != wantMsg {
		t.Errorf("ResumeTooOld message =\n%q\nwant\n%q", refusal.subMessage, wantMsg)
	}

	// And the position it names is honoured.
	honoured := runFeed(t, g, script, feedCounter, 0, true, refusal.subEarliest, "", false)
	if !honoured.subOk {
		t.Fatalf("the horizon it reported must itself be resumable")
	}
	if len(honoured.backlogSeqs) != 1024 {
		t.Fatalf("backlog len = %d, want 1024", len(honoured.backlogSeqs))
	}
	if honoured.backlogPayloads[0] != "event 1" || honoured.backlogPayloads[1023] != "event 1024" {
		t.Errorf("backlog spans %q..%q, want \"event 1\"..\"event 1024\"", honoured.backlogPayloads[0], honoured.backlogPayloads[1023])
	}
	if honoured.subMessage != "" {
		t.Errorf("a successful resume carries no refusal message, got %q", honoured.subMessage)
	}
}

// a_json_event_carries_its_own_position
func TestDatabaseEventFeedJsonEventCarriesItsPosition(t *testing.T) {
	g := compileDatabaseApp(t)
	r := runFeed(t, g, `j:{"event":"node_deleted","address":"x"}`, feedCounter, 0, true, feedCounter, "", false)
	if !r.subOk || len(r.backlogPayloads) != 1 {
		t.Fatalf("resume: %+v", r)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.backlogPayloads[0]), &body); err != nil {
		t.Fatalf("payload %q is not valid JSON: %v", r.backlogPayloads[0], err)
	}
	if got := intField(body["seq"], -1); got != r.seqs[0] {
		t.Errorf("body.seq = %d, want the emitted position %d", got, r.seqs[0])
	}
	if body["event"] != "node_deleted" || body["address"] != "x" {
		t.Errorf("body = %v, original members must survive", body)
	}
}

// an_opaque_payload_is_not_rewritten
func TestDatabaseEventFeedOpaquePayloadNotRewritten(t *testing.T) {
	g := compileDatabaseApp(t)
	r := runFeed(t, g, "e:not json at all", feedCounter, 0, true, feedCounter, "", false)
	if !r.subOk {
		t.Fatalf("resume refused: %+v", r)
	}
	assertTextSlice(t, r.backlogPayloads, []string{"not json at all"}, "backlog")
}

// Positions are not contiguous by design: the WAL burns numbers on the
// shared counter, and `after=` is strictly-greater-than, never "next".
func TestDatabaseEventFeedNonContiguousPositions(t *testing.T) {
	g := compileDatabaseApp(t)
	script := "e:a;b:5;e:b"
	base := runFeed(t, g, script, feedCounter, 0, false, 0, "", false)
	assertIntSlice(t, base.seqs, []int{feedCounter + 1, feedCounter + 7}, "seqs with a 5-record WAL gap")

	for after, want := range map[int][]int{
		feedCounter + 1: {feedCounter + 7},
		feedCounter + 3: {feedCounter + 7}, // a burned position is a legal `after`
		feedCounter + 6: {feedCounter + 7},
		feedCounter + 7: {},
		feedCounter + 9: {}, // past the live edge: nothing, not a refusal
	} {
		r := runFeed(t, g, script, feedCounter, 0, true, after, "", false)
		if !r.subOk {
			t.Errorf("after=%d refused, want ok", after)
			continue
		}
		assertIntSlice(t, r.backlogSeqs, want, fmt.Sprintf("backlog after=%d", after))
	}
}

// routes.rs applies Audience::admits to the backlog it replays.
func TestDatabaseEventFeedBacklogAudienceFilter(t *testing.T) {
	g := compileDatabaseApp(t)
	script := "o:alice:p1;e:pub;o:bob:p2"
	s1, s2, s3 := feedCounter+1, feedCounter+2, feedCounter+3

	cases := []struct {
		owner   string
		isAdmin bool
		want    []int
	}{
		{"bob", false, []int{s2, s3}},
		{"alice", false, []int{s1, s2}},
		{"carol", false, []int{s2}},
		{"root", true, []int{s1, s2, s3}},
	}
	for _, c := range cases {
		r := runFeed(t, g, script, feedCounter, 0, true, feedCounter, c.owner, c.isAdmin)
		if !r.subOk {
			t.Fatalf("resume refused: %+v", r)
		}
		// subscribe itself returns everything; the filter is the caller's.
		assertIntSlice(t, r.backlogSeqs, []int{s1, s2, s3}, "unfiltered backlog")
		assertIntSlice(t, r.visibleSeqs, c.want, fmt.Sprintf("visible to %s admin=%v", c.owner, c.isAdmin))
		if r.visibleCount != len(c.want) {
			t.Errorf("visibleCount = %d, want %d", r.visibleCount, len(c.want))
		}
	}
}

// Hand-traced eviction with a 2-deep ring (counter 1, the fresh-process
// value): emits a,b,c,d land at 2,3,4,5; a (2) then b (3) are evicted, so
// the horizon is 3, and `after=3` is still answerable while `after=2` is
// not.
func TestDatabaseEventFeedEvictionHandTrace(t *testing.T) {
	g := compileDatabaseApp(t)
	script := "e:a;e:b;e:c;e:d"

	r := runFeed(t, g, script, 1, 2, false, 0, "", false)
	assertIntSlice(t, r.seqs, []int{2, 3, 4, 5}, "seqs")
	if r.feedEarliest != 3 || r.feedRetained != 2 || r.feedNext != 6 {
		t.Errorf("feed = {earliest %d retained %d next %d}, want {3 2 6}", r.feedEarliest, r.feedRetained, r.feedNext)
	}

	at3 := runFeed(t, g, script, 1, 2, true, 3, "", false)
	if !at3.subOk {
		t.Fatalf("after=3 (the evicted event's own position) must be honoured")
	}
	assertIntSlice(t, at3.backlogSeqs, []int{4, 5}, "backlog after=3")
	assertTextSlice(t, at3.backlogPayloads, []string{"c", "d"}, "payloads after=3")

	at2 := runFeed(t, g, script, 1, 2, true, 2, "", false)
	if at2.subOk {
		t.Fatalf("after=2 is behind the horizon and must be refused")
	}
	if at2.subRequested != 2 || at2.subEarliest != 3 || at2.subRetained != 2 {
		t.Errorf("refusal = {%d %d %d}, want {2 3 2}", at2.subRequested, at2.subEarliest, at2.subRetained)
	}
	wantMsg := "cannot resume the event feed from 2: the oldest position still available is 3 (2 of at most 2 events retained, in memory only — nothing published before this process started is retained at all). The events between those two positions are gone from the feed and this server will not pretend otherwise by starting from the live edge. Reconcile from a full read instead."
	if at2.subMessage != wantMsg {
		t.Errorf("message =\n%q\nwant\n%q", at2.subMessage, wantMsg)
	}

	// Below capacity nothing is evicted and the horizon stays at the mint.
	short := runFeed(t, g, "e:a;e:b", 1, 2, true, 1, "", false)
	if !short.subOk || short.feedEarliest != 1 {
		t.Errorf("a ring at exactly capacity evicts nothing: ok=%v earliest=%d", short.subOk, short.feedEarliest)
	}
	assertIntSlice(t, short.backlogSeqs, []int{2, 3}, "backlog at capacity")
}

// ---------------------------------------------------------------
// emit_json's structured insert (simplification 5)
// ---------------------------------------------------------------

func TestDatabaseJsonObjectWithSeq(t *testing.T) {
	g := compileDatabaseApp(t)
	cases := []struct {
		body string
		seq  int
		want string
	}{
		{`{"event":"x"}`, 5, `{"event":"x","seq":5}`},
		{`{}`, 5, `{"seq":5}`},
		{`  { "a" : 1 }  `, 42, `{"a" : 1,"seq":42}`},
		{`[1,2]`, 5, `[1,2]`},
		{`"str"`, 5, `"str"`},
		{`not json at all`, 5, `not json at all`},
		{`{`, 5, `{`},
	}
	for _, c := range cases {
		ts := freshDatabaseApp(t, g)
		d := postJSON(t, ts, "runJsonObjectWithSeq", c.body, c.seq)
		if got := textField(d["jsonWithSeq"], ""); got != c.want {
			t.Errorf("jsonObjectWithSeq(%q, %d) = %q, want %q", c.body, c.seq, got, c.want)
		}
		if c.want != c.body && c.want[0] == '{' {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(textField(d["jsonWithSeq"], "")), &parsed); err != nil {
				t.Errorf("result for %q is not valid JSON: %v", c.body, err)
			} else if intField(parsed["seq"], -1) != c.seq {
				t.Errorf("result for %q lacks seq=%d: %v", c.body, c.seq, parsed)
			}
		}
	}
}

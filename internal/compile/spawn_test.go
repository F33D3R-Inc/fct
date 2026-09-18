package compile

import (
	"strings"
	"testing"
)

// spawnJoinApp is Milestone 5's proving example: two procs spawned
// concurrently (real goroutines under the hood — see runtime/spawn_test.go
// for the live wall-clock proof) and both joined, in the same block, before
// the spawning proc returns — the shape internal/ir/build.go's
// checkSpawnsJoined must accept.
const spawnJoinApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc doubleBoth(a: int, b: int) -> int:
        let h1 = spawn double(a)
        let h2 = spawn double(b)
        let r1 = join h1
        let r2 = join h2
        return r1 + r2
    state result: int = 0
    action run(a: int, b: int):
        let r = do doubleBoth(a, b)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestSpawnJoinCompiles(t *testing.T) {
	g, err := String(spawnJoinApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	names := map[string]bool{}
	for _, p := range g.Procs {
		names[p.Name] = true
	}
	if !names["double"] || !names["doubleBoth"] {
		t.Fatalf("want procs double,doubleBoth, got %+v", names)
	}
}

// fireAndForgetSpawnApp writes a bare `spawn ...` statement, never bound to a
// handle at all — the one shape the grammar itself refuses (see ast.Spawn's
// doc): a spawned goroutine nothing could ever join would be exactly the
// fire-and-forget leak structured concurrency exists to forbid.
const fireAndForgetSpawnApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        spawn double(a)
        return 0
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestBareSpawnStatementIsCompileError(t *testing.T) {
	_, err := String(fireAndForgetSpawnApp)
	if err == nil {
		t.Fatal("want a compile error for a bare (unbound) spawn statement, got none")
	}
	if !strings.Contains(err.Error(), "fire-and-forget") {
		t.Fatalf("error %q should explain spawn has no fire-and-forget form", err.Error())
	}
}

// unjoinedSpawnApp is the structured-concurrency enforcement proof the task
// itself calls for: a proc that spawns a task and never joins it must be
// rejected at compile time.
const unjoinedSpawnApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        let h = spawn double(a)
        return 0
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestUnjoinedSpawnIsCompileError(t *testing.T) {
	_, err := String(unjoinedSpawnApp)
	if err == nil {
		t.Fatal("want a compile error: `h` is spawned but never joined")
	}
	if !strings.Contains(err.Error(), "never joins") && !strings.Contains(err.Error(), "still unjoined") {
		t.Fatalf("error %q should explain the handle is never joined", err.Error())
	}
}

// spawnThenIfApp spawns a task and then branches (an `if`) BEFORE joining it
// in the same block — the case checkSpawnsJoined's noPendingSpawns guard
// exists for: the `if`'s `then` branch could return early, in which case a
// `join` sitting after the `if` in the same block would never run, leaking
// the goroutine past this proc's own return. Must be rejected even though a
// `join h` does textually appear later in the same statement list.
const spawnThenIfApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        let h = spawn double(a)
        if a > 0:
            return 1
        let r = join h
        return r
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestSpawnBeforeBranchWithoutJoinIsCompileError(t *testing.T) {
	_, err := String(spawnThenIfApp)
	if err == nil {
		t.Fatal("want a compile error: an `if` that could return early sits between the spawn and its join")
	}
	if !strings.Contains(err.Error(), "unjoined") {
		t.Fatalf("error %q should name the still-unjoined handle", err.Error())
	}
}

// doubleJoinApp joins the same handle twice — a handle is a use-once value.
const doubleJoinApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        let h = spawn double(a)
        let r1 = join h
        let r2 = join h
        return r1 + r2
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestJoinTwiceIsCompileError(t *testing.T) {
	_, err := String(doubleJoinApp)
	if err == nil {
		t.Fatal("want a compile error: `h` is joined twice")
	}
	if !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("error %q should say the handle was already joined", err.Error())
	}
}

// taskHandleAsValueApp tries to use a spawned handle as an ordinary value
// (here, returned directly) instead of joining it — refused because a
// handle is not a real value the type system tracks any other way (see
// checkNoTaskUse): the ENTIRE enforcement mechanism depends on a handle never
// being copyable/passable/storable, only joinable.
const taskHandleAsValueApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        let h = spawn double(a)
        let r = join h
        return h
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestTaskHandleUsedAsValueIsCompileError(t *testing.T) {
	_, err := String(taskHandleAsValueApp)
	if err == nil {
		t.Fatal("want a compile error: `h` (a task handle) used as an ordinary return value")
	}
	if !strings.Contains(err.Error(), "task handle") {
		t.Fatalf("error %q should say `h` is a task handle", err.Error())
	}
}

// spawnUnknownProcApp spawns a name that isn't a declared proc.
const spawnUnknownProcApp = `app A:
    proc runIt(a: int) -> int:
        let h = spawn doesNotExist(a)
        let r = join h
        return r
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestSpawnUnknownProcIsCompileError(t *testing.T) {
	_, err := String(spawnUnknownProcApp)
	if err == nil {
		t.Fatal("want a compile error: spawn of an unknown proc")
	}
	if !strings.Contains(err.Error(), "unknown proc") {
		t.Fatalf("error %q should say the proc is unknown", err.Error())
	}
}

// spawnJoinFireAndForgetApp proves `join h` with no bind still compiles (a
// pure synchronization barrier — waits for the task but discards its
// result).
const spawnJoinFireAndForgetApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    proc runIt(a: int) -> int:
        let h = spawn double(a)
        join h
        return a
    state result: int = 0
    action run(a: int):
        let r = do runIt(a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestFireAndForgetJoinCompiles(t *testing.T) {
	if _, err := String(spawnJoinFireAndForgetApp); err != nil {
		t.Fatalf("compile: %v", err)
	}
}

// channelApp proves channel()/send()/recv() compile inside a proc, and that a
// channel handle (just an int under the hood) is a perfectly ordinary int
// parameter for another proc to receive.
const channelApp = `app A:
    proc producer(ch: int, msg: text) -> bool:
        return send(ch, msg)
    proc consumer(ch: int) -> text:
        return recv(ch)
    proc roundTrip(msg: text) -> text:
        let ch = channel()
        let h1 = spawn producer(ch, msg)
        let h2 = spawn consumer(ch)
        join h1
        let got = join h2
        return got
    state result: text = ""
    action run(msg: text):
        let r = do roundTrip(msg)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestChannelCompiles(t *testing.T) {
	g, err := String(channelApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 3 {
		t.Fatalf("want 3 procs, got %d", len(g.Procs))
	}
}

// channelOutsideProcApp calls channel() from an action body, where it (like
// readFile/httpGet) must be refused: it is real, blocking concurrency
// machinery with exactly one interpreter, reached only from a proc's frame.
const channelOutsideProcApp = `app A:
    state result: int = 0
    action run():
        result = channel()
    view Home at "/":
        box:
            text "{result}"
`

func TestChannelOutsideProcIsCompileError(t *testing.T) {
	_, err := String(channelOutsideProcApp)
	if err == nil {
		t.Fatal("want a compile error: channel() called outside a proc body")
	}
	if !strings.Contains(err.Error(), "only available inside a proc body") {
		t.Fatalf("error %q should say channel() is proc-only", err.Error())
	}
}

// spawnOutsideProcApp writes `spawn`/`join` directly inside an action body —
// grammar that only exists inside parseProcBody, so this must fail with SOME
// clear parse error (an action's own statement grammar has no `spawn`/`join`
// case at all).
const spawnOutsideProcApp = `app A:
    proc double(n: int) -> int:
        return n * 2
    state result: int = 0
    action run(a: int):
        let h = spawn double(a)
        result = join h
    view Home at "/":
        box:
            text "{result}"
`

func TestSpawnOutsideProcIsCompileError(t *testing.T) {
	_, err := String(spawnOutsideProcApp)
	if err == nil {
		t.Fatal("want a compile error: spawn/join used inside an action body")
	}
}

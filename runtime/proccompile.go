package runtime

// The proc engine: every proc body (and every daemon body) is compiled once,
// the first time it runs, from its IR into a tree of Go closures over a flat
// slot array, and every later call runs the closures.
//
// The tree-walking interpreter this replaces resolved every local by name at
// every step: a `ref` was a walk up a chain of map[string]any frames, a `let`
// a map insert, and each loop iteration allocated a fresh map for its body's
// scope. For the self-hosted compiler and the storage engine written in fct —
// loops over byte buffers and characters, millions of steps per call — that
// name resolution was most of the running time (a map hash per variable read)
// and the per-iteration maps most of the garbage.
//
// Names are resolved here instead, at compile time. The compiler
// (internal/ir/build.go's procBlock) scopes proc locals lexically and refuses
// redeclaring a name already in scope, so every reference names exactly one
// declaration, known statically: each declaration gets its own slot in the
// invocation's []any, and a reference compiles to that slot's index. A
// loop-body local keeps its slot across iterations, which is unobservable —
// the body's `let` always initializes it before any read in that iteration.
// Operators, builtins and statement kinds are dispatched once, when the
// closure is built, rather than by comparing strings at every evaluation, and
// arithmetic and comparison on two ints skip applyBin's generic conversions.
//
// Composite values (lists, byte buffers, maps, structs) keep the language's
// value semantics — no binding ever observes a mutation made through another
// — but by copy-on-write rather than the interpreter's copy-on-bind. Binding
// a value (a `let`, a reassignment, a parameter) shares it; each slot carries
// an ownership bit saying whether the value it holds is referenced from
// anywhere else, and an in-place mutation (`xs[i] = v`, `m[k] = v`,
// `s.f = v`, `xs = append(xs, v)`) first copies a value its slot does not
// own. A slot owns what it holds when the value was created fresh for it (a
// literal, `bytes(n)`, `append(…)`, a first copy-on-write) and loses the
// bit the moment its value can escape — whenever it is read anywhere that
// could retain it (bound elsewhere, placed in a literal, passed to a proc
// or to a builtin that may return it). Reads that cannot retain the value —
// `xs[i]`, `s.f`, `len(xs)`, an operator's operand — leave the bit alone,
// so a byte-buffer loop mutates in place and `out = append(out, x)` grows
// in amortized constant time, where copy-on-bind copied the whole value on
// every call, binding and append.
//
// Otherwise semantics are the interpreter's, statement for statement and
// expression for expression: the same `x = x + e` in-place text append (now
// per slot), the same error messages, and the same helpers (applyBin,
// callProcBuiltin, coerceRet, …) wherever a value is actually computed.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"facet/internal/ir"
)

// pfr is one proc or daemon invocation's locals: one slot per declaration,
// and the text builders behind the `x = x + e` append path, per slot.
type pfr struct {
	slots []any
	own   []bool // own[i]: slots[i]'s composite value is referenced from nowhere else
	bld   []*strings.Builder
	// ints holds the value of every statically int-typed slot (see
	// procCompiler.ints) unboxed; such a slot's entry in slots is unused.
	ints []int
}

type cexpr func(*pfr) (any, error)
type cstmt func(*pfr) (ctlSignal, error)

// procCode is one compiled body: the slot count an invocation allocates, the
// slot each parameter binds, and the statements.
type procCode struct {
	nslots   int
	params   []int
	intParam []bool // intParam[i]: parameter i is an int, bound into fr.ints
	// borrow[i]: parameter i is only ever inspected — indexed, measured,
	// its fields read into scalars and operators — never retained or
	// written in place (the body cannot write it in place anyway: the
	// slot does not own what a caller shares). A caller hands such an
	// argument over without giving up its own ownership, so passing a
	// value to a reader does not make the caller's next in-place write
	// a copy (Rust's shared borrow, inferred).
	borrow []bool
	body   []cstmt
	frames sync.Pool // idle *pfr of this code's size, reused across calls
}

// procCompiler resolves names while a body is compiled: scopes is the stack
// of lexical blocks, each mapping a name to its slot.
type procCompiler struct {
	s *Server
	// label: the profile label of the code being compiled (procLabel of
	// its proc; no label for a daemon body).
	label context.Context
	// structTypes: slots statically known to hold a value of a declared
	// struct type (a struct parameter, a `let` of a struct-typed
	// expression, a struct-returning `do`), so a read of an int field of
	// one is native (isInt / intExpr's "get").
	structTypes map[int]string
	// floats / floatLists: slots statically known to hold a float / a list
	// of floats — static types only (the values stay boxed in the slot), so
	// a float expression over them evaluates natively (floatExpr) and boxes
	// only its result.
	floats     map[int]bool
	floatLists map[int]bool
	scopes     []map[string]int
	next       int
	// ints / intLists: slots statically known to hold an int / a list of
	// ints (an int parameter, or a declaration whose initializer is one —
	// the language is statically typed, so a slot never changes type).
	// Expressions over them are evaluated as native ints (intExpr),
	// boxing only a result that is bound or stored.
	ints     map[int]bool
	intLists map[int]bool
	// Move analysis: pos numbers every declaration and name resolution in
	// textual order; lastPos/declPos are each slot's last occurrence and
	// its declaration; loops are the position ranges loop bodies span.
	// moves are the arguments that may hand their value over (see
	// moveArg), decided once the whole body is compiled.
	pos     int
	lastPos map[int]int
	declPos map[int]int
	loops   [][2]int
	moves   []moveCand
	// escaped: slots whose value (or a field of it) may be retained past
	// the statement that reads it — bound elsewhere, returned, stored,
	// passed on. A parameter that never escapes is borrowed (procCode.
	// borrow): its caller keeps its ownership across the call.
	escaped map[int]bool
	// Field moves: wholeLast is each slot's last occurrence other than as
	// the root of a field path (`s` in `s.f.g`); pathReads every field path
	// read off a local (`s.f.g`, the whole chain, not its prefixes). A
	// field path bound or passed on (`let x = s.f`, `f(s.f.g)`, `T{a:
	// s.f}`) takes the field with its ownership (fieldMoveCand) when
	// nothing after it reads s whole or a path overlapping it — the
	// functional update `let mut xs = s.f; xs[i] = v; return T{…, f: xs}`
	// then writes in place instead of copying the whole field.
	wholeLast  map[int]int
	pathReads  []pathRead
	fieldMoves []fieldMoveCand
	// fieldObj: the next lookup is the root of a field path; chainInner:
	// the next get is the object of a get (not a path's end)
	fieldObj   bool
	chainInner bool
}

// pathRead is one read of the field path `path` (dot-joined) off slot's
// value, at pos.
type pathRead struct {
	slot int
	path string
	pos  int
}

// fieldMoveCand is a field path at position pos that may take its value
// with its ownership: when no later occurrence reads the slot whole or a
// path that is a prefix or an extension of it, and no loop around it
// could run it again with the slot still live.
type fieldMoveCand struct {
	slot int
	path string
	pos  int
	ok   *bool
}

// pathsOverlap: one dot-joined field path is the other or runs through it.
func pathsOverlap(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	return a == b || strings.HasPrefix(b, a+".")
}

// moveCand is a bare-local argument at position pos: it moves when no
// later occurrence of its slot exists and no loop around it could run it
// again with the slot still live (every enclosing loop declares the slot).
type moveCand struct {
	slot, pos int
	ok        *bool
	// self: the statement assigns the call's result to this same slot, so
	// the old value dies there — it moves when nothing between this
	// argument and that assignment reads the slot (settled by selfMoves).
	self bool
}

// selfMoves settles the self candidates of the statement just compiled,
// before its assignment target is resolved.
func (c *procCompiler) selfMoves(from int) {
	kept := c.moves[:from]
	for _, m := range c.moves[from:] {
		if m.self {
			*m.ok = c.lastPos[m.slot] == m.pos
			continue
		}
		kept = append(kept, m)
	}
	c.moves = kept
}

func (c *procCompiler) decideMoves() {
	for _, m := range c.moves {
		ok := c.lastPos[m.slot] == m.pos
		for _, l := range c.loops {
			if m.pos > l[0] && m.pos <= l[1] && c.declPos[m.slot] <= l[0] {
				ok = false
			}
		}
		*m.ok = ok
	}
	for _, m := range c.fieldMoves {
		ok := c.wholeLast[m.slot] < m.pos
		for _, r := range c.pathReads {
			if r.slot == m.slot && r.pos > m.pos && pathsOverlap(r.path, m.path) {
				ok = false
			}
		}
		for _, l := range c.loops {
			if m.pos > l[0] && m.pos <= l[1] && c.declPos[m.slot] <= l[0] {
				ok = false
			}
		}
		*m.ok = ok
	}
}

func (c *procCompiler) push() { c.scopes = append(c.scopes, map[string]int{}) }
func (c *procCompiler) pop()  { c.scopes = c.scopes[:len(c.scopes)-1] }

// declare gives name a new slot in the innermost block.
func (c *procCompiler) declare(name string) int {
	i := c.next
	c.next++
	c.scopes[len(c.scopes)-1][name] = i
	c.pos++
	if c.declPos != nil {
		c.declPos[i], c.lastPos[i] = c.pos, c.pos
	}
	return i
}

// lookup finds the slot of the innermost visible declaration of name, and
// records the occurrence for the move analysis.
func (c *procCompiler) lookup(name string) (int, bool) {
	slot, ok := c.resolve(name)
	fieldObj := c.fieldObj
	c.fieldObj = false
	if ok && c.lastPos != nil {
		c.pos++
		c.lastPos[slot] = c.pos
		if !fieldObj && c.wholeLast != nil {
			c.wholeLast[slot] = c.pos
		}
	}
	return slot, ok
}

// resolve is lookup without recording an occurrence (type queries).
func (c *procCompiler) resolve(name string) (int, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if slot, ok := c.scopes[i][name]; ok {
			return slot, true
		}
	}
	return 0, false
}

// moveArg compiles a call argument. A bare local whose value is dead after
// the call (moveCand, or the statement's own assignment target, which the
// call's result overwrites) is handed over: the callee's parameter owns it
// when this slot did, and the slot is emptied. Otherwise the value is
// shared as any retained read is.
func (c *procCompiler) moveArg(e *ir.Expr, target int) func(*pfr) (any, bool, error) {
	return c.moveArgTo(e, target, nil, 0)
}

// moveArgTo is moveArg for argument i of a call to site: when the callee
// borrows that parameter (procSite.borrows), a shared argument keeps this
// slot's ownership — the callee only reads it, and this frame is paused
// until it returns. (A moved argument is handed over either way.)
func (c *procCompiler) moveArgTo(e *ir.Expr, target int, site *procSite, i int) func(*pfr) (any, bool, error) {
	if e != nil && e.Kind == "ref" {
		if slot, ok := c.lookup(e.Name); ok && c.ints[slot] {
			return func(fr *pfr) (any, bool, error) { return boxInt(fr.ints[slot]), false, nil }
		} else if ok {
			move := new(bool)
			c.moves = append(c.moves, moveCand{slot: slot, pos: c.pos, ok: move, self: slot == target})
			if site == nil {
				c.escaped[slot] = true
			} else {
				// passed to a callee: an escape unless the callee borrows,
				// which is known only once it is compiled — conservatively
				// an escape for this body's own borrow inference
				c.escaped[slot] = true
			}
			return func(fr *pfr) (any, bool, error) {
				v := fr.slots[slot]
				if *move {
					owned := fr.own[slot]
					fr.slots[slot], fr.own[slot] = nil, false
					return v, owned, nil
				}
				if site == nil || !site.borrows(i) {
					fr.own[slot] = false
				}
				return v, false, nil
			}
		}
	}
	if rs, root, path, isPath := c.localPath(e); isPath {
		return c.takePath(rs, root, path)
	}
	return c.ownedExpr(e, target)
}

// ownedExpr compiles a value together with whether the code receiving it
// owns it. A proc call's result is owned when the callee handed over a
// value nothing else references; `append(xs, e)` of a local at its last
// use grows that list in place when the local owned it (a list threaded
// through calls, `let mut out = append(parts, x)`, is then built in
// amortized constant time per element rather than copied whole at each
// step); anything else is owned when it is freshly built (fresh). target
// is the slot the enclosing statement assigns, whose old value dies there
// (moveCand.self); the statement settles those candidates (selfMoves).
func (c *procCompiler) ownedExpr(e *ir.Expr, target int) argFn {
	if e != nil && e.Kind == "call" {
		if site := c.site(e.Name); site != nil {
			args := make([]argFn, len(e.Args))
			for i, a := range e.Args {
				args[i] = c.moveArgTo(a, target, site, i)
			}
			return func(fr *pfr) (any, bool, error) {
				var buf [6]any
				vals, mask, err := callArgs(args, fr, buf[:])
				if err != nil {
					return nil, false, err
				}
				return site.call(vals, mask)
			}
		}
		if e.Name == "append" && c.s.byProc["append"] == nil && len(e.Args) == 2 && e.Args[0] != nil && e.Args[0].Kind == "ref" {
			if slot, ok := c.lookup(e.Args[0].Name); ok && !c.ints[slot] {
				move := new(bool)
				c.moves = append(c.moves, moveCand{slot: slot, pos: c.pos, ok: move, self: slot == target})
				elem := c.expr(e.Args[1])
				return func(fr *pfr) (any, bool, error) {
					xs := fr.slots[slot]
					v, err := elem(fr)
					if err != nil {
						return nil, false, err
					}
					if arr, isList := xs.([]any); isList && *move && fr.own[slot] {
						// the list's only holder, dead after this: grown
						// in place and handed over
						fr.slots[slot], fr.own[slot] = nil, false
						return listGrow(arr, v), true, nil
					}
					return appendCopy(xs, v), true, nil
				}
			}
		}
	}
	x := c.expr(e)
	fresh := c.fresh(e)
	return func(fr *pfr) (any, bool, error) {
		v, err := x(fr)
		return v, fresh, err
	}
}

type argFn = func(*pfr) (any, bool, error)

// callArgs evaluates a call's arguments onto buf, with the mask of those
// whose ownership is handed to the callee.
func callArgs(args []argFn, fr *pfr, buf []any) ([]any, uint64, error) {
	var vals []any
	if len(args) <= len(buf) {
		vals = buf[:len(args)]
	} else {
		vals = make([]any, len(args))
	}
	var mask uint64
	for i, a := range args {
		v, owned, err := a(fr)
		if err != nil {
			return nil, 0, err
		}
		vals[i] = v
		if owned && i < 64 {
			mask |= 1 << uint(i)
		}
	}
	return vals, mask, nil
}

// target resolves an assignment target; a name the compiler never declared
// (impossible from compiled source) is declared in the innermost block,
// exactly where the interpreter's frame.set would have created it.
func (c *procCompiler) target(name string) int {
	if slot, ok := c.lookup(name); ok {
		return slot
	}
	return c.declare(name)
}

// compileProc builds a proc's code: its parameters declared in order, then
// its body in the same block (a parameter and a top-level local share one
// scope, as they shared one frame).
func (s *Server) compileProc(params []ir.Param, body []ir.Stmt, label context.Context) *procCode {
	c := &procCompiler{s: s, label: label, structTypes: map[int]string{}, floats: map[int]bool{}, floatLists: map[int]bool{}, ints: map[int]bool{}, intLists: map[int]bool{}, lastPos: map[int]int{}, declPos: map[int]int{}, escaped: map[int]bool{}, wholeLast: map[int]int{}}
	c.push()
	pc := &procCode{}
	for _, p := range params {
		slot := c.declare(p.Name)
		if p.Type == "int" && !p.Map && p.Depth <= 1 {
			if p.List {
				c.intLists[slot] = true
			} else {
				c.ints[slot] = true
			}
		}
		if !p.List && !p.Map && c.isStructType(p.Type) {
			c.structTypes[slot] = p.Type
		}
		if p.Type == "float" && !p.Map && p.Depth <= 1 {
			if p.List {
				c.floatLists[slot] = true
			} else {
				c.floats[slot] = true
			}
		}
		pc.params = append(pc.params, slot)
		pc.intParam = append(pc.intParam, c.ints[slot])
	}
	pc.body = c.stmts(body)
	c.pop()
	c.decideMoves()
	pc.borrow = make([]bool, len(pc.params))
	for i, slot := range pc.params {
		pc.borrow[i] = !pc.intParam[i] && !c.escaped[slot]
	}
	pc.nslots = c.next
	return pc
}

// codeFor returns key's compiled code, compiling it on first use.
func (s *Server) codeFor(key any, params []ir.Param, body []ir.Stmt) *procCode {
	if v, ok := s.procCode.Load(key); ok {
		return v.(*procCode)
	}
	// The profile label of the code being compiled: its call sites switch
	// back to it when a callee returns (procSite.call).
	label := context.Background()
	if p, ok := key.(*ir.Proc); ok {
		label = procLabel(p.Name)
	}
	pc := s.compileProc(params, body, label)
	v, _ := s.procCode.LoadOrStore(key, pc)
	return v.(*procCode)
}

// procSite is one call site's callee, resolved when the site is compiled:
// the proc (byProc is fixed once the server is built) and, after the first
// call, its code — so a call hashes no names at all.
type procSite struct {
	s    *Server
	p    *ir.Proc
	code atomic.Pointer[procCode]
	// callee / caller: the profile labels a profiled call switches the
	// goroutine to on entry and back to on return (see call).
	callee, caller context.Context
}

// procLabels holds one labelled context per proc name, built once.
var procLabels sync.Map

// procLabel is the context carrying the `proc` profile label for name.
func procLabel(name string) context.Context {
	if v, ok := procLabels.Load(name); ok {
		return v.(context.Context)
	}
	v, _ := procLabels.LoadOrStore(name, pprof.WithLabels(context.Background(), pprof.Labels("proc", name)))
	return v.(context.Context)
}

// borrows reports whether the callee borrows its parameter i (procCode.
// borrow), compiling it first if no call has yet.
func (ps *procSite) borrows(i int) bool {
	pc := ps.code.Load()
	if pc == nil {
		pc = ps.s.codeFor(ps.p, ps.p.Params, ps.p.Body)
		ps.code.Store(pc)
	}
	return i < len(pc.borrow) && pc.borrow[i]
}

func (c *procCompiler) site(name string) *procSite {
	if p := c.s.byProc[name]; p != nil {
		return &procSite{s: c.s, p: p, callee: procLabel(name), caller: c.label}
	}
	return nil
}

func (ps *procSite) call(args []any, owned uint64) (any, bool, error) {
	pc := ps.code.Load()
	if pc == nil {
		pc = ps.s.codeFor(ps.p, ps.p.Params, ps.p.Body)
		ps.code.Store(pc)
	}
	if ps.s.profiling {
		// Under a CPU profile (SetProfiling), every sample carries the fct
		// proc it was taken in — the innermost one — as the `proc` label,
		// so `go tool pprof -tags` reads as a profile of the program rather
		// than of the evaluator. Off, this costs one branch per call.
		// Both labelled contexts are built once (procLabel), so a call
		// switches the goroutine's labels and allocates nothing — pprof.Do
		// built a context and a label set per call, which was most of what
		// a profiled run allocated.
		pprof.SetGoroutineLabels(ps.callee)
		defer pprof.SetGoroutineLabels(ps.caller)
		return ps.s.runCodeOwned(pc, ps.p, args, owned)
	}
	return ps.s.runCodeOwned(pc, ps.p, args, owned)
}

// run executes compiled statements, stopping at the first control transfer.
func runStmts(body []cstmt, fr *pfr) (ctlSignal, error) {
	for _, st := range body {
		sig, err := st(fr)
		if err != nil || sig.kind != ctlNone {
			return sig, err
		}
	}
	return ctlSignal{}, nil
}

// newFrame hands out a zeroed frame for one invocation, reusing an idle one
// when there is one: a call allocates nothing for its locals in steady state.
func (pc *procCode) newFrame() *pfr {
	if fr, ok := pc.frames.Get().(*pfr); ok {
		return fr
	}
	return &pfr{slots: make([]any, pc.nslots), own: make([]bool, pc.nslots), ints: make([]int, pc.nslots)}
}

// bindParam binds argument i to its parameter's slot: an int parameter
// unboxed, anything else shared (owned when the caller handed it over).
func (pc *procCode) bindParam(fr *pfr, i int, v any, owned bool) {
	if pc.intParam[i] {
		fr.ints[pc.params[i]] = asInt(v)
		return
	}
	fr.set(pc.params[i], v, owned)
}

// release zeroes a finished invocation's frame (so it pins none of the
// values it held) and makes it available to the next call.
func (pc *procCode) release(fr *pfr) {
	clear(fr.slots)
	clear(fr.own)
	fr.bld = nil
	pc.frames.Put(fr)
}

// set binds v to slot i; owned says v was created fresh for this binding.
func (fr *pfr) set(i int, v any, owned bool) {
	fr.slots[i] = v
	fr.own[i] = owned
}

// mutable returns slot i's value ready for an in-place write: copied first
// (one level, cloneCompositeValue) unless the slot already owns it.
func (fr *pfr) mutable(i int) any {
	if !fr.own[i] {
		fr.slots[i] = cloneCompositeValue(fr.slots[i])
		fr.own[i] = true
	}
	return fr.slots[i]
}

// structLit compiles a struct literal: each argument goes to its field's
// position in the type's layout, resolved here once. A field filled with a
// value created for it — or with a bare local handed over at its last use
// (moveArg) — is the struct's own from the start (structVal.own).
func (c *procCompiler) structLit(e *ir.Expr) cexpr {
	names, typ := e.Fields, e.Name
	vals := make([]argFn, len(e.Args))
	for i, a := range e.Args {
		vals[i] = c.moveArg(a, -1)
	}
	lay := c.s.structLayout(typ, names)
	pos := make([]int, len(names))
	for i, n := range names {
		p, ok := lay.index[n]
		if !ok {
			field := n
			return func(*pfr) (any, error) {
				return nil, fmt.Errorf("struct %q has no field %q", typ, field)
			}
		}
		pos[i] = p
	}
	width := len(lay.fields)
	return func(fr *pfr) (any, error) {
		out := newStructVal(lay, width)
		var own uint64
		for i, val := range vals {
			v, owned, err := val(fr)
			if err != nil {
				return nil, err
			}
			out.vals[pos[i]] = v
			if owned && pos[i] < 64 {
				own |= 1 << uint(pos[i])
			}
		}
		out.own = own
		return out, nil
	}
}

// fieldPath is e's field path when e is a chain of field reads rooted at a
// name (`s.f.g`: "s", ["f", "g"]), else ok false.
func fieldPath(e *ir.Expr) (root string, path []string, ok bool) {
	for e != nil && e.Kind == "get" {
		path = append(path, e.Field)
		e = e.Obj
	}
	if e == nil || e.Kind != "ref" || len(path) == 0 {
		return "", nil, false
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return e.Name, path, true
}

// takePath compiles a field path of a local bound or passed on: its value,
// and whether its ownership comes with it — when the analysis proves
// nothing reads the path (or its local whole) again (fieldMoveCand) and,
// at run time, the slot owns its struct and each struct on the path owns
// the next. Otherwise the value is shared exactly as a retained field read
// shares it: a one-field path's struct gives the field up.
func (c *procCompiler) takePath(slot int, root string, path []string) argFn {
	c.fieldObj = true
	c.lookup(root)
	key := strings.Join(path, ".")
	c.pathReads = append(c.pathReads, pathRead{slot, key, c.pos})
	mv := new(bool)
	c.fieldMoves = append(c.fieldMoves, fieldMoveCand{slot: slot, path: key, pos: c.pos, ok: mv})
	// the path's value is retained: the local escapes, as a retained read
	c.escaped[slot] = true
	caches := make([]atomic.Pointer[fieldPos], len(path))
	last := len(path) - 1
	return func(fr *pfr) (any, bool, error) {
		v := fr.slots[slot]
		// held: the slot owns the root, and each struct so far the next
		held := fr.own[slot]
		for k, field := range path {
			switch o := v.(type) {
			case structVal:
				var i int
				if fp := caches[k].Load(); fp != nil && fp.lay == o.lay {
					i = fp.i
				} else {
					var has bool
					i, has = o.lay.index[field]
					if !has {
						return nil, false, nil
					}
					caches[k].Store(&fieldPos{lay: o.lay, i: i})
				}
				bit := i < 64 && o.own&(1<<uint(i)) != 0
				if k == last {
					if held && bit && (*mv || last == 0) {
						o.own &^= 1 << uint(i)
						return o.vals[i], *mv, nil
					}
					return o.vals[i], false, nil
				}
				held = held && bit
				v = o.vals[i]
			case record:
				if k == last {
					return o[field], false, nil
				}
				v, held = o[field], false
			default:
				return nil, false, fmt.Errorf("cannot read field %q of a value that is not a struct", field)
			}
		}
		return v, false, nil
	}
}

// getObj compiles a field read's object (e a get). A local at the root of
// a field path is not an occurrence of the local whole, and the path —
// the whole chain, once, at its outermost get — is recorded as read, for
// the field-move analysis (takePath).
func (c *procCompiler) getObj(e *ir.Expr) cexpr {
	terminal := !c.chainInner
	c.chainInner = e.Obj != nil && e.Obj.Kind == "get"
	c.fieldObj = e.Obj != nil && e.Obj.Kind == "ref"
	obj := c.use(e.Obj)
	c.fieldObj, c.chainInner = false, false
	if terminal {
		if rs, _, path, ok := c.localPath(e); ok {
			c.pathReads = append(c.pathReads, pathRead{rs, strings.Join(path, "."), c.pos})
		}
	}
	return obj
}

// localPath is e's field path when it is rooted at a declared, non-int
// local, with that local's slot.
func (c *procCompiler) localPath(e *ir.Expr) (slot int, root string, path []string, ok bool) {
	if c.lastPos == nil {
		return 0, "", nil, false
	}
	root, path, ok = fieldPath(e)
	if !ok {
		return 0, "", nil, false
	}
	slot, ok = c.resolve(root)
	if !ok || c.ints[slot] {
		return 0, "", nil, false
	}
	return slot, root, path, true
}

// textAppendSpine is e1…en when v is `target + e1 + … + en` (a left spine
// of `+` whose leftmost operand is the local target), else nil.
func textAppendSpine(v *ir.Expr, target string) []*ir.Expr {
	var rev []*ir.Expr
	for v != nil && v.Kind == "bin" && v.Op == "+" && v.R != nil {
		rev = append(rev, v.R)
		v = v.L
	}
	if len(rev) == 0 || v == nil || v.Kind != "ref" || v.Name != target {
		return nil
	}
	out := make([]*ir.Expr, len(rev))
	for i, x := range rev {
		out[len(rev)-1-i] = x
	}
	return out
}

// appendText is frame.appendText per slot: grow the text in slot i in place.
func (fr *pfr) appendText(i int, cur, add string) {
	if fr.bld == nil {
		fr.bld = make([]*strings.Builder, len(fr.slots))
	}
	b := fr.bld[i]
	if b == nil || b.Len() != len(cur) || (len(cur) > 0 && unsafe.StringData(b.String()) != unsafe.StringData(cur)) {
		b = &strings.Builder{}
		b.Grow(2*len(cur) + len(add) + 64)
		b.WriteString(cur)
		fr.bld[i] = b
	}
	b.WriteString(add)
	fr.slots[i] = b.String()
}

// ── statements ───────────────────────────────────────────────────────────

func (c *procCompiler) block(body []ir.Stmt) []cstmt {
	c.push()
	out := c.stmts(body)
	c.pop()
	return out
}

func (c *procCompiler) stmts(body []ir.Stmt) []cstmt {
	out := make([]cstmt, 0, len(body))
	for i := range body {
		if st := c.stmt(&body[i]); st != nil {
			out = append(out, st)
		}
	}
	return out
}

func (c *procCompiler) exprs(xs []*ir.Expr) []cexpr {
	out := make([]cexpr, len(xs))
	for i, x := range xs {
		out[i] = c.expr(x)
	}
	return out
}

func evalAll(xs []cexpr, fr *pfr) ([]any, error) {
	out := make([]any, len(xs))
	for i, x := range xs {
		v, err := x(fr)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (c *procCompiler) stmt(st *ir.Stmt) cstmt {
	s := c.s
	switch st.Op {
	case "let":
		isInt, isIntList := c.isInt(st.Value), c.isIntList(st.Value)
		// the struct type the local will hold, decided before it is declared
		// (the value is read in the enclosing scope) and recorded once it is
		letStruct := c.structTypeOf(st.Value)
		letFloat, letFloatList := c.isFloat(st.Value), c.isFloatList(st.Value)
		defer func() {
			if slot, ok := c.resolve(st.Target); ok && !c.ints[slot] {
				if letStruct != "" {
					c.structTypes[slot] = letStruct
				}
				c.floats[slot] = letFloat
				c.floatLists[slot] = letFloatList
			}
		}()
		if isInt {
			val := c.intExpr(st.Value)
			slot := c.declare(st.Target)
			c.ints[slot] = true
			return func(fr *pfr) (ctlSignal, error) {
				n, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.ints[slot] = n
				return ctlSignal{}, nil
			}
		}
		if st.Value != nil && st.Value.Kind == "get" && strings.HasPrefix(st.Target, "__w") && st.Value.Obj != nil && st.Value.Obj.Kind == "ref" {
			// The compiler's own temporary for a nested write (`s.f[k] = v`
			// is `let mut __w = s.f; __w[k] = v; s.f = __w`, see the
			// parser's desugarNestedWrite): the field is taken with its
			// ownership when the slot owns the struct and the struct owns
			// the field, so the write is in place rather than a copy of the
			// whole field. Nothing reads s.f between here and the write-back
			// but the written value's own expression, which the write
			// evaluates first.
			objSlot, objOK := c.lookup(st.Value.Obj.Name)
			field := st.Value.Field
			get := c.expr(st.Value)
			slot := c.declare(st.Target)
			return func(fr *pfr) (ctlSignal, error) {
				if objOK && !c.ints[objSlot] && fr.own[objSlot] {
					if sv, ok := fr.slots[objSlot].(structVal); ok {
						if i, has := sv.lay.index[field]; has && i < 64 && sv.own&(1<<uint(i)) != 0 {
							sv.own &^= 1 << uint(i)
							fr.slots[objSlot] = sv
							fr.set(slot, sv.vals[i], true)
							return ctlSignal{}, nil
						}
					}
				}
				v, err := get(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, v, false)
				return ctlSignal{}, nil
			}
		}
		if rs, root, path, isPath := c.localPath(st.Value); isPath {
			// `let x = s.f…`: x takes the field with its ownership when
			// nothing reads it again (takePath)
			val := c.takePath(rs, root, path)
			slot := c.declare(st.Target)
			c.ints[slot], c.intLists[slot] = isInt, isIntList
			return func(fr *pfr) (ctlSignal, error) {
				v, owned, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, v, owned)
				return ctlSignal{}, nil
			}
		}
		if st.Value != nil && st.Value.Kind == "ref" {
			// `let b = b0` where b0 is not used again hands the value
			// (and its ownership) over rather than sharing it.
			val := c.moveArg(st.Value, -1)
			slot := c.declare(st.Target)
			c.ints[slot], c.intLists[slot] = isInt, isIntList
			return func(fr *pfr) (ctlSignal, error) {
				v, owned, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, v, owned)
				return ctlSignal{}, nil
			}
		}
		val := c.ownedExpr(st.Value, -1)
		slot := c.declare(st.Target)
		c.ints[slot], c.intLists[slot] = isInt, isIntList
		return func(fr *pfr) (ctlSignal, error) {
			v, owned, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.set(slot, v, owned)
			return ctlSignal{}, nil
		}
	case "assign":
		slot := c.target(st.Target)
		v := st.Value
		if c.ints[slot] {
			// An int local: the value, computed natively when it can be.
			if c.isInt(v) {
				val := c.intExpr(v)
				return func(fr *pfr) (ctlSignal, error) {
					n, err := val(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					fr.ints[slot] = n
					return ctlSignal{}, nil
				}
			}
			val := c.expr(v)
			return func(fr *pfr) (ctlSignal, error) {
				nv, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.ints[slot] = asInt(nv)
				return ctlSignal{}, nil
			}
		}
		// `x = x + e1 + … + en` on a text local appends in place (see
		// appendText): `+` is left-associative, so that is
		// `((x + e1) + …) + en`, and text `+` anything is the text followed
		// by toStr of it (applyBin) — every step a concatenation onto x.
		// Each ei is evaluated first, in order, as the plain expression
		// evaluates them (none can assign x, so reading x before or after
		// them is the same), then all are appended.
		if adds := textAppendSpine(v, st.Target); adds != nil {
			parts := c.exprs(adds)
			whole := c.expr(st.Value)
			fresh := c.fresh(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if cs, isText := fr.slots[slot].(string); isText {
					if len(parts) == 1 {
						rv, err := parts[0](fr)
						if err != nil {
							return ctlSignal{}, err
						}
						fr.appendText(slot, cs, toStr(rv))
						return ctlSignal{}, nil
					}
					var buf [8]string
					strs := buf[:0]
					for _, p := range parts {
						rv, err := p(fr)
						if err != nil {
							return ctlSignal{}, err
						}
						strs = append(strs, toStr(rv))
					}
					for _, add := range strs {
						fr.appendText(slot, cs, add)
						cs = fr.slots[slot].(string)
					}
					return ctlSignal{}, nil
				}
				nv, err := whole(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, nv, fresh)
				return ctlSignal{}, nil
			}
		}
		// `b = bytesPut(b, at, src)` writes into an owned byte buffer in
		// place: the builtin's value is b with src at at, and a slot that
		// owns b is the only holder of it, so the write is that value.
		if v != nil && v.Kind == "call" && v.Name == "bytesPut" && c.s.byProc["bytesPut"] == nil && len(v.Args) == 3 && v.Args[0] != nil && v.Args[0].Kind == "ref" && v.Args[0].Name == st.Target {
			at, src := c.intExpr(v.Args[1]), c.use(v.Args[2])
			whole := c.expr(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if b, isBytes := fr.slots[slot].(bytesVal); isBytes && fr.own[slot] {
					a, err := at(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					sv, err := src(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					// src is read before the write: it may be (a slice of) b
					if err := bytesPutInto(b, a, sv); err != nil {
						return ctlSignal{}, err
					}
					return ctlSignal{}, nil
				}
				nv, err := whole(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, nv, true)
				return ctlSignal{}, nil
			}
		}
		// `xs = append(xs, e)` grows an owned list in place.
		if v != nil && v.Kind == "call" && v.Name == "append" && c.s.byProc["append"] == nil && len(v.Args) == 2 && v.Args[0] != nil && v.Args[0].Kind == "ref" && v.Args[0].Name == st.Target {
			elem := c.expr(v.Args[1])
			whole := c.expr(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if arr, isList := fr.slots[slot].([]any); isList && fr.own[slot] {
					ev, err := elem(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					// Room to grow, and the slot still the list's only
					// holder once the element is evaluated (the element may
					// have read it): the box the slot holds is written in
					// place (growOwnedList) instead of boxing a new header.
					if fr.own[slot] {
						growOwnedList(&fr.slots[slot], ev)
						return ctlSignal{}, nil
					}
					fr.slots[slot] = append(arr, ev)
					return ctlSignal{}, nil
				}
				if b, isBytes := fr.slots[slot].(bytesVal); isBytes && fr.own[slot] {
					ev, err := elem(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					if x, ok := isByteInt(ev); ok {
						fr.slots[slot] = append(b, x)
					} else {
						fr.slots[slot] = append(promoteBytes(b), ev)
					}
					return ctlSignal{}, nil
				}
				nv, err := whole(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				fr.set(slot, nv, true)
				return ctlSignal{}, nil
			}
		}
		from := len(c.moves)
		val := c.ownedExpr(st.Value, slot)
		c.selfMoves(from)
		return func(fr *pfr) (ctlSignal, error) {
			nv, owned, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.set(slot, nv, owned)
			return ctlSignal{}, nil
		}
	case "indexset":
		slot := c.target(st.Target)
		name, isBytes := st.Target, st.Bytes
		if c.intLists[slot] && c.isInt(st.Key) && c.isInt(st.Value) {
			// An int written into an int list at an int index: all native.
			key, val := c.intExpr(st.Key), c.intExpr(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if listLen(fr.slots[slot]) < 0 {
					return ctlSignal{}, fmt.Errorf("%q is not an array or map", name)
				}
				idx, err := key(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if n := listLen(fr.slots[slot]); idx < 0 || idx >= n {
					return ctlSignal{}, fmt.Errorf("array index %d out of bounds (length %d) assigning to %q", idx, n, name)
				}
				n, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if isBytes && (n < 0 || n > 255) {
					return ctlSignal{}, fmt.Errorf("byte value %d out of range (must be 0-255) assigning to %q[%d]", n, name, idx)
				}
				switch coll := fr.mutable(slot).(type) {
				case bytesVal:
					if n >= 0 && n <= 255 {
						coll[idx] = byte(n)
					} else {
						// a value a byte cannot hold, into a buffer the
						// compiler types as a plain [int]: promoted
						p := promoteBytes(coll)
						p[idx] = boxInt(n)
						fr.slots[slot] = p
					}
				case []any:
					coll[idx] = boxInt(n)
				}
				return ctlSignal{}, nil
			}
		}
		key, val := c.use(st.Key), c.expr(st.Value)
		return func(fr *pfr) (ctlSignal, error) {
			switch fr.slots[slot].(type) {
			case []any, map[any]any, bytesVal:
			default:
				return ctlSignal{}, fmt.Errorf("%q is not an array or map", name)
			}
			switch coll := fr.mutable(slot).(type) {
			case bytesVal:
				idxV, err := key(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				idx := toInt(idxV)
				if idx < 0 || idx >= len(coll) {
					return ctlSignal{}, fmt.Errorf("array index %d out of bounds (length %d) assigning to %q", idx, len(coll), name)
				}
				v, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if x, ok := isByteInt(v); ok {
					coll[idx] = x
				} else if isBytes {
					return ctlSignal{}, fmt.Errorf("byte value %d out of range (must be 0-255) assigning to %q[%d]", toInt(v), name, idx)
				} else {
					p := promoteBytes(coll)
					p[idx] = v
					fr.slots[slot] = p
				}
			case []any:
				idxV, err := key(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				idx := toInt(idxV)
				if idx < 0 || idx >= len(coll) {
					return ctlSignal{}, fmt.Errorf("array index %d out of bounds (length %d) assigning to %q", idx, len(coll), name)
				}
				v, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if isBytes {
					n := toInt(v)
					if n < 0 || n > 255 {
						return ctlSignal{}, fmt.Errorf("byte value %d out of range (must be 0-255) assigning to %q[%d]", n, name, idx)
					}
					v = n
				}
				coll[idx] = v
			case map[any]any:
				idxV, err := key(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				k, err := mapKey(idxV)
				if err != nil {
					return ctlSignal{}, err
				}
				v, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				coll[k] = v
			default:
				return ctlSignal{}, fmt.Errorf("%q is not an array or map", name)
			}
			return ctlSignal{}, nil
		}
	case "do":
		proc, ret, retList := st.Service, st.Ret, st.RetList
		// a `{K: V}` result is a map, whatever V is — never a native int slot
		retMap := st.RetMap
		// a nested list result's elements are lists — never an int/float list
		retDepth := st.RetDepth
		self := -1
		if st.Bind == "" && st.Target != "" {
			if slot, ok := c.resolve(st.Target); ok {
				self = slot
			}
		}
		from := len(c.moves)
		args := make([]argFn, len(st.Args))
		doSite := c.site(proc)
		for i, a := range st.Args {
			args[i] = c.moveArgTo(a, self, doSite, i)
		}
		c.selfMoves(from)
		bind, into := -1, -1
		if st.Bind != "" {
			bind = c.declare(st.Bind)
			c.ints[bind] = ret == "int" && !retList && !retMap
			c.intLists[bind] = ret == "int" && retList && !retMap && retDepth <= 1
			if !retList && !retMap && c.isStructType(ret) {
				c.structTypes[bind] = ret
			}
			c.floats[bind] = ret == "float" && !retList && !retMap
			c.floatLists[bind] = ret == "float" && retList && !retMap && retDepth <= 1
		} else if st.Target != "" {
			into = c.target(st.Target)
		}
		bindInt := bind >= 0 && c.ints[bind]
		intoInt := into >= 0 && c.ints[into]
		site := c.site(proc)
		return func(fr *pfr) (ctlSignal, error) {
			if site == nil {
				return ctlSignal{}, fmt.Errorf("calls unknown proc %q", proc)
			}
			var buf [6]any
			vals, mask, err := callArgs(args, fr, buf[:])
			if err != nil {
				return ctlSignal{}, err
			}
			res, owned, err := site.call(vals, mask)
			if err != nil {
				return ctlSignal{}, err
			}
			// The result is this slot's own when the callee handed over a
			// value nothing else references (or coercion built a new one).
			if bind >= 0 {
				v := s.coerceRetDepth(res, ret, retList, retDepth)
				if bindInt {
					fr.ints[bind] = asInt(v)
				} else {
					fr.set(bind, v, owned || !sameList(v, res))
				}
			} else if into >= 0 {
				v := s.coerceRetDepth(res, ret, retList, retDepth)
				if intoInt {
					fr.ints[into] = asInt(v)
				} else {
					fr.set(into, v, owned || !sameList(v, res))
				}
			}
			return ctlSignal{}, nil
		}
	case "fieldset":
		slot := c.target(st.Target)
		name, field := st.Target, st.Field
		v := st.Value
		if v != nil && v.Kind == "call" && v.Name == "append" && c.s.byProc["append"] == nil && len(v.Args) == 2 && v.Args[0] != nil && v.Args[0].Kind == "get" && v.Args[0].Field == field && v.Args[0].Obj != nil && v.Args[0].Obj.Kind == "ref" && v.Args[0].Obj.Name == st.Target {
			// `s.f = append(s.f, e)` grows the field's list in place when
			// the slot owns the struct and the struct owns the list
			// (structVal.own); otherwise the builtin's copy, which the
			// struct then owns.
			c.lookup(st.Target)
			elem := c.expr(v.Args[1])
			return func(fr *pfr) (ctlSignal, error) {
				sv, ok := fr.slots[slot].(structVal)
				if !ok {
					return ctlSignal{}, fmt.Errorf("%q is not a struct", name)
				}
				i, has := sv.lay.index[field]
				if !has {
					return ctlSignal{}, fmt.Errorf("struct %q has no field %q", sv.lay.name, field)
				}
				ev, err := elem(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				sv = fr.slots[slot].(structVal)
				if fr.own[slot] && i < 64 && sv.own&(1<<uint(i)) != 0 {
					if arr, isList := sv.vals[i].([]any); isList {
						sv.vals[i] = append(arr, ev)
						return ctlSignal{}, nil
					}
					if b, isBytes := sv.vals[i].(bytesVal); isBytes {
						if x, ok := isByteInt(ev); ok {
							sv.vals[i] = append(b, x)
						} else {
							sv.vals[i] = append(promoteBytes(b), ev)
						}
						return ctlSignal{}, nil
					}
				}
				sv = fr.mutable(slot).(structVal)
				sv.vals[i] = appendCopy(sv.vals[i], ev)
				if i < 64 && sv.own&(1<<uint(i)) == 0 {
					sv.own |= 1 << uint(i)
					fr.slots[slot] = sv
				}
				return ctlSignal{}, nil
			}
		}
		// A bare local stored into the field hands its value (and, when it
		// is dead after this store, its ownership) over, as a `let` of it
		// does; anything else is owned exactly when it is fresh.
		var val func(*pfr) (any, bool, error)
		if v != nil && v.Kind == "ref" {
			val = c.moveArg(v, -1)
		} else {
			x, fresh := c.expr(v), c.fresh(v)
			val = func(fr *pfr) (any, bool, error) {
				nv, err := x(fr)
				return nv, fresh, err
			}
		}
		// The field's position, cached against the layout last written
		// here as a read's is (the "get" case).
		var cache atomic.Pointer[fieldPos]
		return func(fr *pfr) (ctlSignal, error) {
			sv, ok := fr.slots[slot].(structVal)
			if !ok {
				return ctlSignal{}, fmt.Errorf("%q is not a struct", name)
			}
			var i int
			if fp := cache.Load(); fp != nil && fp.lay == sv.lay {
				i = fp.i
			} else {
				var has bool
				i, has = sv.lay.index[field]
				if !has {
					return ctlSignal{}, fmt.Errorf("struct %q has no field %q", sv.lay.name, field)
				}
				cache.Store(&fieldPos{lay: sv.lay, i: i})
			}
			nv, fresh, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			sv = fr.mutable(slot).(structVal)
			sv.vals[i] = nv
			// The struct owns the field's value exactly when nothing else
			// references it (structVal.own).
			if i < 64 {
				bit := uint64(1) << uint(i)
				if (sv.own&bit != 0) != fresh {
					sv.own ^= bit
					fr.slots[slot] = sv
				}
			}
			return ctlSignal{}, nil
		}
	case "actcall":
		args := c.exprs(st.Args)
		name := st.Service
		return func(fr *pfr) (ctlSignal, error) {
			act := s.byAction[name]
			if act == nil {
				return ctlSignal{}, fmt.Errorf("act calls unknown action %q", name)
			}
			vals, err := evalAll(args, fr)
			if err != nil {
				return ctlSignal{}, err
			}
			if _, status, msg := s.runAction(systemSID, act, vals); status != http.StatusOK {
				return ctlSignal{}, fmt.Errorf("act %s failed: %s", name, msg)
			}
			return ctlSignal{}, nil
		}
	case "exprstmt":
		val := c.expr(st.Value)
		return func(fr *pfr) (ctlSignal, error) {
			_, err := val(fr)
			return ctlSignal{}, err
		}
	case "fileread":
		path, isBytes := st.Path, st.Bytes
		bind := -1
		if st.Bind != "" {
			bind = c.declare(st.Bind)
		}
		return func(fr *pfr) (ctlSignal, error) {
			var v any
			var err error
			if isBytes {
				v, err = s.ioReadFileBytes(path)
			} else {
				v, err = s.ioReadFile(path)
			}
			if err != nil {
				return ctlSignal{}, err
			}
			if bind >= 0 {
				fr.set(bind, v, false)
			}
			return ctlSignal{}, nil
		}
	case "filewrite":
		val := c.expr(st.Value)
		path, isBytes := st.Path, st.Bytes
		bind := -1
		if st.Bind != "" {
			bind = c.declare(st.Bind)
		}
		return func(fr *pfr) (ctlSignal, error) {
			v, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			var res any
			if isBytes {
				res, err = s.ioWriteFileBytes(path, v)
			} else {
				res, err = s.ioWriteFile(path, toStr(v))
			}
			if err != nil {
				return ctlSignal{}, err
			}
			if bind >= 0 {
				fr.set(bind, res, false)
			}
			return ctlSignal{}, nil
		}
	case "spawn":
		args := c.exprs(st.Args)
		proc := st.Service
		slot := c.declare(st.Target)
		return func(fr *pfr) (ctlSignal, error) {
			sub := s.byProc[proc]
			if sub == nil {
				return ctlSignal{}, fmt.Errorf("spawn calls unknown proc %q", proc)
			}
			// Reading the arguments cleared their slots' ownership, so
			// neither this frame nor the child mutates a shared value in
			// place: whichever writes first copies.
			vals, err := evalAll(args, fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.own[slot] = false
			fr.slots[slot] = spawnTask(func() (any, error) {
				return s.runProcLocked(sub, vals)
			})
			return ctlSignal{}, nil
		}
	case "join":
		handle := c.target(st.Target)
		name, ret, retList := st.Target, st.Ret, st.RetList
		retDepth := st.RetDepth
		bind := -1
		if st.Bind != "" {
			bind = c.declare(st.Bind)
		}
		return func(fr *pfr) (ctlSignal, error) {
			h, ok := fr.slots[handle].(*taskHandle)
			if !ok {
				return ctlSignal{}, fmt.Errorf("%q is not a spawned task handle", name)
			}
			res, err := h.join()
			if err != nil {
				return ctlSignal{}, err
			}
			if bind >= 0 {
				fr.set(bind, s.coerceRetDepth(res, ret, retList, retDepth), false)
			}
			return ctlSignal{}, nil
		}
	case "return":
		// A returned local is handed to the caller with its ownership: the
		// frame is released right after, so nothing else can reach it.
		if st.Value != nil && st.Value.Kind == "ref" {
			if slot, ok := c.lookup(st.Value.Name); ok && c.ints[slot] {
				return func(fr *pfr) (ctlSignal, error) {
					return ctlSignal{kind: ctlReturn, val: boxInt(fr.ints[slot])}, nil
				}
			} else if ok {
				c.escaped[slot] = true
				return func(fr *pfr) (ctlSignal, error) {
					v, owned := fr.slots[slot], fr.own[slot]
					fr.own[slot] = false
					return ctlSignal{kind: ctlReturn, val: v, owned: owned}, nil
				}
			}
		}
		val := c.moveArg(st.Value, -1)
		return func(fr *pfr) (ctlSignal, error) {
			v, owned, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			return ctlSignal{kind: ctlReturn, val: v, owned: owned}, nil
		}
	case "break":
		return func(*pfr) (ctlSignal, error) { return ctlSignal{kind: ctlBreak}, nil }
	case "continue":
		return func(*pfr) (ctlSignal, error) { return ctlSignal{kind: ctlContinue}, nil }
	case "loop":
		start := c.pos
		cond := c.use(st.Value)
		body := c.block(st.Body)
		c.loops = append(c.loops, [2]int{start, c.pos})
		return func(fr *pfr) (ctlSignal, error) {
			for {
				cv, err := cond(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if !truthy(cv) {
					return ctlSignal{}, nil
				}
				sig, err := runStmts(body, fr)
				if err != nil {
					return ctlSignal{}, err
				}
				switch sig.kind {
				case ctlReturn:
					return sig, nil
				case ctlBreak:
					return ctlSignal{}, nil
				}
			}
		}
	case "if":
		cond := c.use(st.Value)
		then := c.block(st.Body)
		var els []cstmt
		hasElse := st.Else != nil
		if hasElse {
			els = c.block(st.Else)
		}
		return func(fr *pfr) (ctlSignal, error) {
			cv, err := cond(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			if truthy(cv) {
				return runStmts(then, fr)
			}
			if hasElse {
				return runStmts(els, fr)
			}
			return ctlSignal{}, nil
		}
	}
	// An op a proc body never holds: nothing to run, as the interpreter
	// skipped it.
	return nil
}

// ── expressions ──────────────────────────────────────────────────────────

type fieldPos struct {
	lay *structLayout
	i   int
}

// structLayout is the layout of struct type name: its declaration's field
// order (built once per server), or — for a type the graph does not declare,
// which compiled source never produces — the literal's own.
func (s *Server) structLayout(name string, literal []string) *structLayout {
	s.layoutOnce.Do(func() {
		s.layouts = map[string]*structLayout{}
		for _, st := range s.ir.Structs {
			fields := make([]string, len(st.Fields))
			for i, f := range st.Fields {
				fields[i] = f.Name
			}
			s.layouts[st.Name] = newStructLayout(st.Name, fields)
		}
	})
	if l, ok := s.layouts[name]; ok {
		return l
	}
	return newStructLayout(name, append([]string{}, literal...))
}

// procArgValue converts an argument to the shape a proc's parameter of type
// typ (a list of it when list) reads: a map — the row an action's projection or
// a decoded JSON body produced — becomes a structVal in the declared struct's
// or wire type's field order, recursively through struct-typed fields and
// lists of them. A value that is already a structVal, a scalar, or a name that
// is no composite type passes through untouched.
//
// The type is looked up first (one map read: compositeFields), because
// only a struct- or wire-typed parameter can be handed a map at all: a
// list of ints — a page, a byte buffer, a key — or of texts passes through
// with no look at its elements, so a proc handing a 16 KB page down a call
// chain costs nothing per call. A list of a composite type is scanned for
// a map and copied only from the first one on; nearly every such argument
// (every proc-to-proc call's) is already a list of structVals and is
// shared, as slot ownership (see the header) expects.
func (s *Server) procArgValue(typ string, list bool, v any) any {
	if v == nil {
		return v
	}
	if list {
		xs, ok := v.([]any)
		if !ok || len(xs) == 0 || s.compositeFields(typ) == nil {
			return v
		}
		first := -1
		for i, x := range xs {
			if _, isMap := x.(map[string]any); isMap {
				first = i
				break
			}
		}
		if first < 0 {
			return v
		}
		out := make([]any, len(xs))
		copy(out, xs[:first])
		for i := first; i < len(xs); i++ {
			out[i] = s.procArgValue(typ, false, xs[i])
		}
		return out
	}
	m, ok := v.(map[string]any) // `record` is this same type
	if !ok {
		return v
	}
	fields := s.compositeFields(typ)
	if fields == nil {
		return v
	}
	names := make([]string, len(fields))
	vals := make([]any, len(fields))
	for i, f := range fields {
		names[i] = f.Name
		if f.Map {
			vals[i] = s.procArgMap(f.Key, f.Type, m[f.Name])
		} else {
			vals[i] = s.procArgValue(f.Type, f.List, m[f.Name])
		}
	}
	out := newStructVal(s.structLayout(typ, names), len(vals))
	copy(out.vals, vals)
	return out
}

// procArgMap is procArgValue for a `{key: typ}` parameter or field: a map
// the proc engine already holds (map[any]any) passes through; a decoded
// JSON object (map[string]any, or an action's record) becomes one, its keys
// read as the declared key type (an int key arrives as its decimal text) and
// its values converted as procArgValue converts a parameter of typ.
func (s *Server) procArgMap(key, typ string, v any) any {
	var obj map[string]any
	switch t := v.(type) {
	case nil, map[any]any:
		return v
	case map[string]any:
		obj = t
	default:
		return v
	}
	out := make(map[any]any, len(obj))
	for k, val := range obj {
		var mk any = k
		if key == "int" {
			mk = toInt(k)
		}
		out[mk] = s.procArgValue(typ, false, val)
	}
	return out
}

// plainValue is procArgValue's inverse for a value leaving the proc engine
// for an action: every structVal becomes the record its fields spell, lists
// element-wise, so the action's reply JSON-encodes as an object and its own
// `.field` reads (eval.go) work on it. Anything else passes through.
func plainValue(v any) any {
	switch t := v.(type) {
	case structVal:
		out := record{}
		for i, name := range t.lay.fields {
			if i < len(t.vals) {
				out[name] = plainValue(t.vals[i])
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = plainValue(x)
		}
		return out
	case bytesVal:
		return promoteBytes(t)
	case map[any]any:
		// A map's keys are int or text; as JSON they are the object's
		// (text) keys, so a wire reply reads them back as procArgMap does.
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[toStr(k)] = plainValue(x)
		}
		return out
	}
	return v
}

// compositeFields is the declared field list of a proc `struct` or a wire
// `type` named typ, or nil when typ names neither (a scalar, an entity, an
// enum): the two declaration kinds a proc parameter can be typed with. One
// map read: it is asked on every parameter of every proc call, and a
// program the size of the self-hosted engine declares hundreds of structs.
func (s *Server) compositeFields(typ string) []ir.RecordField {
	s.compOnce.Do(func() {
		s.composites = map[string][]ir.RecordField{}
		for _, t := range s.ir.Types {
			out := make([]ir.RecordField, len(t.Fields))
			for i, f := range t.Fields {
				out[i] = ir.RecordField{Name: f.Name, Type: f.Type, List: f.List, Optional: f.Optional}
			}
			s.composites[t.Name] = out
		}
		// A struct wins over a wire type of the same name, as the linear
		// search this replaces found it first.
		for _, st := range s.ir.Structs {
			s.composites[st.Name] = st.Fields
		}
	})
	return s.composites[typ]
}

func constExpr(v any) cexpr { return func(*pfr) (any, error) { return v, nil } }

// expr compiles e where its value may be retained (bound, stored, passed on);
// use compiles e where it is only inspected. The difference matters for a
// bare slot reference: a retained read clears the slot's ownership bit.
func (c *procCompiler) expr(e *ir.Expr) cexpr { return c.compileExpr(e, true) }
func (c *procCompiler) use(e *ir.Expr) cexpr  { return c.compileExpr(e, false) }

func (c *procCompiler) uses(xs []*ir.Expr) []cexpr {
	out := make([]cexpr, len(xs))
	for i, x := range xs {
		out[i] = c.use(x)
	}
	return out
}

// inspectOnly lists the builtins that read their arguments without ever
// returning or storing them (append keeps its second argument, so only its
// first is inspected — see the "call" case).
var inspectOnly = map[string]bool{
	"len": true, "byteLen": true, "bytesToText": true, "join": true, "contains": true, "indexOf": true,
	"charAt": true, "writeFileAt": true, "writeBytes": true, "writeFile": true,
	"aesGcmSeal": true, "aesGcmOpen": true, "aesGcmAuthentic": true,
	"appendFile": true, "sha256Hex": true, "canonicalJson": true, "awaitAny": true, "crc32": true, "bytesPut": true,
	"bytesCmp": true, "bytesCmpRange": true, "uintLE": true, "toHex": true,
}

// fresh reports whether e always evaluates to a composite value created by
// that evaluation (or to a scalar, for which ownership is moot), so the slot
// it is bound to owns it.
func (c *procCompiler) fresh(e *ir.Expr) bool {
	if e == nil {
		return true
	}
	switch e.Kind {
	case "lit", "list", "map", "struct", "un":
		return true
	case "bin":
		return e.Op != "" // every operator builds a new value (or a scalar)
	case "call":
		if c.s.byProc[e.Name] != nil {
			return false
		}
		switch e.Name {
		case "append", "bytes", "textToBytes", "split", "readFileAt", "aesGcmSeal", "aesGcmOpen", "fromHex", "bytesPut":
			return true
		}
	}
	return false
}

func (c *procCompiler) compileExpr(e *ir.Expr, escapes bool) cexpr {
	s := c.s
	if e == nil {
		return constExpr(nil)
	}
	if (e.Kind == "bin" || e.Kind == "un" || e.Kind == "index") && c.isInt(e) {
		// An int-valued operator tree runs on native ints; only its result
		// is boxed.
		ie := c.intExpr(e)
		return func(fr *pfr) (any, error) {
			n, err := ie(fr)
			if err != nil {
				return nil, err
			}
			return boxInt(n), nil
		}
	}
	if e.Kind == "bin" && c.isInt(e.L) && c.isInt(e.R) {
		if cmp, ok := intCmp(e.Op); ok {
			l, r := c.intExpr(e.L), c.intExpr(e.R)
			return func(fr *pfr) (any, error) {
				a, err := l(fr)
				if err != nil {
					return nil, err
				}
				b, err := r(fr)
				if err != nil {
					return nil, err
				}
				return cmp(a, b), nil
			}
		}
	}
	// A float operator tree (or a float builtin over one) runs on native
	// float64s — applyBin's float arithmetic, IEEE division included — and
	// boxes only its result; a float comparison boxes nothing.
	if c.isFloat(e) && c.floatNative(e) {
		fe := c.floatExpr(e)
		return func(fr *pfr) (any, error) {
			f, err := fe(fr)
			if err != nil {
				return nil, err
			}
			return f, nil
		}
	}
	if e.Kind == "bin" && c.isFloat(e.L) && c.isFloat(e.R) {
		if cmp, ok := floatCmp(e.Op); ok {
			l, r := c.floatExpr(e.L), c.floatExpr(e.R)
			return func(fr *pfr) (any, error) {
				a, err := l(fr)
				if err != nil {
					return nil, err
				}
				b, err := r(fr)
				if err != nil {
					return nil, err
				}
				return cmp(a, b), nil
			}
		}
	}
	switch e.Kind {
	case "lit":
		return constExpr(litValue(e))
	case "list":
		if !escapes {
			if cv, ok := constList(e); ok {
				// An all-literal list that is only inspected (`c in [...]`,
				// `len([...])`, a loop bound) is one value built when the
				// body is compiled, not a fresh slice per evaluation: no
				// binding can retain or mutate it from here, so sharing it
				// is unobservable.
				return constExpr(cv)
			}
		}
		if len(e.Args) == 0 {
			// `[]`: one shared empty list. It has no capacity, so nothing
			// can write into it in place — an index write is out of bounds
			// and an append allocates — and every binding of it stays its
			// own value.
			return constExpr(emptyList)
		}
		elems := c.exprs(e.Args)
		return func(fr *pfr) (any, error) {
			return evalAll(elems, fr)
		}
	case "map":
		keys, vals := c.exprs(e.Keys), c.exprs(e.Args)
		return func(fr *pfr) (any, error) {
			out := make(map[any]any, len(vals))
			for i, val := range vals {
				kv, err := keys[i](fr)
				if err != nil {
					return nil, err
				}
				k, err := mapKey(kv)
				if err != nil {
					return nil, err
				}
				v, err := val(fr)
				if err != nil {
					return nil, err
				}
				out[k] = v
			}
			return out, nil
		}
	case "struct":
		names, typ, omit, nulls := e.Fields, e.Name, e.Omit, e.Nulls
		if !s.isWireType(typ) {
			return c.structLit(e)
		}
		// A wire DTO a proc builds for an action: the record shape every
		// other wire value has, so it encodes and reads the same.
		vals := c.exprs(e.Args)
		return func(fr *pfr) (any, error) {
			fields := make(map[string]any, len(vals))
			for i, val := range vals {
				v, err := val(fr)
				if err != nil {
					return nil, err
				}
				fields[names[i]] = v
			}
			omitEmpty(fields, omit)
			nullEmpty(fields, nulls)
			return record(fields), nil
		}
	case "get":
		field := e.Field
		obj := c.getObj(e)
		// A retained read of a local's field gives its value a second
		// holder: the struct in that slot no longer owns it (structVal.own).
		disown := -1
		if escapes && e.Obj != nil && e.Obj.Kind == "ref" {
			if slot, ok := c.resolve(e.Obj.Name); ok && !c.ints[slot] {
				disown = slot
			}
		}
		if escapes {
			// a retained field — of a local, or of a field of it — may
			// alias that local's value: the local escapes
			root := e.Obj
			for root != nil && root.Kind == "get" {
				root = root.Obj
			}
			if root != nil && root.Kind == "ref" {
				if slot, ok := c.resolve(root.Name); ok && !c.ints[slot] {
					c.escaped[slot] = true
				}
			}
		}
		// The field's position is cached against the layout last read here
		// (almost always the only one), so a read is a pointer compare and
		// an index; the cache is an atomic pointer because a spawned task
		// runs the same code concurrently.
		var cache atomic.Pointer[fieldPos]
		return func(fr *pfr) (any, error) {
			o, err := obj(fr)
			if err != nil {
				return nil, err
			}
			sv, ok := o.(structVal)
			if !ok {
				// A wire `type` value (a DTO a proc built or was handed) is a
				// record: its field read as an action's `.field` reads it.
				if rec, isRec := o.(record); isRec {
					return rec[field], nil
				}
				return nil, fmt.Errorf("cannot read field %q of a value that is not a struct", field)
			}
			var i int
			if fp := cache.Load(); fp != nil && fp.lay == sv.lay {
				i = fp.i
			} else {
				var has bool
				i, has = sv.lay.index[field]
				if !has {
					return nil, nil
				}
				cache.Store(&fieldPos{lay: sv.lay, i: i})
			}
			if disown >= 0 && i < 64 && sv.own&(1<<uint(i)) != 0 && fr.own[disown] {
				sv.own &^= 1 << uint(i)
				fr.slots[disown] = sv
			}
			return sv.vals[i], nil
		}
	case "ref":
		slot, ok := c.lookup(e.Name)
		if !ok {
			return constExpr(nil)
		}
		if c.ints[slot] {
			return func(fr *pfr) (any, error) { return boxInt(fr.ints[slot]), nil }
		}
		if escapes {
			c.escaped[slot] = true
			return func(fr *pfr) (any, error) {
				fr.own[slot] = false
				return fr.slots[slot], nil
			}
		}
		return func(fr *pfr) (any, error) { return fr.slots[slot], nil }
	case "index":
		obj, key := c.operand(e.Obj), c.operand(e.Key)
		return func(fr *pfr) (any, error) {
			o, err := obj.get(fr)
			if err != nil {
				return nil, err
			}
			idxV, err := key.get(fr)
			if err != nil {
				return nil, err
			}
			switch coll := o.(type) {
			case []any:
				idx := toInt(idxV)
				if idx < 0 || idx >= len(coll) {
					return nil, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
				}
				return coll[idx], nil
			case bytesVal:
				idx := toInt(idxV)
				if idx < 0 || idx >= len(coll) {
					return nil, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
				}
				return bytesElem(coll, idx), nil
			case map[string]any:
				return coll[toStr(idxV)], nil
			case map[any]any:
				k, err := mapKey(idxV)
				if err != nil {
					return nil, err
				}
				return coll[k], nil
			default:
				return nil, fmt.Errorf("cannot index a value that is not an array or map")
			}
		}
	case "un":
		x := c.use(e.X)
		switch e.Op {
		case "!":
			return func(fr *pfr) (any, error) {
				v, err := x(fr)
				if err != nil {
					return nil, err
				}
				return !truthy(v), nil
			}
		case "-":
			return func(fr *pfr) (any, error) {
				v, err := x(fr)
				if err != nil {
					return nil, err
				}
				return negate(v), nil
			}
		case "~":
			return func(fr *pfr) (any, error) {
				v, err := x(fr)
				if err != nil {
					return nil, err
				}
				return ^toInt(v), nil
			}
		}
		return func(fr *pfr) (any, error) {
			_, err := x(fr)
			return nil, err
		}
	case "bin":
		return c.bin(e)
	case "call":
		if site := c.site(e.Name); site != nil {
			args := make([]argFn, len(e.Args))
			for i, a := range e.Args {
				args[i] = c.moveArgTo(a, -1, site, i)
			}
			return func(fr *pfr) (any, error) {
				var buf [6]any
				vals, mask, err := callArgs(args, fr, buf[:])
				if err != nil {
					return nil, err
				}
				v, _, err := site.call(vals, mask)
				return v, err
			}
		}
		// charAt / slice over native int positions: the builtin's own
		// cases, with no boxing of a position (a position past 65535 into a
		// long text would allocate on every read) and no name dispatch —
		// a lexer's every step.
		if e.Name == "charAt" && len(e.Args) == 2 && c.isInt(e.Args[1]) {
			x, at := c.use(e.Args[0]), c.intExpr(e.Args[1])
			return func(fr *pfr) (any, error) {
				sv, err := x(fr)
				if err != nil {
					return nil, err
				}
				i, err := at(fr)
				if err != nil {
					return nil, err
				}
				if t, ok := sv.(string); ok {
					if i < 0 {
						return "", nil
					}
					return boxStr(runeSlice(t, i, i+1)), nil
				}
				return callBuiltin("charAt", []any{sv, boxInt(i)}), nil
			}
		}
		if e.Name == "slice" && len(e.Args) == 3 && c.isInt(e.Args[1]) && c.isInt(e.Args[2]) {
			x, lo, hi := c.use(e.Args[0]), c.intExpr(e.Args[1]), c.intExpr(e.Args[2])
			return func(fr *pfr) (any, error) {
				sv, err := x(fr)
				if err != nil {
					return nil, err
				}
				a, err := lo(fr)
				if err != nil {
					return nil, err
				}
				b, err := hi(fr)
				if err != nil {
					return nil, err
				}
				switch t := sv.(type) {
				case []any:
					return listSlice(t, a, b), nil
				case string:
					return boxStr(runeSlice(t, a, b)), nil
				}
				// A byte buffer, or any other representation: the builtin's
				// own case.
				return callBuiltin("slice", []any{sv, boxInt(a), boxInt(b)}), nil
			}
		}
		var args []cexpr
		switch {
		case inspectOnly[e.Name]:
			args = c.uses(e.Args)
		case e.Name == "append" && len(e.Args) == 2:
			args = []cexpr{c.use(e.Args[0]), c.expr(e.Args[1])}
		default:
			args = c.exprs(e.Args)
		}
		name := e.Name
		return func(fr *pfr) (any, error) {
			// Arguments are gathered on the stack: neither a proc call nor a
			// builtin keeps the argument slice itself.
			var buf [6]any
			var vals []any
			if len(args) <= len(buf) {
				vals = buf[:len(args)]
			} else {
				vals = make([]any, len(args))
			}
			for i, a := range args {
				v, err := a(fr)
				if err != nil {
					return nil, err
				}
				vals[i] = v
			}
			return s.callProcBuiltin(name, vals)
		}
	}
	return constExpr(nil)
}

// operand is an expression compiled for use as an operand: a slot read or a
// constant is fetched inline by the operator's own closure (the leaves of
// nearly every expression in a loop), anything else through its closure.
type operand struct {
	slot int // >= 0: read this slot
	isC  bool
	c    any
	eval cexpr
}

func (c *procCompiler) operand(e *ir.Expr) operand {
	if e != nil && e.Kind == "ref" {
		if slot, ok := c.resolve(e.Name); ok && !c.ints[slot] {
			c.lookup(e.Name)
			return operand{slot: slot}
		}
	}
	if e != nil && e.Kind == "lit" {
		return operand{slot: -1, isC: true, c: litValue(e)}
	}
	return operand{slot: -1, eval: c.use(e)}
}

func (o *operand) get(fr *pfr) (any, error) {
	if o.slot >= 0 {
		return fr.slots[o.slot], nil
	}
	if o.isC {
		return o.c, nil
	}
	return o.eval(fr)
}

// sameList reports whether coercion handed res back as it was (the same
// list, or an equal scalar), so its ownership carries over.
func sameList(v, res any) bool {
	switch a := v.(type) {
	case []any:
		b, ok := res.([]any)
		return ok && len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
	case bytesVal:
		b, ok := res.(bytesVal)
		return ok && len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
	}
	_, okB := res.([]any)
	_, okC := res.(bytesVal)
	return !okB && !okC
}

// ── native int evaluation ───────────────────────────────────────────────

type iexpr func(*pfr) (int, error)

// boxedInts holds the boxed form of every int in [0, 65536): boxing one of
// them (an index, an offset, a byte, a 16-bit word — nearly every int a
// loop over a buffer produces) allocates nothing.
var boxedInts = func() []any {
	out := make([]any, 1<<16)
	for i := range out {
		out[i] = i
	}
	return out
}()

func boxInt(n int) any {
	if n >= 0 && n < len(boxedInts) {
		return boxedInts[n]
	}
	return n
}

// intArith is intOp's arithmetic and bitwise operators on native ints.
func intArith(op string) (func(a, b int) int, bool) {
	switch op {
	case "+":
		return func(a, b int) int { return a + b }, true
	case "-":
		return func(a, b int) int { return a - b }, true
	case "*":
		return func(a, b int) int { return a * b }, true
	case "/":
		return func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		}, true
	case "%":
		return func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a % b
		}, true
	case "&":
		return func(a, b int) int { return a & b }, true
	case "|":
		return func(a, b int) int { return a | b }, true
	case "^":
		return func(a, b int) int { return a ^ b }, true
	case "<<":
		return shiftLeft, true
	case ">>":
		return shiftRight, true
	}
	return nil, false
}

// intCmp is intOp's comparisons on native ints (a bool boxes for free).
func intCmp(op string) (func(a, b int) any, bool) {
	switch op {
	case "<":
		return func(a, b int) any { return a < b }, true
	case "<=":
		return func(a, b int) any { return a <= b }, true
	case ">":
		return func(a, b int) any { return a > b }, true
	case ">=":
		return func(a, b int) any { return a >= b }, true
	case "==":
		return func(a, b int) any { return a == b }, true
	case "!=":
		return func(a, b int) any { return a != b }, true
	}
	return nil, false
}

// isInt reports whether e is statically an int: an int literal, an int
// slot, an arithmetic or bitwise operator on two ints, a negation or
// complement of one, an element of an int list, or len/byteLen.
func (c *procCompiler) isInt(e *ir.Expr) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case "lit":
		return e.VType == "int"
	case "ref":
		slot, ok := c.resolve(e.Name)
		return ok && c.ints[slot]
	case "bin":
		_, arith := intArith(e.Op)
		return arith && c.isInt(e.L) && c.isInt(e.R)
	case "un":
		return (e.Op == "-" || e.Op == "~") && c.isInt(e.X)
	case "index":
		return c.isIntList(e.Obj) && c.isInt(e.Key)
	case "get":
		// an int field of a value of a declared struct type
		typ, list, isMap := c.structFieldType(c.structTypeOf(e.Obj), e.Field)
		return typ == "int" && !list && !isMap
	case "call":
		if c.s.byProc[e.Name] != nil {
			return false
		}
		switch e.Name {
		case "len", "byteLen", "indexOf", "uintLE", "bytesCmp", "bytesCmpRange", "crc32":
			return true
		}
	}
	return false
}

// ── untyped arithmetic trees ────────────────────────────────────────────

// num is one node's value in an arithmetic tree: an int, a float, or
// (k == numBoxed) any other value, which the node hands to applyBin.
type num struct {
	k uint8
	i int
	f float64
	v any
}

const (
	numInt uint8 = iota
	numFloat
	numBoxed
)

func numOf(v any) num {
	switch t := v.(type) {
	case int:
		return num{k: numInt, i: t}
	case float64:
		return num{k: numFloat, f: t}
	}
	return num{k: numBoxed, v: v}
}

func (n num) box() any {
	switch n.k {
	case numInt:
		return boxInt(n.i)
	case numFloat:
		return n.f
	}
	return n.v
}

type nexpr func(*pfr) (num, error)

func numArithOp(op string) bool {
	return op == "+" || op == "-" || op == "*" || op == "/" || op == "%"
}

func isArithNode(e *ir.Expr) bool {
	return e != nil && ((e.Kind == "bin" && numArithOp(e.Op)) || (e.Kind == "un" && e.Op == "-"))
}

// numBin is applyBin(op, a, b) for two numbers: float arithmetic when
// either is a float (IEEE, no zero guard), else int arithmetic with its
// division-by-zero 0; `%` is always int.
func numBin(op string, a, b num) num {
	if op != "%" && (a.k == numFloat || b.k == numFloat) {
		x, y := a.f, b.f
		if a.k == numInt {
			x = float64(a.i)
		}
		if b.k == numInt {
			y = float64(b.i)
		}
		switch op {
		case "+":
			return num{k: numFloat, f: x + y}
		case "-":
			return num{k: numFloat, f: x - y}
		case "*":
			return num{k: numFloat, f: x * y}
		default:
			return num{k: numFloat, f: x / y}
		}
	}
	x, y := a.i, b.i
	if a.k == numFloat {
		x = int(a.f)
	}
	if b.k == numFloat {
		y = int(b.f)
	}
	switch op {
	case "+":
		return num{i: x + y}
	case "-":
		return num{i: x - y}
	case "*":
		return num{i: x * y}
	case "/":
		if y == 0 {
			return num{}
		}
		return num{i: x / y}
	default:
		if y == 0 {
			return num{}
		}
		return num{i: x % y}
	}
}

// numExpr compiles an arithmetic tree to tagged-number evaluation; a
// non-arithmetic node is a leaf evaluated as a value.
func (c *procCompiler) numExpr(e *ir.Expr) nexpr {
	if e != nil && c.isInt(e) {
		ie := c.intExpr(e)
		return func(fr *pfr) (num, error) {
			n, err := ie(fr)
			return num{i: n}, err
		}
	}
	if e != nil && c.isFloat(e) && c.floatNative(e) {
		fe := c.floatExpr(e)
		return func(fr *pfr) (num, error) {
			f, err := fe(fr)
			return num{k: numFloat, f: f}, err
		}
	}
	if e != nil && e.Kind == "bin" && numArithOp(e.Op) {
		l, r := c.numExpr(e.L), c.numExpr(e.R)
		op := e.Op
		return func(fr *pfr) (num, error) {
			a, err := l(fr)
			if err != nil {
				return num{}, err
			}
			b, err := r(fr)
			if err != nil {
				return num{}, err
			}
			if a.k == numBoxed || b.k == numBoxed {
				return numOf(applyBin(op, a.box(), b.box())), nil
			}
			return numBin(op, a, b), nil
		}
	}
	if e != nil && e.Kind == "un" && e.Op == "-" {
		x := c.numExpr(e.X)
		return func(fr *pfr) (num, error) {
			n, err := x(fr)
			if err != nil {
				return num{}, err
			}
			switch n.k {
			case numInt:
				return num{i: -n.i}, nil
			case numFloat:
				return num{k: numFloat, f: -n.f}, nil
			}
			return numOf(negate(n.v)), nil
		}
	}
	g := c.use(e)
	return func(fr *pfr) (num, error) {
		v, err := g(fr)
		if err != nil {
			return num{}, err
		}
		return numOf(v), nil
	}
}

// ── native float evaluation ─────────────────────────────────────────────

type fexpr func(*pfr) (float64, error)

// floatBuiltins are the builtins a float argument gives a float result
// (callBuiltin's own cases: each is the math function of toFloat(x)).
var floatBuiltins = map[string]func(float64) float64{
	"sqrt": math.Sqrt, "exp": math.Exp, "ln": math.Log, "sin": math.Sin, "cos": math.Cos,
}

// isFloat reports whether e statically evaluates to a float. Mixed
// int/float arithmetic is refused at compile time in a proc body, so a
// tree of float operands is float throughout.
func (c *procCompiler) isFloat(e *ir.Expr) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case "lit":
		return e.VType == "float"
	case "ref":
		slot, ok := c.resolve(e.Name)
		return ok && c.floats[slot]
	case "bin":
		_, arith := floatArith(e.Op)
		return arith && c.isFloat(e.L) && c.isFloat(e.R)
	case "un":
		return e.Op == "-" && c.isFloat(e.X)
	case "index":
		return c.isFloatList(e.Obj) && c.isInt(e.Key)
	case "get":
		typ, list, isMap := c.structFieldType(c.structTypeOf(e.Obj), e.Field)
		return typ == "float" && !list && !isMap
	case "call":
		if p := c.s.byProc[e.Name]; p != nil {
			return p.Ret == "float" && !p.RetList && !p.RetMap
		}
		if _, ok := floatBuiltins[e.Name]; ok && len(e.Args) == 1 {
			return true
		}
		if e.Name == "toFloat" && len(e.Args) == 1 {
			return true
		}
		if e.Name == "abs" && len(e.Args) == 1 {
			return c.isFloat(e.Args[0])
		}
	}
	return false
}

// isFloatList reports whether e is statically a list of floats.
func (c *procCompiler) isFloatList(e *ir.Expr) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case "ref":
		slot, ok := c.resolve(e.Name)
		return ok && c.floatLists[slot]
	case "get":
		typ, list, isMap := c.structFieldType(c.structTypeOf(e.Obj), e.Field)
		return typ == "float" && list && !isMap
	case "call":
		if p := c.s.byProc[e.Name]; p != nil {
			return p.Ret == "float" && p.RetList && !p.RetMap
		}
	}
	return false
}

// floatArith is applyBin's arithmetic on two floats.
func floatArith(op string) (func(a, b float64) float64, bool) {
	switch op {
	case "+":
		return func(a, b float64) float64 { return a + b }, true
	case "-":
		return func(a, b float64) float64 { return a - b }, true
	case "*":
		return func(a, b float64) float64 { return a * b }, true
	case "/":
		// IEEE 754, as applyBin's float "/" (no zero guard)
		return func(a, b float64) float64 { return a / b }, true
	}
	return nil, false
}

// floatCmp is applyBin's comparison of two floats (equal's float branch
// for == and !=).
func floatCmp(op string) (func(a, b float64) any, bool) {
	switch op {
	case "<":
		return func(a, b float64) any { return a < b }, true
	case "<=":
		return func(a, b float64) any { return a <= b }, true
	case ">":
		return func(a, b float64) any { return a > b }, true
	case ">=":
		return func(a, b float64) any { return a >= b }, true
	case "==":
		return func(a, b float64) any { return a == b }, true
	case "!=":
		return func(a, b float64) any { return a != b }, true
	}
	return nil, false
}

// floatNative reports whether floatExpr has a native arm for e itself —
// an operator, or a float builtin over a float (toFloat over an int) —
// rather than evaluating it as a value (the fallback compileExpr must not
// be sent back into, or it would recurse).
func (c *procCompiler) floatNative(e *ir.Expr) bool {
	switch e.Kind {
	case "bin":
		_, ok := floatArith(e.Op)
		return ok
	case "un":
		return e.Op == "-"
	case "call":
		if c.s.byProc[e.Name] != nil || len(e.Args) != 1 {
			return false
		}
		if _, ok := floatBuiltins[e.Name]; ok {
			return c.isFloat(e.Args[0])
		}
		switch e.Name {
		case "abs":
			return c.isFloat(e.Args[0])
		case "toFloat":
			return c.isInt(e.Args[0])
		}
	}
	return false
}

// floatExpr compiles a float-valued e to native float64 evaluation.
func (c *procCompiler) floatExpr(e *ir.Expr) fexpr {
	switch e.Kind {
	case "lit":
		f := toFloat(litValue(e))
		return func(*pfr) (float64, error) { return f, nil }
	case "ref":
		slot, _ := c.lookup(e.Name)
		return func(fr *pfr) (float64, error) { return toFloat(fr.slots[slot]), nil }
	case "bin":
		if fn, ok := floatArith(e.Op); ok {
			l, r := c.floatExpr(e.L), c.floatExpr(e.R)
			return func(fr *pfr) (float64, error) {
				a, err := l(fr)
				if err != nil {
					return 0, err
				}
				b, err := r(fr)
				if err != nil {
					return 0, err
				}
				return fn(a, b), nil
			}
		}
	case "un":
		if e.Op == "-" {
			x := c.floatExpr(e.X)
			return func(fr *pfr) (float64, error) {
				v, err := x(fr)
				return -v, err
			}
		}
	case "index":
		obj, key := c.use(e.Obj), c.intExpr(e.Key)
		return func(fr *pfr) (float64, error) {
			o, err := obj(fr)
			if err != nil {
				return 0, err
			}
			idx, err := key(fr)
			if err != nil {
				return 0, err
			}
			coll, ok := o.([]any)
			if !ok {
				return 0, fmt.Errorf("cannot index a value that is not an array or map")
			}
			if idx < 0 || idx >= len(coll) {
				return 0, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
			}
			return toFloat(coll[idx]), nil
		}
	case "call":
		if c.s.byProc[e.Name] == nil && len(e.Args) == 1 {
			if fn, ok := floatBuiltins[e.Name]; ok && c.isFloat(e.Args[0]) {
				x := c.floatExpr(e.Args[0])
				return func(fr *pfr) (float64, error) {
					v, err := x(fr)
					if err != nil {
						return 0, err
					}
					return fn(v), nil
				}
			}
			if e.Name == "abs" && c.isFloat(e.Args[0]) {
				x := c.floatExpr(e.Args[0])
				return func(fr *pfr) (float64, error) {
					v, err := x(fr)
					return math.Abs(v), err
				}
			}
			if e.Name == "toFloat" && c.isInt(e.Args[0]) {
				x := c.intExpr(e.Args[0])
				return func(fr *pfr) (float64, error) {
					n, err := x(fr)
					return float64(n), err
				}
			}
		}
	}
	// anything else: evaluated as a value, read as a float
	g := c.use(e)
	return func(fr *pfr) (float64, error) {
		v, err := g(fr)
		if err != nil {
			return 0, err
		}
		return toFloat(v), nil
	}
}

// isStructType reports whether name is a declared proc struct type.
func (c *procCompiler) isStructType(name string) bool {
	for _, st := range c.s.ir.Structs {
		if st.Name == name {
			return true
		}
	}
	return false
}

// structFieldType is field's declared type in struct type typ ("" when
// typ is no declared struct or has no such field), with whether it is a
// list and whether a map.
func (c *procCompiler) structFieldType(typ, field string) (string, bool, bool) {
	if typ == "" {
		return "", false, false
	}
	for _, st := range c.s.ir.Structs {
		if st.Name == typ {
			for _, f := range st.Fields {
				if f.Name == field {
					if f.Depth > 1 {
						// a nested list field: its elements are lists
						return "", f.List, f.Map
					}
					return f.Type, f.List, f.Map
				}
			}
		}
	}
	return "", false, false
}

// structTypeOf is the declared struct type e statically evaluates to, ""
// when it is not one (or cannot be known).
func (c *procCompiler) structTypeOf(e *ir.Expr) string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case "ref":
		if slot, ok := c.resolve(e.Name); ok {
			return c.structTypes[slot]
		}
	case "struct":
		if c.isStructType(e.Name) {
			return e.Name
		}
	case "get":
		typ, list, isMap := c.structFieldType(c.structTypeOf(e.Obj), e.Field)
		if !list && !isMap && c.isStructType(typ) {
			return typ
		}
	case "call":
		if p := c.s.byProc[e.Name]; p != nil && !p.RetList && !p.RetMap && c.isStructType(p.Ret) {
			return p.Ret
		}
	}
	return ""
}

// isIntList reports whether e is statically a list of ints.
func (c *procCompiler) isIntList(e *ir.Expr) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case "ref":
		slot, ok := c.resolve(e.Name)
		return ok && c.intLists[slot]
	case "call":
		if c.s.byProc[e.Name] != nil {
			return false
		}
		switch e.Name {
		case "bytes", "textToBytes", "aesGcmSeal", "aesGcmOpen", "fromHex", "bytesPut":
			return true
		case "append":
			return len(e.Args) == 2 && c.isIntList(e.Args[0]) && c.isInt(e.Args[1])
		}
	case "list":
		if len(e.Args) == 0 {
			return false
		}
		for _, a := range e.Args {
			if !c.isInt(a) {
				return false
			}
		}
		return true
	}
	return false
}

// asInt reads an int-typed value: an int as is, anything else (a
// wire-decoded number) through toInt.
func asInt(v any) int {
	if n, ok := v.(int); ok {
		return n
	}
	return toInt(v)
}

// intExpr compiles an expression isInt accepts to native int evaluation.
func (c *procCompiler) intExpr(e *ir.Expr) iexpr {
	switch e.Kind {
	case "lit":
		n := asInt(litValue(e))
		return func(*pfr) (int, error) { return n, nil }
	case "ref":
		slot, _ := c.lookup(e.Name)
		if c.ints[slot] {
			return func(fr *pfr) (int, error) { return fr.ints[slot], nil }
		}
		return func(fr *pfr) (int, error) { return asInt(fr.slots[slot]), nil }
	case "bin":
		fn, _ := intArith(e.Op)
		l, r := c.intExpr(e.L), c.intExpr(e.R)
		if e.R.Kind == "lit" {
			k := asInt(litValue(e.R))
			return func(fr *pfr) (int, error) {
				a, err := l(fr)
				if err != nil {
					return 0, err
				}
				return fn(a, k), nil
			}
		}
		return func(fr *pfr) (int, error) {
			a, err := l(fr)
			if err != nil {
				return 0, err
			}
			b, err := r(fr)
			if err != nil {
				return 0, err
			}
			return fn(a, b), nil
		}
	case "un":
		x := c.intExpr(e.X)
		if e.Op == "~" {
			return func(fr *pfr) (int, error) {
				n, err := x(fr)
				return ^n, err
			}
		}
		return func(fr *pfr) (int, error) {
			n, err := x(fr)
			return -n, err
		}
	case "index":
		key := c.intExpr(e.Key)
		var slot = -1
		var obj cexpr
		if e.Obj.Kind == "ref" {
			slot, _ = c.lookup(e.Obj.Name)
		} else {
			obj = c.use(e.Obj)
		}
		return func(fr *pfr) (int, error) {
			var o any
			if slot >= 0 {
				o = fr.slots[slot]
			} else {
				v, err := obj(fr)
				if err != nil {
					return 0, err
				}
				o = v
			}
			idx, err := key(fr)
			if err != nil {
				return 0, err
			}
			switch coll := o.(type) {
			case bytesVal:
				if idx < 0 || idx >= len(coll) {
					return 0, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
				}
				return int(coll[idx]), nil
			case []any:
				if idx < 0 || idx >= len(coll) {
					return 0, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
				}
				return asInt(coll[idx]), nil
			}
			return 0, fmt.Errorf("cannot index a value that is not an array or map")
		}
	}
	// An int field of a struct value: read by its position (cached against
	// the layout last read here, as the boxed read's is), with no boxing.
	if e.Kind == "get" {
		obj := c.getObj(e)
		field := e.Field
		var cache atomic.Pointer[fieldPos]
		return func(fr *pfr) (int, error) {
			o, err := obj(fr)
			if err != nil {
				return 0, err
			}
			sv, ok := o.(structVal)
			if !ok {
				return 0, fmt.Errorf("cannot read field %q of a value that is not a struct", field)
			}
			if fp := cache.Load(); fp != nil && fp.lay == sv.lay {
				return asInt(sv.vals[fp.i]), nil
			}
			i, has := sv.lay.index[field]
			if !has {
				return 0, nil
			}
			cache.Store(&fieldPos{lay: sv.lay, i: i})
			return asInt(sv.vals[i]), nil
		}
	}
	// uintLE / bytesCmpRange: a page's offsets and a key compared in place
	// (the storage engine's inner loops), computed on native ints.
	if e.Kind == "call" && e.Name == "uintLE" && len(e.Args) == 3 && c.s.byProc["uintLE"] == nil && c.isInt(e.Args[1]) && c.isInt(e.Args[2]) {
		buf, at, n := c.use(e.Args[0]), c.intExpr(e.Args[1]), c.intExpr(e.Args[2])
		return func(fr *pfr) (int, error) {
			b, err := buf(fr)
			if err != nil {
				return 0, err
			}
			a, err := at(fr)
			if err != nil {
				return 0, err
			}
			w, err := n(fr)
			if err != nil {
				return 0, err
			}
			return uintLEInt(b, a, w)
		}
	}
	if e.Kind == "call" && e.Name == "bytesCmpRange" && len(e.Args) == 6 && c.s.byProc["bytesCmpRange"] == nil &&
		c.isInt(e.Args[1]) && c.isInt(e.Args[2]) && c.isInt(e.Args[4]) && c.isInt(e.Args[5]) {
		a, af, at := c.use(e.Args[0]), c.intExpr(e.Args[1]), c.intExpr(e.Args[2])
		b, bf, bt := c.use(e.Args[3]), c.intExpr(e.Args[4]), c.intExpr(e.Args[5])
		return func(fr *pfr) (int, error) {
			av, err := a(fr)
			if err != nil {
				return 0, err
			}
			x0, err := af(fr)
			if err != nil {
				return 0, err
			}
			x1, err := at(fr)
			if err != nil {
				return 0, err
			}
			bv, err := b(fr)
			if err != nil {
				return 0, err
			}
			y0, err := bf(fr)
			if err != nil {
				return 0, err
			}
			y1, err := bt(fr)
			if err != nil {
				return 0, err
			}
			return bytesCmpRangeInt(av, x0, x1, bv, y0, y1)
		}
	}
	// len / indexOf: computed natively (the builtin's own cases, without
	// callBuiltin's dispatch or a boxed result); byteLen through the builtin.
	if e.Kind == "call" && e.Name == "len" && len(e.Args) == 1 && c.s.byProc["len"] == nil {
		x := c.use(e.Args[0])
		return func(fr *pfr) (int, error) {
			v, err := x(fr)
			if err != nil {
				return 0, err
			}
			switch t := v.(type) {
			case []any:
				return len(t), nil
			case bytesVal:
				return len(t), nil
			case map[any]any:
				return len(t), nil
			}
			return runeLen(toStr(v)), nil
		}
	}
	if e.Kind == "call" && e.Name == "indexOf" && len(e.Args) == 3 && c.s.byProc["indexOf"] == nil && c.isInt(e.Args[2]) {
		x, sub, from := c.use(e.Args[0]), c.use(e.Args[1]), c.intExpr(e.Args[2])
		return func(fr *pfr) (int, error) {
			sv, err := x(fr)
			if err != nil {
				return 0, err
			}
			bv, err := sub(fr)
			if err != nil {
				return 0, err
			}
			f, err := from(fr)
			if err != nil {
				return 0, err
			}
			if xs, ok := sv.([]any); ok {
				return listIndexOf(xs, bv, f), nil
			}
			if b, ok := sv.(bytesVal); ok {
				return bytesIndexOf(b, bv, f), nil
			}
			return runeIndexOf(toStr(sv), toStr(bv), f), nil
		}
	}
	g := c.use(e)
	return func(fr *pfr) (int, error) {
		v, err := g(fr)
		if err != nil {
			return 0, err
		}
		return asInt(v), nil
	}
}

// intOp is a binary operator's result on two ints, exactly applyBin's on two
// int operands; ok false for an operator it does not cover.
func intOp(op string) (func(a, b int) any, bool) {
	switch op {
	case "+":
		return func(a, b int) any { return a + b }, true
	case "-":
		return func(a, b int) any { return a - b }, true
	case "*":
		return func(a, b int) any { return a * b }, true
	case "/":
		return func(a, b int) any {
			if b == 0 {
				return 0
			}
			return a / b
		}, true
	case "%":
		return func(a, b int) any {
			if b == 0 {
				return 0
			}
			return a % b
		}, true
	case "<":
		return func(a, b int) any { return a < b }, true
	case "<=":
		return func(a, b int) any { return a <= b }, true
	case ">":
		return func(a, b int) any { return a > b }, true
	case ">=":
		return func(a, b int) any { return a >= b }, true
	case "==":
		return func(a, b int) any { return a == b }, true
	case "!=":
		return func(a, b int) any { return a != b }, true
	case "&":
		return func(a, b int) any { return a & b }, true
	case "|":
		return func(a, b int) any { return a | b }, true
	case "^":
		return func(a, b int) any { return a ^ b }, true
	case "<<":
		return func(a, b int) any { return shiftLeft(a, b) }, true
	case ">>":
		return func(a, b int) any { return shiftRight(a, b) }, true
	}
	return nil, false
}

func (c *procCompiler) bin(e *ir.Expr) cexpr {
	// No operator returns an operand itself (`+` on two lists builds a new
	// one), so operands are only inspected. Each operand is compiled exactly
	// once — compiling it twice at every level would make compile time
	// exponential in an expression's depth (a long `a + b + c + …` chain).
	op := e.Op
	if op == "+" {
		if cat := c.textConcat(e); cat != nil {
			return cat
		}
	}
	switch op {
	case "&&":
		l, r := c.use(e.L), c.use(e.R)
		return func(fr *pfr) (any, error) {
			lv, err := l(fr)
			if err != nil {
				return nil, err
			}
			if !truthy(lv) {
				return false, nil
			}
			rv, err := r(fr)
			if err != nil {
				return nil, err
			}
			return truthy(rv), nil
		}
	case "||":
		l, r := c.use(e.L), c.use(e.R)
		return func(fr *pfr) (any, error) {
			lv, err := l(fr)
			if err != nil {
				return nil, err
			}
			if truthy(lv) {
				return true, nil
			}
			rv, err := r(fr)
			if err != nil {
				return nil, err
			}
			return truthy(rv), nil
		}
	}
	// An arithmetic tree over operands of unknown type (another operator
	// among its operands): evaluated as tagged numbers, boxing only the
	// root (numExpr) — each node exactly applyBin's rule.
	if numArithOp(op) && (isArithNode(e.L) || isArithNode(e.R)) {
		ne := c.numExpr(e)
		return func(fr *pfr) (any, error) {
			n, err := ne(fr)
			if err != nil {
				return nil, err
			}
			return n.box(), nil
		}
	}
	if op == "in" {
		if set := constSet(e.R); set != nil {
			// `x in [literals]`: a hash lookup when x is the elements' own
			// kind (where equal() is plain equality); any other x takes
			// applyBin's scan, so mixed-kind matches (`1 in ["1"]`) stay.
			lo := c.operand(e.L)
			return func(fr *pfr) (any, error) {
				lv, err := lo.get(fr)
				if err != nil {
					return nil, err
				}
				if hit, ok := set.lookup(lv); ok {
					return hit, nil
				}
				return applyBin(op, lv, set.list), nil
			}
		}
	}
	fast, hasFast := intOp(op)
	sfast, hasSfast := strOp(op)
	lo, ro := c.operand(e.L), c.operand(e.R)
	return func(fr *pfr) (any, error) {
		lv, err := lo.get(fr)
		if err != nil {
			return nil, err
		}
		rv, err := ro.get(fr)
		if err != nil {
			return nil, err
		}
		if hasFast {
			if a, ok := lv.(int); ok {
				if b, ok := rv.(int); ok {
					return fast(a, b), nil
				}
			}
		}
		if hasSfast {
			if a, ok := lv.(string); ok {
				if b, ok := rv.(string); ok {
					return sfast(a, b), nil
				}
			}
		}
		return applyBin(op, lv, rv), nil
	}
}

// textConcat compiles a `+` chain whose leftmost leaf is a text literal
// (`"" + n`, `"line " + n + ": " + msg`) as one concatenation: every step
// of the chain is applyBin's text + toStr(right), so the result is the
// parts' texts joined, built once — not a new string per `+`, each
// copying everything before it. nil when the chain is not that shape.
func (c *procCompiler) textConcat(e *ir.Expr) cexpr {
	var leaves []*ir.Expr
	n := e
	for n != nil && n.Kind == "bin" && n.Op == "+" {
		leaves = append(leaves, n.R)
		n = n.L
	}
	if n == nil || n.Kind != "lit" || n.VType != "text" || len(leaves) < 2 {
		return nil
	}
	head := toStr(litValue(n))
	// Compiled leftmost first: lookup numbers each occurrence of a local
	// in compile order for the move analysis, which must be source order.
	parts := make([]cexpr, len(leaves))
	for i := range leaves {
		parts[i] = c.use(leaves[len(leaves)-1-i])
	}
	return func(fr *pfr) (any, error) {
		var buf [8]string
		var texts []string
		if len(parts) <= len(buf) {
			texts = buf[:len(parts)]
		} else {
			texts = make([]string, len(parts))
		}
		total := len(head)
		for i, p := range parts {
			v, err := p(fr)
			if err != nil {
				return nil, err
			}
			texts[i] = toStr(v)
			total += len(texts[i])
		}
		var b strings.Builder
		b.Grow(total)
		b.WriteString(head)
		for _, t := range texts {
			b.WriteString(t)
		}
		return b.String(), nil
	}
}

// strOp is applyBin's result for op on two texts, chosen once per operator
// so the common text comparison skips applyBin's dispatch and equal's
// conversions; false for an operator applyBin does not define on texts.
func strOp(op string) (func(a, b string) any, bool) {
	switch op {
	case "==":
		return func(a, b string) any { return a == b }, true
	case "!=":
		return func(a, b string) any { return a != b }, true
	case "<":
		return func(a, b string) any { return a < b }, true
	case "<=":
		return func(a, b string) any { return a <= b }, true
	case ">":
		return func(a, b string) any { return a > b }, true
	case ">=":
		return func(a, b string) any { return a >= b }, true
	case "+":
		return func(a, b string) any { return a + b }, true
	}
	return nil, false
}

// growOwnedList appends v to the []any held in *slot, whose spare capacity
// has room for it, by rewriting the slice header inside the interface's own
// box rather than boxing a new header: a loop of `out = append(out, x)`
// allocates only when the backing array grows. Sound exactly when the slot
// owns the list (fr.own): an owned value — and so the box it arrived in —
// is referenced from nowhere else, so no other holder can observe the
// header change; it is the same exclusivity that already lets append write
// into the backing array's spare capacity.
//
// When the backing array is full it grows (listGrow) and the new header
// is still written into the same box — except the shared `[]` literal's
// box (emptyList), which is every empty list's and never written: a list
// growing out of it gets a box of its own.
func growOwnedList(slot *any, v any) {
	e := (*[2]unsafe.Pointer)(unsafe.Pointer(slot))
	if e[0] != listTypeWord || e[1] == emptyListBox {
		*slot = listGrow((*slot).([]any), v)
		return
	}
	hdr := (*[]any)(e[1])
	*hdr = listGrow(*hdr, v)
}

// listGrow is append(xs, v), except that a list growing out of no
// capacity starts with room for four: the lists a proc builds are mostly
// short, and append's own 1, 2, 4 steps were an allocation each.
func listGrow(xs []any, v any) []any {
	if cap(xs) == 0 {
		out := make([]any, 1, 4)
		out[0] = v
		return out
	}
	return append(xs, v)
}

// emptyListBox is the data word of emptyList's interface — the box
// growOwnedList must never write into.
var emptyListBox = (*[2]unsafe.Pointer)(unsafe.Pointer(&emptyList))[1]

// listTypeWord is the type word of an interface holding a []any.
var listTypeWord = func() unsafe.Pointer {
	var probe any = []any{}
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&probe))[0]
}()

// emptyList is the value of every `[]` literal (see compileExpr's "list").
var emptyList any = []any{}

// constList is e's value when e is a list literal whose elements are all
// literals (or such lists themselves), built once at compile time.
func constList(e *ir.Expr) ([]any, bool) {
	if e == nil || e.Kind != "list" {
		return nil, false
	}
	out := make([]any, len(e.Args))
	for i, a := range e.Args {
		switch {
		case a != nil && a.Kind == "lit":
			out[i] = litValue(a)
		case a != nil && a.Kind == "list":
			inner, ok := constList(a)
			if !ok {
				return nil, false
			}
			out[i] = inner
		default:
			return nil, false
		}
	}
	return out, true
}

// litSet is a literal list compiled for `in`: the list itself (applyBin's
// operand when the lookup cannot decide) and, when every element is a text
// or every element is an int, the set of them.
type litSet struct {
	list []any
	strs map[string]struct{}
	ints map[int]struct{}
}

func constSet(e *ir.Expr) *litSet {
	list, ok := constList(e)
	if !ok {
		return nil
	}
	set := &litSet{list: list}
	allStr, allInt := true, true
	for _, v := range list {
		if _, ok := v.(string); !ok {
			allStr = false
		}
		if _, ok := v.(int); !ok {
			allInt = false
		}
	}
	if allStr {
		set.strs = make(map[string]struct{}, len(list))
		for _, v := range list {
			set.strs[v.(string)] = struct{}{}
		}
	}
	if allInt {
		set.ints = make(map[int]struct{}, len(list))
		for _, v := range list {
			set.ints[v.(int)] = struct{}{}
		}
	}
	return set
}

// lookup answers `v in set` when v is the elements' own kind; ok is false
// when only applyBin's equal()-by-equal() scan can answer.
func (ls *litSet) lookup(v any) (hit, ok bool) {
	switch t := v.(type) {
	case string:
		if ls.strs != nil {
			_, hit = ls.strs[t]
			return hit, true
		}
	case int:
		if ls.ints != nil {
			_, hit = ls.ints[t]
			return hit, true
		}
	}
	return false, false
}

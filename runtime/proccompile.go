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
	"fmt"
	"net/http"
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
	body     []cstmt
	frames   sync.Pool // idle *pfr of this code's size, reused across calls
}

// procCompiler resolves names while a body is compiled: scopes is the stack
// of lexical blocks, each mapping a name to its slot.
type procCompiler struct {
	s      *Server
	scopes []map[string]int
	next   int
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
	if ok && c.lastPos != nil {
		c.pos++
		c.lastPos[slot] = c.pos
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
	if e != nil && e.Kind == "ref" {
		if slot, ok := c.lookup(e.Name); ok && c.ints[slot] {
			return func(fr *pfr) (any, bool, error) { return boxInt(fr.ints[slot]), false, nil }
		} else if ok {
			move := new(bool)
			c.moves = append(c.moves, moveCand{slot: slot, pos: c.pos, ok: move, self: slot == target})
			return func(fr *pfr) (any, bool, error) {
				v := fr.slots[slot]
				if *move {
					owned := fr.own[slot]
					fr.slots[slot], fr.own[slot] = nil, false
					return v, owned, nil
				}
				fr.own[slot] = false
				return v, false, nil
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
func (s *Server) compileProc(params []ir.Param, body []ir.Stmt) *procCode {
	c := &procCompiler{s: s, ints: map[int]bool{}, intLists: map[int]bool{}, lastPos: map[int]int{}, declPos: map[int]int{}}
	c.push()
	pc := &procCode{}
	for _, p := range params {
		slot := c.declare(p.Name)
		if p.Type == "int" {
			if p.List {
				c.intLists[slot] = true
			} else {
				c.ints[slot] = true
			}
		}
		pc.params = append(pc.params, slot)
		pc.intParam = append(pc.intParam, c.ints[slot])
	}
	pc.body = c.stmts(body)
	c.pop()
	c.decideMoves()
	pc.nslots = c.next
	return pc
}

// codeFor returns key's compiled code, compiling it on first use.
func (s *Server) codeFor(key any, params []ir.Param, body []ir.Stmt) *procCode {
	if v, ok := s.procCode.Load(key); ok {
		return v.(*procCode)
	}
	pc := s.compileProc(params, body)
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
}

func (c *procCompiler) site(name string) *procSite {
	if p := c.s.byProc[name]; p != nil {
		return &procSite{s: c.s, p: p}
	}
	return nil
}

func (ps *procSite) call(args []any, owned uint64) (any, bool, error) {
	pc := ps.code.Load()
	if pc == nil {
		pc = ps.s.codeFor(ps.p, ps.p.Params, ps.p.Body)
		ps.code.Store(pc)
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
		val := c.expr(st.Value)
		fresh := c.fresh(st.Value)
		slot := c.declare(st.Target)
		c.ints[slot], c.intLists[slot] = isInt, isIntList
		return func(fr *pfr) (ctlSignal, error) {
			v, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.set(slot, v, fresh)
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
		// `x = x + e` on a text local appends in place (see appendText).
		if v != nil && v.Kind == "bin" && v.Op == "+" && v.L != nil && v.L.Kind == "ref" && v.L.Name == st.Target {
			add := c.expr(v.R)
			whole := c.expr(st.Value)
			fresh := c.fresh(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if cs, isText := fr.slots[slot].(string); isText {
					rv, err := add(fr)
					if err != nil {
						return ctlSignal{}, err
					}
					fr.appendText(slot, cs, toStr(rv))
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
					fr.slots[slot] = append(arr, ev)
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
		val := c.expr(st.Value)
		fresh := c.fresh(st.Value)
		return func(fr *pfr) (ctlSignal, error) {
			nv, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.set(slot, nv, fresh)
			return ctlSignal{}, nil
		}
	case "indexset":
		slot := c.target(st.Target)
		name, isBytes := st.Target, st.Bytes
		if c.intLists[slot] && c.isInt(st.Key) && c.isInt(st.Value) {
			// An int written into an int list at an int index: all native.
			key, val := c.intExpr(st.Key), c.intExpr(st.Value)
			return func(fr *pfr) (ctlSignal, error) {
				if _, ok := fr.slots[slot].([]any); !ok {
					return ctlSignal{}, fmt.Errorf("%q is not an array or map", name)
				}
				coll := fr.mutable(slot).([]any)
				idx, err := key(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if idx < 0 || idx >= len(coll) {
					return ctlSignal{}, fmt.Errorf("array index %d out of bounds (length %d) assigning to %q", idx, len(coll), name)
				}
				n, err := val(fr)
				if err != nil {
					return ctlSignal{}, err
				}
				if isBytes && (n < 0 || n > 255) {
					return ctlSignal{}, fmt.Errorf("byte value %d out of range (must be 0-255) assigning to %q[%d]", n, name, idx)
				}
				coll[idx] = boxInt(n)
				return ctlSignal{}, nil
			}
		}
		key, val := c.use(st.Key), c.expr(st.Value)
		return func(fr *pfr) (ctlSignal, error) {
			switch fr.slots[slot].(type) {
			case []any, map[any]any:
			default:
				return ctlSignal{}, fmt.Errorf("%q is not an array or map", name)
			}
			switch coll := fr.mutable(slot).(type) {
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
		self := -1
		if st.Bind == "" && st.Target != "" {
			if slot, ok := c.resolve(st.Target); ok {
				self = slot
			}
		}
		from := len(c.moves)
		args := make([]argFn, len(st.Args))
		for i, a := range st.Args {
			args[i] = c.moveArg(a, self)
		}
		c.selfMoves(from)
		bind, into := -1, -1
		if st.Bind != "" {
			bind = c.declare(st.Bind)
			c.ints[bind] = ret == "int" && !retList
			c.intLists[bind] = ret == "int" && retList
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
				v := s.coerceRet(res, ret, retList)
				if bindInt {
					fr.ints[bind] = asInt(v)
				} else {
					fr.set(bind, v, owned || !sameList(v, res))
				}
			} else if into >= 0 {
				v := s.coerceRet(res, ret, retList)
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
		val := c.expr(st.Value)
		name, field := st.Target, st.Field
		return func(fr *pfr) (ctlSignal, error) {
			sv, ok := fr.slots[slot].(structVal)
			if !ok {
				return ctlSignal{}, fmt.Errorf("%q is not a struct", name)
			}
			i, has := sv.lay.index[field]
			if !has {
				return ctlSignal{}, fmt.Errorf("struct %q has no field %q", sv.lay.name, field)
			}
			v, err := val(fr)
			if err != nil {
				return ctlSignal{}, err
			}
			fr.mutable(slot).(structVal).vals[i] = v
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
				fr.set(bind, s.coerceRet(res, ret, retList), false)
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
	"len": true, "byteLen": true, "bytesToText": true, "join": true, "contains": true,
	"charAt": true, "writeFileAt": true, "writeBytes": true, "writeFile": true,
	"aesGcmSeal": true, "aesGcmOpen": true, "aesGcmAuthentic": true,
	"appendFile": true, "sha256Hex": true, "canonicalJson": true,
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
		case "append", "bytes", "textToBytes", "split", "readFileAt", "aesGcmSeal", "aesGcmOpen":
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
	switch e.Kind {
	case "lit":
		return constExpr(litValue(e))
	case "list":
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
		vals := c.exprs(e.Args)
		names, typ, omit, nulls := e.Fields, e.Name, e.Omit, e.Nulls
		if s.isWireType(typ) {
			// A wire DTO a proc builds for an action: the record shape every
			// other wire value has, so it encodes and reads the same.
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
		}
		// A struct value: each literal argument goes to its field's
		// position in the type's layout, resolved here once.
		lay := s.structLayout(typ, names)
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
			out := make([]any, width)
			for i, val := range vals {
				v, err := val(fr)
				if err != nil {
					return nil, err
				}
				out[pos[i]] = v
			}
			return structVal{lay: lay, vals: out}, nil
		}
	case "get":
		obj := c.use(e.Obj)
		field := e.Field
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
				return nil, fmt.Errorf("cannot read field %q of a value that is not a struct", field)
			}
			if fp := cache.Load(); fp != nil && fp.lay == sv.lay {
				return sv.vals[fp.i], nil
			}
			i, has := sv.lay.index[field]
			if !has {
				return nil, nil
			}
			cache.Store(&fieldPos{lay: sv.lay, i: i})
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
				args[i] = c.moveArg(a, -1)
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
	a, okA := v.([]any)
	b, okB := res.([]any)
	if okA && okB {
		return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
	}
	return !okA && !okB
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
	case "call":
		if c.s.byProc[e.Name] != nil {
			return false
		}
		return e.Name == "len" || e.Name == "byteLen"
	}
	return false
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
		case "bytes", "textToBytes", "aesGcmSeal", "aesGcmOpen":
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
			coll, ok := o.([]any)
			if !ok {
				return 0, fmt.Errorf("cannot index a value that is not an array or map")
			}
			if idx < 0 || idx >= len(coll) {
				return 0, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
			}
			return asInt(coll[idx]), nil
		}
	}
	// len / byteLen: the builtin, its result read as an int.
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
	fast, hasFast := intOp(op)
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
		return applyBin(op, lv, rv), nil
	}
}

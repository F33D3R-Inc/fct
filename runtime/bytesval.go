package runtime

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// bytesVal is a byte buffer as the proc engine holds it: a real []byte.
//
// To the language a byte buffer is an [int] whose elements are 0-255 —
// bytes(n), textToBytes, readFileAt, readBytes, aesGcmSeal/Open,
// sha256Bytes, randomBytes and their kin all produce one, and it passes
// wherever an [int] is accepted (a proc parameter, a struct field, `+`
// with a list, `==`, `in`, len, slice, append, an index read or write).
// It used to be the same []any every list is, one boxed interface value
// per byte: sixteen times the bytes of the buffer itself, a 256 KB
// allocation for every copy-on-write edit of a 16 KB page, and every
// element a pointer for the garbage collector to trace. This type keeps
// the language's value exactly — the same ownership and copy-on-write
// rules a slot applies to any list (proccompile.go's header) apply to it
// unchanged, and cloneArrayValue copies it the same way — while the
// evaluator reads and writes real bytes.
//
// The one place a []byte cannot stand in for an [int] is a write of a
// value outside 0-255 into a buffer the compiler types as a plain [int]
// (a parameter, say, which at runtime may be handed a byte buffer): the
// language allows it, so the buffer is promoted to a []any of ints at
// that write (promoteBytes), after which it is an ordinary list. A slot
// the compiler types as a byte buffer (Stmt.Bytes) refuses the write
// instead, exactly as before.
//
// A named type, not a bare []byte: toStr and equal already read a bare
// []byte as the text a database driver handed over, and a byte buffer is
// a list of numbers, not text.
type bytesVal []byte

// MarshalJSON writes the buffer as the JSON array of ints an [int] is —
// never as base64, which encoding/json would choose for a []byte.
func (b bytesVal) MarshalJSON() ([]byte, error) {
	if len(b) == 0 {
		return []byte("[]"), nil
	}
	out := make([]byte, 0, 4*len(b)+2)
	out = append(out, '[')
	for i, x := range b {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, itoa(int(x))...)
	}
	return append(out, ']'), nil
}

// promoteBytes is the []any of ints a byte buffer becomes when an element
// outside 0-255 is stored into it.
func promoteBytes(b bytesVal) []any {
	out := make([]any, len(b))
	for i, x := range b {
		out[i] = boxedInts[x]
	}
	return out
}

// bytesOf reads v as a byte buffer's bytes without copying: the buffer
// itself, or the bytes an []any of ints spells; ok false for anything
// else, and for a list holding a non-byte (which the caller reports).
func bytesOf(v any) ([]byte, bool) {
	switch t := v.(type) {
	case bytesVal:
		return t, true
	case []any:
		out := make([]byte, len(t))
		for i, x := range t {
			n := toInt(x)
			if n < 0 || n > 255 {
				return nil, false
			}
			out[i] = byte(n)
		}
		return out, true
	}
	return nil, false
}

// isByteInt reports whether v is an int a byte buffer can hold.
func isByteInt(v any) (byte, bool) {
	n, ok := v.(int)
	if !ok || n < 0 || n > 255 {
		return 0, false
	}
	return byte(n), true
}

// bytesElem is element i of a byte buffer as the language sees it: an int.
func bytesElem(b bytesVal, i int) any { return boxedInts[b[i]] }

// listLen is len() of either list representation, or -1 for a non-list.
func listLen(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case bytesVal:
		return len(t)
	}
	return -1
}

// listElems is a list's elements as []any — the list itself, or a byte
// buffer's ints (a copy) — for the few paths that walk a list generically.
func listElems(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case bytesVal:
		return promoteBytes(t), true
	}
	return nil, false
}

// ── the byte-level primitives a binary format is read with ─────────────

// bytesCmpBuiltin implements `bytesCmp(a, b) -> int`: the lexicographic
// order of two buffers, -1/0/1, a shorter prefix first — what a B+tree key
// comparison is.
func bytesCmpBuiltin(a, b any) (any, error) {
	x, ok := bytesOf(a)
	if !ok {
		return nil, fmt.Errorf("bytesCmp: the first argument is not a byte buffer")
	}
	y, ok := bytesOf(b)
	if !ok {
		return nil, fmt.Errorf("bytesCmp: the second argument is not a byte buffer")
	}
	return bytes.Compare(x, y), nil
}

// bytesCmpRangeBuiltin implements `bytesCmpRange(a, aFrom, aTo, b, bFrom,
// bTo) -> int`: bytesCmp over a[aFrom..aTo) and b[bFrom..bTo), copying
// neither — a key inside a page compared against a probe in place.
func bytesCmpRangeBuiltin(a any, aFrom, aTo int, b any, bFrom, bTo int) (any, error) {
	c, err := bytesCmpRangeInt(a, aFrom, aTo, b, bFrom, bTo)
	if err != nil {
		return nil, err
	}
	return cmpBoxed[c+1], nil
}

// cmpBoxed is -1, 0 and 1 boxed once: a comparison's result as a value
// allocates nothing (Go boxes only 0-255 for free, not -1).
var cmpBoxed = [3]any{-1, 0, 1}

// bytesCmpRangeInt is bytesCmpRange's result as an int, for the proc
// engine's native int path (proccompile.go's intExpr) and the builtin.
func bytesCmpRangeInt(a any, aFrom, aTo int, b any, bFrom, bTo int) (int, error) {
	x, ok := bytesOf(a)
	if !ok {
		return 0, fmt.Errorf("bytesCmpRange: the first argument is not a byte buffer")
	}
	y, ok := bytesOf(b)
	if !ok {
		return 0, fmt.Errorf("bytesCmpRange: the second argument is not a byte buffer")
	}
	if aFrom < 0 || aTo > len(x) || aFrom > aTo {
		return 0, fmt.Errorf("bytesCmpRange: range %d..%d is outside a buffer of %d bytes", aFrom, aTo, len(x))
	}
	if bFrom < 0 || bTo > len(y) || bFrom > bTo {
		return 0, fmt.Errorf("bytesCmpRange: range %d..%d is outside a buffer of %d bytes", bFrom, bTo, len(y))
	}
	return bytes.Compare(x[aFrom:aTo], y[bFrom:bTo]), nil
}

// uintLEBuiltin implements `uintLE(b, at, n) -> int`: the little-endian
// unsigned integer in b[at..at+n), n 1-8.
func uintLEBuiltin(b any, at, n int) (any, error) {
	v, err := uintLEInt(b, at, n)
	if err != nil {
		return nil, err
	}
	return boxInt(v), nil
}

// uintLEInt is uintLE's result as an int, for the proc engine's native int
// path (proccompile.go's intExpr) and the builtin.
func uintLEInt(b any, at, n int) (int, error) {
	x, ok := bytesOf(b)
	if !ok {
		return 0, fmt.Errorf("uintLE: not a byte buffer")
	}
	if n < 1 || n > 8 {
		return 0, fmt.Errorf("uintLE: width %d is not 1-8 bytes", n)
	}
	if at < 0 || at+n > len(x) {
		return 0, fmt.Errorf("uintLE: bytes %d..%d are outside a buffer of %d bytes", at, at+n, len(x))
	}
	var v uint64
	for i := n - 1; i >= 0; i-- {
		v = v<<8 | uint64(x[at+i])
	}
	return int(v), nil
}

// toHexBuiltin implements `toHex(b) -> text`: lowercase hex, two digits a byte.
func toHexBuiltin(b any) (any, error) {
	x, ok := bytesOf(b)
	if !ok {
		return nil, fmt.Errorf("toHex: not a byte buffer")
	}
	return hex.EncodeToString(x), nil
}

// fromHexBuiltin implements `fromHex(s) -> bytes`: toHex's inverse; text
// that is not hex decodes to an empty buffer.
func fromHexBuiltin(s string) (any, error) {
	out, err := hex.DecodeString(s)
	if err != nil {
		return bytesVal{}, nil
	}
	return bytesVal(out), nil
}

// bytesPutBuiltin implements `bytesPut(dst, at, src) -> [int]`: a copy of
// dst with src's bytes written over dst[at..at+len(src)] — dst's length and
// every other byte unchanged. A range outside dst, or a value that is not a
// byte, is an error. (The proc engine does `b = bytesPut(b, at, src)` in
// place when b's slot owns it: bytesPutInto.)
func bytesPutBuiltin(dst any, at int, src any) (any, error) {
	d, ok := bytesOf(dst)
	if !ok {
		return nil, fmt.Errorf("bytesPut: the destination is not a byte buffer")
	}
	out := make(bytesVal, len(d))
	copy(out, d)
	if err := bytesPutInto(out, at, src); err != nil {
		return nil, err
	}
	return out, nil
}

// bytesPutInto writes src over b[at..at+len(src)] in place.
func bytesPutInto(b bytesVal, at int, src any) error {
	s, ok := bytesOf(src)
	if !ok {
		return fmt.Errorf("bytesPut: the source is not a byte buffer")
	}
	if at < 0 || at+len(s) > len(b) {
		return fmt.Errorf("bytesPut: writing %d bytes at %d is outside a buffer of %d bytes", len(s), at, len(b))
	}
	copy(b[at:], s)
	return nil
}

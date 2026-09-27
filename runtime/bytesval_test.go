package runtime

import (
	"encoding/json"
	"testing"

	"facet/internal/compile"
)

// The byte buffer's language contract, over the native representation:
// value semantics through every binding, copy-on-write in place, promotion
// to a plain int list on a write a byte cannot hold, and every builtin that
// takes or gives a buffer agreeing with the [int] it stands for.
const bytesValApp = `app B:
    struct Holder:
        buf: [int]
        tag: text

    # value semantics: a copy bound before a write does not see it; the
    # original's ownership survives a read-only pass through a callee
    proc independent() -> text:
        let mut a = bytes(3)
        a[0] = 7
        let b = a
        a[1] = 9
        let n = do peek(a)
        a[2] = 5
        return "" + a + "|" + b + "|" + n
    proc peek(b: [int]) -> int:
        return b[0] + len(b)

    # a parameter typed [int] that holds a buffer takes any int: the buffer
    # promotes, and the caller's own copy is untouched
    proc promote(b0: [int]) -> [int]:
        let mut b = b0
        b[1] = 1000
        return b
    proc promoted() -> text:
        let mut a = bytes(2)
        a[0] = 1
        let p = do promote(a)
        return "" + a + "|" + p + "|" + len(p) + "|" + p[1]

    # append: in place when owned, a byte or a promotion
    proc grows() -> text:
        let mut a = bytes(0)
        a = append(a, 200)
        let alias = a
        a = append(a, 201)
        a = append(a, 300)
        return "" + a + "|" + alias

    # text round trips, slices, concatenation, comparison, membership
    proc mixed() -> text:
        let t = textToBytes("héllo")
        let back = bytesToText(t)
        let s = slice(t, 1, 3)
        let plain = [104, 195]
        let same = plain == slice(t, 0, 2)
        let joined = slice(t, 0, 1) + [1, 2] + slice(t, 4, 6)
        let has = 108 in t
        let hasNot = 300 in t
        return back + "|" + len(t) + "|" + s + "|" + same + "|" + joined + "|" + len(joined) + "|" + has + "|" + hasNot + "|" + join(s, "-") + "|" + first(t)

    # struct fields and their in-place growth
    proc held() -> text:
        let mut h = Holder{buf: bytes(1), tag: "x"}
        h.buf = append(h.buf, 2)
        h.buf = append(h.buf, 999)
        let g = Holder{buf: [5, 6], tag: "y"}
        return "" + h.buf + "|" + len(h.buf) + "|" + g.buf

    # a bytes-typed local refuses a non-byte
    proc refused() -> text:
        let mut a = bytes(1)
        a[0] = 256
        return "" + a

    # crypto and files take and give buffers
    proc crypted() -> text uses io.file:
        let key = sha256Bytes(textToBytes("k"))
        let nonce = slice(sha256Bytes(textToBytes("n")), 0, 12)
        let sealed = aesGcmSeal(key, nonce, textToBytes("payload"))
        let opened = aesGcmOpen(key, nonce, sealed)
        let w = writeFileAt("buf.bin", 0, opened)
        let r = readFileAt("buf.bin", 2, 3)
        let c = crc32(opened, 0, len(opened))
        let c2 = crc32([112, 97, 121, 108, 111, 97, 100], 0, 7)
        return bytesToText(opened) + "|" + bytesToText(r) + "|" + (c == c2) + "|" + len(sealed)

    action run(which: text) -> text:
        if which == "independent":
            let r = do independent()
            return r
        if which == "promoted":
            let r2 = do promoted()
            return r2
        if which == "grows":
            let r3 = do grows()
            return r3
        if which == "mixed":
            let r4 = do mixed()
            return r4
        if which == "held":
            let r5 = do held()
            return r5
        if which == "refused":
            let r6 = do refused()
            return r6
        let r7 = do crypted()
        return r7
    view Home at "/":
        text "bytes"
`

func TestBytesValueSemantics(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)
	g, err := compile.String(bytesValApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(dir)
	defer srv.Shutdown()
	want := map[string]string{
		"independent": "7,9,5|7,0,0|10",
		"promoted":    "1,0|1,1000|2|1000",
		"grows":       "200,201,300|200",
		"mixed":       "héllo|6|195,169|true|104,1,2,108,111|5|true|false|195-169|104",
		"held":        "0,2,999|3|5,6",
		"crypted":     "payload|ylo|true|23",
	}
	for which, w := range want {
		got, err := srv.RunValue("ada", "member", true, "run", []any{which})
		if err != nil {
			t.Fatalf("%s: %v", which, err)
		}
		if out := toStr(got); out != w {
			t.Errorf("%s = %q, want %q", which, out, w)
		}
	}
	if _, err := srv.RunValue("ada", "member", true, "run", []any{"refused"}); err == nil {
		t.Error("a bytes-typed local accepted 256")
	}
}

// A byte buffer leaving the proc engine is the JSON int list an [int] is.
func TestBytesValueEncodesAsInts(t *testing.T) {
	b, err := json.Marshal(map[string]any{"b": bytesVal{1, 2, 255}, "e": bytesVal{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"b":[1,2,255],"e":[]}` {
		t.Errorf("encoded %s", b)
	}
	if p := plainValue(bytesVal{3}); toStr(p) != "3" {
		t.Errorf("plainValue: %v", p)
	}
	if !equal(bytesVal{1, 2}, []any{1, 2}) || equal(bytesVal{1}, []any{2}) || !equal([]any{9}, bytesVal{9}) {
		t.Error("byte buffers compare element by element with int lists")
	}
	out := appendCopy(bytesVal{1}, 2)
	if _, ok := out.(bytesVal); !ok {
		t.Errorf("append of a byte keeps the buffer: %T", out)
	}
	if _, ok := appendCopy(bytesVal{1}, 1000).([]any); !ok {
		t.Error("append of a non-byte promotes")
	}
}

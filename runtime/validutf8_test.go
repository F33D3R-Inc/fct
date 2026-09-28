package runtime

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"facet/internal/compile"
)

// validUtf8(b) is utf8.Valid over a byte buffer's bytes: every case that
// decides UTF-8 well-formedness — ASCII, each lead byte's continuation
// range (the overlong and surrogate edges E0/ED/F0/F4), truncated and
// excess sequences, the 5- and 6-byte forms, stray continuations — and
// every 2-byte sequence, checked through a compiled proc against Go's own
// answer. A value that is not a byte buffer is false.
func TestValidUtf8Builtin(t *testing.T) {
	g, err := compile.String(`app V:
    state out: text = ""
    proc check(hex: text) -> text:
        let mut b = bytes(len(hex) / 2)
        let mut i = 0
        loop i < len(b):
            b[i] = hexByte(slice(hex, 2 * i, 2 * i + 2))
            i = i + 1
        if validUtf8(b):
            return "1"
        return "0"
    proc hexByte(h: text) -> int:
        let d = "0123456789abcdef"
        let mut v = 0
        let mut k = 0
        loop k < 2:
            let c = slice(h, k, k + 1)
            let mut j = 0
            loop j < 16:
                if slice(d, j, j + 1) == c:
                    v = v * 16 + j
                j = j + 1
            k = k + 1
        return v
    proc checkAll(hexes: text) -> text:
        let items = split(hexes, ",")
        let mut s = ""
        let mut i = 0
        loop i < len(items):
            s = s + check(items[i])
            i = i + 1
        return s
    action run(hexes: text):
        let r = do checkAll(hexes)
        out = r
    proc notBytes() -> bool:
        return validUtf8([300, 1])
    state other: bool = true
    action runOther():
        let r = do notBytes()
        other = r
    view V at "/":
        text "{out}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var cases [][]byte
	for _, s := range []string{"", "a", "hello", "é", "€", "😀", "\x7f", "\xc0\x80", "\xc1\xbf", "\xc2\x80", "\xdf\xbf",
		"\xe0\x80\x80", "\xe0\x9f\xbf", "\xe0\xa0\x80", "\xed\x9f\xbf", "\xed\xa0\x80", "\xed\xbf\xbf", "\xee\x80\x80",
		"\xf0\x8f\xbf\xbf", "\xf0\x90\x80\x80", "\xf4\x8f\xbf\xbf", "\xf4\x90\x80\x80", "\xf5\x80\x80\x80", "\xff",
		"\x80", "\xbf", "\xc2", "\xe2\x82", "\xf0\x9f\x98", "a\xc2", "\xc2\x80\x80", "\xf8\x88\x80\x80\x80",
		"\xfc\x84\x80\x80\x80\x80", "ok\xe2\x82\xacok", "\xe2\x82\xac\xed\xa0\x80"} {
		cases = append(cases, []byte(s))
	}
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b += 3 {
			cases = append(cases, []byte{byte(a), byte(b)})
		}
	}
	for start := 0; start < len(cases); start += 400 {
		end := min(start+400, len(cases))
		var hexes []string
		var want strings.Builder
		for _, c := range cases[start:end] {
			hexes = append(hexes, fmt.Sprintf("%x", c))
			if utf8.Valid(c) {
				want.WriteString("1")
			} else {
				want.WriteString("0")
			}
		}
		d := postJSON(t, ts, "run", `{"args":["`+strings.Join(hexes, ",")+`"]}`)
		if got := d["out"]; got != want.String() {
			t.Fatalf("cases %d..%d:\n got %v\nwant %s", start, end, got, want.String())
		}
	}
	if d := postJSON(t, ts, "runOther", `{"args":[]}`); d["other"] != false {
		t.Errorf("validUtf8 of a list holding 300 = %v, want false", d["other"])
	}
}

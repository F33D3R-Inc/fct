package runtime

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

// jsonQuote(s) is json.Marshal's spelling of the string s, byte for byte:
// \b and \f by name, other controls as \u00xx, HTML-safe, and each invalid
// UTF-8 byte replaced by U+FFFD (written as the character, not escaped).
func TestJSONQuote(t *testing.T) {
	cases := map[string]string{
		"":              `""`,
		"plain":         `"plain"`,
		`a"b\c`:         `"a\"b\\c"`,
		"\n\r\t\b\f":    `"\n\r\t\b\f"`,
		"\x00\x1f\x7f":  `"\u0000\u001f` + "\x7f" + `"`,
		"<a & b>=":      `"\u003ca \u0026 b\u003e="`,
		"\u2028\u2029":  `"\u2028\u2029"`,
		"é日本😀":          `"é日本😀"`,
		"bad\xffbyte":   `"bad` + "\ufffd" + `byte"`,
		"trunc\xe2\x82": `"trunc` + "\ufffd\ufffd" + `"`,
	}
	for in, want := range cases {
		if got := jsonQuote(in); got != want {
			t.Errorf("jsonQuote(%q) = %s, want %s", in, got, want)
		}
		b, _ := json.Marshal(in)
		if string(b) != want {
			t.Errorf("fixture %q: json.Marshal gives %s, not %s", in, b, want)
		}
	}
}

const jsonQuoteApp = `app Q:
    proc quoteAll(xs: [text]) -> text:
        let mut out = ""
        let mut i = 0
        loop i < len(xs):
            out = out + jsonQuote(xs[i]) + ";"
            i = i + 1
        return out
    state result: text = ""
    action run:
        let r = do quoteAll(["a<b", "q\"", "line\nbreak"])
        result = r
    view Home at "/":
        text "{result}"
`

func TestJSONQuoteInProcs(t *testing.T) {
	g, err := compile.String(jsonQuoteApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	want := `"a\u003cb";"q\"";"line\nbreak";`
	if got := postJSON(t, ts, "run", `{"args":[]}`)["result"]; got != want {
		t.Fatalf("result = %v, want %s", got, want)
	}
}

const jsonQuoteOutsideProc = `app Q:
    state result: text = ""
    action run(s: text):
        result = jsonQuote(s)
    view Home at "/":
        text "{result}"
`

// jsonQuote is proc-only, like the other byte/text-encoding builtins.
func TestJSONQuoteIsProcOnly(t *testing.T) {
	if _, err := compile.String(jsonQuoteOutsideProc); err == nil {
		t.Fatal("jsonQuote in an action compiled; want the proc-only refusal")
	}
}

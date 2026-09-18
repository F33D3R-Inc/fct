package runtime

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// toIntSlice converts a wire-decoded delta value (a JSON array, so []any of
// float64 once decoded) to a plain []int, the shape every test below wants to
// compare against a Go-computed reference slice.
func toIntSlice(t *testing.T, v any) []int {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("value = %#v (%T), want a JSON array", v, v)
	}
	out := make([]int, len(items))
	for i, it := range items {
		out[i] = toInt(it)
	}
	return out
}

// crc8BufRunApp generalizes runtime/proc_test.go's TestProcCRC8Live from a
// single hardcoded byte to a real multi-byte CRC-8 (polynomial 0x07, no
// reflection) computed over an actual byte buffer: `bytes(4)` allocates it,
// four indexed writes fill it from the proc's own parameters (the realistic
// shape a real caller's input takes — a proc still cannot accept an array/
// byte-buffer PARAMETER in this milestone, since that type can't be declared
// for a param/return, so the bytes are threaded in as plain int params and
// assembled into the buffer inside the proc), and the CRC loop reads it back
// with `len`/index exactly like any array consumer would — outer loop over
// each buffer byte, inner loop over its 8 bits, `crc & 128`/`(crc << 1) ^ 7`
// read straight off `buf[i]` and the running `crc` local, proving bitwise
// operators compose with byte-buffer element reads exactly as they do with
// any other int (task requirement 5).
const crc8BufRunApp = `app A:
    proc crc8Buf(b0: int, b1: int, b2: int, b3: int) -> int:
        let mut buf = bytes(4)
        buf[0] = b0
        buf[1] = b1
        buf[2] = b2
        buf[3] = b3
        let mut crc = 0
        let mut i = 0
        loop i < len(buf):
            crc = crc ^ buf[i]
            let mut j = 0
            loop j < 8:
                if (crc & 128) != 0:
                    crc = (crc << 1) ^ 7
                else:
                    crc = crc << 1
                crc = crc & 255
                j = j + 1
            i = i + 1
        return crc
    state result: int = 0
    action run(b0: int, b1: int, b2: int, b3: int):
        let r = do crc8Buf(b0, b1, b2, b3)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// goCRC8Multi is the reference implementation of the identical multi-byte
// CRC-8 crc8BufRunApp's proc computes, written with Go's own bitwise
// operators over a plain []int — the cross-check every live result below is
// asserted against, the same verification standard runtime/proc_test.go's
// goCRC8 (single-byte) set for the bitwise-operator work.
func goCRC8Multi(bs []int) int {
	crc := 0
	for _, b := range bs {
		crc ^= b & 0xFF
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc = crc << 1
			}
			crc &= 0xFF
		}
	}
	return crc
}

func TestBytesBufferCRC8Live(t *testing.T) {
	g, err := compile.String(crc8BufRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := [][4]int{
		{0x00, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x03, 0x04},
		{0xA5, 0xFF, 0x00, 0x7B},
		{255, 255, 255, 255},
	}
	for _, in := range cases {
		want := goCRC8Multi(in[:])
		deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%d,%d,%d,%d]}`, in[0], in[1], in[2], in[3]))
		if got := toInt(deltas["result"]); got != want {
			t.Fatalf("crc8Buf(%v) over the wire = %v, want %d (Go's own multi-byte CRC-8 over the identical bytes)", in, deltas["result"], want)
		}
	}
}

// xorCipherRunApp is the task's alternative worked example: build a byte
// buffer from an input, then XOR every byte against a fixed key byte in
// place (`buf[i] = buf[i] ^ key`) — a genuine XOR-cipher-style transform
// exercising an index READ, a bitwise `^`, and an index WRITE back into the
// same buffer slot in one statement. It returns the transformed buffer whole,
// as `-> [int]`: a byte buffer is exactly the []any an array is at runtime
// (see internal/ir/build.go's bytesType doc), so it needs no new return-path
// machinery — the existing list-return coercion (runtime/server.go's
// coerceRet) that already handles a proc declared `-> [T]` carries it over
// the wire unchanged. This is this task's requirement 6 (a way to observe a
// byte buffer's final content over HTTP without trusting only `len()`).
const xorCipherRunApp = `app A:
    proc xorCipher(a: int, b: int, c: int, key: int) -> [int]:
        let mut buf = bytes(3)
        buf[0] = a
        buf[1] = b
        buf[2] = c
        let mut i = 0
        loop i < len(buf):
            buf[i] = buf[i] ^ key
            i = i + 1
        return buf
    state cipherOut: [int] = []
    action run(a: int, b: int, c: int, key: int):
        let r = do xorCipher(a, b, c, key)
        cipherOut = r
    view Home at "/":
        box:
            text "{cipherOut}"
`

func TestBytesBufferXorCipherLive(t *testing.T) {
	g, err := compile.String(xorCipherRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	in := [3]int{10, 200, 42}
	key := 0x5A
	want := []int{in[0] ^ key, in[1] ^ key, in[2] ^ key}

	deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%d,%d,%d,%d]}`, in[0], in[1], in[2], key))
	got := toIntSlice(t, deltas["cipherOut"])
	if len(got) != len(want) {
		t.Fatalf("xorCipher(%v, key=%#x) over the wire = %v, want %v", in, key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("xorCipher(%v, key=%#x)[%d] over the wire = %d, want %d (Go's own byte ^ key)", in, key, i, got[i], want[i])
		}
	}

	// Applying the same key a second time must decrypt back to the original
	// bytes — the textbook XOR-cipher property (`(x ^ k) ^ k == x`), a second
	// independent cross-check beyond the raw Go-computed values above.
	deltas = postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%d,%d,%d,%d]}`, want[0], want[1], want[2], key))
	roundTrip := toIntSlice(t, deltas["cipherOut"])
	for i := range in {
		if roundTrip[i] != in[i] {
			t.Fatalf("round-trip xorCipher(xorCipher(x, key), key)[%d] = %d, want original %d", i, roundTrip[i], in[i])
		}
	}
}

// bytesOutOfRangeApp isolates the range check: writing 256 (above the 0-255
// domain) or -1 (below it) into a byte-buffer slot must be a clean runtime
// error, never a silent truncate/wraparound (Go's own `byte(256) == 0` and
// `byte(-1) == 255` would both be silent data corruption here) and never a
// panic reaching the HTTP layer — the same standard
// TestArrayOutOfBoundsIsCleanError already holds array index errors to.
const bytesOutOfRangeApp = `app A:
    proc badHigh() -> int:
        let mut buf = bytes(2)
        buf[0] = 256
        return buf[0]
    proc badLow() -> int:
        let mut buf = bytes(2)
        buf[0] = 0 - 1
        return buf[0]
    proc ok255() -> int:
        let mut buf = bytes(2)
        buf[0] = 255
        return buf[0]
    state result: int = 0
    action doBadHigh():
        let r = do badHigh()
        result = r
    action doBadLow():
        let r = do badLow()
        result = r
    action doOk255():
        let r = do ok255()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestBytesBufferOutOfRangeIsCleanError(t *testing.T) {
	g, err := compile.String(bytesOutOfRangeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, c := range []struct {
		action string
	}{{"doBadHigh"}, {"doBadLow"}} {
		resp, err := postRaw(t, ts, c.action, `{"args":[]}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode < 400 {
			t.Fatalf("%s (out-of-range byte write) returned %d, want a 4xx/5xx error status", c.action, resp.StatusCode)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "out of range") {
			t.Errorf("%s error body should mention the value being out of range, got: %s", c.action, body)
		}
		resp.Body.Close()
	}

	// 255 is the top of the valid range and must NOT be rejected — the check
	// is a closed [0, 255] interval, not an off-by-one-tighter one.
	deltas := postJSON(t, ts, "doOk255", `{"args":[]}`)
	if got := toInt(deltas["result"]); got != 255 {
		t.Fatalf("writing 255 into a byte-buffer slot = %v, want 255 (255 is in range)", deltas["result"])
	}

	// The server must still be alive and answering normally after both failed
	// calls — proof this was a handled error, not a crash.
	deltas = postJSON(t, ts, "doOk255", `{"args":[]}`)
	if got := toInt(deltas["result"]); got != 255 {
		t.Fatalf("server did not recover cleanly after the out-of-range calls: ok255() = %v, want 255", deltas["result"])
	}
}

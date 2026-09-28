package parser

import "sort"

// BuiltinSite is where a builtin may run — the one fact the compiler's
// placement rules and the browser runtime must agree on.
type BuiltinSite int

const (
	// SiteEverywhere: pure (or, for now/rand, placement-checked) and
	// implemented identically by the server (runtime/eval.go's callBuiltin)
	// and the browser (runtime/assets/facet.js's evCall), so it may appear in
	// a view, a policy, a derive, or an action the browser runs.
	// runtime/builtinparity_test.go fails when facet.js lacks one of these or
	// answers one differently.
	SiteEverywhere BuiltinSite = iota
	// SiteAuthority: only the server implements it (a secret, a stored file,
	// the zone database, a 64-bit bit pattern, the caller's sent parameters,
	// the server's own log). Callable in an action — which it pins to the
	// server — a proc, or a derive (which then serves only actions); never in
	// a view or policy.
	SiteAuthority
	// SiteProc: only inside a proc body — real I/O, concurrency, byte
	// buffers and the cryptography over them, all of which live in the proc
	// engine (runtime/proccompile.go) alone.
	SiteProc
)

// builtinSites is every builtin in call position and where it may run.
var builtinSites = map[string]BuiltinSite{
	// the clock and RNG (effectful: a view refuses them, an action that uses
	// them runs on the server unless it only writes @client state)
	"now": SiteEverywhere, "rand": SiteEverywhere,
	// math, money and conversion
	"abs": SiteEverywhere, "min": SiteEverywhere, "max": SiteEverywhere, "floor": SiteEverywhere, "round": SiteEverywhere,
	"money": SiteEverywhere, "toMoney": SiteEverywhere, "toFloat": SiteEverywhere, "toInt": SiteEverywhere,
	// text
	"len": SiteEverywhere, "upper": SiteEverywhere, "lower": SiteEverywhere, "trim": SiteEverywhere,
	"contains": SiteEverywhere, "take": SiteEverywhere, "split": SiteEverywhere, "join": SiteEverywhere,
	"slice": SiteEverywhere, "charAt": SiteEverywhere, "replace": SiteEverywhere, "slug": SiteEverywhere,
	"byteLen": SiteEverywhere,
	// indexOf(s, sub, from) / indexOf(xs, x, from): a rune index or a list
	// position, -1 when absent — what orders rows by a list's order
	// (`for w in Work where w.id in ids by indexOf(ids, w.id, 0)`). A byte
	// buffer's form stays in procs, where buffers live.
	"indexOf": SiteEverywhere,
	// dates and formatting
	"year": SiteEverywhere, "month": SiteEverywhere, "day": SiteEverywhere,
	"ago": SiteEverywhere, "compact": SiteEverywhere, "commas": SiteEverywhere, "iso": SiteEverywhere, "fromIso": SiteEverywhere,
	// lists and JSON
	"first": SiteEverywhere, "fromJson": SiteEverywhere,

	// server only
	"print": SiteAuthority, // a line in the authority's own log
	"given": SiteAuthority, // which optional parameters the caller sent
	// IEEE-754 bit casts: the 64-bit pattern does not fit a browser number
	"floatBits": SiteAuthority, "floatFromBits": SiteAuthority,
	// The transcendental functions a scoring model is written in (a cosine,
	// a Gaussian, a sigmoid, an exponential decay): Go's math.Exp/Log/Sqrt,
	// so an answer is predictable from Go's documentation. The authority's
	// only, since the browser's evaluator would have to agree bit for bit.
	"exp": SiteAuthority, "ln": SiteAuthority, "sqrt": SiteAuthority,
	// sin/cos: the Hann window and the FFT twiddle factors signal analysis
	// (facets/media/audio.fct) is written in; Go's math.Sin/Cos.
	"sin": SiteAuthority, "cos": SiteAuthority,
	// authentication secrets, stored files, signatures and content ids
	"verifyPassword": SiteAuthority, "totpSecret": SiteAuthority, "totpValid": SiteAuthority, "randomToken": SiteAuthority,
	"fileDigest": SiteAuthority, "ed25519Verify": SiteAuthority, "ecdsaP256Verify": SiteAuthority,
	"sha256Hex": SiteAuthority, "canonicalJson": SiteAuthority, "shuffleOrder": SiteAuthority,
	// u64 arithmetic over an int's 64 bits: a Rust u64 (shard ids, write
	// sequences, counters, byte counts off the wire) is held as that bit
	// pattern, and these are the operations whose answer depends on reading
	// it unsigned. A value past 2^53 does not survive a browser number.
	"u64Cmp": SiteAuthority, "u64Min": SiteAuthority, "u64Max": SiteAuthority, "u64SatSub": SiteAuthority,
	"u64Div": SiteAuthority, "u64Rem": SiteAuthority, "u64Text": SiteAuthority, "u64Parse": SiteAuthority,
	"u64ParseError": SiteAuthority, "u64ToFloat": SiteAuthority,
	// wall-clock time in an IANA zone (the zone database is embedded server-side)
	"fromLocal": SiteAuthority, "formatIn": SiteAuthority, "zoneValid": SiteAuthority,

	// proc bodies only
	"append": SiteProc, "bytes": SiteProc, "textToBytes": SiteProc, "bytesToText": SiteProc,
	// validUtf8(b) -> bool: whether a byte buffer is well-formed UTF-8 (no overlong form, no
	// surrogate, nothing past U+10FFFF) — what a decoder written in fct (selfhost/fabric_json.fct)
	// checks a string's raw bytes with instead of a per-byte loop
	"validUtf8": SiteProc,
	// jsonQuote(s) -> text: s as a JSON string literal, exactly as encoding/json's json.Marshal
	// spells it (HTML-safe: <, >, & as \u003c/\u003e/\u0026; U+2028/U+2029 escaped; each invalid
	// UTF-8 byte as the character U+FFFD) — what an IR or API encoder written in fct (selfhost/ir_expr.fct) quotes
	// every string with instead of a per-byte loop
	"jsonQuote":  SiteProc,
	"aesGcmSeal": SiteProc, "aesGcmOpen": SiteProc, "aesGcmAuthentic": SiteProc, "randomBytes": SiteProc,
	// indexOf(s, sub, from) -> int: the rune index of sub in s at or after from, -1 when absent —
	// what a parser written in fct (selfhost/ir_json.fct) scans a document with instead of a
	// charAt per byte (strings.Index, rune-indexed like slice/charAt)

	// keyed digests and password hashing over byte buffers (runtime/cryptobuiltins.go): what a
	// self-hosted runtime signs its cookies and stores its credentials with
	"p256PrivateKey": SiteProc, "p256PublicKey": SiteProc, "p256Ecdh": SiteProc, "es256Sign": SiteProc, "fromBase64UrlRaw": SiteProc, "httpSend": SiteProc,
	// uploadBytes(ref) -> [int]: the content of one of this server's stored uploads (the value
	// a `bytes` parameter holds), empty when ref is not one — what a proc decodes an upload from.
	"uploadBytes": SiteProc,
	"sha256Bytes": SiteProc, "hmacSha256": SiteProc, "base64UrlRaw": SiteProc, "bcryptHash": SiteProc, "bcryptMatches": SiteProc,
	"readFile": SiteProc, "writeFile": SiteProc, "appendFile": SiteProc, "fileExists": SiteProc, "truncateFile": SiteProc,
	"fileSize": SiteProc, "fileModTime": SiteProc, "readFileAt": SiteProc, "writeFileAt": SiteProc, "syncFile": SiteProc, "renameFile": SiteProc, "removeFile": SiteProc, "lockFile": SiteProc, "crc32": SiteProc, "bytesCmp": SiteProc, "bytesCmpRange": SiteProc, "uintLE": SiteProc, "toHex": SiteProc, "fromHex": SiteProc, "bytesPut": SiteProc,
	"httpGet": SiteProc, "httpPost": SiteProc,
	"listen": SiteProc, "listenOn": SiteProc, "accept": SiteProc, "connect": SiteProc,
	"readBytes": SiteProc, "writeBytes": SiteProc, "closeConn": SiteProc, "setTimeoutMs": SiteProc, "connError": SiteProc,
	"pollBytes": SiteProc, "connOpen": SiteProc, "shutdownConn": SiteProc, "closeWrite": SiteProc,
	"closeListener": SiteProc, "listenError": SiteProc, "listenerPort": SiteProc, "grantRead": SiteProc,
	// grantDir(path) (main only): an operator-named directory the io.file builtins may use;
	// listDir(path) -> [text] and makeDir(path) -> bool: a directory's files, and one made
	// (runtime/grants.go, runtime/dirs.go); readStdinLine() -> text: one line of stdin.
	"grantDir": SiteProc, "listDir": SiteProc, "makeDir": SiteProc, "readStdinLine": SiteProc,
	"writeStdout": SiteProc, "writeStderr": SiteProc, "readStdin": SiteProc, "envVar": SiteProc, "envSet": SiteProc,
	"channel": SiteProc, "send": SiteProc, "trySend": SiteProc, "recv": SiteProc, "sleepMs": SiteProc, "monoMs": SiteProc, "nowMs": SiteProc, "signals": SiteProc,
	"awaitAny": SiteProc, "closeChannel": SiteProc, "exitProcess": SiteProc, "processStats": SiteProc, "listenTls": SiteProc, "connPeer": SiteProc, "connectTls": SiteProc,
}

// BuiltinSiteOf reports where builtin name may run, and whether it is one.
func BuiltinSiteOf(name string) (BuiltinSite, bool) {
	s, ok := builtinSites[name]
	return s, ok
}

// Builtins is every builtin name, sorted.
func Builtins() []string {
	out := make([]string, 0, len(builtinSites))
	for n := range builtinSites {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ClientBuiltins is every SiteEverywhere builtin, sorted: the set the
// browser runtime must implement.
func ClientBuiltins() []string {
	var out []string
	for n, s := range builtinSites {
		if s == SiteEverywhere {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

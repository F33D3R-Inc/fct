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
	"aesGcmSeal": SiteProc, "aesGcmOpen": SiteProc, "aesGcmAuthentic": SiteProc, "randomBytes": SiteProc,
	"readFile": SiteProc, "writeFile": SiteProc, "appendFile": SiteProc, "fileExists": SiteProc, "truncateFile": SiteProc,
	"fileSize": SiteProc, "readFileAt": SiteProc, "writeFileAt": SiteProc, "syncFile": SiteProc, "renameFile": SiteProc, "removeFile": SiteProc,
	"httpGet": SiteProc, "httpPost": SiteProc,
	"listen": SiteProc, "listenOn": SiteProc, "accept": SiteProc, "connect": SiteProc,
	"readBytes": SiteProc, "writeBytes": SiteProc, "closeConn": SiteProc, "setTimeoutMs": SiteProc, "connError": SiteProc,
	"pollBytes": SiteProc, "connOpen": SiteProc, "shutdownConn": SiteProc,
	"closeListener": SiteProc, "listenError": SiteProc, "grantRead": SiteProc,
	"writeStdout": SiteProc, "writeStderr": SiteProc, "readStdin": SiteProc, "envVar": SiteProc, "envSet": SiteProc,
	"channel": SiteProc, "send": SiteProc, "recv": SiteProc, "sleepMs": SiteProc, "monoMs": SiteProc, "nowMs": SiteProc, "signals": SiteProc,
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

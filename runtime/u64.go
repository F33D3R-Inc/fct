package runtime

import (
	"math"
	"strconv"
	"strings"
)

// The u64 builtins. The language has one integer type, a signed 64-bit int,
// and the fabric ports hold a Rust u64 (shard ids, write sequences,
// monotonic counters, byte counts and timestamps decoded off the wire) as
// that int's 64-bit pattern. Addition, subtraction, multiplication,
// equality, bitwise operators and shifts already give the u64 answer on
// those bits (two's complement), so they need nothing new. What does not
// are the operations whose result depends on reading the top bit as 2^63
// rather than as a sign: ordering (u64Cmp, u64Min, u64Max — and so a
// BTreeMap<u64, _>'s key order), saturating_sub (u64SatSub), division and
// remainder (u64Div, u64Rem), Display (u64Text), FromStr (u64Parse /
// u64ParseError, Rust's own messages) and `as f64` (u64ToFloat). Those are
// what the fabric needs; a separate unsigned scalar type would buy nothing
// the bit pattern does not already carry, and would have to be threaded
// through the type system, the wire and the self-hosted compiler for it.
//
// Division or remainder by zero answers 0, the same total-runtime sentinel
// int `/` and `%` use (Rust would panic).

func u64Arg(args []any, i int) uint64 {
	if i < len(args) {
		return uint64(toInt(args[i]))
	}
	return 0
}

func u64Builtin(name string, args []any) any {
	a, b := u64Arg(args, 0), u64Arg(args, 1)
	switch name {
	case "u64Cmp":
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	case "u64Min":
		return int(min(a, b))
	case "u64Max":
		return int(max(a, b))
	case "u64SatSub":
		if a < b {
			return 0
		}
		return int(a - b)
	case "u64Div":
		if b == 0 {
			return 0
		}
		return int(a / b)
	case "u64Rem":
		if b == 0 {
			return 0
		}
		return int(a % b)
	case "u64Text":
		return strconv.FormatUint(a, 10)
	case "u64Parse":
		v, msg := parseU64(toStr(argAt(args, 0)))
		if msg != "" {
			return 0
		}
		return int(v)
	case "u64ParseError":
		_, msg := parseU64(toStr(argAt(args, 0)))
		return msg
	case "u64ToFloat":
		// `u as f64`: round to nearest, ties to even — Go's uint64->float64
		// conversion is exactly that.
		return float64(a)
	}
	return nil
}

func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// parseU64 is Rust's `<u64 as FromStr>::from_str`: an optional leading '+',
// then ASCII digits only; the error is ParseIntError's Display.
func parseU64(s string) (uint64, string) {
	if s == "" {
		return 0, "cannot parse integer from empty string"
	}
	digits := strings.TrimPrefix(s, "+")
	if digits == "" {
		return 0, "invalid digit found in string"
	}
	var v uint64
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, "invalid digit found in string"
		}
		d := uint64(c - '0')
		// checked_mul then checked_add, digit by digit: the first
		// overflow ends the parse, before any later character is seen.
		if v > (math.MaxUint64-d)/10 {
			return 0, "number too large to fit in target type"
		}
		v = v*10 + d
	}
	return v, ""
}

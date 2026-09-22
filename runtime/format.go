package runtime

import (
	"strconv"
	"time"
)

// The formatting builtins a feed reads by: `ago(ts)`, `compact(n)`, `commas(n)`.
// Each is mirrored character for character in assets/facet.js, and
// runtime/format_test.go runs the shipped client's copy over the same cases —
// a count that renders "1.3K" on first paint and "1.2K" after hydration is a
// page that changes under the reader, so the arithmetic here is integer-only:
// no float formatting either side could round differently.

// clock is the render clock `ago` reads. It is a variable so a test can pin it;
// nothing else may set it.
var clock = time.Now

// iso renders a unix timestamp as an RFC 3339 UTC instant ("2026-09-21T21:40:00Z")
// — the wire form a `datetime`-typed field/parameter carries (runtime/
// contract.go's wireSchema emits `format: date-time` for it), the same way
// `money(n)` renders a `money` int as its own decimal text. `now()` is the
// int this formats: `iso(now())` is how an action populates a `datetime`
// field with the current instant.
func iso(ts int) string {
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// ago renders a unix timestamp relative to now the way a timeline does: "now"
// under a minute, then "5m", "20h", then a date — "Jun 3" inside the current
// year, "Jun 3, 2025" outside it. Days are not counted ("3d") because past a day
// a reader wants the date, and a 30-day boundary is a fact about a calendar
// that two clocks can disagree on at midnight.
//
// It is pure in the sense the placement calculus cares about — it writes
// nothing, it reads no state — but it reads the clock, so the server's first
// paint and the client's re-render can differ by however long the page took to
// arrive. That is what "20h" means; the client's refresh recomputes it.
func ago(ts, now int) string {
	d := now - ts
	switch {
	case d < 60:
		return "now"
	case d < 3600:
		return strconv.Itoa(d/60) + "m"
	case d < 86400:
		return strconv.Itoa(d/3600) + "h"
	}
	t := time.Unix(int64(ts), 0).UTC()
	n := time.Unix(int64(now), 0).UTC()
	s := t.Month().String()[:3] + " " + strconv.Itoa(t.Day())
	if t.Year() != n.Year() {
		s += ", " + strconv.Itoa(t.Year())
	}
	return s
}

// compact renders a count the way an engagement bar does: plain under a
// thousand, then one decimal and a unit — "1.3K", "79.8K", "604K", "240.1M",
// "2B". The decimal is dropped when it is zero, never rounded: 1999 is "1.9K",
// because the integer division both interpreters perform truncates, and a
// truncation is the same on every platform where a rounding might not be.
func compact(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	unit, suffix := 1, ""
	switch {
	case n >= 1_000_000_000:
		unit, suffix = 1_000_000_000, "B"
	case n >= 1_000_000:
		unit, suffix = 1_000_000, "M"
	case n >= 1000:
		unit, suffix = 1000, "K"
	}
	var s string
	if unit == 1 {
		s = strconv.Itoa(n)
	} else {
		tenths := n / (unit / 10)
		s = strconv.Itoa(tenths / 10)
		if tenths%10 != 0 {
			s += "." + strconv.Itoa(tenths%10)
		}
		s += suffix
	}
	if neg {
		return "-" + s
	}
	return s
}

// commas renders an integer with thousands separators: 1352 → "1,352".
func commas(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.Itoa(n)
	var out []byte
	for i, c := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

package registry

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is a parsed MAJOR.MINOR.PATCH. Pre-release and build metadata are
// dropped — facets version with plain release tags, which keeps selection
// simple and predictable.
type semver struct{ major, minor, patch int }

// parseSemver reads "v1.2.3" or "1.2.3" (a missing minor/patch counts as 0).
func parseSemver(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return semver{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return semver{}, false
	}
	var v semver
	dst := []*int{&v.major, &v.minor, &v.patch}
	for i := range parts {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return semver{}, false
		}
		*dst[i] = n
	}
	return v, true
}

// cmp returns -1, 0, or 1 comparing a to b.
func (a semver) cmp(b semver) int {
	switch {
	case a.major != b.major:
		return sign(a.major - b.major)
	case a.minor != b.minor:
		return sign(a.minor - b.minor)
	case a.patch != b.patch:
		return sign(a.patch - b.patch)
	}
	return 0
}

func (v semver) String() string { return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch) }

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// isHexSHA reports whether s is a 40-character hex commit id.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// isTagForm reports whether a selection form should be resolved against the tag
// list (an exact version, or a caret/tilde range) rather than treated as a
// branch name.
func isTagForm(form string) bool {
	if strings.HasPrefix(form, "^") || strings.HasPrefix(form, "~") {
		_, ok := parseSemver(form[1:])
		return ok
	}
	_, ok := parseSemver(form)
	return ok
}

// selectTag chooses one tag from a repo's tag list per a selection form:
//
//	"" / "latest"   highest vX.Y.Z tag
//	"v1.2.3"        that exact tag
//	"^1.2.3"        highest tag >= 1.2.3 within the same major
//	"~1.2.3"        highest tag >= 1.2.3 within the same major+minor
func selectTag(tags []Tag, form string) (Tag, error) {
	type cand struct {
		tag Tag
		v   semver
	}
	var cands []cand
	for _, t := range tags {
		if v, ok := parseSemver(t.Name); ok {
			cands = append(cands, cand{t, v})
		}
	}
	if len(cands) == 0 {
		return Tag{}, fmt.Errorf("no vX.Y.Z tags published (publish a release tag, or pin a branch/commit)")
	}

	switch {
	case form == "" || form == "latest":
		best := cands[0]
		for _, c := range cands[1:] {
			if c.v.cmp(best.v) > 0 {
				best = c
			}
		}
		return best.tag, nil

	case strings.HasPrefix(form, "^") || strings.HasPrefix(form, "~"):
		caret := form[0] == '^'
		base, ok := parseSemver(form[1:])
		if !ok {
			return Tag{}, fmt.Errorf("invalid version range %q", form)
		}
		var best *cand
		for i := range cands {
			c := cands[i]
			if c.v.cmp(base) < 0 || c.v.major != base.major {
				continue
			}
			if !caret && c.v.minor != base.minor {
				continue
			}
			if best == nil || c.v.cmp(best.v) > 0 {
				best = &cands[i]
			}
		}
		if best == nil {
			return Tag{}, fmt.Errorf("no published tag matches %q", form)
		}
		return best.tag, nil

	default: // exact version
		want, ok := parseSemver(form)
		if !ok {
			return Tag{}, fmt.Errorf("invalid version %q", form)
		}
		for _, c := range cands {
			if c.v.cmp(want) == 0 {
				return c.tag, nil
			}
		}
		return Tag{}, fmt.Errorf("no tag %s found", want)
	}
}

// satisfiesRange reports whether v meets a space-separated AND of comparators
// (e.g. ">=1.4.0", "^1.2.0", or ">=1.0.0 <2.0.0"). A bare version means exact.
func satisfiesRange(expr string, v semver) (bool, error) {
	for _, tok := range strings.Fields(expr) {
		ok, err := satisfiesOne(tok, v)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func satisfiesOne(tok string, v semver) (bool, error) {
	parse := func(s string) (semver, error) {
		b, ok := parseSemver(s)
		if !ok {
			return semver{}, fmt.Errorf("invalid version constraint %q", tok)
		}
		return b, nil
	}
	switch {
	case strings.HasPrefix(tok, ">="):
		b, err := parse(tok[2:])
		return v.cmp(b) >= 0, err
	case strings.HasPrefix(tok, "<="):
		b, err := parse(tok[2:])
		return v.cmp(b) <= 0, err
	case strings.HasPrefix(tok, ">"):
		b, err := parse(tok[1:])
		return v.cmp(b) > 0, err
	case strings.HasPrefix(tok, "<"):
		b, err := parse(tok[1:])
		return v.cmp(b) < 0, err
	case strings.HasPrefix(tok, "="):
		b, err := parse(tok[1:])
		return v.cmp(b) == 0, err
	case strings.HasPrefix(tok, "^"):
		b, err := parse(tok[1:])
		return v.cmp(b) >= 0 && v.major == b.major, err
	case strings.HasPrefix(tok, "~"):
		b, err := parse(tok[1:])
		return v.cmp(b) >= 0 && v.major == b.major && v.minor == b.minor, err
	default:
		b, err := parse(tok)
		return v.cmp(b) == 0, err
	}
}

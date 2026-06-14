package codegen

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

// A facet's `style:` block is the cross-platform style layer: a small, token-based
// vocabulary that the compiler resolves — at build time — into a concrete inline
// style on the facet's root element. Because the native neutral tree (fa.ParseView
// → fa.resolveStyle) reads each node's inline style, the SAME resolved style lands
// on web (DOM), iOS (SwiftUI) and Android (Compose) with no per-platform code.
//
// It is deliberately not arbitrary CSS: every value is a design token (a spacing
// step, a color name, a radius/size/weight token, a layout enum), so appearance is
// expressed once and renders identically everywhere. Web-only effects (`:hover`,
// `@media`, keyframes) are explicitly out of scope — they belong to a future,
// clearly web-scoped escape hatch, never to this block.

// spaceUnit is the spacing scale: a `gap`/`pad` token n means n*spaceUnit px (a
// 4px base, the standard-library rhythm).
const spaceUnit = 4

// colorTokens maps semantic color names to concrete hex, resolved to literals
// (not CSS vars) so native renders the exact same color as web. Values mirror the
// std design system (std/style.css :root).
var colorTokens = map[string]string{
	"fg":          "#0f1419",
	"text":        "#0f1419",
	"muted":       "#5b7083",
	"border":      "#cfd9de",
	"bg":          "#ffffff",
	"surface":     "#ffffff",
	"primary":     "#1d9bf0",
	"accent":      "#1d9bf0",
	"on-primary":  "#ffffff",
	"danger":      "#f4212e",
	"transparent": "transparent",
}

var radiusTokens = map[string]int{"none": 0, "sm": 6, "md": 12, "lg": 16, "pill": 999, "full": 999}
var fontSizeTokens = map[string]int{"sm": 13, "base": 15, "md": 15, "lg": 18, "xl": 24, "2xl": 28}
var fontWeightTokens = map[string]int{"normal": 400, "medium": 600, "bold": 700, "black": 800}
var alignTokens = map[string]string{"start": "flex-start", "center": "center", "end": "flex-end", "stretch": "stretch"}
var justifyTokens = map[string]string{"start": "flex-start", "center": "center", "end": "flex-end", "between": "space-between"}

// resolveStyleBlock turns a facet's style: props into one inline-style string for
// the root element. Layout props (direction/gap/align/justify) imply a flex
// container so they behave the same on web and in the neutral tree. Every unknown
// property or token is a compile error — the data contract for appearance.
func resolveStyleBlock(props []ast.StyleProp) (string, error) {
	if len(props) == 0 {
		return "", nil
	}
	var body []string // non-container declarations, in author order
	direction := ""
	needsFlex := false
	for _, p := range props {
		switch p.Key {
		case "direction":
			if p.Val != "row" && p.Val != "column" {
				return "", fmt.Errorf("style direction must be `row` or `column`, got %q", p.Val)
			}
			direction, needsFlex = p.Val, true
		case "gap":
			n, err := spaceVal("gap", p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "gap:"+px(n))
			needsFlex = true
		case "align":
			v, ok := alignTokens[p.Val]
			if !ok {
				return "", tokenErr("align", p.Val, alignTokens)
			}
			body = append(body, "align-items:"+v)
			needsFlex = true
		case "justify":
			v, ok := justifyTokens[p.Val]
			if !ok {
				return "", tokenErr("justify", p.Val, justifyTokens)
			}
			body = append(body, "justify-content:"+v)
			needsFlex = true
		case "grow":
			switch p.Val {
			case "true":
				body = append(body, "flex:1 1 0%")
			case "false":
			default:
				return "", fmt.Errorf("style grow must be `true` or `false`, got %q", p.Val)
			}
		case "pad":
			s, err := padVal(p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "padding:"+s)
		case "pad-x":
			n, err := spaceVal("pad-x", p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "padding-left:"+px(n), "padding-right:"+px(n))
		case "pad-y":
			n, err := spaceVal("pad-y", p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "padding-top:"+px(n), "padding-bottom:"+px(n))
		case "width", "height":
			v, err := dimVal(p.Key, p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, p.Key+":"+v)
		case "bg":
			c, err := colorVal("bg", p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "background:"+c)
		case "fg":
			c, err := colorVal("fg", p.Val)
			if err != nil {
				return "", err
			}
			body = append(body, "color:"+c)
		case "radius":
			n, ok := radiusTokens[p.Val]
			if !ok {
				return "", tokenErr("radius", p.Val, radiusTokens)
			}
			body = append(body, "border-radius:"+px(n))
		case "font-size":
			n, ok := fontSizeTokens[p.Val]
			if !ok {
				return "", tokenErr("font-size", p.Val, fontSizeTokens)
			}
			body = append(body, "font-size:"+px(n))
		case "font-weight":
			n, ok := fontWeightTokens[p.Val]
			if !ok {
				return "", tokenErr("font-weight", p.Val, fontWeightTokens)
			}
			body = append(body, "font-weight:"+strconv.Itoa(n))
		default:
			return "", fmt.Errorf("unknown style property %q (expected one of: align, bg, direction, fg, font-size, font-weight, gap, grow, height, justify, pad, pad-x, pad-y, radius, width)", p.Key)
		}
	}
	var out []string
	switch {
	case direction != "":
		out = append(out, "display:flex", "flex-direction:"+direction)
	case needsFlex:
		out = append(out, "display:flex")
	}
	out = append(out, body...)
	return strings.Join(out, ";"), nil
}

func px(n int) string { return strconv.Itoa(n) + "px" }

// spaceVal resolves a spacing token (a non-negative integer step) to pixels.
func spaceVal(prop, val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("style %s must be a non-negative spacing step (e.g. 2 → %dpx), got %q", prop, 2*spaceUnit, val)
	}
	return n * spaceUnit, nil
}

// padVal resolves `pad` — one step (uniform) or two steps (vertical horizontal).
func padVal(val string) (string, error) {
	f := strings.Fields(val)
	switch len(f) {
	case 1:
		n, err := spaceVal("pad", f[0])
		if err != nil {
			return "", err
		}
		return px(n), nil
	case 2:
		v, err := spaceVal("pad", f[0])
		if err != nil {
			return "", err
		}
		h, err := spaceVal("pad", f[1])
		if err != nil {
			return "", err
		}
		return px(v) + " " + px(h), nil
	default:
		return "", fmt.Errorf("style pad takes 1 step (uniform) or 2 (vertical horizontal), got %q", val)
	}
}

// dimVal resolves a width/height: `fill`, a pixel step count, or a percentage.
func dimVal(prop, val string) (string, error) {
	switch {
	case val == "fill":
		return "100%", nil
	case strings.HasSuffix(val, "%"):
		if _, err := strconv.Atoi(strings.TrimSuffix(val, "%")); err == nil {
			return val, nil
		}
	default:
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			return px(n), nil
		}
	}
	return "", fmt.Errorf("style %s must be `fill`, a pixel count (e.g. 120), or a percentage (e.g. 50%%), got %q", prop, val)
}

// colorVal resolves a color token to hex, or passes a literal #hex through (an
// occasional necessity, kept explicit).
func colorVal(prop, val string) (string, error) {
	if strings.HasPrefix(val, "#") {
		return val, nil
	}
	if c, ok := colorTokens[val]; ok {
		return c, nil
	}
	return "", fmt.Errorf("style %s %q is not a color token (%s) or a #hex value", prop, val, sortedKeys(colorTokens))
}

func tokenErr[V any](prop, val string, table map[string]V) error {
	return fmt.Errorf("style %s %q is not one of: %s", prop, val, sortedKeys(table))
}

func sortedKeys[V any](m map[string]V) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// injectRootStyle merges the resolved inline style into the facet's root element,
// either extending an existing style="" (resolved tokens first, so a hand-written
// inline declaration still wins) or adding a fresh attribute. Mirrors
// injectFacetID's tag scan (skipping leading {{...}} actions).
func injectRootStyle(s, css string) string {
	if css == "" {
		return s
	}
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "{{") {
			j := strings.Index(s[i:], "}}")
			if j < 0 {
				return s
			}
			i += j + 2
			continue
		}
		if s[i] == '>' {
			break
		}
		i++
	}
	if i >= len(s) {
		return s // no opening tag (root is a {{...}} action) — nothing to attach to
	}
	tagEnd := i
	if i > 0 && s[i-1] == '/' {
		tagEnd = i - 1
	}
	if k := strings.Index(s[:tagEnd], ` style="`); k >= 0 {
		insert := k + len(` style="`)
		return s[:insert] + css + ";" + s[insert:]
	}
	return s[:tagEnd] + ` style="` + css + `"` + s[tagEnd:]
}

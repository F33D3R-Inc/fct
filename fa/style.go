package fa

import (
	"strconv"
	"strings"
)

// Style is the server-resolved, platform-neutral layout + appearance of a
// ViewNode. It removes guesswork from native clients: instead of a renderer
// inferring "is this a row?" from CSS class names, the SERVER resolves layout and
// style once and ships it, so iOS/Android render exactly what the web does.
//
// It is intentionally a small, cross-platform vocabulary (flexbox-shaped, which
// maps cleanly to SwiftUI stacks and Compose Row/Column): direction + gap + align
// + padding for layout, and a handful of paint properties. It is resolved from a
// node's inline style="" plus a built-in table for the standard library's design
// system; unknown classes simply contribute nothing (sane defaults remain).
type Style struct {
	Direction string `json:"direction,omitempty"` // "row" | "column"
	Gap       int    `json:"gap,omitempty"`       // spacing between children, px
	// Pad is an internal uniform-padding shorthand used by the class table; the
	// resolver expands it into per-side padding, so it is never emitted directly.
	Pad        int    `json:"-"`
	PadT       int    `json:"padT,omitempty"`       // padding, per side, px
	PadR       int    `json:"padR,omitempty"`       //
	PadB       int    `json:"padB,omitempty"`       //
	PadL       int    `json:"padL,omitempty"`       //
	Align      string `json:"align,omitempty"`      // start | center | end | stretch
	Justify    string `json:"justify,omitempty"`    // start | center | end | between
	Grow       bool   `json:"grow,omitempty"`       // expand to fill the main axis
	Width      string `json:"width,omitempty"`      // "120px" | "30%" | "fill"
	Height     string `json:"height,omitempty"`     // "120px" | "50%" | "fill"
	BG         string `json:"bg,omitempty"`         // background color (css token)
	FG         string `json:"fg,omitempty"`         // text/foreground color
	FontSize   int    `json:"fontSize,omitempty"`   // px
	FontWeight int    `json:"fontWeight,omitempty"` // 400 | 600 | 700 | 800
	Radius     int    `json:"radius,omitempty"`     // corner radius, px
}

func (s *Style) isZero() bool { return *s == Style{} }

// expandPad fills any per-side padding left unset from the uniform Pad shorthand,
// then clears Pad (it is never serialized). Called once after resolution.
func (s *Style) expandPad() {
	if s.Pad != 0 {
		if s.PadT == 0 {
			s.PadT = s.Pad
		}
		if s.PadR == 0 {
			s.PadR = s.Pad
		}
		if s.PadB == 0 {
			s.PadB = s.Pad
		}
		if s.PadL == 0 {
			s.PadL = s.Pad
		}
		s.Pad = 0
	}
}

func (s *Style) merge(o Style) {
	if o.Direction != "" {
		s.Direction = o.Direction
	}
	if o.Gap != 0 {
		s.Gap = o.Gap
	}
	if o.Pad != 0 {
		s.Pad = o.Pad
	}
	if o.PadT != 0 {
		s.PadT = o.PadT
	}
	if o.PadR != 0 {
		s.PadR = o.PadR
	}
	if o.PadB != 0 {
		s.PadB = o.PadB
	}
	if o.PadL != 0 {
		s.PadL = o.PadL
	}
	if o.Align != "" {
		s.Align = o.Align
	}
	if o.Justify != "" {
		s.Justify = o.Justify
	}
	if o.Grow {
		s.Grow = true
	}
	if o.Width != "" {
		s.Width = o.Width
	}
	if o.Height != "" {
		s.Height = o.Height
	}
	if o.BG != "" {
		s.BG = o.BG
	}
	if o.FG != "" {
		s.FG = o.FG
	}
	if o.FontSize != 0 {
		s.FontSize = o.FontSize
	}
	if o.FontWeight != 0 {
		s.FontWeight = o.FontWeight
	}
	if o.Radius != 0 {
		s.Radius = o.Radius
	}
}

// classStyles is the design-system table: how each standard-library class
// contributes to the resolved style. It is curated (FA owns std.CSS, so it knows
// what these classes mean) and extensible — covering the common layout containers
// and atoms. A class not listed contributes nothing.
var classStyles = map[string]Style{
	// horizontal containers
	"fa-row":             {Direction: "row", Align: "center", Gap: 8},
	"fa-post__header":    {Direction: "row", Gap: 10},
	"fa-post__actions":   {Direction: "row", Justify: "between"},
	"fa-vidctl":          {Direction: "row", Align: "center", Gap: 10, Pad: 8},
	"fa-engage":          {Direction: "row", Gap: 8},
	"fa-feedtabs":        {Direction: "row"},
	"fa-tabs":            {Direction: "row"},
	"fa-storybar":        {Direction: "row", Gap: 12, Pad: 12},
	"fa-catchips":        {Direction: "row", Gap: 8},
	"fa-roomctl":         {Direction: "row", Align: "center", Gap: 12, Justify: "center"},
	"fa-composer":        {Direction: "row", Gap: 10, Pad: 12},
	"fa-composer__bar":   {Direction: "row", Justify: "between", Align: "center"},
	"fa-composer__tools": {Direction: "row"},
	"fa-setrow":          {Direction: "row", Justify: "between", Align: "center", Pad: 12},
	"fa-bottomnav":       {Direction: "row", Justify: "between", Pad: 8},
	"fa-spacebar":        {Direction: "row", Align: "center", Gap: 10, Pad: 10},
	"fa-subrow":          {Direction: "row", Align: "center", Gap: 10, Pad: 10},
	"fa-sresult":         {Direction: "row", Align: "center", Gap: 10, Pad: 10},
	"fa-navrail__item":   {Direction: "row", Align: "center", Gap: 14, Pad: 10},
	"fa-roomhead":        {Direction: "row", Align: "center", Gap: 12, Pad: 12},
	"fa-topbar":          {Direction: "row", Align: "center", Justify: "between", Pad: 10},
	"fa-vcard__row":      {Direction: "row", Gap: 10},
	"fa-chatcompose":     {Direction: "row", Gap: 6, Pad: 8},

	// vertical containers
	"fa-stack":          {Direction: "column"},
	"fa-card":           {Direction: "column", Pad: 16, Radius: 12, Gap: 8},
	"fa-composer__main": {Direction: "column", Gap: 8, Grow: true},
	"fa-vcard__meta":    {Direction: "column"},
	"fa-rrcard":         {Direction: "column", Pad: 12, Radius: 16, Gap: 8},

	// atoms / appearance
	"fa-btn":               {Direction: "row", Align: "center", Pad: 8, Radius: 999, FontWeight: 600},
	"fa-btn--primary":      {BG: "#1d9bf0", FG: "#ffffff"},
	"fa-btn--secondary":    {FG: "#0f1419"},
	"fa-btn--danger":       {BG: "#f4212e", FG: "#ffffff"},
	"fa-badge":             {Pad: 4, Radius: 999, FontSize: 12, FontWeight: 700},
	"fa-pill":              {Pad: 6, Radius: 999, FontSize: 12, FontWeight: 700},
	"fa-tip":               {Direction: "row", Align: "center", Gap: 6, Pad: 8, Radius: 999, BG: "#ffc107", FontWeight: 800},
	"fa-post__name":        {FontWeight: 700},
	"fa-statcard__value":   {FontSize: 28, FontWeight: 800},
	"fa-channelhead__name": {FontSize: 24, FontWeight: 800},
}

// resolveStyle computes a node's Style from its tag, classes, and inline style.
// Precedence: tag default → each class (in order) → inline style (wins).
func resolveStyle(tag string, attrs map[string]string) *Style {
	s := &Style{}
	switch tag {
	case "button", "a":
		s.Direction, s.Align = "row", "center"
	}
	if cls := attrs["class"]; cls != "" {
		for _, c := range strings.Fields(cls) {
			if partial, ok := classStyles[c]; ok {
				s.merge(partial)
			}
		}
	}
	if inline := attrs["style"]; inline != "" {
		applyInlineStyle(s, inline)
	}
	s.expandPad()
	if s.isZero() {
		return nil
	}
	return s
}

// applyInlineStyle parses a CSS `style="…"` string into the neutral Style. Only a
// cross-platform-meaningful subset is read; the rest is ignored.
func applyInlineStyle(s *Style, inline string) {
	for _, decl := range strings.Split(inline, ";") {
		prop, val, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		val = strings.TrimSpace(val)
		switch prop {
		case "width":
			s.Width = val
		case "height":
			s.Height = val
		case "background", "background-color":
			s.BG = val
		case "color":
			s.FG = val
		case "padding":
			setPadding(s, val)
		case "padding-top":
			s.PadT = pxOf(val)
		case "padding-right":
			s.PadR = pxOf(val)
		case "padding-bottom":
			s.PadB = pxOf(val)
		case "padding-left":
			s.PadL = pxOf(val)
		case "border-radius":
			s.Radius = pxOf(val)
		case "font-size":
			s.FontSize = pxOf(val)
		case "font-weight":
			if n, err := strconv.Atoi(val); err == nil {
				s.FontWeight = n
			} else if val == "bold" {
				s.FontWeight = 700
			}
		case "gap":
			s.Gap = pxOf(val)
		case "flex-direction":
			if val == "row" || val == "column" {
				s.Direction = val
			}
		case "display":
			if val == "flex" && s.Direction == "" {
				s.Direction = "row" // CSS flex defaults to row
			}
		case "justify-content":
			s.Justify = mapJustify(val)
		case "align-items":
			s.Align = mapAlign(val)
		}
	}
}

// setPadding parses the CSS padding shorthand (1–4 values) into per-side padding,
// following CSS order: all | vert horiz | top horiz bottom | top right bottom left.
func setPadding(s *Style, val string) {
	p := strings.Fields(val)
	switch len(p) {
	case 1:
		n := pxOf(p[0])
		s.PadT, s.PadR, s.PadB, s.PadL = n, n, n, n
	case 2:
		v, h := pxOf(p[0]), pxOf(p[1])
		s.PadT, s.PadB, s.PadR, s.PadL = v, v, h, h
	case 3:
		t, h, b := pxOf(p[0]), pxOf(p[1]), pxOf(p[2])
		s.PadT, s.PadR, s.PadL, s.PadB = t, h, h, b
	case 4:
		s.PadT, s.PadR, s.PadB, s.PadL = pxOf(p[0]), pxOf(p[1]), pxOf(p[2]), pxOf(p[3])
	}
}

func pxOf(v string) int {
	v = strings.TrimSuffix(strings.TrimSpace(v), "px")
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

func mapJustify(v string) string {
	switch v {
	case "center":
		return "center"
	case "flex-end", "end":
		return "end"
	case "space-between":
		return "between"
	default:
		return "start"
	}
}

func mapAlign(v string) string {
	switch v {
	case "center":
		return "center"
	case "flex-end", "end":
		return "end"
	case "stretch":
		return "stretch"
	default:
		return "start"
	}
}

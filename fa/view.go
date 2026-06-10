package fa

import (
	"html"
	"strings"
)

// ViewNode is a platform-NEUTRAL representation of a rendered facet: a tree of
// abstract UI elements (box / text / button / image / input / link / icon) with
// their attributes, the facet id, and any action. It is the bridge that lets the
// SAME server-rendered facet drive ANY renderer:
//
//	web runtime   → DOM nodes
//	iOS runtime   → UIKit / SwiftUI views
//	Android runtime → Jetpack Compose
//
// The HTML path (Render) stays the web renderer; RenderTree is the neutral path a
// native client consumes over the same SSE wire protocol. data-facet-id still
// identifies each node, so surgical updates work identically on native.
type ViewNode struct {
	Kind     string            `json:"kind"`              // box|text|button|image|input|link|icon
	Tag      string            `json:"tag,omitempty"`     // original element (a layout hint)
	Attrs    map[string]string `json:"attrs,omitempty"`   // class, style, href, src, …
	Text     string            `json:"text,omitempty"`    // text content (for kind "text")
	FacetID  string            `json:"facetId,omitempty"` // data-facet-id (surgical update target)
	Action   string            `json:"action,omitempty"`  // data-action (tap/click → event)
	Style    *Style            `json:"style,omitempty"`   // server-resolved layout + appearance
	Children []*ViewNode       `json:"children,omitempty"`
}

// RenderTree renders a facet and returns it as a neutral ViewNode tree (instead
// of an HTML string). A native client requests this, applies it to native views,
// and forwards Action taps to the same /events endpoint — no browser, no HTML.
func (c *Compiled) RenderTree(facet string, data any) (*ViewNode, error) {
	h, err := c.Render(facet, data)
	if err != nil {
		return nil, err
	}
	return ParseView(string(h))
}

// ParseView converts an HTML fragment (the server's web rendering of a facet) to
// a neutral ViewNode tree. It is dependency-free — a small tokenizer over the
// well-formed HTML our templates emit — so the framework stays zero-dep. The
// single root element becomes the root node; multiple siblings are wrapped in a
// box.
func ParseView(fragment string) (*ViewNode, error) {
	p := &htmlParser{s: fragment}
	nodes := p.parseChildren("")
	switch len(nodes) {
	case 0:
		return &ViewNode{Kind: "box"}, nil
	case 1:
		return nodes[0], nil
	default:
		return &ViewNode{Kind: "box", Children: nodes}, nil
	}
}

// ── kind mapping ─────────────────────────────────────────────────────────────

var kindByTag = map[string]string{
	"button": "button",
	"a":      "link",
	"img":    "image",
	"input":  "input", "textarea": "input", "select": "input",
	"svg": "icon",
}

// Text-bearing inline/heading tags map to "text"; everything else is a "box"
// (a layout container). Native runtimes map box→stack/flex, text→Text/Label.
var textTags = map[string]bool{
	"span": true, "p": true, "strong": true, "b": true, "em": true, "i": true,
	"small": true, "label": true, "time": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "td": true, "th": true, "caption": true,
}

func kindFor(tag string) string {
	if k, ok := kindByTag[tag]; ok {
		return k
	}
	if textTags[tag] {
		return "text"
	}
	return "box"
}

var voidTags = map[string]bool{
	"img": true, "input": true, "br": true, "hr": true, "meta": true,
	"link": true, "source": true, "area": true, "col": true,
}

// ── minimal HTML tokenizer → tree ───────────────────────────────────────────

type htmlParser struct {
	s string
	i int
}

// parseChildren reads sibling nodes until it hits the closing tag of parent (or
// EOF), which it consumes. parent "" is the top level.
func (p *htmlParser) parseChildren(parent string) []*ViewNode {
	var nodes []*ViewNode
	for p.i < len(p.s) {
		if p.s[p.i] == '<' {
			if strings.HasPrefix(p.s[p.i:], "<!--") {
				if end := strings.Index(p.s[p.i:], "-->"); end >= 0 {
					p.i += end + 3
				} else {
					p.i = len(p.s)
				}
				continue
			}
			if p.i+1 < len(p.s) && p.s[p.i+1] == '/' {
				p.readCloseTag() // closes parent (well-formed input)
				return nodes
			}
			name, attrs, selfClose := p.readOpenTag()
			node := nodeFromTag(name, attrs)
			switch {
			case name == "svg":
				// Opaque icon: skip the SVG body to the matching close.
				if end := indexFold(p.s[p.i:], "</svg>"); end >= 0 {
					p.i += end + len("</svg>")
				} else {
					p.i = len(p.s)
				}
			case selfClose || voidTags[name]:
				// no children
			default:
				node.Children = p.parseChildren(name)
				// A text element wrapping only plain text (e.g. <span>Ada</span>)
				// collapses to a single text node carrying the string — what a
				// native Text/Label view needs.
				if node.Kind == "text" {
					if txt, ok := foldText(node.Children); ok {
						node.Text = txt
						node.Children = nil
					}
				}
			}
			nodes = append(nodes, node)
		} else {
			j := strings.IndexByte(p.s[p.i:], '<')
			var raw string
			if j < 0 {
				raw, p.i = p.s[p.i:], len(p.s)
			} else {
				raw, p.i = p.s[p.i:p.i+j], p.i+j
			}
			if t := strings.TrimSpace(raw); t != "" {
				nodes = append(nodes, &ViewNode{Kind: "text", Text: html.UnescapeString(t)})
			}
		}
	}
	return nodes
}

// readOpenTag parses "<name attr=… …>" or "… />" starting at p.i on '<'.
func (p *htmlParser) readOpenTag() (name string, attrs map[string]string, selfClose bool) {
	p.i++ // skip '<'
	name = p.readName()
	attrs = map[string]string{}
	for p.i < len(p.s) {
		p.skipSpace()
		if p.i >= len(p.s) {
			break
		}
		c := p.s[p.i]
		if c == '/' {
			selfClose = true
			p.i++
			continue
		}
		if c == '>' {
			p.i++
			break
		}
		an := p.readName()
		if an == "" {
			p.i++ // avoid stalling on stray char
			continue
		}
		av := ""
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == '=' {
			p.i++
			p.skipSpace()
			av = p.readAttrValue()
		}
		attrs[strings.ToLower(an)] = av
	}
	return name, attrs, selfClose
}

func (p *htmlParser) readCloseTag() {
	p.i += 2 // skip '</'
	p.readName()
	if j := strings.IndexByte(p.s[p.i:], '>'); j >= 0 {
		p.i += j + 1
	} else {
		p.i = len(p.s)
	}
}

func (p *htmlParser) readName() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '>' || c == '/' || c == '=' {
			break
		}
		p.i++
	}
	return strings.ToLower(p.s[start:p.i])
}

func (p *htmlParser) readAttrValue() string {
	if p.i >= len(p.s) {
		return ""
	}
	q := p.s[p.i]
	if q == '"' || q == '\'' {
		p.i++
		start := p.i
		for p.i < len(p.s) && p.s[p.i] != q {
			p.i++
		}
		v := p.s[start:p.i]
		if p.i < len(p.s) {
			p.i++ // closing quote
		}
		return html.UnescapeString(v)
	}
	// unquoted
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ' ' && p.s[p.i] != '>' && p.s[p.i] != '/' {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *htmlParser) skipSpace() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func nodeFromTag(name string, attrs map[string]string) *ViewNode {
	n := &ViewNode{Kind: kindFor(name), Tag: name}
	if len(attrs) > 0 {
		n.Attrs = attrs
	}
	if v, ok := attrs["data-facet-id"]; ok {
		n.FacetID = v
	}
	if v, ok := attrs["data-action"]; ok {
		n.Action = v
	}
	n.Style = resolveStyle(name, attrs)
	return n
}

// foldText returns the concatenated text if every child is a plain text leaf
// (so a text element can absorb it), else ok=false (it has nested elements).
func foldText(children []*ViewNode) (string, bool) {
	var b strings.Builder
	for _, c := range children {
		if c.Kind != "text" || len(c.Children) > 0 {
			return "", false
		}
		b.WriteString(c.Text)
	}
	return b.String(), true
}

// indexFold is strings.Index, case-insensitive.
func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

package runtime

import (
	"html"
	"regexp"
	"strings"
)

// The inline Markdown rules, applied after the text is HTML-escaped (so the
// captured groups are already safe). Code first, then links, then bold, then
// italic, then strikethrough — so `**` inside a code span is left alone and `*`
// does not eat `**`.
//
// A link's destination is admitted by scheme, not by escaping: `http(s)://`,
// `mailto:`, or a site-relative `/…` path. That is what keeps `javascript:` (and
// `data:`, and anything else) out of an href — a URL that does not match is not
// a link and stays the literal text the author typed. The text is already
// escaped, so `&` in a query string arrives as `&amp;`, which is the correct
// spelling inside an attribute; quotes arrived as `&#34;` and cannot close it.
var (
	mdCode   = regexp.MustCompile("`([^`]+)`")
	mdLink   = regexp.MustCompile(`\[([^\]]+)\]\(((?:https?://|mailto:|/)[^\s)]*)\)`)
	mdBold   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdItalic = regexp.MustCompile(`\*([^*]+)\*`)
	mdStrike = regexp.MustCompile(`~~([^~]+)~~`)
	mdOrder  = regexp.MustCompile(`^\d+\. `)
)

// markdownHTML renders a safe subset of Markdown to HTML: ``` code fences,
// `#`/`##`/`###` headings, `- ` and `1. ` lists, `> ` quotations, `---` rules,
// paragraphs, and inline code/links/bold/italic/strikethrough.
// The input is fully HTML-escaped first, so no raw HTML survives. The identical
// algorithm runs in assets/facet.js, so server-rendered first paint and client
// hydration produce the same markup.
func markdownHTML(src string) string {
	lines := strings.Split(src, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "```"):
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(lines[i], "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // closing fence
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("</code></pre>")
		case strings.HasPrefix(line, "### "):
			b.WriteString("<h3>" + mdInline(line[4:]) + "</h3>")
			i++
		case strings.HasPrefix(line, "## "):
			b.WriteString("<h2>" + mdInline(line[3:]) + "</h2>")
			i++
		case strings.HasPrefix(line, "# "):
			b.WriteString("<h1>" + mdInline(line[2:]) + "</h1>")
			i++
		case strings.HasPrefix(line, "> "):
			// A quotation: the same shape as the list arm below — a run of prefixed
			// lines collected into one wrapper. Its body is a paragraph, joined the
			// way the paragraph arm joins its lines, so the inside of a quote is made
			// of the same thing as the prose around it and <blockquote> gets the flow
			// content it is meant to hold rather than bare inline text.
			var quote []string
			for i < len(lines) && strings.HasPrefix(lines[i], "> ") {
				quote = append(quote, mdInline(lines[i][2:]))
				i++
			}
			b.WriteString("<blockquote><p>" + strings.Join(quote, "<br>") + "</p></blockquote>")
		case strings.HasPrefix(line, "- "):
			b.WriteString("<ul>")
			for i < len(lines) && strings.HasPrefix(lines[i], "- ") {
				b.WriteString("<li>" + mdInline(lines[i][2:]) + "</li>")
				i++
			}
			b.WriteString("</ul>")
		case mdOrder.MatchString(line):
			// An ordered list is the bulleted one with a number where the dash was.
			// The numbers the author wrote are dropped: <ol> counts for itself, so
			// `1. 1. 1.` and `1. 2. 3.` render the same, as they do everywhere.
			b.WriteString("<ol>")
			for i < len(lines) && mdOrder.MatchString(lines[i]) {
				b.WriteString("<li>" + mdInline(lines[i][len(mdOrder.FindString(lines[i])):]) + "</li>")
				i++
			}
			b.WriteString("</ol>")
		case strings.TrimSpace(line) == "---":
			b.WriteString("<hr>")
			i++
		case strings.TrimSpace(line) == "":
			i++
		default:
			var para []string
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && !mdBlockStart(lines[i]) {
				para = append(para, mdInline(lines[i]))
				i++
			}
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
		}
	}
	return b.String()
}

// mdBlockStart reports whether a line opens a new block (so paragraph collection
// stops before it).
func mdBlockStart(line string) bool {
	return strings.HasPrefix(line, "```") ||
		strings.HasPrefix(line, "# ") ||
		strings.HasPrefix(line, "## ") ||
		strings.HasPrefix(line, "### ") ||
		strings.HasPrefix(line, "> ") ||
		strings.HasPrefix(line, "- ") ||
		mdOrder.MatchString(line) ||
		strings.TrimSpace(line) == "---"
}

func mdInline(s string) string {
	s = html.EscapeString(s)
	s = mdCode.ReplaceAllString(s, "<code>$1</code>")
	// rel=noopener: an article's links point off-site as often as not, and a page
	// a reader lands on from here must not be handed a window back into this one.
	s = mdLink.ReplaceAllString(s, `<a href="$2" rel="noopener">$1</a>`)
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdItalic.ReplaceAllString(s, "<em>$1</em>")
	s = mdStrike.ReplaceAllString(s, "<del>$1</del>")
	return s
}

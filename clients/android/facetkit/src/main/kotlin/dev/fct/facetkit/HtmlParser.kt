package dev.fct.facetkit

/**
 * FacetHtmlParser converts an HTML fragment (the server's web rendering of a
 * facet, carried by SSE update events) into a neutral [ViewNode] tree. It is the
 * Kotlin port of Go's `fa.ParseView`, so the Android client applies the exact same
 * live updates the browser does.
 */
object FacetHtmlParser {
    fun parse(fragment: String): ViewNode {
        val p = Parser(fragment.toCharArray())
        val nodes = p.parseChildren()
        return when (nodes.size) {
            0 -> ViewNode(kind = "box")
            1 -> nodes[0]
            else -> ViewNode(kind = "box", children = nodes)
        }
    }

    private val kindByTag = mapOf(
        "button" to "button", "a" to "link", "img" to "image",
        "input" to "input", "textarea" to "input", "select" to "input", "svg" to "icon"
    )
    private val textTags = setOf(
        "span", "p", "strong", "b", "em", "i", "small", "label", "time",
        "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "caption"
    )
    private val voidTags = setOf("img", "input", "br", "hr", "meta", "link", "source", "area", "col")

    fun kindFor(tag: String): String = kindByTag[tag] ?: if (tag in textTags) "text" else "box"

    private class Parser(val chars: CharArray) {
        var i = 0

        fun parseChildren(): List<ViewNode> {
            val nodes = mutableListOf<ViewNode>()
            while (i < chars.size) {
                if (chars[i] == '<') {
                    if (matches("<!--")) {
                        val e = find("-->", i); i = if (e < 0) chars.size else e + 3; continue
                    }
                    if (i + 1 < chars.size && chars[i + 1] == '/') {
                        readCloseTag(); return nodes
                    }
                    val (name, attrs, selfClose) = readOpenTag()
                    var node = nodeFromTag(name, attrs)
                    when {
                        name == "svg" -> {
                            val e = findFold("</svg>", i); i = if (e < 0) chars.size else e + 6
                        }
                        selfClose || name in voidTags -> {}
                        else -> {
                            val kids = parseChildren()
                            val folded = foldText(kids)
                            node = if (node.kind == "text" && folded != null) {
                                node.copy(text = folded)
                            } else {
                                node.copy(children = kids.ifEmpty { null })
                            }
                        }
                    }
                    nodes.add(node)
                } else {
                    val start = i
                    while (i < chars.size && chars[i] != '<') i++
                    val raw = String(chars, start, i - start).trim()
                    if (raw.isNotEmpty()) nodes.add(ViewNode(kind = "text", text = htmlUnescape(raw)))
                }
            }
            return nodes
        }

        fun readOpenTag(): Triple<String, Map<String, String>, Boolean> {
            i++ // skip '<'
            val name = readName()
            val attrs = mutableMapOf<String, String>()
            var selfClose = false
            while (i < chars.size) {
                skipSpace()
                if (i >= chars.size) break
                val c = chars[i]
                if (c == '/') { selfClose = true; i++; continue }
                if (c == '>') { i++; break }
                val an = readName()
                if (an.isEmpty()) { i++; continue }
                var av = ""
                skipSpace()
                if (i < chars.size && chars[i] == '=') {
                    i++; skipSpace(); av = readAttrValue()
                }
                attrs[an.lowercase()] = av
            }
            return Triple(name, attrs, selfClose)
        }

        fun readCloseTag() {
            i += 2 // skip '</'
            readName()
            val e = find(">", i)
            i = if (e < 0) chars.size else e + 1
        }

        fun readName(): String {
            val start = i
            while (i < chars.size) {
                val c = chars[i]
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '>' || c == '/' || c == '=') break
                i++
            }
            return String(chars, start, i - start).lowercase()
        }

        fun readAttrValue(): String {
            if (i >= chars.size) return ""
            val q = chars[i]
            if (q == '"' || q == '\'') {
                i++
                val start = i
                while (i < chars.size && chars[i] != q) i++
                val v = String(chars, start, i - start)
                if (i < chars.size) i++
                return htmlUnescape(v)
            }
            val start = i
            while (i < chars.size && chars[i] != ' ' && chars[i] != '>' && chars[i] != '/') i++
            return String(chars, start, i - start)
        }

        fun skipSpace() {
            while (i < chars.size) {
                when (chars[i]) {
                    ' ', '\t', '\n', '\r' -> i++
                    else -> return
                }
            }
        }

        fun nodeFromTag(name: String, attrs: Map<String, String>): ViewNode = ViewNode(
            kind = kindFor(name),
            tag = name,
            attrs = attrs.ifEmpty { null },
            facetId = attrs["data-facet-id"],
            action = attrs["data-action"],
            style = StyleResolver.resolve(name, attrs),
        )

        fun foldText(children: List<ViewNode>): String? {
            val sb = StringBuilder()
            for (c in children) {
                if (c.kind != "text" || (c.children?.isNotEmpty() == true)) return null
                sb.append(c.text ?: "")
            }
            return sb.toString()
        }

        fun matches(s: String): Boolean {
            if (i + s.length > chars.size) return false
            for (k in s.indices) if (chars[i + k] != s[k]) return false
            return true
        }

        fun find(s: String, from: Int): Int {
            var k = from
            while (k + s.length <= chars.size) {
                var ok = true
                for (j in s.indices) if (chars[k + j] != s[j]) { ok = false; break }
                if (ok) return k
                k++
            }
            return -1
        }

        fun findFold(s: String, from: Int): Int {
            val low = s.lowercase()
            var k = from
            while (k + low.length <= chars.size) {
                var ok = true
                for (j in low.indices) if (chars[k + j].lowercaseChar() != low[j]) { ok = false; break }
                if (ok) return k
                k++
            }
            return -1
        }
    }

    fun htmlUnescape(s: String): String {
        if (!s.contains('&')) return s
        var out = s
        val named = mapOf(
            "&amp;" to "&", "&lt;" to "<", "&gt;" to ">", "&quot;" to "\"",
            "&#39;" to "'", "&#34;" to "\"", "&apos;" to "'", "&nbsp;" to " "
        )
        for ((k, v) in named) out = out.replace(k, v)
        return out
    }
}

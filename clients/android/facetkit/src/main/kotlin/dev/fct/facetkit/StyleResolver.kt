package dev.fct.facetkit

/**
 * StyleResolver resolves a node's [Style] from its tag, classes, and inline
 * style — so live SSE fragments (parsed on-device) get the SAME styling the server
 * resolves for the initial tree.
 *
 * NOTE: [classStyles] mirrors `fa/style.go` (the single source of truth). The
 * long-term plan is to push neutral, already-styled trees over SSE so this table
 * lives only on the server; until then this kept-in-sync copy makes updates
 * pixel-consistent with the web.
 */
object StyleResolver {

    fun resolve(tag: String, attrs: Map<String, String>): Style? {
        var s = Style()
        if (tag == "button" || tag == "a") s = s.copy(direction = "row", align = "center")
        attrs["class"]?.split(Regex("\\s+"))?.forEach { c ->
            classStyles[c]?.let { s = merge(s, it) }
        }
        attrs["style"]?.let { s = applyInline(s, it) }
        return if (s == Style()) null else s
    }

    private fun merge(a: Style, b: Style) = Style(
        direction = b.direction ?: a.direction,
        gap = b.gap ?: a.gap,
        pad = b.pad ?: a.pad,
        align = b.align ?: a.align,
        justify = b.justify ?: a.justify,
        grow = b.grow ?: a.grow,
        width = b.width ?: a.width,
        bg = b.bg ?: a.bg,
        fg = b.fg ?: a.fg,
        fontSize = b.fontSize ?: a.fontSize,
        fontWeight = b.fontWeight ?: a.fontWeight,
        radius = b.radius ?: a.radius,
    )

    private fun applyInline(start: Style, inline: String): Style {
        var s = start
        for (decl in inline.split(";")) {
            val idx = decl.indexOf(':')
            if (idx < 0) continue
            val prop = decl.substring(0, idx).trim().lowercase()
            val v = decl.substring(idx + 1).trim()
            s = when (prop) {
                "width" -> s.copy(width = v)
                "background", "background-color" -> s.copy(bg = v)
                "color" -> s.copy(fg = v)
                "padding" -> s.copy(pad = px(v))
                "border-radius" -> s.copy(radius = px(v))
                "font-size" -> s.copy(fontSize = px(v))
                "font-weight" -> s.copy(fontWeight = v.toIntOrNull() ?: if (v == "bold") 700 else null)
                "gap" -> s.copy(gap = px(v))
                "flex-direction" -> if (v == "row" || v == "column") s.copy(direction = v) else s
                "display" -> if (v == "flex" && s.direction == null) s.copy(direction = "row") else s
                "justify-content" -> s.copy(justify = mapJustify(v))
                "align-items" -> s.copy(align = mapAlign(v))
                else -> s
            }
        }
        return s
    }

    private fun px(v: String): Int {
        var t = v.trim().removeSuffix("px")
        val dot = t.indexOf('.')
        if (dot >= 0) t = t.substring(0, dot)
        return t.trim().toIntOrNull() ?: 0
    }

    private fun mapJustify(v: String) = when (v) {
        "center" -> "center"; "flex-end", "end" -> "end"; "space-between" -> "between"; else -> "start"
    }

    private fun mapAlign(v: String) = when (v) {
        "center" -> "center"; "flex-end", "end" -> "end"; "stretch" -> "stretch"; else -> "start"
    }

    // Mirror of fa/style.go classStyles.
    private val classStyles: Map<String, Style> = mapOf(
        "fa-row" to Style(direction = "row", align = "center", gap = 8),
        "fa-post__header" to Style(direction = "row", gap = 10),
        "fa-post__actions" to Style(direction = "row", justify = "between"),
        "fa-vidctl" to Style(direction = "row", align = "center", gap = 10, pad = 8),
        "fa-engage" to Style(direction = "row", gap = 8),
        "fa-feedtabs" to Style(direction = "row"),
        "fa-tabs" to Style(direction = "row"),
        "fa-storybar" to Style(direction = "row", gap = 12, pad = 12),
        "fa-catchips" to Style(direction = "row", gap = 8),
        "fa-roomctl" to Style(direction = "row", align = "center", gap = 12, justify = "center"),
        "fa-composer" to Style(direction = "row", gap = 10, pad = 12),
        "fa-composer__bar" to Style(direction = "row", justify = "between", align = "center"),
        "fa-composer__tools" to Style(direction = "row"),
        "fa-setrow" to Style(direction = "row", justify = "between", align = "center", pad = 12),
        "fa-bottomnav" to Style(direction = "row", justify = "between", pad = 8),
        "fa-spacebar" to Style(direction = "row", align = "center", gap = 10, pad = 10),
        "fa-subrow" to Style(direction = "row", align = "center", gap = 10, pad = 10),
        "fa-sresult" to Style(direction = "row", align = "center", gap = 10, pad = 10),
        "fa-navrail__item" to Style(direction = "row", align = "center", gap = 14, pad = 10),
        "fa-roomhead" to Style(direction = "row", align = "center", gap = 12, pad = 12),
        "fa-topbar" to Style(direction = "row", align = "center", justify = "between", pad = 10),
        "fa-vcard__row" to Style(direction = "row", gap = 10),
        "fa-chatcompose" to Style(direction = "row", gap = 6, pad = 8),
        "fa-stack" to Style(direction = "column"),
        "fa-card" to Style(direction = "column", pad = 16, radius = 12, gap = 8),
        "fa-composer__main" to Style(direction = "column", gap = 8, grow = true),
        "fa-vcard__meta" to Style(direction = "column"),
        "fa-rrcard" to Style(direction = "column", pad = 12, radius = 16, gap = 8),
        "fa-btn" to Style(direction = "row", align = "center", pad = 8, radius = 999, fontWeight = 600),
        "fa-btn--primary" to Style(bg = "#1d9bf0", fg = "#ffffff"),
        "fa-btn--secondary" to Style(fg = "#0f1419"),
        "fa-btn--danger" to Style(bg = "#f4212e", fg = "#ffffff"),
        "fa-badge" to Style(pad = 4, radius = 999, fontSize = 12, fontWeight = 700),
        "fa-pill" to Style(pad = 6, radius = 999, fontSize = 12, fontWeight = 700),
        "fa-tip" to Style(direction = "row", align = "center", gap = 6, pad = 8, radius = 999, bg = "#ffc107", fontWeight = 800),
        "fa-post__name" to Style(fontWeight = 700),
        "fa-statcard__value" to Style(fontSize = 28, fontWeight = 800),
        "fa-channelhead__name" to Style(fontSize = 24, fontWeight = 800),
    )
}

import Foundation

/// StyleResolver resolves a node's `Style` from its tag, classes, and inline style,
/// so SSE fragments parsed on-device get the SAME styling the server resolves for
/// the initial tree.
///
/// NOTE: `classStyles` mirrors `fa/style.go` (the single source of truth). The
/// long-term plan is to push neutral, already-styled trees over SSE so this table
/// lives only on the server.
enum StyleResolver {

    static func resolve(tag: String, attrs: [String: String]) -> Style? {
        var s = Style()
        if tag == "button" || tag == "a" { s.direction = "row"; s.align = "center" }
        if let cls = attrs["class"] {
            for c in cls.split(separator: " ") {
                if let partial = classStyles[String(c)] { merge(&s, partial) }
            }
        }
        if let inline = attrs["style"] { applyInline(&s, inline) }
        return isZero(s) ? nil : s
    }

    private static func isZero(_ s: Style) -> Bool {
        s.direction == nil && s.gap == nil && s.pad == nil && s.align == nil &&
        s.justify == nil && s.grow == nil && s.width == nil && s.bg == nil &&
        s.fg == nil && s.fontSize == nil && s.fontWeight == nil && s.radius == nil
    }

    private static func merge(_ s: inout Style, _ o: Style) {
        if let v = o.direction { s.direction = v }
        if let v = o.gap { s.gap = v }
        if let v = o.pad { s.pad = v }
        if let v = o.align { s.align = v }
        if let v = o.justify { s.justify = v }
        if let v = o.grow { s.grow = v }
        if let v = o.width { s.width = v }
        if let v = o.bg { s.bg = v }
        if let v = o.fg { s.fg = v }
        if let v = o.fontSize { s.fontSize = v }
        if let v = o.fontWeight { s.fontWeight = v }
        if let v = o.radius { s.radius = v }
    }

    private static func applyInline(_ s: inout Style, _ inline: String) {
        for decl in inline.split(separator: ";") {
            guard let colon = decl.firstIndex(of: ":") else { continue }
            let prop = decl[..<colon].trimmingCharacters(in: .whitespaces).lowercased()
            let val = decl[decl.index(after: colon)...].trimmingCharacters(in: .whitespaces)
            switch prop {
            case "width": s.width = val
            case "background", "background-color": s.bg = val
            case "color": s.fg = val
            case "padding": s.pad = px(val)
            case "border-radius": s.radius = px(val)
            case "font-size": s.fontSize = px(val)
            case "font-weight": s.fontWeight = Int(val) ?? (val == "bold" ? 700 : nil)
            case "gap": s.gap = px(val)
            case "flex-direction": if val == "row" || val == "column" { s.direction = val }
            case "display": if val == "flex", s.direction == nil { s.direction = "row" }
            case "justify-content": s.justify = mapJustify(val)
            case "align-items": s.align = mapAlign(val)
            default: break
            }
        }
    }

    private static func px(_ v: String) -> Int {
        var t = v.hasSuffix("px") ? String(v.dropLast(2)) : v
        if let dot = t.firstIndex(of: ".") { t = String(t[..<dot]) }
        return Int(t.trimmingCharacters(in: .whitespaces)) ?? 0
    }

    private static func mapJustify(_ v: String) -> String {
        switch v { case "center": return "center"; case "flex-end", "end": return "end"
        case "space-between": return "between"; default: return "start" }
    }
    private static func mapAlign(_ v: String) -> String {
        switch v { case "center": return "center"; case "flex-end", "end": return "end"
        case "stretch": return "stretch"; default: return "start" }
    }

    // Mirror of fa/style.go classStyles.
    static let classStyles: [String: Style] = [
        "fa-row": Style(direction: "row", gap: 8, align: "center"),
        "fa-post__header": Style(direction: "row", gap: 10),
        "fa-post__actions": Style(direction: "row", justify: "between"),
        "fa-vidctl": Style(direction: "row", gap: 10, pad: 8, align: "center"),
        "fa-engage": Style(direction: "row", gap: 8),
        "fa-feedtabs": Style(direction: "row"),
        "fa-tabs": Style(direction: "row"),
        "fa-storybar": Style(direction: "row", gap: 12, pad: 12),
        "fa-catchips": Style(direction: "row", gap: 8),
        "fa-roomctl": Style(direction: "row", gap: 12, align: "center", justify: "center"),
        "fa-composer": Style(direction: "row", gap: 10, pad: 12),
        "fa-composer__bar": Style(direction: "row", align: "center", justify: "between"),
        "fa-composer__tools": Style(direction: "row"),
        "fa-setrow": Style(direction: "row", pad: 12, align: "center", justify: "between"),
        "fa-bottomnav": Style(direction: "row", pad: 8, justify: "between"),
        "fa-spacebar": Style(direction: "row", gap: 10, pad: 10, align: "center"),
        "fa-subrow": Style(direction: "row", gap: 10, pad: 10, align: "center"),
        "fa-sresult": Style(direction: "row", gap: 10, pad: 10, align: "center"),
        "fa-navrail__item": Style(direction: "row", gap: 14, pad: 10, align: "center"),
        "fa-roomhead": Style(direction: "row", gap: 12, pad: 12, align: "center"),
        "fa-topbar": Style(direction: "row", pad: 10, align: "center", justify: "between"),
        "fa-vcard__row": Style(direction: "row", gap: 10),
        "fa-chatcompose": Style(direction: "row", gap: 6, pad: 8),
        "fa-stack": Style(direction: "column"),
        "fa-card": Style(direction: "column", gap: 8, pad: 16, radius: 12),
        "fa-composer__main": Style(direction: "column", gap: 8, grow: true),
        "fa-vcard__meta": Style(direction: "column"),
        "fa-rrcard": Style(direction: "column", gap: 8, pad: 12, radius: 16),
        "fa-btn": Style(direction: "row", pad: 8, align: "center", fontWeight: 600, radius: 999),
        "fa-btn--primary": Style(bg: "#1d9bf0", fg: "#ffffff"),
        "fa-btn--secondary": Style(fg: "#0f1419"),
        "fa-btn--danger": Style(bg: "#f4212e", fg: "#ffffff"),
        "fa-badge": Style(pad: 4, fontSize: 12, fontWeight: 700, radius: 999),
        "fa-pill": Style(pad: 6, fontSize: 12, fontWeight: 700, radius: 999),
        "fa-tip": Style(direction: "row", gap: 6, pad: 8, align: "center", bg: "#ffc107", fontWeight: 800, radius: 999),
        "fa-post__name": Style(fontWeight: 700),
        "fa-statcard__value": Style(fontSize: 28, fontWeight: 800),
        "fa-channelhead__name": Style(fontSize: 24, fontWeight: 800),
    ]
}

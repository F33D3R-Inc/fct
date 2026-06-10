import Foundation

/// FacetHTMLParser converts an HTML fragment (the server's web rendering of a
/// facet, as carried by SSE update events) into a neutral `ViewNode` tree. It is
/// the Swift port of Go's `fa.ParseView`, so the iOS client applies the exact same
/// live updates the browser does — a `replace` event's fragment becomes a native
/// subtree.
public enum FacetHTMLParser {
    public static func parse(_ fragment: String) -> ViewNode {
        var p = Parser(chars: Array(fragment))
        let nodes = p.parseChildren()
        switch nodes.count {
        case 0: return ViewNode(kind: "box")
        case 1: return nodes[0]
        default: return ViewNode(kind: "box", children: nodes)
        }
    }

    private static let kindByTag: [String: String] = [
        "button": "button", "a": "link", "img": "image",
        "input": "input", "textarea": "input", "select": "input", "svg": "icon"
    ]
    private static let textTags: Set<String> = [
        "span", "p", "strong", "b", "em", "i", "small", "label", "time",
        "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "caption"
    ]
    private static let voidTags: Set<String> = [
        "img", "input", "br", "hr", "meta", "link", "source", "area", "col"
    ]

    static func kind(for tag: String) -> String {
        if let k = kindByTag[tag] { return k }
        if textTags.contains(tag) { return "text" }
        return "box"
    }

    private struct Parser {
        let chars: [Character]
        var i = 0

        mutating func parseChildren() -> [ViewNode] {
            var nodes: [ViewNode] = []
            while i < chars.count {
                if chars[i] == "<" {
                    if matches("<!--") {
                        if let end = find("-->", from: i) { i = end + 3 } else { i = chars.count }
                        continue
                    }
                    if i + 1 < chars.count && chars[i + 1] == "/" {
                        readCloseTag() // closes the parent (well-formed input)
                        return nodes
                    }
                    let (name, attrs, selfClose) = readOpenTag()
                    var node = nodeFromTag(name, attrs)
                    if name == "svg" {
                        if let end = findFold("</svg>", from: i) { i = end + 6 } else { i = chars.count }
                    } else if selfClose || FacetHTMLParser.voidTags.contains(name) {
                        // no children
                    } else {
                        let kids = parseChildren()
                        if node.kind == "text", let folded = foldText(kids) {
                            node.text = folded
                        } else {
                            node.children = kids.isEmpty ? nil : kids
                        }
                    }
                    nodes.append(node)
                } else {
                    let start = i
                    while i < chars.count && chars[i] != "<" { i += 1 }
                    let raw = String(chars[start..<i]).trimmingCharacters(in: .whitespacesAndNewlines)
                    if !raw.isEmpty {
                        nodes.append(ViewNode(kind: "text", text: raw.htmlUnescaped()))
                    }
                }
            }
            return nodes
        }

        mutating func readOpenTag() -> (String, [String: String], Bool) {
            i += 1 // skip '<'
            let name = readName()
            var attrs: [String: String] = [:]
            var selfClose = false
            while i < chars.count {
                skipSpace()
                if i >= chars.count { break }
                let c = chars[i]
                if c == "/" { selfClose = true; i += 1; continue }
                if c == ">" { i += 1; break }
                let an = readName()
                if an.isEmpty { i += 1; continue }
                var av = ""
                skipSpace()
                if i < chars.count && chars[i] == "=" {
                    i += 1
                    skipSpace()
                    av = readAttrValue()
                }
                attrs[an.lowercased()] = av
            }
            return (name, attrs, selfClose)
        }

        mutating func readCloseTag() {
            i += 2 // skip '</'
            _ = readName()
            if let end = find(">", from: i) { i = end + 1 } else { i = chars.count }
        }

        mutating func readName() -> String {
            let start = i
            while i < chars.count {
                let c = chars[i]
                if c == " " || c == "\t" || c == "\n" || c == "\r" || c == ">" || c == "/" || c == "=" { break }
                i += 1
            }
            return String(chars[start..<i]).lowercased()
        }

        mutating func readAttrValue() -> String {
            guard i < chars.count else { return "" }
            let q = chars[i]
            if q == "\"" || q == "'" {
                i += 1
                let start = i
                while i < chars.count && chars[i] != q { i += 1 }
                let v = String(chars[start..<i])
                if i < chars.count { i += 1 }
                return v.htmlUnescaped()
            }
            let start = i
            while i < chars.count && chars[i] != " " && chars[i] != ">" && chars[i] != "/" { i += 1 }
            return String(chars[start..<i])
        }

        mutating func skipSpace() {
            while i < chars.count {
                switch chars[i] {
                case " ", "\t", "\n", "\r": i += 1
                default: return
                }
            }
        }

        func nodeFromTag(_ name: String, _ attrs: [String: String]) -> ViewNode {
            var n = ViewNode(kind: FacetHTMLParser.kind(for: name), tag: name)
            if !attrs.isEmpty { n.attrs = attrs }
            n.facetId = attrs["data-facet-id"]
            n.action = attrs["data-action"]
            return n
        }

        func foldText(_ children: [ViewNode]) -> String? {
            var s = ""
            for c in children {
                if c.kind != "text" || (c.children?.isEmpty == false) { return nil }
                s += c.text ?? ""
            }
            return s
        }

        // MARK: char helpers

        func matches(_ s: String) -> Bool {
            let t = Array(s)
            guard i + t.count <= chars.count else { return false }
            for k in 0..<t.count where chars[i + k] != t[k] { return false }
            return true
        }

        func find(_ s: String, from: Int) -> Int? {
            let t = Array(s)
            var k = from
            while k + t.count <= chars.count {
                var ok = true
                for j in 0..<t.count where chars[k + j] != t[j] { ok = false; break }
                if ok { return k }
                k += 1
            }
            return nil
        }

        func findFold(_ s: String, from: Int) -> Int? {
            let t = Array(s.lowercased())
            var k = from
            while k + t.count <= chars.count {
                var ok = true
                for j in 0..<t.count where Character(chars[k + j].lowercased()) != t[j] { ok = false; break }
                if ok { return k }
                k += 1
            }
            return nil
        }
    }
}

extension String {
    /// Decodes the handful of HTML entities our templates emit.
    func htmlUnescaped() -> String {
        guard contains("&") else { return self }
        var s = self
        let named = ["&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": "\"",
                     "&#39;": "'", "&#34;": "\"", "&apos;": "'", "&nbsp;": "\u{00a0}"]
        for (k, v) in named { s = s.replacingOccurrences(of: k, with: v) }
        return s
    }
}

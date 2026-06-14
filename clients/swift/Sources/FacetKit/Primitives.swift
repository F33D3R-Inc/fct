import CryptoKit
import Foundation

/// FacetMeta is one manifest entry's per-primitive runtime rules — the native
/// mirror of the web runtime's manifest registry: the kind, the stream `window:`,
/// the signal `ttl:`, and the client render body for vault (`decrypt:`) / media
/// (`source:`). The client fetches `/manifest.json` at start and keys these by
/// facet name.
public struct FacetMeta: Decodable {
    public let name: String
    public let kind: String
    public let window: String?
    public let ttl: String?
    public let client: String?

    var windowCount: Int { Int(window ?? "") ?? 0 }
    var ttlMs: Int { FacetPrimitives.goDurationMs(ttl ?? "") }
}

struct FacetManifest: Decodable {
    let facets: [FacetMeta]
}

/// The client side of the primitive taxonomy, mirroring fa-runtime.js: facet-id
/// → name resolution, Go duration parsing, the client render-body engine (escaped
/// `{field}` interpolation plus `{if}`/`{for}`) for trusted bodies, vault envelope
/// decryption, and media tag normalization. Pure functions, shared by
/// FacetClient and the tests.
enum FacetPrimitives {
    /// The facet name of a facet-id instance: "LikeButton:post:42" → "LikeButton".
    static func facetName(_ id: String) -> String {
        if let i = id.firstIndex(of: ":") { return String(id[..<i]) }
        return id
    }

    /// Parses the simple Go durations the compiler accepts (200ms, 5s, 2m, 1h).
    static func goDurationMs(_ s: String) -> Int {
        for (suffix, factor) in [("ms", 1.0), ("s", 1000.0), ("m", 60_000.0), ("h", 3_600_000.0)] {
            if s.hasSuffix(suffix) {
                guard let n = Double(s.dropLast(suffix.count)) else { return 0 }
                return Int(n * factor)
            }
        }
        return 0
    }

    /// HTML-escapes a value before it is substituted into a client render body —
    /// decrypted plaintext can never inject elements (web parity: the runtime
    /// escapes, then parses).
    static func escape(_ s: String) -> String {
        var out = ""
        out.reserveCapacity(s.count)
        for c in s {
            switch c {
            case "&": out += "&amp;"
            case "<": out += "&lt;"
            case ">": out += "&gt;"
            case "\"": out += "&quot;"
            case "'": out += "&#39;"
            default: out.append(c)
            }
        }
        return out
    }

    /// Renders a TRUSTED client body (the compiled manifest's decrypt:/source:
    /// template) against UNTRUSTED values. Interpolated values are HTML-ESCAPED;
    /// literal text is not. Supports {field}/{a.b}, {if expr}…{else}…{end} and
    /// {for v in path}…{end}. The exact behavior of the web runtime's fill().
    ///
    /// Values are structured (String / Bool / numbers / [Any] / [String: Any]),
    /// e.g. a vault's parsed JSON plaintext, so loops/conditions see real arrays
    /// and nested objects.
    static func fill(_ body: String, _ values: [String: Any]) -> String {
        let toks = tokenizeTpl(body)
        var i = 0
        let nodes = parseBlock(toks, &i, stopElse: false)
        return renderTpl(nodes, values)
    }

    // A parsed client-body node. `.tpl`/`.expr` carry text/interpolation; `.cond`
    // and `.loop` are the nested control structures.
    indirect enum TplNode {
        case text(String)
        case interp(String)
        case cond(expr: String, then: [TplNode], els: [TplNode])
        case loop(v: String, iter: String, body: [TplNode])
    }

    private enum Tok {
        case text(String), interp(String), ifTok(String), elseTok, endTok, forTok(v: String, iter: String)
    }

    private static func tokenizeTpl(_ body: String) -> [Tok] {
        var toks: [Tok] = []
        var rest = Substring(body)
        while let open = rest.firstIndex(of: "{") {
            if open != rest.startIndex { toks.append(.text(String(rest[..<open]))) }
            let afterOpen = rest.index(after: open)
            guard let close = rest[afterOpen...].firstIndex(of: "}") else {
                toks.append(.text(String(rest[open...])))
                return toks
            }
            let inner = rest[afterOpen..<close].trimmingCharacters(in: .whitespaces)
            if inner.hasPrefix("if ") {
                toks.append(.ifTok(String(inner.dropFirst(3)).trimmingCharacters(in: .whitespaces)))
            } else if inner == "else" {
                toks.append(.elseTok)
            } else if inner == "end" {
                toks.append(.endTok)
            } else if inner.hasPrefix("for ") {
                let parts = inner.dropFirst(4).components(separatedBy: " in ")
                if parts.count == 2 {
                    toks.append(.forTok(v: parts[0].trimmingCharacters(in: .whitespaces),
                                        iter: parts[1].trimmingCharacters(in: .whitespaces)))
                } else {
                    toks.append(.text("{\(inner)}"))
                }
            } else {
                toks.append(.interp(inner))
            }
            rest = rest[rest.index(after: close)...]
        }
        if !rest.isEmpty { toks.append(.text(String(rest))) }
        return toks
    }

    private static func parseBlock(_ toks: [Tok], _ i: inout Int, stopElse: Bool) -> [TplNode] {
        var nodes: [TplNode] = []
        while i < toks.count {
            switch toks[i] {
            case .endTok: return nodes
            case .elseTok where stopElse: return nodes
            case .text(let s): i += 1; nodes.append(.text(s))
            case .interp(let e): i += 1; nodes.append(.interp(e))
            case .ifTok(let e):
                i += 1
                let then = parseBlock(toks, &i, stopElse: true)
                var els: [TplNode] = []
                if i < toks.count, case .elseTok = toks[i] { i += 1; els = parseBlock(toks, &i, stopElse: false) }
                if i < toks.count, case .endTok = toks[i] { i += 1 }
                nodes.append(.cond(expr: e, then: then, els: els))
            case .forTok(let v, let iter):
                i += 1
                let body = parseBlock(toks, &i, stopElse: false)
                if i < toks.count, case .endTok = toks[i] { i += 1 }
                nodes.append(.loop(v: v, iter: iter, body: body))
            case .elseTok: i += 1 // stray else at this level — ignore
            }
        }
        return nodes
    }

    private static func renderTpl(_ nodes: [TplNode], _ scope: [String: Any]) -> String {
        var out = ""
        for n in nodes {
            switch n {
            case .text(let s): out += s
            case .interp(let e):
                if let v = evalExpr(e, scope) { out += escape(stringify(v)) }
            case .cond(let e, let then, let els):
                out += renderTpl(truthy(evalExpr(e, scope)) ? then : els, scope)
            case .loop(let v, let iter, let body):
                if let arr = evalExpr(iter, scope) as? [Any] {
                    for item in arr {
                        var s2 = scope; s2[v] = item
                        out += renderTpl(body, s2)
                    }
                }
            }
        }
        return out
    }

    // evalExpr supports `lhs OP rhs` comparisons, a leading `!`, and bare operands
    // (literals or dotted paths). Mirrors the web runtime.
    static func evalExpr(_ expr: String, _ scope: [String: Any]) -> Any? {
        let e = expr.trimmingCharacters(in: .whitespaces)
        for op in ["==", "!=", "<=", ">=", "<", ">"] { // two-char first
            if let r = e.range(of: op) {
                let lhs = String(e[..<r.lowerBound])
                let rhs = String(e[r.upperBound...])
                return compare(op, operand(lhs, scope), operand(rhs, scope))
            }
        }
        if e.hasPrefix("!") { return !truthy(evalExpr(String(e.dropFirst()), scope)) }
        return operand(e, scope)
    }

    private static func operand(_ s0: String, _ scope: [String: Any]) -> Any? {
        let s = s0.trimmingCharacters(in: .whitespaces)
        if s == "true" { return true }
        if s == "false" { return false }
        if let d = Double(s) { return d }
        if s.count >= 2, let f = s.first, (f == "\"" || f == "'"), s.last == f {
            return String(s.dropFirst().dropLast())
        }
        var cur: Any? = scope
        for seg in s.split(separator: ".") {
            guard let dict = cur as? [String: Any] else { return nil }
            cur = dict[String(seg)]
        }
        return cur
    }

    private static func compare(_ op: String, _ a: Any?, _ b: Any?) -> Bool {
        if op == "==" { return stringify(a) == stringify(b) }
        if op == "!=" { return stringify(a) != stringify(b) }
        let na = numberOf(a), nb = numberOf(b)
        if let x = na, let y = nb {
            switch op { case "<": return x < y; case "<=": return x <= y; case ">": return x > y; default: return x >= y }
        }
        let sa = stringify(a), sb = stringify(b)
        switch op { case "<": return sa < sb; case "<=": return sa <= sb; case ">": return sa > sb; default: return sa >= sb }
    }

    // truthy mirrors Go template emptiness: nil/false/0/""/[]/{} are falsy.
    static func truthy(_ v: Any?) -> Bool {
        switch v {
        case nil: return false
        case let b as Bool: return b
        case let d as Double: return d != 0
        case let i as Int: return i != 0
        case let s as String: return !s.isEmpty
        case let a as [Any]: return !a.isEmpty
        case let m as [String: Any]: return !m.isEmpty
        default: return true
        }
    }

    private static func numberOf(_ v: Any?) -> Double? {
        switch v {
        case let d as Double: return d
        case let i as Int: return Double(i)
        case let s as String: return Double(s)
        case let b as Bool: return b ? 1 : 0
        default: return nil
        }
    }

    private static func stringify(_ v: Any?) -> String {
        switch v {
        case nil: return ""
        case let s as String: return s
        case let b as Bool: return b ? "true" : "false"
        case let d as Double: return d == d.rounded() ? String(Int(d)) : String(d)
        case let i as Int: return String(i)
        default: return "\(v!)"
        }
    }

    static func isFieldName(_ s: String) -> Bool {
        guard let first = s.first, first == "_" || first.isLetter else { return false }
        return s.allSatisfy { $0 == "_" || $0.isLetter || $0.isNumber }
    }

    /// A signal payload key that is safe to set as a data-* attribute (no
    /// data-action / data-fa-* hijack) — mirrors the web runtime's guard.
    static func safeSignalKey(_ k: String) -> Bool {
        if k == "action" || k.lowercased().hasPrefix("fa") { return false }
        return isFieldName(k)
    }

    /// Decrypts a vault envelope — base64 of 12-byte IV ‖ ciphertext ‖ tag,
    /// AES-GCM — with a key that exists only on this device. Returns nil (and the
    /// envelope stays put) on any failure: never render garbage.
    static func decryptEnvelope(_ b64: String, key: SymmetricKey) -> String? {
        guard let data = Data(base64Encoded: b64), data.count >= 28,
              let box = try? AES.GCM.SealedBox(combined: data),
              let pt = try? AES.GCM.open(box, using: key) else { return nil }
        return String(data: pt, encoding: .utf8)
    }

    /// Normalizes a media `source:` body's transport tags (<hls>/<dash>) to
    /// <video>, the element the parser and player understand.
    static func normalizeMedia(_ body: String) -> String {
        var s = body
        for tag in ["hls", "dash"] {
            s = s.replacingOccurrences(of: "<\(tag) ", with: "<video ")
            s = s.replacingOccurrences(of: "<\(tag)/", with: "<video/")
            s = s.replacingOccurrences(of: "<\(tag)>", with: "<video>")
            s = s.replacingOccurrences(of: "</\(tag)>", with: "</video>")
        }
        return s
    }
}

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
/// → name resolution, Go duration parsing, the escaped `{field}` interpolation
/// for trusted client render bodies, vault envelope decryption, and media tag
/// normalization. Pure functions, shared by FacetClient and the tests.
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

    /// Substitutes `{field}` holes in a TRUSTED body (the compiled manifest's
    /// client render body) with HTML-ESCAPED values. Field interpolation only —
    /// an unknown field renders empty; a non-field hole ({if x} …) is left
    /// literal. Exactly the web runtime's fill().
    static func fill(_ body: String, _ values: [String: String]) -> String {
        var out = ""
        var rest = Substring(body)
        while let open = rest.firstIndex(of: "{") {
            out += rest[..<open]
            let afterOpen = rest.index(after: open)
            guard let close = rest[afterOpen...].firstIndex(of: "}") else {
                out += rest[open...]
                return out
            }
            let name = rest[afterOpen..<close].trimmingCharacters(in: .whitespaces)
            if isFieldName(name) {
                out += escape(values[name] ?? "")
            } else {
                out += rest[open...close]
            }
            rest = rest[rest.index(after: close)...]
        }
        out += rest
        return out
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

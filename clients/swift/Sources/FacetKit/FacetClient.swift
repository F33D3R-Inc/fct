import Foundation
import SwiftUI
import CryptoKit

/// A server-pushed event, decoded from the SSE stream. Mirrors the web runtime:
/// `op` is replace/append/prepend/remove (or `_conn` for the connection id),
/// `facetId` targets the node, `fragment` is the new HTML (parsed to a ViewNode).
struct FacetEvent: Decodable {
    let op: String?
    let facet_id: String?
    let fragment: String?   // styled neutral-tree JSON (native); also the signed bytes
    let hmac: String?
    let conn: String?
    let key: String?        // signing key, on the _conn frame
}

/// FacetClient is the Apple-platform FA runtime. It is the analogue of
/// `fa-runtime.js`: it loads a screen as a neutral view tree, holds one SSE
/// connection, applies pushed updates by facet id, and forwards taps to the single
/// `/events` endpoint. It owns NO application logic — the server decides what every
/// action does and pushes the result back.
///
/// ```swift
/// let client = FacetClient(baseURL: URL(string: "https://app.example.com")!)
/// // in a view:  FacetScreen(client: client, route: "/")
/// ```
@MainActor
public final class FacetClient: ObservableObject {
    /// The current screen tree; SwiftUI re-renders when it changes.
    @Published public private(set) var tree: ViewNode?
    @Published public private(set) var title: String = ""
    @Published public private(set) var connected = false

    public let baseURL: URL
    private let session: URLSession
    private var connID: String?
    private var signKey: SymmetricKey?
    private var sseTask: Task<Void, Never>?
    private var pending: [(String, [String: String])] = [] // actions fired before connID

    // Per-primitive runtime state (mirrors the web runtime's manifest registry).
    private var registry: [String: FacetMeta] = [:]            // facet name → rules
    private var vaultKeys: [String: SymmetricKey] = [:]        // vault name → AES-GCM key
    private var signalTasks: [String: Task<Void, Never>] = [:] // facet-id → ttl expiry
    private var signalAttrs: [String: [String]] = [:]          // facet-id → applied data-* keys

    /// Called on every relayed `signal` event with (facet id, payload) — the
    /// programmatic hook for ephemeral peer state (typing, presence) when a tree
    /// attribute isn't enough.
    public var onSignal: ((String, [String: String]) -> Void)?

    public init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
    }

    /// Loads `route` as a native view tree and opens the live connection.
    public func start(route: String) {
        Task { await loadManifest() }
        Task { await loadScreen(route) }
        openSSE()
    }

    public func stop() {
        sseTask?.cancel()
        sseTask = nil
        connected = false
    }

    // MARK: - Navigation / initial render

    /// Fetches a route as a neutral tree (FA-Native) and shows it. Client-side
    /// navigation: the SSE connection is untouched, so live facets persist.
    public func navigate(to route: String) {
        Task { await loadScreen(route) }
    }

    private func loadScreen(_ route: String) async {
        var req = URLRequest(url: url(route))
        req.setValue("1", forHTTPHeaderField: "FA-Native")
        do {
            let (data, _) = try await session.data(for: req)
            let screen = try JSONDecoder().decode(ScreenResponse.self, from: data)
            self.tree = postProcess(screen.tree)
            self.title = screen.title ?? ""
        } catch {
            // leave the previous tree in place on error
        }
    }

    /// Fetches the compiled manifest and indexes its per-primitive rules (stream
    /// window, signal ttl, vault/media client bodies) by facet name — the same
    /// registry the web runtime builds from /manifest.json.
    private func loadManifest() async {
        guard let (data, _) = try? await session.data(from: url("/manifest.json")),
              let m = try? JSONDecoder().decode(FacetManifest.self, from: data) else { return }
        for f in m.facets { registry[f.name] = f }
        if let t = tree { tree = postProcess(t) } // rules may unlock vault/media nodes
    }

    /// Registers a vault's AES-GCM key (hex) and decrypts any visible envelopes.
    /// The key exists only on this device — it is never sent to the server, which
    /// is the vault guarantee (the native mirror of web `fa.vault.key`).
    public func vaultKey(_ facet: String, hexKey: String) {
        guard let bytes = Self.hexToBytes(hexKey) else { return }
        vaultKeys[facet] = SymmetricKey(data: bytes)
        if let t = tree { tree = postProcess(t) }
    }

    // MARK: - SSE

    private func openSSE() {
        sseTask?.cancel()
        sseTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                do {
                    var req = URLRequest(url: self.url("/sse"))
                    req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    req.setValue("1", forHTTPHeaderField: "FA-Native") // get styled trees, not HTML
                    let (bytes, _) = try await self.session.bytes(for: req)
                    await self.setConnected(true)
                    var dataLines: [String] = []
                    for try await line in bytes.lines {
                        if line.isEmpty {
                            if !dataLines.isEmpty {
                                await self.handleFrame(dataLines.joined(separator: "\n"))
                                dataLines.removeAll()
                            }
                        } else if line.hasPrefix("data:") {
                            dataLines.append(String(line.dropFirst(5)).trimmingCharacters(in: .whitespaces))
                        }
                    }
                } catch {
                    // fallthrough to reconnect
                }
                await self.setConnected(false)
                self.connID = nil
                try? await Task.sleep(nanoseconds: 1_000_000_000)
            }
        }
    }

    private func setConnected(_ v: Bool) { connected = v }

    private func handleFrame(_ json: String) {
        guard let data = json.data(using: .utf8),
              let ev = try? JSONDecoder().decode(FacetEvent.self, from: data) else { return }
        if ev.op == "_conn" {
            connID = ev.conn
            if let k = ev.key, let bytes = Self.hexToBytes(k) { signKey = SymmetricKey(data: bytes) }
            flushPending()
            return
        }
        guard verify(ev) else { return } // drop any event we cannot authenticate
        apply(ev)
    }

    /// Verifies an event's HMAC-SHA256 over op\0facet_id\0fragment, matching the
    /// server's signing (and the web runtime). Without a key yet, accept (parity
    /// with the web client when no key is present).
    private func verify(_ ev: FacetEvent) -> Bool {
        guard let signKey else { return true }
        guard let hmacHex = ev.hmac else { return false }
        var msg = Data()
        msg.append(Data((ev.op ?? "").utf8)); msg.append(0)
        msg.append(Data((ev.facet_id ?? "").utf8)); msg.append(0)
        msg.append(Data((ev.fragment ?? "").utf8))
        let mac = HMAC<SHA256>.authenticationCode(for: msg, using: signKey)
        let computed = mac.map { String(format: "%02x", $0) }.joined()
        return computed == hmacHex
    }

    private func apply(_ ev: FacetEvent) {
        if ev.op == "signal" { applySignal(ev); return }
        guard let tree, let id = ev.facet_id else { return }
        let node = ev.fragment.flatMap { decodeNode($0) }
        switch ev.op {
        case "replace":
            if let node { self.tree = postProcess(tree.replacingFacet(id, with: node)) }
        case "append", "prepend":
            guard let node else { return }
            let prepend = ev.op == "prepend"
            var next = tree.insertingChild(into: id, node, prepend: prepend)
            // stream `window:` — cap retained children, trimming the opposite end
            if let meta = registry[FacetPrimitives.facetName(id)],
               meta.kind == "stream", meta.windowCount > 0 {
                next = next.trimmingChildren(of: id, max: meta.windowCount, dropFromStart: !prepend)
            }
            self.tree = postProcess(next)
        case "remove":
            self.tree = tree.removingFacet(id)
        default:
            break
        }
    }

    // MARK: - signal (ephemeral peer state)

    /// Applies a relayed signal: the payload lands as data-* attributes (plus the
    /// fa-signal-live class) on every node whose data-fa-signal matches the
    /// signal's facet id or name, and reverts after the declared `ttl:` — exactly
    /// the web runtime. `onSignal` fires regardless, for programmatic consumers.
    private func applySignal(_ ev: FacetEvent) {
        guard let id = ev.facet_id else { return }
        var payload: [String: String] = [:]
        if let d = ev.fragment?.data(using: .utf8),
           let p = try? JSONDecoder().decode([String: String].self, from: d) {
            payload = p
        }
        onSignal?(id, payload)

        let name = FacetPrimitives.facetName(id)
        var attrsToSet: [String: String] = [:]
        for (k, v) in payload where FacetPrimitives.safeSignalKey(k) {
            attrsToSet["data-" + k.lowercased()] = v
        }
        if let tree {
            self.tree = tree.mapping { node in
                guard let want = node.attrs?["data-fa-signal"], want == id || want == name else { return node }
                var n = node
                var a = n.attrs ?? [:]
                for (ak, av) in attrsToSet { a[ak] = av }
                a["class"] = Self.addingClass(a["class"], "fa-signal-live")
                n.attrs = a
                return n
            }
        }
        signalAttrs[id] = Array(attrsToSet.keys)

        signalTasks[id]?.cancel()
        let ttl = registry[name]?.ttlMs ?? 0
        guard ttl > 0 else { return }
        signalTasks[id] = Task { [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(ttl) * 1_000_000)
            guard !Task.isCancelled else { return }
            self?.expireSignal(id)
        }
    }

    private func expireSignal(_ id: String) {
        let name = FacetPrimitives.facetName(id)
        let keys = signalAttrs.removeValue(forKey: id) ?? []
        guard let tree else { return }
        self.tree = tree.mapping { node in
            guard let want = node.attrs?["data-fa-signal"], want == id || want == name else { return node }
            var n = node
            var a = n.attrs ?? [:]
            for k in keys { a[k] = nil }
            a["class"] = Self.removingClass(a["class"], "fa-signal-live")
            n.attrs = a
            return n
        }
    }

    private static func addingClass(_ cls: String?, _ token: String) -> String {
        var parts = (cls ?? "").split(separator: " ").map(String.init)
        if !parts.contains(token) { parts.append(token) }
        return parts.joined(separator: " ")
    }

    private static func removingClass(_ cls: String?, _ token: String) -> String? {
        let parts = (cls ?? "").split(separator: " ").map(String.init).filter { $0 != token }
        return parts.isEmpty ? nil : parts.joined(separator: " ")
    }

    // MARK: - vault decrypt + media mount (client-rendered primitives)

    /// Applies the client-rendered primitives to the tree: decrypts ready vault
    /// envelopes and mounts media players. Runs after every tree change, when the
    /// manifest arrives, and when a vault key is registered. Already-processed
    /// nodes are skipped via marker attributes, so the map is cheap.
    private func postProcess(_ tree: ViewNode) -> ViewNode {
        tree.mapping { node in
            if let v = self.vaultNode(node) { return v }
            if let m = self.mediaNode(node) { return m }
            return node
        }
    }

    /// Decrypts one vault node: data-fa-vault names the primitive, the manifest
    /// carries its decrypt: body (there is NO server template — the structural
    /// guarantee), data-fa-envelope is base64(IV ‖ ciphertext ‖ tag). The
    /// decrypted values are escaped, filled into the body, and parsed into the
    /// node's children. Any failure leaves the node untouched.
    private func vaultNode(_ node: ViewNode) -> ViewNode? {
        guard let name = node.attrs?["data-fa-vault"],
              let meta = registry[name], meta.kind == "vault",
              let body = meta.client, !body.isEmpty,
              let env = node.attrs?["data-fa-envelope"],
              node.attrs?["data-fa-decrypted"] != env,
              let key = vaultKeys[name],
              let plaintext = FacetPrimitives.decryptEnvelope(env, key: key) else { return nil }
        var values = ["plaintext": plaintext]
        if let d = plaintext.data(using: .utf8),
           let obj = (try? JSONSerialization.jsonObject(with: d)) as? [String: Any] {
            for (k, v) in obj { values[k] = "\(v)" } // JSON plaintext exposes its fields
        }
        var n = node
        n.children = [FacetHTMLParser.parse(FacetPrimitives.fill(body, values))]
        var a = n.attrs ?? [:]
        a["data-fa-decrypted"] = env
        n.attrs = a
        return n
    }

    /// Mounts one media node: the manifest's source: body, holes filled from the
    /// node's data-* attributes, <hls>/<dash> normalized to <video>, parsed, and
    /// marked kind "media" so the renderer shows a real player.
    private func mediaNode(_ node: ViewNode) -> ViewNode? {
        guard let name = node.attrs?["data-fa-media"],
              let meta = registry[name], meta.kind == "media",
              let body = meta.client, !body.isEmpty,
              node.attrs?["data-fa-mounted"] == nil else { return nil }
        var values: [String: String] = [:]
        for (k, v) in node.attrs ?? [:] where k.hasPrefix("data-") {
            let f = String(k.dropFirst("data-".count))
            if f == "action" || f.hasPrefix("fa-") { continue }
            values[f] = v
        }
        let html = FacetPrimitives.normalizeMedia(FacetPrimitives.fill(body, values))
        let player = FacetHTMLParser.parse(html).mapping { p in
            var q = p
            if q.tag == "video" || q.tag == "audio" { q.kind = "media" }
            return q
        }
        var n = node
        n.children = [player]
        var a = n.attrs ?? [:]
        a["data-fa-mounted"] = "1"
        n.attrs = a
        return n
    }

    /// A native fragment is the styled tree as JSON; decode it. Fall back to HTML
    /// parsing if a server sent a plain fragment.
    private func decodeNode(_ fragment: String) -> ViewNode? {
        if let d = fragment.data(using: .utf8), let n = try? JSONDecoder().decode(ViewNode.self, from: d) {
            return n
        }
        return FacetHTMLParser.parse(fragment)
    }

    private static func hexToBytes(_ hex: String) -> Data? {
        guard hex.count % 2 == 0 else { return nil }
        var out = Data(capacity: hex.count / 2)
        var idx = hex.startIndex
        while idx < hex.endIndex {
            let next = hex.index(idx, offsetBy: 2)
            guard let b = UInt8(hex[idx..<next], radix: 16) else { return nil }
            out.append(b)
            idx = next
        }
        return out
    }

    // MARK: - Actions (taps → /events)

    /// Sends an action to the server (the native equivalent of a click on a
    /// `data-action` element). Queued until the connection id is known.
    public func send(_ type: String, payload: [String: String] = [:]) {
        guard let connID else { pending.append((type, payload)); return }
        var req = URLRequest(url: url("/events"))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let body: [String: Any] = ["type": type, "payload": payload, "conn": connID]
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        Task { _ = try? await session.data(for: req) }
    }

    private func flushPending() {
        let q = pending
        pending.removeAll()
        for (t, p) in q { send(t, payload: p) }
    }

    // MARK: - helpers

    private func url(_ path: String) -> URL {
        URL(string: path, relativeTo: baseURL) ?? baseURL
    }
}

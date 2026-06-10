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

    public init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
    }

    /// Loads `route` as a native view tree and opens the live connection.
    public func start(route: String) {
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
            self.tree = screen.tree
            self.title = screen.title ?? ""
        } catch {
            // leave the previous tree in place on error
        }
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
        guard let tree, let id = ev.facet_id else { return }
        let node = ev.fragment.flatMap { decodeNode($0) }
        switch ev.op {
        case "replace":
            if let node { self.tree = tree.replacingFacet(id, with: node) }
        case "append":
            if let node { self.tree = tree.insertingChild(into: id, node, prepend: false) }
        case "prepend":
            if let node { self.tree = tree.insertingChild(into: id, node, prepend: true) }
        case "remove":
            self.tree = tree.removingFacet(id)
        default:
            break
        }
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

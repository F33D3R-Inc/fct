import Foundation
import SwiftUI

/// A server-pushed event, decoded from the SSE stream. Mirrors the web runtime:
/// `op` is replace/append/prepend/remove (or `_conn` for the connection id),
/// `facetId` targets the node, `fragment` is the new HTML (parsed to a ViewNode).
struct FacetEvent: Decodable {
    let op: String?
    let facet_id: String?
    let fragment: String?
    let conn: String?
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
            flushPending()
            return
        }
        apply(ev)
    }

    private func apply(_ ev: FacetEvent) {
        guard let tree, let id = ev.facet_id else { return }
        switch ev.op {
        case "replace":
            if let frag = ev.fragment {
                self.tree = tree.replacingFacet(id, with: FacetHTMLParser.parse(frag))
            }
        case "append":
            if let frag = ev.fragment {
                self.tree = tree.insertingChild(into: id, FacetHTMLParser.parse(frag), prepend: false)
            }
        case "prepend":
            if let frag = ev.fragment {
                self.tree = tree.insertingChild(into: id, FacetHTMLParser.parse(frag), prepend: true)
            }
        case "remove":
            self.tree = tree.removingFacet(id)
        default:
            break
        }
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

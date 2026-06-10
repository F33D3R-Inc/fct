import SwiftUI

/// FacetView renders one neutral `ViewNode` (and its subtree) to native SwiftUI
/// views. This is the renderer — the analogue of the web runtime's DOM
/// application. It maps abstract kinds to native controls:
///
///   box   → VStack / HStack (a layout heuristic from the class)
///   text  → Text
///   button→ Button (tap sends node.action to the server)
///   image → AsyncImage
///   input → TextField
///   link  → tappable Text that navigates
///   icon  → SF Symbol placeholder
public struct FacetView: View {
    let node: ViewNode
    @ObservedObject var client: FacetClient

    public init(node: ViewNode, client: FacetClient) {
        self.node = node
        self.client = client
    }

    public var body: some View {
        switch node.kind {
        case "text":
            Text(node.text ?? collectText(node))

        case "button":
            Button {
                if let action = node.action { client.send(action, payload: node.actionPayload) }
            } label: {
                childrenStack(axis: .horizontal)
            }
            .buttonStyle(.plain)

        case "image":
            AsyncImage(url: URL(string: node.attrs?["src"] ?? "")) { phase in
                switch phase {
                case .success(let img): img.resizable().scaledToFill()
                default: Color.gray.opacity(0.15)
                }
            }

        case "input":
            // Server-authoritative input: edits are sent as the field's action.
            FacetField(node: node, client: client)

        case "link":
            Button {
                if let href = node.attrs?["href"] { client.navigate(to: href) }
            } label: {
                if let t = node.text { Text(t) } else { childrenStack(axis: .horizontal) }
            }
            .buttonStyle(.plain)
            .foregroundColor(.accentColor)

        case "icon":
            Image(systemName: "circle.fill").imageScale(.small).foregroundColor(.secondary)

        default: // "box"
            childrenStack(axis: layoutAxis())
        }
    }

    // MARK: - layout

    /// Heuristic: rows lay out horizontally. The stdlib marks them with classes
    /// like fa-row / *__row / *__head, or uses inline-flex containers.
    private func layoutAxis() -> Axis {
        let cls = node.attrs?["class"] ?? ""
        for token in ["fa-row", "row", "__row", "__head", "__actions", "__meta", "fa-vidctl", "fa-engage", "fa-tabs", "fa-feedtabs"] where cls.contains(token) {
            return .horizontal
        }
        return .vertical
    }

    @ViewBuilder
    private func childrenStack(axis: Axis) -> some View {
        let kids = node.children ?? []
        if axis == .horizontal {
            HStack(alignment: .center, spacing: 6) { childViews(kids) }
        } else {
            VStack(alignment: .leading, spacing: 6) { childViews(kids) }
        }
    }

    @ViewBuilder
    private func childViews(_ kids: [ViewNode]) -> some View {
        ForEach(Array(kids.enumerated()), id: \.offset) { _, child in
            FacetView(node: child, client: client)
        }
    }

    /// Concatenates descendant text (for a text element that wasn't pre-folded).
    private func collectText(_ n: ViewNode) -> String {
        if let t = n.text { return t }
        return (n.children ?? []).map { collectText($0) }.joined()
    }
}

/// FacetField renders an `input` node as a TextField whose edits are pushed to the
/// server as the input's action (server-authoritative form state).
private struct FacetField: View {
    let node: ViewNode
    @ObservedObject var client: FacetClient
    @State private var text: String = ""

    var body: some View {
        let placeholder = node.attrs?["placeholder"] ?? ""
        let name = node.attrs?["name"] ?? ""
        return TextField(placeholder, text: $text)
            .textFieldStyle(.roundedBorder)
            .onAppear { text = node.attrs?["value"] ?? "" }
            .onSubmit {
                if let action = node.action { client.send(action, payload: [name: text]) }
            }
    }
}

/// FacetScreen is the top-level view: it shows a loading state until the first
/// tree arrives, then renders it and applies live updates.
///
/// ```swift
/// struct ContentView: View {
///     @StateObject var client = FacetClient(baseURL: URL(string: "https://app.example.com")!)
///     var body: some View { FacetScreen(client: client, route: "/") }
/// }
/// ```
public struct FacetScreen: View {
    @ObservedObject var client: FacetClient
    let route: String

    public init(client: FacetClient, route: String) {
        self.client = client
        self.route = route
    }

    public var body: some View {
        Group {
            if let tree = client.tree {
                ScrollView { FacetView(node: tree, client: client) }
            } else {
                ProgressView()
            }
        }
        .onAppear { client.start(route: route) }
    }
}

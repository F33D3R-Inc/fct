import SwiftUI

/// FacetView renders one neutral `ViewNode` (and its subtree) to native SwiftUI
/// views. It reads the SERVER-RESOLVED `style` (direction/gap/pad/align/paint) so
/// layout is exact, not inferred from class names. Abstract kinds map to native
/// controls:
///
///   box   → VStack / HStack (per style.direction)
///   text  → Text (with weight/size/color)
///   button→ Button (tap sends node.action; paint from style)
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
        content.modifier(StyleModifier(style: node.style))
    }

    @ViewBuilder
    private var content: some View {
        switch node.kind {
        case "text":
            Text(node.text ?? collectText(node))

        case "button":
            Button {
                if let action = node.action { client.send(action, payload: node.actionPayload) }
            } label: {
                childrenStack()
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
            FacetField(node: node, client: client)

        case "link":
            Button {
                if let href = node.attrs?["href"] { client.navigate(to: href) }
            } label: {
                if let t = node.text { Text(t) } else { childrenStack() }
            }
            .buttonStyle(.plain)
            .foregroundColor(.accentColor)

        case "icon":
            Image(systemName: "circle.fill").imageScale(.small).foregroundColor(.secondary)

        default: // "box"
            childrenStack()
        }
    }

    // MARK: - layout (driven by the server-resolved style)

    @ViewBuilder
    private func childrenStack() -> some View {
        let kids = node.children ?? []
        let s = node.style
        let gap = CGFloat(s?.gap ?? 6)
        if (s?.direction ?? "column") == "row" {
            HStack(alignment: vAlign(s?.align), spacing: gap) { childViews(kids) }
        } else {
            VStack(alignment: hAlign(s?.align), spacing: gap) { childViews(kids) }
        }
    }

    @ViewBuilder
    private func childViews(_ kids: [ViewNode]) -> some View {
        ForEach(Array(kids.enumerated()), id: \.offset) { _, child in
            FacetView(node: child, client: client)
        }
    }

    private func vAlign(_ a: String?) -> VerticalAlignment {
        switch a { case "start": return .top; case "end": return .bottom; default: return .center }
    }
    private func hAlign(_ a: String?) -> HorizontalAlignment {
        switch a { case "center": return .center; case "end": return .trailing; default: return .leading }
    }

    private func collectText(_ n: ViewNode) -> String {
        if let t = n.text { return t }
        return (n.children ?? []).map { collectText($0) }.joined()
    }
}

/// StyleModifier applies the server-resolved paint (padding, background, corner
/// radius, font weight/size, text color) to any node's view.
private struct StyleModifier: ViewModifier {
    let style: Style?

    func body(content: Content) -> some View {
        guard let s = style else { return AnyView(content) }
        var v = AnyView(content)
        if let weight = s.fontWeight {
            v = AnyView(v.fontWeight(weight >= 700 ? .bold : (weight >= 600 ? .semibold : .regular)))
        }
        if let size = s.fontSize {
            v = AnyView(v.font(.system(size: CGFloat(size))))
        }
        if let fg = s.fg, let c = Color(hex: fg) {
            v = AnyView(v.foregroundColor(c))
        }
        // Per-side padding (explicit units from the server).
        let insets = EdgeInsets(top: CGFloat(s.padT ?? 0), leading: CGFloat(s.padL ?? 0),
                                bottom: CGFloat(s.padB ?? 0), trailing: CGFloat(s.padR ?? 0))
        if insets != EdgeInsets() { v = AnyView(v.padding(insets)) }
        if let bg = s.bg, let c = Color(hex: bg) {
            v = AnyView(v.background(c))
        }
        if let r = s.radius, r > 0 {
            v = AnyView(v.clipShape(RoundedRectangle(cornerRadius: CGFloat(min(r, 28)))))
        }
        if let w = s.width { v = Self.dim(v, w, .horizontal) }
        if let h = s.height { v = Self.dim(v, h, .vertical) }
        if s.grow == true {
            v = AnyView(v.frame(maxWidth: .infinity))
        }
        return v
    }

    /// Applies an explicit dimension: a fixed px size, or `fill`/`100%` → expand.
    /// (Fractional `%` other than 100 renders as fill on iOS — exact fractions are
    /// a SwiftUI GeometryReader concern tracked for a later revision.)
    static func dim(_ v: AnyView, _ value: String, _ axis: Axis) -> AnyView {
        if value == "fill" || value.hasSuffix("%") {
            return AnyView(axis == .horizontal ? v.frame(maxWidth: .infinity) : v.frame(maxHeight: .infinity))
        }
        let n = value.hasSuffix("px") ? String(value.dropLast(2)) : value
        if let px = Double(n) {
            return AnyView(axis == .horizontal ? v.frame(width: CGFloat(px)) : v.frame(height: CGFloat(px)))
        }
        return v
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

/// FacetScreen is the top-level view: a loading state until the first tree arrives,
/// then the rendered tree with live updates applied.
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

extension Color {
    /// Parses "#rrggbb" / "#rgb" hex into a Color (returns nil for named colors).
    init?(hex: String) {
        var h = hex.trimmingCharacters(in: .whitespaces)
        guard h.hasPrefix("#") else { return nil }
        h.removeFirst()
        if h.count == 3 { h = h.map { "\($0)\($0)" }.joined() }
        guard h.count == 6, let v = UInt64(h, radix: 16) else { return nil }
        self = Color(
            red: Double((v >> 16) & 0xff) / 255,
            green: Double((v >> 8) & 0xff) / 255,
            blue: Double(v & 0xff) / 255
        )
    }
}

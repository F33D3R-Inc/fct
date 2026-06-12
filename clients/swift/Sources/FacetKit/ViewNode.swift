import Foundation

/// ViewNode is the platform-neutral UI element the FA server emits (the Swift
/// mirror of Go's `fa.ViewNode`). A tree of these is everything the client needs
/// to render a screen natively — no HTML, no WebView.
///
/// `kind` is abstract: `box` (a layout container), `text`, `button`, `image`,
/// `input`, `link`, `icon`. `facetId` identifies the node for surgical updates;
/// `action` is the event a tap sends back to the server.
public struct ViewNode: Codable, Equatable {
    public var kind: String
    public var tag: String?
    public var attrs: [String: String]?
    public var text: String?
    public var facetId: String?
    public var action: String?
    public var style: Style?
    public var children: [ViewNode]?

    public init(kind: String,
                tag: String? = nil,
                attrs: [String: String]? = nil,
                text: String? = nil,
                facetId: String? = nil,
                action: String? = nil,
                style: Style? = nil,
                children: [ViewNode]? = nil) {
        self.kind = kind
        self.tag = tag
        self.attrs = attrs
        self.text = text
        self.facetId = facetId
        self.action = action
        self.style = style
        self.children = children
    }

    /// The data-* payload a tap carries back (every data-* attribute except the
    /// reserved action / facet-id / fa-* ones) — mirrors the web runtime.
    public var actionPayload: [String: String] {
        var out: [String: String] = [:]
        for (k, v) in attrs ?? [:] where k.hasPrefix("data-") {
            let name = String(k.dropFirst("data-".count))
            if name == "action" || name == "facet-id" || name.hasPrefix("fa-") { continue }
            out[name] = v
        }
        return out
    }

    /// A CSS class token test, used by the renderer for layout heuristics.
    public func hasClass(_ token: String) -> Bool {
        (attrs?["class"] ?? "").split(separator: " ").contains { $0 == Substring(token) }
    }

    // MARK: - Surgical tree updates (mirror the web runtime's DOM ops)

    /// Returns a copy of the tree with the node identified by `facetId` replaced.
    public func replacingFacet(_ id: String, with newNode: ViewNode) -> ViewNode {
        if facetId == id { return newNode }
        var copy = self
        copy.children = children?.map { $0.replacingFacet(id, with: newNode) }
        return copy
    }

    /// Returns a copy with `child` appended/prepended to the node `facetId`.
    public func insertingChild(into id: String, _ child: ViewNode, prepend: Bool) -> ViewNode {
        var copy = self
        if facetId == id {
            var kids = copy.children ?? []
            if prepend { kids.insert(child, at: 0) } else { kids.append(child) }
            copy.children = kids
            return copy
        }
        copy.children = children?.map { $0.insertingChild(into: id, child, prepend: prepend) }
        return copy
    }

    /// Returns a copy with the node `facetId` removed.
    public func removingFacet(_ id: String) -> ViewNode {
        var copy = self
        copy.children = children?
            .filter { $0.facetId != id }
            .map { $0.removingFacet(id) }
        return copy
    }

    /// Returns a copy with the children of node `id` capped at `max` — the native
    /// mirror of the web runtime's stream `window:` trim. After an append, excess
    /// drops from the start (oldest first); after a prepend, from the end.
    public func trimmingChildren(of id: String, max: Int, dropFromStart: Bool) -> ViewNode {
        var copy = self
        if facetId == id, let kids = children, kids.count > max {
            copy.children = dropFromStart ? Array(kids.suffix(max)) : Array(kids.prefix(max))
            return copy
        }
        copy.children = children?.map { $0.trimmingChildren(of: id, max: max, dropFromStart: dropFromStart) }
        return copy
    }

    /// Returns a copy with `transform` applied to every node, bottom-up. The
    /// primitive scans (signal apply, vault decrypt, media mount) are tree maps.
    public func mapping(_ transform: (ViewNode) -> ViewNode) -> ViewNode {
        var copy = self
        copy.children = children?.map { $0.mapping(transform) }
        return transform(copy)
    }
}

/// Style is the server-resolved, platform-neutral layout + appearance of a node
/// (the Swift mirror of Go's `fa.Style`). The renderer reads this instead of
/// guessing from class names — direction/gap/pad/align drive the stack; bg/fg/
/// fontWeight/radius drive the paint.
public struct Style: Codable, Equatable {
    public var direction: String?
    public var gap: Int?
    public var padT: Int?
    public var padR: Int?
    public var padB: Int?
    public var padL: Int?
    public var align: String?
    public var justify: String?
    public var grow: Bool?
    public var width: String?
    public var height: String?
    public var bg: String?
    public var fg: String?
    public var fontSize: Int?
    public var fontWeight: Int?
    public var radius: Int?
}

/// The JSON a route returns to a native client (`FA-Native: 1`): the screen title
/// and its neutral view tree.
public struct ScreenResponse: Codable {
    public var title: String?
    public var tree: ViewNode
}

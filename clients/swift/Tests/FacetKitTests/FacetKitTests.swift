import XCTest
@testable import FacetKit

final class FacetKitTests: XCTestCase {

    // The Swift HTML→tree parser must agree with Go's fa.ParseView: a button with
    // an action and a folded text child.
    func testParseButton() throws {
        let node = FacetHTMLParser.parse(
            #"<button class="fa-tip" data-action="tip.send" data-facet-id="TipButton"><span>🪙 100</span></button>"#
        )
        XCTAssertEqual(node.kind, "button")
        XCTAssertEqual(node.action, "tip.send")
        XCTAssertEqual(node.facetId, "TipButton")
        XCTAssertEqual(node.children?.count, 1)
        XCTAssertEqual(node.children?.first?.kind, "text")
        XCTAssertEqual(node.children?.first?.text, "🪙 100")
    }

    // Composition + void elements + nested boxes survive parsing.
    func testParseComposition() throws {
        let node = FacetHTMLParser.parse(
            #"<div class="row" data-facet-id="Row"><img class="fa-avatar" src="/a.png" data-facet-id="Avatar"/><span>Ada</span></div>"#
        )
        XCTAssertEqual(node.kind, "box")
        XCTAssertEqual(node.facetId, "Row")
        let img = node.children?.first { $0.kind == "image" }
        XCTAssertEqual(img?.facetId, "Avatar")
        XCTAssertEqual(img?.attrs?["src"], "/a.png")
        let text = node.children?.first { $0.kind == "text" }
        XCTAssertEqual(text?.text, "Ada")
    }

    // SVG collapses to a single opaque icon (not a tree of path/circle elements).
    func testSvgCollapsesToIcon() throws {
        let node = FacetHTMLParser.parse(
            #"<button data-action="play"><svg viewBox="0 0 24 24"><path d="M3 3"/></svg>Play</button>"#
        )
        XCTAssertEqual(node.kind, "button")
        let icons = (node.children ?? []).filter { $0.kind == "icon" }
        let plays = (node.children ?? []).filter { $0.kind == "text" && $0.text == "Play" }
        XCTAssertEqual(icons.count, 1)
        XCTAssertEqual(plays.count, 1)
    }

    // Surgical update: replacing a facet by id swaps only that subtree.
    func testReplaceFacet() {
        let tree = ViewNode(kind: "box", facetId: "Card", children: [
            ViewNode(kind: "text", text: "before", facetId: "Label"),
            ViewNode(kind: "button", facetId: "Btn")
        ])
        let updated = tree.replacingFacet("Label", with: ViewNode(kind: "text", text: "after", facetId: "Label"))
        XCTAssertEqual(updated.children?.first?.text, "after")
        XCTAssertEqual(updated.children?.last?.facetId, "Btn") // sibling untouched
    }

    // append / remove behave like the web runtime's DOM ops.
    func testAppendAndRemove() {
        var tree = ViewNode(kind: "box", facetId: "List", children: [])
        tree = tree.insertingChild(into: "List", ViewNode(kind: "text", text: "row1", facetId: "r1"), prepend: false)
        tree = tree.insertingChild(into: "List", ViewNode(kind: "text", text: "row0", facetId: "r0"), prepend: true)
        XCTAssertEqual(tree.children?.map { $0.facetId }, ["r0", "r1"])
        tree = tree.removingFacet("r1")
        XCTAssertEqual(tree.children?.map { $0.facetId }, ["r0"])
    }

    // ScreenResponse decodes the server's FA-Native JSON.
    func testDecodeScreenResponse() throws {
        let json = #"{"title":"Home","tree":{"kind":"button","action":"x","children":[{"kind":"text","text":"Go"}]}}"#
        let resp = try JSONDecoder().decode(ScreenResponse.self, from: Data(json.utf8))
        XCTAssertEqual(resp.title, "Home")
        XCTAssertEqual(resp.tree.kind, "button")
        XCTAssertEqual(resp.tree.action, "x")
        XCTAssertEqual(resp.tree.children?.first?.text, "Go")
    }
}

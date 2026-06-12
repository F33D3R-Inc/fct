import CryptoKit
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

    // ── primitive runtime semantics (native mirror of fa-runtime.js) ────────

    // Stream `window:` — children cap, trimming the opposite end of the insert.
    func testTrimmingChildren() {
        var tree = ViewNode(kind: "box", facetId: "LiveChat", children: [
            ViewNode(kind: "text", text: "1"), ViewNode(kind: "text", text: "2"),
            ViewNode(kind: "text", text: "3"), ViewNode(kind: "text", text: "4"),
        ])
        let appended = tree.trimmingChildren(of: "LiveChat", max: 2, dropFromStart: true)
        XCTAssertEqual(appended.children?.map { $0.text }, ["3", "4"]) // oldest dropped
        let prepended = tree.trimmingChildren(of: "LiveChat", max: 2, dropFromStart: false)
        XCTAssertEqual(prepended.children?.map { $0.text }, ["1", "2"])
        tree = tree.trimmingChildren(of: "Other", max: 1, dropFromStart: true)
        XCTAssertEqual(tree.children?.count, 4) // non-matching facet untouched
    }

    func testFacetNameAndDurations() {
        XCTAssertEqual(FacetPrimitives.facetName("LikeButton:post:42"), "LikeButton")
        XCTAssertEqual(FacetPrimitives.facetName("Typing"), "Typing")
        XCTAssertEqual(FacetPrimitives.goDurationMs("200ms"), 200)
        XCTAssertEqual(FacetPrimitives.goDurationMs("5s"), 5000)
        XCTAssertEqual(FacetPrimitives.goDurationMs("2m"), 120_000)
        XCTAssertEqual(FacetPrimitives.goDurationMs("nope"), 0)
    }

    // fill substitutes only {field} holes, HTML-escaping values (vault safety:
    // decrypted plaintext can never inject elements).
    func testFillEscapes() {
        let out = FacetPrimitives.fill("<p>{plaintext}</p>{ if x }{missing}",
                                       ["plaintext": "<b>&hi'\""])
        XCTAssertEqual(out, "<p>&lt;b&gt;&amp;hi&#39;&quot;</p>{ if x }")
    }

    func testNormalizeMedia() {
        XCTAssertEqual(FacetPrimitives.normalizeMedia(#"<hls src="/v.m3u8"/>"#),
                       #"<video src="/v.m3u8"/>"#)
        XCTAssertEqual(FacetPrimitives.normalizeMedia("<dash src=\"x\"></dash>"),
                       "<video src=\"x\"></video>")
    }

    // Vault round-trip: AES-GCM over base64(IV ‖ ciphertext ‖ tag) — the same
    // combined envelope the web runtime decrypts.
    func testVaultEnvelopeRoundTrip() throws {
        let key = SymmetricKey(size: .bits256)
        let sealed = try AES.GCM.seal(Data("hello vault".utf8), using: key)
        let envelope = sealed.combined!.base64EncodedString()
        XCTAssertEqual(FacetPrimitives.decryptEnvelope(envelope, key: key), "hello vault")
        // Fail closed: wrong key decrypts to nil, never garbage.
        XCTAssertNil(FacetPrimitives.decryptEnvelope(envelope, key: SymmetricKey(size: .bits256)))
        XCTAssertNil(FacetPrimitives.decryptEnvelope("not-base64!", key: key))
    }

    // Signal payload keys must not hijack reserved attributes.
    func testSafeSignalKeys() {
        XCTAssertTrue(FacetPrimitives.safeSignalKey("who"))
        XCTAssertFalse(FacetPrimitives.safeSignalKey("action"))
        XCTAssertFalse(FacetPrimitives.safeSignalKey("fa-anything"))
        XCTAssertFalse(FacetPrimitives.safeSignalKey("1bad"))
    }

    // The manifest registry decodes per-primitive rules.
    func testDecodeManifest() throws {
        let json = #"{"facets":[{"name":"LiveChat","kind":"stream","facet_id":"LiveChat","window":"100","ttl":"","when":null},{"name":"Typing","kind":"signal","facet_id":"Typing","ttl":"5s","when":null}]}"#
        let m = try JSONDecoder().decode(FacetManifest.self, from: Data(json.utf8))
        XCTAssertEqual(m.facets.first?.windowCount, 100)
        XCTAssertEqual(m.facets.last?.ttlMs, 5000)
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

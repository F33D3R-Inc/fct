package dev.fct.facetkit

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Test

class FacetKitTest {

    @Test fun parseButton() {
        val n = FacetHtmlParser.parse(
            """<button class="fa-tip" data-action="tip.send" data-facet-id="TipButton"><span>🪙 100</span></button>"""
        )
        assertEquals("button", n.kind)
        assertEquals("tip.send", n.action)
        assertEquals("TipButton", n.facetId)
        assertEquals(1, n.children?.size)
        assertEquals("text", n.children?.first()?.kind)
        assertEquals("🪙 100", n.children?.first()?.text)
    }

    @Test fun parseComposition() {
        val n = FacetHtmlParser.parse(
            """<div class="row" data-facet-id="Row"><img class="fa-avatar" src="/a.png" data-facet-id="Avatar"/><span>Ada</span></div>"""
        )
        assertEquals("box", n.kind)
        assertEquals("Row", n.facetId)
        val img = n.children?.firstOrNull { it.kind == "image" }
        assertEquals("Avatar", img?.facetId)
        assertEquals("/a.png", img?.attrs?.get("src"))
        assertEquals("Ada", n.children?.firstOrNull { it.kind == "text" }?.text)
    }

    @Test fun svgCollapsesToIcon() {
        val n = FacetHtmlParser.parse(
            """<button data-action="play"><svg viewBox="0 0 24 24"><path d="M3 3"/></svg>Play</button>"""
        )
        assertEquals("button", n.kind)
        assertEquals(1, n.children?.count { it.kind == "icon" })
        assertEquals(1, n.children?.count { it.kind == "text" && it.text == "Play" })
    }

    @Test fun stylesResolveOnDeviceLikeServer() {
        // Class-based row + design-system button paint, resolved client-side from
        // the parsed fragment (kept consistent with fa/style.go).
        val row = FacetHtmlParser.parse("""<div class="fa-row"><span>a</span></div>""")
        assertEquals("row", row.style?.direction)

        val btn = FacetHtmlParser.parse("""<button class="fa-btn fa-btn--primary" data-action="x">Go</button>""")
        assertEquals("#1d9bf0", btn.style?.bg)
        assertEquals(999, btn.style?.radius)

        // Inline width (a progress fill).
        val bar = FacetHtmlParser.parse("""<span class="fa-progress__fill" style="width:30%"></span>""")
        assertEquals("30%", bar.style?.width)
    }

    @Test fun surgicalUpdates() {
        var tree = ViewNode(
            kind = "box", facetId = "List", children = listOf(
                ViewNode(kind = "text", text = "before", facetId = "Label"),
                ViewNode(kind = "button", facetId = "Btn"),
            )
        )
        tree = tree.replacingFacet("Label", ViewNode(kind = "text", text = "after", facetId = "Label"))
        assertEquals("after", tree.children?.first()?.text)
        assertEquals("Btn", tree.children?.last()?.facetId)

        tree = tree.insertingChild("List", ViewNode(kind = "text", facetId = "x"), prepend = true)
        assertEquals("x", tree.children?.first()?.facetId)
        tree = tree.removingFacet("Btn")
        assertNull(tree.children?.firstOrNull { it.facetId == "Btn" })
    }

    @Test fun decodeScreenResponse() {
        val json = Json { ignoreUnknownKeys = true }
        val resp = json.decodeFromString<ScreenResponse>(
            """{"title":"Home","tree":{"kind":"button","action":"x","style":{"bg":"#1d9bf0"},"children":[{"kind":"text","text":"Go"}]}}"""
        )
        assertEquals("Home", resp.title)
        assertEquals("button", resp.tree.kind)
        assertEquals("#1d9bf0", resp.tree.style?.bg)
        assertNotNull(resp.tree.children)
        assertEquals("Go", resp.tree.children?.first()?.text)
    }
}

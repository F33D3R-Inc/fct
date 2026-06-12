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

    @Test fun styleArrivesFromServerOnTheTree() {
        // Style is resolved on the SERVER and decoded from the tree — the client
        // holds no style table. A fragment parsed as a fallback carries no style.
        val json = Json { ignoreUnknownKeys = true }
        val node = json.decodeFromString<ViewNode>(
            """{"kind":"button","style":{"direction":"row","bg":"#1d9bf0","radius":999}}"""
        )
        assertEquals("row", node.style?.direction)
        assertEquals("#1d9bf0", node.style?.bg)
        assertEquals(999, node.style?.radius)

        val fallback = FacetHtmlParser.parse("""<div class="fa-row"></div>""")
        assertNull(fallback.style) // parser no longer resolves style (single source: server)
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

    // ── primitive runtime semantics (native mirror of fa-runtime.js) ────────

    @Test fun trimmingChildrenEnforcesStreamWindow() {
        val tree = ViewNode(
            kind = "box", facetId = "LiveChat", children = listOf(
                ViewNode(kind = "text", text = "1"), ViewNode(kind = "text", text = "2"),
                ViewNode(kind = "text", text = "3"), ViewNode(kind = "text", text = "4"),
            )
        )
        val appended = tree.trimmingChildren("LiveChat", 2, dropFromStart = true)
        assertEquals(listOf("3", "4"), appended.children?.map { it.text }) // oldest dropped
        val prepended = tree.trimmingChildren("LiveChat", 2, dropFromStart = false)
        assertEquals(listOf("1", "2"), prepended.children?.map { it.text })
        val other = tree.trimmingChildren("Other", 1, dropFromStart = true)
        assertEquals(4, other.children?.size) // non-matching facet untouched
    }

    @Test fun facetNamesAndDurations() {
        assertEquals("LikeButton", Primitives.facetName("LikeButton:post:42"))
        assertEquals("Typing", Primitives.facetName("Typing"))
        assertEquals(200L, Primitives.goDurationMs("200ms"))
        assertEquals(5000L, Primitives.goDurationMs("5s"))
        assertEquals(120_000L, Primitives.goDurationMs("2m"))
        assertEquals(0L, Primitives.goDurationMs("nope"))
    }

    // fill substitutes only {field} holes, HTML-escaping values (vault safety:
    // decrypted plaintext can never inject elements).
    @Test fun fillEscapes() {
        val out = Primitives.fill(
            "<p>{plaintext}</p>{ if x }{missing}",
            mapOf("plaintext" to "<b>&hi'\"")
        )
        assertEquals("<p>&lt;b&gt;&amp;hi&#39;&quot;</p>{ if x }", out)
    }

    @Test fun normalizeMediaTags() {
        assertEquals("""<video src="/v.m3u8"/>""", Primitives.normalizeMedia("""<hls src="/v.m3u8"/>"""))
        assertEquals("""<video src="x"></video>""", Primitives.normalizeMedia("""<dash src="x"></dash>"""))
    }

    // Vault round-trip: AES-GCM over base64(IV ‖ ciphertext ‖ tag) — the same
    // combined envelope the web runtime decrypts.
    @Test fun vaultEnvelopeRoundTrip() {
        val key = ByteArray(32) { it.toByte() }
        val iv = ByteArray(12) { (it + 1).toByte() }
        val cipher = javax.crypto.Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(
            javax.crypto.Cipher.ENCRYPT_MODE,
            javax.crypto.spec.SecretKeySpec(key, "AES"),
            javax.crypto.spec.GCMParameterSpec(128, iv),
        )
        val sealed = iv + cipher.doFinal("hello vault".toByteArray())
        val envelope = java.util.Base64.getEncoder().encodeToString(sealed) // JVM test only
        assertEquals("hello vault", Primitives.decryptEnvelope(envelope, key))
        // Fail closed: wrong key decrypts to null, never garbage.
        assertNull(Primitives.decryptEnvelope(envelope, ByteArray(32) { 9 }))
        assertNull(Primitives.decryptEnvelope("not-base64!", key))
    }

    @Test fun base64DecoderMatchesJvm() {
        for (s in listOf("", "a", "ab£", "hello vault!", "1234567890")) {
            val bytes = s.toByteArray()
            val enc = java.util.Base64.getEncoder().encodeToString(bytes)
            assertEquals(bytes.toList(), Primitives.base64Decode(enc)?.toList())
        }
    }

    // Signal payload keys must not hijack reserved attributes.
    @Test fun safeSignalKeys() {
        assertEquals(true, Primitives.safeSignalKey("who"))
        assertEquals(false, Primitives.safeSignalKey("action"))
        assertEquals(false, Primitives.safeSignalKey("fa-anything"))
        assertEquals(false, Primitives.safeSignalKey("1bad"))
    }

    // The manifest registry decodes per-primitive rules.
    @Test fun decodeManifest() {
        val json = Json { ignoreUnknownKeys = true }
        val m = json.decodeFromString<FacetManifest>(
            """{"facets":[{"name":"LiveChat","kind":"stream","facet_id":"LiveChat","window":"100","when":null},{"name":"Typing","kind":"signal","facet_id":"Typing","ttl":"5s","when":null}]}"""
        )
        assertEquals(100, m.facets.first().windowCount)
        assertEquals(5000L, m.facets.last().ttlMs)
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

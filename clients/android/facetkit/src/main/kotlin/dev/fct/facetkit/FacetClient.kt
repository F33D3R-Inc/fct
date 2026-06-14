package dev.fct.facetkit

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import java.net.HttpURLConnection
import java.net.URL
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

/**
 * The SSE wire version this client speaks (see STABILITY.md). The server rejects
 * a mismatch at connect (426) and announces its own version on the hello frame.
 */
private const val WIRE_VERSION = "1"

/** A server-pushed SSE event (mirror of the web runtime's frame). */
@Serializable
private data class FacetEvent(
    val op: String? = null,
    val facet_id: String? = null,
    val fragment: String? = null, // styled neutral-tree JSON (native); the signed bytes
    val hmac: String? = null,
    val conn: String? = null,
    val key: String? = null,      // signing key, on the _conn frame
    val v: String? = null,        // server's SSE wire version, on the _conn frame
)

@Serializable
private data class EventOut(val type: String, val payload: Map<String, String>, val conn: String)

/**
 * FacetClient is the Android FA runtime — the analogue of fa-runtime.js. It loads a
 * screen as a neutral view tree, holds one SSE connection, applies pushed updates
 * by facet id, and forwards taps to the single /events endpoint. It owns NO
 * application logic — the server decides what every action does.
 *
 * ```kotlin
 * val client = remember { FacetClient("https://app.example.com") }
 * FacetScreen(client, route = "/")
 * ```
 */
class FacetClient(
    baseUrl: String,
    private val scope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.Main),
) {
    private val base = baseUrl.trimEnd('/')
    private val json = Json { ignoreUnknownKeys = true }

    /** Compose-observable screen state. */
    var tree by mutableStateOf<ViewNode?>(null)
        private set
    var title by mutableStateOf("")
        private set
    var connected by mutableStateOf(false)
        private set

    /**
     * Set (and reconnection stopped) when this client and the server speak
     * different SSE wire versions — fail loud, never render garbage.
     */
    var wireError by mutableStateOf<String?>(null)
        private set

    private var connId: String? = null
    private var signKey: ByteArray? = null
    private val pending = mutableListOf<Pair<String, Map<String, String>>>()

    // Per-primitive runtime state (mirrors the web runtime's manifest registry).
    private val registry = mutableMapOf<String, FacetMeta>()   // facet name → rules
    private val vaultKeys = mutableMapOf<String, ByteArray>()  // vault name → AES-GCM key
    private val signalJobs = mutableMapOf<String, Job>()       // facet-id → ttl expiry
    private val signalAttrs = mutableMapOf<String, List<String>>() // facet-id → applied data-* keys

    /**
     * Called on every relayed `signal` event with (facet id, payload) — the
     * programmatic hook for ephemeral peer state (typing, presence) when a tree
     * attribute isn't enough.
     */
    var onSignal: ((String, Map<String, String>) -> Unit)? = null

    /** Loads [route] as a native view tree and opens the live connection. */
    fun start(route: String) {
        loadManifest()
        navigate(route)
        openSse()
    }

    /**
     * Fetches the compiled manifest and indexes its per-primitive rules (stream
     * window, signal ttl, vault/media client bodies) by facet name — the same
     * registry the web runtime builds from /manifest.json.
     */
    private fun loadManifest() {
        scope.launch {
            val m = withContext(Dispatchers.IO) {
                try {
                    val conn = (URL("$base/manifest.json").openConnection() as HttpURLConnection)
                        .apply { connectTimeout = 5000 }
                    conn.inputStream.bufferedReader().use {
                        json.decodeFromString<FacetManifest>(it.readText())
                    }
                } catch (_: Exception) {
                    null
                }
            } ?: return@launch
            for (f in m.facets) registry[f.name] = f
            tree?.let { tree = postProcess(it) } // rules may unlock vault/media nodes
        }
    }

    /**
     * Registers a vault's AES-GCM key (hex) and decrypts any visible envelopes.
     * The key exists only on this device — it is never sent to the server, which
     * is the vault guarantee (the native mirror of web `fa.vault.key`).
     */
    fun vaultKey(facet: String, hexKey: String) {
        val bytes = hexToBytes(hexKey) ?: return
        vaultKeys[facet] = bytes
        tree?.let { tree = postProcess(it) }
    }

    fun stop() {
        scope.cancel()
    }

    /** Client-side navigation — the SSE connection is untouched, so facets persist. */
    fun navigate(route: String) {
        scope.launch {
            val screen = withContext(Dispatchers.IO) { fetchScreen(route) } ?: return@launch
            tree = postProcess(screen.tree)
            title = screen.title ?: ""
        }
    }

    private fun fetchScreen(route: String): ScreenResponse? = try {
        val conn = (URL(base + route).openConnection() as HttpURLConnection).apply {
            setRequestProperty("FA-Native", "1")
            connectTimeout = 5000
        }
        conn.inputStream.bufferedReader().use { json.decodeFromString<ScreenResponse>(it.readText()) }
    } catch (_: Exception) {
        null
    }

    private fun openSse() {
        scope.launch(Dispatchers.IO) {
            while (isActive) {
                try {
                    val conn = (URL("$base/sse").openConnection() as HttpURLConnection).apply {
                        setRequestProperty("Accept", "text/event-stream")
                        setRequestProperty("FA-Native", "1") // get styled trees, not HTML
                        setRequestProperty("FA-Wire-Version", WIRE_VERSION)
                        connectTimeout = 5000
                        readTimeout = 0
                    }
                    if (conn.responseCode == 426) {
                        // Fatal — do not reconnect-loop against an incompatible server.
                        val server = conn.getHeaderField("FA-Wire-Version") ?: "?"
                        withContext(Dispatchers.Main) {
                            failWire("server speaks SSE wire v$server, this client speaks v$WIRE_VERSION")
                        }
                        return@launch
                    }
                    conn.inputStream.bufferedReader().use { reader ->
                        withContext(Dispatchers.Main) { connected = true }
                        val data = StringBuilder()
                        var line: String?
                        while (reader.readLine().also { line = it } != null) {
                            val l = line!!
                            if (l.isEmpty()) {
                                if (data.isNotEmpty()) {
                                    val frame = data.toString(); data.clear()
                                    withContext(Dispatchers.Main) { handleFrame(frame) }
                                    if (wireError != null) return@launch // fatal — stop the stream
                                }
                            } else if (l.startsWith("data:")) {
                                data.append(l.removePrefix("data:").trim())
                            }
                        }
                    }
                } catch (_: Exception) {
                    // reconnect
                }
                withContext(Dispatchers.Main) { connected = false }
                connId = null
                delay(1000)
            }
        }
    }

    private fun failWire(msg: String) {
        wireError = msg
        connected = false
    }

    private fun handleFrame(frame: String) {
        val ev = try { json.decodeFromString<FacetEvent>(frame) } catch (_: Exception) { return }
        if (ev.op == "_conn") {
            // Hello-frame version check: catches a new client against an old server
            // (the other direction is rejected with 426 before any frame arrives).
            if (ev.v != null && ev.v != WIRE_VERSION) {
                failWire("server speaks SSE wire v${ev.v}, this client speaks v$WIRE_VERSION")
                return
            }
            connId = ev.conn
            ev.key?.let { signKey = hexToBytes(it) }
            flushPending()
            return
        }
        if (!verify(ev)) return // drop any event we cannot authenticate
        apply(ev)
    }

    /** Verifies HMAC-SHA256 over op\0facet_id\0fragment (matches the server/web). */
    private fun verify(ev: FacetEvent): Boolean {
        val key = signKey ?: return true // no key yet → accept (parity with web)
        val expected = ev.hmac ?: return false
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(key, "HmacSHA256"))
        mac.update((ev.op ?: "").toByteArray()); mac.update(0.toByte())
        mac.update((ev.facet_id ?: "").toByteArray()); mac.update(0.toByte())
        mac.update((ev.fragment ?: "").toByteArray())
        val computed = mac.doFinal().joinToString("") { "%02x".format(it) }
        return computed == expected
    }

    private fun apply(ev: FacetEvent) {
        if (ev.op == "signal") {
            applySignal(ev)
            return
        }
        val cur = tree ?: return
        val id = ev.facet_id ?: return
        val node = ev.fragment?.let { decodeNode(it) }
        tree = when (ev.op) {
            "replace" -> node?.let { postProcess(cur.replacingFacet(id, it)) } ?: cur
            "append", "prepend" -> node?.let {
                val prepend = ev.op == "prepend"
                var next = cur.insertingChild(id, it, prepend)
                // stream `window:` — cap retained children, trimming the opposite end
                val meta = registry[Primitives.facetName(id)]
                if (meta != null && meta.kind == "stream" && meta.windowCount > 0) {
                    next = next.trimmingChildren(id, meta.windowCount, dropFromStart = !prepend)
                }
                postProcess(next)
            } ?: cur
            "remove" -> cur.removingFacet(id)
            else -> cur
        }
    }

    // ── signal (ephemeral peer state) ──────────────────────────────────────────

    /**
     * Applies a relayed signal: the payload lands as data-* attributes (plus the
     * fa-signal-live class) on every node whose data-fa-signal matches the
     * signal's facet id or name, and reverts after the declared `ttl:` — exactly
     * the web runtime. [onSignal] fires regardless, for programmatic consumers.
     */
    private fun applySignal(ev: FacetEvent) {
        val id = ev.facet_id ?: return
        val payload: Map<String, String> = try {
            json.decodeFromString(ev.fragment ?: "{}")
        } catch (_: Exception) {
            emptyMap()
        }
        onSignal?.invoke(id, payload)

        val name = Primitives.facetName(id)
        val attrsToSet = payload
            .filterKeys { Primitives.safeSignalKey(it) }
            .mapKeys { (k, _) -> "data-" + k.lowercase() }
        tree?.let { cur ->
            tree = cur.mapping { node ->
                val want = node.attrs?.get("data-fa-signal")
                if (want != id && want != name) return@mapping node
                val a = (node.attrs ?: emptyMap()).toMutableMap()
                a.putAll(attrsToSet)
                a["class"] = addClass(a["class"], "fa-signal-live")
                node.copy(attrs = a)
            }
        }
        signalAttrs[id] = attrsToSet.keys.toList()

        signalJobs.remove(id)?.cancel()
        val ttl = registry[name]?.ttlMs ?: 0
        if (ttl <= 0) return
        signalJobs[id] = scope.launch {
            delay(ttl)
            expireSignal(id)
        }
    }

    private fun expireSignal(id: String) {
        val name = Primitives.facetName(id)
        val keys = signalAttrs.remove(id) ?: emptyList()
        val cur = tree ?: return
        tree = cur.mapping { node ->
            val want = node.attrs?.get("data-fa-signal")
            if (want != id && want != name) return@mapping node
            val a = (node.attrs ?: emptyMap()).toMutableMap()
            for (k in keys) a.remove(k)
            val cls = removeClass(a["class"], "fa-signal-live")
            if (cls == null) a.remove("class") else a["class"] = cls
            node.copy(attrs = a)
        }
    }

    private fun addClass(cls: String?, token: String): String {
        val parts = (cls ?: "").split(' ').filter { it.isNotEmpty() }
        return if (token in parts) parts.joinToString(" ") else (parts + token).joinToString(" ")
    }

    private fun removeClass(cls: String?, token: String): String? {
        val parts = (cls ?: "").split(' ').filter { it.isNotEmpty() && it != token }
        return if (parts.isEmpty()) null else parts.joinToString(" ")
    }

    // ── vault decrypt + media mount (client-rendered primitives) ──────────────

    /**
     * Applies the client-rendered primitives to the tree: decrypts ready vault
     * envelopes and mounts media players. Runs after every tree change, when the
     * manifest arrives, and when a vault key is registered. Already-processed
     * nodes are skipped via marker attributes, so the map is cheap.
     */
    private fun postProcess(tree: ViewNode): ViewNode =
        tree.mapping { node -> vaultNode(node) ?: mediaNode(node) ?: node }

    /**
     * Decrypts one vault node: data-fa-vault names the primitive, the manifest
     * carries its decrypt: body (there is NO server template — the structural
     * guarantee), data-fa-envelope is base64(IV ‖ ciphertext ‖ tag). The
     * decrypted values are escaped, filled into the body, and parsed into the
     * node's children. Any failure leaves the node untouched.
     */
    private fun vaultNode(node: ViewNode): ViewNode? {
        val attrs = node.attrs ?: return null
        val name = attrs["data-fa-vault"] ?: return null
        val meta = registry[name] ?: return null
        if (meta.kind != "vault") return null
        val body = meta.client ?: return null
        if (body.isEmpty()) return null
        val env = attrs["data-fa-envelope"] ?: return null
        if (attrs["data-fa-decrypted"] == env) return null
        val key = vaultKeys[name] ?: return null
        val plaintext = Primitives.decryptEnvelope(env, key) ?: return null

        val values = mutableMapOf<String, Any?>("plaintext" to plaintext)
        try { // a JSON plaintext exposes its fields (structured: arrays/objects for if/for)
            val el = json.parseToJsonElement(plaintext)
            if (el is JsonObject) for ((k, v) in el) values[k] = jsonToAny(v)
        } catch (_: Exception) {
        }
        val a = attrs.toMutableMap()
        a["data-fa-decrypted"] = env
        return node.copy(
            attrs = a,
            children = listOf(FacetHtmlParser.parse(Primitives.fill(body, values))),
        )
    }

    /** Converts parsed JSON into plain Kotlin types the client-body interpreter
     *  understands (Map / List / String / Double / Boolean / null). */
    private fun jsonToAny(el: JsonElement): Any? = when (el) {
        is JsonNull -> null
        is JsonObject -> el.mapValues { jsonToAny(it.value) }
        is JsonArray -> el.map { jsonToAny(it) }
        is JsonPrimitive -> when {
            el.isString -> el.content
            el.content == "true" -> true
            el.content == "false" -> false
            else -> el.content.toDoubleOrNull() ?: el.content
        }
    }

    /**
     * Mounts one media node: the manifest's source: body, holes filled from the
     * node's data-* attributes, <hls>/<dash> normalized to <video>, parsed, and
     * marked kind "media" so the renderer shows a real player.
     */
    private fun mediaNode(node: ViewNode): ViewNode? {
        val attrs = node.attrs ?: return null
        val name = attrs["data-fa-media"] ?: return null
        val meta = registry[name] ?: return null
        if (meta.kind != "media") return null
        val body = meta.client ?: return null
        if (body.isEmpty() || attrs["data-fa-mounted"] != null) return null

        val values = mutableMapOf<String, String>()
        for ((k, v) in attrs) {
            if (!k.startsWith("data-")) continue
            val f = k.removePrefix("data-")
            if (f == "action" || f.startsWith("fa-")) continue
            values[f] = v
        }
        val html = Primitives.normalizeMedia(Primitives.fill(body, values))
        val player = FacetHtmlParser.parse(html).mapping { p ->
            if (p.tag == "video" || p.tag == "audio") p.copy(kind = "media") else p
        }
        val a = attrs.toMutableMap()
        a["data-fa-mounted"] = "1"
        return node.copy(attrs = a, children = listOf(player))
    }

    /** A native fragment is the styled tree as JSON; decode it (HTML fallback). */
    private fun decodeNode(fragment: String): ViewNode? =
        try { json.decodeFromString<ViewNode>(fragment) } catch (_: Exception) { FacetHtmlParser.parse(fragment) }

    private fun hexToBytes(hex: String): ByteArray? =
        if (hex.length % 2 != 0) null
        else ByteArray(hex.length / 2) { hex.substring(it * 2, it * 2 + 2).toInt(16).toByte() }

    /** Sends an action to the server (a tap). Queued until the connection id is known. */
    fun send(type: String, payload: Map<String, String> = emptyMap()) {
        val cid = connId
        if (cid == null) {
            pending.add(type to payload)
            return
        }
        scope.launch(Dispatchers.IO) {
            try {
                val conn = (URL("$base/events").openConnection() as HttpURLConnection).apply {
                    requestMethod = "POST"
                    doOutput = true
                    setRequestProperty("Content-Type", "application/json")
                }
                conn.outputStream.use { it.write(json.encodeToString(EventOut(type, payload, cid)).toByteArray()) }
                conn.responseCode // fire
            } catch (_: Exception) {
            }
        }
    }

    private fun flushPending() {
        val q = pending.toList()
        pending.clear()
        for ((t, p) in q) send(t, p)
    }
}

package dev.fct.facetkit

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.net.HttpURLConnection
import java.net.URL

/** A server-pushed SSE event (mirror of the web runtime's frame). */
@Serializable
private data class FacetEvent(
    val op: String? = null,
    val facet_id: String? = null,
    val fragment: String? = null,
    val conn: String? = null,
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

    private var connId: String? = null
    private val pending = mutableListOf<Pair<String, Map<String, String>>>()

    /** Loads [route] as a native view tree and opens the live connection. */
    fun start(route: String) {
        navigate(route)
        openSse()
    }

    fun stop() {
        scope.cancel()
    }

    /** Client-side navigation — the SSE connection is untouched, so facets persist. */
    fun navigate(route: String) {
        scope.launch {
            val screen = withContext(Dispatchers.IO) { fetchScreen(route) } ?: return@launch
            tree = screen.tree
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
                        connectTimeout = 5000
                        readTimeout = 0
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

    private fun handleFrame(frame: String) {
        val ev = try { json.decodeFromString<FacetEvent>(frame) } catch (_: Exception) { return }
        if (ev.op == "_conn") {
            connId = ev.conn
            flushPending()
            return
        }
        apply(ev)
    }

    private fun apply(ev: FacetEvent) {
        val cur = tree ?: return
        val id = ev.facet_id ?: return
        tree = when (ev.op) {
            "replace" -> ev.fragment?.let { cur.replacingFacet(id, FacetHtmlParser.parse(it)) } ?: cur
            "append" -> ev.fragment?.let { cur.insertingChild(id, FacetHtmlParser.parse(it), false) } ?: cur
            "prepend" -> ev.fragment?.let { cur.insertingChild(id, FacetHtmlParser.parse(it), true) } ?: cur
            "remove" -> cur.removingFacet(id)
            else -> cur
        }
    }

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

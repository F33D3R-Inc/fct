package dev.fct.facetkit

import kotlinx.serialization.Serializable
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/**
 * FacetMeta is one manifest entry's per-primitive runtime rules — the native
 * mirror of the web runtime's manifest registry: the kind, the stream `window:`,
 * the signal `ttl:`, and the client render body for vault (`decrypt:`) / media
 * (`source:`). The client fetches `/manifest.json` at start and keys these by
 * facet name.
 */
@Serializable
data class FacetMeta(
    val name: String,
    val kind: String = "facet",
    val window: String? = null,
    val ttl: String? = null,
    val client: String? = null,
) {
    val windowCount: Int get() = window?.toIntOrNull() ?: 0
    val ttlMs: Long get() = Primitives.goDurationMs(ttl ?: "")
}

@Serializable
internal data class FacetManifest(val facets: List<FacetMeta> = emptyList())

/**
 * The client side of the primitive taxonomy, mirroring fa-runtime.js: facet-id →
 * name resolution, Go duration parsing, the client render-body engine (escaped
 * `{field}` interpolation plus `{if}`/`{for}`) for trusted bodies, vault envelope
 * decryption, and media tag normalization. Pure functions, shared by FacetClient
 * and the tests.
 */
internal object Primitives {
    /** The facet name of a facet-id instance: "LikeButton:post:42" → "LikeButton". */
    fun facetName(id: String): String = id.substringBefore(':')

    private val duration = Regex("^(\\d+(?:\\.\\d+)?)(ms|s|m|h)$")

    /** Parses the simple Go durations the compiler accepts (200ms, 5s, 2m, 1h). */
    fun goDurationMs(s: String): Long {
        val m = duration.find(s) ?: return 0
        val n = m.groupValues[1].toDouble()
        return when (m.groupValues[2]) {
            "ms" -> n
            "s" -> n * 1000
            "m" -> n * 60_000
            else -> n * 3_600_000
        }.toLong()
    }

    /**
     * HTML-escapes a value before it is substituted into a client render body —
     * decrypted plaintext can never inject elements (web parity: the runtime
     * escapes, then parses).
     */
    fun escape(s: String): String = buildString(s.length) {
        for (c in s) when (c) {
            '&' -> append("&amp;")
            '<' -> append("&lt;")
            '>' -> append("&gt;")
            '"' -> append("&quot;")
            '\'' -> append("&#39;")
            else -> append(c)
        }
    }

    /**
     * Renders a TRUSTED client body (the compiled manifest's decrypt:/source:
     * template) against UNTRUSTED values. Interpolated values are HTML-ESCAPED;
     * literal text is not. Supports {field}/{a.b}, {if expr}…{else}…{end} and
     * {for v in path}…{end} — the exact behavior of the web runtime's fill().
     *
     * Values are structured (String / Boolean / Number / List<*> / Map<*, *>),
     * e.g. a vault's parsed JSON plaintext, so loops/conditions see real arrays
     * and nested objects.
     */
    @JvmName("fillAny")
    fun fill(body: String, values: Map<String, Any?>): String {
        val toks = tokenizeTpl(body)
        val nodes = parseBlock(toks, intArrayOf(0), stopElse = false)
        return renderTpl(nodes, values)
    }

    /** String-valued convenience overload (media data-* attributes are strings). */
    fun fill(body: String, values: Map<String, String>): String =
        fill(body, values.mapValues { it.value as Any? })

    /** A parsed client-body node. */
    private sealed interface TplNode {
        data class Text(val s: String) : TplNode
        data class Interp(val e: String) : TplNode
        data class Cond(val expr: String, val then: List<TplNode>, val els: List<TplNode>) : TplNode
        data class Loop(val v: String, val iter: String, val body: List<TplNode>) : TplNode
    }

    private sealed interface Tok {
        data class Text(val s: String) : Tok
        data class Interp(val e: String) : Tok
        data class IfTok(val e: String) : Tok
        object ElseTok : Tok
        object EndTok : Tok
        data class ForTok(val v: String, val iter: String) : Tok
    }

    private val forHead = Regex("^for\\s+([A-Za-z_]\\w*)\\s+in\\s+(.+)$")

    private fun tokenizeTpl(body: String): List<Tok> {
        val toks = mutableListOf<Tok>()
        var rest = body
        while (true) {
            val open = rest.indexOf('{')
            if (open < 0) { if (rest.isNotEmpty()) toks.add(Tok.Text(rest)); break }
            if (open > 0) toks.add(Tok.Text(rest.substring(0, open)))
            val close = rest.indexOf('}', open + 1)
            if (close < 0) { toks.add(Tok.Text(rest.substring(open))); break }
            val inner = rest.substring(open + 1, close).trim()
            when {
                inner.startsWith("if ") -> toks.add(Tok.IfTok(inner.substring(3).trim()))
                inner == "else" -> toks.add(Tok.ElseTok)
                inner == "end" -> toks.add(Tok.EndTok)
                inner.startsWith("for ") -> {
                    val m = forHead.find(inner)
                    if (m != null) toks.add(Tok.ForTok(m.groupValues[1], m.groupValues[2].trim()))
                    else toks.add(Tok.Text("{$inner}"))
                }
                else -> toks.add(Tok.Interp(inner))
            }
            rest = rest.substring(close + 1)
        }
        return toks
    }

    // i is a single-element cursor (pass-by-reference) into toks.
    private fun parseBlock(toks: List<Tok>, i: IntArray, stopElse: Boolean): List<TplNode> {
        val nodes = mutableListOf<TplNode>()
        while (i[0] < toks.size) {
            when (val tk = toks[i[0]]) {
                is Tok.EndTok -> return nodes
                is Tok.ElseTok -> { if (stopElse) return nodes; i[0]++ }
                is Tok.Text -> { i[0]++; nodes.add(TplNode.Text(tk.s)) }
                is Tok.Interp -> { i[0]++; nodes.add(TplNode.Interp(tk.e)) }
                is Tok.IfTok -> {
                    i[0]++
                    val then = parseBlock(toks, i, stopElse = true)
                    var els = emptyList<TplNode>()
                    if (i[0] < toks.size && toks[i[0]] is Tok.ElseTok) { i[0]++; els = parseBlock(toks, i, stopElse = false) }
                    if (i[0] < toks.size && toks[i[0]] is Tok.EndTok) i[0]++
                    nodes.add(TplNode.Cond(tk.e, then, els))
                }
                is Tok.ForTok -> {
                    i[0]++
                    val body = parseBlock(toks, i, stopElse = false)
                    if (i[0] < toks.size && toks[i[0]] is Tok.EndTok) i[0]++
                    nodes.add(TplNode.Loop(tk.v, tk.iter, body))
                }
            }
        }
        return nodes
    }

    private fun renderTpl(nodes: List<TplNode>, scope: Map<String, Any?>): String = buildString {
        for (n in nodes) when (n) {
            is TplNode.Text -> append(n.s)
            is TplNode.Interp -> evalExpr(n.e, scope)?.let { append(escape(stringify(it))) }
            is TplNode.Cond -> append(renderTpl(if (truthy(evalExpr(n.expr, scope))) n.then else n.els, scope))
            is TplNode.Loop -> (evalExpr(n.iter, scope) as? List<*>)?.forEach { item ->
                append(renderTpl(n.body, scope + (n.v to item)))
            }
        }
    }

    private val ops = listOf("==", "!=", "<=", ">=", "<", ">") // two-char first

    // evalExpr supports `lhs OP rhs` comparisons, a leading `!`, and bare operands
    // (literals or dotted paths). Mirrors the web runtime.
    fun evalExpr(expr: String, scope: Map<String, Any?>): Any? {
        val e = expr.trim()
        for (op in ops) {
            val at = e.indexOf(op)
            if (at >= 0) return compare(op, operand(e.substring(0, at), scope), operand(e.substring(at + op.length), scope))
        }
        if (e.startsWith("!")) return !truthy(evalExpr(e.substring(1), scope))
        return operand(e, scope)
    }

    private fun operand(s0: String, scope: Map<String, Any?>): Any? {
        val s = s0.trim()
        if (s == "true") return true
        if (s == "false") return false
        s.toDoubleOrNull()?.let { return it }
        if (s.length >= 2 && (s.first() == '"' || s.first() == '\'') && s.last() == s.first()) {
            return s.substring(1, s.length - 1)
        }
        var cur: Any? = scope
        for (seg in s.split('.')) {
            cur = (cur as? Map<*, *>)?.get(seg) ?: return null
        }
        return cur
    }

    private fun compare(op: String, a: Any?, b: Any?): Boolean {
        if (op == "==") return stringify(a) == stringify(b)
        if (op == "!=") return stringify(a) != stringify(b)
        val na = numberOf(a); val nb = numberOf(b)
        if (na != null && nb != null) return when (op) {
            "<" -> na < nb; "<=" -> na <= nb; ">" -> na > nb; else -> na >= nb
        }
        val sa = stringify(a); val sb = stringify(b)
        return when (op) { "<" -> sa < sb; "<=" -> sa <= sb; ">" -> sa > sb; else -> sa >= sb }
    }

    // truthy mirrors Go template emptiness: null/false/0/""/[]/{} are falsy.
    private fun truthy(v: Any?): Boolean = when (v) {
        null -> false
        is Boolean -> v
        is Number -> v.toDouble() != 0.0
        is String -> v.isNotEmpty()
        is List<*> -> v.isNotEmpty()
        is Map<*, *> -> v.isNotEmpty()
        else -> true
    }

    private fun numberOf(v: Any?): Double? = when (v) {
        is Number -> v.toDouble()
        is String -> v.toDoubleOrNull()
        is Boolean -> if (v) 1.0 else 0.0
        else -> null
    }

    private fun stringify(v: Any?): String = when (v) {
        null -> ""
        is Boolean -> if (v) "true" else "false"
        is Double -> if (v == Math.floor(v) && !v.isInfinite()) v.toLong().toString() else v.toString()
        else -> v.toString()
    }

    /**
     * A signal payload key that is safe to set as a data-* attribute (no
     * data-action / data-fa-* hijack) — mirrors the web runtime's guard.
     */
    fun safeSignalKey(k: String): Boolean =
        k != "action" && !k.lowercase().startsWith("fa") &&
            Regex("^[A-Za-z_]\\w*$").matches(k)

    /**
     * Decrypts a vault envelope — base64 of 12-byte IV ‖ ciphertext ‖ tag,
     * AES-GCM — with a key that exists only on this device. Returns null (and the
     * envelope stays put) on any failure: never render garbage.
     */
    fun decryptEnvelope(b64: String, key: ByteArray): String? = try {
        val data = base64Decode(b64)
        if (data == null || data.size < 28) {
            null
        } else {
            val cipher = Cipher.getInstance("AES/GCM/NoPadding")
            cipher.init(
                Cipher.DECRYPT_MODE,
                SecretKeySpec(key, "AES"),
                GCMParameterSpec(128, data, 0, 12),
            )
            String(cipher.doFinal(data, 12, data.size - 12), Charsets.UTF_8)
        }
    } catch (_: Exception) {
        null
    }

    /**
     * Normalizes a media `source:` body's transport tags (<hls>/<dash>) to
     * <video>, the element the parser and player understand.
     */
    fun normalizeMedia(body: String): String {
        var s = body
        for (tag in listOf("hls", "dash")) {
            s = s.replace("<$tag ", "<video ")
                .replace("<$tag/", "<video/")
                .replace("<$tag>", "<video>")
                .replace("</$tag>", "</video>")
        }
        return s
    }

    private const val B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

    /**
     * Minimal base64 decoder — hand-rolled because java.util.Base64 needs API 26
     * (minSdk is 24) and android.util.Base64 is absent from plain-JVM unit tests.
     */
    fun base64Decode(s: String): ByteArray? {
        val clean = s.trim().trimEnd('=')
        var buf = 0
        var bits = 0
        val out = ArrayList<Byte>(clean.length * 3 / 4 + 2)
        for (c in clean) {
            val v = B64.indexOf(c)
            if (v < 0) return null
            buf = (buf shl 6) or v
            bits += 6
            if (bits >= 8) {
                bits -= 8
                out.add(((buf shr bits) and 0xff).toByte())
            }
        }
        return out.toByteArray()
    }
}

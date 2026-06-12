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
 * name resolution, Go duration parsing, the escaped `{field}` interpolation for
 * trusted client render bodies, vault envelope decryption, and media tag
 * normalization. Pure functions, shared by FacetClient and the tests.
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

    private val hole = Regex("\\{\\s*([A-Za-z_]\\w*)\\s*\\}")

    /**
     * Substitutes `{field}` holes in a TRUSTED body (the compiled manifest's
     * client render body) with HTML-ESCAPED values. Field interpolation only — an
     * unknown field renders empty; a non-field hole ({if x} …) is left literal.
     * Exactly the web runtime's fill().
     */
    fun fill(body: String, values: Map<String, String>): String =
        hole.replace(body) { m ->
            val v = values[m.groupValues[1]]
            if (v != null) escape(v) else ""
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

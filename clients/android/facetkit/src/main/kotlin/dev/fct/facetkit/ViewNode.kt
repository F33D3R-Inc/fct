package dev.fct.facetkit

import kotlinx.serialization.Serializable

/**
 * Style is the server-resolved, platform-neutral layout + appearance of a node
 * (the Kotlin mirror of Go's `fa.Style`). The renderer reads this instead of
 * guessing from class names.
 */
@Serializable
data class Style(
    val direction: String? = null,
    val gap: Int? = null,
    val padT: Int? = null,
    val padR: Int? = null,
    val padB: Int? = null,
    val padL: Int? = null,
    val align: String? = null,
    val justify: String? = null,
    val grow: Boolean? = null,
    val width: String? = null,
    val height: String? = null,
    val bg: String? = null,
    val fg: String? = null,
    val fontSize: Int? = null,
    val fontWeight: Int? = null,
    val radius: Int? = null,
)

/**
 * ViewNode is the platform-neutral UI element the FA server emits (the Kotlin
 * mirror of Go's `fa.ViewNode`). A tree of these is everything the client needs to
 * render a screen natively — no HTML, no WebView.
 */
@Serializable
data class ViewNode(
    val kind: String,
    val tag: String? = null,
    val attrs: Map<String, String>? = null,
    val text: String? = null,
    val facetId: String? = null,
    val action: String? = null,
    val style: Style? = null,
    val children: List<ViewNode>? = null,
) {
    /** The data-* payload a tap carries back (mirrors the web runtime). */
    val actionPayload: Map<String, String>
        get() = (attrs ?: emptyMap()).mapNotNull { (k, v) ->
            if (!k.startsWith("data-")) return@mapNotNull null
            val name = k.removePrefix("data-")
            if (name == "action" || name == "facet-id" || name.startsWith("fa-")) null else name to v
        }.toMap()

    // Surgical tree updates (mirror the web runtime's DOM ops).

    fun replacingFacet(id: String, newNode: ViewNode): ViewNode =
        if (facetId == id) newNode
        else copy(children = children?.map { it.replacingFacet(id, newNode) })

    fun insertingChild(into: String, child: ViewNode, prepend: Boolean): ViewNode =
        if (facetId == into) {
            val kids = (children ?: emptyList()).toMutableList()
            if (prepend) kids.add(0, child) else kids.add(child)
            copy(children = kids)
        } else {
            copy(children = children?.map { it.insertingChild(into, child, prepend) })
        }

    fun removingFacet(id: String): ViewNode =
        copy(children = children?.filter { it.facetId != id }?.map { it.removingFacet(id) })

    /**
     * Returns a copy with the children of node [id] capped at [max] — the native
     * mirror of the web runtime's stream `window:` trim. After an append, excess
     * drops from the start (oldest first); after a prepend, from the end.
     */
    fun trimmingChildren(id: String, max: Int, dropFromStart: Boolean): ViewNode {
        val kids = children
        if (facetId == id && kids != null && kids.size > max) {
            return copy(children = if (dropFromStart) kids.takeLast(max) else kids.take(max))
        }
        return copy(children = kids?.map { it.trimmingChildren(id, max, dropFromStart) })
    }

    /**
     * Returns a copy with [transform] applied to every node, bottom-up. The
     * primitive scans (signal apply, vault decrypt, media mount) are tree maps.
     */
    fun mapping(transform: (ViewNode) -> ViewNode): ViewNode =
        transform(copy(children = children?.map { it.mapping(transform) }))
}

/** The JSON a route returns to a native client (`FA-Native: 1`). */
@Serializable
data class ScreenResponse(val title: String? = null, val tree: ViewNode)

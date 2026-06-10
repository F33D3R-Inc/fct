package dev.fct.facetkit

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import coil.compose.AsyncImage

/**
 * FacetView renders one neutral [ViewNode] (and its subtree) to native Compose. It
 * reads the SERVER-RESOLVED [Style] (direction/gap/pad/align/paint) so layout is
 * exact, not inferred. box → Column/Row, text → Text, button → clickable, image →
 * AsyncImage, input → OutlinedTextField, link → navigate, icon → placeholder.
 */
@Composable
fun FacetView(node: ViewNode, client: FacetClient) {
    val mod = styleModifier(node.style)
    when (node.kind) {
        "text" -> Text(
            text = node.text ?: collectText(node),
            color = node.style?.fg?.let { colorOf(it) } ?: Color.Unspecified,
            fontWeight = node.style?.fontWeight?.let { if (it >= 600) FontWeight.Bold else FontWeight.Normal },
            fontSize = node.style?.fontSize?.let { it.sp } ?: androidx.compose.ui.unit.TextUnit.Unspecified,
            modifier = mod,
        )

        "button" -> Row(
            modifier = mod.clickable { node.action?.let { client.send(it, node.actionPayload) } },
            horizontalArrangement = Arrangement.spacedBy((node.style?.gap ?: 6).dp),
            verticalAlignment = Alignment.CenterVertically,
        ) { children(node, client) }

        "image" -> AsyncImage(model = node.attrs?.get("src"), contentDescription = null, modifier = mod)

        "input" -> FacetField(node, client, mod)

        "link" -> Text(
            text = node.text ?: collectText(node),
            color = Color(0xFF1D9BF0),
            modifier = mod.clickable { node.attrs?.get("href")?.let { client.navigate(it) } },
        )

        "icon" -> Text("●", color = Color.Gray, modifier = mod)

        else -> box(node, client, mod) // "box"
    }
}

@Composable
private fun box(node: ViewNode, client: FacetClient, mod: Modifier) {
    val s = node.style
    val gap = (s?.gap ?: 6).dp
    if (s?.direction == "row") {
        Row(
            modifier = mod,
            horizontalArrangement = if (s.justify == "between") Arrangement.SpaceBetween else Arrangement.spacedBy(gap),
            verticalAlignment = if (s.align == "start") Alignment.Top else if (s.align == "end") Alignment.Bottom else Alignment.CenterVertically,
        ) { children(node, client) }
    } else {
        Column(
            modifier = mod,
            verticalArrangement = Arrangement.spacedBy(gap),
            horizontalAlignment = if (s?.align == "center") Alignment.CenterHorizontally else if (s?.align == "end") Alignment.End else Alignment.Start,
        ) { children(node, client) }
    }
}

@Composable
private fun children(node: ViewNode, client: FacetClient) {
    node.children?.forEach { child -> FacetView(child, client) }
}

private fun styleModifier(style: Style?): Modifier {
    var m: Modifier = Modifier
    val s = style ?: return m
    if (s.grow == true) m = m.fillMaxWidth()
    s.width?.let { m = applyDim(m, it, horizontal = true) }
    s.height?.let { m = applyDim(m, it, horizontal = false) }
    s.radius?.let { if (it > 0) m = m.clip(RoundedCornerShape(minOf(it, 28).dp)) }
    s.bg?.let { colorOf(it)?.let { c -> m = m.background(c) } }
    val l = s.padL ?: 0; val t = s.padT ?: 0; val r = s.padR ?: 0; val b = s.padB ?: 0
    if (l > 0 || t > 0 || r > 0 || b > 0) {
        m = m.padding(start = l.dp, top = t.dp, end = r.dp, bottom = b.dp)
    }
    return m
}

// applyDim applies an explicit dimension: px size, fill/100%, or a fraction.
// Compose renders fractional widths exactly via fillMaxWidth(fraction).
private fun applyDim(m: Modifier, value: String, horizontal: Boolean): Modifier = when {
    value == "fill" || value == "100%" -> if (horizontal) m.fillMaxWidth() else m.fillMaxHeight()
    value.endsWith("%") -> {
        val f = (value.dropLast(1).toFloatOrNull() ?: 100f) / 100f
        if (horizontal) m.fillMaxWidth(f) else m.fillMaxHeight(f)
    }
    else -> {
        val px = (if (value.endsWith("px")) value.dropLast(2) else value).toIntOrNull()
        if (px != null) (if (horizontal) m.width(px.dp) else m.height(px.dp)) else m
    }
}

private fun collectText(n: ViewNode): String =
    n.text ?: (n.children ?: emptyList()).joinToString("") { collectText(it) }

/** Parses "#rrggbb" / "#rgb" into a Compose Color (null for named colors). */
private fun colorOf(hex: String): Color? {
    var h = hex.trim()
    if (!h.startsWith("#")) return null
    h = h.removePrefix("#")
    if (h.length == 3) h = h.map { "$it$it" }.joinToString("")
    if (h.length != 6) return null
    val v = h.toLongOrNull(16) ?: return null
    return Color(
        red = ((v shr 16) and 0xff) / 255f,
        green = ((v shr 8) and 0xff) / 255f,
        blue = (v and 0xff) / 255f,
    )
}

@Composable
private fun FacetField(node: ViewNode, client: FacetClient, mod: Modifier) {
    var text by remember { mutableStateOf(node.attrs?.get("value") ?: "") }
    OutlinedTextField(
        value = text,
        onValueChange = { text = it },
        placeholder = { node.attrs?.get("placeholder")?.let { Text(it) } },
        modifier = mod,
    )
    // A submit pushes the value as the input's action (server-authoritative state).
}

/**
 * FacetScreen is the top-level composable: a loading spinner until the first tree
 * arrives, then the rendered tree with live updates applied.
 *
 * ```kotlin
 * setContent {
 *     val client = remember { FacetClient("https://app.example.com") }
 *     FacetScreen(client, route = "/")
 * }
 * ```
 */
@Composable
fun FacetScreen(client: FacetClient, route: String) {
    LaunchedEffect(Unit) { client.start(route) }
    val tree = client.tree
    if (tree == null) {
        CircularProgressIndicator()
    } else {
        Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
            FacetView(tree, client)
        }
    }
}

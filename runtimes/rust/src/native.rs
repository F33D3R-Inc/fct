//! Native (FA-Native) neutral-tree rendering — the Rust mirror of fa/view.go
//! (ParseView) and fa/style.go (resolveStyle). Renders HTML as for the web, then
//! parses it into a platform-neutral ViewNode tree (kinds box/text/button/image/
//! input/link/icon) — identical to Go's RenderTree = ParseView(Render(...)) — so
//! the SAME backend drives web and native. The ViewNode JSON matches Go
//! field-for-field (kind, tag, attrs, text, facetId, action, style, children;
//! attrs keys sorted) so a native client decodes it and HMAC verification over the
//! fragment string holds across runtimes.

use crate::json::Json;

fn kind_for(tag: &str) -> &'static str {
    match tag {
        "button" => "button",
        "a" => "link",
        "img" => "image",
        "input" | "textarea" | "select" => "input",
        "svg" => "icon",
        "span" | "p" | "strong" | "b" | "em" | "i" | "small" | "label" | "time" | "h1" | "h2"
        | "h3" | "h4" | "h5" | "h6" | "td" | "th" | "caption" => "text",
        _ => "box",
    }
}

fn is_void(tag: &str) -> bool {
    matches!(tag, "img" | "input" | "br" | "hr" | "meta" | "link" | "source" | "area" | "col")
}

// The std design-system class → style table (mirror of fa/style.go classStyles),
// encoded compactly as "field=value;..." and parsed by parse_partial.
const CLASS_STYLES: &[(&str, &str)] = &[
    ("fa-row", "direction=row;align=center;gap=8"),
    ("fa-post__header", "direction=row;gap=10"),
    ("fa-post__actions", "direction=row;justify=between"),
    ("fa-vidctl", "direction=row;align=center;gap=10;pad=8"),
    ("fa-engage", "direction=row;gap=8"),
    ("fa-feedtabs", "direction=row"),
    ("fa-tabs", "direction=row"),
    ("fa-storybar", "direction=row;gap=12;pad=12"),
    ("fa-catchips", "direction=row;gap=8"),
    ("fa-roomctl", "direction=row;align=center;gap=12;justify=center"),
    ("fa-composer", "direction=row;gap=10;pad=12"),
    ("fa-composer__bar", "direction=row;justify=between;align=center"),
    ("fa-composer__tools", "direction=row"),
    ("fa-setrow", "direction=row;justify=between;align=center;pad=12"),
    ("fa-bottomnav", "direction=row;justify=between;pad=8"),
    ("fa-spacebar", "direction=row;align=center;gap=10;pad=10"),
    ("fa-subrow", "direction=row;align=center;gap=10;pad=10"),
    ("fa-sresult", "direction=row;align=center;gap=10;pad=10"),
    ("fa-navrail__item", "direction=row;align=center;gap=14;pad=10"),
    ("fa-roomhead", "direction=row;align=center;gap=12;pad=12"),
    ("fa-topbar", "direction=row;align=center;justify=between;pad=10"),
    ("fa-vcard__row", "direction=row;gap=10"),
    ("fa-chatcompose", "direction=row;gap=6;pad=8"),
    ("fa-stack", "direction=column"),
    ("fa-card", "direction=column;pad=16;radius=12;gap=8"),
    ("fa-composer__main", "direction=column;gap=8;grow=1"),
    ("fa-vcard__meta", "direction=column"),
    ("fa-rrcard", "direction=column;pad=12;radius=16;gap=8"),
    ("fa-btn", "direction=row;align=center;pad=8;radius=999;fontWeight=600"),
    ("fa-btn--primary", "bg=#1d9bf0;fg=#ffffff"),
    ("fa-btn--secondary", "fg=#0f1419"),
    ("fa-btn--danger", "bg=#f4212e;fg=#ffffff"),
    ("fa-badge", "pad=4;radius=999;fontSize=12;fontWeight=700"),
    ("fa-pill", "pad=6;radius=999;fontSize=12;fontWeight=700"),
    ("fa-tip", "direction=row;align=center;gap=6;pad=8;radius=999;bg=#ffc107;fontWeight=800"),
    ("fa-post__name", "fontWeight=700"),
    ("fa-statcard__value", "fontSize=28;fontWeight=800"),
    ("fa-channelhead__name", "fontSize=24;fontWeight=800"),
];

// ── working style ───────────────────────────────────────────────────────────

#[derive(Default, Clone)]
struct Sty {
    direction: Option<String>,
    gap: Option<i64>,
    pad: Option<i64>,
    pad_t: Option<i64>,
    pad_r: Option<i64>,
    pad_b: Option<i64>,
    pad_l: Option<i64>,
    align: Option<String>,
    justify: Option<String>,
    grow: bool,
    width: Option<String>,
    height: Option<String>,
    bg: Option<String>,
    fg: Option<String>,
    font_size: Option<i64>,
    font_weight: Option<i64>,
    radius: Option<i64>,
}

impl Sty {
    fn is_zero(&self) -> bool {
        self.direction.is_none()
            && self.gap.is_none()
            && self.pad_t.is_none()
            && self.pad_r.is_none()
            && self.pad_b.is_none()
            && self.pad_l.is_none()
            && self.align.is_none()
            && self.justify.is_none()
            && !self.grow
            && self.width.is_none()
            && self.height.is_none()
            && self.bg.is_none()
            && self.fg.is_none()
            && self.font_size.is_none()
            && self.font_weight.is_none()
            && self.radius.is_none()
    }

    fn merge(&mut self, o: &Sty) {
        if o.direction.is_some() {
            self.direction = o.direction.clone();
        }
        if o.gap.is_some() {
            self.gap = o.gap;
        }
        if o.pad.is_some() {
            self.pad = o.pad;
        }
        if o.pad_t.is_some() {
            self.pad_t = o.pad_t;
        }
        if o.pad_r.is_some() {
            self.pad_r = o.pad_r;
        }
        if o.pad_b.is_some() {
            self.pad_b = o.pad_b;
        }
        if o.pad_l.is_some() {
            self.pad_l = o.pad_l;
        }
        if o.align.is_some() {
            self.align = o.align.clone();
        }
        if o.justify.is_some() {
            self.justify = o.justify.clone();
        }
        if o.grow {
            self.grow = true;
        }
        if o.width.is_some() {
            self.width = o.width.clone();
        }
        if o.height.is_some() {
            self.height = o.height.clone();
        }
        if o.bg.is_some() {
            self.bg = o.bg.clone();
        }
        if o.fg.is_some() {
            self.fg = o.fg.clone();
        }
        if o.font_size.is_some() {
            self.font_size = o.font_size;
        }
        if o.font_weight.is_some() {
            self.font_weight = o.font_weight;
        }
        if o.radius.is_some() {
            self.radius = o.radius;
        }
    }

    fn expand_pad(&mut self) {
        if let Some(p) = self.pad.take() {
            if self.pad_t.is_none() {
                self.pad_t = Some(p);
            }
            if self.pad_r.is_none() {
                self.pad_r = Some(p);
            }
            if self.pad_b.is_none() {
                self.pad_b = Some(p);
            }
            if self.pad_l.is_none() {
                self.pad_l = Some(p);
            }
        }
    }

    fn to_json(&self) -> Json {
        let mut p: Vec<(&str, Json)> = Vec::new();
        if let Some(v) = &self.direction {
            p.push(("direction", Json::Str(v.clone())));
        }
        if let Some(v) = self.gap {
            p.push(("gap", Json::Num(v as f64)));
        }
        if let Some(v) = self.pad_t {
            p.push(("padT", Json::Num(v as f64)));
        }
        if let Some(v) = self.pad_r {
            p.push(("padR", Json::Num(v as f64)));
        }
        if let Some(v) = self.pad_b {
            p.push(("padB", Json::Num(v as f64)));
        }
        if let Some(v) = self.pad_l {
            p.push(("padL", Json::Num(v as f64)));
        }
        if let Some(v) = &self.align {
            p.push(("align", Json::Str(v.clone())));
        }
        if let Some(v) = &self.justify {
            p.push(("justify", Json::Str(v.clone())));
        }
        if self.grow {
            p.push(("grow", Json::Bool(true)));
        }
        if let Some(v) = &self.width {
            p.push(("width", Json::Str(v.clone())));
        }
        if let Some(v) = &self.height {
            p.push(("height", Json::Str(v.clone())));
        }
        if let Some(v) = &self.bg {
            p.push(("bg", Json::Str(v.clone())));
        }
        if let Some(v) = &self.fg {
            p.push(("fg", Json::Str(v.clone())));
        }
        if let Some(v) = self.font_size {
            p.push(("fontSize", Json::Num(v as f64)));
        }
        if let Some(v) = self.font_weight {
            p.push(("fontWeight", Json::Num(v as f64)));
        }
        if let Some(v) = self.radius {
            p.push(("radius", Json::Num(v as f64)));
        }
        Json::obj(p)
    }
}

fn set_field(s: &mut Sty, key: &str, val: &str) {
    match key {
        "direction" => s.direction = Some(val.to_string()),
        "align" => s.align = Some(val.to_string()),
        "justify" => s.justify = Some(val.to_string()),
        "width" => s.width = Some(val.to_string()),
        "height" => s.height = Some(val.to_string()),
        "bg" => s.bg = Some(val.to_string()),
        "fg" => s.fg = Some(val.to_string()),
        "grow" => s.grow = val != "0" && !val.is_empty(),
        "gap" => s.gap = val.parse().ok(),
        "pad" => s.pad = val.parse().ok(),
        "padT" => s.pad_t = val.parse().ok(),
        "padR" => s.pad_r = val.parse().ok(),
        "padB" => s.pad_b = val.parse().ok(),
        "padL" => s.pad_l = val.parse().ok(),
        "fontSize" => s.font_size = val.parse().ok(),
        "fontWeight" => s.font_weight = val.parse().ok(),
        "radius" => s.radius = val.parse().ok(),
        _ => {}
    }
}

fn parse_partial(spec: &str) -> Sty {
    let mut s = Sty::default();
    for kv in spec.split(';') {
        if let Some((k, v)) = kv.split_once('=') {
            set_field(&mut s, k, v);
        }
    }
    s
}

fn class_style(c: &str) -> Option<Sty> {
    CLASS_STYLES.iter().find(|(name, _)| *name == c).map(|(_, spec)| parse_partial(spec))
}

fn resolve_style(tag: &str, attrs: &[(String, String)]) -> Option<Json> {
    let mut s = Sty::default();
    if tag == "button" || tag == "a" {
        s.direction = Some("row".into());
        s.align = Some("center".into());
    }
    if let Some(cls) = attr_get(attrs, "class") {
        for c in cls.split_whitespace() {
            if let Some(partial) = class_style(c) {
                s.merge(&partial);
            }
        }
    }
    if let Some(inline) = attr_get(attrs, "style") {
        apply_inline(&mut s, inline);
    }
    s.expand_pad();
    if s.is_zero() {
        None
    } else {
        Some(s.to_json())
    }
}

fn px(v: &str) -> i64 {
    let mut v = v.trim();
    if let Some(stripped) = v.strip_suffix("px") {
        v = stripped;
    }
    let v = v.trim();
    let v = v.split('.').next().unwrap_or(v);
    v.trim().parse().unwrap_or(0)
}

fn map_justify(v: &str) -> &'static str {
    match v {
        "center" => "center",
        "flex-end" | "end" => "end",
        "space-between" => "between",
        _ => "start",
    }
}

fn map_align(v: &str) -> &'static str {
    match v {
        "center" => "center",
        "flex-end" | "end" => "end",
        "stretch" => "stretch",
        _ => "start",
    }
}

fn set_padding(s: &mut Sty, val: &str) {
    let p: Vec<i64> = val.split_whitespace().map(px).collect();
    match p.len() {
        1 => {
            s.pad_t = Some(p[0]);
            s.pad_r = Some(p[0]);
            s.pad_b = Some(p[0]);
            s.pad_l = Some(p[0]);
        }
        2 => {
            s.pad_t = Some(p[0]);
            s.pad_b = Some(p[0]);
            s.pad_r = Some(p[1]);
            s.pad_l = Some(p[1]);
        }
        3 => {
            s.pad_t = Some(p[0]);
            s.pad_r = Some(p[1]);
            s.pad_l = Some(p[1]);
            s.pad_b = Some(p[2]);
        }
        4 => {
            s.pad_t = Some(p[0]);
            s.pad_r = Some(p[1]);
            s.pad_b = Some(p[2]);
            s.pad_l = Some(p[3]);
        }
        _ => {}
    }
}

fn apply_inline(s: &mut Sty, inline: &str) {
    for decl in inline.split(';') {
        let (prop, val) = match decl.split_once(':') {
            Some((p, v)) => (p.trim().to_lowercase(), v.trim()),
            None => continue,
        };
        match prop.as_str() {
            "width" => s.width = Some(val.to_string()),
            "height" => s.height = Some(val.to_string()),
            "background" | "background-color" => s.bg = Some(val.to_string()),
            "color" => s.fg = Some(val.to_string()),
            "padding" => set_padding(s, val),
            "padding-top" => s.pad_t = Some(px(val)),
            "padding-right" => s.pad_r = Some(px(val)),
            "padding-bottom" => s.pad_b = Some(px(val)),
            "padding-left" => s.pad_l = Some(px(val)),
            "border-radius" => s.radius = Some(px(val)),
            "font-size" => s.font_size = Some(px(val)),
            "font-weight" => {
                if let Ok(n) = val.parse::<i64>() {
                    s.font_weight = Some(n);
                } else if val == "bold" {
                    s.font_weight = Some(700);
                }
            }
            "gap" => s.gap = Some(px(val)),
            "flex-direction" => {
                if val == "row" || val == "column" {
                    s.direction = Some(val.to_string());
                }
            }
            "display" => {
                if val == "flex" && s.direction.is_none() {
                    s.direction = Some("row".into());
                }
            }
            "justify-content" => s.justify = Some(map_justify(val).to_string()),
            "align-items" => s.align = Some(map_align(val).to_string()),
            "flex" | "flex-grow" => {
                if let Some(first) = val.split_whitespace().next() {
                    if px(first) > 0 {
                        s.grow = true;
                    }
                }
            }
            _ => {}
        }
    }
}

fn attr_get<'a>(attrs: &'a [(String, String)], key: &str) -> Option<&'a str> {
    attrs.iter().find(|(k, _)| k == key).map(|(_, v)| v.as_str())
}

// ── ViewNode builder (fixed Go field order on serialize) ────────────────────

#[derive(Default)]
struct NodeB {
    kind: String,
    tag: Option<String>,
    attrs: Vec<(String, String)>,
    text: Option<String>,
    facet_id: Option<String>,
    action: Option<String>,
    style: Option<Json>,
    children: Vec<NodeB>,
}

impl NodeB {
    fn to_json(&self) -> Json {
        let mut p: Vec<(&str, Json)> = Vec::new();
        p.push(("kind", Json::Str(self.kind.clone())));
        if let Some(t) = &self.tag {
            p.push(("tag", Json::Str(t.clone())));
        }
        if !self.attrs.is_empty() {
            let mut keys: Vec<&(String, String)> = self.attrs.iter().collect();
            keys.sort_by(|a, b| a.0.cmp(&b.0));
            let obj = Json::Obj(keys.iter().map(|(k, v)| (k.clone(), Json::Str(v.clone()))).collect());
            p.push(("attrs", obj));
        }
        if let Some(t) = &self.text {
            p.push(("text", Json::Str(t.clone())));
        }
        if let Some(f) = &self.facet_id {
            p.push(("facetId", Json::Str(f.clone())));
        }
        if let Some(a) = &self.action {
            p.push(("action", Json::Str(a.clone())));
        }
        if let Some(st) = &self.style {
            p.push(("style", st.clone()));
        }
        if !self.children.is_empty() {
            p.push(("children", Json::Arr(self.children.iter().map(|c| c.to_json()).collect())));
        }
        Json::obj(p)
    }
}

fn node_from_tag(name: &str, attrs: Vec<(String, String)>) -> NodeB {
    let mut n = NodeB { kind: kind_for(name).to_string(), tag: Some(name.to_string()), ..Default::default() };
    n.facet_id = attr_get(&attrs, "data-facet-id").map(String::from);
    n.action = attr_get(&attrs, "data-action").map(String::from);
    n.style = resolve_style(name, &attrs);
    if !attrs.is_empty() {
        n.attrs = attrs;
    }
    n
}

fn fold_text(children: &[NodeB]) -> Option<String> {
    let mut out = String::new();
    for c in children {
        if c.kind != "text" || !c.children.is_empty() {
            return None;
        }
        out.push_str(c.text.as_deref().unwrap_or(""));
    }
    Some(out)
}

// ── HTML tokenizer → NodeB tree ─────────────────────────────────────────────

struct Parser {
    s: Vec<char>,
    i: usize,
}

impl Parser {
    fn parse_children(&mut self) -> Vec<NodeB> {
        let mut nodes = Vec::new();
        while self.i < self.s.len() {
            if self.s[self.i] == '<' {
                if self.starts_with("<!--") {
                    if let Some(end) = self.find_from("-->", self.i) {
                        self.i = end + 3;
                    } else {
                        self.i = self.s.len();
                    }
                    continue;
                }
                if self.i + 1 < self.s.len() && self.s[self.i + 1] == '/' {
                    self.read_close_tag();
                    return nodes;
                }
                let (name, attrs, self_close) = self.read_open_tag();
                let mut node = node_from_tag(&name, attrs);
                if name == "svg" {
                    if let Some(end) = self.find_ci("</svg>", self.i) {
                        self.i = end + 6;
                    } else {
                        self.i = self.s.len();
                    }
                } else if self_close || is_void(&name) {
                    // no children
                } else {
                    node.children = self.parse_children();
                    if node.kind == "text" {
                        if let Some(txt) = fold_text(&node.children) {
                            node.text = Some(txt);
                            node.children.clear();
                        }
                    }
                }
                nodes.push(node);
            } else {
                let start = self.i;
                while self.i < self.s.len() && self.s[self.i] != '<' {
                    self.i += 1;
                }
                let raw: String = self.s[start..self.i].iter().collect();
                let t = raw.trim();
                if !t.is_empty() {
                    nodes.push(NodeB { kind: "text".into(), text: Some(html_unescape(t)), ..Default::default() });
                }
            }
        }
        nodes
    }

    fn read_open_tag(&mut self) -> (String, Vec<(String, String)>, bool) {
        self.i += 1; // '<'
        let name = self.read_name();
        let mut attrs: Vec<(String, String)> = Vec::new();
        let mut self_close = false;
        while self.i < self.s.len() {
            self.skip_space();
            if self.i >= self.s.len() {
                break;
            }
            let c = self.s[self.i];
            if c == '/' {
                self_close = true;
                self.i += 1;
                continue;
            }
            if c == '>' {
                self.i += 1;
                break;
            }
            let an = self.read_name();
            if an.is_empty() {
                self.i += 1;
                continue;
            }
            let mut av = String::new();
            self.skip_space();
            if self.i < self.s.len() && self.s[self.i] == '=' {
                self.i += 1;
                self.skip_space();
                av = self.read_attr_value();
            }
            attrs.push((an.to_lowercase(), av));
        }
        (name, attrs, self_close)
    }

    fn read_close_tag(&mut self) {
        self.i += 2;
        self.read_name();
        if let Some(end) = self.find_from(">", self.i) {
            self.i = end + 1;
        } else {
            self.i = self.s.len();
        }
    }

    fn read_name(&mut self) -> String {
        let start = self.i;
        while self.i < self.s.len() {
            let c = self.s[self.i];
            if matches!(c, ' ' | '\t' | '\n' | '\r' | '>' | '/' | '=') {
                break;
            }
            self.i += 1;
        }
        self.s[start..self.i].iter().collect::<String>().to_lowercase()
    }

    fn read_attr_value(&mut self) -> String {
        if self.i >= self.s.len() {
            return String::new();
        }
        let q = self.s[self.i];
        if q == '"' || q == '\'' {
            self.i += 1;
            let start = self.i;
            while self.i < self.s.len() && self.s[self.i] != q {
                self.i += 1;
            }
            let v: String = self.s[start..self.i].iter().collect();
            if self.i < self.s.len() {
                self.i += 1;
            }
            return html_unescape(&v);
        }
        let start = self.i;
        while self.i < self.s.len() && !matches!(self.s[self.i], ' ' | '>' | '/') {
            self.i += 1;
        }
        self.s[start..self.i].iter().collect()
    }

    fn skip_space(&mut self) {
        while self.i < self.s.len() && matches!(self.s[self.i], ' ' | '\t' | '\n' | '\r') {
            self.i += 1;
        }
    }

    fn starts_with(&self, p: &str) -> bool {
        let pc: Vec<char> = p.chars().collect();
        if self.i + pc.len() > self.s.len() {
            return false;
        }
        self.s[self.i..self.i + pc.len()] == pc[..]
    }

    fn find_from(&self, needle: &str, from: usize) -> Option<usize> {
        let nc: Vec<char> = needle.chars().collect();
        let mut i = from;
        while i + nc.len() <= self.s.len() {
            if self.s[i..i + nc.len()] == nc[..] {
                return Some(i);
            }
            i += 1;
        }
        None
    }

    fn find_ci(&self, needle: &str, from: usize) -> Option<usize> {
        let nc: Vec<char> = needle.to_lowercase().chars().collect();
        let mut i = from;
        while i + nc.len() <= self.s.len() {
            let win: String = self.s[i..i + nc.len()].iter().collect::<String>().to_lowercase();
            if win.chars().collect::<Vec<_>>() == nc {
                return Some(i);
            }
            i += 1;
        }
        None
    }
}

fn html_unescape(s: &str) -> String {
    let chars: Vec<char> = s.chars().collect();
    let mut out = String::new();
    let mut i = 0;
    while i < chars.len() {
        if chars[i] == '&' {
            if let Some(semi) = chars[i + 1..].iter().position(|&c| c == ';') {
                let body: String = chars[i + 1..i + 1 + semi].iter().collect();
                let replaced = decode_entity(&body);
                if let Some(r) = replaced {
                    out.push_str(&r);
                    i = i + 1 + semi + 1;
                    continue;
                }
            }
        }
        out.push(chars[i]);
        i += 1;
    }
    out
}

fn decode_entity(body: &str) -> Option<String> {
    if body.is_empty() {
        return None;
    }
    let bytes: Vec<char> = body.chars().collect();
    if bytes[0] == '#' {
        let code = if bytes.len() > 1 && (bytes[1] == 'x' || bytes[1] == 'X') {
            u32::from_str_radix(&body[2..], 16).ok()?
        } else {
            body[1..].parse::<u32>().ok()?
        };
        return char::from_u32(code).map(|c| c.to_string());
    }
    match body {
        "amp" => Some("&".into()),
        "lt" => Some("<".into()),
        "gt" => Some(">".into()),
        "quot" => Some("\"".into()),
        "apos" => Some("'".into()),
        "nbsp" => Some(" ".into()),
        _ => None,
    }
}

/// parse_view_json renders an HTML fragment to a neutral ViewNode tree (as Json).
pub fn parse_view_json(fragment: &str) -> Json {
    let mut p = Parser { s: fragment.chars().collect(), i: 0 };
    let nodes = p.parse_children();
    match nodes.len() {
        0 => Json::obj(vec![("kind", Json::Str("box".into()))]),
        1 => nodes[0].to_json(),
        _ => {
            let kids = Json::Arr(nodes.iter().map(|n| n.to_json()).collect());
            Json::obj(vec![("kind", Json::Str("box".into())), ("children", kids)])
        }
    }
}

/// tree_json renders HTML → ViewNode tree → compact JSON string (the native fragment).
pub fn tree_json(fragment: &str) -> String {
    parse_view_json(fragment).to_string()
}

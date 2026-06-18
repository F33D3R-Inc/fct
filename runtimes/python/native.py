"""Native (FA-Native) neutral-tree rendering — the Python mirror of fa/view.go
(ParseView) and fa/style.go (resolveStyle).

A native client (FacetKit / facetkit) connects with ``FA-Native: 1`` and consumes
a platform-neutral ViewNode tree (kinds box/text/button/image/input/link/icon)
instead of HTML. We render HTML exactly as for the web, then parse it into that
tree — identical to Go's RenderTree = ParseView(Render(...)) — so the SAME backend
drives web and native. The ViewNode JSON matches Go field-for-field (kind, tag,
attrs, text, facetId, action, style, children; attrs keys sorted).
"""

import re

KIND_BY_TAG = {
    "button": "button", "a": "link", "img": "image",
    "input": "input", "textarea": "input", "select": "input", "svg": "icon",
}
TEXT_TAGS = {"span", "p", "strong", "b", "em", "i", "small", "label", "time",
             "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "caption"}
VOID_TAGS = {"img", "input", "br", "hr", "meta", "link", "source", "area", "col"}

CLASS_STYLES = {
    "fa-row": {"direction": "row", "align": "center", "gap": 8},
    "fa-post__header": {"direction": "row", "gap": 10},
    "fa-post__actions": {"direction": "row", "justify": "between"},
    "fa-vidctl": {"direction": "row", "align": "center", "gap": 10, "pad": 8},
    "fa-engage": {"direction": "row", "gap": 8},
    "fa-feedtabs": {"direction": "row"},
    "fa-tabs": {"direction": "row"},
    "fa-storybar": {"direction": "row", "gap": 12, "pad": 12},
    "fa-catchips": {"direction": "row", "gap": 8},
    "fa-roomctl": {"direction": "row", "align": "center", "gap": 12, "justify": "center"},
    "fa-composer": {"direction": "row", "gap": 10, "pad": 12},
    "fa-composer__bar": {"direction": "row", "justify": "between", "align": "center"},
    "fa-composer__tools": {"direction": "row"},
    "fa-setrow": {"direction": "row", "justify": "between", "align": "center", "pad": 12},
    "fa-bottomnav": {"direction": "row", "justify": "between", "pad": 8},
    "fa-spacebar": {"direction": "row", "align": "center", "gap": 10, "pad": 10},
    "fa-subrow": {"direction": "row", "align": "center", "gap": 10, "pad": 10},
    "fa-sresult": {"direction": "row", "align": "center", "gap": 10, "pad": 10},
    "fa-navrail__item": {"direction": "row", "align": "center", "gap": 14, "pad": 10},
    "fa-roomhead": {"direction": "row", "align": "center", "gap": 12, "pad": 12},
    "fa-topbar": {"direction": "row", "align": "center", "justify": "between", "pad": 10},
    "fa-vcard__row": {"direction": "row", "gap": 10},
    "fa-chatcompose": {"direction": "row", "gap": 6, "pad": 8},
    "fa-stack": {"direction": "column"},
    "fa-card": {"direction": "column", "pad": 16, "radius": 12, "gap": 8},
    "fa-composer__main": {"direction": "column", "gap": 8, "grow": True},
    "fa-vcard__meta": {"direction": "column"},
    "fa-rrcard": {"direction": "column", "pad": 12, "radius": 16, "gap": 8},
    "fa-btn": {"direction": "row", "align": "center", "pad": 8, "radius": 999, "fontWeight": 600},
    "fa-btn--primary": {"bg": "#1d9bf0", "fg": "#ffffff"},
    "fa-btn--secondary": {"fg": "#0f1419"},
    "fa-btn--danger": {"bg": "#f4212e", "fg": "#ffffff"},
    "fa-badge": {"pad": 4, "radius": 999, "fontSize": 12, "fontWeight": 700},
    "fa-pill": {"pad": 6, "radius": 999, "fontSize": 12, "fontWeight": 700},
    "fa-tip": {"direction": "row", "align": "center", "gap": 6, "pad": 8, "radius": 999, "bg": "#ffc107", "fontWeight": 800},
    "fa-post__name": {"fontWeight": 700},
    "fa-statcard__value": {"fontSize": 28, "fontWeight": 800},
    "fa-channelhead__name": {"fontSize": 24, "fontWeight": 800},
}

STYLE_ORDER = ["direction", "gap", "padT", "padR", "padB", "padL", "align",
               "justify", "grow", "width", "height", "bg", "fg", "fontSize", "fontWeight", "radius"]
NODE_ORDER = ["kind", "tag", "attrs", "text", "facetId", "action", "style", "children"]

_ENTITY_RE = re.compile(r"&(#x?[0-9a-fA-F]+|[a-zA-Z]+);")
_NAMED = {"amp": "&", "lt": "<", "gt": ">", "quot": '"', "apos": "'", "nbsp": " "}


def kind_for(tag):
    if tag in KIND_BY_TAG:
        return KIND_BY_TAG[tag]
    return "text" if tag in TEXT_TAGS else "box"


def html_unescape(s):
    def repl(m):
        body = m.group(1)
        if body[0] == "#":
            try:
                code = int(body[2:], 16) if body[1] in "xX" else int(body[1:])
                return chr(code)
            except ValueError:
                return m.group(0)
        return _NAMED.get(body, m.group(0))

    return _ENTITY_RE.sub(repl, s)


# -- HTML -> ViewNode tree ---------------------------------------------------

class _Parser:
    def __init__(self, s):
        self.s = s
        self.i = 0

    def parse_children(self, parent):
        nodes = []
        while self.i < len(self.s):
            if self.s[self.i] == "<":
                if self.s.startswith("<!--", self.i):
                    end = self.s.find("-->", self.i)
                    self.i = end + 3 if end >= 0 else len(self.s)
                    continue
                if self.i + 1 < len(self.s) and self.s[self.i + 1] == "/":
                    self.read_close_tag()
                    return nodes
                name, attrs, self_close = self.read_open_tag()
                node = node_from_tag(name, attrs)
                if name == "svg":
                    end = self.s.lower().find("</svg>", self.i)
                    self.i = end + 6 if end >= 0 else len(self.s)
                elif self_close or name in VOID_TAGS:
                    pass
                else:
                    node["children"] = self.parse_children(name)
                    if node["kind"] == "text":
                        folded = fold_text(node["children"])
                        if folded is not None:
                            node["text"] = folded
                            del node["children"]
                    if "children" in node and not node["children"]:
                        del node["children"]
                nodes.append(node)
            else:
                j = self.s.find("<", self.i)
                if j < 0:
                    raw = self.s[self.i:]
                    self.i = len(self.s)
                else:
                    raw = self.s[self.i:j]
                    self.i = j
                t = raw.strip()
                if t:
                    nodes.append({"kind": "text", "text": html_unescape(t)})
        return nodes

    def read_open_tag(self):
        self.i += 1
        name = self.read_name()
        attrs = {}
        self_close = False
        while self.i < len(self.s):
            self.skip_space()
            if self.i >= len(self.s):
                break
            c = self.s[self.i]
            if c == "/":
                self_close = True
                self.i += 1
                continue
            if c == ">":
                self.i += 1
                break
            an = self.read_name()
            if an == "":
                self.i += 1
                continue
            av = ""
            self.skip_space()
            if self.i < len(self.s) and self.s[self.i] == "=":
                self.i += 1
                self.skip_space()
                av = self.read_attr_value()
            attrs[an.lower()] = av
        return name, attrs, self_close

    def read_close_tag(self):
        self.i += 2
        self.read_name()
        j = self.s.find(">", self.i)
        self.i = j + 1 if j >= 0 else len(self.s)

    def read_name(self):
        start = self.i
        while self.i < len(self.s):
            c = self.s[self.i]
            if c in " \t\n\r></=":
                break
            self.i += 1
        return self.s[start:self.i].lower()

    def read_attr_value(self):
        if self.i >= len(self.s):
            return ""
        q = self.s[self.i]
        if q in "\"'":
            self.i += 1
            start = self.i
            while self.i < len(self.s) and self.s[self.i] != q:
                self.i += 1
            v = self.s[start:self.i]
            if self.i < len(self.s):
                self.i += 1
            return html_unescape(v)
        start = self.i
        while self.i < len(self.s) and self.s[self.i] not in " >/":
            self.i += 1
        return self.s[start:self.i]

    def skip_space(self):
        while self.i < len(self.s) and self.s[self.i] in " \t\n\r":
            self.i += 1


def parse_view(fragment):
    p = _Parser(fragment)
    nodes = p.parse_children("")
    if len(nodes) == 0:
        return {"kind": "box"}
    if len(nodes) == 1:
        return nodes[0]
    return {"kind": "box", "children": nodes}


def node_from_tag(name, attrs):
    n = {"kind": kind_for(name), "tag": name}
    if attrs:
        n["attrs"] = attrs
    if attrs.get("data-facet-id") is not None:
        n["facetId"] = attrs["data-facet-id"]
    if attrs.get("data-action") is not None:
        n["action"] = attrs["data-action"]
    st = resolve_style(name, attrs)
    if st:
        n["style"] = st
    return n


def fold_text(children):
    out = []
    for c in children or []:
        if c["kind"] != "text" or c.get("children"):
            return None
        out.append(c.get("text", ""))
    return "".join(out)


# -- style resolution --------------------------------------------------------

def resolve_style(tag, attrs):
    s = {}
    if tag in ("button", "a"):
        s["direction"] = "row"
        s["align"] = "center"
    cls = attrs.get("class")
    if cls:
        for c in cls.split():
            if c in CLASS_STYLES:
                _merge(s, CLASS_STYLES[c])
    if attrs.get("style"):
        _apply_inline(s, attrs["style"])
    _expand_pad(s)
    return _order_style(s) if not _is_zero(s) else None


def _merge(s, o):
    for k in ("direction", "align", "justify", "width", "height", "bg", "fg"):
        if o.get(k):
            s[k] = o[k]
    for k in ("gap", "pad", "padT", "padR", "padB", "padL", "fontSize", "fontWeight", "radius"):
        if o.get(k):
            s[k] = o[k]
    if o.get("grow"):
        s["grow"] = True


def _expand_pad(s):
    if s.get("pad"):
        for side in ("padT", "padR", "padB", "padL"):
            if not s.get(side):
                s[side] = s["pad"]
        del s["pad"]


def _is_zero(s):
    for k in STYLE_ORDER:
        if s.get(k):
            return False
    return True


def _order_style(s):
    out = {}
    for k in STYLE_ORDER:
        v = s.get(k)
        if k == "grow":
            if v:
                out["grow"] = True
        elif isinstance(v, str):
            if v:
                out[k] = v
        elif v:
            out[k] = v
    return out


def _apply_inline(s, inline):
    for decl in inline.split(";"):
        if ":" not in decl:
            continue
        prop, val = decl.split(":", 1)
        prop = prop.strip().lower()
        val = val.strip()
        if prop == "width":
            s["width"] = val
        elif prop == "height":
            s["height"] = val
        elif prop in ("background", "background-color"):
            s["bg"] = val
        elif prop == "color":
            s["fg"] = val
        elif prop == "padding":
            _set_padding(s, val)
        elif prop == "padding-top":
            s["padT"] = _px(val)
        elif prop == "padding-right":
            s["padR"] = _px(val)
        elif prop == "padding-bottom":
            s["padB"] = _px(val)
        elif prop == "padding-left":
            s["padL"] = _px(val)
        elif prop == "border-radius":
            s["radius"] = _px(val)
        elif prop == "font-size":
            s["fontSize"] = _px(val)
        elif prop == "font-weight":
            if val.isdigit():
                s["fontWeight"] = int(val)
            elif val == "bold":
                s["fontWeight"] = 700
        elif prop == "gap":
            s["gap"] = _px(val)
        elif prop == "flex-direction":
            if val in ("row", "column"):
                s["direction"] = val
        elif prop == "display":
            if val == "flex" and not s.get("direction"):
                s["direction"] = "row"
        elif prop == "justify-content":
            s["justify"] = _map_justify(val)
        elif prop == "align-items":
            s["align"] = _map_align(val)
        elif prop in ("flex", "flex-grow"):
            f = val.split()
            if f and _px(f[0]) > 0:
                s["grow"] = True


def _set_padding(s, val):
    p = [_px(x) for x in val.split()]
    if len(p) == 1:
        s["padT"] = s["padR"] = s["padB"] = s["padL"] = p[0]
    elif len(p) == 2:
        s["padT"] = s["padB"] = p[0]
        s["padR"] = s["padL"] = p[1]
    elif len(p) == 3:
        s["padT"] = p[0]
        s["padR"] = s["padL"] = p[1]
        s["padB"] = p[2]
    elif len(p) == 4:
        s["padT"], s["padR"], s["padB"], s["padL"] = p[0], p[1], p[2], p[3]


def _px(v):
    v = v.strip()
    if v.endswith("px"):
        v = v[:-2]
    if "." in v:
        v = v[:v.index(".")]
    try:
        return int(v.strip())
    except ValueError:
        return 0


def _map_justify(v):
    if v == "center":
        return "center"
    if v in ("flex-end", "end"):
        return "end"
    if v == "space-between":
        return "between"
    return "start"


def _map_align(v):
    if v == "center":
        return "center"
    if v in ("flex-end", "end"):
        return "end"
    if v == "stretch":
        return "stretch"
    return "start"


# -- serialization (Go field order; sorted attrs) ----------------------------

def node_to_json(node):
    out = {}
    for k in NODE_ORDER:
        if node.get(k) is None:
            continue
        if k == "attrs":
            keys = sorted(node["attrs"].keys())
            if not keys:
                continue
            out["attrs"] = {ak: node["attrs"][ak] for ak in keys}
        elif k == "children":
            if not node["children"]:
                continue
            out["children"] = [node_to_json(c) for c in node["children"]]
        else:
            out[k] = node[k]
    return out

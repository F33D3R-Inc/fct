"""Facet Architecture - Python server runtime.

The Python analogue of the Go ``fa`` package: it turns the compiler's neutral
output (``manifest.json`` + ``render.json``, from ``fct build`` with
``target = "python"``) into a live app - SSE transport, HMAC-signed event push,
the render-IR interpreter, and the ``/events`` router. The wire format, signing
layout, and client runtime are shared with every other target (see
docs/BACKENDS.md, fa/wire.go, fa/event.go).

Zero dependencies: standard library only.
"""

import hashlib
import hmac
import html
import json
import os
import queue
import re
import secrets
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import native

WIRE_VERSION = "1"


def dumps(obj):
    """Compact JSON, matching the wire bytes the other runtimes emit (no spaces)."""
    return json.dumps(obj, separators=(",", ":"))


# -- render IR -> block tree -------------------------------------------------
# The flat op stream is parsed once into a nested tree for a clean recursive walk.

def build_tree(ops):
    pos = [0]

    def block():
        nodes = []
        while pos[0] < len(ops):
            op = ops[pos[0]]
            if op["op"] in ("end", "else"):
                return nodes
            pos[0] += 1
            kind = op["op"]
            if kind == "text":
                nodes.append({"t": "text", "v": op.get("v", "")})
            elif kind == "expr":
                nodes.append({"t": "expr", "x": op["x"]})
            elif kind == "child":
                nodes.append({"t": "child", "name": op["name"], "props": op.get("props", [])})
            elif kind == "if":
                then_b = block()
                els_b = []
                if pos[0] < len(ops) and ops[pos[0]]["op"] == "else":
                    pos[0] += 1
                    els_b = block()
                if pos[0] < len(ops) and ops[pos[0]]["op"] == "end":
                    pos[0] += 1
                nodes.append({"t": "if", "x": op["x"], "then": then_b, "els": els_b})
            elif kind == "for":
                body = block()
                if pos[0] < len(ops) and ops[pos[0]]["op"] == "end":
                    pos[0] += 1
                nodes.append({"t": "for", "var": op["var"], "x": op["x"], "body": body})
        return nodes

    return block()


# -- expression evaluator (neutral irExpr AST) -------------------------------

def eval_expr(x, scope):
    k = x["k"]
    if k == "num":
        n = x.get("n", "0")
        return float(n) if "." in n else int(n)
    if k == "str":
        return x.get("s", "")
    if k == "bool":
        return bool(x.get("b", False))
    if k == "path":
        return eval_path(x, scope)
    if k == "call":
        fn = eval_path(x["recv"], scope)
        args = [eval_expr(a, scope) for a in x.get("args", [])]
        return fn(*args) if callable(fn) else None
    if k == "unary":
        if x["op"] == "!":
            return not truthy(eval_expr(x["x"], scope))
        return -_num(eval_expr(x["x"], scope))
    if k == "bin":
        return eval_bin(x["op"], eval_expr(x["l"], scope), eval_expr(x["r"], scope))
    return None


def eval_path(x, scope):
    segs = x.get("segs", [])
    src = scope["locals"] if x.get("local") else scope["data"]
    cur = _get(src, segs[0])
    for seg in segs[1:]:
        if cur is None:
            return None
        cur = _get(cur, seg)
    return cur


def _get(obj, key):
    if isinstance(obj, dict):
        return obj.get(key)
    return getattr(obj, key, None)


def eval_bin(op, a, b):
    if op == "==":
        return a == b
    if op == "!=":
        return a != b
    if op == "<":
        return _num(a) < _num(b)
    if op == "<=":
        return _num(a) <= _num(b)
    if op == ">":
        return _num(a) > _num(b)
    if op == ">=":
        return _num(a) >= _num(b)
    if op == "&&":
        return truthy(a) and truthy(b)
    if op == "||":
        return a if truthy(a) else b
    if op == "+":
        if isinstance(a, str) or isinstance(b, str):
            return _fmt(a) + _fmt(b)
        return _num(a) + _num(b)
    if op == "-":
        return _num(a) - _num(b)
    if op == "*":
        return _num(a) * _num(b)
    if op == "/":
        return _num(a) / _num(b)
    if op == "%":
        return _num(a) % _num(b)
    return None


def _num(v):
    if isinstance(v, bool):
        return 1 if v else 0
    if isinstance(v, (int, float)):
        return v
    try:
        return float(v)
    except (TypeError, ValueError):
        return 0


def truthy(v):
    return bool(v)


def _fmt(v):
    if v is None:
        return ""
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)


def html_escape(s):
    return html.escape(_fmt(s), quote=True)


# -- renderer ----------------------------------------------------------------

def render_nodes(nodes, scope, app):
    out = []
    for n in nodes:
        t = n["t"]
        if t == "text":
            out.append(n["v"])
        elif t == "expr":
            out.append(html_escape(eval_expr(n["x"], scope)))
        elif t == "if":
            branch = n["then"] if truthy(eval_expr(n["x"], scope)) else n["els"]
            out.append(render_nodes(branch, scope, app))
        elif t == "for":
            items = eval_expr(n["x"], scope) or []
            for item in items:
                locals_ = dict(scope["locals"])
                locals_[n["var"]] = item
                out.append(render_nodes(n["body"], {"data": scope["data"], "locals": locals_}, app))
        elif t == "child":
            child_data = {}
            for p in n["props"]:
                child_data[p["name"]] = eval_expr(p["x"], scope) if "x" in p and p["x"] else p.get("lit", "")
            out.append(app.render_facet(n["name"], child_data)["html"])
    return "".join(out)


_TAG_RE = re.compile(r"<([a-zA-Z][\w-]*)")
_HOLE_RE = re.compile(r"\{([^}]+)\}")


def resolve_facet_id(pattern, data):
    def repl(m):
        segs = m.group(1).split(".")
        cur = _get(data, segs[0])
        for seg in segs[1:]:
            if cur is None:
                break
            cur = _get(cur, seg)
        return _fmt(cur)

    return _HOLE_RE.sub(repl, pattern)


def inject_facet_id(html_str, fid):
    return _TAG_RE.sub(lambda m: m.group(0) + ' data-facet-id="%s"' % fid, html_str, count=1)


# -- signing (mirrors fa/event.go sign(): op \0 facet_id \0 fragment) --------

def sign_event(key_hex, ev):
    if not key_hex:
        return ev
    mac = hmac.new(bytes.fromhex(key_hex), digestmod=hashlib.sha256)
    mac.update(ev["op"].encode())
    mac.update(b"\x00")
    mac.update(ev["facet_id"].encode())
    mac.update(b"\x00")
    mac.update(ev["fragment"].encode())
    ev["hmac"] = mac.hexdigest()
    return ev


# -- App ---------------------------------------------------------------------

class App:
    def __init__(self, gen_dir="generated", fa_key=None, title="FA", runtime_js=None):
        self.gen_dir = gen_dir
        self.key_hex = fa_key if fa_key is not None else os.environ.get("FA_SIGNING_KEY", "")
        self.title = title
        here = os.path.dirname(os.path.abspath(__file__))
        self.runtime_js = runtime_js or os.path.join(here, "..", "..", "runtime", "fa-runtime.js")
        with open(os.path.join(gen_dir, "manifest.json")) as f:
            self.manifest = json.load(f)
        with open(os.path.join(gen_dir, "render.json")) as f:
            self.ir = json.load(f)
        self.trees = {}
        self.facets = {}
        for fac in self.ir["facets"]:
            self.facets[fac["name"]] = fac
            self.trees[fac["name"]] = build_tree(fac["render"])
        self.handlers = {}
        self._root = None
        self.conns = {}  # conn id -> queue.Queue
        self._lock = threading.Lock()

    def root(self, name, data_fn):
        self._root = (name, data_fn)
        return self

    def on(self, event_type, fn):
        self.handlers[event_type] = fn
        return self

    def render_facet(self, name, data):
        tree = self.trees.get(name)
        if tree is None:
            raise KeyError("unknown facet " + name)
        fid = resolve_facet_id(self.facets[name]["facet_id"], data)
        body = render_nodes(tree, {"data": data, "locals": {}}, self)
        return {"facet_id": fid, "html": inject_facet_id(body, fid)}

    def tree_fragment(self, html_str):
        """The neutral ViewNode tree of a rendered facet, as a JSON string (the
        native fragment) — mirror of Go's ParseView."""
        return dumps(native.node_to_json(native.parse_view(html_str)))

    def push_rerender(self, event_type, conn, data):
        c = self.conns.get(conn)
        if c is None:
            return
        for fac in self.ir["facets"]:
            for w in fac.get("when", []):
                if event_type not in w["events"]:
                    continue
                for mu in w["mutations"]:
                    target = mu.get("target") or fac["name"]
                    r = self.render_facet(target, data)
                    op = "replace" if mu["op"] == "replace_all" else mu["op"]
                    # Native connection → ViewNode-tree fragment; web → HTML.
                    fragment = self.tree_fragment(r["html"]) if c["native"] else r["html"]
                    ev = sign_event(self.key_hex, {"op": op, "facet_id": r["facet_id"], "fragment": fragment})
                    c["q"].put("data: " + dumps(ev) + "\n\n")

    def listen(self, addr=None):
        addr = addr or os.environ.get("FA_ADDR", "localhost:7373")
        host, _, port = addr.partition(":")
        app = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                p = self.path.split("?")[0]
                if p == "/":
                    return app._serve_shell(self)
                if p == "/sse":
                    return app._serve_sse(self)
                if p == "/manifest.json":
                    return app._json(self, app.manifest)
                if p == "/render.json":
                    return app._json(self, app.ir)
                if p == "/fa-runtime.js":
                    with open(app.runtime_js, "rb") as f:
                        body = f.read()
                    self.send_response(200)
                    self.send_header("Content-Type", "text/javascript")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                self.send_response(404)
                self.end_headers()

            def do_POST(self):
                if self.path.split("?")[0] == "/events":
                    return app._handle_events(self)
                self.send_response(404)
                self.end_headers()

        server = ThreadingHTTPServer((host, int(port)), Handler)
        print("fa(python): listening on http://%s:%s" % (host, port))
        server.serve_forever()

    # -- handlers ------------------------------------------------------------
    def _serve_shell(self, h):
        # Native client (FA-Native: 1) → ScreenResponse {title, tree}; browser → HTML.
        if h.headers.get("FA-Native") == "1":
            tree = {"kind": "box"}
            if self._root:
                name, data_fn = self._root
                html_str = self.render_facet(name, data_fn({}))["html"]
                tree = native.node_to_json(native.parse_view(html_str))
            return self._json(h, {"title": self.title, "tree": tree})
        body = ""
        if self._root:
            name, data_fn = self._root
            body = self.render_facet(name, data_fn({}))["html"]
        page = ('<!doctype html><html><head><meta charset="utf-8">'
                '<meta name="fa-key" content="%s"><title>%s</title></head><body>%s'
                '<script src="/fa-runtime.js"></script></body></html>'
                % (self.key_hex, html_escape(self.title), body))
        data = page.encode()
        h.send_response(200)
        h.send_header("Content-Type", "text/html; charset=utf-8")
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data)

    def _serve_sse(self, h):
        v = "1"
        if "?" in h.path:
            for kv in h.path.split("?")[1].split("&"):
                if kv.startswith("v="):
                    v = kv[2:]
        if v != WIRE_VERSION:
            h.send_response(426)
            h.end_headers()
            return
        h.send_response(200)
        h.send_header("Content-Type", "text/event-stream")
        h.send_header("Cache-Control", "no-cache")
        h.send_header("Connection", "keep-alive")
        h.send_header("X-Accel-Buffering", "no")
        h.end_headers()
        cid = secrets.token_hex(16)
        q = queue.Queue()
        with self._lock:
            self.conns[cid] = {"q": q, "native": h.headers.get("FA-Native") == "1"}
        hello = dumps({"op": "_conn", "conn": cid, "key": self.key_hex, "v": WIRE_VERSION})
        try:
            h.wfile.write(("data: %s\n\n" % hello).encode())
            h.wfile.flush()
            while True:
                try:
                    msg = q.get(timeout=25)
                except queue.Empty:
                    msg = ": keepalive\n\n"
                h.wfile.write(msg.encode())
                h.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            with self._lock:
                self.conns.pop(cid, None)

    def _handle_events(self, h):
        length = int(h.headers.get("Content-Length", 0))
        raw = h.rfile.read(length) if length else b"{}"
        h.send_response(204)
        h.end_headers()
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            return
        fn = self.handlers.get(body.get("type"))
        if not fn:
            return
        ctx = {"type": body.get("type"), "payload": body.get("payload") or {}, "conn": body.get("conn")}
        data = fn(ctx)
        if data is None:
            return
        self.push_rerender(ctx["type"], ctx["conn"], data)

    def _json(self, h, obj):
        data = dumps(obj).encode()
        h.send_response(200)
        h.send_header("Content-Type", "application/json")
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data)

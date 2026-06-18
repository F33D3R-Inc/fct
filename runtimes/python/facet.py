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

import framework as fw
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
                out.append(render_nodes(n["body"], {"data": scope["data"], "locals": locals_, "view": scope.get("view")}, app))
        elif t == "child":
            child_data = {}
            for p in n["props"]:
                child_data[p["name"]] = eval_expr(p["x"], scope) if "x" in p and p["x"] else p.get("lit", "")
            out.append(app.render_facet(n["name"], child_data, scope.get("view"))["html"])
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
        self.conns = {}  # conn id -> {"q", "native", "identity"}
        self._lock = threading.Lock()

        # Framework surface (uniform with fa/).
        self.policies = {}
        self._identity_fn = None
        self._sessions = None
        self.limiter = fw.RateLimiter(20, 40)
        self._broker = fw.LocalBroker()
        self._broker.subscribe(self._deliver_local)
        self.metrics = fw.Metrics()
        self._auth = None
        self._admin = None
        self._draining = False

    def auth(self):
        if self._auth is None:
            self._auth = fw.Auth()
        return self._auth

    def admin(self, authorize=None, title=None, resources=None):
        self._admin = {
            "authorize": authorize or (lambda req: False),
            "title": title or (self.title + " · admin"),
            "resources": resources or [],
        }
        return self

    def root(self, name, data_fn):
        self._root = (name, data_fn)
        return self

    def on(self, event_type, fn):
        self.handlers[event_type] = fn
        return self

    def identify(self, fn):
        self._identity_fn = fn
        return self

    def policy(self, name, fn):
        self.policies[name] = fn
        return self

    def sessions(self, **opts):
        self._sessions = fw.Sessions(self.key_hex, **opts)
        if self._identity_fn is None:
            self._identity_fn = lambda req: self._sessions.identity(req.get("cookie"))
        return self._sessions

    def broker(self, b):
        self._broker = b
        self._broker.subscribe(self._deliver_local)
        return self

    def view(self, req):
        return {"identity": (self._identity_fn(req) if self._identity_fn else "") or "", "req": req}

    def new_form(self, content_type, body):
        return fw.Form.parse(content_type, body)

    def render_facet(self, name, data, view=None):
        tree = self.trees.get(name)
        if tree is None:
            raise KeyError("unknown facet " + name)
        view = view or {"identity": ""}
        allowed, data = fw.enforce_who(self.facets[name].get("who"), self.policies, view, data)
        if not allowed:
            return {"facet_id": "", "html": ""}
        fid = resolve_facet_id(self.facets[name]["facet_id"], data)
        body = render_nodes(tree, {"data": data, "locals": {}, "view": view}, self)
        return {"facet_id": fid, "html": inject_facet_id(body, fid)}

    def tree_fragment(self, html_str):
        """The neutral ViewNode tree of a rendered facet, as a JSON string (the
        native fragment) — mirror of Go's ParseView."""
        return dumps(native.node_to_json(native.parse_view(html_str)))

    def push_rerender(self, event_type, conn, data):
        c = self.conns.get(conn)
        is_native = c["native"] if c else False
        view = {"identity": c["identity"] if c else ""}
        frames = []
        for fac in self.ir["facets"]:
            for w in fac.get("when", []):
                if event_type not in w["events"]:
                    continue
                for mu in w["mutations"]:
                    target = mu.get("target") or fac["name"]
                    r = self.render_facet(target, data, view)
                    if not r["facet_id"] and not r["html"]:
                        continue  # who: denied
                    op = "replace" if mu["op"] == "replace_all" else mu["op"]
                    fragment = self.tree_fragment(r["html"]) if is_native else r["html"]
                    ev = sign_event(self.key_hex, {"op": op, "facet_id": r["facet_id"], "fragment": fragment})
                    frames.append(ev)
        if frames:
            self.metrics.events_out += len(frames)
            self._broker.publish(dumps({"conn": conn, "frames": frames}))

    def _deliver_local(self, msg):
        try:
            m = json.loads(msg)
        except ValueError:
            return
        c = self.conns.get(m["conn"])
        if c is None:
            return
        for ev in m["frames"]:
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
                if p == "/healthz":
                    return app._text(self, 200, "ok")
                if p == "/readyz":
                    return app._text(self, 503 if app._draining else 200, "draining" if app._draining else "ready")
                if p == "/debug/metrics":
                    return app._json(self, app.metrics.snapshot())
                if p == "/metrics":
                    return app._text(self, 200, app.metrics.prometheus(), "text/plain; version=0.0.4; charset=utf-8")
                if p == "/admin":
                    return app._serve_admin(self)
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
    @staticmethod
    def _req(h):
        return {"cookie": h.headers.get("Cookie"), "origin": h.headers.get("Origin"), "host": h.headers.get("Host")}

    def _serve_shell(self, h):
        view = self.view(self._req(h))
        # Native client (FA-Native: 1) → ScreenResponse {title, tree}; browser → HTML.
        if h.headers.get("FA-Native") == "1":
            tree = {"kind": "box"}
            if self._root:
                name, data_fn = self._root
                html_str = self.render_facet(name, data_fn(view), view)["html"]
                tree = native.node_to_json(native.parse_view(html_str))
            return self._json(h, {"title": self.title, "tree": tree})
        body = ""
        if self._root:
            name, data_fn = self._root
            body = self.render_facet(name, data_fn(view), view)["html"]
        page = ('<!doctype html><html><head><meta charset="utf-8">'
                '<meta name="fa-key" content="%s"><title>%s</title></head><body>%s'
                '<script src="/fa-runtime.js"></script></body></html>'
                % (self.key_hex, html_escape(self.title), body))
        data = page.encode()
        h.send_response(200)
        h.send_header("Content-Type", "text/html; charset=utf-8")
        h.send_header("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
        h.send_header("X-Content-Type-Options", "nosniff")
        h.send_header("Referrer-Policy", "same-origin")
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
        ident = (self._identity_fn(self._req(h)) if self._identity_fn else "") or ""
        with self._lock:
            self.conns[cid] = {"q": q, "native": h.headers.get("FA-Native") == "1", "identity": ident}
            self.metrics.conns_active += 1
            self.metrics.conns_total += 1
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
                self.metrics.conns_active -= 1

    def _handle_events(self, h):
        # CSRF defense-in-depth (cross-origin reject) + per-IP throttle.
        if not fw.same_origin(h.headers.get("Origin"), h.headers.get("Host")):
            self.metrics.forbidden += 1
            h.send_response(403)
            h.end_headers()
            return
        ip = h.client_address[0] if h.client_address else ""
        if not self.limiter.allow(ip):
            self.metrics.rate_limited += 1
            h.send_response(429)
            h.end_headers()
            return
        self.metrics.events_in += 1
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
        c = self.conns.get(body.get("conn"))
        ctx = {"type": body.get("type"), "payload": body.get("payload") or {}, "conn": body.get("conn"), "identity": c["identity"] if c else ""}
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

    def _text(self, h, status, text, content_type="text/plain; charset=utf-8"):
        data = text.encode()
        h.send_response(status)
        h.send_header("Content-Type", content_type)
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data)

    def _serve_admin(self, h):
        # Deny-by-default admin panel: live metrics, open connections, resources.
        adm = self._admin
        from urllib.parse import parse_qs, urlparse as _urlparse
        if adm is None or not adm["authorize"](self._req(h)):
            return self._text(h, 403, "forbidden")
        params = parse_qs(_urlparse(h.path).query)
        esc = html_escape
        m = self.metrics.snapshot()
        body = "<h1>%s</h1>" % esc(adm["title"])
        body += "<h2>Metrics</h2><ul>" + "".join("<li>%s: <b>%d</b></li>" % (k, v) for k, v in m.items()) + "</ul>"
        body += "<p>Open connections: <b>%d</b></p>" % len(self.conns)
        res_name = params.get("resource", [None])[0]
        resource = next((r for r in adm["resources"] if r["name"] == res_name), None)
        if resource and params.get("id"):
            fields = resource.get("get", lambda i: [])(params["id"][0])
            body += "<h2>%s · %s</h2><table>" % (esc(resource["label"]), esc(params["id"][0]))
            body += "".join("<tr><th>%s</th><td>%s</td></tr>" % (esc(f["label"]), esc(f["value"])) for f in fields) + "</table>"
        elif resource:
            rows = resource.get("list", lambda: [])()
            body += "<h2>%s</h2><table><tr>%s</tr>" % (esc(resource["label"]), "".join("<th>%s</th>" % esc(c) for c in resource.get("columns", [])))
            for row in rows:
                cells = "".join("<td>%s</td>" % esc(c) for c in row["cells"])
                body += '<tr>%s <td><a href="/admin?resource=%s&id=%s">view</a></td></tr>' % (cells, resource["name"], row["id"])
            body += "</table>"
        else:
            body += "<h2>Resources</h2><ul>" + "".join('<li><a href="/admin?resource=%s">%s</a></li>' % (r["name"], esc(r["label"])) for r in adm["resources"]) + "</ul>"
        self._text(h, 200, "<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body>%s</body></html>" % (esc(adm["title"]), body), "text/html; charset=utf-8")

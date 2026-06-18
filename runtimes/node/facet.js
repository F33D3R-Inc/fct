'use strict';
// Facet Architecture — Node.js server runtime.
//
// The Node analogue of the Go `fa/` package: it turns the compiler's neutral
// output (manifest.json + render.json, from `fct build` with target = "node")
// into a live app. It owns the SSE transport, HMAC-signed event push, the
// render-IR interpreter, and the /events router — app code never hand-writes
// streaming or signing. The wire format, signing layout, and client runtime are
// shared with every other target (see docs/BACKENDS.md, fa/wire.go, fa/event.go).
//
// Zero dependencies: only Node built-ins (http, crypto, fs).

const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const native = require('./native');

const WIRE_VERSION = '1';

// ── render IR → block tree ──────────────────────────────────────────────────
// The flat op stream (text/expr/if/else/end/for/child) is parsed once into a
// nested tree so rendering is a clean recursive walk.

function buildTree(ops) {
  let pos = 0;
  function block() {
    const nodes = [];
    while (pos < ops.length) {
      const op = ops[pos];
      if (op.op === 'end' || op.op === 'else') return nodes;
      pos++;
      switch (op.op) {
        case 'text': nodes.push({ t: 'text', v: op.v }); break;
        case 'expr': nodes.push({ t: 'expr', x: op.x }); break;
        case 'child': nodes.push({ t: 'child', name: op.name, props: op.props || [] }); break;
        case 'if': {
          const thenB = block();
          let elseB = [];
          if (ops[pos] && ops[pos].op === 'else') { pos++; elseB = block(); }
          if (ops[pos] && ops[pos].op === 'end') pos++;
          nodes.push({ t: 'if', x: op.x, then: thenB, els: elseB });
          break;
        }
        case 'for': {
          const body = block();
          if (ops[pos] && ops[pos].op === 'end') pos++;
          nodes.push({ t: 'for', var: op.var, x: op.x, body: body });
          break;
        }
      }
    }
    return nodes;
  }
  return block();
}

// ── expression evaluator (neutral irExpr AST) ───────────────────────────────

function evalExpr(x, scope) {
  switch (x.k) {
    case 'num': return x.n.indexOf('.') >= 0 ? parseFloat(x.n) : parseInt(x.n, 10);
    case 'str': return x.s || '';
    case 'bool': return !!x.b;
    case 'path': return evalPath(x, scope);
    case 'call': {
      const fn = evalPath(x.recv, scope);
      const args = (x.args || []).map((a) => evalExpr(a, scope));
      return typeof fn === 'function' ? fn.apply(null, args) : undefined;
    }
    case 'unary':
      return x.op === '!' ? !truthy(evalExpr(x.x, scope)) : -num(evalExpr(x.x, scope));
    case 'bin': return evalBin(x.op, evalExpr(x.l, scope), evalExpr(x.r, scope));
  }
  return undefined;
}

function evalPath(x, scope) {
  const segs = x.segs || [];
  let cur = x.local ? scope.locals[segs[0]] : scope.data[segs[0]];
  for (let i = 1; i < segs.length; i++) {
    if (cur == null) return undefined;
    cur = cur[segs[i]];
  }
  return cur;
}

function evalBin(op, a, b) {
  switch (op) {
    case '==': return a === b;
    case '!=': return a !== b;
    case '<': return num(a) < num(b);
    case '<=': return num(a) <= num(b);
    case '>': return num(a) > num(b);
    case '>=': return num(a) >= num(b);
    case '&&': return truthy(a) && truthy(b);
    case '||': return truthy(a) ? a : b;
    case '+': return (typeof a === 'string' || typeof b === 'string') ? '' + a + b : num(a) + num(b);
    case '-': return num(a) - num(b);
    case '*': return num(a) * num(b);
    case '/': return num(a) / num(b);
    case '%': return num(a) % num(b);
  }
  return undefined;
}

function num(v) { return typeof v === 'number' ? v : (v === true ? 1 : v === false ? 0 : parseFloat(v) || 0); }
function truthy(v) {
  if (Array.isArray(v)) return v.length > 0;
  if (typeof v === 'number') return v !== 0;
  if (typeof v === 'string') return v.length > 0;
  return !!v;
}

function htmlEscape(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// ── renderer ────────────────────────────────────────────────────────────────

function renderNodes(nodes, scope, app) {
  let out = '';
  for (const n of nodes) {
    switch (n.t) {
      case 'text': out += n.v; break;
      case 'expr': out += htmlEscape(fmt(evalExpr(n.x, scope))); break;
      case 'if':
        out += truthy(evalExpr(n.x, scope)) ? renderNodes(n.then, scope, app) : renderNodes(n.els, scope, app);
        break;
      case 'for': {
        const list = evalExpr(n.x, scope) || [];
        for (const item of list) {
          const locals = Object.assign({}, scope.locals);
          locals[n.var] = item;
          out += renderNodes(n.body, { data: scope.data, locals: locals }, app);
        }
        break;
      }
      case 'child': {
        const childData = {};
        for (const p of n.props) childData[p.name] = p.x ? evalExpr(p.x, scope) : p.lit;
        out += app.renderFacet(n.name, childData).html;
        break;
      }
    }
  }
  return out;
}

function fmt(v) {
  if (v == null) return '';
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  return '' + v;
}

// Resolve a facet_id pattern ("LikeButton:post:{post.id}") against data.
function resolveFacetID(pattern, data) {
  return pattern.replace(/\{([^}]+)\}/g, function (_, p) {
    const segs = p.split('.');
    let cur = data[segs[0]];
    for (let i = 1; i < segs.length && cur != null; i++) cur = cur[segs[i]];
    return fmt(cur);
  });
}

// Inject data-facet-id onto the first element of a rendered body (mirrors the Go
// path's injectFacetID).
function injectFacetID(html, id) {
  return html.replace(/<([a-zA-Z][\w-]*)/, function (m) { return m + ' data-facet-id="' + id + '"'; });
}

// ── signing (mirrors fa/event.go sign(): op \0 facet_id \0 fragment) ─────────

function signEvent(keyHex, ev) {
  if (!keyHex) return ev;
  const mac = crypto.createHmac('sha256', Buffer.from(keyHex, 'hex'));
  mac.update(ev.op); mac.update(Buffer.from([0]));
  mac.update(ev.facet_id); mac.update(Buffer.from([0]));
  mac.update(ev.fragment);
  ev.hmac = mac.digest('hex');
  return ev;
}

// ── App ──────────────────────────────────────────────────────────────────────

class App {
  // opts: { genDir, faKey (hex; '' = dev/no signing), runtimeJs (path to
  // fa-runtime.js), title }
  constructor(opts) {
    opts = opts || {};
    this.genDir = opts.genDir || 'generated';
    this.keyHex = opts.faKey != null ? opts.faKey : (process.env.FA_SIGNING_KEY || '');
    this.title = opts.title || 'FA';
    this.runtimeJs = opts.runtimeJs || path.join(__dirname, '..', '..', 'runtime', 'fa-runtime.js');

    this.manifest = JSON.parse(fs.readFileSync(path.join(this.genDir, 'manifest.json'), 'utf8'));
    this.ir = JSON.parse(fs.readFileSync(path.join(this.genDir, 'render.json'), 'utf8'));
    this.trees = {};   // facet name → block tree
    this.facets = {};  // facet name → ir facet
    for (const f of this.ir.facets) { this.facets[f.name] = f; this.trees[f.name] = buildTree(f.render); }

    this.handlers = {}; // event type → fn(ctx) → data
    this._root = null;  // { name, dataFn }
    this.conns = new Map(); // conn id → { res }
  }

  // root sets the facet rendered at GET / and its initial data.
  root(name, dataFn) { this._root = { name: name, dataFn: dataFn }; return this; }

  // on registers a when: handler. fn(ctx) returns the fresh facet data; the
  // runtime re-renders per the facet's when-mutations and pushes signed events
  // to the acting connection.
  on(type, fn) { this.handlers[type] = fn; return this; }

  // renderFacet interprets a facet's IR with data → { facet_id, html } with
  // data-facet-id injected on the root element.
  renderFacet(name, data) {
    const tree = this.trees[name];
    if (!tree) throw new Error('unknown facet ' + name);
    const html = injectFacetID(renderNodes(tree, { data: data, locals: {} }, this), resolveFacetID(this.facets[name].facet_id, data));
    return { facet_id: resolveFacetID(this.facets[name].facet_id, data), html: html };
  }

  // ── HTTP ────────────────────────────────────────────────────────────────
  listen(addr) {
    const a = addr || process.env.FA_ADDR || 'localhost:7373';
    const [host, port] = a.indexOf(':') >= 0 ? a.split(':') : ['localhost', a];
    const server = http.createServer(this._handle.bind(this));
    server.listen(parseInt(port, 10), host, () => console.log('fa(node): listening on http://' + host + ':' + port));
    return server;
  }

  _handle(req, res) {
    const url = req.url.split('?')[0];
    if (req.method === 'GET' && url === '/') return this._serveShell(req, res);
    if (req.method === 'GET' && url === '/sse') return this._serveSSE(req, res);
    if (req.method === 'POST' && url === '/events') return this._handleEvents(req, res);
    if (req.method === 'GET' && url === '/manifest.json') return this._json(res, this.manifest);
    if (req.method === 'GET' && url === '/render.json') return this._json(res, this.ir);
    if (req.method === 'GET' && url === '/fa-runtime.js') {
      res.writeHead(200, { 'Content-Type': 'text/javascript' });
      return res.end(fs.readFileSync(this.runtimeJs));
    }
    res.writeHead(404); res.end('not found');
  }

  _serveShell(req, res) {
    // A native client (FA-Native: 1) gets the neutral ViewNode tree as a
    // ScreenResponse {title, tree}; a browser gets the HTML shell.
    if (req.headers['fa-native'] === '1') {
      let tree = { kind: 'box' };
      if (this._root) {
        const html = this.renderFacet(this._root.name, this._root.dataFn({})).html;
        tree = native.nodeToJSON(native.parseView(html));
      }
      return this._json(res, { title: this.title, tree: tree });
    }
    let body = '';
    if (this._root) body = this.renderFacet(this._root.name, this._root.dataFn({})).html;
    const html = '<!doctype html><html><head><meta charset="utf-8">' +
      '<meta name="fa-key" content="' + this.keyHex + '">' +
      '<title>' + htmlEscape(this.title) + '</title></head><body>' +
      body + '<script src="/fa-runtime.js"></script></body></html>';
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(html);
  }

  _serveSSE(req, res) {
    const v = (req.url.split('?')[1] || '').split('&').map((s) => s.split('=')).reduce((m, kv) => (m[kv[0]] = kv[1], m), {}).v || '1';
    if (v !== WIRE_VERSION) { res.writeHead(426); return res.end('fa: unsupported wire version ' + v); }
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', 'Connection': 'keep-alive', 'X-Accel-Buffering': 'no' });
    const id = crypto.randomBytes(16).toString('hex');
    this.conns.set(id, { res: res, native: req.headers['fa-native'] === '1' });
    const hello = JSON.stringify({ op: '_conn', conn: id, key: this.keyHex, v: WIRE_VERSION });
    res.write('data: ' + hello + '\n\n');
    const hb = setInterval(() => res.write(': keepalive\n\n'), 25000);
    req.on('close', () => { clearInterval(hb); this.conns.delete(id); });
  }

  _handleEvents(req, res) {
    let buf = '';
    req.on('data', (c) => { buf += c; });
    req.on('end', () => {
      let body;
      try { body = JSON.parse(buf || '{}'); } catch (e) { res.writeHead(400); return res.end('bad json'); }
      const fn = this.handlers[body.type];
      res.writeHead(204); res.end();
      if (!fn) return;
      const ctx = { type: body.type, payload: body.payload || {}, conn: body.conn };
      const data = fn(ctx);
      if (data == null) return;
      this._pushReRender(body.type, body.conn, data);
    });
  }

  // Map an incoming event type to the facet whose when: declares it, then apply
  // its mutations as signed events to the acting connection.
  _pushReRender(type, conn, data) {
    const c = this.conns.get(conn);
    if (!c) return;
    for (const f of this.ir.facets) {
      for (const w of f.when || []) {
        if (w.events.indexOf(type) < 0) continue;
        for (const mu of w.mutations) {
          const target = mu.target || f.name;
          const r = this.renderFacet(target, data);
          // A native connection receives the neutral ViewNode tree as the
          // fragment (and signs over it); a web connection receives HTML.
          const fragment = c.native ? native.treeJSON(r.html) : r.html;
          const ev = signEvent(this.keyHex, { op: mu.op === 'replace_all' ? 'replace' : mu.op, facet_id: r.facet_id, fragment: fragment });
          c.res.write('data: ' + JSON.stringify(ev) + '\n\n');
        }
      }
    }
  }

  _json(res, obj) { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); }
}

module.exports = { App, renderNodes, buildTree, evalExpr, signEvent, resolveFacetID, htmlEscape, WIRE_VERSION };

// fa-runtime.js — Facet Architecture client runtime (v0 walking skeleton).
//
// Pure plumbing. It:
//   1. holds one SSE connection; its first frame (op "_conn") carries this
//      connection's id, which the runtime echoes on every action so the server
//      can deliver the response to THIS connection only (no cross-client leak);
//   2. verifies each pushed event's HMAC against <meta name="fa-key"> and applies
//      it to the DOM node identified by data-facet-id;
//   3. forwards [data-action] clicks to a SINGLE /events endpoint;
//   4. exposes fa.subscribe(channel) for topic fan-out, re-subscribing on reconnect;
//   5. enforces the client side of the primitive taxonomy from the manifest:
//      stream `window:` trims, signal apply + `ttl:` expiry, vault client-side
//      decrypt (fa.vault.key), media player mounting.
//
// No per-action route table, no application logic, no client state beyond the
// connection id, pending/subscription bookkeeping, and the manifest registry.
(function () {
  'use strict';

  var cfg = { sse_path: '/sse', events_path: '/events', manifest_path: '/manifest.json' };
  var WIRE_VERSION = '1';    // SSE wire version this runtime speaks (see STABILITY.md)
  var connID = null;
  var pending = [];          // actions fired before connID is known
  var subscriptions = {};    // channels to (re)subscribe to
  var registry = {};         // facet name → manifest entry (kind, window, ttl, client body)
  var wireMismatch = null;   // set when the server speaks a different wire version (fatal)

  function facetName(id) { return String(id || '').split(':')[0]; }
  function metaFor(id) { return registry[facetName(id)] || null; }

  // parseMs parses the simple Go durations the compiler accepts (200ms, 5s, 2m, 1h).
  function parseMs(s) {
    var m = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(String(s || ''));
    if (!m) return 0;
    var n = parseFloat(m[1]);
    return n * ({ ms: 1, s: 1000, m: 60000, h: 3600000 })[m[2]];
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // fill renders a TRUSTED client body (the compiled manifest's decrypt:/source:
  // template) against UNTRUSTED values (decrypted plaintext / media metadata).
  // Interpolated values are HTML-ESCAPED; literal template text is not. Supports
  // {field} / {a.b} interpolation, {if expr}…{else}…{end} and
  // {for v in path}…{end}. Parsed templates are cached by body string.
  var tplCache = {};
  function fill(body, values) {
    body = String(body);
    var tpl = tplCache[body] || (tplCache[body] = parseTpl(tokenizeTpl(body)));
    return renderTpl(tpl, values || {});
  }

  // tokenizeTpl splits the body into text and {…} tags. A `{` always opens a hole
  // (the compiler reserves it), mirroring the FDL looks grammar.
  function tokenizeTpl(body) {
    var toks = [], re = /\{([^}]*)\}/g, last = 0, m;
    while ((m = re.exec(body))) {
      if (m.index > last) toks.push({ t: 'text', s: body.slice(last, m.index) });
      var inner = m[1].trim();
      if (/^if\s+/.test(inner)) toks.push({ t: 'if', e: inner.slice(3).trim() });
      else if (inner === 'else') toks.push({ t: 'else' });
      else if (inner === 'end') toks.push({ t: 'end' });
      else if (/^for\s+/.test(inner)) {
        var mm = /^for\s+([A-Za-z_]\w*)\s+in\s+(.+)$/.exec(inner);
        toks.push(mm ? { t: 'for', v: mm[1], it: mm[2].trim() } : { t: 'text', s: m[0] });
      } else if (/^cmp\s+/.test(inner)) {
        // {cmp Name|field=expr|…} — a client-instantiated child facet. Props are
        // pipe-separated; each value is an expression evaluated in the current scope
        // (so object props pass by reference), keyed by the child's field name.
        var parts = inner.replace(/^cmp\s+/, '').split('|'), props = [];
        for (var pi = 1; pi < parts.length; pi++) {
          var eq = parts[pi].indexOf('=');
          if (eq >= 0) props.push({ key: parts[pi].slice(0, eq).trim(), expr: parts[pi].slice(eq + 1) });
        }
        toks.push({ t: 'cmp', name: parts[0].trim(), props: props });
      } else toks.push({ t: 'interp', e: inner });
      last = re.lastIndex;
    }
    if (last < body.length) toks.push({ t: 'text', s: body.slice(last) });
    return toks;
  }

  // parseTpl folds the flat token stream into nested if/for nodes.
  function parseTpl(toks) {
    var i = 0;
    function block(stopElse) {
      var nodes = [];
      while (i < toks.length) {
        var tk = toks[i];
        if (tk.t === 'end' || (stopElse && tk.t === 'else')) return nodes;
        i++;
        if (tk.t === 'text' || tk.t === 'interp' || tk.t === 'cmp') nodes.push(tk);
        else if (tk.t === 'if') {
          var then_ = block(true), els = [];
          if (toks[i] && toks[i].t === 'else') { i++; els = block(false); }
          if (toks[i] && toks[i].t === 'end') i++;
          nodes.push({ t: 'if', e: tk.e, then: then_, els: els });
        } else if (tk.t === 'for') {
          var b = block(false);
          if (toks[i] && toks[i].t === 'end') i++;
          nodes.push({ t: 'for', v: tk.v, it: tk.it, body: b });
        }
      }
      return nodes;
    }
    return block(false);
  }

  function renderTpl(nodes, scope) {
    var out = '';
    for (var k = 0; k < nodes.length; k++) {
      var n = nodes[k];
      if (n.t === 'text') out += n.s;
      else if (n.t === 'interp') { var v = evalExpr(n.e, scope); if (v != null) out += esc(v); }
      else if (n.t === 'cmp') {
        // Instantiate a child facet: render its compiled client view against a scope
        // built from the props (evaluated here, so object props pass by reference).
        var reg = registry[n.name];
        if (reg && reg.view) {
          var cs = {};
          n.props.forEach(function (p) { cs[p.key] = evalExpr(p.expr, scope); });
          out += fill(reg.view, cs); // raw HTML (the child view), not escaped
        }
      }
      else if (n.t === 'if') out += renderTpl(truthy(evalExpr(n.e, scope)) ? n.then : n.els, scope);
      else if (n.t === 'for') {
        var arr = evalExpr(n.it, scope);
        if (arr && typeof arr !== 'string' && arr.length != null) {
          for (var j = 0; j < arr.length; j++) {
            var s2 = Object.create(scope); s2[n.v] = arr[j];
            out += renderTpl(n.body, s2);
          }
        }
      }
    }
    return out;
  }

  // evalExpr evaluates an FDL expression against a scope object, mirroring the
  // compiler's grammar (internal/codegen/expr.go): || && ; comparisons ; + - ;
  // * / % ; unary ! - ; literals, dotted paths, and parentheses. It is a small
  // tokenizer + precedence-climbing parser — NO eval/Function, so it is CSP-safe.
  // This is the one client evaluator shared by fill (vault/media bodies), client
  // bindings, and actions; semantics match Go's template builtins (truthy/compare).
  function evalExpr(e, scope) {
    return parseOr({ toks: lexExpr(String(e)), i: 0 }, scope || {});
  }

  function lexExpr(s) {
    var toks = [], i = 0;
    while (i < s.length) {
      var c = s.charAt(i);
      if (c === ' ' || c === '\t') { i++; continue; }
      if (c === '(') { toks.push({ k: 'lp' }); i++; continue; }
      if (c === ')') { toks.push({ k: 'rp' }); i++; continue; }
      if (c === '[') { toks.push({ k: 'lb' }); i++; continue; }
      if (c === ']') { toks.push({ k: 'rb' }); i++; continue; }
      if (c === ',') { toks.push({ k: 'comma' }); i++; continue; }
      if (c === '.') { toks.push({ k: 'dot' }); i++; continue; }
      if (c === '"' || c === "'") {
        var q = c, str = ''; i++;
        while (i < s.length && s.charAt(i) !== q) { if (s.charAt(i) === '\\') i++; str += s.charAt(i); i++; }
        i++; toks.push({ k: 'str', v: str }); continue;
      }
      if (c >= '0' && c <= '9') {
        var j = i; while (j < s.length && /[0-9.]/.test(s.charAt(j))) j++;
        toks.push({ k: 'num', v: parseFloat(s.slice(i, j)) }); i = j; continue;
      }
      if (/[A-Za-z_]/.test(c)) {
        var j2 = i; while (j2 < s.length && /[A-Za-z0-9_]/.test(s.charAt(j2))) j2++;
        toks.push({ k: 'ident', t: s.slice(i, j2) }); i = j2; continue;
      }
      var two = s.substr(i, 2);
      if (two === '==' || two === '!=' || two === '<=' || two === '>=' || two === '&&' || two === '||') {
        toks.push({ k: 'op', t: two }); i += 2; continue;
      }
      if ('<>!+-*/%'.indexOf(c) >= 0) { toks.push({ k: 'op', t: c }); i++; continue; }
      i++; // skip anything unexpected
    }
    return toks;
  }

  function tk(p) { return p.i < p.toks.length ? p.toks[p.i] : null; }
  function eatOp(p, op) { var t = tk(p); if (t && t.k === 'op' && t.t === op) { p.i++; return true; } return false; }

  function parseOr(p, sc) {
    var l = parseAnd(p, sc);
    while (eatOp(p, '||')) { var r = parseAnd(p, sc); l = truthy(l) ? l : r; }
    return l;
  }
  function parseAnd(p, sc) {
    var l = parseCmp(p, sc);
    while (eatOp(p, '&&')) { var r = parseCmp(p, sc); l = truthy(l) ? r : l; }
    return l;
  }
  function parseCmp(p, sc) {
    var l = parseAdd(p, sc), ops = ['==', '!=', '<=', '>=', '<', '>'];
    for (var k = 0; k < ops.length; k++) {
      if (eatOp(p, ops[k])) return compare(ops[k], l, parseAdd(p, sc));
    }
    return l;
  }
  function parseAdd(p, sc) {
    var l = parseMul(p, sc);
    for (;;) {
      if (eatOp(p, '+')) l = add(l, parseMul(p, sc));
      else if (eatOp(p, '-')) l = (+l) - (+parseMul(p, sc));
      else return l;
    }
  }
  function parseMul(p, sc) {
    var l = parseUnary(p, sc);
    for (;;) {
      if (eatOp(p, '*')) l = (+l) * (+parseUnary(p, sc));
      else if (eatOp(p, '/')) l = (+l) / (+parseUnary(p, sc));
      else if (eatOp(p, '%')) l = (+l) % (+parseUnary(p, sc));
      else return l;
    }
  }
  function parseUnary(p, sc) {
    if (eatOp(p, '!')) return !truthy(parseUnary(p, sc));
    if (eatOp(p, '-')) return -(+parseUnary(p, sc));
    return parsePrimary(p, sc);
  }
  function parsePrimary(p, sc) {
    var t = tk(p);
    if (!t) return undefined;
    if (t.k === 'num') { p.i++; return t.v; }
    if (t.k === 'str') { p.i++; return t.v; }
    if (t.k === 'lp') { p.i++; var v = parseOr(p, sc); if (tk(p) && tk(p).k === 'rp') p.i++; return v; }
    if (t.k === 'lb') { // array literal [a, b, c]
      p.i++; var arr = [];
      while (tk(p) && tk(p).k !== 'rb') {
        arr.push(parseOr(p, sc));
        if (tk(p) && tk(p).k === 'comma') p.i++; else break;
      }
      if (tk(p) && tk(p).k === 'rb') p.i++;
      return arr;
    }
    if (t.k === 'ident') {
      if (t.t === 'true') { p.i++; return true; }
      if (t.t === 'false') { p.i++; return false; }
      return parsePath(p, sc);
    }
    p.i++; return undefined;
  }
  function parsePath(p, sc) {
    var segs = [tk(p).t]; p.i++;
    while (tk(p) && tk(p).k === 'dot') { p.i++; var t = tk(p); if (t && t.k === 'ident') { segs.push(t.t); p.i++; } }
    if (tk(p) && tk(p).k === 'lp') { // method/func call: not supported client-side
      var depth = 0;
      do { var x = tk(p); p.i++; if (x.k === 'lp') depth++; else if (x.k === 'rp') depth--; } while (tk(p) && depth > 0);
      return undefined;
    }
    var v = sc;
    for (var i = 0; i < segs.length; i++) { if (v == null) return undefined; v = v[segs[i]]; }
    return v;
  }
  // add prefers numeric addition (matching the compiler's `add`); arrays concat
  // (so `items = items + [x]` appends), and otherwise it falls back to string
  // concatenation.
  function add(a, b) {
    if (Array.isArray(a)) return a.concat(b);
    if (Array.isArray(b)) return [a].concat(b);
    var na = +a, nb = +b;
    if (typeof a !== 'boolean' && typeof b !== 'boolean' && !isNaN(na) && !isNaN(nb)) return na + nb;
    return String(a) + String(b);
  }
  function compare(op, a, b) {
    if (op === '==') return a == b;
    if (op === '!=') return a != b;
    var na = +a, nb = +b, num = !isNaN(na) && !isNaN(nb);
    var x = num ? na : a, y = num ? nb : b;
    return op === '<' ? x < y : op === '<=' ? x <= y : op === '>' ? x > y : x >= y;
  }
  // truthy mirrors Go template emptiness so client if/for matches server looks:
  // null/false/0/""/[]/{} are falsy.
  function truthy(v) {
    if (v == null || v === false) return false;
    if (v === true) return true;
    if (typeof v === 'number') return v !== 0;
    if (typeof v === 'string') return v.length > 0;
    if (Array.isArray(v)) return v.length > 0;
    if (typeof v === 'object') return Object.keys(v).length > 0;
    return true;
  }

  function meta(name) {
    var el = document.querySelector('meta[name="' + name + '"]');
    return el ? el.content : '';
  }

  // ── HMAC verification ──────────────────────────────────────────────────────

  var hmacKey = null;

  function hexToBytes(hex) {
    var out = new Uint8Array(hex.length / 2);
    for (var i = 0; i < hex.length; i += 2) out[i / 2] = parseInt(hex.substr(i, 2), 16);
    return out;
  }

  function initKey() {
    var hex = meta('fa-key');
    if (!hex || !(window.crypto && crypto.subtle)) return Promise.resolve();
    return crypto.subtle
      .importKey('raw', hexToBytes(hex), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify'])
      .then(function (k) { hmacKey = k; })
      .catch(function () {});
  }

  function verify(ev) {
    if (!hmacKey) return Promise.resolve(true);
    var msg = new TextEncoder().encode(
      (ev.op || '') + '\x00' + (ev.facet_id || '') + '\x00' + (ev.fragment || '')
    );
    return crypto.subtle.verify('HMAC', hmacKey, hexToBytes(ev.hmac || ''), msg)
      .catch(function () { return false; });
  }

  // ── DOM application ────────────────────────────────────────────────────────

  function findFacet(id) {
    var sel = '[data-facet-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]';
    return document.querySelector(sel);
  }

  function apply(ev) {
    var el = ev.facet_id ? findFacet(ev.facet_id) : null;
    switch (ev.op) {
      case 'replace': if (el) { el.outerHTML = ev.fragment; scanClient(); } break;
      case 'append':  if (el) { el.insertAdjacentHTML('beforeend', ev.fragment); trimWindow(el, ev.facet_id, 'append'); scanClient(); } break;
      case 'prepend': if (el) { el.insertAdjacentHTML('afterbegin', ev.fragment); trimWindow(el, ev.facet_id, 'prepend'); scanClient(); } break;
      case 'remove':  if (el) el.remove(); break;
      case 'signal':  applySignal(ev); break;
      default: break;
    }
  }

  // ── stream: window enforcement ─────────────────────────────────────────────
  // A stream's `window: N` caps retained items: after an append/prepend the
  // container is trimmed from the opposite end, so the DOM never grows unbounded
  // under a high-frequency stream.

  function trimWindow(el, id, op) {
    var m = metaFor(id);
    if (!m || m.kind !== 'stream') return;
    var w = parseInt(m.window, 10);
    if (!w || w <= 0) return;
    while (el.children.length > w) {
      el.removeChild(op === 'prepend' ? el.lastElementChild : el.firstElementChild);
    }
  }

  // ── signal: ephemeral peer state ───────────────────────────────────────────
  // A signal is never rendered by the server. The relayed payload lands on
  // elements that opt in with data-fa-signal="<facet name or full facet-id>": the
  // runtime sets each payload key as a data-* attribute, adds .fa-signal-live,
  // and reverts both after the signal's declared ttl: — so a typing indicator is
  // pure CSS over [data-fa-signal].fa-signal-live, zero app JS.

  function applySignal(ev) {
    var payload;
    try { payload = JSON.parse(ev.fragment || '{}'); } catch (_) { return; }
    var name = facetName(ev.facet_id);
    var els = document.querySelectorAll('[data-fa-signal]');
    for (var i = 0; i < els.length; i++) {
      var want = els[i].getAttribute('data-fa-signal');
      if (want === ev.facet_id || want === name) fireSignal(els[i], payload, metaFor(ev.facet_id));
    }
  }

  function fireSignal(el, payload, meta) {
    var keys = [];
    for (var k in payload) {
      // safe, non-reserved attribute names only (no data-action / data-fa-* hijack)
      if (!/^[a-z][a-z0-9_]*$/i.test(k) || k === 'action' || /^fa/i.test(k)) continue;
      var attr = 'data-' + k.toLowerCase();
      el.setAttribute(attr, payload[k]);
      keys.push(attr);
    }
    el.classList.add('fa-signal-live');
    if (el._faSignalTimer) clearTimeout(el._faSignalTimer);
    var ttl = meta ? parseMs(meta.ttl) : 0;
    if (ttl > 0) {
      el._faSignalTimer = setTimeout(function () {
        el.classList.remove('fa-signal-live');
        keys.forEach(function (a) { el.removeAttribute(a); });
      }, ttl);
    }
  }

  // ── vault: client-side decrypt ─────────────────────────────────────────────
  // The server only ever sees the encrypted envelope; the manifest carries the
  // vault's decrypt: body and emits NO server template. The app provides the key
  // (fa.vault.key(name, hexKey) — e.g. derived from the user's credentials and
  // never sent to the server); the runtime AES-GCM-decrypts every
  // [data-fa-vault] element's data-fa-envelope (base64 of 12-byte IV ‖
  // ciphertext) and renders the decrypt: body with the plaintext, escaped.

  var vaultKeys = {};   // facet name → Promise<CryptoKey>

  function provideVaultKey(name, hexKey) {
    if (!(window.crypto && crypto.subtle)) return Promise.reject(new Error('WebCrypto unavailable'));
    var p = crypto.subtle.importKey('raw', hexToBytes(hexKey), { name: 'AES-GCM' }, false, ['decrypt']);
    vaultKeys[name] = p;
    return p.then(scanClient);
  }

  function b64ToBytes(b64) {
    var bin = atob(b64), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function decryptVault(el) {
    var name = el.getAttribute('data-fa-vault');
    var meta = registry[name];
    var env = el.getAttribute('data-fa-envelope');
    var keyP = vaultKeys[name];
    if (!meta || meta.kind !== 'vault' || !env || !keyP) return;
    if (el._faEnvelope === env) return; // this envelope is already decrypted
    keyP.then(function (key) {
      var bytes = b64ToBytes(env);
      return crypto.subtle.decrypt({ name: 'AES-GCM', iv: bytes.slice(0, 12) }, key, bytes.slice(12));
    }).then(function (buf) {
      var pt = new TextDecoder().decode(buf);
      var values = { plaintext: pt };
      try { // a JSON plaintext exposes its fields to the decrypt: body too
        var obj = JSON.parse(pt);
        if (obj && typeof obj === 'object') for (var k in obj) values[k] = obj[k];
      } catch (_) {}
      el.innerHTML = fill(meta.client || '', values);
      el._faEnvelope = env;
    }).catch(function () {
      console.warn('[fa] vault decrypt failed for', name); // envelope stays — never render garbage
    });
  }

  // ── media: the runtime owns the player ─────────────────────────────────────
  // A media primitive's source: body describes the source; the runtime mounts
  // the actual player inside each [data-fa-media] element, filling {field} holes
  // from the element's data-* attributes. <hls>/<dash> normalize to <video>
  // (native HLS where the browser supports it), and players get controls.

  function mountMedia(el) {
    if (el._faMediaMounted) return;
    var name = el.getAttribute('data-fa-media');
    var meta = registry[name];
    if (!meta || meta.kind !== 'media' || !meta.client) return;
    var values = {};
    for (var k in el.dataset) {
      if (!/^fa[A-Z]/.test(k)) values[k] = el.dataset[k];
    }
    var html = fill(meta.client, values)
      .replace(/<(\/?)(hls|dash)\b/gi, '<$1video')
      .replace(/<(video|audio)\b(?![^>]*\bcontrols\b)/gi, '<$1 controls')
      .replace(/<(video|audio)([^>]*?)\/>/gi, '<$1$2></$1>'); // self-closing → pair
    el.innerHTML = html;
    el._faMediaMounted = true;
  }

  // scanClient applies the client-rendered primitives to the current DOM:
  // decrypts ready vaults and mounts media players. Runs at boot, after every
  // applied DOM event, after navigation, and when a vault key arrives.
  function scanClient() {
    var i, els;
    els = document.querySelectorAll('[data-fa-vault]');
    for (i = 0; i < els.length; i++) decryptVault(els[i]);
    els = document.querySelectorAll('[data-fa-media]');
    for (i = 0; i < els.length; i++) mountMedia(els[i]);
  }

  function handleEvent(ev) {
    if (ev.op === '_conn') { // control frame, not a DOM op
      // Fail loud on a wire-version mismatch (new runtime against an old server
      // — the other direction is rejected server-side with 426 at connect).
      if (ev.v && ev.v !== WIRE_VERSION) {
        wireMismatch = 'server speaks SSE wire v' + ev.v + ', this runtime speaks v' + WIRE_VERSION;
        console.error('[fa] FATAL: ' + wireMismatch + ' — not reconnecting; upgrade the page or the server');
        return;
      }
      onConn(ev.conn);
      return;
    }
    verify(ev).then(function (ok) {
      if (!ok) { console.warn('[fa] dropped event with bad signature', ev.op, ev.facet_id); return; }
      apply(ev);
    });
  }

  // ── connection identity ────────────────────────────────────────────────────

  function onConn(id) {
    connID = id;
    // (re)subscribe to any channels, then flush actions queued before connect.
    Object.keys(subscriptions).forEach(function (ch) { send('fa.subscribe', { channel: ch }); });
    var q = pending; pending = [];
    q.forEach(function (a) { send(a.type, a.payload); });
  }

  // ── transport ──────────────────────────────────────────────────────────────

  var delay = 1000;
  function connectSSE() {
    // Declare the wire version we speak as ?v= (EventSource cannot set headers);
    // a server that doesn't speak it rejects the connect with 426 — fail loud.
    var url = cfg.sse_path + (cfg.sse_path.indexOf('?') < 0 ? '?' : '&') + 'v=' + WIRE_VERSION;
    var es = new EventSource(url);
    es.onmessage = function (e) {
      try { handleEvent(JSON.parse(e.data)); } catch (_) {}
      if (wireMismatch) es.close(); // fatal — stop the stream, no reconnect
    };
    es.onopen = function () { delay = 1000; };
    es.onerror = function () {
      es.close();
      if (wireMismatch) return;      // fatal — do not reconnect-loop
      connID = null;                 // a reconnect gets a fresh connection id
      setTimeout(connectSSE, delay);
      delay = Math.min(delay * 2, 30000);
    };
  }

  // send posts one event, including this connection's id so the server can
  // address the reply. If the id isn't known yet, the action is queued.
  function send(type, payload) {
    if (!connID) { pending.push({ type: type, payload: payload }); return; }
    fetch(cfg.events_path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: type, payload: payload, conn: connID }),
      credentials: 'same-origin'
    }).catch(function () {});
  }

  function wireActions() {
    document.addEventListener('click', function (e) {
      var el = e.target.closest('[data-action]');
      if (!el) return;
      e.preventDefault();
      applyOptimistic(el);
      var payload = {};
      // every data-* attribute except the action and reserved data-fa-* ones
      for (var k in el.dataset) {
        if (k !== 'action' && !/^fa[A-Z]/.test(k)) payload[k] = el.dataset[k];
      }
      send(el.dataset.action, payload);
    });
  }

  // applyOptimistic gives instant feedback before the server replies. The author
  // declares data-fa-optimistic="classA classB" on the action element; the
  // runtime toggles those classes on the enclosing facet immediately. The
  // server's authoritative replace then reconciles (it overwrites the node). If
  // no reply lands within the TTL, the optimistic guess is reverted.
  function applyOptimistic(el) {
    var spec = el.getAttribute('data-fa-optimistic');
    if (!spec) return;
    var node = el.closest('[data-facet-id]') || el;
    var classes = spec.split(/\s+/).filter(Boolean);
    classes.forEach(function (c) { node.classList.toggle(c); });
    node.setAttribute('aria-busy', 'true');
    setTimeout(function () {
      if (node.isConnected) { // not replaced by the server → revert the guess
        classes.forEach(function (c) { node.classList.toggle(c); });
        node.removeAttribute('aria-busy');
      }
    }, 5000);
  }

  function assign(dst, src) { for (var k in src) dst[k] = src[k]; return dst; }

  // ── client reactivity (Brick 4, docs/REACTIVITY.md) ─────────────────────────
  // Compiled fine-grained reactivity. The manifest carries, per facet: `state`
  // (signals + initial values), `bindings` (the text nodes a signal feeds, marked
  // data-fa-bind="bN"), and `actions` (named signal mutations a DOM event runs,
  // wired via data-fa-on-<event>). On an event we run the action's assignments on
  // THIS instance's signal store, then write each bound node directly — no virtual
  // DOM, no diff, zero round-trip. The signal store is server-authoritative state's
  // local, ephemeral complement; persistent changes still flow through /events.

  var reactiveEvents = {}; // DOM event type → true, gathered from manifest handlers

  // applyAction mutates a signal store in place by an action's assignments,
  // left-to-right (a later assignment sees earlier ones). Pure given the store —
  // the unit-testable core, independent of the DOM.
  function applyAction(signals, action) {
    if (action && action.mutations) {
      action.mutations.forEach(function (m) { signals[m.target] = evalExpr(m.expr, signals); });
    }
    return signals;
  }

  // bindingText computes the string each binding renders for the given scope
  // (signals plus any client-derived values).
  function bindingText(scope, bindings) {
    var out = {};
    (bindings || []).forEach(function (b) {
      var v = evalExpr(b.expr, scope);
      out[b.id] = v == null ? '' : String(v);
    });
    return out;
  }

  // computeScope layers client-derived values (Brick 5) over the signal store,
  // evaluating each in manifest order so a later derived value sees earlier ones.
  // Derived values are read-only — only signals are ever assigned (by actions) —
  // so they are recomputed fresh on every flush rather than stored.
  function computeScope(signals, derived) {
    var scope = {};
    for (var k in signals) scope[k] = signals[k];
    (derived || []).forEach(function (d) { scope[d.name] = evalExpr(d.expr, scope); });
    return scope;
  }

  function currentPath() { return typeof location !== 'undefined' ? location.pathname : '/'; }

  // signalsFor returns a facet instance's live signal store, creating it from the
  // manifest's initial values on first touch (matching the server's first paint).
  // Beyond declared `state:` signals it seeds the built-in `route` (Brick 10, the
  // current client path) and each async `query:` as a reactive {loading,error,data}
  // value (Brick 11) — both are reactive roots the runtime keeps up to date.
  function signalsFor(root) {
    if (root._faSignals) return root._faSignals;
    var m = metaFor(root.getAttribute('data-facet-id')), s = {};
    if (m && m.state) m.state.forEach(function (st) { s[st.name] = evalExpr(st.init, {}); });
    if (m && m.queries) m.queries.forEach(function (q) { s[q.name] = { loading: true, error: false, data: null }; });
    s.route = currentPath();
    root._faSignals = s;
    return s;
  }

  // bindNodes maps this instance's binding ids to their DOM nodes, excluding any
  // that belong to a NESTED facet (which owns its own bindings of the same id).
  function bindNodes(root) {
    var all = root.querySelectorAll('[data-fa-bind]'), out = {};
    for (var i = 0; i < all.length; i++) {
      if (all[i].closest('[data-facet-id]') === root) out[all[i].getAttribute('data-fa-bind')] = all[i];
    }
    return out;
  }

  // ── fine-grained invalidation (enterprise core) ──────────────────────────────
  // The dependency graph compiled into the manifest drives SURGICAL updates: when
  // a signal changes we recompute only the derived values that transitively depend
  // on it and patch only the bindings / attributes / lists / inputs whose roots are
  // dirty — never "recompute everything" (the old O(everything)-per-event flush).
  // `update(root, null)` is a full paint (hydrate / route / query resolve);
  // `invalidate(root, changed)` is the per-event fine-grained path. Writes inside a
  // single event are already batched: we diff the signal store once, after the
  // action and its effects have run, then dispatch one update.

  // rootsOf returns the reactive root identifiers an expression reads (idents not
  // following a dot, minus booleans) — the client mirror of the compiler's
  // exprRoots, so the graph the compiler recorded and the one we dispatch on match.
  function rootsOf(expr) {
    var toks = lexExpr(String(expr)), out = [], seen = {};
    for (var i = 0; i < toks.length; i++) {
      var t = toks[i];
      if (t.k !== 'ident' || (i > 0 && toks[i - 1].k === 'dot')) continue;
      if (t.t === 'true' || t.t === 'false' || seen[t.t]) continue;
      seen[t.t] = 1; out.push(t.t);
    }
    return out;
  }

  // tplRoots collects the reactive roots a client template's holes (interp/if/for)
  // read, excluding a bound loop variable — i.e. the outer deps of a list/region.
  function tplRoots(tpl, exclude) {
    var out = [], seen = {};
    tokenizeTpl(String(tpl)).forEach(function (tk) {
      var e = (tk.t === 'interp' || tk.t === 'if') ? tk.e : (tk.t === 'for' ? tk.it : null);
      if (e == null) return;
      rootsOf(e).forEach(function (r) { if (r !== exclude && !seen[r]) { seen[r] = 1; out.push(r); } });
    });
    return out;
  }

  // facetGraph precomputes (once per manifest entry) the dependency metadata the
  // invalidator needs: each derived value's deps and each list/region's deps.
  function facetGraph(m) {
    if (m._graph) return m._graph;
    var g = { derived: [], lists: [], regions: [] };
    (m.derived || []).forEach(function (d) { g.derived.push({ name: d.name, expr: d.expr, deps: rootsOf(d.expr) }); });
    (m.lists || []).forEach(function (L) { g.lists.push({ entry: L, deps: tplRoots(L.item, L.var).concat([L.signal]) }); });
    (m.regions || []).forEach(function (R) {
      var deps = rootsOf(R.cond || '').concat(tplRoots(R.body || '', null));
      if (R.els) deps = deps.concat(tplRoots(R.els, null));
      g.regions.push({ entry: R, deps: deps });
    });
    m._graph = g;
    return g;
  }

  function anyDirty(deps, dirty) { for (var i = 0; i < (deps || []).length; i++) if (dirty[deps[i]]) return true; return false; }

  // dirtySet expands the changed signal names through the derived graph (manifest =
  // evaluation order) so a binding on a derived value is invalidated when any
  // upstream signal changes.
  function dirtySet(g, changed) {
    var dirty = {};
    changed.forEach(function (n) { dirty[n] = true; });
    g.derived.forEach(function (d) { if (anyDirty(d.deps, dirty)) dirty[d.name] = true; });
    return dirty;
  }

  // buildScope layers derived values over the signal store, recomputing only the
  // dirty ones (dirty=null → full recompute) and caching the rest on the instance
  // so an unrelated signal change never recomputes the whole derived chain.
  function buildScope(root, g, dirty) {
    var signals = signalsFor(root), scope = {}, cache = root._faDerived || (root._faDerived = {});
    for (var k in signals) scope[k] = signals[k];
    g.derived.forEach(function (d) {
      if (dirty === null || dirty[d.name] || !(d.name in cache)) cache[d.name] = evalExpr(d.expr, scope);
      scope[d.name] = cache[d.name];
    });
    return scope;
  }

  function update(root, dirty) {
    var m = metaFor(root.getAttribute('data-facet-id'));
    if (!m) return;
    var g = facetGraph(m), scope = buildScope(root, g, dirty);
    var hit = dirty ? function (deps) { return anyDirty(deps, dirty); } : function () { return true; };
    if (m.bindings) applyBindings(root, m.bindings, scope, hit);
    if (m.lists) g.lists.forEach(function (L) { if (hit(L.deps)) reconcileList(root, L.entry, scope); });
    if (g.regions.length) g.regions.forEach(function (R) { if (hit(R.deps)) reconcileRegion(root, R.entry, scope); });
    if (m.inputs) syncInputs(root, m, scope, hit);
  }

  function flush(root) { update(root, null); }

  function invalidate(root, changed) {
    if (!changed.length) return;
    var m = metaFor(root.getAttribute('data-facet-id'));
    if (m) update(root, dirtySet(facetGraph(m), changed));
  }

  function snapshot(o) { var s = {}; for (var k in o) s[k] = o[k]; return s; }
  function changedKeys(after, before) { var out = []; for (var k in after) if (after[k] !== before[k]) out.push(k); return out; }

  // applyBindings patches text + attribute bindings whose roots are dirty (hit).
  function applyBindings(root, bindings, scope, hit) {
    var byId = {};
    bindings.forEach(function (b) { byId[b.id] = b; });
    var nodes = bindNodes(root);
    for (var id in nodes) {
      var b = byId[id];
      if (b && b.node === 'text' && hit(b.signals)) {
        var v = evalExpr(b.expr, scope);
        nodes[id].textContent = v == null ? '' : String(v);
      }
    }
    var all = root.querySelectorAll('[data-fa-bind-attr]');
    for (var i = 0; i < all.length; i++) {
      if (all[i].closest('[data-facet-id]') !== root) continue; // nested facet owns its own
      var ids = all[i].getAttribute('data-fa-bind-attr').split(/\s+/);
      for (var j = 0; j < ids.length; j++) {
        var bb = byId[ids[j]];
        if (bb && (bb.node === 'attr' || bb.node === 'boolattr') && hit(bb.signals)) applyAttr(all[i], bb, evalExpr(bb.expr, scope));
      }
    }
  }

  // ── attribute / class / show bindings (Brick 8) ──────────────────────────────
  // A reactive signal inside an attribute value compiles to a controlled attribute
  // plus data-fa-bind-attr="<binding ids>" on the element. On flush the runtime
  // re-evaluates each and writes it: a "boolattr" (disabled/hidden/checked/…) is
  // toggled by presence (truthy → present), so `hidden="{!visible}"` shows/hides
  // and `disabled="{busy}"` never renders the foot-gun disabled="false"; any other
  // attr ("attr") is set to the string value (class, href, aria-*, style, …).

  function applyAttr(el, b, val) {
    if (b.node === 'boolattr') {
      if (truthy(val)) el.setAttribute(b.attr, ''); else el.removeAttribute(b.attr);
    } else {
      el.setAttribute(b.attr, val == null ? '' : String(val));
    }
  }

  // ── two-way form bindings (Brick 9) ──────────────────────────────────────────
  // bind:value / bind:checked compile to data-fa-bind-value / data-fa-bind-checked
  // markers. syncInputs writes the control FROM the signal on flush (skipping a
  // focused text field so it never clobbers the caret); onInput writes the signal
  // FROM the control on every keystroke/toggle, runs effects, and re-flushes.

  function syncInputs(root, m, scope, hit) {
    var els = root.querySelectorAll('[data-fa-bind-value],[data-fa-bind-checked]');
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      if (el.closest('[data-facet-id]') !== root) continue;
      var cname = el.getAttribute('data-fa-bind-checked');
      if (cname != null) {
        if (!hit || hit([cname])) el.checked = truthy(scope[cname]);
        continue;
      }
      var name = el.getAttribute('data-fa-bind-value');
      if (hit && !hit([name])) continue;
      var v = scope[name];
      v = v == null ? '' : String(v);
      if (document.activeElement !== el && el.value !== v) el.value = v;
    }
  }

  function onInput(e) {
    var el = e.target.closest && e.target.closest('[data-fa-bind-value],[data-fa-bind-checked]');
    if (!el) return;
    var root = el.closest('[data-facet-id]');
    if (!root) return;
    var m = metaFor(root.getAttribute('data-facet-id'));
    var signals = signalsFor(root), before = snapshot(signals);
    if (el.hasAttribute('data-fa-bind-checked')) {
      signals[el.getAttribute('data-fa-bind-checked')] = !!el.checked;
    } else {
      var name = el.getAttribute('data-fa-bind-value'), cur = signals[name], raw = el.value;
      // Preserve a numeric signal's type so arithmetic on it keeps working.
      signals[name] = (typeof cur === 'number' && raw.trim() !== '' && !isNaN(+raw)) ? +raw : raw;
    }
    runEffects(m, signals, before);
    invalidate(root, changedKeys(signals, before));
  }

  function wireInputs() {
    document.addEventListener('input', onInput);
    document.addEventListener('change', onInput);
  }

  // ── reactive lists, keyed reconciliation (Brick 6) ──────────────────────────
  // A `for v in <signal>` over a list signal compiles to an empty <fa-for> host;
  // the runtime renders each item with the client template engine (fill) and
  // reconciles the host's children by key — reusing unchanged nodes, moving them
  // into order, creating new ones, and removing the rest. Keys are item.id when
  // present, else the index, so reorders and inserts don't rebuild the world.

  // listItems is the DOM-free core: it computes the desired [{key, html}] for a
  // list signal in a scope. Unit-tested without a browser.
  function listItems(list, scope) {
    var arr = evalExpr(list.signal, scope); // a signal name OR a path (query.data, derived)
    if (!arr || arr.length == null) return [];
    var out = [];
    for (var i = 0; i < arr.length; i++) {
      var item = arr[i], s2 = {};
      for (var k in scope) s2[k] = scope[k];
      s2[list.var] = item;
      var key = (item != null && item.id != null) ? String(item.id) : String(i);
      out.push({ key: key, html: fill(list.item, s2) });
    }
    return out;
  }

  function listHost(root, id) {
    var all = root.querySelectorAll('[data-fa-list="' + id + '"]');
    for (var i = 0; i < all.length; i++) if (all[i].closest('[data-facet-id]') === root) return all[i];
    return null;
  }

  function htmlToNode(html) {
    var t = document.createElement('template');
    t.innerHTML = String(html).trim();
    return t.content.firstElementChild;
  }

  // longestIncreasing returns the indices i (ascending) of a longest strictly
  // increasing subsequence of arr's values, skipping entries with arr[i] < 0 (new
  // items). It is the heart of minimal-move reconciliation: the nodes at those
  // indices are already in relative order and must NOT be moved — only the rest
  // are repositioned, so a reorder/insert touches O(moved) nodes, not O(n).
  // Patience sorting with binary search + predecessor links (Vue 3's getSequence).
  function longestIncreasing(arr) {
    var n = arr.length, piles = [], prev = new Array(n), i;
    for (i = 0; i < n; i++) prev[i] = -1;
    for (i = 0; i < n; i++) {
      if (arr[i] < 0) continue;
      var lo = 0, hi = piles.length;
      while (lo < hi) { var mid = (lo + hi) >> 1; if (arr[piles[mid]] < arr[i]) lo = mid + 1; else hi = mid; }
      if (lo > 0) prev[i] = piles[lo - 1];
      if (lo === piles.length) piles.push(i); else piles[lo] = i;
    }
    var res = [], k = piles.length ? piles[piles.length - 1] : -1;
    while (k >= 0) { res.push(k); k = prev[k]; }
    res.reverse();
    return res;
  }

  // reconcileChildren keys host's children to `desired` ([{key, html}]) with a
  // minimal set of DOM moves: reuse a node by key (re-rendering its HTML only when
  // changed), create the new ones, remove the gone ones, then move only the nodes
  // that fall outside the longest stable (already-ordered) run. `place(node, i)`
  // lets the virtual path absolutely-position rows instead of stacking them.
  function reconcileChildren(host, desired, place) {
    var oldKids = Array.prototype.slice.call(host.children);
    var oldPos = {}, i;
    for (i = 0; i < oldKids.length; i++) { var ok = oldKids[i].getAttribute('data-fa-key'); if (ok != null) oldPos[ok] = i; }

    var n = desired.length, nodes = new Array(n), source = new Array(n), want = {};
    for (i = 0; i < n; i++) {
      var d = desired[i]; want[d.key] = 1;
      var oi = (d.key in oldPos) ? oldPos[d.key] : -1;
      source[i] = oi;
      var node;
      if (oi >= 0) {
        node = oldKids[oi];
        if (node._faHtml !== d.html) { // content changed → re-render, keep the slot
          var fresh = htmlToNode(d.html);
          if (fresh) { fresh.setAttribute('data-fa-key', d.key); fresh._faHtml = d.html; host.replaceChild(fresh, node); oldKids[oi] = fresh; node = fresh; }
        }
      } else {
        node = htmlToNode(d.html);
        if (node) { node.setAttribute('data-fa-key', d.key); node._faHtml = d.html; }
      }
      nodes[i] = node || null;
      if (nodes[i] && place) place(nodes[i], i);
    }

    for (i = 0; i < oldKids.length; i++) { var gk = oldKids[i].getAttribute('data-fa-key'); if (gk != null && !want[gk] && oldKids[i].parentNode === host) host.removeChild(oldKids[i]); }

    var stable = {}; longestIncreasing(source).forEach(function (idx) { stable[idx] = 1; });
    for (i = n - 1; i >= 0; i--) {
      var nd = nodes[i]; if (!nd) continue;
      var ref = (i + 1 < n) ? nodes[i + 1] : null;
      if (source[i] === -1 || !stable[i]) host.insertBefore(nd, ref); // new or out of order → (re)insert
    }
  }

  // visibleRange is the pure windowing math for virtualized lists: the half-open
  // [start, end) range of rows that intersect the viewport, padded by `overscan`
  // rows each side and clamped to [0, count]. Unit-tested without a browser.
  function visibleRange(scrollTop, viewportH, itemH, count, overscan) {
    if (!itemH || itemH <= 0 || count <= 0) return { start: 0, end: count };
    var start = Math.floor(scrollTop / itemH) - overscan;
    var end = Math.ceil((scrollTop + viewportH) / itemH) + overscan;
    return { start: Math.max(0, start), end: Math.min(count, Math.max(0, end)) };
  }

  function reconcileList(root, list, scope) {
    var host = listHost(root, list.id);
    if (!host) return;
    if (list.virtual) { reconcileVirtual(host, list, scope); return; }
    reconcileChildren(host, listItems(list, scope), null);
    mountFacets(host); // hydrate any client-instantiated child facets in the items
  }

  // reconcileVirtual renders only the rows intersecting the scroll viewport. The
  // host becomes a scroll container with one relative sizer whose height is the
  // full list (so the scrollbar is honest); visible rows are reused/reconciled by
  // key and absolutely positioned at their index. A one-time scroll listener
  // re-runs this on scroll. The app sizes the host's height in CSS; rows are a
  // fixed `virtual <height>` px. This is O(viewport), so a 100k-row list stays
  // flat in the DOM.
  function reconcileVirtual(host, list, scope) {
    var arr = evalExpr(list.signal, scope);
    if (!arr || arr.length == null) arr = [];
    var itemH = list.height || 40, count = arr.length, overscan = 4;

    if (host.style.position !== 'relative') { host.style.position = 'relative'; host.style.display = 'block'; host.style.overflow = 'auto'; }
    var sizer = host._faSizer;
    if (!sizer || sizer.parentNode !== host) {
      sizer = document.createElement('div'); sizer.className = 'fa-virt-sizer'; sizer.style.position = 'relative'; sizer.style.width = '100%';
      host.appendChild(sizer); host._faSizer = sizer;
    }
    sizer.style.height = (count * itemH) + 'px';
    host._faList = list; host._faScope = scope;
    if (!host._faVirtBound) {
      host._faVirtBound = true;
      host.addEventListener('scroll', function () { if (host._faList) reconcileVirtual(host, host._faList, host._faScope); });
    }

    var r = visibleRange(host.scrollTop || 0, host.clientHeight || 0, itemH, count, overscan);
    var desired = [];
    for (var i = r.start; i < r.end; i++) {
      var item = arr[i], s2 = {}; for (var k in scope) s2[k] = scope[k]; s2[list.var] = item;
      var key = (item != null && item.id != null) ? String(item.id) : String(i);
      desired.push({ key: key, html: fill(list.item, s2), top: i * itemH });
    }
    reconcileChildren(sizer, desired, function (node, idx) {
      node.style.position = 'absolute'; node.style.left = '0'; node.style.right = '0'; node.style.top = desired[idx].top + 'px';
    });
    mountFacets(sizer); // hydrate any client-instantiated child facets in the visible rows
  }

  // ── structural control flow: reactive if-regions ────────────────────────────
  // A reactive `{if cond}` over signals compiles to an empty <fa-if> host. The
  // runtime evaluates the condition and renders the then- or else-branch INTO the
  // host, replacing its contents — a true mount/unmount (the inactive branch is not
  // in the DOM at all, unlike a `hidden` show-binding). It re-renders only when the
  // rendered HTML actually changes, and instantiates any child facets in the new
  // subtree (mountFacets) so components work inside conditionals.

  // mountFacets brings every facet instance inside a freshly-rendered container to
  // life — initial paint from its signal store and async queries kicked off — by
  // reusing the same per-instance machinery hydrate uses at boot. Handlers are
  // globally delegated, so a child instance is interactive the moment it is in the
  // DOM; this just paints and queries it.
  function mountFacets(container) {
    var els = container.querySelectorAll('[data-facet-id]');
    for (var i = 0; i < els.length; i++) {
      var m = metaFor(els[i].getAttribute('data-facet-id'));
      if (!m) continue;
      if (m.bindings || m.lists || m.inputs || m.queries || m.regions) flush(els[i]);
      if (m.queries) runQueries(els[i]);
    }
  }

  function regionHost(root, id) {
    var all = root.querySelectorAll('[data-fa-region="' + id + '"]');
    for (var i = 0; i < all.length; i++) if (all[i].closest('[data-facet-id]') === root) return all[i];
    return null;
  }

  function reconcileRegion(root, region, scope) {
    var host = regionHost(root, region.id);
    if (!host) return;
    var on = truthy(evalExpr(region.cond, scope));
    var html = on ? fill(region.body, scope) : (region.els ? fill(region.els, scope) : '');
    if (host._faHtml === html) return; // branch + content unchanged → nothing to do
    host.innerHTML = html;
    host._faHtml = html;
    mountFacets(host); // client-instantiate child facets in the new subtree (Brick: components in regions)
    scanClient();      // hydrate any vault/media inside
  }

  function actionByName(m, name) {
    if (m && m.actions) for (var i = 0; i < m.actions.length; i++) if (m.actions[i].name === name) return m.actions[i];
    return null;
  }

  // runEffects fires every effect whose dependency signals changed between `before`
  // and the current store (Brick 7). Effects run ONCE per cycle, in order, and do
  // not re-trigger one another — so an effect that mutates a signal another effect
  // watches will not loop. The (possibly further) mutations are flushed to the DOM
  // by the caller.
  function runEffects(m, signals, before) {
    if (!m || !m.effects) return;
    m.effects.forEach(function (eff) {
      var changed = eff.deps.some(function (d) { return signals[d] !== before[d]; });
      if (changed) applyAction(signals, actionByName(m, eff.action));
    });
  }

  function runAction(root, name) {
    var m = metaFor(root.getAttribute('data-facet-id'));
    var act = actionByName(m, name);
    if (!act) return;
    var signals = signalsFor(root), before = snapshot(signals);
    applyAction(signals, act);
    runEffects(m, signals, before);
    invalidate(root, changedKeys(signals, before));
  }

  // ── async queries (Brick 11) ─────────────────────────────────────────────────
  // A `query: name from "url"` is seeded {loading:true} in the signal store, then
  // fetched once per instance on mount. On resolve the store gets
  // {loading:false, data:<json>} (or {error:true} on failure) and the instance is
  // flushed — so `{name.data.title}` and `hidden="{name.loading}"` (Brick 8) light
  // up with no per-render round-trip. Server-authoritative by transport: the URL is
  // a normal same-origin endpoint the app's backend serves.
  function runQueries(root) {
    if (root._faQueried) return;
    var m = metaFor(root.getAttribute('data-facet-id'));
    if (!m || !m.queries || !m.queries.length) return;
    root._faQueried = true;
    var signals = signalsFor(root);
    m.queries.forEach(function (q) {
      fetch(q.url, { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
        .then(function (r) { if (!r.ok) throw new Error('http ' + r.status); return r.json(); })
        .then(function (data) { signals[q.name] = { loading: false, error: false, data: data }; flush(root); })
        .catch(function () { signals[q.name] = { loading: false, error: true, data: null }; flush(root); });
    });
  }

  // ── route signal + active links (Brick 10) ───────────────────────────────────
  // `route` is a built-in reactive signal seeded from location.pathname. On every
  // navigation the runtime updates each live instance's route and re-flushes, so a
  // client router falls straight out of Brick 8: `<section hidden="{route != '/'}">`.
  // Links marked data-nav whose href matches the current path get .fa-active +
  // aria-current for styling, no app code.
  function markActiveLinks() {
    var links = document.querySelectorAll('a[data-nav]');
    for (var i = 0; i < links.length; i++) {
      var a = links[i], match = false;
      try { match = new URL(a.getAttribute('href') || '', location.href).pathname === currentPath(); } catch (_) {}
      a.classList.toggle('fa-active', match);
      if (match) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
  }

  function updateRoute() {
    var els = document.querySelectorAll('[data-facet-id]');
    for (var i = 0; i < els.length; i++) {
      var r = els[i]._faSignals;
      if (r && 'route' in r) { r.route = currentPath(); flush(els[i]); }
    }
    markActiveLinks();
  }

  // hydrateReactive paints every facet instance from its signals once on load
  // (and after navigation): it fills reactive lists the server emitted empty,
  // reconciles any binding whose initial value the server could not pre-render
  // (route/query-dependent), syncs form controls, and kicks off async queries.
  function hydrateReactive() {
    var els = document.querySelectorAll('[data-facet-id]');
    for (var i = 0; i < els.length; i++) {
      var m = metaFor(els[i].getAttribute('data-facet-id'));
      if (!m) continue;
      if (m.bindings || m.lists || m.inputs || m.queries) flush(els[i]);
      if (m.queries) runQueries(els[i]);
    }
  }

  // wireReactive adds one delegated listener per event type the app uses. On an
  // event it finds the nearest element wired for that event, resolves its facet
  // instance, and runs the named action locally.
  function wireReactive() {
    Object.keys(reactiveEvents).forEach(function (evt) {
      document.addEventListener(evt, function (e) {
        var attr = 'data-fa-on-' + evt;
        var el = e.target.closest('[' + attr + ']');
        if (!el) return;
        var root = el.closest('[data-facet-id]');
        if (!root) return;
        e.preventDefault();
        runAction(root, el.getAttribute(attr));
      });
    });
  }

  // ── client-side navigation ───────────────────────────────────────────────
  // A link marked data-nav is fetched as a fragment ({title, html}) and swapped
  // into the root mount WITHOUT a page reload, so the one SSE connection and all
  // live facets survive across pages. Falls back to a full load on any error.

  function rootMount() { return document.querySelector('[data-facet-id="fa:root"]'); }

  function navigate(url, push) {
    fetch(url, { headers: { 'FA-Nav': '1' }, credentials: 'same-origin' })
      .then(function (r) { return (r.ok || r.status === 404) ? r.json() : null; })
      .then(function (data) {
        if (!data) { window.location.href = url; return; }
        var root = rootMount();
        if (root) root.innerHTML = data.html;
        if (data.title) document.title = data.title;
        if (push) history.pushState({ faNav: 1 }, '', url);
        scanSubscribes();  // new content may declare channel subscriptions
        scanClient();      // … and vault/media elements to hydrate
        hydrateReactive(); // … and reactive facets to paint
        updateRoute();     // … and the route signal + active links to refresh (Brick 10)
        window.scrollTo(0, 0);
      })
      .catch(function () { window.location.href = url; });
  }

  function wireNav() {
    document.addEventListener('click', function (e) {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey) return;
      var a = e.target.closest('a[data-nav]');
      if (!a) return;
      var href = a.getAttribute('href');
      if (!href || href.charAt(0) === '#' || a.target === '_blank') return;
      var u;
      try { u = new URL(href, location.href); } catch (_) { return; }
      if (u.origin !== location.origin) return; // same-origin only
      e.preventDefault();
      navigate(u.pathname + u.search, true);
    });
    window.addEventListener('popstate', function () {
      navigate(location.pathname + location.search, false);
    });
  }

  // scanSubscribes auto-subscribes to channels declared on the page via
  // data-fa-subscribe="channel" — a CSP-safe way (no inline script) for a
  // server-rendered surface to opt into scoped live updates.
  function scanSubscribes() {
    var els = document.querySelectorAll('[data-fa-subscribe]');
    for (var i = 0; i < els.length; i++) {
      var ch = els[i].getAttribute('data-fa-subscribe');
      if (ch) window.fa.subscribe(ch);
    }
  }

  function boot() {
    fetch(cfg.manifest_path)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (m) {
        if (m && m.runtime) assign(cfg, m.runtime);
        if (m && m.facets) m.facets.forEach(function (f) {
          registry[f.name] = f;
          if (f.handlers) f.handlers.forEach(function (h) { reactiveEvents[h.event] = true; });
        });
      })
      .catch(function () {})
      .then(initKey)
      .then(function () { connectSSE(); wireActions(); wireNav(); wireReactive(); wireInputs(); scanSubscribes(); scanClient(); hydrateReactive(); updateRoute(); });
  }

  // Node-requireable for unit tests of the pure helpers (no DOM/network).
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      fill: fill, evalExpr: evalExpr, applyAction: applyAction, bindingText: bindingText,
      computeScope: computeScope, listItems: listItems, runEffects: runEffects,
      rootsOf: rootsOf, tplRoots: tplRoots, facetGraph: facetGraph, dirtySet: dirtySet,
      changedKeys: changedKeys, longestIncreasing: longestIncreasing, visibleRange: visibleRange,
      _register: function (name, entry) { registry[name] = entry; } // test seam: register a facet view
    };
  }

  // Browser: expose the public API and boot. Public API: subscribe to a channel
  // for topic fan-out (server authorizes), and provide a vault's decryption key
  // (client-side only — never sent).
  if (typeof window !== 'undefined') {
    window.fa = {
      subscribe: function (channel) {
        subscriptions[channel] = 1;
        if (connID) send('fa.subscribe', { channel: channel });
      },
      unsubscribe: function (channel) {
        delete subscriptions[channel];
        if (connID) send('fa.unsubscribe', { channel: channel });
      },
      vault: { key: provideVaultKey }
    };

    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', boot);
    } else {
      boot();
    }
  }
})();

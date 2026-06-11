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
  var connID = null;
  var pending = [];          // actions fired before connID is known
  var subscriptions = {};    // channels to (re)subscribe to
  var registry = {};         // facet name → manifest entry (kind, window, ttl, client body)

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

  // fill substitutes {field} holes in a TRUSTED body (the compiled manifest's
  // client render body) with HTML-ESCAPED values — round 1 supports plain field
  // interpolation only (no client-side if/for).
  function fill(body, values) {
    return String(body).replace(/\{\s*([A-Za-z_]\w*)\s*\}/g, function (_, name) {
      return Object.prototype.hasOwnProperty.call(values, name) ? esc(values[name]) : '';
    });
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
    if (ev.op === '_conn') { onConn(ev.conn); return; } // control frame, not a DOM op
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
    var es = new EventSource(cfg.sse_path);
    es.onmessage = function (e) { try { handleEvent(JSON.parse(e.data)); } catch (_) {} };
    es.onopen = function () { delay = 1000; };
    es.onerror = function () {
      es.close();
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
        scanSubscribes(); // new content may declare channel subscriptions
        scanClient();     // … and vault/media elements to hydrate
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
        if (m && m.facets) m.facets.forEach(function (f) { registry[f.name] = f; });
      })
      .catch(function () {})
      .then(initKey)
      .then(function () { connectSSE(); wireActions(); wireNav(); scanSubscribes(); scanClient(); });
  }

  // Public API: subscribe to a channel for topic fan-out (server authorizes),
  // and provide a vault's decryption key (client-side only — never sent).
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
})();

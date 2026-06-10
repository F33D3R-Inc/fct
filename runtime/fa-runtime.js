// fa-runtime.js — Facet Architecture client runtime (v0 walking skeleton).
//
// Pure plumbing. It:
//   1. holds one SSE connection; its first frame (op "_conn") carries this
//      connection's id, which the runtime echoes on every action so the server
//      can deliver the response to THIS connection only (no cross-client leak);
//   2. verifies each pushed event's HMAC against <meta name="fa-key"> and applies
//      it to the DOM node identified by data-facet-id;
//   3. forwards [data-action] clicks to a SINGLE /events endpoint;
//   4. exposes fa.subscribe(channel) for topic fan-out, re-subscribing on reconnect.
//
// No per-action route table, no application logic, no client state beyond the
// connection id and pending/subscription bookkeeping.
(function () {
  'use strict';

  var cfg = { sse_path: '/sse', events_path: '/events', manifest_path: '/manifest.json' };
  var connID = null;
  var pending = [];          // actions fired before connID is known
  var subscriptions = {};    // channels to (re)subscribe to

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
      case 'replace': if (el) el.outerHTML = ev.fragment; break;
      case 'append':  if (el) el.insertAdjacentHTML('beforeend', ev.fragment); break;
      case 'prepend': if (el) el.insertAdjacentHTML('afterbegin', ev.fragment); break;
      case 'remove':  if (el) el.remove(); break;
      default: break;
    }
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

  function boot() {
    fetch(cfg.manifest_path)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (m) { if (m && m.runtime) assign(cfg, m.runtime); })
      .catch(function () {})
      .then(initKey)
      .then(function () { connectSSE(); wireActions(); });
  }

  // Public API: subscribe to a channel for topic fan-out (server authorizes).
  window.fa = {
    subscribe: function (channel) {
      subscriptions[channel] = 1;
      if (connID) send('fa.subscribe', { channel: channel });
    },
    unsubscribe: function (channel) {
      delete subscriptions[channel];
      if (connID) send('fa.unsubscribe', { channel: channel });
    }
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();

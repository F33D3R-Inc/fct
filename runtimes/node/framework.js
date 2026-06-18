'use strict';
// Framework parity surface — the Node ports of fa/session.go, fa/authz.go,
// fa/security.go, fa/form.go, and fa/broker.go. These are the "batteries" that
// must be IDENTICAL across every backend language for FA's language-agnostic
// pitch to hold: signed-cookie sessions, who: authorization, CSRF/same-origin,
// per-IP rate limiting, forms, and the pluggable broker. The session cookie and
// HMAC layout match Go byte-for-byte, so a cookie minted by a Go server is read by
// this one (and vice versa) when they share a signing key.

const crypto = require('crypto');

// ── signed-cookie sessions (mirror fa/session.go) ───────────────────────────

class Sessions {
  constructor(keyHex, opts) {
    opts = opts || {};
    this.key = Buffer.from(keyHex || '', 'hex');
    this.name = opts.name || 'fa_session';
    this.maxAge = opts.maxAge != null ? opts.maxAge : 30 * 24 * 3600; // seconds
    this.secure = opts.secure !== false;
    this.sameSite = opts.sameSite || 'Lax';
  }

  _sign(payload) {
    const mac = crypto.createHmac('sha256', this.key);
    mac.update(payload);
    return payload + '.' + mac.digest('hex');
  }

  // setCookie returns the Set-Cookie header value carrying the signed session.
  setCookie(values) {
    const payload = Buffer.from(JSON.stringify(values)).toString('base64url');
    let c = this.name + '=' + this._sign(payload) + '; Path=/; HttpOnly; SameSite=' + this.sameSite + '; Max-Age=' + this.maxAge;
    if (this.secure) c += '; Secure';
    return c;
  }

  clearCookie() {
    let c = this.name + '=; Path=/; HttpOnly; SameSite=' + this.sameSite + '; Max-Age=0';
    if (this.secure) c += '; Secure';
    return c;
  }

  // load reads + verifies the session from a Cookie header, returning its values
  // ({} when missing/forged/tampered).
  load(cookieHeader) {
    const raw = parseCookie(cookieHeader || '', this.name);
    if (!raw) return {};
    const dot = raw.lastIndexOf('.');
    if (dot < 0) return {};
    const payload = raw.slice(0, dot);
    const sig = raw.slice(dot + 1);
    const mac = crypto.createHmac('sha256', this.key);
    mac.update(payload);
    const want = mac.digest();
    let got;
    try { got = Buffer.from(sig, 'hex'); } catch (e) { return {}; }
    if (got.length !== want.length || !crypto.timingSafeEqual(got, want)) return {};
    try { return JSON.parse(Buffer.from(payload, 'base64url').toString('utf8')) || {}; } catch (e) { return {}; }
  }

  identity(cookieHeader) { return this.load(cookieHeader).uid || ''; }
}

function parseCookie(header, name) {
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq < 0) continue;
    if (part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim();
  }
  return null;
}

// ── who: authorization (mirror fa/authz.go) ─────────────────────────────────

// enforceWho applies a facet's who: block for a viewer. Returns { allowed, data }
// — allowed=false means a require policy denied the viewer (render nothing);
// otherwise data is a redacted COPY (the caller's data is never mutated).
function enforceWho(who, policies, view, data) {
  if (!who) return { allowed: true, data: data };
  for (const name of who.require || []) {
    const fn = policies[name];
    if (!fn || !fn(view)) return { allowed: false, data: data };
  }
  let out = data;
  for (const r of who.redact || []) {
    if (r.unless && policies[r.unless] && policies[r.unless](view)) continue; // policy passes → keep
    out = deleteField(out, r.field.split('.'));
  }
  return { allowed: true, data: out };
}

// deleteField returns a copy of obj with the dotted path removed, cloning only
// the containers along the path (siblings are shared — render is read-only).
function deleteField(obj, parts) {
  if (obj == null || typeof obj !== 'object') return obj;
  const copy = Array.isArray(obj) ? obj.slice() : Object.assign({}, obj);
  const head = parts[0];
  if (parts.length === 1) { delete copy[head]; return copy; }
  if (copy[head] != null) copy[head] = deleteField(copy[head], parts.slice(1));
  return copy;
}

// ── CSRF / same-origin (mirror fa/security.go sameOrigin) ───────────────────

function sameOrigin(req) {
  const origin = req.headers['origin'];
  if (!origin) return true; // same-origin nav or non-browser client
  try {
    const host = new URL(origin).host;
    return host === req.headers['host'];
  } catch (e) { return false; }
}

// ── per-IP rate limiter (mirror fa/security.go rateLimiter) ──────────────────

class RateLimiter {
  constructor(ratePerSec, burst) {
    this.rate = ratePerSec;
    this.burst = burst;
    this.buckets = new Map();
  }

  allow(key) {
    const now = Date.now() / 1000;
    let b = this.buckets.get(key);
    if (!b) {
      if (this.buckets.size > 100000) this.buckets.clear();
      b = { n: this.burst, last: now };
      this.buckets.set(key, b);
    }
    b.n += (now - b.last) * this.rate;
    if (b.n > this.burst) b.n = this.burst;
    b.last = now;
    if (b.n < 1) return false;
    b.n -= 1;
    return true;
  }
}

// ── forms (mirror fa/form.go) ───────────────────────────────────────────────

const EMAIL_RE = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;

class Form {
  constructor(values) {
    this.values = values || {};
    this.errors = {};
  }

  static parse(contentType, body) {
    const values = {};
    if ((contentType || '').indexOf('application/x-www-form-urlencoded') >= 0) {
      for (const pair of body.split('&')) {
        if (!pair) continue;
        const eq = pair.indexOf('=');
        const k = decodeURIComponent((eq < 0 ? pair : pair.slice(0, eq)).replace(/\+/g, ' '));
        const v = eq < 0 ? '' : decodeURIComponent(pair.slice(eq + 1).replace(/\+/g, ' '));
        if (!(k in values)) values[k] = v;
      }
    }
    return new Form(values);
  }

  get(field) { return (this.values[field] || '').trim(); }
  _fail(field, msg) { if (!(field in this.errors)) this.errors[field] = msg; }
  required(field, msg) { if (this.get(field) === '') this._fail(field, msg); return this; }
  minLen(field, n, msg) { const v = this.get(field); if (v !== '' && [...v].length < n) this._fail(field, msg); return this; }
  maxLen(field, n, msg) { if ([...this.get(field)].length > n) this._fail(field, msg); return this; }
  email(field, msg) { const v = this.get(field); if (v !== '' && !EMAIL_RE.test(v)) this._fail(field, msg); return this; }
  confirm(field, other, msg) { if (this.get(field) !== this.get(other)) this._fail(field, msg); return this; }
  check(field, ok, msg) { if (!ok) this._fail(field, msg); return this; }
  valid() { return Object.keys(this.errors).length === 0; }
  error(field) { return this.errors[field] || ''; }
}

// ── broker (mirror fa/broker.go) ────────────────────────────────────────────

// LocalBroker is the default single-process broker: publish delivers
// synchronously to local subscribers. A multi-instance deployment supplies a
// Redis/NATS-backed broker with the same two methods (publish/subscribe).
class LocalBroker {
  constructor() { this.fns = []; }
  publish(msg) { for (const fn of this.fns) fn(msg); }
  subscribe(fn) { this.fns.push(fn); }
}

// ── password hashing + accounts (mirror fa/auth.go) ─────────────────────────

const PBKDF2_ITER = 600000;
const SALT_LEN = 16;
const KEY_LEN = 32;
const SCHEME = 'pbkdf2-sha256';

// hashPassword returns a self-describing PBKDF2-HMAC-SHA256 hash, format
// "pbkdf2-sha256$<iter>$<rawstd-b64 salt>$<rawstd-b64 key>" — identical to
// fa/auth.go, so a hash made by a Go server verifies here and vice versa.
function hashPassword(pw, iter) {
  iter = iter || PBKDF2_ITER;
  const salt = crypto.randomBytes(SALT_LEN);
  const key = crypto.pbkdf2Sync(pw, salt, iter, KEY_LEN, 'sha256');
  return SCHEME + '$' + iter + '$' + b64raw(salt) + '$' + b64raw(key);
}

function verifyPassword(encoded, pw) {
  const parts = (encoded || '').split('$');
  if (parts.length !== 4 || parts[0] !== SCHEME) return false;
  const iter = parseInt(parts[1], 10);
  if (!(iter >= 1)) return false;
  let salt, want;
  try { salt = Buffer.from(parts[2], 'base64'); want = Buffer.from(parts[3], 'base64'); } catch (e) { return false; }
  if (want.length === 0) return false;
  const got = crypto.pbkdf2Sync(pw, salt, iter, want.length, 'sha256');
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}

function b64raw(buf) { return buf.toString('base64').replace(/=+$/, ''); }

// A decoy hash so login() of an unknown user still spends ~one verify (mitigates
// user-enumeration timing).
const DECOY = hashPassword('decoy-' + crypto.randomBytes(8).toString('hex'), 1);

// Auth is an in-memory account store with hashed passwords. Record the returned
// account's login in the session (uid) after a successful login.
class Auth {
  constructor() { this.users = new Map(); } // login → { login, hash, roles }

  signup(login, password, roles) {
    if (!login) throw new Error('fa: empty login');
    if (this.users.has(login)) throw new Error('fa: login taken');
    if ((password || '').length < 8) throw new Error('fa: weak password (min 8)');
    const u = { login: login, hash: hashPassword(password), roles: roles || [] };
    this.users.set(login, u);
    return u;
  }

  login(login, password) {
    const u = this.users.get(login);
    if (!u) { verifyPassword(DECOY, password); return null; } // constant-ish time
    return verifyPassword(u.hash, password) ? u : null;
  }

  get(login) { return this.users.get(login) || null; }
}

// ── metrics (mirror fa/observe.go) ──────────────────────────────────────────

class Metrics {
  constructor() {
    this.events_in = 0; this.events_out = 0;
    this.conns_active = 0; this.conns_total = 0;
    this.rate_limited = 0; this.forbidden = 0;
  }
  snapshot() {
    return {
      events_in: this.events_in, events_out: this.events_out,
      conns_active: this.conns_active, conns_total: this.conns_total,
      rate_limited: this.rate_limited, forbidden: this.forbidden,
    };
  }
  prometheus() {
    const m = this.snapshot();
    const line = (name, help, typ, v) => '# HELP ' + name + ' ' + help + '\n# TYPE ' + name + ' ' + typ + '\n' + name + ' ' + v + '\n';
    return line('fa_events_in_total', 'Client actions received at /events.', 'counter', m.events_in) +
      line('fa_events_out_total', 'Events published to the broker.', 'counter', m.events_out) +
      line('fa_sse_connections_active', 'Currently-open SSE connections.', 'gauge', m.conns_active) +
      line('fa_sse_connections_total', 'SSE connections ever opened.', 'counter', m.conns_total) +
      line('fa_events_rate_limited_total', 'Requests to /events rejected by the per-IP rate limit (429).', 'counter', m.rate_limited) +
      line('fa_events_forbidden_total', 'Requests to /events rejected by guard/authz/CSRF (403).', 'counter', m.forbidden);
  }
}

module.exports = {
  Sessions, parseCookie, enforceWho, deleteField, sameOrigin, RateLimiter, Form, LocalBroker,
  hashPassword, verifyPassword, Auth, Metrics,
};

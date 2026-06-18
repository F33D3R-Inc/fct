"""Framework parity surface — the Python ports of fa/session.go, fa/authz.go,
fa/security.go, fa/form.go, and fa/broker.go. These "batteries" are identical
across every backend language so FA's language-agnostic pitch holds: signed-cookie
sessions, who: authorization, CSRF/same-origin, per-IP rate limiting, forms, and a
pluggable broker. The session cookie + HMAC layout match Go byte-for-byte (a cookie
minted by a Go server reads here, given a shared signing key).
"""

import base64
import hashlib
import hmac
import json
import os
import re
import time
from urllib.parse import unquote_plus, urlparse


# -- signed-cookie sessions (mirror fa/session.go) ---------------------------

class Sessions:
    def __init__(self, key_hex, name="fa_session", max_age=30 * 24 * 3600, secure=True, same_site="Lax"):
        self.key = bytes.fromhex(key_hex) if key_hex else b""
        self.name = name
        self.max_age = max_age
        self.secure = secure
        self.same_site = same_site

    def _sign(self, payload):
        mac = hmac.new(self.key, payload.encode(), hashlib.sha256).hexdigest()
        return payload + "." + mac

    def set_cookie(self, values):
        body = json.dumps(values, separators=(",", ":")).encode()
        payload = base64.urlsafe_b64encode(body).rstrip(b"=").decode()
        c = "%s=%s; Path=/; HttpOnly; SameSite=%s; Max-Age=%d" % (self.name, self._sign(payload), self.same_site, self.max_age)
        if self.secure:
            c += "; Secure"
        return c

    def clear_cookie(self):
        c = "%s=; Path=/; HttpOnly; SameSite=%s; Max-Age=0" % (self.name, self.same_site)
        if self.secure:
            c += "; Secure"
        return c

    def load(self, cookie_header):
        raw = parse_cookie(cookie_header or "", self.name)
        if not raw or "." not in raw:
            return {}
        payload, _, sig = raw.rpartition(".")
        want = hmac.new(self.key, payload.encode(), hashlib.sha256).hexdigest()
        if not hmac.compare_digest(sig, want):
            return {}
        try:
            pad = "=" * (-len(payload) % 4)
            body = base64.urlsafe_b64decode(payload + pad)
            out = json.loads(body)
            return out if isinstance(out, dict) else {}
        except (ValueError, TypeError):
            return {}

    def identity(self, cookie_header):
        return self.load(cookie_header).get("uid", "")


def parse_cookie(header, name):
    for part in header.split(";"):
        if "=" not in part:
            continue
        k, _, v = part.partition("=")
        if k.strip() == name:
            return v.strip()
    return None


# -- who: authorization (mirror fa/authz.go) ---------------------------------

def enforce_who(who, policies, view, data):
    """Returns (allowed, data). allowed False ⇒ a require policy denied the viewer
    (render nothing). data is a redacted COPY (caller's data never mutated)."""
    if not who:
        return True, data
    for name in who.get("require", []):
        fn = policies.get(name)
        if fn is None or not fn(view):
            return False, data
    out = data
    for r in who.get("redact", []):
        unless = r.get("unless")
        if unless and policies.get(unless) and policies[unless](view):
            continue
        out = delete_field(out, r["field"].split("."))
    return True, out


def delete_field(obj, parts):
    if not isinstance(obj, dict):
        return obj
    copy = dict(obj)
    head = parts[0]
    if len(parts) == 1:
        copy.pop(head, None)
        return copy
    if head in copy:
        copy[head] = delete_field(copy[head], parts[1:])
    return copy


# -- CSRF / same-origin (mirror fa/security.go sameOrigin) -------------------

def same_origin(origin, host):
    if not origin:
        return True
    try:
        return urlparse(origin).netloc == host
    except ValueError:
        return False


# -- per-IP rate limiter (mirror fa/security.go rateLimiter) ------------------

class RateLimiter:
    def __init__(self, rate_per_sec=20, burst=40):
        self.rate = rate_per_sec
        self.burst = burst
        self.buckets = {}

    def allow(self, key):
        now = time.monotonic()
        b = self.buckets.get(key)
        if b is None:
            if len(self.buckets) > 100000:
                self.buckets.clear()
            b = {"n": self.burst, "last": now}
            self.buckets[key] = b
        b["n"] += (now - b["last"]) * self.rate
        if b["n"] > self.burst:
            b["n"] = self.burst
        b["last"] = now
        if b["n"] < 1:
            return False
        b["n"] -= 1
        return True


# -- forms (mirror fa/form.go) -----------------------------------------------

_EMAIL_RE = re.compile(r"^[^@\s]+@[^@\s]+\.[^@\s]+$")


class Form:
    def __init__(self, values=None):
        self.values = values or {}
        self.errors = {}

    @classmethod
    def parse(cls, content_type, body):
        values = {}
        if "application/x-www-form-urlencoded" in (content_type or ""):
            for pair in body.split("&"):
                if not pair:
                    continue
                k, _, v = pair.partition("=")
                k = unquote_plus(k)
                if k not in values:
                    values[k] = unquote_plus(v)
        return cls(values)

    def get(self, field):
        return (self.values.get(field) or "").strip()

    def _fail(self, field, msg):
        if field not in self.errors:
            self.errors[field] = msg

    def required(self, field, msg):
        if self.get(field) == "":
            self._fail(field, msg)
        return self

    def min_len(self, field, n, msg):
        v = self.get(field)
        if v != "" and len(v) < n:
            self._fail(field, msg)
        return self

    def max_len(self, field, n, msg):
        if len(self.get(field)) > n:
            self._fail(field, msg)
        return self

    def email(self, field, msg):
        v = self.get(field)
        if v != "" and not _EMAIL_RE.match(v):
            self._fail(field, msg)
        return self

    def confirm(self, field, other, msg):
        if self.get(field) != self.get(other):
            self._fail(field, msg)
        return self

    def check(self, field, ok, msg):
        if not ok:
            self._fail(field, msg)
        return self

    def valid(self):
        return len(self.errors) == 0

    def error(self, field):
        return self.errors.get(field, "")


# -- broker (mirror fa/broker.go) --------------------------------------------

class LocalBroker:
    """Default single-process broker: publish delivers synchronously to local
    subscribers. A multi-instance deployment supplies a Redis/NATS broker with the
    same publish(msg)/subscribe(fn) methods."""

    def __init__(self):
        self.fns = []

    def publish(self, msg):
        for fn in self.fns:
            fn(msg)

    def subscribe(self, fn):
        self.fns.append(fn)


# -- password hashing + accounts (mirror fa/auth.go) -------------------------

PBKDF2_ITER = 600000
SALT_LEN = 16
KEY_LEN = 32
SCHEME = "pbkdf2-sha256"


def _b64(b):
    return base64.b64encode(b).rstrip(b"=").decode()


def _b64d(s):
    return base64.b64decode(s + "=" * (-len(s) % 4))


def hash_password(pw, iterations=PBKDF2_ITER):
    """Self-describing PBKDF2-HMAC-SHA256 hash, format
    "pbkdf2-sha256$<iter>$<rawstd-b64 salt>$<rawstd-b64 key>" — identical to
    fa/auth.go, so a hash made by a Go server verifies here and vice versa."""
    salt = os.urandom(SALT_LEN)
    key = hashlib.pbkdf2_hmac("sha256", pw.encode(), salt, iterations, KEY_LEN)
    return "%s$%d$%s$%s" % (SCHEME, iterations, _b64(salt), _b64(key))


def verify_password(encoded, pw):
    parts = (encoded or "").split("$")
    if len(parts) != 4 or parts[0] != SCHEME:
        return False
    try:
        iterations = int(parts[1])
    except ValueError:
        return False
    if iterations < 1:
        return False
    try:
        salt = _b64d(parts[2])
        want = _b64d(parts[3])
    except (ValueError, TypeError):
        return False
    if not want:
        return False
    got = hashlib.pbkdf2_hmac("sha256", pw.encode(), salt, iterations, len(want))
    return hmac.compare_digest(got, want)


_DECOY = hash_password("decoy-" + os.urandom(8).hex(), 1)


class Auth:
    """In-memory account store with hashed passwords. Record the returned account's
    login in the session (uid) after a successful login."""

    def __init__(self):
        self.users = {}  # login → {"login", "hash", "roles"}

    def signup(self, login, password, roles=None):
        if not login:
            raise ValueError("fa: empty login")
        if login in self.users:
            raise ValueError("fa: login taken")
        if len(password or "") < 8:
            raise ValueError("fa: weak password (min 8)")
        u = {"login": login, "hash": hash_password(password), "roles": roles or []}
        self.users[login] = u
        return u

    def login(self, login, password):
        u = self.users.get(login)
        if u is None:
            verify_password(_DECOY, password)  # constant-ish time
            return None
        return u if verify_password(u["hash"], password) else None

    def get(self, login):
        return self.users.get(login)


# -- metrics (mirror fa/observe.go) ------------------------------------------

class Metrics:
    def __init__(self):
        self.events_in = 0
        self.events_out = 0
        self.conns_active = 0
        self.conns_total = 0
        self.rate_limited = 0
        self.forbidden = 0

    def snapshot(self):
        return {
            "events_in": self.events_in, "events_out": self.events_out,
            "conns_active": self.conns_active, "conns_total": self.conns_total,
            "rate_limited": self.rate_limited, "forbidden": self.forbidden,
        }

    def prometheus(self):
        m = self.snapshot()

        def line(name, help_, typ, v):
            return "# HELP %s %s\n# TYPE %s %s\n%s %d\n" % (name, help_, name, typ, name, v)

        return (
            line("fa_events_in_total", "Client actions received at /events.", "counter", m["events_in"]) +
            line("fa_events_out_total", "Events published to the broker.", "counter", m["events_out"]) +
            line("fa_sse_connections_active", "Currently-open SSE connections.", "gauge", m["conns_active"]) +
            line("fa_sse_connections_total", "SSE connections ever opened.", "counter", m["conns_total"]) +
            line("fa_events_rate_limited_total", "Requests to /events rejected by the per-IP rate limit (429).", "counter", m["rate_limited"]) +
            line("fa_events_forbidden_total", "Requests to /events rejected by guard/authz/CSRF (403).", "counter", m["forbidden"])
        )

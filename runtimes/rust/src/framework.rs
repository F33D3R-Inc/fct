//! Framework parity surface — the Rust ports of fa/session.go, fa/authz.go,
//! fa/security.go, fa/form.go, and fa/broker.go. These "batteries" are identical
//! across every backend language so FA's language-agnostic pitch holds:
//! signed-cookie sessions, who: authorization, CSRF/same-origin, per-IP rate
//! limiting, forms, and a pluggable broker. The session cookie + HMAC layout match
//! Go byte-for-byte (a cookie minted by a Go server reads here, given a shared
//! key). Dependency-free: base64url is implemented here (std has none).

use crate::json::{self, Json};
use crate::sha256;
use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Instant;

// ── base64url (no padding; RawURLEncoding) ──────────────────────────────────

const B64: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

pub fn b64url_encode(data: &[u8]) -> String {
    let mut out = String::new();
    for chunk in data.chunks(3) {
        let b0 = chunk[0] as u32;
        let b1 = *chunk.get(1).unwrap_or(&0) as u32;
        let b2 = *chunk.get(2).unwrap_or(&0) as u32;
        let n = (b0 << 16) | (b1 << 8) | b2;
        out.push(B64[((n >> 18) & 63) as usize] as char);
        out.push(B64[((n >> 12) & 63) as usize] as char);
        if chunk.len() > 1 {
            out.push(B64[((n >> 6) & 63) as usize] as char);
        }
        if chunk.len() > 2 {
            out.push(B64[(n & 63) as usize] as char);
        }
    }
    out
}

pub fn b64url_decode(s: &str) -> Option<Vec<u8>> {
    let mut rev = [255u8; 256];
    for (i, &c) in B64.iter().enumerate() {
        rev[c as usize] = i as u8;
    }
    let bytes = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < bytes.len() {
        let n = bytes.len() - i;
        if n < 2 {
            return None;
        }
        let c0 = rev[bytes[i] as usize];
        let c1 = rev[bytes[i + 1] as usize];
        if c0 == 255 || c1 == 255 {
            return None;
        }
        out.push((c0 << 2) | (c1 >> 4));
        if n >= 3 {
            let c2 = rev[bytes[i + 2] as usize];
            if c2 == 255 {
                return None;
            }
            out.push((c1 << 4) | (c2 >> 2));
            if n >= 4 {
                let c3 = rev[bytes[i + 3] as usize];
                if c3 == 255 {
                    return None;
                }
                out.push((c2 << 6) | c3);
            }
        }
        i += 4;
    }
    Some(out)
}

// ── signed-cookie sessions (mirror fa/session.go) ───────────────────────────

pub struct Sessions {
    key: Vec<u8>,
    pub name: String,
    max_age: i64,
    secure: bool,
    same_site: String,
}

impl Sessions {
    pub fn new(key_hex: &str) -> Sessions {
        Sessions {
            key: sha256::from_hex(key_hex),
            name: "fa_session".to_string(),
            max_age: 30 * 24 * 3600,
            secure: true,
            same_site: "Lax".to_string(),
        }
    }

    pub fn with_name(mut self, name: &str) -> Sessions {
        self.name = name.to_string();
        self
    }
    pub fn insecure(mut self) -> Sessions {
        self.secure = false;
        self
    }

    fn sign(&self, payload: &str) -> String {
        format!("{}.{}", payload, sha256::hex(&sha256::hmac_sha256(&self.key, payload.as_bytes())))
    }

    /// set_cookie returns the Set-Cookie header value for the signed session.
    /// `values` is a Json::Obj of string values.
    pub fn set_cookie(&self, values: &Json) -> String {
        let payload = b64url_encode(values.to_string().as_bytes());
        let mut c = format!("{}={}; Path=/; HttpOnly; SameSite={}; Max-Age={}", self.name, self.sign(&payload), self.same_site, self.max_age);
        if self.secure {
            c.push_str("; Secure");
        }
        c
    }

    pub fn clear_cookie(&self) -> String {
        let mut c = format!("{}=; Path=/; HttpOnly; SameSite={}; Max-Age=0", self.name, self.same_site);
        if self.secure {
            c.push_str("; Secure");
        }
        c
    }

    /// load reads + verifies the session from a Cookie header → a Json::Obj
    /// (empty when missing/forged/tampered).
    pub fn load(&self, cookie_header: &str) -> Json {
        let empty = Json::Obj(Vec::new());
        let raw = match parse_cookie(cookie_header, &self.name) {
            Some(r) => r,
            None => return empty,
        };
        let dot = match raw.rfind('.') {
            Some(d) => d,
            None => return empty,
        };
        let (payload, sig) = (&raw[..dot], &raw[dot + 1..]);
        let want = sha256::hex(&sha256::hmac_sha256(&self.key, payload.as_bytes()));
        if !ct_eq(sig.as_bytes(), want.as_bytes()) {
            return empty;
        }
        match b64url_decode(payload).and_then(|b| json::parse(&String::from_utf8_lossy(&b))) {
            Some(j @ Json::Obj(_)) => j,
            _ => empty,
        }
    }

    pub fn identity(&self, cookie_header: &str) -> String {
        self.load(cookie_header).get("uid").and_then(|j| j.as_str()).unwrap_or("").to_string()
    }
}

/// ct_eq is a length-checked, constant-time-ish byte compare (avoids early-out on
/// the first mismatched byte for the signature check).
fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for i in 0..a.len() {
        diff |= a[i] ^ b[i];
    }
    diff == 0
}

pub fn parse_cookie(header: &str, name: &str) -> Option<String> {
    for part in header.split(';') {
        let part = part.trim();
        if let Some((k, v)) = part.split_once('=') {
            if k.trim() == name {
                return Some(v.trim().to_string());
            }
        }
    }
    None
}

// ── who: authorization (mirror fa/authz.go) ─────────────────────────────────

pub struct View {
    pub identity: String,
}

pub type Policy = Box<dyn Fn(&View) -> bool + Send + Sync>;

pub struct WhoDef {
    pub require: Vec<String>,
    pub redact: Vec<(String, String)>, // (field path, unless-policy)
}

/// enforce_who applies a facet's who: block. Returns (allowed, data) — allowed
/// false ⇒ a require policy denied the viewer; data is a redacted copy.
pub fn enforce_who(who: Option<&WhoDef>, policies: &HashMap<String, Policy>, view: &View, data: &Json) -> (bool, Json) {
    let who = match who {
        Some(w) => w,
        None => return (true, data.clone()),
    };
    for name in &who.require {
        match policies.get(name) {
            Some(f) if f(view) => {}
            _ => return (false, data.clone()),
        }
    }
    let mut out = data.clone();
    for (field, unless) in &who.redact {
        if !unless.is_empty() {
            if let Some(p) = policies.get(unless) {
                if p(view) {
                    continue;
                }
            }
        }
        let parts: Vec<&str> = field.split('.').collect();
        out = delete_field(&out, &parts);
    }
    (true, out)
}

/// delete_field returns a copy of data with the dotted path removed.
pub fn delete_field(data: &Json, parts: &[&str]) -> Json {
    let pairs = match data {
        Json::Obj(p) => p,
        _ => return data.clone(),
    };
    let head = parts[0];
    let mut out = Vec::new();
    for (k, v) in pairs {
        if k == head {
            if parts.len() == 1 {
                continue; // drop
            }
            out.push((k.clone(), delete_field(v, &parts[1..])));
        } else {
            out.push((k.clone(), v.clone()));
        }
    }
    Json::Obj(out)
}

// ── CSRF / same-origin (mirror fa/security.go sameOrigin) ───────────────────

pub fn same_origin(origin: Option<&str>, host: Option<&str>) -> bool {
    let origin = match origin {
        Some(o) if !o.is_empty() => o,
        _ => return true, // same-origin nav or non-browser client
    };
    // Extract the host portion of the Origin URL (after scheme://, before /).
    let after_scheme = origin.split("://").nth(1).unwrap_or(origin);
    let origin_host = after_scheme.split('/').next().unwrap_or("");
    Some(origin_host) == host
}

// ── per-IP rate limiter (mirror fa/security.go rateLimiter) ──────────────────

pub struct RateLimiter {
    rate: f64,
    burst: f64,
    buckets: Mutex<HashMap<String, (f64, Instant)>>,
}

impl RateLimiter {
    pub fn new(rate_per_sec: f64, burst: f64) -> RateLimiter {
        RateLimiter { rate: rate_per_sec, burst, buckets: Mutex::new(HashMap::new()) }
    }

    pub fn allow(&self, key: &str) -> bool {
        let now = Instant::now();
        let mut buckets = self.buckets.lock().unwrap();
        if buckets.len() > 100_000 && !buckets.contains_key(key) {
            buckets.clear();
        }
        let entry = buckets.entry(key.to_string()).or_insert((self.burst, now));
        let elapsed = now.duration_since(entry.1).as_secs_f64();
        entry.0 = (entry.0 + elapsed * self.rate).min(self.burst);
        entry.1 = now;
        if entry.0 < 1.0 {
            return false;
        }
        entry.0 -= 1.0;
        true
    }
}

// ── forms (mirror fa/form.go) ───────────────────────────────────────────────

pub struct Form {
    pub values: HashMap<String, String>,
    pub errors: HashMap<String, String>,
}

impl Form {
    pub fn parse(content_type: &str, body: &str) -> Form {
        let mut values = HashMap::new();
        if content_type.contains("application/x-www-form-urlencoded") {
            for pair in body.split('&') {
                if pair.is_empty() {
                    continue;
                }
                let (k, v) = pair.split_once('=').unwrap_or((pair, ""));
                values.entry(url_decode(k)).or_insert_with(|| url_decode(v));
            }
        }
        Form { values, errors: HashMap::new() }
    }

    pub fn get(&self, field: &str) -> String {
        self.values.get(field).map(|s| s.trim().to_string()).unwrap_or_default()
    }
    fn fail(&mut self, field: &str, msg: &str) {
        self.errors.entry(field.to_string()).or_insert_with(|| msg.to_string());
    }
    pub fn required(&mut self, field: &str, msg: &str) -> &mut Form {
        if self.get(field).is_empty() {
            self.fail(field, msg);
        }
        self
    }
    pub fn min_len(&mut self, field: &str, n: usize, msg: &str) -> &mut Form {
        let v = self.get(field);
        if !v.is_empty() && v.chars().count() < n {
            self.fail(field, msg);
        }
        self
    }
    pub fn email(&mut self, field: &str, msg: &str) -> &mut Form {
        let v = self.get(field);
        if !v.is_empty() && !plausible_email(&v) {
            self.fail(field, msg);
        }
        self
    }
    pub fn valid(&self) -> bool {
        self.errors.is_empty()
    }
    pub fn error(&self, field: &str) -> String {
        self.errors.get(field).cloned().unwrap_or_default()
    }
}

fn plausible_email(v: &str) -> bool {
    let parts: Vec<&str> = v.split('@').collect();
    parts.len() == 2 && !parts[0].is_empty() && parts[1].contains('.') && !parts[1].starts_with('.') && !parts[1].ends_with('.') && !v.contains(char::is_whitespace)
}

fn url_decode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            b'%' if i + 2 < bytes.len() => {
                let hi = (bytes[i + 1] as char).to_digit(16);
                let lo = (bytes[i + 2] as char).to_digit(16);
                if let (Some(h), Some(l)) = (hi, lo) {
                    out.push((h * 16 + l) as u8);
                    i += 3;
                } else {
                    out.push(bytes[i]);
                    i += 1;
                }
            }
            c => {
                out.push(c);
                i += 1;
            }
        }
    }
    String::from_utf8_lossy(&out).to_string()
}

// ── broker (mirror fa/broker.go) ────────────────────────────────────────────

/// Broker fans event messages across application instances. The default is
/// in-process (LocalBroker); a multi-instance deployment supplies a Redis/NATS
/// adapter implementing the same two methods.
pub trait Broker: Send + Sync {
    fn publish(&self, msg: String);
    fn on_message(&self, handler: Arc<dyn Fn(String) + Send + Sync>);
}

pub struct LocalBroker {
    handler: Mutex<Option<Arc<dyn Fn(String) + Send + Sync>>>,
}

impl LocalBroker {
    pub fn new() -> LocalBroker {
        LocalBroker { handler: Mutex::new(None) }
    }
}

impl Default for LocalBroker {
    fn default() -> Self {
        LocalBroker::new()
    }
}

impl Broker for LocalBroker {
    fn publish(&self, msg: String) {
        let h = self.handler.lock().unwrap().clone();
        if let Some(h) = h {
            h(msg);
        }
    }
    fn on_message(&self, handler: Arc<dyn Fn(String) + Send + Sync>) {
        *self.handler.lock().unwrap() = Some(handler);
    }
}

// ── standard base64 (alphabet +/, no padding) ───────────────────────────────

const B64STD: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

fn b64_encode(data: &[u8], table: &[u8]) -> String {
    let mut out = String::new();
    for chunk in data.chunks(3) {
        let b0 = chunk[0] as u32;
        let b1 = *chunk.get(1).unwrap_or(&0) as u32;
        let b2 = *chunk.get(2).unwrap_or(&0) as u32;
        let n = (b0 << 16) | (b1 << 8) | b2;
        out.push(table[((n >> 18) & 63) as usize] as char);
        out.push(table[((n >> 12) & 63) as usize] as char);
        if chunk.len() > 1 {
            out.push(table[((n >> 6) & 63) as usize] as char);
        }
        if chunk.len() > 2 {
            out.push(table[(n & 63) as usize] as char);
        }
    }
    out
}

fn b64_decode(s: &str, table: &[u8]) -> Option<Vec<u8>> {
    let mut rev = [255u8; 256];
    for (i, &c) in table.iter().enumerate() {
        rev[c as usize] = i as u8;
    }
    let bytes = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < bytes.len() {
        let n = bytes.len() - i;
        if n < 2 {
            return None;
        }
        let c0 = rev[bytes[i] as usize];
        let c1 = rev[bytes[i + 1] as usize];
        if c0 == 255 || c1 == 255 {
            return None;
        }
        out.push((c0 << 2) | (c1 >> 4));
        if n >= 3 {
            let c2 = rev[bytes[i + 2] as usize];
            if c2 == 255 {
                return None;
            }
            out.push((c1 << 4) | (c2 >> 2));
            if n >= 4 {
                let c3 = rev[bytes[i + 3] as usize];
                if c3 == 255 {
                    return None;
                }
                out.push((c2 << 6) | c3);
            }
        }
        i += 4;
    }
    Some(out)
}

// ── password hashing + accounts (mirror fa/auth.go) ─────────────────────────

const PBKDF2_ITER: u32 = 600_000;
const PBKDF2_SALT_LEN: usize = 16;
const PBKDF2_KEY_LEN: usize = 32;
const PBKDF2_SCHEME: &str = "pbkdf2-sha256";

/// pbkdf2_hmac_sha256 derives a dklen-byte key (RFC 8018, HMAC-SHA256 PRF).
pub fn pbkdf2_hmac_sha256(password: &[u8], salt: &[u8], iter: u32, dklen: usize) -> Vec<u8> {
    let hlen = 32usize;
    let blocks = dklen.div_ceil(hlen);
    let mut out = Vec::with_capacity(blocks * hlen);
    for block in 1..=blocks as u32 {
        let mut msg = salt.to_vec();
        msg.extend_from_slice(&block.to_be_bytes());
        let mut u = sha256::hmac_sha256(password, &msg);
        let mut t = u;
        for _ in 1..iter {
            u = sha256::hmac_sha256(password, &u);
            for j in 0..hlen {
                t[j] ^= u[j];
            }
        }
        out.extend_from_slice(&t);
    }
    out.truncate(dklen);
    out
}

/// hash_password returns a self-describing PBKDF2-HMAC-SHA256 hash, format
/// "pbkdf2-sha256$<iter>$<rawstd-b64 salt>$<rawstd-b64 key>" — identical to
/// fa/auth.go, so a hash made by a Go server verifies here and vice versa.
pub fn hash_password(password: &str) -> String {
    hash_password_iter(password, PBKDF2_ITER)
}

pub fn hash_password_iter(password: &str, iter: u32) -> String {
    let salt = rand_bytes(PBKDF2_SALT_LEN);
    let key = pbkdf2_hmac_sha256(password.as_bytes(), &salt, iter, PBKDF2_KEY_LEN);
    format!("{}${}${}${}", PBKDF2_SCHEME, iter, b64_encode(&salt, B64STD), b64_encode(&key, B64STD))
}

pub fn verify_password(encoded: &str, password: &str) -> bool {
    let parts: Vec<&str> = encoded.split('$').collect();
    if parts.len() != 4 || parts[0] != PBKDF2_SCHEME {
        return false;
    }
    let iter: u32 = match parts[1].parse() {
        Ok(n) if n >= 1 => n,
        _ => return false,
    };
    let salt = match b64_decode(parts[2], B64STD) {
        Some(s) => s,
        None => return false,
    };
    let want = match b64_decode(parts[3], B64STD) {
        Some(w) if !w.is_empty() => w,
        _ => return false,
    };
    let got = pbkdf2_hmac_sha256(password.as_bytes(), &salt, iter, want.len());
    ct_eq(&got, &want)
}

/// rand_bytes reads cryptographic entropy from /dev/urandom, with a time+hash
/// fallback (dependency-free; no rng crate).
pub fn rand_bytes(n: usize) -> Vec<u8> {
    use std::io::Read;
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let mut buf = vec![0u8; n];
        if f.read_exact(&mut buf).is_ok() {
            return buf;
        }
    }
    use std::time::{SystemTime, UNIX_EPOCH};
    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default();
    let mut seed = now.as_nanos().to_le_bytes().to_vec();
    let mut out = Vec::new();
    let mut ctr = 0u64;
    while out.len() < n {
        seed.extend_from_slice(&ctr.to_le_bytes());
        out.extend_from_slice(&sha256::sha256(&seed));
        ctr += 1;
    }
    out.truncate(n);
    out
}

#[derive(Clone)]
pub struct Account {
    pub login: String,
    pub hash: String,
    pub roles: Vec<String>,
}

/// Auth is an in-memory account store with hashed passwords.
pub struct Auth {
    users: Mutex<HashMap<String, Account>>,
}

impl Default for Auth {
    fn default() -> Self {
        Auth::new()
    }
}

impl Auth {
    pub fn new() -> Auth {
        Auth { users: Mutex::new(HashMap::new()) }
    }

    pub fn signup(&self, login: &str, password: &str, roles: Vec<String>) -> Result<Account, String> {
        if login.is_empty() {
            return Err("fa: empty login".into());
        }
        if password.chars().count() < 8 {
            return Err("fa: weak password (min 8)".into());
        }
        let mut users = self.users.lock().unwrap();
        if users.contains_key(login) {
            return Err("fa: login taken".into());
        }
        let acct = Account { login: login.to_string(), hash: hash_password(password), roles };
        users.insert(login.to_string(), acct.clone());
        Ok(acct)
    }

    pub fn login(&self, login: &str, password: &str) -> Option<Account> {
        let users = self.users.lock().unwrap();
        match users.get(login) {
            Some(a) if verify_password(&a.hash, password) => Some(a.clone()),
            _ => None,
        }
    }
}

// ── metrics (mirror fa/observe.go) ──────────────────────────────────────────

#[derive(Default)]
pub struct Metrics {
    pub events_in: AtomicI64,
    pub events_out: AtomicI64,
    pub conns_active: AtomicI64,
    pub conns_total: AtomicI64,
    pub rate_limited: AtomicI64,
    pub forbidden: AtomicI64,
}

impl Metrics {
    pub fn new() -> Metrics {
        Metrics::default()
    }

    pub fn inc(field: &AtomicI64) {
        field.fetch_add(1, Ordering::Relaxed);
    }

    pub fn inc_by(field: &AtomicI64, n: i64) {
        field.fetch_add(n, Ordering::Relaxed);
    }

    pub fn snapshot_json(&self) -> Json {
        Json::obj(vec![
            ("events_in", Json::Num(self.events_in.load(Ordering::Relaxed) as f64)),
            ("events_out", Json::Num(self.events_out.load(Ordering::Relaxed) as f64)),
            ("conns_active", Json::Num(self.conns_active.load(Ordering::Relaxed) as f64)),
            ("conns_total", Json::Num(self.conns_total.load(Ordering::Relaxed) as f64)),
            ("rate_limited", Json::Num(self.rate_limited.load(Ordering::Relaxed) as f64)),
            ("forbidden", Json::Num(self.forbidden.load(Ordering::Relaxed) as f64)),
        ])
    }

    pub fn prometheus(&self) -> String {
        let g = |f: &AtomicI64| f.load(Ordering::Relaxed);
        let line = |name: &str, help: &str, typ: &str, v: i64| format!("# HELP {n} {h}\n# TYPE {n} {t}\n{n} {v}\n", n = name, h = help, t = typ, v = v);
        format!(
            "{}{}{}{}{}{}",
            line("fa_events_in_total", "Client actions received at /events.", "counter", g(&self.events_in)),
            line("fa_events_out_total", "Events published to the broker.", "counter", g(&self.events_out)),
            line("fa_sse_connections_active", "Currently-open SSE connections.", "gauge", g(&self.conns_active)),
            line("fa_sse_connections_total", "SSE connections ever opened.", "counter", g(&self.conns_total)),
            line("fa_events_rate_limited_total", "Requests to /events rejected by the per-IP rate limit (429).", "counter", g(&self.rate_limited)),
            line("fa_events_forbidden_total", "Requests to /events rejected by guard/authz/CSRF (403).", "counter", g(&self.forbidden)),
        )
    }
}

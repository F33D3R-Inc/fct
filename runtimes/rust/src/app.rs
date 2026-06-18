//! The App: the Rust analogue of Go's `fa` package. Owns the HTTP + SSE
//! transport, the HMAC-signed event push, the render-IR interpreter, and the
//! /events router. Dependency-free — a hand-rolled HTTP/1.1 server over
//! std::net, the JSON/SHA-256 modules, and std threads.

use crate::framework::{self, Broker, View};
use crate::json::{self, Json};
use crate::native;
use crate::render::{self, Node, Scope};
use crate::sha256;
use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::mpsc::{self, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

pub const WIRE_VERSION: &str = "1";

/// Ctx is passed to a handler: the action type, its payload, and the acting
/// connection id.
pub struct Ctx {
    pub type_: String,
    pub payload: Json,
    pub conn: String,
    pub identity: String,
}

pub type Handler = Box<dyn Fn(&Ctx) -> Option<Json> + Send + Sync>;
pub type RootFn = Box<dyn Fn() -> Json + Send + Sync>;

/// AdminRow is one row in a resource's list view.
pub struct AdminRow {
    pub id: String,
    pub cells: Vec<String>,
}

/// AdminResource is one manageable model in the admin panel.
pub struct AdminResource {
    pub name: String,
    pub label: String,
    pub columns: Vec<String>,
    pub list: Box<dyn Fn() -> Vec<AdminRow> + Send + Sync>,
    pub get: Box<dyn Fn(&str) -> Vec<(String, String)> + Send + Sync>,
}

/// AdminCfg configures the deny-by-default admin panel. authorize receives the
/// viewer identity; with no AdminCfg set, /admin is refused.
pub struct AdminCfg {
    pub title: String,
    pub authorize: Box<dyn Fn(&str) -> bool + Send + Sync>,
    pub resources: Vec<AdminResource>,
}

struct FacetDef {
    facet_id: String,
    tree: Vec<Node>,
    who: Option<framework::WhoDef>,
}

struct WhenDef {
    events: Vec<String>,
    mutations: Vec<(String, String)>, // (op, target)
}

pub struct App {
    key_hex: String,
    title: String,
    runtime_js: String,
    manifest_raw: String,
    render_raw: String,
    facets: HashMap<String, FacetDef>,
    whens: Vec<WhenDef>, // facet-name-agnostic; each carries its own default target
    handlers: HashMap<String, Handler>,
    root: Option<(String, RootFn)>,
    conns: Arc<Mutex<HashMap<String, Conn>>>,
    // Framework surface (uniform with fa/).
    policies: HashMap<String, framework::Policy>,
    identity_fn: Option<Box<dyn Fn(&str) -> String + Send + Sync>>,
    sessions: Option<Arc<framework::Sessions>>,
    limiter: framework::RateLimiter,
    broker: Arc<dyn Broker>,
    metrics: framework::Metrics,
    auth: Option<Arc<framework::Auth>>,
    admin: Option<AdminCfg>,
}

/// A live SSE connection: its event channel, whether it is a native (FA-Native)
/// client (ViewNode-tree fragments instead of HTML), and the viewer identity.
struct Conn {
    tx: Sender<String>,
    native: bool,
    identity: String,
}

impl App {
    /// new loads the generated artifacts from `gen_dir`. `fa_key` is the hex
    /// signing key ("" disables signing). `runtime_js` is the path to the shared
    /// fa-runtime.js.
    pub fn new(gen_dir: &str, fa_key: &str, title: &str, runtime_js: &str) -> App {
        let manifest_raw = std::fs::read_to_string(format!("{}/manifest.json", gen_dir))
            .expect("read manifest.json");
        let render_raw = std::fs::read_to_string(format!("{}/render.json", gen_dir))
            .expect("read render.json");
        let ir = json::parse(&render_raw).expect("parse render.json");

        let mut facets = HashMap::new();
        let mut whens = Vec::new();
        if let Some(arr) = ir.get("facets").and_then(|j| j.as_arr()) {
            for f in arr {
                let name = f.get("name").and_then(|j| j.as_str()).unwrap_or("").to_string();
                let facet_id = f.get("facet_id").and_then(|j| j.as_str()).unwrap_or("").to_string();
                let empty = Vec::new();
                let ops = f.get("render").and_then(|j| j.as_arr()).unwrap_or(&empty);
                let who = f.get("who").map(|w| {
                    let require = w
                        .get("require")
                        .and_then(|j| j.as_arr())
                        .map(|a| a.iter().filter_map(|j| j.as_str().map(String::from)).collect())
                        .unwrap_or_default();
                    let mut redact = Vec::new();
                    if let Some(rs) = w.get("redact").and_then(|j| j.as_arr()) {
                        for r in rs {
                            let field = r.get("field").and_then(|j| j.as_str()).unwrap_or("").to_string();
                            let unless = r.get("unless").and_then(|j| j.as_str()).unwrap_or("").to_string();
                            redact.push((field, unless));
                        }
                    }
                    framework::WhoDef { require, redact }
                });
                facets.insert(name.clone(), FacetDef { facet_id, tree: render::build_tree(ops), who });
                if let Some(ws) = f.get("when").and_then(|j| j.as_arr()) {
                    for w in ws {
                        let events: Vec<String> = w
                            .get("events")
                            .and_then(|j| j.as_arr())
                            .map(|a| a.iter().filter_map(|j| j.as_str().map(String::from)).collect())
                            .unwrap_or_default();
                        let mut mutations = Vec::new();
                        if let Some(ms) = w.get("mutations").and_then(|j| j.as_arr()) {
                            for m in ms {
                                let op = m.get("op").and_then(|j| j.as_str()).unwrap_or("replace").to_string();
                                let target = m
                                    .get("target")
                                    .and_then(|j| j.as_str())
                                    .map(String::from)
                                    .unwrap_or_else(|| name.clone());
                                mutations.push((op, target));
                            }
                        }
                        whens.push(WhenDef { events, mutations });
                    }
                }
            }
        }

        App {
            key_hex: fa_key.to_string(),
            title: title.to_string(),
            runtime_js: runtime_js.to_string(),
            manifest_raw,
            render_raw,
            facets,
            whens,
            handlers: HashMap::new(),
            root: None,
            conns: Arc::new(Mutex::new(HashMap::new())),
            policies: HashMap::new(),
            identity_fn: None,
            sessions: None,
            limiter: framework::RateLimiter::new(20.0, 40.0),
            broker: Arc::new(framework::LocalBroker::new()),
            metrics: framework::Metrics::new(),
            auth: None,
            admin: None,
        }
    }

    /// with_auth installs an in-memory password-account store. Get it with
    /// auth_store() to signup users before listen().
    pub fn with_auth(mut self) -> App {
        self.auth = Some(Arc::new(framework::Auth::new()));
        self
    }

    pub fn auth_store(&self) -> Option<Arc<framework::Auth>> {
        self.auth.clone()
    }

    /// admin enables the deny-by-default admin panel at GET /admin.
    pub fn admin(mut self, cfg: AdminCfg) -> App {
        self.admin = Some(cfg);
        self
    }

    pub fn root<F: Fn() -> Json + Send + Sync + 'static>(mut self, name: &str, f: F) -> App {
        self.root = Some((name.to_string(), Box::new(f)));
        self
    }

    pub fn on<F: Fn(&Ctx) -> Option<Json> + Send + Sync + 'static>(mut self, type_: &str, f: F) -> App {
        self.handlers.insert(type_.to_string(), Box::new(f));
        self
    }

    /// policy registers a named who: authorization policy. Chainable.
    pub fn policy<F: Fn(&View) -> bool + Send + Sync + 'static>(mut self, name: &str, f: F) -> App {
        self.policies.insert(name.to_string(), Box::new(f));
        self
    }

    /// identify sets the resolver mapping a request's Cookie header to a stable
    /// identity (used for who: views and scoped delivery).
    pub fn identify<F: Fn(&str) -> String + Send + Sync + 'static>(mut self, f: F) -> App {
        self.identity_fn = Some(Box::new(f));
        self
    }

    /// with_sessions installs a signed-cookie SessionManager bound to this app's
    /// key as the default identity resolver (the "uid" value).
    pub fn with_sessions(mut self) -> App {
        let s = Arc::new(framework::Sessions::new(&self.key_hex));
        if self.identity_fn.is_none() {
            let s2 = Arc::clone(&s);
            self.identity_fn = Some(Box::new(move |cookie| s2.identity(cookie)));
        }
        self.sessions = Some(s);
        self
    }

    /// broker overrides the default in-process broker for multi-instance fan-out.
    pub fn with_broker(mut self, b: Arc<dyn Broker>) -> App {
        self.broker = b;
        self
    }

    /// sessions returns the configured SessionManager (for minting login cookies).
    pub fn session_manager(&self) -> Option<Arc<framework::Sessions>> {
        self.sessions.clone()
    }

    fn view_from_cookie(&self, cookie: &str) -> View {
        let identity = self.identity_fn.as_ref().map(|f| f(cookie)).unwrap_or_default();
        View { identity }
    }

    /// render_facet interprets a facet's IR with `data` → (facet_id, html), with
    /// data-facet-id injected on the root element. It ENFORCES the facet's who:
    /// block for `view` (denied require → empty render; redact strips fields from a
    /// data copy); `view` threads to child facets so nested who: is gated too.
    pub fn render_facet(&self, name: &str, data: &Json, view: &View) -> (String, String) {
        let def = match self.facets.get(name) {
            Some(d) => d,
            None => return (String::new(), String::new()),
        };
        let (allowed, data) = framework::enforce_who(def.who.as_ref(), &self.policies, view, data);
        if !allowed {
            return (String::new(), String::new());
        }
        let fid = render::resolve_facet_id(&def.facet_id, &data);
        let locals: HashMap<String, Json> = HashMap::new();
        let body = self.render_nodes(&def.tree, &Scope { data: &data, locals: &locals }, view);
        (fid.clone(), render::inject_facet_id(&body, &fid))
    }

    fn render_nodes(&self, nodes: &[Node], scope: &Scope, view: &View) -> String {
        let mut out = String::new();
        for n in nodes {
            match n {
                Node::Text(s) => out.push_str(s),
                Node::Expr(x) => out.push_str(&render::html_escape(&render::fmt(&render::eval(x, scope)))),
                Node::If { x, then_, els } => {
                    let branch = if render::truthy(&render::eval(x, scope)) { then_ } else { els };
                    out.push_str(&self.render_nodes(branch, scope, view));
                }
                Node::For { var, x, body } => {
                    if let Some(items) = render::eval(x, scope).as_arr() {
                        for item in items {
                            let mut locals = scope.locals.clone();
                            locals.insert(var.clone(), item.clone());
                            out.push_str(&self.render_nodes(body, &Scope { data: scope.data, locals: &locals }, view));
                        }
                    }
                }
                Node::Child { name, props } => {
                    let mut pairs: Vec<(&str, Json)> = Vec::new();
                    for p in props {
                        let v = match &p.x {
                            Some(x) => render::eval(x, scope),
                            None => Json::Str(p.lit.clone()),
                        };
                        pairs.push((p.name.as_str(), v));
                    }
                    let (_, html) = self.render_facet(name, &Json::obj(pairs), view);
                    out.push_str(&html);
                }
            }
        }
        out
    }

    /// sign builds an event frame, attaching the HMAC over op \0 facet_id \0
    /// fragment when a key is set (matching fa/event.go).
    fn sign(&self, op: &str, facet_id: &str, fragment: &str) -> Json {
        let mut pairs = vec![
            ("op", Json::Str(op.to_string())),
            ("facet_id", Json::Str(facet_id.to_string())),
            ("fragment", Json::Str(fragment.to_string())),
        ];
        if !self.key_hex.is_empty() {
            let mut msg = Vec::new();
            msg.extend_from_slice(op.as_bytes());
            msg.push(0);
            msg.extend_from_slice(facet_id.as_bytes());
            msg.push(0);
            msg.extend_from_slice(fragment.as_bytes());
            let mac = sha256::hmac_sha256(&sha256::from_hex(&self.key_hex), &msg);
            pairs.push(("hmac", Json::Str(sha256::hex(&mac))));
        }
        Json::obj(pairs)
    }

    fn push_rerender(&self, type_: &str, conn: &str, data: &Json) {
        // Snapshot the connection's native flag + identity, render the mutations
        // (enforcing who: for that viewer), and publish the signed frames through
        // the broker — which delivers to whichever instance holds the connection.
        let (native_conn, identity) = {
            let conns = self.conns.lock().unwrap();
            match conns.get(conn) {
                Some(c) => (c.native, c.identity.clone()),
                None => return,
            }
        };
        let view = View { identity };
        let mut frames: Vec<Json> = Vec::new();
        for w in &self.whens {
            if !w.events.iter().any(|e| e == type_) {
                continue;
            }
            for (op, target) in &w.mutations {
                let (fid, html) = self.render_facet(target, data, &view);
                if fid.is_empty() && html.is_empty() {
                    continue; // who: denied
                }
                let op = if op == "replace_all" { "replace" } else { op.as_str() };
                let fragment = if native_conn { native::tree_json(&html) } else { html };
                frames.push(Json::Str(self.sign(op, &fid, &fragment).to_string()));
            }
        }
        if frames.is_empty() {
            return;
        }
        framework::Metrics::inc_by(&self.metrics.events_out, frames.len() as i64);
        let msg = Json::obj(vec![("conn", Json::Str(conn.to_string())), ("frames", Json::Arr(frames))]);
        self.broker.publish(msg.to_string());
    }

    /// deliver_local applies a broker message to a locally-held connection (the
    /// subscribe side of the broker). A conn not found here lives on another instance.
    fn deliver_local(&self, msg: &str) {
        let parsed = match json::parse(msg) {
            Some(p) => p,
            None => return,
        };
        let conn = parsed.get("conn").and_then(|j| j.as_str()).unwrap_or("");
        let tx = {
            let conns = self.conns.lock().unwrap();
            match conns.get(conn) {
                Some(c) => c.tx.clone(),
                None => return,
            }
        };
        if let Some(frames) = parsed.get("frames").and_then(|j| j.as_arr()) {
            for f in frames {
                if let Some(frame) = f.as_str() {
                    let _ = tx.send(format!("data: {}\n\n", frame));
                }
            }
        }
    }

    pub fn listen(self, addr: &str) {
        let addr = addr.to_string();
        let listener = TcpListener::bind(&addr).expect("bind");
        println!("fa(rust): listening on http://{}", addr);
        let app = Arc::new(self);
        // Wire the broker's delivery handler to this app's local connections.
        let a = Arc::clone(&app);
        app.broker.on_message(Arc::new(move |msg| a.deliver_local(&msg)));
        for stream in listener.incoming() {
            if let Ok(stream) = stream {
                let app = Arc::clone(&app);
                std::thread::spawn(move || app.serve(stream));
            }
        }
    }

    fn serve(&self, mut stream: TcpStream) {
        let (method, path, headers, body) = match read_request(&mut stream) {
            Some(r) => r,
            None => return,
        };
        let is_native = headers.get("fa-native").map(|v| v == "1").unwrap_or(false);
        let cookie = headers.get("cookie").cloned().unwrap_or_default();
        let origin = headers.get("origin").cloned();
        let host = headers.get("host").cloned();
        let ip = stream.peer_addr().map(|a| a.ip().to_string()).unwrap_or_default();
        let route = path.split('?').next().unwrap_or("/");
        match (method.as_str(), route) {
            ("GET", "/") => self.serve_shell(&mut stream, is_native, &cookie),
            ("GET", "/sse") => self.serve_sse(stream, &path, is_native, &cookie),
            ("POST", "/events") => self.handle_events(&mut stream, &body, origin.as_deref(), host.as_deref(), &ip),
            ("GET", "/manifest.json") => write_response(&mut stream, 200, "application/json", self.manifest_raw.as_bytes()),
            ("GET", "/render.json") => write_response(&mut stream, 200, "application/json", self.render_raw.as_bytes()),
            ("GET", "/healthz") => write_response(&mut stream, 200, "text/plain", b"ok"),
            ("GET", "/readyz") => write_response(&mut stream, 200, "text/plain", b"ready"),
            ("GET", "/debug/metrics") => write_response(&mut stream, 200, "application/json", self.metrics.snapshot_json().to_string().as_bytes()),
            ("GET", "/metrics") => write_response(&mut stream, 200, "text/plain; version=0.0.4; charset=utf-8", self.metrics.prometheus().as_bytes()),
            ("GET", "/admin") => {
                let identity = self.view_from_cookie(&cookie).identity;
                self.serve_admin(&mut stream, &path, &identity);
            }
            ("GET", "/fa-runtime.js") => match std::fs::read(&self.runtime_js) {
                Ok(js) => write_response(&mut stream, 200, "text/javascript", &js),
                Err(_) => write_response(&mut stream, 404, "text/plain", b"no runtime"),
            },
            _ => write_response(&mut stream, 404, "text/plain", b"not found"),
        }
    }

    fn serve_shell(&self, stream: &mut TcpStream, is_native: bool, cookie: &str) {
        let view = self.view_from_cookie(cookie);
        // Native client (FA-Native: 1) → ScreenResponse {title, tree}; browser → HTML.
        if is_native {
            let tree = match &self.root {
                Some((name, f)) => native::parse_view_json(&self.render_facet(name, &f(), &view).1),
                None => Json::obj(vec![("kind", Json::Str("box".into()))]),
            };
            let resp = Json::obj(vec![("title", Json::Str(self.title.clone())), ("tree", tree)]);
            write_response(stream, 200, "application/json", resp.to_string().as_bytes());
            return;
        }
        let body = match &self.root {
            Some((name, f)) => self.render_facet(name, &f(), &view).1,
            None => String::new(),
        };
        let page = format!(
            "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"fa-key\" content=\"{}\"><title>{}</title></head><body>{}<script src=\"/fa-runtime.js\"></script></body></html>",
            self.key_hex,
            render::html_escape(&self.title),
            body
        );
        // Baseline security headers (CSP is the backstop behind escaping).
        let data = page.into_bytes();
        let head = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Security-Policy: default-src 'self'; style-src 'self' 'unsafe-inline'\r\nX-Content-Type-Options: nosniff\r\nReferrer-Policy: same-origin\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            data.len()
        );
        let _ = stream.write_all(head.as_bytes());
        let _ = stream.write_all(&data);
        let _ = stream.flush();
    }

    fn serve_sse(&self, mut stream: TcpStream, path: &str, is_native: bool, cookie: &str) {
        let v = query_param(path, "v").unwrap_or_else(|| "1".to_string());
        if v != WIRE_VERSION {
            write_response(&mut stream, 426, "text/plain", b"unsupported wire version");
            return;
        }
        let head = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: keep-alive\r\nX-Accel-Buffering: no\r\n\r\n";
        if stream.write_all(head.as_bytes()).is_err() {
            return;
        }

        let cid = sha256::hex(&random16());
        let (tx, rx) = mpsc::channel::<String>();
        let identity = self.view_from_cookie(cookie).identity;
        self.conns.lock().unwrap().insert(cid.clone(), Conn { tx, native: is_native, identity });
        framework::Metrics::inc(&self.metrics.conns_active);
        framework::Metrics::inc(&self.metrics.conns_total);

        let hello = Json::obj(vec![
            ("op", Json::Str("_conn".into())),
            ("conn", Json::Str(cid.clone())),
            ("key", Json::Str(self.key_hex.clone())),
            ("v", Json::Str(WIRE_VERSION.into())),
        ]);
        if stream.write_all(format!("data: {}\n\n", hello.to_string()).as_bytes()).is_err() {
            self.conns.lock().unwrap().remove(&cid);
            framework::Metrics::inc_by(&self.metrics.conns_active, -1);
            return;
        }
        let _ = stream.flush();

        loop {
            let msg = match rx.recv_timeout(Duration::from_secs(25)) {
                Ok(m) => m,
                Err(mpsc::RecvTimeoutError::Timeout) => ": keepalive\n\n".to_string(),
                Err(_) => break,
            };
            if stream.write_all(msg.as_bytes()).is_err() || stream.flush().is_err() {
                break;
            }
        }
        self.conns.lock().unwrap().remove(&cid);
        framework::Metrics::inc_by(&self.metrics.conns_active, -1);
    }

    fn serve_admin(&self, stream: &mut TcpStream, path: &str, identity: &str) {
        let adm = match &self.admin {
            Some(a) => a,
            None => {
                write_response(stream, 403, "text/plain", b"forbidden");
                return;
            }
        };
        if !(adm.authorize)(identity) {
            write_response(stream, 403, "text/plain", b"forbidden");
            return;
        }
        let esc = render::html_escape;
        let mut body = format!("<h1>{}</h1>", esc(&adm.title));
        body.push_str("<h2>Metrics</h2><ul>");
        if let Json::Obj(pairs) = self.metrics.snapshot_json() {
            for (k, v) in pairs {
                body.push_str(&format!("<li>{}: <b>{}</b></li>", k, render::fmt(&v)));
            }
        }
        body.push_str("</ul>");
        body.push_str(&format!("<p>Open connections: <b>{}</b></p>", self.conns.lock().unwrap().len()));
        let res_name = query_param(path, "resource");
        let resource = res_name.as_ref().and_then(|n| adm.resources.iter().find(|r| &r.name == n));
        match (resource, query_param(path, "id")) {
            (Some(r), Some(id)) => {
                body.push_str(&format!("<h2>{} · {}</h2><table>", esc(&r.label), esc(&id)));
                for (label, value) in (r.get)(&id) {
                    body.push_str(&format!("<tr><th>{}</th><td>{}</td></tr>", esc(&label), esc(&value)));
                }
                body.push_str("</table>");
            }
            (Some(r), None) => {
                body.push_str(&format!("<h2>{}</h2><table><tr>", esc(&r.label)));
                for c in &r.columns {
                    body.push_str(&format!("<th>{}</th>", esc(c)));
                }
                body.push_str("</tr>");
                for row in (r.list)() {
                    body.push_str("<tr>");
                    for c in &row.cells {
                        body.push_str(&format!("<td>{}</td>", esc(c)));
                    }
                    body.push_str(&format!(" <td><a href=\"/admin?resource={}&id={}\">view</a></td></tr>", r.name, row.id));
                }
                body.push_str("</table>");
            }
            _ => {
                body.push_str("<h2>Resources</h2><ul>");
                for r in &adm.resources {
                    body.push_str(&format!("<li><a href=\"/admin?resource={}\">{}</a></li>", r.name, esc(&r.label)));
                }
                body.push_str("</ul>");
            }
        }
        let page = format!("<!doctype html><html><head><meta charset=\"utf-8\"><title>{}</title></head><body>{}</body></html>", esc(&adm.title), body);
        write_response(stream, 200, "text/html; charset=utf-8", page.as_bytes());
    }

    fn handle_events(&self, stream: &mut TcpStream, body: &[u8], origin: Option<&str>, host: Option<&str>, ip: &str) {
        // CSRF defense-in-depth (cross-origin reject) + per-IP throttle.
        if !framework::same_origin(origin, host) {
            framework::Metrics::inc(&self.metrics.forbidden);
            write_response(stream, 403, "text/plain", b"cross-origin");
            return;
        }
        if !self.limiter.allow(ip) {
            framework::Metrics::inc(&self.metrics.rate_limited);
            write_response(stream, 429, "text/plain", b"rate limited");
            return;
        }
        framework::Metrics::inc(&self.metrics.events_in);
        write_response(stream, 204, "text/plain", b"");
        let parsed = match json::parse(&String::from_utf8_lossy(body)) {
            Some(p) => p,
            None => return,
        };
        let type_ = parsed.get("type").and_then(|j| j.as_str()).unwrap_or("").to_string();
        let conn = parsed.get("conn").and_then(|j| j.as_str()).unwrap_or("").to_string();
        let payload = parsed.get("payload").cloned().unwrap_or(Json::Null);
        let identity = {
            let conns = self.conns.lock().unwrap();
            conns.get(&conn).map(|c| c.identity.clone()).unwrap_or_default()
        };
        if let Some(h) = self.handlers.get(&type_) {
            let ctx = Ctx { type_: type_.clone(), payload, conn: conn.clone(), identity };
            if let Some(data) = h(&ctx) {
                self.push_rerender(&type_, &conn, &data);
            }
        }
    }
}

// ── tiny HTTP plumbing ──────────────────────────────────────────────────────

fn read_request(stream: &mut TcpStream) -> Option<(String, String, HashMap<String, String>, Vec<u8>)> {
    let read_clone = stream.try_clone().ok()?;
    let mut reader = BufReader::new(read_clone);

    let mut line = String::new();
    if reader.read_line(&mut line).ok()? == 0 {
        return None;
    }
    let mut parts = line.trim_end().split(' ');
    let method = parts.next()?.to_string();
    let path = parts.next()?.to_string();

    let mut headers = HashMap::new();
    loop {
        let mut h = String::new();
        if reader.read_line(&mut h).ok()? == 0 {
            break;
        }
        let t = h.trim_end();
        if t.is_empty() {
            break;
        }
        if let Some((k, v)) = t.split_once(':') {
            headers.insert(k.trim().to_lowercase(), v.trim().to_string());
        }
    }

    let mut body = Vec::new();
    if let Some(len) = headers.get("content-length").and_then(|v| v.parse::<usize>().ok()) {
        body.resize(len, 0);
        reader.read_exact(&mut body).ok()?;
    }
    Some((method, path, headers, body))
}

fn write_response(stream: &mut TcpStream, status: u16, content_type: &str, body: &[u8]) {
    let reason = match status {
        200 => "OK",
        204 => "No Content",
        404 => "Not Found",
        426 => "Upgrade Required",
        _ => "OK",
    };
    let head = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        status,
        reason,
        content_type,
        body.len()
    );
    let _ = stream.write_all(head.as_bytes());
    let _ = stream.write_all(body);
    let _ = stream.flush();
}

fn query_param(path: &str, key: &str) -> Option<String> {
    let q = path.split('?').nth(1)?;
    for kv in q.split('&') {
        let mut it = kv.splitn(2, '=');
        if it.next()? == key {
            return Some(it.next().unwrap_or("").to_string());
        }
    }
    None
}

fn random16() -> [u8; 16] {
    // Dependency-free entropy: hash the current time + a per-call counter through
    // SHA-256 and take 16 bytes. Connection ids only need to be unguessable to a
    // remote client, and the time+counter+hash gives that without a crypto-rng
    // crate.
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};
    static CTR: AtomicU64 = AtomicU64::new(0);
    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default();
    let c = CTR.fetch_add(1, Ordering::Relaxed);
    let mut seed = Vec::new();
    seed.extend_from_slice(&now.as_nanos().to_le_bytes());
    seed.extend_from_slice(&c.to_le_bytes());
    let h = sha256::sha256(&seed);
    let mut out = [0u8; 16];
    out.copy_from_slice(&h[..16]);
    out
}

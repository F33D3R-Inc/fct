// Execute facet.js's boot path against the real page's IR + state, under a
// minimal DOM shim, so a render-time throw shows up with a stack trace instead
// of as a blank page in the browser.
const fs = require("fs");

const pageHtml = fs.readFileSync(process.argv[2], "utf8");
const clientJs = fs.readFileSync(process.argv[3], "utf8");

// The page's own CSRF token, when it carried one. A canned value is enough while
// nothing leaves the process, and wrong the moment something does: the authority
// checks the token against the session, so a request made with a stand-in comes
// back 403 and the client — which treats any failed answer as "no answer" —
// silently keeps whatever it had.
const pageCsrf =
  (pageHtml.match(/name="fa-csrf" content="([^"]*)"/) || [null, "test-csrf"])[1];

function scriptById(id) {
  const m = pageHtml.match(
    new RegExp('<script[^>]*id="' + id + '"[^>]*>([\\s\\S]*?)</script>')
  );
  if (!m) throw new Error("missing #" + id + " in page");
  return m[1];
}

// A selector, parsed. The client uses a small, fixed vocabulary — a tag, a
// class, an attribute with or without a value, optionally scoped to direct
// children — and nothing here tries to be more than that. What it must not do is
// silently match nothing: a shim whose querySelector always returns null makes
// every refresh path in the client a no-op, and a test over it proves only that
// the first paint did not throw.
function parseSelector(sel) {
  let s = String(sel).trim();
  const scoped = s.startsWith(":scope >");
  if (scoped) s = s.slice(":scope >".length).trim();
  const out = { scoped, tag: null, classes: [], attrs: [] };
  const tag = s.match(/^[a-zA-Z][\w-]*/);
  if (tag) { out.tag = tag[0].toUpperCase(); s = s.slice(tag[0].length); }
  const re = /\.([-\w]+)|\[([-\w:]+)(?:=["']?([^"'\]]*)["']?)?\]/g;
  let m;
  while ((m = re.exec(s))) {
    if (m[1]) out.classes.push(m[1]);
    else out.attrs.push([m[2], m[3]]);
  }
  return out;
}

class Node {
  constructor(tag) {
    this.tagName = (tag || "").toUpperCase();
    // 1 = element. The client stamps a node's `class`/`style` escape hatch only
    // onto elements (`e.nodeType === 1`); without this the whole branch was dead
    // here, so a client that stopped applying either would still pass.
    this.nodeType = 1;
    this.children = [];
    this.attrs = {};
    this._listeners = {};
    this.style = {};
    this.dataset = {};
    this._text = "";
    this.classList = {
      _s: new Set(),
      add: (...c) => c.forEach((x) => this.classList._s.add(x)),
      remove: (...c) => c.forEach((x) => this.classList._s.delete(x)),
      toggle: (c, on) => (on ? this.classList._s.add(c) : this.classList._s.delete(c)),
      contains: (c) => this.classList._s.has(c),
    };
  }
  get className() { return [...this.classList._s].join(" "); }
  set className(v) { this.classList._s = new Set(String(v).split(/\s+/).filter(Boolean)); }
  get textContent() {
    return this.children.length
      ? this.children.map((c) => c.textContent).join("")
      : this._text;
  }
  set textContent(v) { this.children = []; this._text = String(v); }
  set innerHTML(v) { this.children = []; this._text = String(v); }
  get innerHTML() { return this._text; }
  // A DocumentFragment is not inserted; its children are, and it is left empty.
  // That is what the DOM does, and the client relies on it: an `if` that is not a
  // region renders a fragment so its children become the parent's children (see
  // render0 in runtime/assets/facet.js). A shim that pushed the fragment itself
  // would count a wrapper the browser never creates, and would report the two
  // renderers as agreeing on a structure only one of them produces.
  appendChild(c) {
    if (c && c.nodeType === 11) {
      for (const k of c.children.slice()) this.appendChild(k);
      c.children = [];
      return c;
    }
    this.children.push(c);
    c.parentNode = this;
    return c;
  }
  append(...cs) { cs.forEach((c) => this.appendChild(typeof c === "string" ? new Text(c) : c)); }
  insertBefore(c, ref) {
    const i = this.children.indexOf(ref);
    this.children.splice(i < 0 ? this.children.length : i, 0, c);
    return c;
  }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; }
  replaceChildren(...cs) { this.children = []; cs.forEach((c) => this.appendChild(c)); }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  hasAttribute(k) { return k in this.attrs; }
  addEventListener(type, fn) { (this._listeners[type] || (this._listeners[type] = [])).push(fn); }
  removeEventListener(type, fn) {
    const l = this._listeners[type] || [];
    this._listeners[type] = l.filter((f) => f !== fn);
  }
  matches(sel) {
    const q = typeof sel === "string" ? parseSelector(sel) : sel;
    if (q.tag && q.tag !== this.tagName) return false;
    for (const c of q.classes) if (!this.classList.contains(c)) return false;
    for (const [k, v] of q.attrs) {
      if (!(k in this.attrs)) return false;
      if (v !== undefined && this.attrs[k] !== v) return false;
    }
    return true;
  }
  querySelectorAll(sel) {
    const q = parseSelector(sel);
    const out = [];
    const scan = (n, depth) => {
      for (const c of n.children) {
        if (!c.matches) continue;
        if ((!q.scoped || depth === 0) && c.matches(q)) out.push(c);
        if (!q.scoped) scan(c, depth + 1);
      }
    };
    scan(this, 0);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  closest(sel) {
    const q = parseSelector(sel);
    for (let n = this; n; n = n.parentNode) if (n.matches && n.matches(q)) return n;
    return null;
  }
  focus() {}
  // Descend the tree collecting text, so the shim can report what rendered.
  visibleText() {
    if (!this.children.length) return this._text ? [this._text] : [];
    return this.children.flatMap((c) => (c.visibleText ? c.visibleText() : [String(c)]));
  }
  countNodes() {
    return 1 + this.children.reduce((n, c) => n + (c.countNodes ? c.countNodes() : 1), 0);
  }
  // Every anchor destination the client produced. Text alone cannot distinguish a
  // link from inert text with the same label, which is exactly the difference a
  // computed destination turns on.
  hrefs() {
    const out = this.tagName === "A" && this.attrs.href != null ? [this.attrs.href] : [];
    return out.concat(this.children.flatMap((c) => (c.hrefs ? c.hrefs() : [])));
  }
  // Every value the client put somewhere that is NOT body text: an attribute, an
  // input's placeholder (a DOM *property*, so it is not in attrs), and the text of
  // the controls whose label is an IR attribute rather than a text node. The
  // server writes all of these as escaped markup, so this is the only way to ask
  // whether the two sides put the same characters in the same places.
  // Every two-way control in the tree, with the state a browser would act on.
  // `checked` and the current `value` are DOM *properties* — the server writes
  // them as markup and the client assigns them — so nothing in attrs can answer
  // "is this box ticked", which is the only question a checkbox test is asking.
  // The choices a <select> is offering, as one comparable string: each option's
  // stored value, a `*` on the selected one, then its label, joined by `|`.
  //
  // A select's options are child elements whose `value` and `selected` are DOM
  // *properties* the client assigns, so neither attrs nor the page's text can
  // answer "what can be chosen here, and which is chosen" — which is the entire
  // question a choice list drawn from data asks.
  optionList() {
    return this.children
      .filter((c) => c.tagName === "OPTION")
      .map((o) => {
        const v = o.attrs.value != null ? o.attrs.value : String(o.value == null ? "" : o.value);
        return v + (o.selected ? "*" : "") + "=" + o.textContent;
      })
      .join("|");
  }
  controlDump() {
    const out = [];
    if (this.attrs["data-fa-input"] != null) {
      out.push({
        tag: this.tagName,
        bind: this.attrs["data-fa-input"],
        type: this.attrs.type || "",
        name: this.attrs.name || "",
        value: this.attrs.value != null ? this.attrs.value : String(this.value == null ? "" : this.value),
        checked: !!this.checked,
        role: this.attrs.role || "",
        autocomplete: this.attrs.autocomplete || "",
        cls: this.parentNode ? this.parentNode.className : "",
        options: this.tagName === "SELECT" ? this.optionList() : "",
      });
    }
    return out.concat(this.children.flatMap((c) => (c.controlDump ? c.controlDump() : [])));
  }
  attrDump() {
    const out = [];
    for (const k of Object.keys(this.attrs)) out.push([this.tagName, k, this.attrs[k]]);
    if (this.placeholder != null) out.push([this.tagName, "placeholder", String(this.placeholder)]);
    if (["OPTION", "BUTTON", "LABEL", "A"].includes(this.tagName)) {
      out.push([this.tagName, "text", this.textContent]);
    }
    return out.concat(this.children.flatMap((c) => (c.attrDump ? c.attrDump() : [])));
  }
}

class Text extends Node {
  constructor(t) { super("#text"); this.nodeType = 3; this._text = String(t); }
}

const root = new Node("div");
root.setAttribute("id", "fa-root");

const byId = {
  "fa-root": root,
  "fa-ir": Object.assign(new Node("script"), { _text: scriptById("fa-ir") }),
  "fa-state": Object.assign(new Node("script"), { _text: scriptById("fa-state") }),
  "fa-css": new Node("style"),
  "fa-theme": new Node("style"),
};

const store = {};
global.localStorage = {
  getItem: (k) => (k in store ? store[k] : null),
  setItem: (k, v) => (store[k] = String(v)),
  removeItem: (k) => delete store[k],
};

global.document = {
  documentElement: new Node("html"),
  head: new Node("head"),
  body: new Node("body"),
  cookie: "",
  getElementById: (id) => byId[id] || null,
  createElement: (t) => new Node(t),
  createTextNode: (t) => new Text(t),
  createDocumentFragment: () => Object.assign(new Node("#fragment"), { nodeType: 11 }),
  createComment: (t) => Object.assign(new Text(t), { tagName: "#comment", nodeType: 8 }),
  querySelector: (sel) =>
    sel === 'meta[name="fa-csrf"]'
      ? Object.assign(new Node("meta"), { attrs: { content: pageCsrf } })
      : null,
  querySelectorAll: () => [],
  addEventListener: () => {},
};

global.window = {
  addEventListener: () => {},
  location: { pathname: "/", href: "http://127.0.0.1:9312/", search: "" },
  history: { pushState: () => {}, replaceState: () => {} },
  localStorage: global.localStorage,
  crypto: require("crypto").webcrypto,
  matchMedia: () => ({ matches: false, addEventListener: () => {} }),
  // EventSource deliberately absent: boot must not depend on it.
};

// ── talking to the authority ────────────────────────────────────────────────
//
// argv[5], when given, is {"base": "http://127.0.0.1:PORT", "cookie": "fa_sid=…"}
// and turns on a real `fetch` pointed at the app the test is running.
//
// Boot must not depend on it, and does not: without this argument every request
// the client makes throws on the relative URL and the client swallows it, which
// is the shape every test written before this one runs in. With it, the half of
// the client that asks the authority a question — a region whose rows the render
// did not carry, an aggregate whose value it did not materialize — can be
// observed doing so, and answered.
const authority = process.argv[5] ? JSON.parse(process.argv[5]) : null;
const nodeFetch = global.fetch;
let pendingRequests = 0;
if (authority) {
  global.fetch = (url, opts) => {
    const full = String(url).startsWith("http") ? String(url) : authority.base + url;
    const headers = Object.assign({}, (opts && opts.headers) || {});
    if (authority.cookie) headers["Cookie"] = authority.cookie;
    pendingRequests++;
    return nodeFetch(full, Object.assign({}, opts || {}, { headers }))
      .then((r) => {
        // The client treats any failed answer as "no answer" and silently keeps
        // what it had, which is right in a browser and invisible in a test. Say
        // it here instead.
        if (!r.ok) console.log("REQUEST REFUSED", full, r.status);
        return r;
      })
      .catch((e) => { console.log("REQUEST FAILED", full, String(e)); throw e; })
      .finally(() => { pendingRequests--; });
  };
}

// settle waits for the client's own requests to land and for what they trigger
// to render. A round trip is the point of the test that turns `authority` on, so
// reporting before it returns would measure the wait rather than the answer.
async function settle() {
  if (!authority) return;
  for (let i = 0; i < 100; i++) {
    await new Promise((r) => setTimeout(r, 10));
    if (pendingRequests === 0) {
      // One more turn, so the .then that applies the answer has run.
      await new Promise((r) => setTimeout(r, 10));
      if (pendingRequests === 0) return;
    }
  }
}
global.location = global.window.location;
global.history = global.window.history;
global.navigator = { language: "en-US" };
global.btoa = (s) => Buffer.from(s, "binary").toString("base64");
global.atob = (s) => Buffer.from(s, "base64").toString("binary");
global.requestAnimationFrame = (f) => f();
global.setTimeout = setTimeout;

try {
  new Function(clientJs)();
} catch (e) {
  console.log("=== CLIENT THREW DURING BOOT ===");
  console.log(e && e.stack ? e.stack : e);
  console.log("=== root after the throw:", root.countNodes(), "nodes ===");
  process.exit(1);
}

// report dumps everything a test can ask about what is on the page right now.
// Emitted once after boot and, when a script ran, once after it — so a test can
// assert on the difference an interaction made rather than only on first paint.
function report(prefix) {
  const text = root.visibleText().map((s) => s.trim()).filter(Boolean);
  console.log((prefix || "root ") + "nodes:", root.countNodes());
  if (!prefix) console.log("rendered text:", JSON.stringify(text.slice(0, 25)));
  // The whole rendered text, whitespace-normalized: enough to diff a client render
  // against the server's HTML without depending on how either splits its nodes.
  console.log(prefix + "all text:", JSON.stringify(text.join("").replace(/\s+/g, "")));
  console.log(prefix + "hrefs:", JSON.stringify(root.hrefs()));
  console.log(prefix + "attrs:", JSON.stringify(root.attrDump()));
  console.log(prefix + "controls:", JSON.stringify(root.controlDump()));
}

console.log("=== boot OK ===");
report("");
if (root.countNodes() <= 1) {
  console.log("!!! ROOT IS EMPTY — this is the blank page !!!");
  process.exit(2);
}

// ── driving the page ─────────────────────────────────────────────────────────
//
// argv[4] is an optional JSON list of interactions: [{sel, do, value}]. It exists
// because rendering is only half of a control. The half that was never covered —
// the actor writes the cell, the cell's dependents re-render — is where a control
// bound to a `@client` cell earns its place in the language, and it cannot be
// observed by looking at first paint.
//
// Each step does exactly what a browser does to the element under the pointer and
// no more. For a radio that is: check the one that was clicked. It does NOT
// uncheck the others — the client has to, from the cell, or the assertion that
// one radio is selected fails with two.
function dispatch(target, type) {
  const path = [];
  for (let n = target; n; n = n.parentNode) path.push(n);
  const ev = { type, target, preventDefault() {}, stopPropagation() {}, metaKey: false, ctrlKey: false };
  for (const n of path) for (const fn of (n._listeners && n._listeners[type]) || []) fn(ev);
}

const script = process.argv[4] ? JSON.parse(process.argv[4]) : null;
main();

async function main() {
  // The first paint may itself have asked the authority a question.
  await settle();
  if (!script) return;
  for (const step of script) {
    const el = root.querySelector(step.sel);
    if (!el) {
      console.log("=== NO ELEMENT MATCHED", step.sel, "===");
      process.exit(3);
    }
    const type = el.getAttribute("type");
    if (step.do === "type") {
      el.value = String(step.value);
      dispatch(el, "input");
    } else {
      if (type === "checkbox") el.checked = !el.checked;
      else if (type === "radio") el.checked = true;
      // Real browsers fire both for a checkbox or radio; firing both here is also
      // the check that the second one is a no-op rather than a second write.
      dispatch(el, "input");
      dispatch(el, "change");
    }
  }
  await settle();
  console.log("=== after script ===");
  report("after ");
}

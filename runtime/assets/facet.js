// Facet client runtime — fixed, application-agnostic plumbing. Ships once and
// interprets the per-app IR; no application logic lives here. It renders the
// view from the IR + state, runs client-placed actions locally, forwards
// server-placed actions to the authority, and refreshes only the regions a
// change touches. Its expression semantics mirror runtime/eval.go exactly.
(function () {
  "use strict";

  const root = document.getElementById("fa-root");
  const csrfMeta = document.querySelector('meta[name="fa-csrf"]');
  let csrf = csrfMeta ? csrfMeta.getAttribute("content") : "";

  // ── @e2e sealed fields ───────────────────────────────────────────────────────
  // The dataflow guarantee is the language's: a sealed value is encrypted on this
  // client before it is ever sent (dispatch seals the flagged args), the authority
  // only ever stores/serves ciphertext, and a reader opens it here. fct owns *that*
  // — the cipher itself is delegated to a provider an app can replace (set
  // window.facetE2E before this script to plug in real per-recipient keys, e.g.
  // Vovin). The built-in default is real AES-GCM (Web Crypto) under a per-app key
  // kept in localStorage — enough to demonstrate seal→store-ciphertext→open with no
  // server involvement; swap it for true multi-party key exchange in production.
  const E2E_PLACEHOLDER = "🔒";
  const facetE2E = window.facetE2E || (window.facetE2E = defaultE2E());

  function defaultE2E() {
    const subtle = window.crypto && window.crypto.subtle;
    const b64 = (buf) => btoa(String.fromCharCode.apply(null, new Uint8Array(buf)));
    const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
    let keyPromise = null;
    function getKey() {
      if (keyPromise) return keyPromise;
      keyPromise = (async () => {
        let raw = localStorage.getItem("fa-e2e-key");
        if (raw) return subtle.importKey("raw", unb64(raw), "AES-GCM", false, ["encrypt", "decrypt"]);
        const k = await subtle.generateKey({ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
        localStorage.setItem("fa-e2e-key", b64(await subtle.exportKey("raw", k)));
        return k;
      })();
      return keyPromise;
    }
    return {
      async seal(plaintext) {
        if (!subtle) { console.warn("facet @e2e: Web Crypto unavailable; value sent unsealed"); return toStr(plaintext); }
        const key = await getKey();
        const iv = window.crypto.getRandomValues(new Uint8Array(12));
        const ct = await subtle.encrypt({ name: "AES-GCM", iv }, key, new TextEncoder().encode(toStr(plaintext)));
        return "fae2e1:" + b64(iv) + "." + b64(ct);
      },
      async open(ciphertext) {
        const s = toStr(ciphertext);
        if (!subtle || s.indexOf("fae2e1:") !== 0) return s; // not sealed / can't open: show as-is
        try {
          const [ivb, ctb] = s.slice(7).split(".");
          const pt = await subtle.decrypt({ name: "AES-GCM", iv: unb64(ivb) }, await getKey(), unb64(ctb));
          return new TextDecoder().decode(pt);
        } catch (_) { return "🔒"; } // wrong key (not the intended recipient): stays sealed
      },
    };
  }

  // openE2E opens every sealed placeholder under scope (default: the whole root):
  // it reads the stashed ciphertext, asks the provider to open it, and swaps the
  // lock glyph for the plaintext. Idempotent — a span already opened for its
  // current ciphertext is skipped, so repeated refreshes don't re-decrypt.
  function openE2E(scope) {
    const host = scope || root;
    for (const span of host.querySelectorAll("[data-fa-e2e]")) {
      const ciph = span.getAttribute("data-fa-e2e");
      if (span.__faOpened === ciph) continue;
      span.__faOpened = ciph;
      facetE2E.open(ciph).then((pt) => { if (span.__faOpened === ciph) span.textContent = pt; });
    }
  }

  // Per-page state, replaced on SPA navigation by load().
  let ir, store, actions, bindings, policies, stateType, routes;
  let regionById, inputById; // tracked dynamic regions and two-way inputs, by id
  let initialStore = {}; // snapshot of state at page load — dirty(cell) compares against it
  let touched = {}; // cell -> true once the user has edited its input — touched(cell)
  const actState = {}; // action name -> { pending, error }: reactive form/action status

  // setPending records an action's in-flight/error status and refreshes the
  // regions that read pending()/failed() for it.
  function setPending(name, pending, error) {
    actState[name] = { pending: pending, error: error || "" };
    refresh(["@act:" + name]);
  }

  function load(newIr, newState) {
    ir = newIr;
    store = newState;
    initialStore = Object.assign({}, newState); // baseline for dirty(); reset per page
    touched = {};
    actions = index(ir.actions, "name");
    bindings = index(ir.bindings, "id");
    policies = index(ir.policies, "name");
    routes = ir.routes || [];
    stateType = {};
    for (const s of ir.states) stateType[s.name] = s.type;
    regionById = {};
    inputById = {};
    (function collect(nodes) {
      for (const n of nodes) {
        if ((n.kind === "list" || n.kind === "if" || n.kind === "use" || n.kind === "tabs" || n.kind === "match" || n.kind === "overlay") && n.id) regionById[n.id] = n;
        if ((n.kind === "input" || n.kind === "select" || n.kind === "upload" || n.kind === "typeahead") && n.id) inputById[n.id] = n;
        if (n.children) collect(n.children);
      }
    })(ir.view);
  }

  const components = {}; // name -> component, stable across the whole app

  // ── shared expression interpreter (mirrors eval.go) ─────────────────────────
  function ev(e, sc) {
    if (!e) return null;
    switch (e.kind) {
      case "lit":
        if (e.vtype === "int") return toInt(e.val);
        if (e.vtype === "bool") return !!e.val;
        return toStr(e.val);
      case "list":
        return (e.args || []).map((a) => ev(a, sc));
      case "ref":
        return sc[e.name];
      case "get": {
        const o = ev(e.obj, sc);
        return o && typeof o === "object" ? o[e.field] : null;
      }
      case "eget": {
        const rows = sc[e.name] || [];
        const key = ev(e.key, sc);
        for (const r of rows) if (r && eq(r.id, key)) return r[e.field];
        return null;
      }
      case "agg": {
        let rows = sc[e.name] || [];
        // Filtered form: keep rows the predicate accepts, item var bound per row.
        if (e.where) {
          const had = Object.prototype.hasOwnProperty.call(sc, e.var);
          const prev = sc[e.var];
          rows = rows.filter((r) => { sc[e.var] = r; return truthy(ev(e.where, sc)); });
          if (had) sc[e.var] = prev; else delete sc[e.var];
        }
        if (e.op === "exists") return rows.length > 0;
        if (e.op === "count") return rows.length;
        // sum/avg/min/max reduce a numeric field over the (filtered) rows.
        let total = 0, n = 0, lo = 0, hi = 0;
        for (const r of rows) {
          const v = toInt(r[e.field]);
          if (n === 0) { lo = v; hi = v; } else { if (v < lo) lo = v; if (v > hi) hi = v; }
          total += v; n++;
        }
        if (e.op === "avg") return n === 0 ? 0 : (total / n) | 0;
        if (e.op === "min") return lo;
        if (e.op === "max") return hi;
        return total; // sum
      }
      case "astate": {
        if (e.op === "dirty") return !eq(store[e.name], initialStore[e.name]);
        if (e.op === "touched") return !!touched[e.name];
        const s = actState[e.name] || {};
        return e.op === "pending" ? !!s.pending : (s.error || "");
      }
      case "call":
        return evCall(e, sc);
      case "un": {
        const x = ev(e.x, sc);
        return e.op === "!" ? !truthy(x) : -toInt(x);
      }
      case "bin": {
        const l = ev(e.l, sc), r = ev(e.r, sc);
        switch (e.op) {
          case "&&": return truthy(l) && truthy(r);
          case "||": return truthy(l) || truthy(r);
          case "+":
            if (typeof l === "string") return l + toStr(r);
            if (typeof r === "string") return toStr(l) + r;
            return toInt(l) + toInt(r);
          case "-": return toInt(l) - toInt(r);
          case "*": return toInt(l) * toInt(r);
          case "/": return toInt(r) === 0 ? 0 : (toInt(l) / toInt(r)) | 0;
          case "%": return toInt(r) === 0 ? 0 : toInt(l) % toInt(r);
          case "==": return eq(l, r);
          case "!=": return !eq(l, r);
          case "<": return toInt(l) < toInt(r);
          case "<=": return toInt(l) <= toInt(r);
          case ">": return toInt(l) > toInt(r);
          case ">=": return toInt(l) >= toInt(r);
          case "in": return Array.isArray(r) && r.some((x) => eq(l, x));
        }
      }
    }
    return null;
  }

  // evCall mirrors runtime/eval.go: now/rand are defensive only (placed on the
  // server), the rest are the pure standard library.
  function evCall(e, sc) {
    const a = (i) => (e.args && i < e.args.length ? ev(e.args[i], sc) : null);
    switch (e.name) {
      case "now": return Math.floor(Date.now() / 1000);
      case "rand": { const n = toInt(a(0)); return n > 0 ? Math.floor(Math.random() * n) : 0; }
      case "abs": { const n = toInt(a(0)); return n < 0 ? -n : n; }
      case "min": { const x = toInt(a(0)), y = toInt(a(1)); return x < y ? x : y; }
      case "max": { const x = toInt(a(0)), y = toInt(a(1)); return x > y ? x : y; }
      case "floor": case "round": return toInt(a(0));
      case "money": return money(toInt(a(0)));
      case "len": { const v = a(0); return Array.isArray(v) ? v.length : Array.from(toStr(v)).length; }
      case "upper": return toStr(a(0)).toUpperCase();
      case "lower": return toStr(a(0)).toLowerCase();
      case "trim": return toStr(a(0)).trim();
      case "contains": return toStr(a(0)).includes(toStr(a(1)));
      case "year": return new Date(toInt(a(0)) * 1000).getUTCFullYear();
      case "month": return new Date(toInt(a(0)) * 1000).getUTCMonth() + 1;
      case "day": return new Date(toInt(a(0)) * 1000).getUTCDate();
    }
    return null;
  }
  function money(cents) {
    let neg = cents < 0; if (neg) cents = -cents;
    const frac = cents % 100;
    const s = ((cents / 100) | 0) + "." + ((frac / 10) | 0) + (frac % 10);
    return neg ? "-" + s : s;
  }
  function truthy(v) { if (Array.isArray(v)) return v.length > 0; return !(v === false || v === 0 || v === "" || v == null); }
  function toInt(v) { if (typeof v === "number") return v | 0; if (v === true) return 1; return 0; }
  function toStr(v) { if (v == null) return ""; if (typeof v === "boolean") return v ? "true" : "false"; return "" + v; }
  function eq(a, b) {
    if (typeof a === "string" || typeof b === "string") return toStr(a) === toStr(b);
    if (typeof a === "boolean" || typeof b === "boolean") return truthy(a) === truthy(b);
    return toInt(a) === toInt(b);
  }

  // ── rendering ───────────────────────────────────────────────────────────────
  function el(tag, cls) { const e = document.createElement(tag); if (cls) e.className = cls; return e; }

  // appendSegs renders interpolated segments (text, button labels) into a parent:
  // literals as text, top-level binds as live-updating spans, in-region exprs inline.
  function appendSegs(parent, segs, sc) {
    for (const seg of segs || []) {
      if (seg.lit != null) parent.appendChild(document.createTextNode(seg.lit));
      else if (seg.e2e) {
        // A sealed (@e2e) value: ev() yields ciphertext. Render the lock placeholder
        // and stash the ciphertext; openE2E() opens it (async) into the plaintext.
        const s = el("span", "fa-e2e");
        const ciph = toStr(ev(seg.bind ? bindings[seg.bind].expr : seg.expr, sc));
        s.setAttribute("data-fa-e2e", ciph);
        if (seg.bind) s.setAttribute("data-fa-bind", seg.bind);
        s.textContent = E2E_PLACEHOLDER;
        parent.appendChild(s);
      } else if (seg.bind) {
        const b = el("span"); b.setAttribute("data-fa-bind", seg.bind);
        b.textContent = toStr(ev(bindings[seg.bind].expr, sc));
        parent.appendChild(b);
      } else if (seg.expr) {
        parent.appendChild(document.createTextNode(toStr(ev(seg.expr, sc))));
      }
    }
  }

  // segsToStr flattens segments to a plain string for an attribute (an image src).
  function segsToStr(segs, sc) {
    let out = "";
    for (const seg of segs || []) {
      if (seg.lit != null) out += seg.lit;
      else if (seg.bind) out += toStr(ev(bindings[seg.bind].expr, sc));
      else if (seg.expr) out += toStr(ev(seg.expr, sc));
    }
    return out;
  }

  // ── Markdown (mirrors runtime/richtext.go exactly) ──────────────────────────
  function mdEscape(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&#34;").replace(/'/g, "&#39;");
  }
  function mdInline(s) {
    s = mdEscape(s);
    s = s.replace(/`([^`]+)`/g, "<code>$1</code>");
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/\*([^*]+)\*/g, "<em>$1</em>");
    return s;
  }
  function mdBlockStart(line) {
    return line.startsWith("```") || line.startsWith("# ") || line.startsWith("## ") ||
      line.startsWith("### ") || line.startsWith("- ");
  }
  function markdownHtml(src) {
    const lines = String(src).split("\n");
    let out = "", i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (line.startsWith("```")) {
        i++;
        const code = [];
        while (i < lines.length && !lines[i].startsWith("```")) { code.push(lines[i]); i++; }
        if (i < lines.length) i++;
        out += "<pre><code>" + mdEscape(code.join("\n")) + "</code></pre>";
      } else if (line.startsWith("### ")) { out += "<h3>" + mdInline(line.slice(4)) + "</h3>"; i++; }
      else if (line.startsWith("## ")) { out += "<h2>" + mdInline(line.slice(3)) + "</h2>"; i++; }
      else if (line.startsWith("# ")) { out += "<h1>" + mdInline(line.slice(2)) + "</h1>"; i++; }
      else if (line.startsWith("- ")) {
        out += "<ul>";
        while (i < lines.length && lines[i].startsWith("- ")) { out += "<li>" + mdInline(lines[i].slice(2)) + "</li>"; i++; }
        out += "</ul>";
      } else if (line.trim() === "") { i++; }
      else {
        const para = [];
        while (i < lines.length && lines[i].trim() !== "" && !mdBlockStart(lines[i])) { para.push(mdInline(lines[i])); i++; }
        out += "<p>" + para.join("<br>") + "</p>";
      }
    }
    return out;
  }

  function render(node, sc) {
    switch (node.kind) {
      case "box": {
        const d = el("div", "fa-box");
        for (const c of node.children || []) d.appendChild(render(c, sc));
        return d;
      }
      case "row": {
        const d = el("div", "fa-row");
        for (const c of node.children || []) d.appendChild(render(c, sc));
        return d;
      }
      case "text": {
        const span = el("span", "fa-text");
        appendSegs(span, node.segs, sc);
        return span;
      }
      case "image": {
        const img = el("img", "fa-image");
        img.src = segsToStr(node.segs, sc);
        img.alt = "";
        return img;
      }
      case "icon": {
        const i = el("span", "fa-icon");
        i.setAttribute("data-fa-icon", node.name || "");
        i.setAttribute("aria-hidden", "true");
        return i;
      }
      case "video": {
        const v = el("video", "fa-video");
        v.controls = true;
        v.src = segsToStr(node.segs, sc);
        return v;
      }
      case "richtext": {
        const d = el("div", "fa-richtext");
        d.innerHTML = markdownHtml(segsToStr(node.segs, sc)); // markdownHtml escapes + emits a fixed safe tag set
        return d;
      }
      case "badge": {
        const s = el("span", "fa-badge");
        appendSegs(s, node.segs, sc);
        return s;
      }
      case "tabs": {
        const d = el("div", "fa-tabs");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillTabs(d, node, sc);
        return d;
      }
      case "button": {
        const b = el("button");
        appendSegs(b, node.segs, sc);
        b.setAttribute("data-fa-action", node.action);
        b.__fa = { action: node.action, args: node.args || [], scope: sc };
        return b;
      }
      case "list": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillList(d, node, sc);
        return d;
      }
      case "if": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillIf(d, node, sc);
        return d;
      }
      case "match": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillMatch(d, node, sc);
        return d;
      }
      case "use": {
        const d = el("div", node.id ? null : "fa-use");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillUse(d, node, sc);
        return d;
      }
      case "input": {
        const i = el("input");
        i.setAttribute("data-fa-input", node.bind);
        if (node.placeholder) i.placeholder = node.placeholder;
        i.value = toStr(sc[node.bind]);
        return i;
      }
      case "select": {
        const s = el("select", "fa-select");
        s.setAttribute("data-fa-input", node.bind);
        const cur = toStr(sc[node.bind]);
        for (const o of node.options || []) {
          const opt = el("option");
          opt.value = o.value; opt.textContent = o.label;
          if (o.value === cur) opt.selected = true;
          s.appendChild(opt);
        }
        return s;
      }
      case "overlay": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillOverlay(d, node, sc);
        return d;
      }
      case "typeahead": {
        const frag = document.createDocumentFragment();
        const i = el("input", "fa-typeahead");
        i.setAttribute("data-fa-input", node.bind);
        if (node.placeholder) i.placeholder = node.placeholder;
        i.value = toStr(sc[node.bind]);
        const listId = "ta-" + node.id;
        i.setAttribute("list", listId);
        const dl = el("datalist"); dl.id = listId;
        for (const v of typeaheadValues(node, sc)) { const o = el("option"); o.value = v; dl.appendChild(o); }
        frag.appendChild(i); frag.appendChild(dl);
        return frag;
      }
      case "form": {
        const f = el("form", "fa-form");
        f.setAttribute("data-fa-form", node.action);
        f.__fa = { action: node.action, args: node.args || [], scope: sc };
        for (const c of node.children || []) f.appendChild(render(c, sc));
        const submit = el("button"); submit.type = "submit"; submit.textContent = node.label;
        f.appendChild(submit);
        return f;
      }
      case "upload": {
        const label = el("label", "fa-upload");
        label.appendChild(document.createTextNode(node.label || "Upload"));
        const i = el("input"); i.type = "file";
        i.setAttribute("data-fa-upload", node.bind);
        label.appendChild(i);
        return label;
      }
      case "link": {
        // Hide a link to a route the actor may not enter (the server also refuses it).
        if (!routeAllowed(node.path)) return document.createComment("guarded");
        const a = el("a", "fa-link");
        a.textContent = node.label;
        a.setAttribute("href", node.path);
        return a;
      }
    }
    return document.createComment("?");
  }

  function fillList(container, node, sc) {
    container.textContent = "";
    for (const row of selectRows(sc[node.coll] || [], node, sc)) {
      const childScope = Object.assign({}, sc); childScope[node.var] = row;
      for (const c of node.children || []) container.appendChild(render(c, childScope));
    }
  }
  function fillIf(container, node, sc) {
    container.textContent = "";
    if (truthy(ev(node.cond, sc))) {
      for (const c of node.children || []) container.appendChild(render(c, sc));
    }
  }
  // An overlay shows a modal layer while its bound cell is truthy: a backdrop (a
  // click on which closes it) wrapping a centered panel of the children.
  function fillOverlay(container, node, sc) {
    container.textContent = "";
    if (!truthy(sc[node.bind])) return;
    const backdrop = el("div", "fa-overlay-backdrop");
    backdrop.setAttribute("data-fa-close", node.bind);
    const panel = el("div", "fa-overlay-panel");
    for (const c of node.children || []) panel.appendChild(render(c, sc));
    backdrop.appendChild(panel);
    container.appendChild(backdrop);
  }
  // typeaheadValues are the unique, non-empty values of the source field across the
  // bound collection — the native completion list the input offers.
  function typeaheadValues(node, sc) {
    const rows = sc[node.coll] || store[node.coll] || [];
    const seen = new Set(); const out = [];
    for (const r of rows) { const v = toStr(r[node.value]); if (v && !seen.has(v)) { seen.add(v); out.push(v); } }
    out.sort();
    return out;
  }
  function fillMatch(container, node, sc) {
    container.textContent = "";
    const val = toStr(ev(node.cond, sc));
    let arm = (node.children || []).find((c) => c.kind === "case" && c.value === val);
    if (!arm) arm = (node.children || []).find((c) => c.kind === "else");
    if (arm) for (const c of arm.children || []) container.appendChild(render(c, sc));
  }
  function activeTabValue(node, sc) {
    const tabs = node.children || [];
    const cur = toStr(sc[node.bind]);
    if (tabs.some((t) => t.value === cur)) return cur;
    return tabs.length ? tabs[0].value : cur;
  }
  function fillTabs(container, node, sc) {
    container.textContent = "";
    const tabs = node.children || [];
    const active = activeTabValue(node, sc);
    const strip = el("div", "fa-tabstrip");
    strip.setAttribute("role", "tablist");
    for (const t of tabs) {
      const b = el("button", "fa-tab");
      b.setAttribute("role", "tab");
      b.textContent = t.label;
      b.setAttribute("data-fa-tab", t.value);
      b.setAttribute("data-fa-tab-bind", node.bind);
      if (t.value === active) b.setAttribute("aria-selected", "true");
      strip.appendChild(b);
    }
    container.appendChild(strip);
    const cur = tabs.find((t) => t.value === active);
    if (cur) for (const c of cur.children || []) container.appendChild(render(c, sc));
  }
  function fillUse(container, node, sc) {
    container.textContent = "";
    const comp = components[node.name];
    if (!comp) return;
    const childScope = Object.assign({}, sc);
    comp.params.forEach((p, i) => (childScope[p.name] = coerce(ev((node.args || [])[i], sc), p.type)));
    for (const c of comp.view) container.appendChild(render(c, childScope));
  }

  // selectRows mirrors runtime/server.go: filter by `where`, order by `by`, cap by
  // `limit` — so the client re-renders a list exactly as the server first painted it.
  function selectRows(rows, node, sc) {
    let out = rows;
    if (node.where) {
      out = [];
      for (const r of rows) {
        const child = Object.assign({}, sc); child[node.var] = r;
        if (truthy(ev(node.where, child))) out.push(r);
      }
    }
    if (node.order) {
      out = out.slice().sort((a, b) => {
        const c = cmpVal(a[node.order], b[node.order]);
        return node.desc ? -c : c;
      });
    }
    if (node.limit) { const lim = toInt(ev(node.limit, sc)); if (lim > 0 && out.length > lim) out = out.slice(0, lim); }
    return out;
  }
  function cmpVal(a, b) {
    if (typeof a === "number" && typeof b === "number") return a - b;
    const as = toStr(a), bs = toStr(b);
    return as < bs ? -1 : as > bs ? 1 : 0;
  }

  function coerce(v, type) {
    if (type === "int" || type === "money" || type === "date") return toInt(v);
    if (type === "bool") return truthy(v);
    if (type === "text") return toStr(v);
    return v; // enum (already text) or entity-typed (a record) — pass through
  }

  // routeAllowed evaluates a route's guard policy (if any) against current state,
  // matching the path to a route pattern. Open routes are always allowed.
  function routeAllowed(path) {
    for (const rt of routes) {
      if (!matchRoute(rt.path, path)) continue;
      if (!rt.requires) return true;
      const pol = policies[rt.requires];
      return pol ? truthy(ev(pol.expr, store)) : false;
    }
    return true; // not an app route (e.g. an external or asset link)
  }
  function isAppRoute(path) {
    for (const rt of routes) if (matchRoute(rt.path, path)) return true;
    return false;
  }
  function matchRoute(pattern, path) {
    const ps = trimSlash(pattern).split("/");
    const cs = trimSlash(path).split("/");
    if (ps.length !== cs.length) return false;
    for (let i = 0; i < ps.length; i++) {
      if (ps[i][0] === ":") { if (cs[i] === "") return false; continue; }
      if (ps[i] !== cs[i]) return false;
    }
    return true;
  }
  function trimSlash(s) { return s.replace(/^\/+|\/+$/g, ""); }

  // ── fine-grained refresh: only regions a changed state feeds ────────────────
  function refresh(changed) {
    const ids = new Set();
    for (const name of changed) for (const id of (ir.depGraph[name] || [])) ids.add(id);
    for (const id of ids) {
      if (bindings[id]) {
        for (const e of root.querySelectorAll('[data-fa-bind="' + id + '"]')) {
          const v = toStr(ev(bindings[id].expr, store));
          if (e.hasAttribute("data-fa-e2e")) {
            // A sealed bind got a new ciphertext: restash it and let openE2E reopen.
            e.setAttribute("data-fa-e2e", v);
            e.textContent = E2E_PLACEHOLDER;
          } else {
            e.textContent = v;
          }
        }
      } else if (regionById[id]) {
        const node = regionById[id];
        const c = root.querySelector('[data-fa-region="' + id + '"]');
        if (c) (node.kind === "list" ? fillList : node.kind === "use" ? fillUse : node.kind === "tabs" ? fillTabs : node.kind === "match" ? fillMatch : node.kind === "overlay" ? fillOverlay : fillIf)(c, node, store);
      } else if (inputById[id]) {
        const node = inputById[id];
        const e = root.querySelector('[data-fa-input="' + node.bind + '"]');
        if (e && e !== document.activeElement) e.value = toStr(store[node.bind]);
      }
    }
    openE2E(); // open any sealed placeholders the refresh (re)rendered
  }

  // ── dispatch: the compiler already chose where each action runs ──────────────
  async function dispatch(action, args, sc, source) {
    clearError(source);
    const act = actions[action];
    if (!act) return;
    const vals = (args || []).map((a) => ev(a, sc));
    // @e2e: seal the flagged arguments on this client before anything leaves it, so
    // the authority only ever receives ciphertext. The compiler proved each sealed
    // param flows solely into its @e2e field, so sealing here is sound.
    if (act.seal && act.seal.length) {
      for (let i = 0; i < act.params.length; i++) {
        if (act.seal.indexOf(act.params[i].name) >= 0) vals[i] = await facetE2E.seal(vals[i]);
      }
    }
    if (act.placement === "client") {
      runClient(act, vals);
      return;
    }
    // Optimistic update: predict the effect locally for instant feedback, then let
    // the authority's response (and SSE) reconcile to the true result. If the
    // action is rejected, the prediction is rolled back to the pre-dispatch state.
    let snapshot = null, predicted = null;
    if (act.optimistic) { snapshot = Object.assign({}, store); predicted = predict(act, vals); }
    setPending(action, true, "");          // reactive: pending(action) is now true
    if (source) { source.setAttribute("aria-busy", "true"); source.disabled = true; } // no double-submit
    let res;
    try {
      res = await fetch("/event", {
        method: "POST", headers: { "Content-Type": "application/json", "X-Facet-CSRF": csrf },
        body: JSON.stringify({ action, args: vals }),
      });
    } finally {
      if (source) { source.removeAttribute("aria-busy"); source.disabled = false; }
    }
    if (!res.ok) {
      if (snapshot) { for (const k of predicted) store[k] = snapshot[k]; refresh(predicted); }
      const msg = (await res.text()).trim();
      showError(source, msg || "Something went wrong.");
      setPending(action, false, msg || "Something went wrong."); // failed(action) now set
      return;
    }
    setPending(action, false, "");         // success: clear pending + error
    const data = await res.json();
    if (data.reload) { location.reload(); return; } // identity changed (login/logout)
    applyDeltas(data.deltas);
  }

  function runClient(act, vals) {
    const scope = Object.assign({}, store);
    act.params.forEach((p, i) => (scope[p.name] = vals[i]));
    const changed = [];
    for (const st of act.body) if (st.op === "assign") {
      const v = ev(st.value, scope);
      if (store[st.target] !== v) { store[st.target] = v; scope[st.target] = v; changed.push(st.target); }
    }
    refresh(changed);
  }

  // predict applies a server action's body to local state for an optimistic paint.
  // Entity adds get a temporary negative id; the authoritative SSE snapshot then
  // replaces the whole collection, swapping the prediction for the real row.
  function predict(act, vals) {
    const scope = Object.assign({}, store);
    act.params.forEach((p, i) => (scope[p.name] = vals[i]));
    const changed = [];
    let tempId = -Date.now();
    for (const st of act.body) {
      if (st.op === "assign") {
        const v = ev(st.value, scope);
        store[st.target] = v; scope[st.target] = v; changed.push(st.target);
      } else if (st.op === "add") {
        const row = { id: tempId-- };
        for (const fi of st.fields) row[fi.name] = ev(fi.expr, scope);
        store[st.entity] = (store[st.entity] || []).concat([row]);
        scope[st.entity] = store[st.entity]; changed.push(st.entity);
      } else if (st.op === "set") {
        const key = ev(st.key, scope);
        store[st.entity] = (store[st.entity] || []).map((r) =>
          eq(r.id, key) ? Object.assign({}, r, { [st.field]: ev(st.value, scope) }) : r);
        scope[st.entity] = store[st.entity]; changed.push(st.entity);
      } else if (st.op === "remove") {
        if (st.where) {
          // Filtered delete: drop every row the predicate accepts, item var bound.
          const had = Object.prototype.hasOwnProperty.call(scope, st.var);
          const prev = scope[st.var];
          store[st.entity] = (store[st.entity] || []).filter((r) => {
            scope[st.var] = r; return !truthy(ev(st.where, scope));
          });
          if (had) scope[st.var] = prev; else delete scope[st.var];
        } else {
          const key = ev(st.key, scope);
          store[st.entity] = (store[st.entity] || []).filter((r) => !eq(r.id, key));
        }
        scope[st.entity] = store[st.entity]; changed.push(st.entity);
      } else if (st.op === "clear") {
        store[st.entity] = []; scope[st.entity] = store[st.entity]; changed.push(st.entity);
      }
    }
    refresh(changed);
    return changed;
  }

  function applyDeltas(deltas) {
    const changed = [];
    for (const k in (deltas || {})) { store[k] = deltas[k]; changed.push(k); }
    refresh(changed);
  }

  // ── inline validation / error surface ────────────────────────────────────────
  function showError(source, msg) {
    const host = source && (source.closest("form") || source.parentNode);
    if (!host) { console.warn("facet:", msg); return; }
    let e = host.querySelector(":scope > .fa-error");
    if (!e) { e = el("div", "fa-error"); e.setAttribute("role", "alert"); host.appendChild(e); }
    e.textContent = msg;
  }
  function clearError(source) {
    const host = source && (source.closest("form") || source.parentNode);
    if (!host) return;
    const e = host.querySelector(":scope > .fa-error");
    if (e) e.remove();
  }

  // ── file upload ──────────────────────────────────────────────────────────────
  // A small file goes in one multipart POST; a large one is sent in resumable
  // chunks (init → chunk* → finish), so a transfer bigger than one request limit
  // still completes. Either way the server returns the URL we store and bind.
  const CHUNK_BYTES = 4 * 1024 * 1024; // 4 MiB — switch to chunked above this size

  async function upload(input) {
    const bind = input.getAttribute("data-fa-upload");
    const file = input.files && input.files[0];
    if (!file) return;
    const label = input.closest("label");
    label && label.setAttribute("aria-busy", "true");
    try {
      const url = file.size > CHUNK_BYTES ? await uploadChunked(file) : await uploadSingle(file);
      if (!url) { showError(input, "Upload failed."); return; }
      store[bind] = url;
      refresh([bind]);
    } finally {
      label && label.removeAttribute("aria-busy");
    }
  }

  async function uploadSingle(file) {
    const body = new FormData(); body.append("file", file);
    const res = await fetch("/upload", { method: "POST", headers: { "X-Facet-CSRF": csrf }, body });
    if (!res.ok) return null;
    return (await res.json()).url;
  }

  async function uploadChunked(file) {
    const csrfHdr = { "X-Facet-CSRF": csrf };
    let res = await fetch("/upload/init", {
      method: "POST", headers: { ...csrfHdr, "Content-Type": "application/json" },
      body: JSON.stringify({ filename: file.name }),
    });
    if (!res.ok) return null;
    const id = (await res.json()).id;
    for (let off = 0; off < file.size; off += CHUNK_BYTES) {
      res = await fetch("/upload/chunk?id=" + encodeURIComponent(id), {
        method: "POST", headers: csrfHdr, body: file.slice(off, off + CHUNK_BYTES),
      });
      if (!res.ok) { fetch("/upload/abort?id=" + encodeURIComponent(id), { method: "POST", headers: csrfHdr }); return null; }
    }
    res = await fetch("/upload/finish?id=" + encodeURIComponent(id), { method: "POST", headers: csrfHdr });
    if (!res.ok) return null;
    return (await res.json()).url;
  }

  // ── SPA navigation: follow internal links without a full reload ───────────────
  async function navigate(path, push) {
    let res;
    try { res = await fetch(path, { headers: { "X-Facet-Nav": "1" } }); } catch (_) { location.href = path; return; }
    if (!res.ok) { location.href = path; return; }
    const html = await res.text();
    const doc = new DOMParser().parseFromString(html, "text/html");
    const irEl = doc.getElementById("fa-ir"), stateEl = doc.getElementById("fa-state");
    if (!irEl || !stateEl) { location.href = path; return; } // a guarded/error page: hard-load it
    const meta = doc.querySelector('meta[name="fa-csrf"]');
    if (meta) csrf = meta.getAttribute("content");
    if (doc.title) document.title = doc.title;
    // keep the description in sync so per-route metadata survives SPA navigation
    const newDesc = doc.querySelector('meta[name="description"]');
    if (newDesc) {
      let cur = document.querySelector('meta[name="description"]');
      if (!cur) { cur = document.createElement("meta"); cur.setAttribute("name", "description"); document.head.appendChild(cur); }
      cur.setAttribute("content", newDesc.getAttribute("content") || "");
    }
    load(JSON.parse(irEl.textContent), JSON.parse(stateEl.textContent));
    mount();
    if (push) history.pushState({ fa: true }, "", path);
    window.scrollTo(0, 0);
  }

  function mount() {
    root.textContent = "";
    for (const n of ir.view) root.appendChild(render(n, store));
    openE2E(); // open the sealed values the first render placed
  }

  // ── wire it up (once) ─────────────────────────────────────────────────────────
  function boot() {
    for (const c of (ir.components || [])) components[c.name] = c;
    mount();
  }

  load(JSON.parse(document.getElementById("fa-ir").textContent),
       JSON.parse(document.getElementById("fa-state").textContent));
  boot();

  root.addEventListener("click", function (e) {
    const link = e.target.closest("a.fa-link");
    if (link) {
      const href = link.getAttribute("href");
      if (href && href[0] === "/" && isAppRoute(href) && !e.metaKey && !e.ctrlKey) {
        e.preventDefault(); navigate(href, true);
      }
      return;
    }
    const tab = e.target.closest("[data-fa-tab]");
    if (tab) {
      e.preventDefault();
      const bind = tab.getAttribute("data-fa-tab-bind");
      store[bind] = tab.getAttribute("data-fa-tab");
      refresh([bind]);
      return;
    }
    // A click on the overlay backdrop itself (not its panel) closes the overlay.
    const closer = e.target.closest("[data-fa-close]");
    if (closer && e.target === closer) {
      e.preventDefault();
      const bind = closer.getAttribute("data-fa-close");
      store[bind] = false;
      refresh([bind]);
      return;
    }
    const t = e.target.closest("[data-fa-action]");
    if (t && t.__fa) { e.preventDefault(); dispatch(t.__fa.action, t.__fa.args, t.__fa.scope, t); }
  });
  root.addEventListener("submit", function (e) {
    const f = e.target.closest("[data-fa-form]");
    if (f && f.__fa) { e.preventDefault(); dispatch(f.__fa.action, f.__fa.args, f.__fa.scope, f); }
  });
  root.addEventListener("input", function (e) {
    const t = e.target.closest("[data-fa-input]");
    if (!t) return;
    const name = t.getAttribute("data-fa-input");
    store[name] = (stateType[name] === "int" || stateType[name] === "money" || stateType[name] === "date") ? toInt(t.value) : t.value;
    touched[name] = true; // touched(name) is now true; dirty(name) tracks the value
    refresh([name]);
  });
  root.addEventListener("change", function (e) {
    const t = e.target.closest("[data-fa-upload]");
    if (t) upload(t);
  });
  window.addEventListener("popstate", function () { navigate(location.pathname, false); });

  // ── live shared state: subscribe to the authority's change stream ────────────
  // The server fans out durable/entity changes over SSE so every client converges
  // with no refresh — and so the acting client sees its own entity writes, which
  // is why /event returns only per-session deltas. The opening snapshot makes a
  // late-joining client whole. We apply deltas the same way as an action result.
  if (window.EventSource) {
    const live = new EventSource("/live");
    live.onmessage = function (e) {
      let msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      applyDeltas(msg.deltas || {});
    };
  }

  function index(arr, key) { const m = Object.create(null); for (const x of arr || []) m[x[key]] = x; return m; }
})();

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
  // Every node kind that is two-way bound to a cell. One list, because three
  // separate places used to ask this question and a new control that was added to
  // two of them worked until the cell changed underneath it.
  const CONTROL_KINDS = { input: 1, select: 1, upload: 1, typeahead: 1, textarea: 1, checkbox: 1, radio: 1 };

  // ── region rows: the result set, not the table ─────────────────────────────
  // A `for` no longer reads a collection out of `store` and filters it here. The
  // authority resolves it — through the database, with its indexes — and sends
  // back exactly the rows it rendered, addressed by the region's render path
  // (see regionPath). This client renders those rows and does not re-filter or
  // re-sort them: a second filter here would be a second definition of the same
  // query, free to drift from the one that painted the page.
  //
  // The cost is that a change to a state cell a `where` reads is no longer free.
  // regionFP records the dependency fingerprint each region's rows were resolved
  // under; when it stops matching, the region re-asks the authority.
  let regionRows = {}; // region path -> the rows the authority resolved
  let regionFP = {};   // region path -> the fingerprint those rows were resolved under
  let regionCtx = {};  // region path -> { el, node, sc }: where to re-render it in place
  let regionPending = {}; // region path -> the fingerprint currently in flight
  let entityVersion = {}; // entity -> bump count; an announced change stales every region reading it

  // ── materialized aggregates ────────────────────────────────────────────────
  // `count(...)`, `exists(...)` and `Entity(id).field` resolve by scanning a
  // collection, and collections no longer cross to the browser — so the value the
  // server computed crosses instead, addressed by the render path it was computed
  // at plus the aggregate's position in this page's IR. `load` stamps that
  // position onto the parsed expression objects themselves (__faAgg), so `ev`
  // looks it up in O(1) with nothing to keep in sync but the walk order, which is
  // the same fixed order runtime/region.go walks.
  //
  // A miss falls back to scanning whatever is in scope, which is what a route
  // policy and a client-placed action — neither of which the server renders —
  // still do; those keep their collection (see clientColls).
  let aggValues = {}; // "<render path>|<index>" -> the value the authority computed
  let curPath = "";   // the render path currently being evaluated
  let unaddressed = 0; // depth of evaluations with no render address (evRow)
  let dataSeq = 0;    // the authority's write count this page's data was built at

  // ── media: the client renders signed URLs, and signs nothing ────────────────
  //
  // A row stores a durable reference ("/uploads/<name>"); the signature that
  // makes it fetchable is minted per render, and only the server can mint one —
  // it is an HMAC under the master signing key, which no browser may hold. So the
  // signatures arrive as DATA, alongside the state they belong to: the bootstrap
  // (@media), every /region answer, every SSE frame, and the upload response.
  //
  // mediaSrc is therefore a lookup, not a computation. That is the whole point:
  // runtime/media.go's mediaSrc is the one implementation of "what URL does this
  // media value render as", and mirroring it here would mean either shipping the
  // key or growing a second, wrong answer. A value with no grant — an external
  // "https://..." cover, a static path, or a reference this payload did not
  // carry — is used exactly as the row holds it, which is what the client did
  // before signing existed.
  let mediaMap = {}; // stored media value -> the URL to render it by, right now
  function mediaSrc(v) { return (v && mediaMap[v]) || v; }
  function mergeMedia(m) { for (const k in (m || {})) mediaMap[k] = m[k]; }
  let initialStore = {}; // snapshot of state at page load — dirty(cell) compares against it
  let touched = {}; // cell -> true once the user has edited its input — touched(cell)
  const actState = {}; // action name -> { pending, error }: reactive form/action status

  // setPending records an action's in-flight/error status and refreshes the
  // regions that read pending()/failed() for it.
  function setPending(name, pending, error) {
    actState[name] = { pending: pending, error: error || "" };
    refresh(["@act:" + name]);
  }

  // Every list that arrives in the IR is optional.
  //
  // The IR is produced by Go's encoding/json, which marshals a nil slice as
  // `null`, not `[]`. So a component with no parameters, an action with no
  // arguments, an app with no states — every empty list — reaches this file as
  // `null`, and `for (const x of null)` throws.
  //
  // That throw is not a degraded render, it is a blank page: `mount` clears the
  // root before it rebuilds, so an exception anywhere in the tree leaves nothing
  // behind. The symptom is the server-rendered page flashing and vanishing on
  // every load. `ComposeBox` and `SearchBox` — two components with no params on
  // the F33D3R home screen — did exactly that.
  //
  // Most of this file already guarded inline (`node.children || []`), which is
  // why the bug hid: the convention existed but was applied by hand, so the two
  // sites that forgot it were indistinguishable from the ones that didn't. This
  // names the convention instead, so "is this list optional?" has one answer —
  // yes, always — rather than a judgement call at each use.
  function list(xs) { return xs || []; }

  function load(newIr, newState) {
    ir = newIr;
    store = newState;
    // The bootstrap carries this page's region result sets alongside its state
    // cells. Lift them out so `store` stays what it has always been — state, and
    // the few collections the client's own aggregates still need.
    regionRows = newState["@regions"] || {};
    aggValues = newState["@aggs"] || {};
    dataSeq = newState["@seq"] || 0;
    // Grants are per page: a fresh page brings its own, and carrying the previous
    // page's would only keep entries nothing renders. Lifted out with the rest of
    // the render's machinery so `store` stays state, and so an expiring URL never
    // becomes a state cell dirty() would compare against.
    mediaMap = {};
    mergeMedia(newState["@media"]);
    delete newState["@regions"];
    delete newState["@aggs"];
    delete newState["@seq"];
    delete newState["@media"];
    regionFP = {}; regionCtx = {}; regionPending = {}; entityVersion = {};
    numberAggregates(newIr);
    // Restore a saved palette the server's cookie didn't carry (e.g. cookies off):
    // the localStorage mirror is the fallback source of truth for the `theme` state.
    if ("theme" in store && !store.theme) {
      try { const ls = localStorage.getItem("fa-theme"); if (ls) store.theme = ls; } catch (e) {}
    }
    lastTheme = undefined; // re-evaluate data-theme against this page's store
    initialStore = Object.assign({}, newState); // baseline for dirty(); reset per page
    touched = {};
    actions = index(ir.actions, "name");
    bindings = index(ir.bindings, "id");
    policies = index(ir.policies, "name");
    routes = ir.routes || [];
    stateType = {};
    for (const s of list(ir.states)) stateType[s.name] = s.type;
    regionById = {};
    inputById = {};
    function collect(nodes) {
      for (const n of list(nodes)) {
        if ((n.kind === "list" || n.kind === "if" || n.kind === "use" || n.kind === "tabs" || n.kind === "match" || n.kind === "overlay") && n.id) regionById[n.id] = n;
        // A control whose choices come from data is a region too, under the id it
        // already has: the collection behind it changes, so its options must.
        if (optionsRegionId(n)) regionById[n.id] = n;
        if (CONTROL_KINDS[n.kind] && n.id) inputById[n.id] = n;
        if (n.children) collect(n.children);
      }
    }
    collect(list(ir.view));
    // A component's body renders on this page, so the regions and two-way inputs
    // inside it are addresses on this page and must be reachable by id — the page
    // tree alone does not contain them. Component ids are namespaced per component
    // by the compiler, so they cannot collide with the page's own.
    for (const c of list(ir.components)) collect(list(c.view));
    syncTheme(); // apply (and persist) the active palette for this page
  }

  // segLists is every interpolated value hanging off a node: its own segments and
  // each attribute's. It is ir.Node.SegLists, in ir.Node.SegLists' order, and it
  // has to be — the two sides address aggregates by their *position* in this walk,
  // so a list one side reads and the other skips shifts every number after it, and
  // every aggregate from there on resolves to another one's value.
  function segLists(nd) {
    const out = [nd.segs, nd.label, nd.placeholder, nd.pathSegs, nd.classSegs, nd.alt];
    for (const o of list(nd.options)) out.push(o.label);
    return out;
  }

  // numberAggregates walks the IR this page shipped in ONE fixed order — the view
  // tree pre-order (and within a node: every interpolated value it carries, then
  // cond, where, limit, val, a `use`'s args, then children), then the page's
  // bindings, then every component's view — stamping each aggregate/lookup
  // expression with its position. runtime/region.go walks the identical structure
  // in the identical order, which is the whole of the agreement between the two
  // sides.
  function numberAggregates(g) {
    let n = 0;
    function expr(e) {
      if (!e) return;
      if (e.kind === "agg" || e.kind === "eget") e.__faAgg = n++;
      expr(e.l); expr(e.r); expr(e.x); expr(e.obj); expr(e.key); expr(e.where);
      for (const a of list(e.args)) expr(a);
    }
    function nodes(ns) {
      for (const nd of list(ns)) {
        for (const segs of segLists(nd)) for (const sg of list(segs)) expr(sg.expr);
        expr(nd.cond); expr(nd.where); expr(nd.limit); expr(nd.val);
        if (nd.kind === "use") for (const a of list(nd.args)) expr(a);
        nodes(nd.children);
      }
    }
    nodes(g.view);
    for (const b of list(g.bindings)) expr(b.expr);
    for (const c of list(g.components)) nodes(c.view);
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
        const mat = materialized(e);
        if (mat !== undefined) return mat;
        const rows = collRows(e, sc);
        const key = ev(e.key, sc);
        for (const r of rows) if (r && eq(r.id, key)) return r[e.field];
        return null;
      }
      case "agg": {
        const mat = materialized(e);
        if (mat !== undefined) return mat;
        let rows = collRows(e, sc);
        // Filtered form: keep rows the predicate accepts, item var bound per row.
        if (e.where) {
          const had = Object.prototype.hasOwnProperty.call(sc, e.var);
          const prev = sc[e.var];
          rows = rows.filter((r) => { sc[e.var] = r; return truthy(evRow(e.where, sc)); });
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

  // collRows is the collection an aggregate or lookup scans, and the one place
  // that says so when there is nothing to scan.
  //
  // A collection reaches the client only because runtime/region.go's clientColls
  // proved this evaluator reads it (the render answers everything else by
  // shipping the value). So an expression that arrives here with no rows and no
  // materialized answer is a hole in that analysis, not an empty table — and an
  // empty table is exactly what `[]` would silently make it look like: `exists`
  // false, `count` 0, forever. It is said out loud instead. The page keeps
  // rendering, the same stance the authority takes when a store read fails.
  function collRows(e, sc) {
    const rows = sc[e.name];
    if (rows === undefined) {
      console.error("facet: " + (e.kind === "agg" ? e.op + "(...)" : "lookup") +
        " over " + e.name + " has no rows on the client and no value from the render — " +
        "clientColls (runtime/region.go) did not ship it. Treating it as empty, which is probably wrong.");
      return [];
    }
    return rows;
  }

  // materialized returns the value the authority computed for this aggregate at
  // the render path currently being evaluated, or undefined if it did not compute
  // one (an expression the server never renders, or one evRow is evaluating).
  function materialized(e) {
    if (unaddressed > 0) return undefined;
    return e.__faAgg === undefined ? undefined : aggValues[curPath + "|" + e.__faAgg];
  }

  // evRow evaluates a predicate for one row of a set the surrounding render
  // addresses as a whole — a `for`'s where over a `[T]` cell, an aggregate's own
  // filter. It is (*materializer).perRow in runtime/region.go, for the same
  // reason: the address of an aggregate is (render path, position), and here the
  // path stands still while the bindings change, so that pair names a position
  // rather than an evaluation. Row one's `exists(...)` would otherwise answer for
  // every row after it. runtime/control_test.go pins the two halves together.
  function evRow(e, sc) {
    unaddressed++;
    try { return ev(e, sc); } finally { unaddressed--; }
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
  // Mirrors eval.go's toInt exactly, including the accepted spelling of a number
  // written as text — optional sign, digits with an optional fractional part,
  // optional exponent, truncated toward zero.
  //
  // The regex is the point. `Number("0x10")` is 16 and `Number("")` is 0 and
  // `Number("Infinity")` is finite-looking; Go's ParseFloat accepts "inf" and
  // "nan". Leaning on either built-in would let the server and the client read
  // the same input as two different numbers, so a row would render one way on
  // first paint and another the instant the client took over. Both halves accept
  // exactly this shape and nothing else.
  //
  // Every `<input>`'s .value is a string, so before this every `int`-typed input
  // in every Facet app stored 0 no matter what was typed into it.
  const FA_NUMERIC = /^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/;
  function toInt(v) {
    if (typeof v === "number") return Math.trunc(v);
    if (v === true) return 1;
    if (typeof v === "string") {
      const t = v.trim();
      if (!FA_NUMERIC.test(t)) return 0;
      return Math.trunc(Number(t));
    }
    return 0;
  }
  // headingLevel turns a heading's evaluated level into the element it renders
  // as. It is runtime/server.go's headingLevel, character for character: a
  // heading whose level clamped differently on the two sides would change the
  // shape of the document at hydration. The compiler refuses every level it can
  // prove wrong (internal/ir/heading.go); what reaches here arrived as a value,
  // and this path is total, so the choice is between an element that does not
  // exist and the nearest one that does.
  function headingLevel(v) { const n = toInt(v); return n < 1 ? 1 : n > 6 ? 6 : n; }
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
        // A binding is re-evaluated later by refresh(), which has no tree to walk,
        // so the span remembers the render path its aggregates were addressed from.
        b.__faPath = curPath;
        b.textContent = toStr(ev(bindings[seg.bind].expr, sc));
        parent.appendChild(b);
      } else if (seg.expr) {
        parent.appendChild(document.createTextNode(toStr(ev(seg.expr, sc))));
      }
    }
  }

  // segsToStr flattens segments to a plain string for an attribute (an image src).
  // linkHref mirrors runtime/link.go exactly, including the escaping.
  //
  // An interpolated value is data going into a URL path, where `/`, `?`, `#` and
  // `%` all mean something: an unescaped handle of `a/b` becomes two path
  // segments and routes elsewhere. Each interpolated run is escaped and the
  // author's literal separators are not, which is what keeps `/profile/{handle}`
  // one route with one parameter.
  //
  // The escaping is written out rather than delegated to encodeURIComponent,
  // because neither language's built-in matches the other: Go's PathEscape
  // leaves `$&+:=@` alone in a path segment, encodeURIComponent escapes those
  // and leaves `!*'()`. A handle containing `=` would render one href on first
  // paint and a different one after hydration — a link that changes under the
  // cursor. Both sides escape everything outside RFC 3986's unreserved set
  // instead, and a test runs the two over the same inputs.
  function escapePathSegment(s) {
    let out = "";
    for (const byte of new TextEncoder().encode(s)) {
      const c = String.fromCharCode(byte);
      if (/[A-Za-z0-9\-._~]/.test(c)) out += c;
      else out += "%" + byte.toString(16).toUpperCase().padStart(2, "0");
    }
    return out;
  }

  function linkHref(node, sc) {
    if (!node.pathSegs) return node.path;
    // A route expression is the whole path supplied as a value, not data landing
    // in a segment of one, so its separators are its own — escaping them would
    // destroy it. Mirrors the `n.Route` arm of runtime/server.go.
    if (node.route) return segsToStr(node.pathSegs, sc);

    let out = "";
    for (const seg of node.pathSegs) {
      // Dynamic when it carries an expression or a top-level binding; mirrors
      // runtime/link.go.
      if (seg.expr || seg.bind) out += escapePathSegment(segsToStr([seg], sc));
      else out += seg.lit || "";
    }
    return out;
  }

  // classText mirrors runtime/classtext.go exactly, including the filtering.
  function classText(segs, sc) {
    let out = "";
    for (const seg of segs || []) {
      if (seg.expr || seg.bind) out += escapeClassToken(segsToStr([seg], sc));
      else out += seg.lit || "";
    }
    return out;
  }
  function escapeClassToken(s) {
    let out = "";
    for (const c of s) if (/[A-Za-z0-9\-_]/.test(c)) out += c;
    return out;
  }

  function segsToStr(segs, sc) {
    let out = "";
    for (const seg of segs || []) {
      if (seg.lit != null) out += seg.lit;
      else if (seg.bind) out += toStr(ev(bindings[seg.bind].expr, sc));
      else if (seg.expr) out += toStr(ev(seg.expr, sc));
    }
    return out;
  }

  // attrText is the client's half of the rule runtime/server.go's Server.attrText
  // states: every interpolated value that is NOT body text — an attribute
  // (placeholder, src, data-fa-icon) or a control's plain text (a submit button,
  // an option, a tab, a link) — is the flattened segments and nothing else.
  //
  // There is no escaper here on purpose. The server escapes because it is
  // building markup; this side hands the string to the DOM through a property
  // (`i.placeholder = …`, `el.textContent = …`, `setAttribute`), which stores the
  // characters rather than parsing them. So `" onmouseover="alert(1)` is a
  // placeholder containing a quote on both sides, not an event handler on either.
  // The invariant the two must keep is that this returns exactly what the
  // server's escaped output decodes to — runtime/attrtext_test.go runs both over
  // the same hostile values. Never build markup out of this: an innerHTML here
  // would be the divergence, since the server's escaping would no longer describe
  // what the client does.
  function attrText(segs, sc) { return segsToStr(segs, sc); }

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
      line.startsWith("### ") || line.startsWith("> ") || line.startsWith("- ");
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
      else if (line.startsWith("> ")) {
        // Mirrors runtime/richtext.go: a run of quoted lines becomes one
        // <blockquote> holding a paragraph, joined the way a paragraph joins.
        const quote = [];
        while (i < lines.length && lines[i].startsWith("> ")) { quote.push(mdInline(lines[i].slice(2))); i++; }
        out += "<blockquote><p>" + quote.join("<br>") + "</p></blockquote>";
      }
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

  // render builds the DOM element for a node, then stamps the CSS escape-hatch
  // class/style onto it (mirroring the server's nodeAttrs) so a region re-rendered
  // on the client matches first paint. Fragments (typeahead) carry no attributes.
  // Render paths address what was rendered, not what exists — the same rule the
  // server follows (runtime/region.go), computed here from the same IR so the
  // two sides name a region identically with nothing shared between them.
  //
  //   /2/0/1      the second root node's first child's second child
  //   /2/0#17/1   ...inside the row with id 17 of an enclosing `for`
  //
  // A region id (`l0`) exists only for a region at the top level of a page; a
  // list inside a `tabs`, a component or another `for` has none, and the path is
  // what lets the client ask for its rows anyway.
  function childPath(path, i) { return path + "/" + i; }
  function rowPath(path, row) { return path + "#" + (row && row.id != null ? toStr(row.id) : ""); }

  // render is the single recursion point of the tree — render0 is called from
  // nowhere else, and renderKids and mount both come back through here — so it is
  // also the one place the current render path is set, mirroring the single
  // `rd.mat.path = path` at the top of runtime/server.go's renderer.node. Every
  // node addresses the aggregates it evaluates from its own path, not from the
  // path of whichever region happens to enclose it: a `count(...)` interpolated
  // into a `text` under a list row was computed by the server at that text node's
  // address, and looking it up at the region's address misses and silently
  // renders 0.
  function render(node, sc, path) {
    curPath = path;
    const e = render0(node, sc, path);
    if (e && e.nodeType === 1) {
      // An interpolated class is resolved here, at the same one place the literal
      // one is applied; mirrors the resolve at the top of runtime/server.go's
      // renderer.node. Each interpolated run is filtered to the class-token
      // characters — a class attribute is a token list, so an unfiltered value
      // could add a class it was never given a slot for.
      const cls = node.classSegs ? classText(node.classSegs, sc) : node.class;
      if (cls) e.className = (e.className ? e.className + " " : "") + cls;
      if (node.style) e.setAttribute("style", node.style);
      // The author's anchor name is the element's id — what a `#install` link
      // scrolls to. Mirrors nodeAttrs in runtime/server.go; it is not node.id,
      // which is the runtime's own region address.
      if (node.anchor) e.setAttribute("id", node.anchor);
      e.__faPath = path; // so refresh() can re-fill this region without re-walking the tree
    }
    return e;
  }

  // renderKids renders a node's children, each under its own render path.
  function renderKids(container, nodes, sc, base) {
    let i = 0;
    for (const c of list(nodes)) container.appendChild(render(c, sc, childPath(base, i++)));
  }

  function render0(node, sc, path) {
    switch (node.kind) {
      case "box": {
        const d = el("div", "fa-box");
        renderKids(d, node.children, sc, path);
        return d;
      }
      case "row": {
        const d = el("div", "fa-row");
        renderKids(d, node.children, sc, path);
        return d;
      }
      case "text": {
        const span = el("span", "fa-text");
        appendSegs(span, node.segs, sc);
        return span;
      }
      case "heading": {
        // A text leaf that lands in an <h1>…<h6> instead of a span: same
        // segments, same appendSegs, same escaping. The level is a value because
        // the depth a header renders at belongs to whoever used it.
        const h = el("h" + headingLevel(ev(node.level, sc)), "fa-heading");
        appendSegs(h, node.segs, sc);
        return h;
      }
      case "image": {
        const img = el("img", "fa-image");
        img.src = mediaSrc(attrText(node.segs, sc));
        // Absent, this is "" — correct markup for a decorative image, and the
        // reason a missing description is reported by the compiler rather than
        // showing up as anything wrong here.
        img.alt = attrText(node.alt, sc);
        return img;
      }
      case "icon": {
        const i = el("span", "fa-icon");
        i.setAttribute("data-fa-icon", attrText(node.segs, sc));
        i.setAttribute("aria-hidden", "true");
        return i;
      }
      case "video": {
        const v = el("video", "fa-video");
        v.controls = true;
        v.src = mediaSrc(attrText(node.segs, sc));
        // A <video> has no alt; its accessible name is aria-label, and an empty
        // one names nothing — so it is written only when there is a name.
        const name = attrText(node.alt, sc);
        if (name) v.setAttribute("aria-label", name);
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
        fillTabs(d, node, sc, path);
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
        fillList(d, node, sc, path);
        return d;
      }
      case "if": {
        // The mirror of runtime/server.go's `case "if"`, and for the same reason:
        // `if` is control flow, not a box, so one that is not a region has no
        // element of its own and its children belong to the parent. A fragment is
        // how "no element" is said here — render() skips a fragment (nodeType 11)
        // when it applies class/style, and appendChild inlines its children.
        if (!node.id) {
          const f = document.createDocumentFragment();
          fillIf(f, node, sc, path);
          return f;
        }
        // A top-level `if` keeps its element: it is the re-fill anchor, and it has
        // to be there while the branch is false. `display:contents` keeps it out
        // of the parent's layout, so an empty one takes no grid or flex slot.
        const d = el("div");
        d.setAttribute("data-fa-region", node.id);
        d.setAttribute("style", "display:contents");
        fillIf(d, node, sc, path);
        return d;
      }
      case "match": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillMatch(d, node, sc, path);
        return d;
      }
      case "use": {
        const d = el("div", node.id ? null : "fa-use");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillUse(d, node, sc, path);
        return d;
      }
      case "input": {
        const i = el("input");
        // node.value is `password`/`newpassword`'s autocomplete token: this is
        // an `input` with two more attributes, mirroring runtime/server.go's arm.
        // The token is written through verbatim on both sides, so neither file
        // holds a mapping from keyword to token that the other could disagree
        // with — and hydrating a masked box into an unmasked one would show the
        // secret the server had just hidden.
        if (node.value) { i.setAttribute("type", "password"); i.setAttribute("autocomplete", node.value); }
        i.setAttribute("data-fa-input", node.bind);
        i.placeholder = attrText(node.placeholder, sc);
        i.value = toStr(sc[node.bind]);
        return i;
      }
      case "select": {
        const s = el("select", "fa-select");
        s.setAttribute("data-fa-input", node.bind);
        const rid = optionsRegionId(node);
        if (rid) s.setAttribute("data-fa-region", rid);
        fillOptions(s, node, sc, path);
        return s;
      }
      // The four controls that render alongside "input". Each arm mirrors the arm
      // of the same name in runtime/server.go, attribute for attribute, because
      // the server's markup is the first paint and this is what replaces it a few
      // milliseconds later — a control that hydrated into a different element
      // would silently lose whatever the actor had already done to it.
      case "textarea": {
        const t = el("textarea", "fa-textarea");
        t.setAttribute("data-fa-input", node.bind);
        t.placeholder = attrText(node.placeholder, sc);
        // Both, and in this order: a textarea's content is its default value (what
        // the server wrote) and .value is its current one.
        t.textContent = toStr(sc[node.bind]);
        t.value = toStr(sc[node.bind]);
        return t;
      }
      case "checkbox": {
        // node.value === "switch" is `toggle`: the same control, said differently.
        const wrap = el("label", node.value === "switch" ? "fa-toggle" : "fa-checkbox");
        const i = el("input");
        i.setAttribute("type", "checkbox");
        i.setAttribute("data-fa-input", node.bind);
        if (node.value === "switch") i.setAttribute("role", "switch");
        i.checked = truthy(sc[node.bind]);
        wrap.appendChild(i);
        const w = el("span"); w.textContent = attrText(node.label, sc);
        wrap.appendChild(w);
        return wrap;
      }
      case "radio": {
        const g = el("div", "fa-radio");
        g.setAttribute("role", "radiogroup");
        const rid = optionsRegionId(node);
        if (rid) g.setAttribute("data-fa-region", rid);
        fillOptions(g, node, sc, path);
        return g;
      }
      case "overlay": {
        const d = el("div");
        if (node.id) d.setAttribute("data-fa-region", node.id);
        fillOverlay(d, node, sc, path);
        return d;
      }
      case "typeahead": {
        const frag = document.createDocumentFragment();
        const i = el("input", "fa-typeahead");
        i.setAttribute("data-fa-input", node.bind);
        i.placeholder = attrText(node.placeholder, sc);
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
        renderKids(f, node.children, sc, path);
        const submit = el("button"); submit.type = "submit"; submit.textContent = attrText(node.label, sc);
        f.appendChild(submit);
        return f;
      }
      case "upload": {
        const label = el("label", "fa-upload");
        label.appendChild(document.createTextNode(attrText(node.label, sc)));
        const i = el("input"); i.type = "file";
        i.setAttribute("data-fa-upload", node.bind);
        label.appendChild(i);
        return label;
      }
      case "link": {
        const href = linkHref(node, sc);
        // Hide a link to a route the actor may not enter (the server also refuses it).
        if (!routeAllowed(href)) return document.createComment("guarded");
        // A computed destination could not be route-checked at compile time, so it
        // is checked here: it is a link only if it names a route this app serves.
        // An off-site URL arriving as a value is not one, deliberately — only a
        // destination the author wrote may leave this app. Mirrors the `n.Route`
        // arm of runtime/server.go.
        // An external destination is re-checked against the same scheme allowlist
        // the compiler used, on the string about to reach the browser; mirrors the
        // `n.External` arm of runtime/server.go.
        if ((node.route && !isAppRoute(href)) ||
            (node.external && !safeExternalHref(href))) {
          const s = el("span", "fa-link");
          s.textContent = attrText(node.label, sc);
          return s;
        }
        const a = el("a", "fa-link");
        a.textContent = attrText(node.label, sc);
        a.setAttribute("href", href);
        // Unconditional on an external link and not the author's to forget; see
        // the same decision written out in runtime/server.go. No `target`.
        if (node.external) a.setAttribute("rel", "noopener noreferrer");
        return a;
      }
    }
    return document.createComment("?");
  }

  // rowsFor resolves the rows a repeating node renders — the client half of
  // runtime/server.go's renderer.rows, and used by the two nodes that repeat: a
  // `for` region and the `for` inside a choice list.
  //
  // The two kinds of repeat resolve in two different places, exactly as they do on
  // the server. Over a `[T]` state cell it is local data the client already holds,
  // so it is filtered and ordered here (selectRows). Over an entity it is a query:
  // the authority answered it against the database and sent the result set, which
  // is used as given — no filter, no sort, no limit re-applied. When the state the
  // answer was computed under has moved, a fresh one is asked for and what is
  // known now is returned meanwhile, so a pending round trip never blanks a list
  // that already has rows.
  function rowsFor(node, sc, path, container) {
    if (node.coll in stateType) return selectRows(sc[node.coll] || [], node, sc);
    if (container) regionCtx[path] = { el: container, node: node, sc: sc };
    const fp = listFingerprint(node, sc);
    // The bootstrap (and every region response) is itself an answer under the
    // state it arrived with, so rows with no recorded fingerprint adopt this one.
    if (path in regionRows && !(path in regionFP)) regionFP[path] = fp;
    // Ask once per question: a region whose rows the answer did not carry (an
    // inactive tab the authority did not render) must not re-ask in a loop.
    if (regionFP[path] !== fp && regionPending[path] !== fp) {
      regionPending[path] = fp;
      askPageData(path);
    }
    return regionRows[path] || [];
  }

  // rowPathFor is the address of row i of a repeating node: the client half of
  // runtime/region.go's listRowPath, branching on the same fact in the same order.
  // A `[T]` cell's rows carry no id to be told apart by, so their ordinal is folded
  // into the path first; a table's rows are addressed by the id, which survives the
  // row moving. Both halves must spell an address identically or the aggregate
  // values and nested-region rows the render recorded sit at keys nobody asks for.
  function rowPathFor(node, base, i, row) {
    return node.coll in stateType ? rowPath(childPath(base, i), row) : rowPath(base, row);
  }

  // fillList renders a `for` region.
  function fillList(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    let i = 0;
    for (const row of rowsFor(node, sc, path, container)) {
      const childScope = Object.assign({}, sc); childScope[node.var] = row;
      renderKids(container, node.children, childScope, rowPathFor(node, path, i++, row));
    }
  }

  // optionsRegionId is the region address of a control whose choices come from
  // data, and "" for one whose choices are fixed — the mirror of optionRegionID in
  // runtime/server.go.
  //
  // A control is a region only when it has something to re-render: a fixed list
  // never changes, and a list drawn from a collection changes whenever that
  // collection does. The id is the control's own, because the element holding the
  // options IS the control.
  function optionsRegionId(node) {
    return (node.kind === "select" || node.kind === "radio") && list(node.children).length ? (node.id || "") : "";
  }

  // optionValue is one choice's stored identity: the literal the author wrote, or
  // the value this render computed for this row. It is ir.Node.val standing beside
  // ir.Node.value, resolved — the one thing about a data-driven choice that does
  // not exist until now.
  function optionValue(node, sc) {
    return node.val ? toStr(ev(node.val, sc)) : toStr(node.value);
  }

  // eachOption walks a control's choices in source order, yielding each one's
  // stored value and its displayed label.
  //
  // It is the whole of what a select and a radio group share, and the whole of what
  // "choices from data" means on this side: `options` (every choice fixed) and the
  // `option`/`options` children (the list holding something computed) are one list
  // said two ways, and each row of a repeating entry supplies one choice. It is
  // runtime/server.go's renderer.eachOption; runtime/control_test.go pins them.
  function eachOption(node, sc, path, emit) {
    for (const o of list(node.options)) emit(o.value, segsToStr(o.label, sc));
    let i = 0;
    for (const c of list(node.children)) {
      const cpath = childPath(path, i++);
      if (c.kind !== "options") { emit(optionValue(c, sc), segsToStr(c.label, sc)); continue; }
      let j = 0;
      for (const row of rowsFor(c, sc, cpath)) {
        const childScope = Object.assign({}, sc); childScope[c.var] = row;
        // A row's expressions are addressed the way the server addressed them, so
        // an aggregate inside a label resolves to the same value on both sides.
        curPath = rowPathFor(c, cpath, j++, row);
        emit(optionValue(c, childScope), segsToStr(c.label, childScope));
      }
      curPath = path; // the rows moved it; the control's own scope resumes here
    }
  }

  // fillOptions (re)builds a control's choice list in place.
  //
  // One function for a select and a radio group, because a choice list is one idea:
  // the same walk, the same current value, and only the markup of one choice
  // differs. Each arm mirrors the arm of the same name in runtime/server.go — in
  // particular a radio's `name` is the bound cell, so every radio written against
  // that cell is one-of-N to the browser without being told so.
  //
  // It doubles as the control's own re-render: rebuilding sets every choice's
  // selected/checked state from the cell, which is what syncControl does. So a
  // control whose options come from data refreshes on both of its edges — its cell
  // changed, or the collection behind it did — through one path.
  function fillOptions(container, node, sc, path) {
    // Never rebuild the control the actor is operating — the rule the two-way input
    // path has always followed. syncControl still writes the cell back into it.
    if (holdsFocus(container)) return;
    container.textContent = "";
    curPath = path; // this control's own expressions are addressed from here
    const cur = toStr(sc[node.bind]);
    eachOption(node, sc, path, (value, label) => {
      if (node.kind === "radio") {
        const wrap = el("label", "fa-radio-option");
        const i = el("input");
        i.setAttribute("type", "radio");
        i.setAttribute("name", node.bind);
        i.setAttribute("value", value);
        i.setAttribute("data-fa-input", node.bind);
        i.checked = value === cur;
        wrap.appendChild(i);
        const w = el("span"); w.textContent = label;
        wrap.appendChild(w);
        container.appendChild(wrap);
      } else {
        const opt = el("option");
        opt.value = value; opt.textContent = label;
        if (value === cur) opt.selected = true;
        container.appendChild(opt);
      }
    });
  }

  // listFingerprint is what a region's rows depend on: the values its `where` and
  // `limit` read out of scope, plus a counter the authority bumps whenever it
  // announces that the collection changed. When it moves, the rows are stale.
  function listFingerprint(node, sc) {
    const names = node.__faDeps || (node.__faDeps = exprRefs(node.where).concat(exprRefs(node.limit)));
    const vals = {};
    for (const n of names) vals[n] = sc[n];
    return JSON.stringify([entityVersion[node.coll] || 0, vals]);
  }

  // exprRefs lists every name an expression reads, in a stable order.
  function exprRefs(e, out) {
    out = out || [];
    if (!e) return out;
    if (e.kind === "ref" && out.indexOf(e.name) < 0) out.push(e.name);
    if ((e.kind === "agg" || e.kind === "eget") && out.indexOf(e.name) < 0) out.push(e.name);
    for (const sub of [e.l, e.r, e.x, e.obj, e.key, e.where]) exprRefs(sub, out);
    for (const a of list(e.args)) exprRefs(a, out);
    return out;
  }

  // askPageData re-asks the authority for this page's data under the client's
  // current state: the rows of every region it renders, and the value of every
  // aggregate those regions evaluate. It answers with data rather than HTML, so
  // this renderer stays the only thing that builds DOM and the payload stays the
  // size of a page.
  //
  // One request answers the whole page deliberately. A state change usually feeds
  // several regions at once — a search box filters posts *and* people, and a new
  // post moves both a feed and a count beside it — and asking per region would
  // cost a round trip each and let them disagree with one another in between.
  //
  // Only the newest question is answered: a keystroke supersedes the request the
  // previous keystroke made, so a slow reply can never overwrite a newer one.
  let pageDataSeq = 0;
  async function askPageData(key) {
    const seq = ++pageDataSeq;
    let res;
    try {
      res = await fetch("/region", {
        method: "POST", headers: { "Content-Type": "application/json", "X-Facet-CSRF": csrf },
        body: JSON.stringify({ path: location.pathname, key: key || "", state: store }),
      });
    } catch (_) { return; }
    if (seq !== pageDataSeq || !res.ok) return;
    let data;
    try { data = await res.json(); } catch (_) { return; }
    if (seq !== pageDataSeq) return;
    for (const k in (data.regions || {})) regionRows[k] = data.regions[k] || [];
    aggValues = data.aggs || {};
    mergeMedia(data.media); // fresh rows arrive with the signatures to render them by
    regionFP = {}; // these rows answer the state the request carried; re-adopted on fill
    applyPageData();
  }

  // applyPageData re-renders what the authority just recomputed: every tracked
  // region and every bound interpolation.
  //
  // A region holding the focused element is skipped. Rebuilding it would take the
  // caret out from under someone mid-word, and it is the same rule the two-way
  // input path has always followed — an input the user is editing is never
  // overwritten by a refresh.
  function applyPageData() {
    for (const id in regionById) {
      const node = regionById[id];
      const c = root.querySelector('[data-fa-region="' + id + '"]');
      if (!c || holdsFocus(c)) continue;
      fillFor(node.kind)(c, node, store, c.__faPath || "");
    }
    for (const id in bindings) refreshBind(id);
    openE2E();
  }

  function holdsFocus(el) {
    const active = document.activeElement;
    return !!(active && el && typeof el.contains === "function" && el.contains(active));
  }

  // fillFor maps a region node's kind to the function that re-renders it.
  function fillFor(kind) {
    return kind === "list" ? fillList : kind === "use" ? fillUse : kind === "tabs" ? fillTabs
      : kind === "match" ? fillMatch : kind === "overlay" ? fillOverlay
      : kind === "select" || kind === "radio" ? fillOptions : fillIf;
  }

  // refreshBind re-evaluates one tracked interpolation in place, at the render
  // path it was first evaluated at (so its aggregates resolve to the same value).
  function refreshBind(id) {
    for (const e of root.querySelectorAll('[data-fa-bind="' + id + '"]')) {
      curPath = e.__faPath || "";
      const v = toStr(ev(bindings[id].expr, store));
      if (e.hasAttribute("data-fa-e2e")) {
        // A sealed bind got a new ciphertext: restash it and let openE2E reopen.
        e.setAttribute("data-fa-e2e", v);
        e.textContent = E2E_PLACEHOLDER;
      } else if (e.textContent !== v) {
        e.textContent = v;
      }
    }
  }

  function fillIf(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    if (truthy(ev(node.cond, sc))) renderKids(container, node.children, sc, path);
  }
  // An overlay shows a modal layer while its bound cell is truthy: a backdrop (a
  // click on which closes it) wrapping a centered panel of the children.
  function fillOverlay(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    if (!truthy(sc[node.bind])) return;
    const backdrop = el("div", "fa-overlay-backdrop");
    backdrop.setAttribute("data-fa-close", node.bind);
    const panel = el("div", "fa-overlay-panel");
    renderKids(panel, node.children, sc, path);
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
  function fillMatch(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    const arms = list(node.children);
    const val = toStr(ev(node.cond, sc));
    let i = arms.findIndex((c) => c.kind === "case" && c.value === val);
    if (i < 0) i = arms.findIndex((c) => c.kind === "else");
    if (i >= 0) renderKids(container, arms[i].children, sc, childPath(path, i));
  }
  function activeTabValue(node, sc) {
    const tabs = node.children || [];
    const cur = toStr(sc[node.bind]);
    if (tabs.some((t) => t.value === cur)) return cur;
    return tabs.length ? tabs[0].value : cur;
  }
  function fillTabs(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    const tabs = list(node.children);
    const active = activeTabValue(node, sc);
    const strip = el("div", "fa-tabstrip");
    strip.setAttribute("role", "tablist");
    for (const t of tabs) {
      const b = el("button", "fa-tab");
      b.setAttribute("role", "tab");
      b.textContent = attrText(t.label, sc);
      b.setAttribute("data-fa-tab", t.value);
      b.setAttribute("data-fa-tab-bind", node.bind);
      if (t.value === active) b.setAttribute("aria-selected", "true");
      strip.appendChild(b);
    }
    container.appendChild(strip);
    // Only the active tab's body renders — on both sides — so it is addressed
    // under its own index, and an inactive tab's regions simply do not exist
    // until it is selected and their rows are asked for.
    const i = tabs.findIndex((t) => t.value === active);
    if (i >= 0) renderKids(container, tabs[i].children, sc, childPath(path, i));
  }
  function fillUse(container, node, sc, path) {
    container.textContent = "";
    curPath = path; // this region's own expressions are addressed from here
    const comp = components[node.name];
    if (!comp) return;
    const childScope = Object.assign({}, sc);
    list(comp.params).forEach((p, i) => (childScope[p.name] = coerce(ev(list(node.args)[i], sc), p.type)));
    // The component body renders inline at the call site, so it is addressed
    // under this `use` node's path — one component used twice yields two
    // distinct sets of region keys, which is what makes them addresses.
    renderKids(container, comp.view, childScope, path);
  }

  // selectRows mirrors runtime/server.go: filter by `where`, order by `by`, cap by
  // `limit` — so the client re-renders a list exactly as the server first painted it.
  function selectRows(rows, node, sc) {
    let out = rows;
    if (node.where) {
      out = [];
      for (const r of rows) {
        const child = Object.assign({}, sc); child[node.var] = r;
        if (truthy(evRow(node.where, child))) out.push(r);
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
  function routeAllowed(href) {
    const path = hrefPath(href);
    if (path === "") return true; // an anchor on the page the reader is already on
    for (const rt of routes) {
      if (!matchRoute(rt.path, path)) continue;
      if (!rt.requires) return true;
      const pol = policies[rt.requires];
      return pol ? truthy(ev(pol.expr, store)) : false;
    }
    return true; // not an app route (e.g. an external or asset link)
  }
  function isAppRoute(href) {
    const path = hrefPath(href);
    if (path === "") return false;
    for (const rt of routes) if (matchRoute(rt.path, path)) return true;
    return false;
  }
  // hrefPath is the route-bearing half of a destination: everything before the
  // `#`. A fragment is a position inside a page, not a route, so it comes off
  // before the route table is asked anything — otherwise `/docs#install` is a
  // one-segment path spelled `docs#install` and matches nothing. Mirrors
  // runtime/link.go.
  function hrefPath(href) {
    const i = href.indexOf("#");
    return i < 0 ? href : href.slice(0, i);
  }
  // safeExternalHref mirrors runtime/link.go: the render-time half of the
  // compiler's scheme allowlist, asked of the string about to reach the browser
  // rather than of a flag set earlier in the pipeline. Case folding is ASCII-only
  // and written out because toLowerCase and Go's do not agree on every input, and
  // an href the server links and the client turns into text is a link that
  // changes on hydration.
  function safeExternalHref(href) {
    for (const scheme of ["https://", "http://"]) {
      if (!hasPrefixFold(href, scheme)) continue;
      const rest = href.slice(scheme.length);
      for (let i = 0; i < rest.length; i++) {
        if (rest[i] === "/" || rest[i] === "?" || rest[i] === "#") return i > 0;
      }
      return rest !== "";
    }
    if (hasPrefixFold(href, "mailto:")) return href.length > "mailto:".length;
    return false;
  }
  function hasPrefixFold(s, prefix) {
    if (s.length < prefix.length) return false;
    for (let i = 0; i < prefix.length; i++) {
      let c = s.charCodeAt(i);
      if (c >= 65 && c <= 90) c += 32;
      if (c !== prefix.charCodeAt(i)) return false;
    }
    return true;
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
  // syncTheme mirrors the built-in `theme` state to <html data-theme> and persists
  // the choice (cookie + localStorage), so an action like `theme = "dark"` flips
  // the palette instantly with no round-trip, and a reload restores it — the server
  // reads the same cookie for first paint. Only apps with an alternate palette carry
  // a `theme` state; everything else is a no-op.
  let lastTheme;
  function syncTheme() {
    if (!("theme" in store)) return;
    const t = toStr(store.theme || "");
    if (t === lastTheme) return;
    lastTheme = t;
    if (t) document.documentElement.setAttribute("data-theme", t);
    else document.documentElement.removeAttribute("data-theme");
    try { localStorage.setItem("fa-theme", t); } catch (e) {}
    document.cookie = "fa_theme=" + encodeURIComponent(t) + ";path=/;max-age=31536000;samesite=lax";
  }

  // controlValue reads what an element bound to a cell currently holds. Which
  // property carries it is decided by the `type` attribute BOTH renderers wrote,
  // never by a DOM property one of them set — so a hydrated control and a
  // server-painted one are read the same way.
  function controlValue(t, name) {
    const type = t.getAttribute("type");
    if (type === "checkbox") return !!t.checked;
    if (type === "radio") return t.getAttribute("value");
    return (stateType[name] === "int" || stateType[name] === "money" || stateType[name] === "date") ? toInt(t.value) : t.value;
  }

  // syncControl writes a cell's value back into every element bound to it.
  //
  // Every element, not the first one found: a radio group is N elements sharing
  // one cell, and the element that has to change when the cell changes is usually
  // not the one the actor touched — choosing `b` is `a` becoming unchecked. The
  // same walk keeps two controls over one cell (a toggle in a menu and the
  // checkbox in the panel it opens) showing the same thing.
  function syncControl(node) {
    const cur = store[node.bind];
    for (const e of root.querySelectorAll('[data-fa-input="' + node.bind + '"]')) {
      const type = e.getAttribute("type");
      if (type === "checkbox") { e.checked = truthy(cur); continue; }
      if (type === "radio") { e.checked = e.getAttribute("value") === toStr(cur); continue; }
      if (e === document.activeElement) continue; // never yank the field being typed in
      if (e.tagName === "TEXTAREA") e.textContent = toStr(cur);
      e.value = toStr(cur);
    }
  }

  function refresh(changed) {
    syncTheme();
    const ids = new Set();
    for (const name of changed) for (const id of (ir.depGraph[name] || [])) ids.add(id);
    // Three independent questions about one id, not three cases of one question.
    // A control whose choices come from data is both a region (the collection
    // moved, so the options must be rebuilt) and a two-way input (the cell moved,
    // so the selection must follow it), and an `else if` would silently answer
    // only the first — leaving a dropdown whose options are right and whose
    // selection is not. The three id spaces are disjoint, so every id that was
    // answered by exactly one branch before is still answered by exactly one.
    for (const id of ids) {
      if (bindings[id]) refreshBind(id);
      if (regionById[id]) {
        const node = regionById[id];
        const c = root.querySelector('[data-fa-region="' + id + '"]');
        // __faPath is stamped by render(): the region's address, which its own
        // re-fill needs so nested regions under it keep the same keys.
        if (c) fillFor(node.kind)(c, node, store, c.__faPath || "");
      }
      if (inputById[id]) syncControl(inputById[id]);
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
    list(act.params).forEach((p, i) => (scope[p.name] = vals[i]));
    const changed = [];
    for (const st of list(act.body)) if (st.op === "assign") {
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
    list(act.params).forEach((p, i) => (scope[p.name] = vals[i]));
    const changed = [];
    let tempId = -Date.now();
    for (const st of list(act.body)) {
      if (st.op === "assign") {
        const v = ev(st.value, scope);
        store[st.target] = v; scope[st.target] = v; changed.push(st.target);
      } else if (st.op === "add") {
        const row = { id: tempId-- };
        for (const fi of list(st.fields)) row[fi.name] = ev(fi.expr, scope);
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

  // applyDeltas folds an authority change into local state.
  //
  // `deltas` carries values the client cannot recompute: per-session state cells
  // from an action's reply, and the rows of the few collections this client's own
  // aggregates still scan. `changed` names every entity that moved, including the
  // ones whose rows were deliberately not sent — bumping their version stales
  // every region reading them, so those regions re-ask for one page of rows
  // instead of the authority pushing a whole table at every subscriber.
  function applyDeltas(deltas, changed) {
    const names = [];
    for (const k in (deltas || {})) { store[k] = deltas[k]; names.push(k); }
    for (const k of list(changed)) {
      entityVersion[k] = (entityVersion[k] || 0) + 1;
      if (names.indexOf(k) < 0) names.push(k);
    }
    refresh(names);
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
  // still completes. Either way the server answers with {url, media}: the durable
  // reference to bind, and the signature to render it by until the next payload
  // brings a fresh one.
  const CHUNK_BYTES = 4 * 1024 * 1024; // 4 MiB — switch to chunked above this size

  async function upload(input) {
    const bind = input.getAttribute("data-fa-upload");
    const file = input.files && input.files[0];
    if (!file) return;
    const label = input.closest("label");
    label && label.setAttribute("aria-busy", "true");
    try {
      // The server answers with two things: the durable reference to bind (and
      // eventually store), and the grant to preview it by. Binding the signed URL
      // is what used to put an expiry inside a column.
      const res = file.size > CHUNK_BYTES ? await uploadChunked(file) : await uploadSingle(file);
      if (!res || !res.url) { showError(input, "Upload failed."); return; }
      mergeMedia(res.media);
      store[bind] = res.url;
      refresh([bind]);
    } finally {
      label && label.removeAttribute("aria-busy");
    }
  }

  async function uploadSingle(file) {
    const body = new FormData(); body.append("file", file);
    const res = await fetch("/upload", { method: "POST", headers: { "X-Facet-CSRF": csrf }, body });
    if (!res.ok) return null;
    return await res.json();
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
    return await res.json();
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

  // Build the whole tree before anything is torn down.
  //
  // This used to clear the root and then append as it rendered, which made a
  // throw anywhere in the tree unrecoverable: the server-rendered page was
  // already gone, and what replaced it was nothing. The page the user saw was
  // blank, and the actual fault — one exception, deep in one component — was
  // visible only in a console nobody had open.
  //
  // Rendering into a detached fragment first means the swap is the last thing
  // that happens and only happens on success. A render fault now costs the
  // client-side upgrade and leaves the server's HTML standing, which is a page
  // that still reads and still submits forms. The failure is reported rather
  // than staged as a blank screen.
  function mount() {
    const frag = document.createDocumentFragment();
    regionCtx = {}; // the previous tree's elements are about to be discarded

    try {
      let i = 0;
      for (const n of list(ir.view)) frag.appendChild(render(n, store, childPath("", i++)));
    } catch (err) {
      // Keep whatever is on screen. Re-throw so it reaches window.onerror and
      // any error reporting, rather than being silently swallowed.
      console.error("facet: render failed, keeping the server-rendered page", err);
      throw err;
    }

    root.textContent = "";
    root.appendChild(frag);
    openE2E(); // open the sealed values the first render placed
  }

  // ── wire it up (once) ─────────────────────────────────────────────────────────
  function boot() {
    for (const c of list(ir.components)) components[c.name] = c;
    mount();
  }

  load(JSON.parse(document.getElementById("fa-ir").textContent),
       JSON.parse(document.getElementById("fa-state").textContent));
  boot();

  root.addEventListener("click", function (e) {
    const link = e.target.closest("a.fa-link");
    if (link) {
      const href = link.getAttribute("href");
      // A destination carrying a `#` is left to the browser: it knows how to land
      // on a page and scroll to an anchor, and a client-side navigate() would drop
      // the fragment on the floor.
      if (href && href[0] === "/" && !href.includes("#") &&
          isAppRoute(href) && !e.metaKey && !e.ctrlKey) {
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
  // A control writing its cell is one handler on two events. Text controls report
  // through `input`; a checkbox or radio reports through `change` (and, in most
  // browsers, `input` as well) — so both are listened to and the second one is a
  // no-op, rather than each control kind getting its own listener to forget.
  function controlWrite(e) {
    const t = e.target.closest("[data-fa-input]");
    if (!t) return;
    const name = t.getAttribute("data-fa-input");
    const v = controlValue(t, name);
    touched[name] = true; // touched(name) is now true; dirty(name) tracks the value
    if (store[name] === v) return; // the same event arriving twice changes nothing
    store[name] = v;
    refresh([name]);
  }
  root.addEventListener("input", controlWrite);
  root.addEventListener("change", function (e) {
    const t = e.target.closest("[data-fa-upload]");
    if (t) { upload(t); return; }
    controlWrite(e);
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
      mergeMedia(msg.media); // before applyDeltas: the rows it folds in reference these
      applyDeltas(msg.deltas || {}, msg.changed || []);
      // The authority's write count. If it has moved past the one this page's data
      // was built at, that data is stale — including the counts, which are values
      // now and cannot be recomputed here. One request brings the whole page back
      // consistent. This is also what closes the page-load → stream-open race the
      // old whole-database opening snapshot used to cover.
      if (msg.seq != null && msg.seq !== dataSeq) { dataSeq = msg.seq; askPageData(""); }
    };
  }

  function index(arr, key) { const m = Object.create(null); for (const x of arr || []) m[x[key]] = x; return m; }
})();

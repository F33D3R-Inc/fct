// Facet client runtime — fixed, application-agnostic plumbing. Ships once and
// interprets the per-app IR; no application logic lives here. It renders the
// view from the IR + state, runs client-placed actions locally, forwards
// server-placed actions to the authority, and refreshes only the regions a
// change touches. Its expression semantics mirror runtime/eval.go exactly.
(function () {
  "use strict";

  const ir = JSON.parse(document.getElementById("fa-ir").textContent);
  const store = JSON.parse(document.getElementById("fa-state").textContent);
  const root = document.getElementById("fa-root");

  const actions = index(ir.actions, "name");
  const bindings = index(ir.bindings, "id");
  const policies = index(ir.policies, "name");
  const stateType = {};
  for (const s of ir.states) stateType[s.name] = s.type;

  // tracked top-level regions (lists/ifs) and inputs, keyed by their id.
  const regionById = {};
  const inputById = {};
  (function collect(nodes) {
    for (const n of nodes) {
      if ((n.kind === "list" || n.kind === "if") && n.id) regionById[n.id] = n;
      if (n.kind === "input") inputById[n.id] = n;
      if (n.children) collect(n.children);
    }
  })(ir.view);

  // ── shared expression interpreter (mirrors eval.go) ─────────────────────────
  function ev(e, sc) {
    if (!e) return null;
    switch (e.kind) {
      case "lit":
        if (e.vtype === "int") return toInt(e.val);
        if (e.vtype === "bool") return !!e.val;
        return toStr(e.val);
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
        const rows = sc[e.name] || [];
        if (e.op === "count") return rows.length;
        let total = 0; // sum
        for (const r of rows) total += toInt(r[e.field]);
        return total;
      }
      case "call": {
        // Effectful builtins are placed on the server by the compiler, so this
        // arm is defensive only — a pure context can never contain a call.
        if (e.name === "now") return Math.floor(Date.now() / 1000);
        if (e.name === "rand") {
          const n = e.args && e.args[0] ? toInt(ev(e.args[0], sc)) : 0;
          return n > 0 ? Math.floor(Math.random() * n) : 0;
        }
        return null;
      }
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
        }
      }
    }
    return null;
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

  function render(node, sc) {
    switch (node.kind) {
      case "box": {
        const d = el("div", "fa-box");
        for (const c of node.children || []) d.appendChild(render(c, sc));
        return d;
      }
      case "text": {
        const span = el("span", "fa-text");
        for (const seg of node.segs || []) {
          if (seg.lit != null) span.appendChild(document.createTextNode(seg.lit));
          else if (seg.bind) {
            const b = el("span"); b.setAttribute("data-fa-bind", seg.bind);
            b.textContent = toStr(ev(bindings[seg.bind].expr, sc));
            span.appendChild(b);
          } else if (seg.expr) {
            span.appendChild(document.createTextNode(toStr(ev(seg.expr, sc))));
          }
        }
        return span;
      }
      case "button": {
        const b = el("button");
        b.textContent = node.label;
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
      case "input": {
        const i = el("input");
        i.setAttribute("data-fa-input", node.bind);
        if (node.placeholder) i.placeholder = node.placeholder;
        i.value = toStr(sc[node.bind]);
        return i;
      }
      case "link": {
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
    if (node.limit && out.length > node.limit) out = out.slice(0, node.limit);
    return out;
  }
  function cmpVal(a, b) {
    if (typeof a === "number" && typeof b === "number") return a - b;
    const as = toStr(a), bs = toStr(b);
    return as < bs ? -1 : as > bs ? 1 : 0;
  }
  function fillIf(container, node, sc) {
    container.textContent = "";
    if (truthy(ev(node.cond, sc))) {
      for (const c of node.children || []) container.appendChild(render(c, sc));
    }
  }

  // ── fine-grained refresh: only regions a changed state feeds ────────────────
  function refresh(changed) {
    const ids = new Set();
    for (const name of changed) for (const id of (ir.depGraph[name] || [])) ids.add(id);
    for (const id of ids) {
      if (bindings[id]) {
        const e = root.querySelector('[data-fa-bind="' + id + '"]');
        if (e) e.textContent = toStr(ev(bindings[id].expr, store));
      } else if (regionById[id]) {
        const c = root.querySelector('[data-fa-region="' + id + '"]');
        if (c) (regionById[id].kind === "list" ? fillList : fillIf)(c, regionById[id], store);
      } else if (inputById[id]) {
        const node = inputById[id];
        const e = root.querySelector('[data-fa-input="' + node.bind + '"]');
        if (e && e !== document.activeElement) e.value = toStr(store[node.bind]);
      }
    }
  }

  // ── dispatch: the compiler already chose where each action runs ──────────────
  async function dispatch(action, args, sc) {
    const act = actions[action];
    if (!act) return;
    const vals = (args || []).map((a) => ev(a, sc));
    if (act.placement === "client") {
      const scope = Object.assign({}, store);
      act.params.forEach((p, i) => (scope[p.name] = vals[i]));
      const changed = [];
      for (const st of act.body) {
        if (st.op === "assign") {
          const v = ev(st.value, scope);
          if (store[st.target] !== v) { store[st.target] = v; scope[st.target] = v; changed.push(st.target); }
        }
      }
      refresh(changed);
    } else {
      const res = await fetch("/event", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, args: vals }),
      });
      if (!res.ok) return;
      const data = await res.json();
      if (data.reload) { location.reload(); return; } // identity changed (login/logout)
      const { deltas } = data;
      const changed = [];
      for (const k in deltas) { store[k] = deltas[k]; changed.push(k); }
      refresh(changed);
    }
  }

  // ── wire it up ──────────────────────────────────────────────────────────────
  root.textContent = "";
  for (const n of ir.view) root.appendChild(render(n, store));

  root.addEventListener("click", function (e) {
    const t = e.target.closest("[data-fa-action]");
    if (t && t.__fa) { e.preventDefault(); dispatch(t.__fa.action, t.__fa.args, t.__fa.scope); }
  });
  root.addEventListener("input", function (e) {
    const t = e.target.closest("[data-fa-input]");
    if (!t) return;
    const name = t.getAttribute("data-fa-input");
    store[name] = stateType[name] === "int" ? toInt(t.value) : t.value;
    refresh([name]);
  });

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
      const deltas = msg.deltas || {};
      const changed = [];
      for (const k in deltas) { store[k] = deltas[k]; changed.push(k); }
      refresh(changed);
    };
  }

  function index(arr, key) { const m = Object.create(null); for (const x of arr || []) m[x[key]] = x; return m; }
})();

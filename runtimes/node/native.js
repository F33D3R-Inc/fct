'use strict';
// Native (FA-Native) neutral-tree rendering — the Node mirror of fa/view.go
// (ParseView) and fa/style.go (resolveStyle). A native client (FacetKit /
// facetkit) connects with `FA-Native: 1` and consumes a platform-neutral ViewNode
// tree (kinds box/text/button/image/input/link/icon) instead of HTML. We render
// HTML exactly as for the web, then parse it into that tree — identical to Go's
// RenderTree = ParseView(Render(...)) — so the SAME backend drives web and native.
//
// The ViewNode JSON matches Go field-for-field (kind, tag, attrs, text, facetId,
// action, style, children; attrs keys sorted) so a native client decodes it and
// HMAC verification over the fragment string succeeds across runtimes.

const KIND_BY_TAG = {
  button: 'button', a: 'link', img: 'image',
  input: 'input', textarea: 'input', select: 'input', svg: 'icon',
};
const TEXT_TAGS = new Set(['span', 'p', 'strong', 'b', 'em', 'i', 'small', 'label',
  'time', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'td', 'th', 'caption']);
const VOID_TAGS = new Set(['img', 'input', 'br', 'hr', 'meta', 'link', 'source', 'area', 'col']);

// The std design-system class → style table (mirror of fa/style.go classStyles).
const CLASS_STYLES = {
  'fa-row': { direction: 'row', align: 'center', gap: 8 },
  'fa-post__header': { direction: 'row', gap: 10 },
  'fa-post__actions': { direction: 'row', justify: 'between' },
  'fa-vidctl': { direction: 'row', align: 'center', gap: 10, pad: 8 },
  'fa-engage': { direction: 'row', gap: 8 },
  'fa-feedtabs': { direction: 'row' },
  'fa-tabs': { direction: 'row' },
  'fa-storybar': { direction: 'row', gap: 12, pad: 12 },
  'fa-catchips': { direction: 'row', gap: 8 },
  'fa-roomctl': { direction: 'row', align: 'center', gap: 12, justify: 'center' },
  'fa-composer': { direction: 'row', gap: 10, pad: 12 },
  'fa-composer__bar': { direction: 'row', justify: 'between', align: 'center' },
  'fa-composer__tools': { direction: 'row' },
  'fa-setrow': { direction: 'row', justify: 'between', align: 'center', pad: 12 },
  'fa-bottomnav': { direction: 'row', justify: 'between', pad: 8 },
  'fa-spacebar': { direction: 'row', align: 'center', gap: 10, pad: 10 },
  'fa-subrow': { direction: 'row', align: 'center', gap: 10, pad: 10 },
  'fa-sresult': { direction: 'row', align: 'center', gap: 10, pad: 10 },
  'fa-navrail__item': { direction: 'row', align: 'center', gap: 14, pad: 10 },
  'fa-roomhead': { direction: 'row', align: 'center', gap: 12, pad: 12 },
  'fa-topbar': { direction: 'row', align: 'center', justify: 'between', pad: 10 },
  'fa-vcard__row': { direction: 'row', gap: 10 },
  'fa-chatcompose': { direction: 'row', gap: 6, pad: 8 },
  'fa-stack': { direction: 'column' },
  'fa-card': { direction: 'column', pad: 16, radius: 12, gap: 8 },
  'fa-composer__main': { direction: 'column', gap: 8, grow: true },
  'fa-vcard__meta': { direction: 'column' },
  'fa-rrcard': { direction: 'column', pad: 12, radius: 16, gap: 8 },
  'fa-btn': { direction: 'row', align: 'center', pad: 8, radius: 999, fontWeight: 600 },
  'fa-btn--primary': { bg: '#1d9bf0', fg: '#ffffff' },
  'fa-btn--secondary': { fg: '#0f1419' },
  'fa-btn--danger': { bg: '#f4212e', fg: '#ffffff' },
  'fa-badge': { pad: 4, radius: 999, fontSize: 12, fontWeight: 700 },
  'fa-pill': { pad: 6, radius: 999, fontSize: 12, fontWeight: 700 },
  'fa-tip': { direction: 'row', align: 'center', gap: 6, pad: 8, radius: 999, bg: '#ffc107', fontWeight: 800 },
  'fa-post__name': { fontWeight: 700 },
  'fa-statcard__value': { fontSize: 28, fontWeight: 800 },
  'fa-channelhead__name': { fontSize: 24, fontWeight: 800 },
};

const STYLE_ORDER = ['direction', 'gap', 'padT', 'padR', 'padB', 'padL', 'align',
  'justify', 'grow', 'width', 'height', 'bg', 'fg', 'fontSize', 'fontWeight', 'radius'];

function kindFor(tag) {
  if (KIND_BY_TAG[tag]) return KIND_BY_TAG[tag];
  return TEXT_TAGS.has(tag) ? 'text' : 'box';
}

// ── HTML → ViewNode tree (mirror of fa/view.go) ─────────────────────────────

function parseView(fragment) {
  const p = new HtmlParser(fragment);
  const nodes = p.parseChildren('');
  if (nodes.length === 0) return { kind: 'box' };
  if (nodes.length === 1) return nodes[0];
  return { kind: 'box', children: nodes };
}

class HtmlParser {
  constructor(s) { this.s = s; this.i = 0; }

  parseChildren(parent) {
    const nodes = [];
    while (this.i < this.s.length) {
      if (this.s[this.i] === '<') {
        if (this.s.startsWith('<!--', this.i)) {
          const end = this.s.indexOf('-->', this.i);
          this.i = end >= 0 ? end + 3 : this.s.length;
          continue;
        }
        if (this.i + 1 < this.s.length && this.s[this.i + 1] === '/') {
          this.readCloseTag();
          return nodes;
        }
        const { name, attrs, selfClose } = this.readOpenTag();
        const node = nodeFromTag(name, attrs);
        if (name === 'svg') {
          const end = this.s.toLowerCase().indexOf('</svg>', this.i);
          this.i = end >= 0 ? end + 6 : this.s.length;
        } else if (selfClose || VOID_TAGS.has(name)) {
          // no children
        } else {
          node.children = this.parseChildren(name);
          if (node.kind === 'text') {
            const folded = foldText(node.children);
            if (folded !== null) { node.text = folded; delete node.children; }
          }
          if (node.children && node.children.length === 0) delete node.children;
        }
        nodes.push(node);
      } else {
        const j = this.s.indexOf('<', this.i);
        let raw;
        if (j < 0) { raw = this.s.slice(this.i); this.i = this.s.length; }
        else { raw = this.s.slice(this.i, j); this.i = j; }
        const t = raw.trim();
        if (t !== '') nodes.push({ kind: 'text', text: htmlUnescape(t) });
      }
    }
    return nodes;
  }

  readOpenTag() {
    this.i++; // '<'
    const name = this.readName();
    const attrs = {};
    let selfClose = false;
    while (this.i < this.s.length) {
      this.skipSpace();
      if (this.i >= this.s.length) break;
      const c = this.s[this.i];
      if (c === '/') { selfClose = true; this.i++; continue; }
      if (c === '>') { this.i++; break; }
      const an = this.readName();
      if (an === '') { this.i++; continue; }
      let av = '';
      this.skipSpace();
      if (this.i < this.s.length && this.s[this.i] === '=') {
        this.i++; this.skipSpace(); av = this.readAttrValue();
      }
      attrs[an.toLowerCase()] = av;
    }
    return { name, attrs, selfClose };
  }

  readCloseTag() {
    this.i += 2;
    this.readName();
    const j = this.s.indexOf('>', this.i);
    this.i = j >= 0 ? j + 1 : this.s.length;
  }

  readName() {
    const start = this.i;
    while (this.i < this.s.length) {
      const c = this.s[this.i];
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '>' || c === '/' || c === '=') break;
      this.i++;
    }
    return this.s.slice(start, this.i).toLowerCase();
  }

  readAttrValue() {
    if (this.i >= this.s.length) return '';
    const q = this.s[this.i];
    if (q === '"' || q === "'") {
      this.i++;
      const start = this.i;
      while (this.i < this.s.length && this.s[this.i] !== q) this.i++;
      const v = this.s.slice(start, this.i);
      if (this.i < this.s.length) this.i++;
      return htmlUnescape(v);
    }
    const start = this.i;
    while (this.i < this.s.length && this.s[this.i] !== ' ' && this.s[this.i] !== '>' && this.s[this.i] !== '/') this.i++;
    return this.s.slice(start, this.i);
  }

  skipSpace() {
    while (this.i < this.s.length && ' \t\n\r'.includes(this.s[this.i])) this.i++;
  }
}

function nodeFromTag(name, attrs) {
  const n = { kind: kindFor(name), tag: name };
  if (Object.keys(attrs).length > 0) n.attrs = attrs;
  if (attrs['data-facet-id'] != null) n.facetId = attrs['data-facet-id'];
  if (attrs['data-action'] != null) n.action = attrs['data-action'];
  const st = resolveStyle(name, attrs);
  if (st) n.style = st;
  return n;
}

function foldText(children) {
  let out = '';
  for (const c of children || []) {
    if (c.kind !== 'text' || (c.children && c.children.length > 0)) return null;
    out += c.text || '';
  }
  return out;
}

function htmlUnescape(s) {
  return s.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (m, body) => {
    if (body[0] === '#') {
      const code = body[1] === 'x' || body[1] === 'X' ? parseInt(body.slice(2), 16) : parseInt(body.slice(1), 10);
      return Number.isFinite(code) ? String.fromCodePoint(code) : m;
    }
    const named = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ' };
    return named[body] != null ? named[body] : m;
  });
}

// ── style resolution (mirror of fa/style.go) ────────────────────────────────

function resolveStyle(tag, attrs) {
  let s = {};
  if (tag === 'button' || tag === 'a') { s.direction = 'row'; s.align = 'center'; }
  const cls = attrs['class'];
  if (cls) {
    for (const c of cls.split(/\s+/).filter(Boolean)) {
      if (CLASS_STYLES[c]) merge(s, CLASS_STYLES[c]);
    }
  }
  if (attrs['style']) applyInline(s, attrs['style']);
  expandPad(s);
  return isZeroStyle(s) ? null : orderStyle(s);
}

function merge(s, o) {
  for (const k of ['direction', 'align', 'justify', 'width', 'height', 'bg', 'fg']) {
    if (o[k]) s[k] = o[k];
  }
  for (const k of ['gap', 'pad', 'padT', 'padR', 'padB', 'padL', 'fontSize', 'fontWeight', 'radius']) {
    if (o[k]) s[k] = o[k];
  }
  if (o.grow) s.grow = true;
}

function expandPad(s) {
  if (s.pad) {
    if (!s.padT) s.padT = s.pad;
    if (!s.padR) s.padR = s.pad;
    if (!s.padB) s.padB = s.pad;
    if (!s.padL) s.padL = s.pad;
    delete s.pad;
  }
}

function isZeroStyle(s) {
  for (const k of STYLE_ORDER) {
    if (k === 'grow') { if (s.grow) return false; }
    else if (typeof s[k] === 'string') { if (s[k]) return false; }
    else if (s[k]) return false;
  }
  return true;
}

// orderStyle returns a new object with keys inserted in Go field order, omitting
// empties — so JSON.stringify produces bytes matching Go's marshaling.
function orderStyle(s) {
  const out = {};
  for (const k of STYLE_ORDER) {
    const v = s[k];
    if (k === 'grow') { if (v) out.grow = true; }
    else if (typeof v === 'string') { if (v) out[k] = v; }
    else if (v) out[k] = v;
  }
  return out;
}

function applyInline(s, inline) {
  for (const decl of inline.split(';')) {
    const idx = decl.indexOf(':');
    if (idx < 0) continue;
    const prop = decl.slice(0, idx).trim().toLowerCase();
    const val = decl.slice(idx + 1).trim();
    switch (prop) {
      case 'width': s.width = val; break;
      case 'height': s.height = val; break;
      case 'background': case 'background-color': s.bg = val; break;
      case 'color': s.fg = val; break;
      case 'padding': setPadding(s, val); break;
      case 'padding-top': s.padT = px(val); break;
      case 'padding-right': s.padR = px(val); break;
      case 'padding-bottom': s.padB = px(val); break;
      case 'padding-left': s.padL = px(val); break;
      case 'border-radius': s.radius = px(val); break;
      case 'font-size': s.fontSize = px(val); break;
      case 'font-weight':
        if (/^\d+$/.test(val)) s.fontWeight = parseInt(val, 10);
        else if (val === 'bold') s.fontWeight = 700;
        break;
      case 'gap': s.gap = px(val); break;
      case 'flex-direction': if (val === 'row' || val === 'column') s.direction = val; break;
      case 'display': if (val === 'flex' && !s.direction) s.direction = 'row'; break;
      case 'justify-content': s.justify = mapJustify(val); break;
      case 'align-items': s.align = mapAlign(val); break;
      case 'flex': case 'flex-grow': {
        const f = val.split(/\s+/);
        if (f.length > 0 && px(f[0]) > 0) s.grow = true;
        break;
      }
    }
  }
}

function setPadding(s, val) {
  const p = val.split(/\s+/).filter(Boolean).map(px);
  if (p.length === 1) { s.padT = s.padR = s.padB = s.padL = p[0]; }
  else if (p.length === 2) { s.padT = s.padB = p[0]; s.padR = s.padL = p[1]; }
  else if (p.length === 3) { s.padT = p[0]; s.padR = s.padL = p[1]; s.padB = p[2]; }
  else if (p.length === 4) { s.padT = p[0]; s.padR = p[1]; s.padB = p[2]; s.padL = p[3]; }
}

function px(v) {
  v = v.trim().replace(/px$/, '');
  const dot = v.indexOf('.');
  if (dot >= 0) v = v.slice(0, dot);
  const n = parseInt(v.trim(), 10);
  return Number.isFinite(n) ? n : 0;
}

function mapJustify(v) {
  return v === 'center' ? 'center' : (v === 'flex-end' || v === 'end') ? 'end' : v === 'space-between' ? 'between' : 'start';
}
function mapAlign(v) {
  return v === 'center' ? 'center' : (v === 'flex-end' || v === 'end') ? 'end' : v === 'stretch' ? 'stretch' : 'start';
}

// ── serialization (Go field order; sorted attrs) ────────────────────────────

const NODE_ORDER = ['kind', 'tag', 'attrs', 'text', 'facetId', 'action', 'style', 'children'];

function nodeToJSON(node) {
  const out = {};
  for (const k of NODE_ORDER) {
    if (node[k] == null) continue;
    if (k === 'attrs') {
      const keys = Object.keys(node.attrs).sort();
      if (keys.length === 0) continue;
      const a = {};
      for (const ak of keys) a[ak] = node.attrs[ak];
      out.attrs = a;
    } else if (k === 'children') {
      if (node.children.length === 0) continue;
      out.children = node.children.map(nodeToJSON);
    } else {
      out[k] = node[k];
    }
  }
  return out;
}

// treeJSON renders HTML → ViewNode tree → compact JSON string (the native fragment).
function treeJSON(html) {
  return JSON.stringify(nodeToJSON(parseView(html)));
}

module.exports = { parseView, resolveStyle, nodeToJSON, treeJSON, htmlUnescape };

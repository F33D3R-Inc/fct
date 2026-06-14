// Tests for the client render-body engine (fill) in fa-runtime.js: interpolation
// with escaping, {if}/{else}/{end}, {for}/{end}, dotted paths, and Go-style
// truthiness. Run: `node runtime/fill_test.js`.
'use strict';
var assert = require('assert');
var fill = require('./fa-runtime.js').fill;

var pass = 0;
function eq(got, want, msg) {
  assert.strictEqual(got, want, msg + '\n  got:  ' + JSON.stringify(got) + '\n  want: ' + JSON.stringify(want));
  pass++;
}

// 1. plain interpolation, HTML-escaped; missing field → empty.
eq(fill('<p>{name}</p>', { name: 'Ada' }), '<p>Ada</p>', 'interp');
eq(fill('<p>{x}</p>', { x: '<b>&"' }), '<p>&lt;b&gt;&amp;&quot;</p>', 'escape');
eq(fill('<p>{missing}</p>', {}), '<p></p>', 'missing → empty');

// 2. dotted paths.
eq(fill('{user.name}', { user: { name: 'Lin' } }), 'Lin', 'dotted path');
eq(fill('{a.b.c}', { a: { b: {} } }), '', 'missing deep path → empty');

// 3. if / else with truthiness (empty array/string/0 are falsy, like Go).
eq(fill('{if admin}yes{else}no{end}', { admin: true }), 'yes', 'if true');
eq(fill('{if admin}yes{else}no{end}', { admin: false }), 'no', 'if false');
eq(fill('{if items}has{else}empty{end}', { items: [] }), 'empty', 'empty array falsy');
eq(fill('{if items}has{else}empty{end}', { items: [1] }), 'has', 'nonempty array truthy');
eq(fill('{if name}{else}anon{end}', { name: '' }), 'anon', 'empty string falsy');
eq(fill('{if n}nz{end}', { n: 0 }), '', 'zero falsy');

// 4. negation and comparisons.
eq(fill('{if !done}todo{end}', { done: false }), 'todo', 'negation');
eq(fill('{if count > 2}many{end}', { count: 5 }), 'many', 'gt');
eq(fill('{if count >= 5}ok{end}', { count: 5 }), 'ok', 'gte');
eq(fill('{if role == "admin"}A{end}', { role: 'admin' }), 'A', 'string eq');
eq(fill('{if role != "admin"}U{end}', { role: 'user' }), 'U', 'string neq');

// 5. for loops, including nesting and escaping of loop values.
eq(fill('{for t in tags}<i>{t}</i>{end}', { tags: ['a', 'b'] }), '<i>a</i><i>b</i>', 'for');
eq(fill('{for u in users}{u.name},{end}', { users: [{ name: 'A' }, { name: 'B' }] }), 'A,B,', 'for dotted');
eq(fill('{for u in users}{if u.on}{u.name} {end}{end}',
  { users: [{ name: 'A', on: true }, { name: 'B', on: false }, { name: 'C', on: true }] }),
  'A C ', 'for + if');
eq(fill('{for r in rows}{for c in r}{c}{end}|{end}', { rows: [[1, 2], [3]] }), '12|3|', 'nested for');
eq(fill('{for t in tags}<i>{t}</i>{end}', { tags: [] }), '', 'empty for renders nothing');

// 6. literal text and braces-as-holes are untouched/handled; outer scope visible in loop.
eq(fill('hi {n}! {for x in xs}{n}{x}{end}', { n: '#', xs: ['a'] }), 'hi #! #a', 'outer scope in loop');

console.log('ok - ' + pass + ' assertions passed');

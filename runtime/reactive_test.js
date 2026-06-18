// Tests for the client reactivity core (Brick 4, docs/REACTIVITY.md) in
// fa-runtime.js: the extended expression evaluator (arithmetic / boolean / parens,
// matching internal/codegen/expr.go) and the DOM-free action→binding logic
// (applyAction, bindingText). Run: `node runtime/reactive_test.js`.
'use strict';
var assert = require('assert');
var rt = require('./fa-runtime.js');
var evalExpr = rt.evalExpr, applyAction = rt.applyAction, bindingText = rt.bindingText, computeScope = rt.computeScope, listItems = rt.listItems, runEffects = rt.runEffects;

var pass = 0;
function eq(got, want, msg) {
  assert.deepStrictEqual(got, want, msg + '\n  got:  ' + JSON.stringify(got) + '\n  want: ' + JSON.stringify(want));
  pass++;
}

// 1. arithmetic with precedence and parens (the gap the old evaluator had).
eq(evalExpr('count + 1', { count: 4 }), 5, 'add');
eq(evalExpr('count - 2', { count: 4 }), 2, 'sub');
eq(evalExpr('2 + 3 * 4', {}), 14, 'precedence: * before +');
eq(evalExpr('(2 + 3) * 4', {}), 20, 'parens override precedence');
eq(evalExpr('10 % 3', {}), 1, 'mod');
eq(evalExpr('-count', { count: 7 }), -7, 'unary minus');

// 2. boolean / negation / comparison, with Go-style truthiness and short-circuit.
eq(evalExpr('!liked', { liked: false }), true, 'negate');
eq(evalExpr('count > 3 && count < 10', { count: 5 }), true, 'and');
eq(evalExpr('count > 100 || count == 5', { count: 5 }), true, 'or');
eq(evalExpr('count == 0', { count: 0 }), true, 'eq splits, not assignment');

// 3. string concat falls back when a side is non-numeric.
eq(evalExpr('name + "!"', { name: 'hi' }), 'hi!', 'string concat');

// 4. applyAction mutates the signal store; later assignments see earlier ones.
var s = { count: 0, liked: false };
applyAction(s, { name: 'like', mutations: [
  { target: 'liked', expr: '!liked' },
  { target: 'count', expr: 'count + 1' }
] });
eq(s, { count: 1, liked: true }, 'applyAction runs assignments in order');

// 5. a second invocation accumulates (no hidden reset).
applyAction(s, { name: 'bump', mutations: [{ target: 'count', expr: 'count + 1' }] });
eq(s.count, 2, 'state accumulates across actions');

// 6. bindingText computes each bound node's text from the current signals.
eq(bindingText({ count: 2, liked: true }, [
  { id: 'b0', expr: 'count', node: 'text' },
  { id: 'b1', expr: 'count + 1', node: 'text' }
]), { b0: '2', b1: '3' }, 'bindingText evaluates each binding');

// 7. the full loop: a counter click increments the displayed count, zero round-trip.
var counter = { count: 0 };
var bump = { name: 'bump', mutations: [{ target: 'count', expr: 'count + 1' }] };
var binds = [{ id: 'b0', expr: 'count', node: 'text' }];
applyAction(counter, bump);
applyAction(counter, bump);
applyAction(counter, bump);
eq(bindingText(counter, binds), { b0: '3' }, 'three clicks → "3" with no server involved');

// 8. derived values (Brick 5): computed over the signal store, in order; a poll's
// total recomputes from votes with no round-trip.
var poll = { yes: 0, no: 0 };
var derived = [{ name: 'total', expr: 'yes + no' }];
applyAction(poll, { name: 'voteYes', mutations: [{ target: 'yes', expr: 'yes + 1' }] });
applyAction(poll, { name: 'voteNo', mutations: [{ target: 'no', expr: 'no + 1' }] });
applyAction(poll, { name: 'voteYes', mutations: [{ target: 'yes', expr: 'yes + 1' }] });
eq(computeScope(poll, derived).total, 3, 'derived total = yes + no recomputes');
eq(bindingText(computeScope(poll, derived), [{ id: 'b0', expr: 'total' }]), { b0: '3' }, 'binding reads derived');

// 9. chained derived: a later derived sees an earlier one.
eq(computeScope({ n: 2 }, [{ name: 'doubled', expr: 'n * 2' }, { name: 'quad', expr: 'doubled * 2' }]).quad, 8, 'chained derived');

// 10. array literals and list append in the evaluator (Brick 6).
eq(evalExpr('[1, 2, 3]', {}), [1, 2, 3], 'array literal');
eq(evalExpr('items + ["x"]', { items: ['a'] }), ['a', 'x'], 'list append via +');

// 11. listItems renders desired [{key, html}] for a list signal, keyed by id else
// index — the DOM-free core of keyed reconciliation.
var list = { signal: 'items', var: 'item', item: '<li>{item}</li>' };
eq(listItems(list, { items: ['Coffee', 'Tea'] }),
  [{ key: '0', html: '<li>Coffee</li>' }, { key: '1', html: '<li>Tea</li>' }],
  'listItems by index');
var keyed = { signal: 'rows', var: 'r', item: '<li>{r.name}</li>' };
eq(listItems(keyed, { rows: [{ id: 7, name: 'A' }, { id: 9, name: 'B' }] }),
  [{ key: '7', html: '<li>A</li>' }, { key: '9', html: '<li>B</li>' }],
  'listItems keyed by item.id');

// 12. effects (Brick 7): an effect fires when its dep changed, runs its action
// once, and effects do not re-trigger one another (no loop).
var m = {
  actions: [
    { name: 'bump', mutations: [{ target: 'count', expr: 'count + 1' }] },
    { name: 'record', mutations: [{ target: 'history', expr: 'history + [count]' }] }
  ],
  effects: [{ deps: ['count'], action: 'record' }]
};
var sig = { count: 0, history: [] };
var before = { count: 0, history: [] };
applyAction(sig, m.actions[0]);   // bump → count = 1
runEffects(m, sig, before);       // count changed → record fires once
eq(sig.history, [1], 'effect fires on dep change and appends');

// dep unchanged → effect does not fire.
var sig2 = { count: 5, history: [] }, before2 = { count: 5, history: [] };
runEffects(m, sig2, before2);
eq(sig2.history, [], 'effect does not fire when its dep is unchanged');

console.log('ok - ' + pass + ' assertions passed');

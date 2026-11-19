'use strict';
// The model picker shows a billing multiplier next to every model. The gateway
// publishes it in the upstream's own spelling, which uses a trailing "x"
// ("0.01x", "1x") as often as a leading one ("x0.11 credits"). A regex that
// only accepts the leading form silently degrades every real value to
// "倍率未知", which is exactly the bug this test locks down.
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/model-picker.js', 'utf8');

function load() {
  const body = source.slice(source.indexOf('  function rate('), source.indexOf('  function mount('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.rate = rate; this.rateText = rateText;', context);
  return context;
}

test('trailing-x multipliers parse', () => {
  const {rate} = load();
  for (const [raw, want] of [['0.01x', 0.01], ['1x', 1], ['0.65x', 0.65], ['1.48x', 1.48], ['0.08x', 0.08]]) {
    assert.equal(rate(raw), want, `rate(${raw})`);
  }
});

test('leading-x and credits suffix parse', () => {
  const {rate} = load();
  for (const [raw, want] of [['x0.11', 0.11], ['x0.11 credits', 0.11], ['0.05 credits', 0.05], ['×0.3', 0.3]]) {
    assert.equal(rate(raw), want, `rate(${raw})`);
  }
});

test('free is zero, not unknown', () => {
  const {rate, rateText} = load();
  assert.equal(rate('免费'), 0);
  assert.equal(rate('free'), 0);
  assert.equal(rateText('免费'), '免费');
});

test('a missing or unparseable value stays unknown', () => {
  const {rate, rateText} = load();
  for (const raw of [null, undefined, '', 'n/a', 'unlimited']) {
    assert.equal(rate(raw), null, `rate(${raw})`);
  }
  assert.equal(rateText(null), '倍率未知');
});

test('rateText renders the parsed value', () => {
  const {rateText} = load();
  assert.equal(rateText('0.01x'), 'x0.01');
  assert.equal(rateText('1x'), 'x1');
});

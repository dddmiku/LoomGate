'use strict';
// 合成测试：倍率解析对 0（免费）、x0.11 credits、未知三种形态的判定。
// 只抽取纯函数段，不驱动 DOM、不发请求。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/model-picker.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function rate('), source.indexOf('  function matches('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.rate = rate; this.rateText = rateText;', context);
  return context;
}
test('zero and x0.00 parse as free, not unknown', () => {
  const {rate, rateText} = seam();
  assert.equal(rate(0), 0);
  assert.equal(rateText(0), '免费');
  assert.equal(rate('x0.00'), 0);
  assert.equal(rateText('x0.00'), '免费');
});
test('real multiplier strings with credits suffix parse exactly', () => {
  const {rate, rateText} = seam();
  assert.equal(rate('x0.11 credits'), 0.11);
  assert.equal(rateText('x0.11 credits'), 'x0.11');
  assert.equal(rate('×1.25'), 1.25);
  assert.equal(rateText('×1.25'), 'x1.25');
});
test('missing or unparseable multipliers stay unknown and are never invented', () => {
  const {rate, rateText} = seam();
  assert.equal(rate(null), null);
  assert.equal(rate(''), null);
  assert.equal(rate('n/a'), null);
  assert.equal(rateText('n/a'), '倍率未知');
});

'use strict';
// 点「运行」或「检查更新」只该重取数据并重画当前页，不该把整页换成加载态。
// 旧实现清空 providers 再走 navigate()，视觉上等同于刷新了整个页面。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

function reloadBody() {
  const start = src.indexOf('async function reload()');
  const end = src.indexOf('\n  async function action(', start);
  assert.ok(start > 0 && end > start, 'reload() must exist');
  return src.slice(start, end);
}

test('reload does not navigate away from the current page', () => {
  const body = reloadBody();
  assert.equal(body.includes('await navigate()'), false, 'reload must not re-run navigate()');
});

test('reload does not clear the provider cache', () => {
  const body = reloadBody();
  assert.equal(body.includes('state.providers=null'), false, 'reload must keep the provider cache');
});

test('reload re-renders the current route in place', () => {
  const body = reloadBody();
  assert.match(body, /await providers\(\)/, 'reload must refresh providers');
  assert.match(body, /renderers\[state\.route\]\(\)/, 'reload must re-run the current renderer');
});

test('run and update-check go through the in-place reload', () => {
  assert.match(src, /name==='update-check'.*await reload\(\)/, 'update-check must reload in place');
});

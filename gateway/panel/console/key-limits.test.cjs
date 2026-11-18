'use strict';
// 0 在 keylimit 里是「不限」的显式语义（只有 >0 的字段生效）。列表里把
// "requests_per_minute:0, max_concurrent:0" 原样打印成 "0/分 · 并发 0" 会被
// 读成「一个请求都不放行」——与真实行为相反。这里执行真实 limitSummary()。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function limitSummary('), source.indexOf('  async function keyPage('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.limitSummary = limitSummary;', context);
  return context.limitSummary;
}
test('all-zero limits read as unlimited, not as a total block', () => {
  const summary = seam();
  const text = summary({requests_per_minute: 0, max_concurrent: 0, queue_timeout_seconds: 0});
  assert.equal(text, '不限流（每次都可立即执行）');
  assert.equal(text.includes('/分'), false);
});
test('no limits configured is stated as inheriting the service default', () => {
  const summary = seam();
  assert.equal(summary(null), '未配置（沿用服务默认）');
  assert.equal(summary(undefined), '未配置（沿用服务默认）');
});
test('partial limits only mention the fields that are actually enforced', () => {
  const summary = seam();
  assert.equal(summary({requests_per_minute: 60, max_concurrent: 0}), '每分钟 60 次');
  assert.equal(summary({max_concurrent: 4, queue_timeout_seconds: 10}), '并发 4 · 排队 10s');
  assert.equal(summary({requests_per_minute: 30, max_concurrent: 2, queue_timeout_seconds: 5}), '每分钟 30 次 · 并发 2 · 排队 5s');
});

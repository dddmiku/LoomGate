'use strict';
// 合成测试：confirmRun 的账号范围不得被放大。组级执行（account_ids 数组）必须
// 严格使用列出的账号；单条任务用 account_id；无范围时直接取消，不开确认框。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function drive(task) {
  const code = source.slice(source.indexOf('  function confirmRun('), source.indexOf('  // legacy_enabled 是'));
  const captured = {req: null, opened: false, toast: null, submitted: false};
  const context = {
    state: {taskAccount: 'scope-account-default', taskProvider: 'workbuddy', pending: new Set()},
    sessionStorage: {getItem: () => null, setItem() {}},
    toast: (m) => { captured.toast = m; },
    esc: (x) => String(x ?? ''),
    providerName: (x) => x,
    button: () => '',
    openDialog: () => { captured.opened = true; },
    $: () => ({set onsubmit(fn) { captured.submit = fn; }, get onsubmit() { return captured.submit; }}),
    mutate: (key, path, body) => { captured.req = body; },
    APIError: class extends Error {},
    location: {hash: ''},
    Map, Set, Object, Array, JSON, Number, String, Promise, console,
  };
  vm.createContext(context);
  vm.runInContext(code + '\nthis.confirmRun = confirmRun;', context);
  context.confirmRun(task);
  if (captured.submit) { captured.submitted = true; captured.submit({preventDefault() {}}); }
  return captured;
}
test('group execution keeps exactly the listed accounts', () => {
  const r = drive({provider_id: 'workbuddy', key: 'balance', account_ids: ['a-fixture', 'b-fixture']});
  assert.ok(r.opened, 'confirmation dialog should open for a valid group');
  assert.deepEqual([...r.req.account_ids], ['a-fixture', 'b-fixture']);
  assert.equal(r.req.task_key, 'balance');
});
test('single task execution uses its own account only', () => {
  const r = drive({provider_id: 'workbuddy', key: 'checkin', account_id: 'a-fixture'});
  assert.deepEqual([...r.req.account_ids], ['a-fixture']);
});
// 账号范围选择器已从任务页移除：任务页按分区展示，没有「当前选中账号」这种状态。
// 无任务参数的调用必须取消，绝不能退化成「全部已启用账号」（那是范围放大）。
test('a run without an explicit account range is cancelled', () => {
  const r = drive(null);
  assert.equal(r.opened, false, 'no confirmation dialog without an explicit range');
  assert.ok(/没有可执行的账号范围/.test(r.toast || ''));
});
test('a task without any account range is cancelled, never widened to all', () => {
  const r = drive({provider_id: 'workbuddy', key: 'balance'});
  assert.equal(r.opened, false, 'no confirmation dialog without an explicit range');
  assert.ok(/没有可执行的账号范围/.test(r.toast || ''));
});

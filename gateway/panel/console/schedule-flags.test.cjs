'use strict';
// cleanSchedule 是「编辑计划」提交前的唯一投影。它必须保留 legacy_enabled：
// 这个字段记录迁移来源（旧配置原本是否启用），漏传就等于编辑一次时间就把审计
// 事实抹掉。这里用源码切片 + vm 执行真实函数，不驱动 DOM。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function cleanSchedule('), source.indexOf('  async function verifySchedule('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.cleanSchedule = cleanSchedule;', context);
  return context;
}
test('legacy source flag survives the edit projection', () => {
  const {cleanSchedule} = seam();
  const out = cleanSchedule({id: 'legacy-checkin', provider_id: 'workbuddy', task_key: 'checkin', account_ids: ['1001'], hours: [9], minute: 0, timezone: 'Asia/Shanghai', enabled: false, once_per_day: true, legacy_enabled: true});
  assert.equal(out.legacy_enabled, true);
  assert.equal(JSON.stringify(out.account_ids), JSON.stringify(['1001']));
  assert.equal(out.task_key, 'checkin');
});
test('plans without a legacy origin stay without one', () => {
  const {cleanSchedule} = seam();
  const out = cleanSchedule({id: 'user-plan', provider_id: 'trae', task_key: 'balance', hours: [8], minute: 30, timezone: 'UTC', enabled: true});
  assert.equal(out.legacy_enabled, undefined);
  assert.equal(JSON.stringify(out.account_ids), '[]');
  assert.equal(out.once_per_day, undefined);
});

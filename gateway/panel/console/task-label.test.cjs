'use strict';
// 旧 config 导入的计划用「模块/」前缀表达「该模块当前的全部任务」（growth/
// 等，见 cmd/server/workbench.go 的 legacyTaskKey）。计划页必须把它渲染成
// 「成长任务（全部任务）」，而不是「全部任务」——后者会把「成长中心这一条
// 流水线」误读成「所有服务的所有任务」，把执行范围描述得比真实的大得多。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam(tasks) {
  const body = source.slice(source.indexOf('  function taskLabel('), source.indexOf('  const routes ='));
  const context = {state: {taskNames: new Map(), tasks: tasks || []}};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.taskLabel = taskLabel;', context);
  return context.taskLabel;
}
test('module-prefix plans name their module, not "全部任务"', () => {
  const label = seam();
  assert.equal(label('workbuddy', 'cn', 'growth/'), '成长任务（全部任务）');
  assert.equal(label('workbuddy', 'cn', 'travel/'), 'Buddy 旅行（全部任务）');
  assert.equal(label('workbuddy', 'cn', 'redeem/'), '连登奖励兑换（全部任务）');
  assert.equal(label('workbuddy', 'cn', 'makeup/'), '日历补登（全部任务）');
});
test('a discovered task name wins over the static module name', () => {
  const label = seam([{provider_id: 'workbuddy', key: 'growth/accept/x', name: '合成成长任务'}]);
  assert.equal(label('workbuddy', 'cn', 'growth/'), '合成成长任务（全部任务）');
});
test('bare wildcard still reads 全部任务 and exact keys keep their names', () => {
  const label = seam();
  assert.equal(label('workbuddy', '', '*'), '全部任务');
  assert.equal(label('workbuddy', '', ''), '全部任务');
  assert.equal(label('workbuddy', 'cn', 'checkin'), '每日签到');
  assert.equal(label('workbuddy', 'cn', 'keepalive'), '账号会话保活');
});
test('an unknown module prefix is shown as itself, never as 全部任务', () => {
  const label = seam();
  assert.equal(label('workbuddy', 'cn', 'future-module/'), 'future-module（全部任务）');
});

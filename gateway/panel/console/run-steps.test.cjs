'use strict';
// 迁移计划用「模块/」前缀一次覆盖多个真实任务（growth/ → growth/accept/<码> +
// growth/<码>）。执行详情若只显示「阶段 / 账号 / 结果」，运维分不清每一步属于
// 哪个任务，失败时也无从核对。这里执行真实的 runStepColumns / runStepCells。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function runStepColumns('), source.indexOf('  function runTimeline('));
  const context = {labels: {inspect: '检查', execute: '执行'}, taskErrors: {operation_unconfirmed: '操作结果未确认'}, discoveryIssueLabels: {module_discovery_failed: '模块状态读取失败'}, esc: (x) => String(x).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'), badge: (v) => '[' + v + ']', table: (headers, rows) => JSON.stringify({headers, rows})};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.runStepColumns = runStepColumns; this.runStepCells = runStepCells; this.runDiscoveryIssues = runDiscoveryIssues;', context);
  return context;
}
test('a single-task run keeps the compact three-column table', () => {
  const {runStepColumns} = seam();
  assert.equal(JSON.stringify(runStepColumns([{task_key: 'checkin'}, {task_key: 'checkin'}])), JSON.stringify(['阶段', '账号', '结果']));
  assert.equal(JSON.stringify(runStepColumns([])), JSON.stringify(['阶段', '账号', '结果']));
  assert.equal(JSON.stringify(runStepColumns(undefined)), JSON.stringify(['阶段', '账号', '结果']));
});
test('a multi-task run gains a 任务 column so steps are attributable', () => {
  const {runStepColumns} = seam();
  assert.equal(JSON.stringify(runStepColumns([{task_key: 'growth/accept/x'}, {task_key: 'growth/x'}])), JSON.stringify(['任务', '阶段', '账号', '结果']));
});
test('cells carry the task key only when the column exists', () => {
  const {runStepCells} = seam();
  const step = {task_key: 'growth/accept/x', stage: 'execute', account_id: 'cn-1', status: 'succeeded'};
  const withTask = runStepCells(step, true);
  assert.equal(withTask.length, 4);
  assert.equal(withTask[0], '<code>growth/accept/x</code>');
  const compact = runStepCells(step, false);
  assert.equal(compact.length, 3);
  assert.equal(compact[0], '执行');
});
test('step summary is visible and escaped beside stable error details', () => {
  const {runStepCells} = seam();
  const cells = runStepCells({stage:'execute',account_id:'cn-1',status:'uncertain',error_code:'operation_unconfirmed',summary:'<strong>先核验</strong>'}, false);
  assert.match(cells[2], /操作结果未确认/);
  assert.match(cells[2], /&lt;strong&gt;先核验&lt;\/strong&gt;/);
});
test('discovery failures are attributable to an account and module without raw provider text', () => {
  const {runDiscoveryIssues} = seam();
  const html = runDiscoveryIssues([{account_id:'cn-1',module_id:'activities',error_code:'module_discovery_failed'}]);
  assert.match(html, /cn-1/);
  assert.match(html, /activities/);
  assert.match(html, /模块状态读取失败/);
});

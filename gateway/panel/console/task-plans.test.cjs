'use strict';
// 任务页契约：行来自随包计划（workbench.schedules），状态列就是定时启用/暂停
// 开关；目录只补充账号状态与目录专用任务，绝不决定计划是否存在。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');

function seam() {
  const body = source.slice(source.indexOf('  function planKeyMatches('), source.indexOf('  function paintTaskList('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.planKeyMatches = planKeyMatches;this.groupsForPlan = groupsForPlan;', context);
  return context;
}

test('module plans cover every discovered task under their prefix', () => {
  const ctx = seam();
  assert.equal(ctx.planKeyMatches('growth/', 'growth/accept/daily-1'), true);
  assert.equal(ctx.planKeyMatches('growth/', 'growth/daily-1'), true);
  assert.equal(ctx.planKeyMatches('growth/', 'balance'), false);
  assert.equal(ctx.planKeyMatches('checkin', 'checkin'), true);
  assert.equal(ctx.planKeyMatches('','anything'), true);
  assert.equal(ctx.planKeyMatches('*','anything'), true);
});

test('groupsForPlan never crosses providers', () => {
  const ctx = seam();
  ctx.state = {
    taskGroups: [
      {provider_id: 'workbuddy', key: 'balance'},
      {provider_id: 'trae', key: 'balance'},
      {provider_id: 'workbuddy', key: 'growth/daily-1'},
    ],
  };
  const groups = ctx.groupsForPlan({provider_id: 'workbuddy', task_key: 'growth/'});
  assert.equal(groups.length, 1);
  assert.equal(groups[0].key, 'growth/daily-1');
});

test('the task row status cell is a verified switch, not a badge', () => {
  // 状态列必须是 role=switch 的按钮，并由 toggle-schedule 动作写回服务端；
  // 旧版的「已启用」徽标 + 行末「停用」按钮组合已被用户明确要求替换。
  const body = source.slice(source.indexOf('  function paintTaskList('), source.indexOf('  const taskErrors='));
  assert.match(body, /role="switch"/);
  assert.match(body, /aria-checked="\$\{plan\.enabled\?'true':'false'\}"/);
  assert.match(body, /data-action="toggle-schedule"/);
  assert.doesNotMatch(body, /badge\(sched\.enabled/);
});

test('a plan without catalog rows still renders, and catalog-only rows stay runnable', () => {
  const body = source.slice(source.indexOf('  function paintTaskList('), source.indexOf('  const taskErrors='));
  assert.match(body, /目录发现/);
  assert.match(body, /无随包定时/);
  // 空目录不再让整页消失：只要还有随包计划就必须渲染。
  assert.doesNotMatch(body, /if\(!state\.taskGroups\.length\)/);
});

test('running a plan uses exactly the catalog accounts, never all accounts', () => {
  const body = source.slice(source.indexOf('  function planRunTask('), source.indexOf('  // Renders one run exactly like the old detail dialog'));
  const context = {taskLabel: (p, r, k) => k, Set, Object, Array};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.planRunTask = planRunTask;', context);
  const task = context.planRunTask({
    plan: {provider_id: 'workbuddy', task_key: 'checkin'},
    groups: [{accounts: [{account_id: 'a', manual_only: false}, {account_id: 'b', manual_only: true}]}],
  });
  assert.deepEqual([...task.account_ids], ['a']);
  assert.equal(task.key, 'checkin');
  assert.equal(context.planRunTask({plan: null, groups: []}), null);
});

test('a saved plan scope wins over the catalog, and an unread catalog never widens the run', () => {
  // 计划自己保存过账号范围时以它为准：目录后来新增/停用账号不能悄悄扩大范围。
  const body = source.slice(source.indexOf('  function planRunTask('), source.indexOf('  // Renders one run exactly like the old detail dialog'));
  const context = {taskLabel: (p, r, k) => k, Set, Object, Array};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.planRunTask = planRunTask;', context);
  const saved = context.planRunTask({
    plan: {provider_id: 'workbuddy', task_key: 'checkin', account_ids: ['pinned']},
    groups: [{accounts: [{account_id: 'other', manual_only: false}]}],
  });
  assert.deepEqual([...saved.account_ids], ['pinned']);
  // 目录本轮没读到时范围为空——空不等于全部，confirmRun 会拒绝发起请求，
  // 而不是拿一次不可见执行去赌。
  const unread = context.planRunTask({plan: {provider_id: 'workbuddy', task_key: 'checkin'}, groups: []});
  assert.deepEqual([...unread.account_ids], []);
});

// 首页必须回到旧版结构：指标卡 + 账号池/积分两块面板。用户明确否定了
// 「需要关注 / 最近执行」那种时间线，因为看不懂也找不到重点。
test('the overview renders legacy-style metric cards and two panels', () => {
  const start = source.indexOf('  async function overview(');
  const end = source.indexOf('  async function services(', start);
  assert.ok(start > 0 && end > start, 'overview must exist');
  const body = source.slice(start, end);
  assert.match(body, /metricCard\(/, 'overview needs metric cards');
  assert.match(body, /账号池/);
  assert.match(body, /积分/);
  assert.doesNotMatch(body, /需要关注/, 'the attention timeline was rejected by the user');
  assert.doesNotMatch(body, /最近执行/, 'the recent-runs timeline was rejected by the user');
});

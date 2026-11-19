'use strict';
// 「日志」按钮必须显示任务自己的输出文本，而不是结构化执行步骤表。
// 旧版行为：点日志 → 看到最近一次运行的文本行。回归时容易又退回详情表。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const path = require('node:path');

const appSrc = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

test('the log dialog requests the task output endpoint', () => {
  assert.match(appSrc, /api\('tasks\/'\+encodeURIComponent\(key\)\+'\/log'/, 'log dialog must call /tasks/<key>/log');
});

test('the log dialog renders lines as text, not a step table', () => {
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('// A plan row runs exactly the accounts', start);
  const body = appSrc.slice(start, end);
  assert.ok(start > 0 && end > start, 'openPlanLog must exist');
  assert.match(body, /class="task-log-output"/, 'log lines need a text container');
  assert.match(body, /lines\.join\("\\n"\)/, 'log lines must be joined as text');
  assert.equal(body.includes('runStepColumns'), false, 'the log dialog must not render the step table');
  assert.equal(body.includes('showTaskRunDetail'), false, 'the log dialog must not fall back to the run detail table');
});

test('a running task keeps refreshing its log', () => {
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('// A plan row runs exactly the accounts', start);
  const body = appSrc.slice(start, end);
  assert.match(body, /data\.running/, 'must react to the running flag');
  assert.match(body, /setTimeout\(\(\)=>load\(\)/, 'a running task must re-read the log');
});

test('the task page no longer renders the execution history block', () => {
  assert.equal(appSrc.includes('id="run-journal"'), false, 'the task page execution-history block was removed');
});

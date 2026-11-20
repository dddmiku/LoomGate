'use strict';
// 「日志」按钮必须显示任务自己的输出文本，而不是结构化执行步骤表。
// 旧版行为：点日志 → 看到最近一次运行的文本行。回归时容易又退回详情表。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const path = require('node:path');

const appSrc = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

test('the log dialog requests the task output endpoint', () => {
  assert.match(appSrc, /api\('task\/log\?key='\+encodeURIComponent\(key\)\+'\&provider='\+encodeURIComponent\(provider\)/,
    'log dialog must scope reads to provider + task key');
});

test('the log dialog renders lines as text, not a step table', () => {
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('function planRunTask(', start);
  const body = appSrc.slice(start, end);
  assert.ok(start > 0 && end > start, 'openPlanLog must exist');
  assert.match(body, /class="task-log-output"/, 'log lines need a text container');
  assert.match(body, /lines\.join\("\\n"\)/, 'log lines must be joined as text');
  assert.equal(body.includes('runStepColumns'), false, 'the log dialog must not render the step table');
  assert.equal(body.includes('showTaskRunDetail'), false, 'the log dialog must not fall back to the run detail table');
});

test('the log dialog matches the old version: title, last-run subtitle, refresh', () => {
  // 旧版形态：标题「任务日志 · <任务名>」、副标题「运行中… / 最近一次 MM-DD HH:MM」、
  // 顶栏一个刷新按钮，正文是一整块文本。用户截图就是按这个对标的。
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('function planRunTask(', start);
  const body = appSrc.slice(start, end);
  assert.match(body, /`任务日志 · \$\{taskLabel\(/, 'dialog title must be 任务日志 · <task label>');
  assert.match(body, /'最近一次 '\+shortTime\(data\.endedAt\)/, 'subtitle must read 最近一次 <time>');
  assert.match(body, /data-action="task-log-refresh"/, 'header needs a refresh button');
  assert.match(body, /class="task-log-output"[^`]*role="log"/, 'text block is the log region');
});

test('a running task keeps refreshing its log', () => {
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('function planRunTask(', start);
  const body = appSrc.slice(start, end);
  assert.match(body, /data\.running/, 'must react to the running flag');
  assert.match(body, /setTimeout\(\(\)=>load\(\)/, 'a running task must re-read the log');
});

test('late responses from a previous log dialog cannot overwrite a newly opened log', () => {
  const start = appSrc.indexOf('async function openPlanLog(');
  const end = appSrc.indexOf('function planRunTask(', start);
  const body = appSrc.slice(start, end);
  assert.match(appSrc, /taskLogGeneration/, 'dialog generations must invalidate late requests');
  assert.match(body, /state\.taskLogGeneration===generation/, 'each response must prove it still owns the dialog');
  assert.match(body, /if\(!currentDialogOpen\(\)\)return/, 'stale responses must be dropped before painting');
});

test('the task page no longer renders the execution history block', () => {
  assert.equal(appSrc.includes('id="run-journal"'), false, 'the task page execution-history block was removed');
});

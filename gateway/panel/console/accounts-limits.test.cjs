'use strict';
// 账号页契约（用户 2026-11-19 明确要求）：
//   - 模型限流与积分过期必须常显、高亮，不能折叠进 details；
//   - WorkBuddy 必须有 CN / Global 区域筛选；
//   - 「接入进度」历史区块已移除。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');

function seam() {
  const body = source.slice(source.indexOf('  function limitNotices('), source.indexOf('  async function modelPage('));
  const context = {esc: (x) => String(x ?? ''), date: (x) => String(x ?? '')};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.limitNotices = limitNotices;this.limitShortReason = limitShortReason;', context);
  return context;
}

test('model rate limits render inline, never inside a collapsed details element', () => {
  const ctx = seam();
  const html = ctx.limitNotices({
    rateLimitedModels: [
      {model: 'deepseek-v4.1-flash', until: '2026-11-19T05:57:00Z', resetAt: '2026-11-19T05:57:00Z', reason: 'model rate limited (6004)', kind: 'rate'},
    ],
  });
  assert.match(html, /data-model-limit="deepseek-v4\.1-flash"/);
  assert.match(html, /模型限流/);
  assert.doesNotMatch(html, /<details/);
  assert.doesNotMatch(html, /summary/);
});

test('no limits means no empty block, and unavailable models are distinguished', () => {
  const ctx = seam();
  assert.equal(ctx.limitNotices({rateLimitedModels: []}), '');
  assert.equal(ctx.limitNotices(null), '');
  const html = ctx.limitNotices({rateLimitedModels: [{model: 'gone', reason: 'no such backend (11102)', kind: 'unavailable'}]});
  assert.match(html, /unavailable/);
  assert.match(html, /上游已无此模型/);
});

test('expiring credits are a highlighted inline note', () => {
  const body = source.slice(source.indexOf('        const balance=typeof credit?.remain'), source.indexOf('        const sessionText='));
  assert.match(body, /class="credit-expiring"/);
  assert.match(body, /data-expiring=/);
  assert.match(body, /即将过期/);
});

test('WorkBuddy exposes a CN / Global realm filter with counts', () => {
  const body = source.slice(source.indexOf('    const drawRealms=()=>{'), source.indexOf('    drawRealms();draw();'));
  assert.match(body, /\['','全区域',rows.length\],\['cn','CN',counts.cn\],\['global','Global',counts.global\]/);
  assert.match(body, /data-realm-choice/);
  assert.match(body, /selected!=='workbuddy'/);
});

test('the connection progress history section is gone', () => {
  const connections = fs.readFileSync(__dirname + '/connections.js', 'utf8');
  // 仅允许注释里说明「已移除」，不允许再渲染标题区块。
  assert.doesNotMatch(connections, /<h2>接入进度<\/h2>/);
  assert.doesNotMatch(source, /LoomConnections\.list\(/);
});

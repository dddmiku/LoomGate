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
function creditCell(credit, querying = true) {
  const pending = source.slice(source.indexOf('  const creditIsPending'), source.indexOf('  const creditAmount'));
  const cell = source.slice(source.indexOf('  function accountCreditCell('), source.indexOf('  function providerFilter('));
  const context = {esc: String, date: String, number: String};
  vm.createContext(context); vm.runInContext(pending + cell + '\nthis.render = accountCreditCell;', context);
  return context.render(credit, querying);
}

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
  const body = creditCell({remain: 30, expiring: 12});
  assert.match(body, /class="credit-expiring"/);
  assert.match(body, /data-expiring=/);
  assert.match(body, /即将过期/);
});

test('credit display distinguishes pending, failure, and upstream unknown cost', () => {
  assert.match(creditCell({error_code: 'credit_pending'}), /查询中/);
  assert.doesNotMatch(creditCell({error_code: 'credit_pending'}, false), /查询中/);
  assert.match(creditCell({error_code: 'upstream_not_reported'}), /上游未返回/);
  assert.match(creditCell({remain: 0}), />0<.*临期积分未知/);
  assert.match(creditCell({remain:0,error_code:'autoclaw_expiring_unavailable'}), />0<.*临期积分未知/);
  assert.doesNotMatch(creditCell({remain:0,error_code:'autoclaw_expiring_unavailable'}), /上次余额/);
  assert.match(creditCell({remain:5,error_code:'autoclaw_credit_unavailable'}), />5<.*刷新失败，显示上次余额/);
});

test('request charge summary preserves known zero and labels partial observations', () => {
  const code = source.slice(source.indexOf('  function requestCredit('), source.indexOf('  function requestDetailHtml('));
  const context={esc:String, number:String};
  vm.createContext(context);vm.runInContext(code + '\nthis.render = requestCredit;', context);
  assert.equal(context.render({credit:0,missing_credit_attempts:0}), '0');
  assert.match(context.render({credit:0,missing_credit_attempts:1}), /^0 .*部分上报/);
  assert.match(context.render({credit:2.125,missing_credit_attempts:3},true), /^2\.125 .*已知小计 · 3 次未上报/);
  assert.match(context.render({credit:null,upstream_started:true}), /上游未上报扣费.*未按 token 估算/);
  assert.match(context.render({credit:null,upstream_started:false}), /未调用上游/);
});

test('request attempt costs are loaded on demand, with zero and missing values kept distinct', async () => {
  const code=source.slice(source.indexOf('  function requestAttemptCredits('),source.indexOf('  function requestDetailHtml('));
  const reads=[], state={version:1,route:'logs'};
  let resolve;
  const context={state,esc:String,api(path){reads.push(path);return new Promise(done=>{resolve=done;});}};
  vm.createContext(context);vm.runInContext(code+'\nthis.load = loadRequestAttemptCredits;',context);
  const node={isConnected:true,innerHTML:'pending',dataset:{}};
  const attempts=[{number:1,credit:0},{number:2,credit:null}];
  const first=context.load(node,'request-1',1);
  resolve({record:{request_id:'request-1',attempts,attempts_truncated:true}});
  await first;
  assert.match(node.innerHTML,/第 1 次：0.*第 2 次：上游未上报.*部分尝试没有留存明细/);
  const second=context.load(node,'request-2',1);
  node.isConnected=false; resolve({record:{request_id:'request-2',attempts:[{number:1,credit:8}]}});
  await second;
  assert.doesNotMatch(node.innerHTML,/次：8/);
  assert.deepEqual(reads,['requests/request-1','requests/request-2']);
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

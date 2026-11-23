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
  vm.createContext(context);vm.runInContext(code+'\nthis.load = loadRequestAttemptDetails;',context);
  const credits={innerHTML:'pending'},accounts={innerHTML:'pending'};
  const node={isConnected:true,dataset:{requestDetail:'request-1'},querySelector(selector){return selector.includes('account')?accounts:credits;}};
  const attempts=[{number:1,credit:0},{number:2,credit:null}];
  const first=context.load(node,'request-1',1);
  resolve({record:{request_id:'request-1',attempts,attempts_truncated:true}});
  await first;
  assert.match(credits.innerHTML,/第 1 次：0.*第 2 次：上游未上报.*部分尝试没有留存明细/);
  node.dataset.requestDetail='request-2';
  const second=context.load(node,'request-2',1);
  node.isConnected=false; resolve({record:{request_id:'request-2',attempts:[{number:1,credit:8}]}});
  await second;
  assert.doesNotMatch(credits.innerHTML,/次：8/);
  assert.deepEqual(reads,['requests/request-1','requests/request-2']);
});

function requestDetailsSeam(api=async()=>({})) {
  const code=source.slice(source.indexOf('  function requestCredit('),source.indexOf('  function requestLogPath('));
  const context={state:{version:1,route:'logs'},api,esc:x=>String(x??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),number:String,date:String,protocolName:String,statusTone:String,badge:(_,text)=>String(text)};
  vm.createContext(context);vm.runInContext(code+'\nthis.accounts = requestAttemptAccounts;this.load = loadRequestAttemptDetails;this.detail = requestDetailHtml;',context);
  return context;
}

test('used accounts come from each real attempt, including retries and missing history', () => {
  const ctx=requestDetailsSeam();
  const html=ctx.accounts({upstream_started:true,attempt_count:3,attempts:[{number:1,account_id:'fixture-first'},{number:2,account_id:'fixture-second'},{number:3}]});
  assert.match(html,/第 1 次：<code>fixture-first<\/code>.*第 2 次：<code>fixture-second<\/code>.*第 3 次：账号信息未保留/);
  assert.match(ctx.accounts({attempts:[{number:4,account_id:'fixture-last'}],attempt_count:5}),/部分尝试没有留存账号明细/);
  assert.match(ctx.accounts({attempts:[{number:1}],attempts_truncated:true}),/部分尝试没有留存账号明细/);
  assert.equal(ctx.accounts({attempts:[]}), '账号信息未保留');
  assert.equal(ctx.accounts({upstream_started:false,decisions:[{account_id:'only-selected'}]}), '未调用上游');
  const detail=ctx.detail({upstream_started:false,request_id:'not-started'});
  assert.match(detail,/<dt>使用账号<\/dt><dd>未调用上游/);
  assert.doesNotMatch(detail,/data-request-attempt-account|data-request-attempt-credit/);
});

test('used account IDs are escaped and historical nicknames are not exposed', () => {
  const ctx=requestDetailsSeam();
  const html=ctx.accounts({attempts:[{number:'<b>',account_id:'<img src=x onerror=alert(1)>',account_name:'private display input'}]});
  assert.match(html,/&lt;b&gt;.*&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.doesNotMatch(html,/<img|<b>|private display input/);
  assert.match(ctx.detail({request_id:'"<x>',upstream_started:true}),/data-request-detail="&quot;&lt;x&gt;"/);
});

test('account and cost share one detail read, with isolated panels and stale responses rejected', async () => {
  const reads=[],pending=new Map();
  const ctx=requestDetailsSeam(path=>{reads.push(path);return new Promise(resolve=>pending.set(path,resolve));});
  const panel=id=>{const accounts={innerHTML:'pending'},credits={innerHTML:'pending'};return {accounts,credits,node:{isConnected:true,dataset:{requestDetail:id},querySelector:s=>s.includes('account')?accounts:credits}};};
  const a=panel('first'),b=panel('second');
  const first=ctx.load(a.node,'first',1),second=ctx.load(b.node,'second',1);
  await ctx.load(a.node,'first',1);
  pending.get('requests/second')({record:{request_id:'second',attempts:[{number:1,account_id:'actual-second',credit:0}]}});await second;
  assert.match(b.accounts.innerHTML,/actual-second/);assert.match(b.credits.innerHTML,/次：0/);
  assert.equal(a.accounts.innerHTML,'pending');
  a.node.dataset.requestDetail='changed';
  pending.get('requests/first')({record:{request_id:'first',attempts:[{number:1,account_id:'actual-first',credit:7}]}});await first;
  assert.equal(a.accounts.innerHTML,'pending');assert.equal(a.credits.innerHTML,'pending');
  const mismatch=panel('mismatch'),m=ctx.load(mismatch.node,'mismatch',1);
  pending.get('requests/mismatch')({record:{request_id:'different',attempts:[{number:1,account_id:'wrong-account',credit:9}]}});await m;
  assert.equal(mismatch.accounts.textContent,'账号信息暂不可读');assert.equal(mismatch.credits.textContent,'逐次花费暂不可读');
  for(const [id,change] of [['left-route',()=>{ctx.state.route='accounts';}],['old-generation',()=>{ctx.state.version=2;}]]) {
    ctx.state.route='logs';ctx.state.version=1;const p=panel(id),read=ctx.load(p.node,id,1);change();
    pending.get('requests/'+id)({record:{request_id:id,attempts:[{number:1,account_id:'stale-account',credit:8}]}});await read;
    assert.equal(p.accounts.innerHTML,'pending');assert.equal(p.credits.innerHTML,'pending');
  }
  assert.deepEqual(reads,['requests/first','requests/second','requests/mismatch','requests/left-route','requests/old-generation']);
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

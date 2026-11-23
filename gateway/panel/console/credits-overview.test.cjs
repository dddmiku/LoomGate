'use strict';
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');

function consoleCredits({route = 'overview', providers = [], accounts = [], nextState, nextProviders} = {}) {
  const state = {version: 1, route, providers, accountProvider: 'autoclaw'};
  const reads = [], timers = [], elements = new Map();
  const elementFor = selector => {
    if (!elements.has(selector)) elements.set(selector, {innerHTML: '', value: '', addEventListener() {}, querySelectorAll() {return [];}});
    return elements.get(selector);
  };
  let currentAccounts = accounts;
  const fn = (from, to) => source.slice(source.indexOf(from), source.indexOf(to, source.indexOf(from)));
  const context = {state, esc: String, number: String, date: String, $: elementFor,
    // This fixture isolates the existing wallet readback loop. The new local
    // activity observer has separate timer/visibility and browser regressions.
    createReadMonitor:()=>({stop(){},setPaused(){}}),
    table: (_, rows) => rows.map(r => r.join('')).join(''), empty: String,
    accountStatus: () => '', providerName: String, installViewContent: value => { elementFor('#view').innerHTML = value; },
    heading: String, button: () => '', notice: String, providerFilter: () => '', bindProviderFilter() {},
    setTimeout(fn, delay) { timers.push({fn, delay}); return timers.length; },
    async api(path) { reads.push(path); return path === 'state' ? (nextState?.() ?? {accounts}) : path === 'workbench/providers' ? {providers: nextProviders?.() ?? providers} : {schedules: []}; },
    async accountsFor(provider, _, onSnapshot) { reads.push('accounts:'+provider); onSnapshot({}); return currentAccounts.map(a=>({...a})); },
  };
  vm.createContext(context);
  const code = [fn('  const creditIsPending', '  function providerFilter'), fn('  async function providers(', '  function createReadMonitor('), fn('  const num =', '  async function services('), fn('  async function accounts()', '  // 模型限流与积分过期必须常显'),fn('  function accountActivityCell(', '  function limitNotices(')].join('\n');
  vm.runInContext(code + '\nthis.renderOverview = overview; this.renderAccounts = accounts; this.readback = scheduleCreditReadback;', context);
  return {context, reads, timers, state, elementFor, setAccounts(rows) {currentAccounts = rows;}};
}
const auto = totals => ({id: 'autoclaw', name: 'AutoClaw', accounts: {total: 1}, credit_totals: totals});

test('overview displays distinct provider values, includes known zero and never counts Doubao', async () => {
  const {context, timers, elementFor} = consoleCredits({
    providers: [auto({remain:30, expiring:null, ok:1, complete:false}), {id:'doubao', name:'Doubao', accounts:{total:2}, credit_totals:{remain:900}}],
    nextState: () => ({creditTotals:{remain:1100}, accounts:[
      {realm:'cn', credits:{remain:10, expiring:0}}, {realm:'global', credits:null}, {realm:'trae', credits:{remain:5}},
      {provider:'doubao', credits:{remain:600}}, {domain:'www.doubao.com', credits:{remain:500}}]})
  });
  await context.renderOverview();
  const html = elementFor('#view').innerHTML;
  assert.match(html, /data-credit-provider="workbuddy".*余额 ≥ 10 .*临期 ≥ 0/);
  assert.match(html, /data-credit-provider="trae".*余额 5 .*临期 未知/);
  assert.match(html, /data-credit-provider="autoclaw".*余额 30 .*临期 未知/);
  assert.doesNotMatch(html, /doubao|Doubao|1100|全部账号合计/);
  assert.equal(timers.length, 0);
});

test('overview reads pending provider results via one ordinary API read and leaves the page frame alone', async () => {
  const {context, reads, timers, elementFor} = consoleCredits({providers:[auto({unknown:1})], nextProviders:()=>[auto({remain:8, expiring:0, ok:1, complete:true})]});
  await context.renderOverview();
  const initial = elementFor('#view').innerHTML;
  assert.match(initial, /查询中/);
  await timers.shift().fn();
  assert.match(elementFor('#overview-provider-credits').innerHTML, /余额 8 .*临期 0/);
  assert.equal(elementFor('#view').innerHTML, initial);
  assert.deepEqual(reads, ['workbench','state','workbench/providers']);
  assert.equal(timers.length, 0);
});

test('accounts readback updates only credit markup, preserving other row facts and the search', async () => {
  const {context, timers, reads, state, elementFor, setAccounts} = consoleCredits({route:'accounts', providers:[auto({unknown:1})], accounts:[{id:'a', nickname:'Initial', credits:{pending:true}}]});
  await context.renderAccounts();
  const search = elementFor('#account-search'), list = elementFor('#account-list');
  search.value='typed query';
  assert.match(list.innerHTML, /查询中/);
  const cell={dataset:{creditAccount:'a'}};
  list.querySelectorAll=()=>[cell];
  setAccounts([{id:'a', nickname:'Do not overwrite', credits:{remain:0, expiring:4, expiring_text:'月底'}}]);
  await timers.shift().fn();
  assert.equal(search.value, 'typed query');
  assert.equal(state.accountRows[0].nickname, 'Initial');
  assert.match(cell.innerHTML, />0<.*4 即将过期 · 月底/);
  assert.doesNotMatch(elementFor('#account-credit-summary').innerHTML, /查询中/);
  assert.deepEqual(reads, ['accounts:autoclaw','accounts:autoclaw']);
  assert.equal(timers.length, 0);
});

test('credit readback drops an old route result even if transport resolves after navigation', async () => {
  const {context, timers, state} = consoleCredits();
  let resolve, painted = 0;
  context.readback(()=>state.version===1, ()=>new Promise(done=>{resolve=done;}), ()=>{painted++;return true;});
  const inFlight=timers.shift().fn();
  state.version++; resolve({remain:3}); await inFlight;
  assert.equal(painted,0); assert.equal(timers.length,0);
});

test('credit readback backs off and clears pending after a fixed budget', async () => {
  const {context, timers} = consoleCredits();
  const actual=[], retries=[];
  context.readback(()=>true,async()=>({pending:true}),(_,canRetry)=>{retries.push(canRetry);return canRetry;});
  while(timers.length){const task=timers.shift();actual.push(task.delay);await task.fn();}
  assert.deepEqual(actual,[1000,2000,4000,8000,15000,30000,60000]);
  assert.deepEqual(retries,[true,true,true,true,true,true,false]);
});

'use strict';
// ═══ 更新日志 ═══
// 2026-11-20：运行真实用量页渲染器，锁定日期筛选在卡片、模型、调用方与空态中的一致口径。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
const totals = (requests, prompt, completion, credit = 0) => ({
  requests, prompt_tokens: prompt, completion_tokens: completion,
  cached_tokens: 0, total_tokens: prompt + completion, credit,
  failed_requests: 0, unreported_requests: 0,
});
const model = (name, t) => ({model: name, totals: t});
function fixture() {
  const first = totals(2, 180, 20, 1), second = totals(3, 270, 30, 2);
  return {
    totals: totals(25, 2250, 250, 20), since: '2026-11-01T00:00:00Z',
    days: [{day: '2026-11-19', totals: first}, {day: '2026-11-20', totals: second}],
    keys: [
      {key_id: 'fixture-active', name: '当前调用方', totals: totals(10, 900, 100, 10),
        days: [{day: '2026-11-19', totals: first}, {day: '2026-11-20', totals: second}],
        models: [model('fixture:lifetime-model', totals(10, 900, 100, 10))],
        day_models: [
          {day: '2026-11-19', models: [model('fixture:daily-model', first)]},
          {day: '2026-11-20', models: [model('fixture:daily-model', second)]},
        ]},
      {key_id: 'fixture-old', name: '历史调用方', totals: totals(15, 1350, 150, 10),
        days: [{day: '2026-11-01', totals: totals(15, 1350, 150, 10)}],
        models: [model('fixture:old-model', totals(15, 1350, 150, 10))],
        day_models: [{day: '2026-11-01', models: [model('fixture:old-model', totals(15, 1350, 150, 10))]}]},
    ],
  };
}
function harness(data, range, clock = Date) {
  const start = source.indexOf('  function usageDays(');
  const end = source.indexOf('  // The request log follows', start);
  assert.ok(start >= 0 && end > start, 'usage renderer not found');
  const tableStart = source.indexOf('  function table(');
  const tableEnd = source.indexOf('  class APIError ', tableStart);
  const view = {innerHTML: ''};
  const state = {usageRange: range, route: 'usage', version: 0};
  const sandbox = {
    Date: clock, state, $: () => view, api: async () => data, APIError: Error,
    navigator: {onLine: true}, toast() {},
    esc: x => String(x ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    number: n => typeof n === 'number' ? n.toLocaleString('zh-CN') : '—',
    token: require('./token-format.js').html,
    date: x => x, heading: () => '', button: () => '',
    empty: (title, detail = '') => `<div class="empty">${title} ${detail}</div>`,
    notice: text => `<div class="notice">${text}</div>`,
  };
  const functions = vm.runInNewContext(source.slice(tableStart, tableEnd) + source.slice(start, end) + '\n({usage,filterUsage})', sandbox);
  return {view, state, render: functions.usage, filter: functions.filterUsage, sandbox};
}
function card(html, title) {
  const start = html.indexOf(`<h2>${title}</h2>`);
  assert.ok(start >= 0, `${title} card missing`);
  return html.slice(start, html.indexOf('</section>', start));
}
function tile(html, title) {
  const start = html.indexOf(`<span>${title}</span>`);
  assert.ok(start >= 0, `${title} tile missing`);
  return html.slice(start, html.indexOf('</article>', start));
}

test('selected day scopes model chart, model details and caller list together with the KPIs', async () => {
  const h = harness(fixture(), {from: '2026-11-20', to: '2026-11-20'});
  await h.render();
  const html = h.view.innerHTML;
  assert.match(tile(html, '总请求'), /<strong[^>]*>3<\/strong>/);
  assert.match(tile(html, '调用方'), /<strong[^>]*>1<\/strong>/);
  assert.match(card(html, '模型分布'), /fixture:daily-model/);
  assert.match(card(html, '模型分布'), /<td><span title="300 tokens">300<\/span><\/td><td>100%<\/td>/);
  assert.doesNotMatch(html, /fixture:lifetime-model|fixture:old-model|历史调用方/);
  assert.match(card(html, '调用方'), /当前调用方/);
  assert.match(card(html, '调用方'), /<div>3<\/div>/);
  assert.match(card(html, '调用方'), /<div><span title="300 tokens">300<\/span><\/div>/);
  assert.match(card(html, '按模型明细'), /<div><span title="270 tokens">270<\/span><\/div>/);
  assert.match(card(html, '按模型明细'), /<div><span title="30 tokens">30<\/span><\/div>/);
  assert.match(card(html, '按模型明细'), /<div>2<\/div>/);
});

test('custom ranges merge the same model across days and callers and sort by selected usage', async () => {
  const data = fixture();
  data.days[1].totals = totals(4, 315, 35, 2);
  data.keys[1].days.push({day: '2026-11-20', totals: totals(1, 45, 5)});
  data.keys[1].day_models.push({day: '2026-11-20', models: [model('fixture:daily-model', totals(1, 45, 5))]});
  const h = harness(data, {from: '2026-11-19', to: '2026-11-20'});
  await h.render();
  const html = h.view.innerHTML;
  assert.match(card(html, '模型分布'), /<td><span title="550 tokens">550<\/span><\/td><td>100%<\/td>/);
  assert.match(card(html, '按模型明细'), /<div>6<\/div>/);
  const callers = card(html, '调用方');
  assert.ok(callers.indexOf('当前调用方') < callers.indexOf('历史调用方'), 'sort must follow selected totals, not lifetime totals');
  assert.match(callers, /<div><span title="500 tokens">500<\/span><\/div>/);
  assert.match(callers, /<div><span title="50 tokens">50<\/span><\/div>/);
  assert.match(tile(html, '调用方'), /<strong[^>]*>2<\/strong>/);
});

test('a date without requests clears all breakdowns instead of retaining lifetime rows', async () => {
  const h = harness(fixture(), {from: '2026-11-10', to: '2026-11-10'});
  await h.render();
  const html = h.view.innerHTML;
  assert.match(tile(html, '总请求'), /<strong[^>]*>0<\/strong>/);
  assert.match(tile(html, '调用方'), /<strong[^>]*>0<\/strong>/);
  assert.doesNotMatch(html, /fixture:lifetime-model|fixture:old-model|fixture:daily-model|当前调用方|历史调用方|conic-gradient/);
});

test('legacy daily totals without model buckets remain usable and disclose missing model breakdowns', async () => {
  const data = fixture();
  data.keys[0].day_models = [];
  const h = harness(data, {from: '2026-11-20', to: '2026-11-20'});
  await h.render();
  const html = h.view.innerHTML;
  assert.match(tile(html, '总请求'), /<strong[^>]*>3<\/strong>/);
  assert.match(card(html, '调用方'), /当前调用方/);
  assert.doesNotMatch(card(html, '模型分布'), /fixture:lifetime-model|fixture:old-model/);
  assert.match(html, /缺少按日模型明细/);
});

test('open-ended ranges include only days on the selected side of the bound', async () => {
  for (const range of [{from: '2026-11-20', to: ''}, {from: '', to: '2026-11-19'}]) {
    const h = harness(fixture(), range);
    await h.render();
    assert.doesNotMatch(card(h.view.innerHTML, '模型分布'), /fixture:lifetime-model/);
    assert.match(card(h.view.innerHTML, '模型分布'), /fixture:daily-model/);
    if (range.from) assert.doesNotMatch(h.view.innerHTML, /历史调用方|fixture:old-model/);
    else assert.match(card(h.view.innerHTML, '模型分布'), /fixture:old-model/);
  }
});

test('failed requests without reported tokens still count as activity in the selected scope', async () => {
  const data = fixture(), failed = {...totals(1, 0, 0), failed_requests: 1, unreported_requests: 1};
  data.days = [{day: '2026-11-21', totals: failed}];
  data.keys[0].days = [{day: '2026-11-21', totals: failed}];
  data.keys[0].day_models = [{day: '2026-11-21', models: [model('fixture:failed-model', failed)]}];
  const h = harness(data, {from: '2026-11-21', to: '2026-11-21'});
  await h.render();
  assert.match(tile(h.view.innerHTML, '总请求'), /1 次失败 · 1 次未报告用量/);
  assert.match(tile(h.view.innerHTML, '调用方'), /<strong[^>]*>1<\/strong>/);
  assert.match(card(h.view.innerHTML, '调用方'), /当前调用方/);
  assert.match(card(h.view.innerHTML, '按模型明细'), /fixture:failed-model/);
});

test('old snapshots with daily grand totals but no caller buckets expose unavailable breakdowns', async () => {
  const data = fixture();
  for (const key of data.keys) { delete key.days; delete key.day_models; }
  const h = harness(data, {from: '2026-11-20', to: '2026-11-20'});
  await h.render();
  assert.match(tile(h.view.innerHTML, '总请求'), /<strong[^>]*>3<\/strong>/);
  assert.match(card(h.view.innerHTML, '调用方'), /缺少按日调用方明细/);
  assert.doesNotMatch(h.view.innerHTML, /fixture:lifetime-model|fixture:old-model|当前调用方|历史调用方/);
});

test('reset restores exact lifetime breakdowns without mutating the original ledger snapshot', async () => {
  const data = fixture(), before = JSON.stringify(data);
  const h = harness(data, {from: '2026-11-20', to: '2026-11-20'});
  await h.render();
  h.state.usageRange = {from: '', to: ''};
  await h.render();
  assert.equal(JSON.stringify(data), before);
  const html = h.view.innerHTML;
  assert.match(tile(html, '总请求'), /<strong[^>]*>25<\/strong>/);
  assert.match(tile(html, '调用方'), /<strong[^>]*>2<\/strong>/);
  assert.match(card(html, '模型分布'), /fixture:lifetime-model/);
  assert.match(card(html, '模型分布'), /fixture:old-model/);
  assert.doesNotMatch(card(html, '模型分布'), /fixture:daily-model/);
});

test('today and quick ranges follow local calendar days after local midnight', async () => {
  const oldTZ = process.env.TZ;
  process.env.TZ = 'Asia/Shanghai';
  try {
    class FixtureDate extends Date {
      constructor(...args) { super(...(args.length ? args : ['2026-11-20T16:30:00Z'])); }
      static now() { return Date.parse('2026-11-20T16:30:00Z'); }
    }
    const h = harness(fixture(), {from: '', to: ''}, FixtureDate);
    await h.render();
    assert.match(h.view.innerHTML, /data-from="2026-11-21" data-to="2026-11-21"/);
    assert.match(h.view.innerHTML, /data-from="2026-11-15" data-to="2026-11-21"/);
  } finally {
    if (oldTZ === undefined) delete process.env.TZ; else process.env.TZ = oldTZ;
  }
});

test('filtering uses the loaded snapshot without downloading it or changing its read timestamp', async () => {
  const data=fixture(), h=harness(data, {from:'',to:''});
  let reads=0;h.sandbox.api=async()=>{reads++;return data;};
  await h.render();const readAt=h.state.usageReadAt;
  h.state.usageRange={from:'2026-11-20',to:'2026-11-20'};h.filter();
  assert.equal(reads,1);assert.equal(h.state.usageReadAt,readAt);
  assert.match(tile(h.view.innerHTML,'总请求'), /<strong[^>]*>3<\/strong>/);
});

test('leaving the route before a read finishes discards that snapshot and its render', async () => {
  const h=harness(fixture(), {from:'',to:''});
  let release;h.sandbox.api=()=>new Promise(resolve=>{release=resolve;});
  const reading=h.render();h.state.version++;h.state.route='keys';release(fixture());await reading;
  assert.equal(h.state.usageSnapshot,null);assert.equal(h.view.innerHTML,'');
});

test('an old read cannot overwrite a newer successful refresh even if abort is ignored', async () => {
  const oldData=fixture(),freshData=fixture(),h=harness(oldData,{from:'',to:''});
  freshData.totals.requests=30;let release,reads=0;
  h.sandbox.api=()=>++reads===1?new Promise(resolve=>{release=resolve;}):Promise.resolve(freshData);
  const old=h.render();h.state.version++;await h.render();release(oldData);await old;
  assert.equal(h.state.usageSnapshot,freshData);
  assert.match(tile(h.view.innerHTML,'总请求'), /<strong[^>]*>30<\/strong>/);
});

test('a failed refresh invalidates cached filtering and preserves the old read timestamp', async () => {
  const h=harness(fixture(),{from:'',to:''});await h.render();
  const readAt=h.state.usageReadAt,html=h.view.innerHTML;
  h.sandbox.api=async()=>{throw new Error('synthetic unavailable');};
  await assert.rejects(h.render(),/synthetic unavailable/);
  assert.equal(h.state.usageSnapshot,null);assert.equal(h.state.usageReadAt,readAt);
  h.state.usageRange={from:'2026-11-20',to:'2026-11-20'};h.filter();
  assert.equal(h.view.innerHTML,html);
});

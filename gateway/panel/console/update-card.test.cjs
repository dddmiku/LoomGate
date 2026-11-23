'use strict';
// /api/update 的信封有两种失败形态（ok=false，或 ok=true 但 unavailable_reason 非空），
// 还有 failed / update_ready / 已检查无更新 / 尚未检查。控制台曾把它们统一显示成
// 「待独立验收」，并在私有仓库不可用时仍写「可检查官方发布并下载已签名的运行包」——
// 两者都是与真实状态相反的说法。这里执行真实 updateView()。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  // Include the small timestamp helpers used by updateView; the seam must execute
  // the same zero-value filtering as the shipped console, not just copy the branch.
  const body = source.slice(source.indexOf('  const date ='), source.indexOf('  async function settings() {'));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.updateView = updateView;', context);
  return context.updateView;
}
const publicStatus = (extra) => Object.assign({
  ok: true,
  status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', commit: 'abcdef1234567890', state: 'idle',
    repo: 'owner/loomgate', signature_verification: true,
  },
}, extra);

test('a failed visibility probe remains retryable rather than hiding the only recovery action', () => {
  const status={enabled:true,visibility:'unknown',state:'failed',current:'v1.0.13',latest_tag:'v1.0.13',
    unavailable_reason:'尚未确认发布仓库可见性，请稍后重试「检查更新」',update_ready:false};
  for(const ok of [true,false]){
    const view=seam()({ok,status,message:status.unavailable_reason});
    assert.equal(view.canCheck,true);
    assert.equal(view.canApply,false);
    assert.doesNotMatch(view.badgeText,/已关闭|未启用|已是最新/);
  }
});

test('update checks have a bounded timeout covering the server metadata budget', () => {
  const code=source.slice(source.indexOf('  function requestTimeout('),source.indexOf('  async function api('));
  const context={};vm.createContext(context);vm.runInContext(code+';this.timeout=requestTimeout;',context);
  assert.equal(context.timeout('update/check'),65000);
  assert.equal(context.timeout('update'),18000);
  assert.equal(context.timeout('connections/start'),40000);
});

test('unread envelope is reported as unread, never as disabled or downloaded', () => {
  const view = seam()(undefined);
  assert.equal(view.unread, true);
  assert.equal(view.badge, 'manual_action');
  assert.equal(view.badgeText, '状态未读取');
  assert.equal(view.canCheck, true);
});

test('ok=false keeps the server message and does not offer the check button', () => {
  const view = seam()({ok: false, enabled: false, message: '热更新未启用：config 里设置 update.enabled=true 后重启网关'});
  assert.equal(view.badge, 'disabled');
  assert.equal(view.canCheck, false);
  assert.ok(view.detail.includes('热更新未启用'), view.detail);
});

test('private repository blocks applying but remains re-checkable if visibility changes', () => {
  const reason = '发布仓库已转为私有，匿名读不到 Release；把仓库改回公开，或配置 update.token';
  const view = seam()(publicStatus({status: {
    enabled: true, visibility: 'private', unavailable_reason: reason,
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate', signature_verification: true,
  }}));
  assert.equal(view.badge, 'unavailable');
  assert.equal(view.badgeText, '不可用');
  assert.equal(view.detail, reason);
  assert.equal(view.canCheck, true);
  assert.equal(view.canApply, false);
  assert.ok(view.lines.some((line) => line.includes('owner/loomgate')), view.lines.join('|'));
});

test('failed state surfaces last_error instead of claiming pending acceptance', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'failed', repo: 'owner/loomgate',
    latest_tag: 'v1.0.1', last_error: 'synthetic-network-error: dial tcp: timeout',
    checked_at: '2026-11-18T00:00:00Z', signature_verification: false,
  }}));
  assert.equal(view.badge, 'failed');
  assert.equal(view.badgeText, '失败');
  assert.equal(view.canCheck, true);
  assert.ok(view.lines.some((line) => line.includes('synthetic-network-error')), view.lines.join('|'));
  assert.ok(!view.detail.includes('待独立验收'));
  assert.ok(view.lines.some((line) => line.includes('未内置发布公钥')), view.lines.join('|'));
});

test('update_ready is the only state that may say an upgrade exists', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate',
    latest_tag: 'v1.0.1', update_ready: true, checked_at: '2026-11-18T00:00:00Z',
    signature_verification: true,
  }}));
  assert.equal(view.badge, 'ready');
  assert.equal(view.badgeText, '可升级');
  assert.ok(view.lines.some((line) => line.includes('v1.0.1')), view.lines.join('|'));
});

test('checked with no update is 已是最新; never-checked is 尚未检查', () => {
  const latest = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate',
    update_ready: false, checked_at: '2026-11-18T00:00:00Z', signature_verification: true,
  }}));
  assert.equal(latest.badgeText, '已是最新');
  const never = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate', signature_verification: true,
  }}));
  assert.equal(never.badgeText, '未检查');
  assert.ok(!never.lines.join('|').includes('已是最新'));
});

test('Go zero checked_at means never checked, not latest', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, state: 'idle', checked_at: '0001-01-01T00:00:00Z',
    signature_verification: true,
  }}));
  assert.equal(view.badgeText, '未检查');
  assert.ok(!view.lines.some(line => line.startsWith('上次检查：')));
});

test('invalid checked_at is not a successful check', () => {
  for (const checked_at of ['invalid', 'unknown', null, 0]) {
    const view = seam()(publicStatus({status: {enabled: true, state: 'idle', checked_at}}));
    assert.equal(view.badgeText, '未检查');
  }
});

test('a signed ready release offers a one click apply action', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, state: 'idle', signature_verification: true,
    repo: 'owner/loomgate', current: 'v1.0.0', latest_tag: 'v1.0.2',
    asset_name: 'fixture-runtime.zip', update_ready: true,
  }}));
  assert.equal(view.canApply, true);
});

test('unread, disabled and unsigned statuses never offer apply', () => {
  for (const value of [undefined, {ok: false}, publicStatus(), publicStatus({status: {
    enabled: true, state: 'idle', signature_verification: false,
    repo: 'owner/loomgate', latest_tag: 'v1.0.2', asset_name: 'fixture.zip', update_ready: true,
  }})]) assert.equal(seam()(value).canApply, false);
});

test('busy update states block both checking and applying', () => {
  for (const state of ['checking', 'downloading', 'handover']) {
    const view = seam()(publicStatus({status: {enabled: true, state, signature_verification: true,
      latest_tag: 'v1.0.2', asset_name: 'fixture.zip', update_ready: true}}));
    assert.equal(view.canCheck, false);
    assert.equal(view.canApply, false);
  }
});

test('apply requires a valid frozen tag, repository and selected asset', () => {
  const ready = {enabled: true, state: 'idle', signature_verification: true,
    repo: 'owner/loomgate', latest_tag: 'v1.0.2', asset_name: 'fixture.zip', update_ready: true};
  for (const override of [{latest_tag: ''}, {latest_tag: '../bad'}, {repo: ''}, {asset_name: ''}]) {
    assert.equal(seam()(publicStatus({status: {...ready, ...override}})).canApply, false);
  }
});

test('build, deployment and remote publication timestamps are visible and distinct', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, state: 'idle', built_at: '2026-11-20T00:00:00Z',
    deployed_at: '2026-11-20T00:30:00Z',
    latest_at: '2026-11-20T01:02:03Z', checked_at: '2026-11-20T02:03:04Z',
  }}));
  assert.ok(view.lines.some(line => line.startsWith('构建时间：')), view.lines.join('|'));
  assert.ok(view.lines.some(line => line.startsWith('部署时间：')), view.lines.join('|'));
  assert.ok(view.lines.some(line => line.startsWith('远端发布时间：')), view.lines.join('|'));
  assert.ok(view.lines.some(line => line.startsWith('上次成功检查：')), view.lines.join('|'));
});

test('zero and invalid update timestamps are omitted instead of rendered as dates', () => {
  const view = seam()(publicStatus({status: {
    enabled: true, state: 'idle', built_at: '0001-01-01T00:00:00Z',
    deployed_at: '0001-01-01T00:00:00Z', latest_at: 'invalid', checked_at: 'unknown',
  }}));
  assert.ok(!view.lines.some(line => /时间|检查/.test(line)), view.lines.join('|'));
});

test('download byte progress uses only observed values', () => {
  const view = seam()(publicStatus({status: {enabled: true, state: 'downloading',
    downloaded_bytes: 1048576, asset_size: 2097152}}));
  assert.ok(view.lines.some(line => line.includes('已下载：1 / 2 MB')));
});

test('signature capability is stated from the real field on every branch', () => {
  const on = seam()(publicStatus());
  assert.ok(on.lines.some((line) => line.includes('签名校验：已启用')), on.lines.join('|'));
  const off = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate', signature_verification: false,
  }}));
  assert.ok(off.lines.some((line) => line.includes('未内置发布公钥')), off.lines.join('|'));
});

test('a legacy staged update preserves the real running version without a manual protection dialog', () => {
  const view = seam()(publicStatus({status: {enabled: true, state: 'idle',
    current: 'v1.0.5', latest_tag: 'v1.0.6', staged_version: 'v1.0.6',
    restart_required: true, update_ready: true, signature_verification: true,
    repo: 'owner/loomgate', asset_name: 'fixture-runtime.tar.gz'}}));
  assert.equal(view.badgeText, '尚未切换');
  assert.equal(view.canApply, false);
  assert.match(view.detail, /尚未完成切换/);
  assert.match(view.detail, /当前版本仍在运行/);
  assert.ok(view.lines.some(line => line === '当前版本：v1.0.5'));
  assert.ok(view.lines.some(line => line === '待启用版本：v1.0.6'));
});

test('automatic handover displays the observed target and keeps every apply control disabled',()=>{
  const view=seam()(publicStatus({status:{enabled:true,state:'handover',auto_restart:true,
    current:'v1.0.7',target_version:'v1.0.8',repo:'owner/loomgate',signature_verification:true}}));
  assert.equal(view.badgeText,'自动切换中');assert.equal(view.canApply,false);assert.equal(view.canCheck,false);
  assert.ok(view.lines.includes('当前版本：v1.0.7'));
  assert.ok(view.lines.includes('更新目标：v1.0.8'));
  assert.match(view.detail,/自动切换/);
});

test('legacy panel also reports a staged runtime as waiting for a gateway restart', () => {
  const legacy = fs.readFileSync(__dirname + '/../update.js', 'utf8');
  const nodes = new Map();
  function node(id) {
    if (!nodes.has(id)) nodes.set(id, {style: {}, classList: {remove() {}, add() {}},
      querySelector() { return node(id + '/fill'); }});
    return nodes.get(id);
  }
  const context = {$: node, esc: String, mrow: (key, value) => key + ':' + value,
    fmtTime: String, relTime: String};
  vm.createContext(context);
  vm.runInContext(legacy.slice(0, legacy.indexOf('async function loadUpdate()')), context);
  context.UP.data = {ok: true, status: {enabled: true, state: 'idle',
    current: 'v1.0.5', staged_version: 'v1.0.6', restart_required: true, update_ready: false}};
  context.UP.modal = true;
  context.UP.modalBusy = true;
  context.renderUpdate();
  assert.equal(node('#btnUpdApply').disabled, true);
  assert.equal(node('#updState').textContent, '等待重启');
  assert.match(node('#updHint').textContent, /当前版本继续服务/);
  assert.equal(node('#updModalTitle').textContent, '等待重启网关');
  assert.match(node('#updModalNote').textContent, /当前版本仍在运行/);
});

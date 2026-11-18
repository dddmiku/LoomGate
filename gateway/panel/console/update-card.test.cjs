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
  const body = source.slice(source.indexOf('  const updateStateLabels ='), source.indexOf('  async function settings() {'));
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

test('unread envelope is reported as unread, never as disabled or downloaded', () => {
  const view = seam()(undefined);
  assert.equal(view.unread, true);
  assert.equal(view.badge, 'manual_action');
  assert.equal(view.badgeText, '状态未读取');
  assert.equal(view.canCheck, false);
});

test('ok=false keeps the server message and does not offer the check button', () => {
  const view = seam()({ok: false, enabled: false, message: '热更新未启用：config 里设置 update.enabled=true 后重启网关'});
  assert.equal(view.badge, 'disabled');
  assert.equal(view.canCheck, false);
  assert.ok(view.detail.includes('热更新未启用'), view.detail);
});

test('private repository reason is shown verbatim and the check button is disabled', () => {
  const reason = '发布仓库已转为私有，匿名读不到 Release；把仓库改回公开，或配置 update.token';
  const view = seam()(publicStatus({status: {
    enabled: true, visibility: 'private', unavailable_reason: reason,
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate', signature_verification: true,
  }}));
  assert.equal(view.badge, 'unavailable');
  assert.equal(view.badgeText, '不可用');
  assert.equal(view.detail, reason);
  assert.equal(view.canCheck, false);
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

test('signature capability is stated from the real field on every branch', () => {
  const on = seam()(publicStatus());
  assert.ok(on.lines.some((line) => line.includes('签名校验：已启用')), on.lines.join('|'));
  const off = seam()(publicStatus({status: {
    enabled: true, visibility: 'public', unavailable_reason: '',
    current: 'v1.0.0', state: 'idle', repo: 'owner/loomgate', signature_verification: false,
  }}));
  assert.ok(off.lines.some((line) => line.includes('未内置发布公钥')), off.lines.join('|'));
});

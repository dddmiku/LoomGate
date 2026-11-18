'use strict';
// 服务端 /api/state 的 AuthSummary 带 parsable / mobile / enterpriseId。控制台曾把
// 这三个字段整体丢掉：损坏或旧格式的账号文件在界面上与「已启用但尚未被池加载」
// 完全一样，用户看不出这个号为什么从不工作；Trae 掩码手机号与 WorkBuddy 企业
// 租户也看不到。这里执行真实的 authSummaryRow / credentialWarning。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function authSummaryRow('), source.indexOf('  async function accountsFor('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.authSummaryRow = authSummaryRow; this.credentialWarning = credentialWarning;', context);
  return context;
}
test('unparsable credentials stay marked and get an explicit warning', () => {
  const {authSummaryRow, credentialWarning} = seam();
  const row = authSummaryRow({uid: 'u-broken', nickname: '旧号', disabled: false, expiresAt: 1800000000, realm: 'cn', parsable: false});
  assert.equal(row.parsable, false);
  assert.equal(row.enabled, true);
  assert.ok(credentialWarning(row).includes('无法解析'), credentialWarning(row));
});
test('a parsed account never shows the broken-credentials warning', () => {
  const {authSummaryRow, credentialWarning} = seam();
  const row = authSummaryRow({uid: 'u-ok', nickname: '正常号', disabled: false, expiresAt: 1800000000, realm: 'global', parsable: true});
  assert.equal(row.parsable, true);
  assert.equal(credentialWarning(row), '');
});
test('mobile and enterpriseId survive the mapping instead of being dropped', () => {
  const {authSummaryRow} = seam();
  const row = authSummaryRow({uid: 'u-trae', nickname: '用户', expiresAt: 1, realm: 'trae', mobile: '157****3894', enterpriseId: 'ent-42', parsable: true});
  assert.equal(row.mobile, '157****3894');
  assert.equal(row.enterpriseId, 'ent-42');
});
test('missing optional fields become empty strings, never undefined', () => {
  const {authSummaryRow} = seam();
  const row = authSummaryRow({uid: 'u-min', nickname: '', expiresAt: 0, realm: 'cn', parsable: true});
  assert.equal(row.mobile, '');
  assert.equal(row.enterpriseId, '');
  assert.equal(row.nickname, '');
});
test('summary without the parsable field is treated as readable (old servers)', () => {
  const {authSummaryRow, credentialWarning} = seam();
  const row = authSummaryRow({uid: 'u-old', nickname: '老服务', expiresAt: 1, realm: 'cn'});
  assert.equal(row.parsable, true);
  assert.equal(credentialWarning(row), '');
});

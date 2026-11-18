'use strict';
// 模型页/任务页的「账号范围」选择器：搜索框只比 id/nickname，Trae 的掩码手机号
// 与 WorkBuddy 的企业租户搜不到。账号页的搜索在轮次 18 已修，这个选择器是同一批
// 账号信息的另一个入口，必须一致。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const code = source.slice(source.indexOf('  const accountScope = id'), source.indexOf('  function toast('));
  const nodes = new Map();
  function node(raw) {
    const id = String(raw).replace(/^#/, '');
    if (!nodes.has(id)) nodes.set(id, {innerHTML: '', value: '', disabled: false, dataset: {}, attrs: {}, onclick: null,
      setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return this.attrs[k]; }, focus() {}});
    return nodes.get(id);
  }
  const context = {state: {route: 'models', modelProvider: 'workbuddy', taskProvider: 'workbuddy'}, $: node,
    esc: (x) => String(x ?? ''), providerName: (x) => x, button: () => '', accountIcon: '',
    openDialog: () => {}, empty: (t, d) => t + '|' + d, toast: () => {}, APIError: class extends Error {}};
  vm.createContext(context);
  vm.runInContext(code + '\nthis.bindAccountScope = bindAccountScope;', context);
  return {context, node};
}
const rows = [
  {id: '250000000000001', nickname: '合成 Trae 用户', realm: 'trae', enabled: true, mobile: '157****3894', enterpriseId: 'ent-synthetic'},
  {id: 'global01', nickname: '合成国际号', realm: 'global', enabled: true, mobile: '', enterpriseId: 'ent-global'},
];
test('account scope search matches the masked mobile number', () => {
  const {context, node} = seam();
  context.bindAccountScope('model-account', rows, '', '全部账号', async () => {});
  node('model-account').onclick();
  node('scope-search').value = '157****3894';
  node('scope-search').oninput();
  assert.equal(node('scope-count').textContent, '匹配 1 个账号');
  assert.match(node('scope-accounts').innerHTML, /250000000000001/);
  assert.doesNotMatch(node('scope-accounts').innerHTML, /global01/);
});
test('account scope search matches the enterprise tenant id', () => {
  const {context, node} = seam();
  context.bindAccountScope('task-account', rows, '', '全部已启用账号', async () => {});
  node('task-account').onclick();
  node('scope-search').value = 'ent-global';
  node('scope-search').oninput();
  assert.equal(node('scope-count').textContent, '匹配 1 个账号');
  assert.match(node('scope-accounts').innerHTML, /global01/);
});
test('an empty search still lists every account', () => {
  const {context, node} = seam();
  context.bindAccountScope('model-account', rows, '', '全部账号', async () => {});
  node('model-account').onclick();
  node('scope-search').value = '';
  node('scope-search').oninput();
  assert.equal(node('scope-count').textContent, '可选 2 个账号');
});

'use strict';
// /api/models 是所有 provider 的聚合目录（cn:/global:/trae:/doubao:…）。
// 控制台按服务页展示时必须只挑属于该服务的命名空间。原实现只有二选一：
// "trae 前缀 → Trae，其余 → WorkBuddy"，于是桌面版注册了豆包推理后，
// doubao:* 模型会混进 WorkBuddy 的模型目录——用户在「WorkBuddy」页看到并选中
// 一个豆包模型，实际调用会走另一个服务，范围显示与真实归属不一致。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  const providerModelScopes'), source.indexOf('  async function loadModels('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.modelsForProvider = modelsForProvider;', context);
  return context.modelsForProvider;
}
const items = [
  {id: 'cn:deepseek'}, {id: 'global:gpt'}, {id: 'trae:deepseek'},
  {id: 'doubao:synthetic-mode:synthetic'},
];
test('WorkBuddy shows only cn:/global: models, never doubao or trae', () => {
  const ids = seam()(items, 'workbuddy').map((m) => m.id);
  assert.deepEqual(ids, ['cn:deepseek', 'global:gpt']);
});
test('Trae shows only trae: models', () => {
  const ids = seam()(items, 'trae').map((m) => m.id);
  assert.deepEqual(ids, ['trae:deepseek']);
});
test('Doubao (account scope) is never fed from the aggregate catalog', () => {
  assert.deepEqual(seam()(items, 'doubao').map((m) => m.id), ['doubao:synthetic-mode:synthetic']);
});
test('an unknown or future provider does not inherit WorkBuddy models', () => {
  // vm 里产出的数组原型属于另一个 realm，deepStrictEqual 会因原型不同判不等；
  // 这里只关心长度与内容。
  const rows = seam()(items, 'future-provider');
  assert.equal(rows.length, 0);
});
test('rows without a string id are dropped instead of crashing the page', () => {
  const rows = [{id: null}, {id: 42}, {id: 'cn:ok'}, null];
  assert.deepEqual(seam()(rows, 'workbuddy').map((m) => m.id), ['cn:ok']);
});

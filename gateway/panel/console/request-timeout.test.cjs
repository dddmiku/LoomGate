'use strict';
// 迁移预检在服务端有 120s 预算（遍历+摘要整个旧工作区）。客户端曾统一 18s 就 abort，
// 慢盘/大目录下正常预检被报成失败，且文案说成「目录读取超时」（那是模型目录）。
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam() {
  const body = source.slice(source.indexOf('  function requestTimeout('), source.indexOf('  async function api('));
  const context = {};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.requestTimeout = requestTimeout; this.timeoutErrorCode = timeoutErrorCode;', context);
  return context;
}
test('migration preflight gets a budget at least as large as the server-side 120s', () => {
  const {requestTimeout} = seam();
  assert.ok(requestTimeout('migration/preflight') >= 120000);
  assert.equal(requestTimeout('migration/preflight'), 120000);
});
test('ordinary reads keep the short budget and cannot hang the page', () => {
  const {requestTimeout} = seam();
  assert.equal(requestTimeout('workbench'), 18000);
  assert.equal(requestTimeout('models/refresh'), 18000);
  assert.equal(requestTimeout('connections/start'), 40000);
});
test('preflight timeout has its own message instead of the model-catalog one', () => {
  const {timeoutErrorCode} = seam();
  assert.equal(timeoutErrorCode('migration/preflight'), 'migration_timeout');
  assert.equal(timeoutErrorCode('workbench'), 'catalog_timeout');
  assert.ok(source.includes('migration_timeout:'));
});

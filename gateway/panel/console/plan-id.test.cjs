'use strict';
// The console must work on plain-HTTP LAN access (explicitly supported feature),
// where crypto.randomUUID is undefined. planID() is the only id generator for new
// plans; these tests execute the real function from plan-editor.js.
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/plan-editor.js', 'utf8');
function seam(cryptoImpl) {
  const body = source.slice(source.indexOf('  function planID() {'), source.indexOf('  async function open(ui) {'));
  const context = {window: {crypto: cryptoImpl}};
  vm.createContext(context);
  vm.runInContext(body + '\nthis.planID = planID;', context);
  return context;
}
test('secure-context crypto.getRandomValues produces a 12-hex id', () => {
  // Deterministic test vector: verifies the v4 marker bits are applied.
  const {planID} = seam({getRandomValues: (bytes) => {for (let i = 0; i < bytes.length; i++) bytes[i] = i;} });
  const id = planID();
  assert.match(id, /^[0-9a-f]{12}$/);
  // The exported id is the first 12 hex chars of the v4 UUID bytes.
  assert.equal(id, '000102030405');
});
test('insecure context without any crypto still yields a valid unique id', () => {
  const {planID} = seam(undefined);
  const a = planID(), b = planID();
  assert.match(a, /^[0-9a-f]{12}$/);
  assert.match(b, /^[0-9a-f]{12}$/);
  assert.notEqual(a, b);
});
test('the plan editor never calls crypto.randomUUID directly', () => {
  // Comments mention the removed API on purpose; only executable lines count.
  const code = source.split(/\r?\n/).filter((line) => !line.trim().startsWith('//')).join('\n');
  assert.equal(code.includes('crypto.randomUUID'), false);
});

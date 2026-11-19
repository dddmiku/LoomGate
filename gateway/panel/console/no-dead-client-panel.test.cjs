'use strict';
// The console must not call endpoints the gateway does not serve. The account
// page used to fetch /workbench/client for an "official desktop client" panel;
// no such handler exists and neither legacy driver exposes a "desktop" method,
// so the panel could only ever render an error. This test pins both halves:
// the dead call stays gone, and the per-account status label it fed survives.
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const path = require('node:path');

const consoleDir = __dirname;
const connectionsSrc = fs.readFileSync(path.join(consoleDir, 'connections.js'), 'utf8');
const appSrc = fs.readFileSync(path.join(consoleDir, 'app.js'), 'utf8');

test('the console never calls the unimplemented workbench/client route', () => {
  // Look for an actual api() invocation, not the explanatory comment.
  for (const [name, src] of [['connections.js', connectionsSrc], ['app.js', appSrc]]) {
    assert.equal(/api\(\s*['"`]workbench\/client/.test(src), false, `${name} still calls /workbench/client`);
  }
});

test('the removed client panel leaves no dangling DOM hooks', () => {
  for (const [name, src] of [['connections.js', connectionsSrc], ['app.js', appSrc]]) {
    const code = src.replace(/^\s*\/\/.*$/gm, ''); // ignore prose
    for (const hook of ['client-status-refresh', 'connect-official-client', 'data-client-state']) {
      assert.equal(code.includes(hook), false, `${name} still references ${hook}`);
    }
  }
});

test('per-account client state labels are still exported', () => {
  // The account row renders connection.state through this map; deleting the
  // panel must not delete the labels it used.
  assert.match(connectionsSrc, /const clientLabels\s*=\s*\{/);
  assert.match(connectionsSrc, /return\s*\{[^}]*clientLabels[^}]*\}/);
});

test('the account page no longer mounts a client status container', () => {
  assert.equal(appSrc.includes('id="client-status"'), false);
  assert.equal(appSrc.includes("$('#client-status')"), false);
});

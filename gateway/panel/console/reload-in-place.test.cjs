'use strict';
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const src = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
function harness(providers, renderers) {
  const start = src.indexOf('  async function renderCurrent(');
  const end = src.indexOf('  async function action(', start);
  assert.ok(start >= 0 && end > start, 'route rendering entry point not found');
  const state = {route: 'accounts', version: 0, pollStarted: 12};
  const view = {dataset: {}, querySelector: () => null};
  const failed = [];
  const document = {querySelector: selector => selector === '#view .page-heading' ? {} : null};
  const sandbox = {
    state, providers, renderers, document,
    $: () => view,
    begin: () => ++state.version,
    showRouteLoading() {},
    finishRouteRender(generation) {if(generation===state.version)view.dataset.loadState='ready';},
    heading: () => '<header></header>',
    fail: error => failed.push(error.message),
  };
  const reload = vm.runInNewContext(src.slice(start, end) + '\nreload', sandbox);
  return {state, view, failed, reload};
}

test('superseded page reload cannot start a render after its provider read returns', async () => {
  let release, reads = 0;
  const rendered = [];
  const h = harness(() => ++reads === 1 ? new Promise(resolve => {release = resolve;}) : Promise.resolve(), {
    accounts: () => rendered.push('accounts'), settings: () => rendered.push('settings'),
  });
  const old = h.reload();
  h.state.route = 'settings';
  await h.reload();
  release(); await old;
  assert.deepEqual(rendered, ['settings']);
  assert.equal(h.view.dataset.loadState, 'ready');
});

test('stale renderer failure does not blank a newer successful page', async () => {
  let reject, reads = 0, signalStarted;
  const started = new Promise(resolve => {signalStarted = resolve;});
  const h = harness(() => Promise.resolve(), {
    accounts: () => ++reads === 1 ? new Promise((_, fail) => {reject = fail; signalStarted();}) : Promise.resolve(),
  });
  const old = h.reload();
  await started;
  await h.reload();
  reject(new Error('stale failure')); await old;
  assert.deepEqual(h.failed, []);
  assert.equal(h.view.dataset.loadState, 'ready');
});

test('explicit refresh forwards provider cache refresh only when requested', async () => {
  const calls = [];
  const h = harness(force => calls.push(force), {accounts: () => {}});
  await h.reload(); await h.reload(true);
  assert.deepEqual(calls, [false, true]);
  assert.equal(h.state.version, 2);
});

test('a current route failure still reports its error', async () => {
  const h = harness(() => Promise.resolve(), {accounts: () => {throw new Error('current failure');}});
  await h.reload();
  assert.deepEqual(h.failed, ['current failure']);
});

'use strict';
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/app.js', 'utf8');
function seam(rows) {
  const code = source.slice(source.indexOf('  function authSummaryRow('), source.indexOf('  function fail('));
  const context = {state: {providers: [{id:'workbuddy',catalog_scope:'provider'}, {id:'trae',catalog_scope:'provider'}]},
    api: async () => ({accounts: rows})};
  vm.createContext(context);vm.runInContext(code + '\nthis.readAccounts = accountsFor;',context);
  return context.readAccounts;
}
test('the real account mapping preserves nickname, credit and pool state', async () => {
  const read = seam([{uid:'synthetic-cn',nickname:'示例国内账号',realm:'cn',expiresAt:1900000000,disabled:false,
    credits:{ok:true,remain:123,expiring:12},pool:{healthy:true,inPool:true,cooling:false}},
    {uid:'synthetic-global',nickname:'示例国际账号',realm:'global',disabled:true},
    {uid:'synthetic-trae',nickname:'示例 Trae',realm:'trae',disabled:false}]);
  const rows = await read('workbuddy');
  assert.equal(rows.length,2);assert.equal(rows[0].nickname,'示例国内账号');
  assert.equal(rows[0].credits.remain,123);assert.equal(rows[0].pool.healthy,true);
  assert.equal(rows[0].expires_at,1900000000);assert.equal(rows[1].enabled,false);
  assert.equal((await read('trae'))[0].nickname,'示例 Trae');
});
test('doubao account mapping uses verified binding facts and never invents a nickname', async () => {
  const context = {state: {providers: [{id:'doubao',catalog_scope:'account'}]},
    api: async () => ({accounts: [
      {id:'bot-account-a',provider:'doubao',nickname:'',bot_id:'bot_abc123',verified_at:'2026-11-18T09:30:00Z',enabled:true,revision:3},
      {id:'bot-account-b',provider:'doubao',nickname:'',enabled:false,revision:1}
    ]})};
  const code = source.slice(source.indexOf('  function authSummaryRow('), source.indexOf('  function fail('));
  vm.createContext(context);vm.runInContext(code + '\nthis.readAccounts = accountsFor;',context);
  const rows = await context.readAccounts('doubao');
  assert.equal(rows.length,2);
  assert.equal(rows[0].nickname,'');            // 不把账号 ID/BotID 冒充昵称
  assert.equal(rows[0].botId,'bot_abc123');
  assert.equal(rows[0].verifiedAt,'2026-11-18T09:30:00Z');
  assert.equal(rows[1].botId,'');               // 未绑定不编造
  assert.equal(rows[1].verifiedAt,null);
});

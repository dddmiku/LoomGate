'use strict';
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/../update.js', 'utf8');
const code = source.slice(0, source.indexOf('/* ── 重复推理保护'));
const ready = extra => ({enabled:true,state:'idle',current:'v1.0.0',latest_tag:'v1.0.2',
  repo:'fixture-owner/fixture-product',asset_name:'fixture-runtime.zip',update_ready:true,
  signature_verification:true,checked_at:'2026-11-20T00:00:00Z',...extra});

function harness(respond = async (path) => path === 'api/update/apply'
  ? {ok:true,status:ready({state:'downloading'})} : {ok:true,status:ready()}) {
  const nodes = new Map(), timers = new Map(), calls = [], reloadTimers=[], reloads=[], storage=new Map();
  let asks = 0, timerID = 0;
  const node = key => {
    if(!nodes.has(key))nodes.set(key,{textContent:'',innerHTML:'',style:{},disabled:false,
      classList:{add(){},remove(){}},querySelector(){return node(key+'/fill');},addEventListener(){}});
    return nodes.get(key);
  };
  const location={hash:'#system',reload:()=>reloads.push(location.hash)};
  const context = {$:node,window:{location},esc:String,mrow:(key,value)=>key+':'+value,fmtTime:String,relTime:String,
    document:{addEventListener(){}},toast(){},
    sessionStorage:{getItem:key=>storage.get(key)||null,setItem:(key,value)=>storage.set(key,value)},
    setTimeout(fn,delay){reloadTimers.push({callback:fn,delay});return reloadTimers.length;},
    ask(...args){asks++;return args.at(-1)();},
    async api(path,body){calls.push({path,body});return respond(path,body);},
    setInterval(fn){timers.set(++timerID,fn);return timerID;},
    clearInterval(id){timers.delete(id);}};
  vm.createContext(context);vm.runInContext(code,context);
  context.UP.data={ok:true,status:ready()};
  context.observeUpdateStatus(context.UP.data.status);
  return {context,node,calls,reloadTimers,reloads,storage,get asks(){return asks;},get timerCount(){return timers.size;},
    async tick(){const run=timers.values().next().value;if(run)await run();}};
}

test('legacy update sends one frozen target immediately without a confirmation dialog', async()=>{
  const h=harness();await h.context.actUpdateApply();
  assert.equal(h.asks,0,'normal update still opens an ask confirmation');
  const posts=h.calls.filter(c=>c.path==='api/update/apply');
  assert.equal(posts.length,1);
  assert.deepEqual(JSON.parse(JSON.stringify(posts[0].body)),{tag:'v1.0.2'});
});

test('legacy update serializes two clicks while the apply response is pending', async()=>{
  let release;
  const waiting=new Promise(done=>{release=done;});
  const h=harness(async path=>path==='api/update/apply'?waiting:{ok:true,status:ready({state:'downloading'})});
  const first=h.context.actUpdateApply(),second=h.context.actUpdateApply();
  assert.equal(h.calls.filter(c=>c.path==='api/update/apply').length,1);
  release({ok:true,status:ready({state:'downloading'})});await Promise.all([first,second]);
});

test('legacy busy states only poll and never submit another apply', async()=>{
  for(const state of ['downloading','handover','restarting']){
    const h=harness();h.context.UP.data={ok:true,status:ready({state,target_version:'v1.0.2',auto_restart:true})};
    await h.context.actUpdateApply();
    assert.equal(h.calls.filter(c=>c.path==='api/update/apply').length,0,state+' submitted another update');
    assert.equal(h.timerCount,1,state+' did not resume readback');
  }
});

test('legacy busy to idle never reports success while the old version is still running', ()=>{
  const h=harness();h.context.UP.target='v1.0.2';h.context.openUpdateModal();h.context.UP.modalBusy=true;
  h.context.renderUpdateModal(ready({state:'idle',current:'v1.0.0'}));
  assert.notEqual(h.node('#updModalTitle').textContent,'更新完成');
  assert.equal(h.context.UP.modalDone,false);
});

test('legacy automatic restart keeps polling without instructing a manual restart', ()=>{
  const h=harness();h.context.UP.target='v1.0.2';h.context.openUpdateModal();
  const status=ready({state:'handover',restart_required:true,auto_restart:true,target_version:'v1.0.2',staged_version:'v1.0.2'});
  h.context.UP.data={ok:true,status};h.context.renderUpdate();
  assert.notEqual(h.node('#updState').textContent,'等待重启');
  assert.notEqual(h.node('#updModalTitle').textContent,'等待重启网关');
  assert.equal(h.context.UP.modalDone,false);
  assert.equal(h.timerCount,1);
});

test('legacy reconnect readback survives a temporary outage and requires healthy running target', async()=>{
  const statuses=[new Error('fixture switching connection'),ready({current:'v1.0.0'}),
    ready({current:'v1.0.2',ready:false}),ready({current:'v1.0.2',healthy:false}),
    ready({current:'v1.0.2',ready:true,healthy:true,update_ready:false})];
  const h=harness(async path=>{assert.equal(path,'api/update');const result=statuses.shift();if(result instanceof Error)throw result;return {ok:true,status:result};});
  h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;h.context.openUpdateModal();h.context.UP.modalBusy=true;h.context.startUpdatePoll();
  await h.tick();assert.equal(h.timerCount,1);assert.notEqual(h.node('#updState').textContent,'不可用');
  for(let i=0;i<3;i++){await h.tick();assert.equal(h.context.UP.modalDone,false);assert.equal(h.timerCount,1);}
  await h.tick();assert.equal(h.node('#updModalTitle').textContent,'更新完成');assert.equal(h.timerCount,0);
  assert.equal(h.calls.filter(c=>c.path==='api/update/apply').length,0);
});

test('legacy polling allows twenty minutes but never turns an unreachable service into success', async()=>{
  const h=harness(async()=>{throw new Error('fixture offline');});
  h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;h.context.openUpdateModal();h.context.startUpdatePoll();
  let ticks=0;while(h.timerCount&&ticks<605){await h.tick();ticks++;}
  assert.equal(h.timerCount,0);assert.equal(ticks,600);assert.notEqual(h.node('#updModalTitle').textContent,'更新完成');
});

test('legacy server failure preserves the reason even when its version equals the target', async()=>{
  const h=harness(async()=>({ok:true,status:ready({state:'failed',current:'v1.0.2',last_error:'fixture validation rejected'})}));
  h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;h.context.openUpdateModal();h.context.startUpdatePoll();
  await h.tick();
  assert.equal(h.timerCount,0);assert.equal(h.node('#updModalTitle').textContent,'更新失败');
  assert.equal(h.node('#updModalText').textContent,'fixture validation rejected');
});

test('legacy definite apply rejection is shown once without polling or resubmitting', async()=>{
  const h=harness(async()=>{const error=new Error('fixture rejected');error.status=409;throw error;});
  await h.context.actUpdateApply();
  assert.equal(h.calls.length,1);assert.equal(h.calls[0].path,'api/update/apply');
  assert.equal(h.timerCount,0);assert.equal(h.node('#updModalTitle').textContent,'更新未开始');
  assert.equal(h.node('#updModalText').textContent,'fixture rejected');
});

test('legacy lost apply response resumes readback and never sends another apply', async()=>{
  let reads=0;
  const h=harness(async path=>{
    if(path==='api/update/apply'){const error=new Error('fixture lost response');error.status=503;throw error;}
    reads++;return {ok:true,status:ready(reads===1?{state:'downloading'}:{current:'v1.0.2',update_ready:false})};
  });
  await h.context.actUpdateApply();assert.equal(h.timerCount,1);
  await h.tick();assert.notEqual(h.node('#updModalTitle').textContent,'更新完成');
  await h.tick();assert.equal(h.node('#updModalTitle').textContent,'更新完成');
  assert.equal(h.calls.filter(c=>c.path==='api/update/apply').length,1);
});

test('legacy busy check reads status rather than submitting a new check', async()=>{
  const h=harness(async()=>({ok:true,status:ready({state:'handover',auto_restart:true,target_version:'v1.0.2'})}));
  h.context.UP.data={ok:true,status:ready({state:'handover',auto_restart:true,target_version:'v1.0.2'})};
  await h.context.actUpdateCheck();
  assert.deepEqual(h.calls.map(c=>c.path),['api/update']);assert.equal(h.timerCount,1);
});

test('legacy hung status read cannot overlap or hold the poll loop forever', async()=>{
  let release;
  const waiting=new Promise(done=>{release=done;});
  const h=harness(async()=>waiting);
  h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;h.context.openUpdateModal();h.context.startUpdatePoll();
  const first=h.tick();
  for(let i=1;i<600;i++)await h.tick();
  assert.equal(h.calls.length,1);assert.equal(h.timerCount,0);
  assert.notEqual(h.node('#updModalTitle').textContent,'更新完成');
  release({ok:true,status:ready()});await first;
});

test('legacy staged service without automatic restart keeps the real staged status', ()=>{
  const h=harness();h.context.UP.target='v1.0.2';h.context.openUpdateModal();
  h.context.UP.data={ok:true,status:ready({restart_required:true,staged_version:'v1.0.2'})};h.context.renderUpdate();
  assert.equal(h.node('#updState').textContent,'等待重启');
  assert.equal(h.node('#updModalTitle').textContent,'等待重启网关');
  assert.notEqual(h.node('#updModalTitle').textContent,'更新完成');
});

test('legacy verified active update reloads frontend once and retains the hash without posting',()=>{
  const h=harness();h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;
  const status=ready({current:'v1.0.2',commit:'fixture-new',ready:true,healthy:true});
  h.context.UP.data={ok:true,status};h.context.renderUpdate();h.context.renderUpdate();
  assert.equal(h.context.UP.awaiting,false);assert.equal(h.reloadTimers.length,1);
  assert.equal(h.reloadTimers[0].delay,0);h.reloadTimers.shift().callback();
  assert.deepEqual(h.reloads,['#system']);assert.deepEqual(h.calls,[]);
  assert.deepEqual(JSON.parse(h.storage.get('loomgate.frontendReload')),
    {repo:'fixture-owner/fixture-product',tag:'v1.0.2',commit:'fixture-new'});
});

test('legacy frontend reload excludes initial target idle failure and unhealthy readbacks',()=>{
  for(const extra of [{current:'v1.0.0'}, {current:'v1.0.2',state:'failed'},
    {current:'v1.0.2',state:'handover'}, {current:'v1.0.2',restart_required:true},
    {current:'v1.0.2',ready:false}, {current:'v1.0.2',healthy:false}]){
    const h=harness();h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;
    h.context.UP.data={ok:true,status:ready(extra)};h.context.renderUpdate();
    assert.equal(h.reloadTimers.length,0,JSON.stringify(extra));
  }
  const h=harness();h.context.UP.data={ok:true,status:ready({current:'v1.0.2',target_version:'v1.0.2'})};
  h.context.renderUpdate();assert.equal(h.reloadTimers.length,0);
});

test('legacy frontend marker prevents a stale busy flag from causing a reload loop',()=>{
  const h=harness();h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;
  h.storage.set('loomgate.frontendReload',JSON.stringify({repo:'fixture-owner/fixture-product',tag:'v1.0.2',commit:''}));
  h.context.UP.data={ok:true,status:ready({current:'v1.0.2'})};h.context.renderUpdate();
  assert.equal(h.reloadTimers.length,0);
});

test('legacy frontend reload stays manual when its marker cannot be persisted or read back',()=>{
  for(const failure of ['throw','discard','readback']){
    const h=harness();h.context.UP.target='v1.0.2';h.context.UP.awaiting=true;h.context.UP.frontendReloadEligible=true;
    if(failure==='readback')h.context.sessionStorage.getItem=()=>{throw new Error('fixture storage unavailable');};
    else h.context.sessionStorage.setItem=()=>{if(failure==='throw')throw new Error('fixture storage unavailable');};
    h.context.UP.data={ok:true,status:ready({current:'v1.0.2'})};h.context.renderUpdate();
    assert.equal(h.reloadTimers.length,0,failure);assert.deepEqual(h.calls,[]);
  }
});

test('legacy historical completed target is not promoted by a pure check into an update reload',()=>{
  const h=harness();
  const historical=ready({current:'v1.0.2',target_version:'v1.0.2',update_ready:false});
  h.context.UP.data={ok:true,status:historical};h.context.renderUpdate();
  assert.equal(h.reloadTimers.length,0,'initial historical completion reloaded');
  h.context.UP.data={ok:true,status:{...historical,state:'checking'}};h.context.renderUpdate();
  assert.equal(h.timerCount,1,'checking must retain read-only polling');
  h.context.UP.data={ok:true,status:historical};h.context.renderUpdate();
  assert.equal(h.reloadTimers.length,0,'pure check acquired frontend reload permission');
  assert.deepEqual(h.calls,[]);
});

test('legacy submitted update never confirms a different repository with the same target tag',async()=>{
  const h=harness(async path=>path==='api/update/apply'
    ? {ok:true,status:ready({repo:'different-owner/different-product',current:'v1.0.2',ready:true,healthy:true})}
    : {ok:true,status:ready()});
  await h.context.actUpdateApply();
  assert.equal(h.calls.filter(c=>c.path==='api/update/apply').length,1);
  assert.equal(h.context.UP.modalDone,false,'different repository was reported as complete');
  assert.equal(h.reloadTimers.length,0,'different repository refreshed frontend');
  assert.equal(h.timerCount,1,'mismatched identity must remain under readback');
});

test('legacy pure check without a historical target finishes polling without awaiting an application',()=>{
  const h=harness();h.context.UP.data={ok:true,status:ready({state:'checking'})};h.context.renderUpdate();
  assert.equal(h.timerCount,1);assert.equal(h.context.UP.awaiting,false);
  h.context.UP.data={ok:true,status:ready()};h.context.renderUpdate();
  assert.equal(h.timerCount,0);assert.equal(h.context.UP.awaiting,false);
  assert.equal(h.reloadTimers.length,0);assert.deepEqual(h.calls,[]);
});

test('legacy observed switch authorizes exactly one matching-repository reload without posting',()=>{
  const h=harness();h.context.UP.data={ok:true,status:ready({state:'handover',target_version:'v1.0.2'})};
  h.context.renderUpdate();assert.equal(h.timerCount,1);
  h.context.UP.data={ok:true,status:ready({current:'v1.0.2',target_version:'v1.0.2',ready:true,healthy:true})};
  h.context.renderUpdate();h.context.renderUpdate();
  assert.equal(h.reloadTimers.length,1);assert.equal(h.timerCount,0);assert.deepEqual(h.calls,[]);
});

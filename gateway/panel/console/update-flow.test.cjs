'use strict';
const assert = require('node:assert/strict');
const {test} = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname+'/app.js','utf8');

function flow(storage=new Map()) {
  let now=100000;
  const nodes=new Map(),timers=[],requests=[],reloads=[];
  const state={version:1,pending:new Set()};
  const card={isConnected:true};
  function $(selector) {
    if(selector==='#setting-update')return card;
    if(!nodes.has(selector))nodes.set(selector,{});
    return nodes.get(selector);
  }
  const location={hash:'#settings',reload:()=>reloads.push(location.hash)};
  const context={state,$,window:{location},Date:{now:()=>now},esc:String,date:String,notice:String,badge:(_,label)=>label,
    validUpdateTime:()=>false,sessionStorage:{getItem:key=>storage.get(key)||null,setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
    setTimeout:(callback,delay)=>{timers.push({callback,delay});return timers.length;},clearTimeout:()=>{},
    api:async path=>{requests.push(path);return context.snapshot;},APIError:class extends Error{}};
  const body=source.slice(source.indexOf('  const updateStateLabels'),source.indexOf('  function paintLoopSetting('));
  vm.createContext(context);vm.runInContext(body+';this.paint=paintUpdateCard;this.read=readUpdateStatus;this.running=updateRunningTarget;',context);
  return {context,card,nodes,storage,timers,requests,reloads,setTime:value=>{now=value;}};
}
const ready = extra => ({ok:true,status:{enabled:true,repo:'fixture/repo',current:'v1.0.0',state:'idle',
  signature_verification:true,latest_tag:'v1.0.2',asset_name:'fixture.zip',update_ready:true,...extra}});

test('only the always-present check and a confirmed update remain as controls',()=>{
  const f=flow();
  f.context.paint(f.card,ready({state:'failed',visibility:'unknown',update_ready:false,unavailable_reason:'fixture visibility probe failed'}));
  const check=f.nodes.get('[data-action="update-check"]'),apply=f.nodes.get('[data-action="update-apply"]');
  assert.equal(check.hidden,false);assert.equal(check.disabled,false);assert.equal(apply.hidden,true);
  f.context.paint(f.card,{ok:false,enabled:false,message:'fixture disabled'});
  assert.equal(check.hidden,false);assert.equal(check.disabled,true);assert.equal(apply.hidden,true);
  f.context.paint(f.card,ready());assert.equal(apply.hidden,false);
  f.context.state.pending.add('update-check');f.context.paint(f.card,ready());
  assert.equal(check.disabled,true);assert.equal(apply.hidden,true);
  assert.doesNotMatch(source,/button\('update-status'/);
});

test('stale tab protection self-clears for authoritative idle and failure without discarding the real error',()=>{
  for(const state of ['idle','failed']) {
    const f=flow();f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
    f.context.paint(f.card,ready({state,last_error:'fixture failure'}));
    assert.equal(f.storage.has('loomgate.unconfirmedUpdate'),false);
    assert.equal(f.nodes.get('[data-action="update-check"]').hidden,false);
    if(state==='failed')assert.match(f.nodes.get('.update-notice').innerHTML,/fixture failure/);
    else assert.equal(f.nodes.get('[data-action="update-apply"]').hidden,false);
  }
});

test('accepted tab submission remains guarded across an early idle result, but never forever',()=>{
  const f=flow();f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2',started_at:100000}));
  f.context.paint(f.card,ready());
  assert.equal(f.nodes.get('[data-action="update-apply"]').hidden,true);
  f.setTime(116000);f.context.paint(f.card,ready());
  assert.equal(f.storage.has('loomgate.unconfirmedUpdate'),false);
  assert.equal(f.nodes.get('[data-action="update-apply"]').hidden,false);
  assert.doesNotMatch(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
});

test('completion requires target process version and each reported ready and healthy flag',()=>{
  const {context}=flow(),intent={repo:'fixture/repo',tag:'v1.0.2'};
  for(const extra of [{current:'v1.0.0'},{state:'handover'},{state:'failed'},{restart_required:true},{ready:false},{healthy:false},{healthy:null}]) {
    assert.equal(context.running(ready({current:'v1.0.2',...extra}).status,intent),false);
  }
  assert.equal(context.running(ready({current:'v1.0.2',healthy:true,ready:true}).status,intent),true);
});

test('unhealthy target keeps polling ownership instead of being mistaken for old idle intent',()=>{
  const f=flow();f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2',started_at:0}));
  f.context.paint(f.card,ready({current:'v1.0.2',healthy:false}));
  assert.equal(f.storage.has('loomgate.unconfirmedUpdate'),true);
  assert.doesNotMatch(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
});

test('an unread card or a later failed process never inherits a success notice',()=>{
  const f=flow();f.context.paint(f.card,undefined,'fixture offline');
  assert.doesNotMatch(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
  f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
  f.context.paint(f.card,ready({current:'v1.0.2'}));
  assert.match(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
  f.context.paint(f.card,ready({current:'v1.0.2',state:'failed'}));
  assert.doesNotMatch(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
});

test('automatic update polling has a twenty minute budget and never posts on timeout',async()=>{
  const f=flow();f.context.snapshot=ready({state:'handover',auto_restart:true,target_version:'v1.0.2'});
  await f.context.read(f.card,1);
  assert.equal(f.timers.length,1);assert.equal(f.timers[0].delay,2000);
  f.setTime(100000+20*60*1000);
  await f.timers.shift().callback();
  assert.equal(f.timers.length,0);
  assert.match(f.nodes.get('[data-update-action-status]').textContent,/暂未核验/);
  assert.deepEqual(f.requests,['update','update']);
});

test('a pending read that finishes after leaving settings cannot paint or schedule another read',async()=>{
  const f=flow();let resolve;f.context.api=()=>new Promise(done=>{resolve=done;});
  const pending=f.context.read(f.card,1);f.card.isConnected=false;resolve(ready({state:'handover'}));await pending;
  assert.equal(f.nodes.size,0);assert.equal(f.timers.length,0);
});

test('verified update clears its intent before one frontend reload and never posts again',()=>{
  const f=flow();f.storage.set('fixture-session','keep');
  f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
  const status=ready({current:'v1.0.2',commit:'fixture-new',ready:true,healthy:true});
  f.context.paint(f.card,status);f.context.paint(f.card,status);
  assert.equal(f.storage.has('loomgate.unconfirmedUpdate'),false);
  assert.equal(f.timers.length,1);assert.equal(f.timers[0].delay,0);
  f.timers.shift().callback();
  assert.deepEqual(f.reloads,['#settings']);assert.deepEqual(f.requests,[]);
  assert.equal(f.storage.get('fixture-session'),'keep');
  assert.deepEqual(JSON.parse(f.storage.get('loomgate.frontendReload')),
    {repo:'fixture/repo',tag:'v1.0.2',commit:'fixture-new'});
  // A stale intent left by failed removeItem cannot create a cross-document loop.
  f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
  const next=flow(f.storage);next.context.paint(next.card,status);
  assert.equal(next.timers.length,0);
  // The bounded marker is replaced when a different update finishes.
  next.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.3'}));
  next.context.paint(next.card,ready({current:'v1.0.3',commit:'fixture-next'}));
  assert.equal(next.timers.length,1);assert.equal(next.storage.size,2);
});

test('frontend reload does not run for unknown failed old or unhealthy update state',()=>{
  for(const extra of [{current:'v1.0.0'}, {current:'v1.0.2',state:'failed'},
    {current:'v1.0.2',state:'handover'}, {current:'v1.0.2',restart_required:true},
    {current:'v1.0.2',ready:false}, {current:'v1.0.2',healthy:false}, {repo:'other/repo',current:'v1.0.2'}]) {
    const f=flow();f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
    f.context.paint(f.card,ready(extra));assert.equal(f.timers.length,0,JSON.stringify(extra));
  }
  const f=flow();f.context.paint(f.card,ready({current:'v1.0.2'}));
  f.context.paint(f.card,undefined);assert.equal(f.timers.length,0);
});

test('frontend reload requires a durable once-only marker when storage is unavailable',()=>{
  for(const failure of ['throw','discard']) {
    const f=flow();f.storage.set('loomgate.unconfirmedUpdate',JSON.stringify({repo:'fixture/repo',tag:'v1.0.2'}));
    f.context.sessionStorage.setItem=()=>{if(failure==='throw')throw new Error('fixture storage unavailable');};
    f.context.sessionStorage.removeItem=()=>{throw new Error('fixture removal unavailable');};
    f.context.paint(f.card,ready({current:'v1.0.2'}));
    assert.equal(f.timers.length,0);assert.deepEqual(f.requests,[]);
    assert.match(f.nodes.get('[data-update-action-status]').textContent,/运行版本已核验/);
  }
});

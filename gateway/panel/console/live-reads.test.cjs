'use strict';
const {test}=require('node:test');const assert=require('node:assert/strict');const fs=require('node:fs');const vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/app.js','utf8');
function harness(api){
  const timers=new Map(),events=new Map(),states=[],applied=[];let id=0;
  const document={hidden:false,addEventListener:(name,fn)=>events.set(name,fn),removeEventListener:name=>events.delete(name)};
  const window={addEventListener:(name,fn)=>events.set(name,fn),removeEventListener:name=>events.delete(name)};
  const sandbox={document,window,AbortController,api,setTimeout:(fn,ms)=>{timers.set(++id,{fn,ms});return id;},clearTimeout:id=>timers.delete(id),Math};
  const start=source.indexOf('  function createReadMonitor('),end=source.indexOf('  // 服务端 /api/state',start);assert.ok(start>=0&&end>start);
  const create=vm.runInNewContext(source.slice(start,end)+'\ncreateReadMonitor',sandbox);
  let current=true;const monitor=create({path:'requests?limit=30',isCurrent:()=>current,apply:data=>applied.push(data),status:value=>states.push(value)});
  return {monitor,document,events,timers,applied,states,setCurrent:value=>current=value,async tick(){const next=timers.entries().next().value;if(!next)return;timers.delete(next[0]);await next[1].fn();},delay(){return [...timers.values()][0]?.ms;}};
}
test('same revision produces a lightweight GET and no repeated application',async()=>{
 let n=0;const paths=[],revision='a'.repeat(64);const h=harness(path=>{paths.push(path);return Promise.resolve(++n===1?{items:[],revision}:{not_modified:true,revision});});
 await h.tick();await h.tick();assert.equal(h.applied.length,1);assert.ok(paths[1].endsWith('revision='+revision));h.monitor.stop();
});
test('hidden pages stop reads and foreground resumes',async()=>{
 let calls=0;const h=harness(()=>{calls++;return Promise.resolve({items:[]});});await h.tick();h.document.hidden=true;h.events.get('visibilitychange')();assert.equal(h.timers.size,0);assert.equal(calls,1);
 h.document.hidden=false;h.events.get('visibilitychange')();assert.equal(h.delay(),0);await h.tick();assert.equal(calls,2);h.monitor.stop();assert.equal(h.events.size,0);
});
test('navigation stop aborts the in-flight read and discards its result',async()=>{
 let release,signal;const h=harness((path,options)=>{signal=options.signal;return new Promise(resolve=>release=resolve);});const pending=h.tick();h.monitor.stop();release({items:['late']});await pending;assert.equal(signal.aborted,true);assert.equal(h.applied.length,0);assert.equal(h.timers.size,0);
});
test('authorization failure stops automatic traffic',async()=>{
 const h=harness(()=>Promise.reject(Object.assign(new Error('fixture'),{status:401})));await h.tick();assert.equal(h.timers.size,0);assert.ok(h.states.includes('unauthorized'));
});
test('an unbound not-modified reply is not treated as successful freshness',async()=>{
 const h=harness(()=>Promise.resolve({not_modified:true,revision:'a'.repeat(64)}));await h.tick();assert.equal(h.applied.length,0);assert.ok(h.states.includes('retrying'));assert.equal(h.delay(),6000);h.monitor.stop();
});
test('temporary failures back off and successful recovery restores interval',async()=>{
 let n=0;const h=harness(()=>++n<3?Promise.reject(new Error('fixture')):Promise.resolve({items:[]}));await h.tick();assert.equal(h.delay(),6000);await h.tick();assert.equal(h.delay(),12000);await h.tick();assert.equal(h.delay(),3000);h.monitor.stop();
});
test('manual pause stops automatic reads without a route reload',async()=>{
 let calls=0;const h=harness(()=>{calls++;return Promise.resolve({items:[]});});h.monitor.setPaused(true);assert.equal(h.timers.size,0);h.monitor.setPaused(false);await h.tick();assert.equal(calls,1);h.monitor.stop();
});
test('focus notifications cannot overlap an in-flight read',async()=>{
 let calls=0,release;const h=harness(()=>{calls++;return new Promise(resolve=>release=resolve);});const pending=h.tick();
 h.events.get('focus')();await h.tick();h.events.get('focus')();await h.tick();assert.equal(calls,1);
 release({items:[]});await pending;assert.equal(h.applied.length,1);h.monitor.stop();
});
test('timing display keeps millisecond distinction and rate excludes waiting',()=>{
 const a=source.indexOf('  function requestDuration('),b=source.indexOf('  function requestTableMarkup(',a);const f=vm.runInNewContext(source.slice(a,b)+'\n({requestDuration,requestTokenRate})');
 assert.equal(f.requestDuration(110),'110ms');assert.notEqual(f.requestDuration(18001),f.requestDuration(18045));assert.equal(f.requestDuration(null),'—');
 assert.equal(f.requestTokenRate({output_tokens:200,duration_ms:5000,ttfb_ms:1000}),50);assert.equal(f.requestTokenRate({output_tokens:20,duration_ms:1000,ttfb_ms:null}),null);assert.equal(f.requestTokenRate({output_tokens:20,duration_ms:1000,ttfb_ms:1000}),null);
});

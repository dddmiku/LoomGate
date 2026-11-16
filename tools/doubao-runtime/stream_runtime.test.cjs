const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs'),crypto=require('node:crypto').webcrypto;
const source=fs.readFileSync(__dirname+'/stream_runtime.js','utf8');
const identity=()=>crypto.subtle.digest('SHA-256',new TextEncoder().encode('synthetic-account')).then(b=>Buffer.from(b).toString('hex'));
async function fixture({busy=false,hang=false,media='text/event-stream'}={}){
  let clock=Date.now(),watchdog,requests=[],cancelled=false;
  const window={};
  const im={getTaskService:()=>({checkpointService:{getIncompleteMainTasks:()=>busy?[{}]:[]}}),externalService:{sseRequestContextService:{getRequestMeta:async()=>({baseURL:'https://www.doubao.com',params:{synthetic:'safe'},headers:{'x-private':'synthetic-private-marker'}})},request:{apiContext:{fetch:async(url,options)=>{
    requests.push({url,options});options.signal.addEventListener('abort',()=>{cancelled=true;});
    return new Response(new ReadableStream({start(c){if(!hang){c.enqueue(new TextEncoder().encode('event: SYNTHETIC\ndata: {}\n\n'));c.close();}},cancel(){cancelled=true;}}),{headers:{'content-type':media}});
  }}}}};
  const modules={z5:{getFlowAccount:()=>({passportApi:{getPassportSDK:async()=>({login:{checkLogin:async()=>({user_id_str:'synthetic-account'})}})}})},Z9:{K:async()=>im},WMy:{useFeedStore:{getState:()=>({bot:'synthetic-current-bot'})},mainConvBotIdSelector:s=>s.bot,createLocalConversationId:()=> 'local_'+crypto.randomUUID()},ntm:{G:{Available:1}},S6:{N:text=>({block_type:10000,content:{text_block:{text}}})},WdY:{createChatQueryUplinkBody:async({messages,sessionId})=>({client_meta:{bot_id:messages[0].bot_id},messages:[{content_block:messages[0].content_blocks_v2}],option:{unique_key:sessionId,start_seq:0,model_config:{model_item_key:messages[0].ext.model_item_key},recovery_option:{is_recovery:false,req_create_time_sec:1}}})},Kk:{P:x=>x}};
  window['chunk_@flow-web/desktop']={push:a=>a[2](id=>modules[id])};
  const context=vm.createContext({window,crypto,TextEncoder,TextDecoder,structuredClone,URL,Intl,AbortController,setTimeout,clearTimeout,setInterval:f=>{watchdog=f;return 1;},clearInterval:()=>{},Date:class extends Date{static now(){return clock;}},btoa:s=>Buffer.from(s,'binary').toString('base64'),atob:s=>Buffer.from(s,'base64').toString('binary')});
  const run=vm.runInContext('('+source+')',context);
  const catalog=async()=>({ok:true,data:{entry_list:[{active_switch_conf:{menu_conf_v2:{use_mode_model_list:true,mode_list:{item_list:[{mode_id:'synthetic-mode',model_list:{item_list:[{model_item_key:'synthetic-model',show_task_tools:0,reasoning_effort_config:{default_level:5}}]}}]}}}}]}});
  const base={identity:await identity()};
  const invoke=(op,data={})=>run({...base,operation:op,...data},catalog);
  const prepare=async()=>{const reply=await invoke('prepare_text',{model:'doubao:synthetic-mode:synthetic-model',prompt:'synthetic text'});if(reply.ok)base.lease=reply.data.lease;return reply;};
  return {invoke,prepare,requests,advance(){clock+=16000;watchdog();},cancelled:()=>cancelled,async probe(input){return vm.runInContext('('+fs.readFileSync(__dirname+'/probe_runtime.js','utf8')+')',context)({...base,...input},catalog);},async probeCleanup(owner){return vm.runInContext('('+fs.readFileSync(__dirname+'/probe_cleanup.js','utf8')+')',context)(owner);}};
}
test('pure construction chooses dynamic bot/model, protects identity and never exports request headers',async()=>{
  const f=await fixture();const bad=await f.invoke('prepare_text',{identity:'b'.repeat(64),model:'doubao:synthetic-mode:synthetic-model',prompt:'synthetic'});assert.equal(bad.error,'desktop_identity_mismatch');
  const p=await f.prepare();assert.equal(p.ok,true);assert.equal(p.data.body.client_meta.bot_id,'synthetic-current-bot');assert.equal(f.requests.length,0);assert.ok(!JSON.stringify(p).includes('synthetic-private-marker'));
  assert.equal((await f.prepare()).error,'desktop_busy');await f.invoke('release_text');
});
test('body ownership and exactly one initial submission, with bounded raw chunks',async()=>{
  const f=await fixture(),p=await f.prepare(),body=p.data.body;
  const changed=structuredClone(body);changed.option.unique_key='different';assert.equal((await f.invoke('open_text',{path:'/chat/completion',body:changed})).error,'desktop_protocol_invalid');assert.equal(f.requests.length,0);
  assert.equal((await f.invoke('open_text',{path:'/chat/completion',body})).ok,true);
  let data='';for(let i=0;i<10;i++){const next=await f.invoke('pull_text');assert.equal(next.ok,true);if(next.data.chunk)data+=Buffer.from(next.data.chunk,'base64');if(next.data.done)break;}
  assert.match(data,/SYNTHETIC/);assert.equal((await f.invoke('open_text',{path:'/chat/completion',body})).error,'desktop_submission_uncertain');assert.equal(f.requests.length,1);await f.invoke('release_text');
});
test('caller cancellation and orphan watchdog abort the owned stream',async()=>{
  for(const orphan of [false,true]){const f=await fixture({hang:true}),p=await f.prepare();assert.equal((await f.invoke('open_text',{path:'/chat/completion',body:p.data.body})).ok,true);if(orphan)f.advance();else await f.invoke('cancel_text');assert.equal(f.cancelled(),true);if(!orphan)await f.invoke('release_text');}
});
test('busy official work and non-SSE responses cannot appear successful',async()=>{
  const busy=await fixture({busy:true});assert.equal((await busy.prepare()).error,'desktop_busy');assert.equal(busy.requests.length,0);
  const f=await fixture({media:'application/json'}),p=await f.prepare();assert.equal((await f.invoke('open_text',{path:'/chat/completion',body:p.data.body})).error,'desktop_response_rejected');assert.equal(f.cancelled(),true);await f.invoke('release_text');
});
test('async stream requires an observed handoff grant and preserves task identity on resume',async()=>{
  const f=await fixture(),p=await f.prepare();
  const drain=async()=>{for(let i=0;i<10;i++){const reply=await f.invoke('pull_text');assert.equal(reply.ok,true);if(reply.data.done)return;}throw Error('stream did not settle');};
  assert.equal((await f.invoke('open_text',{path:'/chat/completion',body:p.data.body})).ok,true);await drain();
  const body={task_id:'synthetic-task',seq_start:0,append_scene:4,ext:{}};
  assert.equal((await f.invoke('open_text',{path:'/chat/async/chunk_stream',body})).error,'desktop_protocol_invalid');
  assert.equal((await f.invoke('authorize_async',{task_id:'synthetic-task',scene:4})).ok,true);
  assert.equal((await f.invoke('open_text',{path:'/chat/async/chunk_stream',body:{...body,task_id:'foreign'}})).error,'desktop_protocol_invalid');
  assert.equal((await f.invoke('open_text',{path:'/chat/async/chunk_stream',body})).ok,true);await drain();
  assert.equal((await f.invoke('open_text',{path:'/chat/async/chunk_stream',body})).error,'desktop_submission_uncertain');
  assert.equal((await f.invoke('open_text',{path:'/chat/async/chunk_stream',body:{...body,seq_start:12},resume:true})).ok,true);await drain();
  assert.equal(f.requests.length,3);await f.invoke('release_text');
});
test('diagnostic cleanup aborts and settles only its owned fetch without touching DOM',async()=>{
  const cleanup=vm.runInNewContext('('+fs.readFileSync(__dirname+'/probe_cleanup.js','utf8')+')',{window:{},setTimeout,clearTimeout});
  assert.equal((await cleanup('no-owner')).ok,true);
  const key=Symbol.for('loomgate.probe.lease'),window={};let aborted=false,settle;
  const lease={owner:'synthetic-owner',settled:false,pending:new Promise(resolve=>{settle=()=>{lease.settled=true;resolve();};}),controller:{abort(){aborted=true;settle();}},timer:null};window[key]=lease;
  const run=vm.runInNewContext('('+fs.readFileSync(__dirname+'/probe_cleanup.js','utf8')+')',{window,setTimeout,clearTimeout});
  assert.equal((await run('foreign')).error,'desktop_ownership_lost');assert.equal(aborted,false);assert.equal(window[key],lease);
  assert.equal((await run('synthetic-owner')).ok,true);assert.equal(aborted,true);assert.equal(window[key],undefined);
});
test('actual diagnostic constructs a private owned fetch without composer or send-button access',async()=>{
  const f=await fixture(),owner=crypto.randomUUID();
  const reply=await f.probe({owner,bot_id:'synthetic-current-bot',prompt:'arbitrary synthetic prompt'});
  assert.equal(reply.ok,true);assert.equal(reply.owner,owner);assert.equal(f.requests.length,1);
  const request=f.requests[0];assert.equal(new URL(request.url).searchParams.get('loomgate_probe'),owner);
  assert.equal(JSON.parse(request.options.body).option.unique_key,owner);
  assert.equal((await f.probeCleanup(owner)).ok,true);assert.equal(f.cancelled(),true);
});

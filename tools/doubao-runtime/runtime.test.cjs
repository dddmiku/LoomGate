// Runs the actual renderer adapter against synthetic SDKs; no installed client.
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const {webcrypto,createHash}=require('node:crypto');
const source=fs.readFileSync(__dirname+'/runtime.js','utf8');
const fingerprint={z5:'9687119c89c1376874f12b2c26072d6b908ae54c2b26c83dca8b9410ead436d8',uui:'0e16f7576bb91259b254f905a5fe84f853d4d4289f57a6b1418fddd93bd2fc65',Pp:'fdb80e31bbdecf4a092e0f242c334a70ebb98e6fd5cd85778ab44597dbca4775'};
const hash=x=>createHash('sha256').update(x).digest('hex');
function fixture({users=['synthetic-user','synthetic-user'],badVersion=false,rejected=false,hold=false}={}) {
  let reads=0,calls=0,release;
  const modules={z5:{getFlowAccount:()=>({passportApi:{getPassportSDK:async()=>({login:{checkLogin:async()=>({user_id_str:users[Math.min(reads++,users.length-1)]})}})}})},uui:{createRequest:()=>({apiContext:{getBaseURL:()=> 'https://www.doubao.com',getCommonParams:async()=>({}),getCommonHeaders:async()=>({}),httpClient:async options=>{
    calls++;assert.equal(options.method,'POST');assert.equal(options.data.bot_id,'synthetic-bot');
    if(hold)await new Promise(resolve=>{release=resolve});
    return {code:rejected?7:0,data:{private_fixture:'not-exported',entry_list:[{active_switch_conf:{menu_conf_v2:{use_mode_model_list:true,mode_list:{item_list:[{mode_id:'mode-dynamic',name:'Synthetic mode',model_list:{item_list:[{model_item_key:'dynamic-new-model',name:'Fresh model',icon_url:'https://example.invalid/?secret=synthetic',model_extra_params:{total_window_size:'64000'}}]}}]}}}}]}};
  }}})},Pp:{Y:()=>({})}};
  const req=id=>id==='WMy'?{mainConvBotIdSelector:s=>s.bot,useFeedStore:{getState:()=>({bot:'synthetic-bot'})}}:modules[id];req.m=Object.fromEntries(Object.keys(modules).map(id=>[id,()=>id]));
  // Unique factory sentinels are synthetic; all identity hashes remain real.
  for(const id of Object.keys(modules))req.m[id]=new Function('return '+JSON.stringify(id));
  const bySource=Object.fromEntries(Object.entries(req.m).map(([id,fn])=>[String(fn),fingerprint[id]]));
  const crypto={randomUUID:()=> 'synthetic-chunk',subtle:{digest:async(_,bytes)=>{
    const value=Buffer.from(bytes).toString();return bySource[value]&&!badVersion?Uint8Array.from(Buffer.from(bySource[value],'hex')).buffer:await webcrypto.subtle.digest('SHA-256',bytes);
  }}};
  const window={'chunk_@flow-web/desktop':{push:item=>item[2](req)}};
  const fn=vm.runInNewContext('('+source+')',{window,crypto,TextEncoder,setTimeout,clearTimeout});
  return {run:input=>fn(input),calls:()=>calls,release:()=>release?.()};
}
(async()=>{
  const input={operation:'catalog',identity:hash('synthetic-user'),bot_id:'synthetic-bot'};
  const good=fixture(),result=await good.run(input);
  assert.equal(result.ok,true);assert.equal(good.calls(),1);
  const connection=fixture(),proof=await connection.run({operation:'connection'});
  assert.equal(proof.ok,true);assert.equal(proof.identity,input.identity);assert.equal(proof.data.bot_id,'synthetic-bot');assert.equal(proof.data.model_count,1);assert.ok(!JSON.stringify(proof).includes('entry_list'));
  assert(JSON.stringify(result).includes('dynamic-new-model'));
  for(const excluded of ['private_fixture','icon_url','not-exported','example.invalid'])assert(!JSON.stringify(result).includes(excluded));
  const switched=fixture({users:['synthetic-user','other-user']});assert.equal((await switched.run(input)).error,'desktop_identity_mismatch');assert.equal(switched.calls(),1);
  const wrong=fixture({users:['other-user']});assert.equal((await wrong.run(input)).error,'desktop_identity_mismatch');assert.equal(wrong.calls(),0);
  const version=fixture({badVersion:true});assert.equal((await version.run(input)).error,'desktop_version_unsupported');assert.equal(version.calls(),0);
  const reject=fixture({rejected:true});assert.equal((await reject.run(input)).error,'desktop_upstream_rejected');
  const identity=fixture();assert.equal((await identity.run({operation:'identity'})).identity,input.identity);assert.equal(identity.calls(),0);
  const held=fixture({hold:true});const pending=held.run(input);while(!held.calls())await new Promise(resolve=>setImmediate(resolve));
  assert.equal((await held.run(input)).error,'desktop_busy');held.release();assert.equal((await pending).ok,true);
  console.log('PASS 7 renderer boundary scenarios; synthetic SDK only');
})().catch(()=>{console.error('renderer_contract_failed');process.exitCode=1});

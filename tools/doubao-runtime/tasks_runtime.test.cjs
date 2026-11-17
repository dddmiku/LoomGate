const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs'),{webcrypto,createHash}=require('node:crypto');
const source=fs.readFileSync(__dirname+'/tasks_runtime.js','utf8'),identity=createHash('sha256').update('synthetic-account').digest('hex');
function fixture(claimResponse){
 let calls=[],current='synthetic-account',group='dynamic-group',checked=false,claimed=false,total=200,fail=false;
 const task=()=>({campaign_code:'dynamic-signin',campaign_group_code:group,task_kind:'daily_signin',title:'Synthetic sign in',task_status:1,daily_checkin:{checked_in_today:checked,last_checkin_date:checked?'2099-01-02':'2099-01-01',reward:{reward_amount:400,reward_type:'synthetic'}}});
 const milestone=()=>({campaign_code:'dynamic-milestone',campaign_group_code:group,task_kind:'points_milestone',title:'Synthetic milestone',task_status:claimed?3:1,reward_mode:2,milestone_threshold:500});
 const api={getBaseURL:()=> 'https://www.doubao.com',getCommonParams:async()=>({}),getCommonHeaders:async()=>({}),httpClient:async req=>{
  const path=new URL(req.url).pathname;calls.push({path,data:req.data});if(fail)return {code:7,data:{}};
  if(path==='/alice/basic/launch')return {code:0,data:{ug_campaign_info:{campaign_group_code:group},private_user_profile:'PRIVATE_SYNTHETIC_PROFILE'}};
  if(path==='/alice/ug/campaign/v3/detail/')return {code:0,data:{campaign_code:group,tasks:[task()],milestones:[milestone()],progress:{total_amount:total},fission_campaign:{invite_code:'PRIVATE_SYNTHETIC_INVITE'}}};
  if(path.endsWith('/checkin')){checked=true;total+=400;return {code:0,data:{checked_in:true}};}
  if(path.endsWith('/claim/')){assert.ok(total>=500);assert.equal(req.data.campaign_item_code,'dynamic-milestone');if(claimResponse)return claimResponse;claimed=true;total+=200;return {code:0,data:{}};}
  throw Error('unexpected operation');
 }};
 const modules={z5:{getFlowAccount:()=>({passportApi:{getPassportSDK:async()=>({login:{checkLogin:async()=>({user_id_str:current})}})}})},uui:{createRequest:()=>({apiContext:api})},Pp:{Y:()=>({})}};
 const window={'chunk_@flow-web/desktop':{push:x=>x[2](id=>modules[id])}};
 const fn=vm.runInNewContext('('+source+')',{window,crypto:webcrypto,TextEncoder,setTimeout,clearTimeout});
 return {run:(operation,fields={})=>fn({operation,identity,...fields}),calls,fail:()=>fail=true,switch:()=>current='other',mutationCount:()=>calls.filter(x=>/checkin$|claim\/$/.test(x.path)).length};
}
test('dynamic activities project only task data and preserve group from launch',async()=>{const f=fixture(),r=await f.run('activities');assert.equal(r.ok,true);assert.equal(r.data.tasks.length,1);assert.equal(r.data.group,'dynamic-group');assert.equal(f.calls[1].data.campaign_group_code,'dynamic-group');assert.ok(!JSON.stringify(r).includes('PRIVATE_SYNTHETIC'));assert.equal(f.mutationCount(),0)});
test('claim below threshold and foreign task identifiers never mutate',async()=>{const f=fixture();assert.equal((await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'})).error,'desktop_task_not_claimable');assert.equal((await f.run('task_checkin',{group:'foreign',task:'dynamic-signin'})).error,'desktop_task_expired');assert.equal(f.mutationCount(),0)});
test('sign in and eligible milestone claim are followed by authoritative readback, repeats do not mutate',async()=>{const f=fixture();let r=await f.run('task_checkin',{group:'dynamic-group',task:'dynamic-signin'});assert.equal(r.ok,true);assert.equal(r.data.tasks[0].daily_checkin.checked_in_today,true);assert.equal(r.data.total_amount,600);r=await f.run('task_checkin',{group:'dynamic-group',task:'dynamic-signin'});assert.equal(r.data.operation_status,'already_done');assert.equal(f.mutationCount(),1);r=await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'});assert.equal(r.data.milestones[0].task_status,3);assert.equal(r.data.total_amount,800);r=await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'});assert.equal(r.data.operation_status,'already_done');assert.equal(f.mutationCount(),2)});
test('identity switch and HTTP business failure fail closed',async()=>{const f=fixture();f.switch();assert.equal((await f.run('activities')).error,'desktop_identity_mismatch');assert.equal(f.calls.length,0);const bad=fixture();bad.fail();const error=await bad.run('activities');assert.equal(error.error,'desktop_upstream_rejected');assert.equal(error.code,7);assert.equal(bad.mutationCount(),0)});

test('outer success cannot hide nested claim rejection or expose its message',async()=>{
 for(const code of [2,'2',-1]){
  const f=fixture({code:0,data:{biz_error:{code,message:'PRIVATE_SYNTHETIC_REASON'},task_status:0}});
  await f.run('task_checkin',{group:'dynamic-group',task:'dynamic-signin'});
  const before=f.calls.length,r=await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'});
  assert.equal(r.ok,false);assert.equal(r.error,'desktop_upstream_rejected');assert.equal(r.code,Number(code));assert.equal(f.mutationCount(),2);
  assert.equal(f.calls.slice(before).filter(x=>x.path.endsWith('/claim/')).length,1);
  assert.ok(!JSON.stringify(r).includes('PRIVATE_SYNTHETIC_REASON'));
 }
});
test('official accepted nested codes never replace authoritative reward state',async()=>{
 for(const code of [0,4,6,'4']){
  const f=fixture({code:0,data:{biz_error:{code},task_status:3}});
  await f.run('task_checkin',{group:'dynamic-group',task:'dynamic-signin'});
  const r=await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'});
  assert.equal(r.ok,true);assert.equal(r.data.operation_status,'submitted');assert.equal(r.data.milestones[0].task_status,1);
  assert.equal(f.calls.at(-1).path,'/alice/ug/campaign/v3/detail/');
 }
});
test('malformed nested claim error is explicit protocol failure',async()=>{
 for(const biz_error of [{},{code:'PRIVATE_SYNTHETIC_REASON'},{code:{}},'invalid']){
  const f=fixture({code:0,data:{biz_error}});await f.run('task_checkin',{group:'dynamic-group',task:'dynamic-signin'});
  const r=await f.run('task_claim',{group:'dynamic-group',task:'dynamic-milestone'});assert.equal(r.error,'desktop_protocol_invalid');assert.ok(!JSON.stringify(r).includes('PRIVATE_SYNTHETIC_REASON'));
 }
});

test('work capability uses a fresh official directory and fails closed on missing or duplicate skills',async()=>{
 for(const mode of ['present','missing','duplicate','unavailable']){
  let reads=0;const task={campaign_group_code:'group',campaign_code:'dynamic-doc',campaign_type:'GenWord',task_status:1};
  const modules={z5:{getFlowAccount:()=>({passportApi:{getPassportSDK:async()=>({login:{checkLogin:async()=>({user_id_str:'synthetic-account'})}})}})},Pp:{Y:()=>({})},aT:{Kn:{System:2}},isL:{ph:()=> 'synthetic-dynamic-skill'},wWT:{skillsFacadeApiService:{AGWListUserAndFeaturedSkills:async q=>{reads++;assert.equal(q.runtime_type,1);return {code:mode==='unavailable'?1:0,data:{skills:Array(mode==='duplicate'?2:mode==='missing'?0:1).fill({name:'synthetic-dynamic-skill',type:2,visible:true})}}}}},uui:{createRequest:()=>({apiContext:{getBaseURL:()=> 'https://www.doubao.com',getCommonParams:async()=>({}),getCommonHeaders:async()=>({}),httpClient:async q=>{if(q.url.endsWith('/launch'))return {code:0,data:{ug_campaign_info:{campaign_group_code:'group'}}};assert.ok(q.url.endsWith('/detail/'));return {code:0,data:{campaign_code:'group',tasks:[task],milestones:[]}}}}})}};
  const window={'chunk_@flow-web/desktop':{push:x=>x[2](id=>modules[id])}},fn=vm.runInNewContext('('+source+')',{window,crypto:webcrypto,TextEncoder,setTimeout,clearTimeout});
  const result=await fn({operation:'activities',identity});assert.equal(result.ok,true);assert.equal(result.data.tasks[0].work_capability.available,mode==='present');assert.equal(reads,1);
 }
});

const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs'),{webcrypto,createHash}=require('node:crypto');
const source=fs.readFileSync(__dirname+'/work_runtime.js','utf8'),identity=createHash('sha256').update('synthetic-account').digest('hex');
function fixture({wrongIdentity=false,wrongTurn=false,more=false,secondUser=false,failed=false}={}){
 let reads=0;const request={operation:'work_read',identity,conversation:'synthetic-conversation',session:'synthetic-session',message:'synthetic-message'};
 const human={user_type:1,conversation_id:request.conversation,local_message_id:request.message,ext:{reply_unique_key:request.session}};
 const answer={user_type:2,status:0,conversation_id:request.conversation,message_id:'synthetic-answer',ext:{reply_unique_key:wrongTurn?'other-session':request.session,is_finish:'1',finish_reason_chat:failed?'failed':'succeed:completion'},content_block:[{block_type:10020,block_id:'synthetic-file',is_finish:true,content:{file_block:{name:'synthetic.xlsx',url:'https://p3-flow-sign.byteimg.com/synthetic?secret=synthetic'}}}]};
 const modules={z5:{getFlowAccount:()=>({passportApi:{getPassportSDK:async()=>({login:{checkLogin:async()=>({user_id_str:wrongIdentity?'another-synthetic': 'synthetic-account'})}})}})},Z9:{K:async()=>({getMessageService:async()=>({})})},i7J:{Crh:{ONE_TO_BOT_CHAT:1},Tty:{FROM_LATEST:3},P7b:{LargeMessageDefault:0}},fw1:{$:async args=>{reads++;assert.equal(args.conversation_id,request.conversation);return {code:0,data:{has_more:more,messages:[human,answer,...secondUser?[{...human,local_message_id:'second'}]:[]]}}}}};
 const requireModule=k=>modules[k];requireModule.m={fw1:()=>{}};
 const window={'chunk_@flow-web/desktop':{push:x=>x[2](requireModule)}};const run=vm.runInNewContext('('+source+')',{window,crypto:webcrypto,TextEncoder});return {run:(extra={})=>run({...request,...extra}),reads:()=>reads};
}
test('official readback requires matching account conversation and exact turn',async()=>{
 const f=fixture(),out=await f.run();assert.equal(out.ok,true);assert.equal(out.data.complete,true);assert.deepEqual(Object.keys(out.data),['complete']);assert.ok(!JSON.stringify(out).includes('synthetic.xlsx'));assert.equal(f.reads(),1);
 const other=fixture({wrongIdentity:true});assert.equal((await other.run()).error,'desktop_identity_mismatch');assert.equal(other.reads(),0);
 assert.equal((await fixture({wrongTurn:true}).run()).data.complete,false);
 assert.equal((await fixture({failed:true}).run()).data.complete,false);
});
test('legacy recovery refuses incomplete or ambiguous first-turn history',async()=>{
 const absent={session:undefined,message:undefined};assert.equal((await fixture().run(absent)).data.complete,true);
 assert.equal((await fixture({more:true}).run(absent)).error,'desktop_ownership_lost');
 assert.equal((await fixture({secondUser:true}).run(absent)).error,'desktop_ownership_lost');
});

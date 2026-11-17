// Read only the exact acknowledged conversation/turn. No generation or claim.
async input=>{
 const fail=error=>({ok:false,error});
 try{
  let r;window['chunk_@flow-web/desktop']?.push([['loomgate-work-read-'+crypto.randomUUID()],{},x=>r=x]);if(typeof r!=='function')return fail('desktop_version_unsupported');
  const identity=async()=>{const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK(),u=await sdk.login.checkLogin({});if(u.is_visitor_account||!u.user_id_str&&!u.user_id)return '';return [...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(String(u.user_id_str||u.user_id))))].map(x=>x.toString(16).padStart(2,'0')).join('')};
  if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
  const im=await r('Z9').K('chatIMService');await im.getMessageService();
  if(typeof r.m?.fw1!=='function'||typeof r('fw1').$!=='function')return fail('desktop_version_unsupported');
  const u=r('i7J'),response=await r('fw1').$({conversation_id:input.conversation,conversation_type:u.Crh.ONE_TO_BOT_CHAT,direction:u.Tty.FROM_LATEST,anchor_index:0,limit:50,evaluate_ab_params:'',evaluate_common_params:'',ext:{},option:{lazy_load_strategy:u.P7b.LargeMessageDefault}});
  if(response?.code!==0||!Array.isArray(response.data?.messages))return fail('desktop_response_rejected');
  if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
  const rows=response.data.messages;
  if(!input.session&&!input.message){
    // Legacy first-turn recovery requires a complete, unambiguous official
    // conversation; never guess a turn in a truncated or multi-turn history.
    const humans=rows.filter(m=>m.user_type===1&&m.conversation_id===input.conversation);
    if(response.data.has_more||humans.length!==1||!humans[0].local_message_id||!humans[0].ext?.reply_unique_key)return fail('desktop_ownership_lost');
    input={...input,session:humans[0].ext.reply_unique_key,message:humans[0].local_message_id};
  }
  const human=rows.find(m=>m.user_type===1&&m.local_message_id===input.message&&m.conversation_id===input.conversation&&m.ext?.reply_unique_key===input.session);
  if(!human)return fail('desktop_ownership_lost');
  const answers=rows.filter(m=>m.user_type===2&&m.conversation_id===input.conversation&&m.ext?.reply_unique_key===input.session);
  if(!answers.length)return {ok:true,identity:input.identity,data:{complete:false}};
  const complete=answers.every(m=>m.status===0&&m.ext?.is_finish==='1'&&m.ext?.finish_reason_chat==='succeed:completion');
  return {ok:true,identity:input.identity,data:{complete}};
 }catch{return fail('desktop_response_rejected')}
}

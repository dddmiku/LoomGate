// Fixed official task operations. All mutation decisions are re-read in the
// logged-in renderer; credentials and unrelated launch/user fields stay here.
async input=>{
  const fail=(error,code)=>({ok:false,error,...Number.isSafeInteger(code)?{code}:{}}),key=Symbol.for('loomgate.activity.v1');
  if(window[key]||window[Symbol.for('loomgate.text.stream.v1')])return fail('desktop_busy');
  const lease=crypto.randomUUID();window[key]=lease;
  const digest=async text=>Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(text))),x=>x.toString(16).padStart(2,'0')).join('');
  const str=v=>typeof v==='string'?v.slice(0,2048):'',num=v=>Number.isSafeInteger(v)?v:0;
  const reward=r=>({reward_amount:num(r?.reward_amount),reward_type:str(r?.reward_type)});
  let timer;
  const run=async()=>{
    let r;const chunks=window['chunk_@flow-web/desktop'];if(!chunks?.push)return fail('desktop_version_unsupported');
    chunks.push([['loomgate-task-'+crypto.randomUUID()],{},x=>r=x]);if(typeof r!=='function')return fail('desktop_version_unsupported');
    const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK();
    const identity=async()=>{const user=await sdk.login.checkLogin({});if(user?.is_visitor_account||!(user?.user_id_str||user?.user_id))return '';return digest(String(user.user_id_str||user.user_id));};
    const before=await identity();if(!before)return fail('desktop_login_required');if(before!==input.identity)return fail('desktop_identity_mismatch');
    const api=r('uui').createRequest().apiContext;if(api.getBaseURL()!=='https://www.doubao.com')return fail('desktop_origin_rejected');
    const call=async(path,data)=>{
      if(await identity()!==before)throw Error('desktop_identity_mismatch');
      const value=await api.httpClient({url:'https://www.doubao.com'+path,method:'POST',params:await api.getCommonParams(),headers:{...await api.getCommonHeaders(),...r('Pp').Y()},data});
      if(value?.code!==0&&value?.code!==200){const error=Error('desktop_upstream_rejected');if(Number.isSafeInteger(value?.code))error.safeCode=value.code;throw error;}
      if(!value.data||typeof value.data!=='object')throw Error('desktop_protocol_invalid');return value.data;
    };
    const basic=await call('/alice/basic/launch',{}),group=basic.ug_campaign_info?.campaign_group_code;
    if(typeof group!=='string'||!group||group.length>128)return fail('desktop_activity_unavailable');
    const read=async()=>{
      const data=await call('/alice/ug/campaign/v3/detail/',{campaign_group_code:group});
      if(!Array.isArray(data.tasks)||!Array.isArray(data.milestones)||(data.campaign_code&&data.campaign_code!==group))throw Error('desktop_protocol_invalid');
      for(const t of [...data.tasks,...data.milestones])if(t.campaign_group_code!==group||typeof t.campaign_code!=='string'||!t.campaign_code)throw Error('desktop_protocol_invalid');
      return data;
    };
    let detail=await read();let changed=false,operationStatus='read';
    if(input.operation!=='activities'){
      if(input.group!==group)return fail('desktop_task_expired');
      const task=[...detail.tasks,...detail.milestones].find(t=>t.campaign_code===input.task);
      if(!task)return fail('desktop_task_expired');
      if(task.daily_checkin?.checked_in_today===true||(!task.daily_checkin&&task.task_status===3)){operationStatus='already_done';}
      else if(input.operation==='task_checkin'){
        if(task.task_kind!=='daily_signin'||!task.daily_checkin||typeof task.daily_checkin.checked_in_today!=='boolean')return fail('desktop_task_manual');
        const response=await call('/alice/ug/campaign/landing/checkin',{scene:group});
        if(response.checked_in!==true)return fail('desktop_checkin_unconfirmed');changed=true;operationStatus='submitted';
      }else if(input.operation==='task_claim'){
        const milestone=detail.milestones.some(t=>t===task);
        const eligible=task.task_status===2||(milestone&&task.reward_mode===2&&Number.isSafeInteger(task.milestone_threshold)&&detail.progress?.total_amount>=task.milestone_threshold);
        if(!eligible)return fail('desktop_task_not_claimable');
        if(task.task_id&&task.task_id!==input.instance)return fail('desktop_task_expired');
        if(task.campaign_type==='WorkTaskMode'&&!task.task_id)return fail('desktop_task_instance_missing');
        const body=task.task_id?{task_id:task.task_id,...(task.campaign_type==='WorkTaskMode'?{campaign_group_code:group,campaign_item_code:task.campaign_code}:{})}:{task_id:'',campaign_group_code:group,campaign_item_code:task.campaign_code};
        const response=await call('/alice/ug/campaign/item/reward/claim/',body);
        // The official claim mapper treats nested business rejection separately
        // from the outer HTTP/API code. Accepted codes still require read-back.
        if(response.biz_error!=null){
          const code=response.biz_error.code;
          if(!['0','4','6'].includes(String(code))){
            if(!Number.isSafeInteger(code)&&!(typeof code==='string'&&/^-?\d{1,9}$/.test(code)))return fail('desktop_protocol_invalid');
            return fail('desktop_upstream_rejected',Number(code));
          }
        }
        changed=true;operationStatus='submitted';
      }else return fail('desktop_protocol_invalid');
      if(changed)detail=await read();
    }
    if(await identity()!==before)return fail('desktop_identity_mismatch');
    const project=t=>({task_id:str(t.task_id),campaign_code:str(t.campaign_code),campaign_group_code:group,task_kind:str(t.task_kind),title:str(t.title),subtitle:str(t.subtitle),description:str(t.description),campaign_type:str(t.campaign_type),task_status:num(t.task_status),reward_mode:num(t.reward_mode),milestone_threshold:num(t.milestone_threshold),reward_summaries:Array.isArray(t.reward_summaries)?t.reward_summaries.slice(0,32).map(reward):[],...(t.daily_checkin?{daily_checkin:{checked_in_today:t.daily_checkin.checked_in_today,last_checkin_date:str(t.daily_checkin.last_checkin_date),reward:reward(t.daily_checkin.reward),reward_status:num(t.daily_checkin.reward_status)}}:{})});
    return {ok:true,identity:before,data:{group,tasks:detail.tasks.slice(0,256).map(project),milestones:detail.milestones.slice(0,64).map(project),total_amount:num(detail.progress?.total_amount),operation_status:operationStatus}};
  };
  try{
    const pending=run().finally(()=>{if(window[key]===lease)delete window[key];});
    return await Promise.race([pending,new Promise(resolve=>{timer=setTimeout(()=>resolve(fail(input.operation==='activities'?'desktop_timeout':'desktop_submission_uncertain')),18000);})]);
  }catch(e){return fail(['desktop_identity_mismatch','desktop_upstream_rejected','desktop_protocol_invalid'].includes(e?.message)?e.message:'desktop_activity_failed',e?.safeCode);}
  finally{clearTimeout(timer);}
}

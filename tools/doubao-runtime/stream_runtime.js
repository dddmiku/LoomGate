// Runs inside the official renderer. No cookies, headers or signed URLs leave it.
async (input, catalogRuntime) => {
  const fail = error => ({ok:false,error});
  const key = Symbol.for('loomgate.text.stream.v1');
  const digest = async text => Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(text))),x=>x.toString(16).padStart(2,'0')).join('');
  let r;
  const chunks=window['chunk_@flow-web/desktop'];
  if(!chunks?.push)return fail('desktop_version_unsupported');
  chunks.push([['loomgate-stream-'+crypto.randomUUID()],{},x=>{r=x;}]);
  if(typeof r!=='function')return fail('desktop_version_unsupported');
  const identity=async()=>{
    const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK();
    const user=await sdk.login.checkLogin({}),id=user?.user_id_str||user?.user_id;
    if(!id||user.is_visitor_account)throw Error('desktop_login_required');
    return digest(String(id));
  };
  const stop=lease=>{lease.controller?.abort();lease.reader?.cancel().catch(()=>{});lease.wake?.();lease.drain?.();};
  const release=lease=>{stop(lease);clearInterval(lease.watchdog);if(window[key]===lease)delete window[key];};
  const canonical=value=>JSON.stringify(value,(_,v)=>v&&typeof v==='object'&&!Array.isArray(v)?Object.fromEntries(Object.keys(v).sort().map(k=>[k,v[k]])):v);
  try {
    if(input.operation==='prepare_text') {
      if(window[key])return fail('desktop_busy');
      if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
      const lease={id:crypto.randomUUID(),identity:input.identity,touched:Date.now(),created:Date.now(),active:false,submitted:false,queue:[],bytes:0,total:0,handoffs:new Map()};
      window[key]=lease;
      lease.native=input.work===true;
      if(lease.native&&!input.work_task){release(lease);return fail('desktop_task_manual');}
      lease.watchdog=setInterval(()=>{if(Date.now()-lease.touched>15000||Date.now()-lease.created>(lease.native?660000:180000))release(lease);},1000);
      try {
        const im=await r('Z9').K('chatIMService');
        if(!im||im.getTaskService().checkpointService.getIncompleteMainTasks().length||im.getTaskService().checkpointService.getIncompleteSubTasks?.().length)return fail('desktop_busy');
	    // Message construction is lazy-loaded by the official service. Requiring
	    // an absent chunk first can cache empty exports for the client lifetime.
	    if(typeof im.getMessageService!=='function')return fail('desktop_version_unsupported');
	    const messageService=await im.getMessageService();
	    if(typeof r.m?.WdY!=='function'||typeof r.m?.Kk!=='function')return fail('desktop_version_unsupported');
	    const uplink=r('WdY'),contextModule=r('Kk');
	    if(typeof uplink.createChatQueryUplinkBody!=='function'||typeof contextModule.P!=='function')return fail('desktop_version_unsupported');
        const bot=r('WMy').mainConvBotIdSelector(r('WMy').useFeedStore.getState());
        if(typeof bot!=='string'||!bot)return fail('desktop_version_unsupported');
        const catalog=await catalogRuntime({operation:'catalog',identity:input.identity,bot_id:bot});
        if(!catalog.ok)return catalog;
        let chosen;
        for(const e of catalog.data.entry_list){const m=e.active_switch_conf?.menu_conf_v2;if(!m)continue;for(const mode of m.use_mode_model_list?m.mode_list.item_list:[{mode_id:'default',model_list:m.model_list}])for(const item of mode.model_list.item_list){const id='doubao:'+mode.mode_id+':'+item.model_item_key;if(id===input.model)chosen={...item,mode_id:mode.mode_id};}}
        if(!chosen)return fail('desktop_model_unavailable');
        // Tool visibility selects the official cloud transport, not whether a
        // model can answer text. Only explicit, task-bound work uses preparers.
        lease.cloud=chosen.show_task_tools===1;
        if(lease.native&&!lease.cloud)return fail('desktop_native_tools_unsupported');
        if(input.conversation!==undefined&&(!/^[A-Za-z0-9_-]{1,128}$/.test(input.conversation)))return fail('desktop_protocol_invalid');
        const message={local_message_id:crypto.randomUUID(),...(input.conversation?{conversation_id:input.conversation}:{local_conversation_id:r('WMy').createLocalConversationId()}),bot_id:bot,create_time:Date.now(),status:r('ntm').G.Available,content_blocks_v2:[r('S6').N(input.prompt)],ext:{model_item_key:chosen.model_item_key,mode_id:chosen.mode_id,conversation_mode:'chat',model_extra_params:JSON.stringify(chosen.model_extra_params||{}),reasoning_effort:String(chosen.reasoning_effort_config.default_level)}};
        if(lease.cloud){
          const cloud=r('QG').rC$?.RuntimeType_Cloud_VM;
          if(cloud!==1||(lease.native&&typeof messageService.prepareExternalMessageRequest!=='function'))return fail('desktop_version_unsupported');
          message.ext.conversation_mode='office';message.ext.general_task_param=JSON.stringify({runtime_type:cloud,agent_task_param:{runtime_type:cloud}});
          if(input.work_task){
            // Re-read eligibility and the official task-to-skill mapping before
            // preparation. Never fabricate attribution reports or skill IDs.
            const api=r('uui').createRequest().apiContext;
            if(api.getBaseURL()!=='https://www.doubao.com')return fail('desktop_origin_rejected');
            const read=async(path,data)=>{const reply=await api.httpClient({url:'https://www.doubao.com'+path,method:'POST',params:await api.getCommonParams(),headers:{...await api.getCommonHeaders(),...r('Pp').Y()},data});if(reply?.code!==0)throw Error('task_read_failed');return reply.data;};
            const basic=await read('/alice/basic/launch',{}),group=basic?.ug_campaign_info?.campaign_group_code;
            if(group!==input.work_task.group)return fail('desktop_task_expired');
            const activity=await read('/alice/ug/campaign/v3/detail/',{campaign_group_code:group});
            const matches=(activity?.tasks||[]).filter(t=>t.campaign_group_code===group&&t.campaign_code===input.work_task.task);
            if(matches.length!==1)return fail('desktop_task_expired');
            const task=matches[0];if((task.task_id||'')!==input.work_task.instance||task.task_status!==1)return fail('desktop_task_expired');
            const name=r('isL').ph(task.campaign_type);
            if(name){
              const directory=await r('wWT').skillsFacadeApiService.AGWListUserAndFeaturedSkills({runtime_type:cloud});
              if(directory?.code!==0||!Array.isArray(directory.data?.skills))return fail('desktop_task_manual');
              const matches=directory.data.skills.filter(s=>s.visible!==false&&s.type===r('aT').Kn.System&&s.name===name);
              if(matches.length!==1)return fail('desktop_task_manual');
              lease.skill=name;
              message.ext.general_task_param=JSON.stringify({runtime_type:cloud,agent_task_param:{runtime_type:cloud},selected_skills:[name],skill_selections:[{name}]});
            }else if(task.campaign_type!=='WorkTaskMode')return fail('desktop_task_manual');
          }
        }
        if(input.work_task&&!lease.native)return fail('desktop_native_tools_unsupported');
        const sessionId=crypto.randomUUID();
        let messages=[message],body;
        if(lease.native){
          const prepared=await messageService.prepareExternalMessageRequest({messages,session:{reportParams:{}},sessionId});
          body=prepared.requestBody;messages=prepared.messages;
          if(body?.option?.general_task_param?.runtime_type!==1||body.option.general_task_param?.agent_task_param?.runtime_type!==1)return fail('desktop_native_tools_unsupported');
          if(body.option.general_task_param.client_option?.workspace||body.option.general_task_param.client_option?.sandbox_id)return fail('desktop_native_tools_unsupported');
          if(lease.skill&&(!body.option.general_task_param.selected_skills?.includes(lease.skill)||!body.option.general_task_param.skill_selections?.some(s=>s.name===lease.skill)))return fail('desktop_task_manual');
        }else body=await uplink.createChatQueryUplinkBody({messages,sessionId});
        if(lease.cloud){
          const cloudParams=body?.option?.general_task_param;
          if(cloudParams?.runtime_type!==1||cloudParams.agent_task_param?.runtime_type!==1)return fail('desktop_native_tools_unsupported');
          // Plain inference never acquires a local workspace, task skill or
          // arbitrary execution surface through the official builder.
          if(!lease.native&&canonical(cloudParams)!==canonical({runtime_type:1,agent_task_param:{runtime_type:1}}))return fail('desktop_native_tools_unsupported');
        }
        if(body.option?.unique_key!==sessionId||body.option.model_config?.model_item_key!==chosen.model_item_key||body.messages?.length!==1||body.client_meta?.bot_id!==bot)return fail('desktop_protocol_invalid');
        if(input.conversation&&(body.client_meta?.conversation_id!==input.conversation||body.option?.need_create_conversation===true))return fail('desktop_protocol_invalid');
        if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
        lease.im=im;lease.body=JSON.parse(JSON.stringify(body));
        lease.context={messages:{[message.local_message_id]:contextModule.P({message:messages[0],sessionId,conversationId:message.conversation_id||message.local_conversation_id})}};
        lease.touched=Date.now();lease.prepared=true;
        return {ok:true,identity:input.identity,data:{lease:lease.id,body:lease.body,model:input.model,native:lease.native}};
      } finally {if(!lease.prepared)release(lease);}
    }
    const lease=window[key];
    if(!lease||lease.id!==input.lease||lease.identity!==input.identity)return fail('desktop_ownership_lost');
    lease.touched=Date.now();
    if(input.operation==='release_text'){
      stop(lease);
      if(lease.pump){let timer;await Promise.race([lease.pump,new Promise(resolve=>{timer=setTimeout(resolve,900);})]);clearTimeout(timer);}
      if(lease.active)return fail('desktop_cleanup_uncertain');
      release(lease);return {ok:true,identity:input.identity};
    }
    if(input.operation==='cancel_text'){
      stop(lease);
      if(lease.pump){let timer;await Promise.race([lease.pump,new Promise(resolve=>{timer=setTimeout(resolve,900);})]);clearTimeout(timer);}
      if(lease.active)return fail('desktop_cleanup_uncertain');
      return {ok:true,identity:input.identity};
    }
    if(input.operation==='authorize_async'){
      if(lease.active||!lease.submitted||typeof input.task_id!=='string'||!input.task_id||input.task_id.length>512||lease.handoffs.size>=32)return fail('desktop_protocol_invalid');
      const prior=lease.handoffs.get(input.task_id);
      if(prior&&canonical(prior.scene)!==canonical(input.scene))return fail('desktop_protocol_invalid');
      if(!prior)lease.handoffs.set(input.task_id,{scene:input.scene,submitted:false});
      return {ok:true,identity:input.identity};
    }
    if(input.operation==='open_text') {
      if(lease.active)return fail('desktop_busy');
      const proposed=structuredClone(input.body);let expected,handoff;
      if(input.path==='/chat/completion'){
        expected=structuredClone(lease.body);
        if(input.resume){
          if(!lease.submitted||!Number.isSafeInteger(proposed.option?.start_seq)||proposed.option.start_seq<0||proposed.option.recovery_option?.is_recovery!==true)return fail('desktop_protocol_invalid');
          expected.option.start_seq=proposed.option.start_seq;expected.option.recovery_option.is_recovery=true;
        } else if(lease.submitted)return fail('desktop_submission_uncertain');
      }else if(input.path==='/chat/async/chunk_stream'){
        handoff=lease.handoffs.get(proposed.task_id);
        if(!handoff||!Number.isSafeInteger(proposed.seq_start)||proposed.seq_start<0||(!input.resume&&proposed.seq_start!==0)||(input.resume&&!handoff.submitted))return fail('desktop_protocol_invalid');
        if(handoff.submitted&&!input.resume)return fail('desktop_submission_uncertain');
        expected={task_id:proposed.task_id,seq_start:proposed.seq_start,append_scene:handoff.scene,ext:{}};
      }else return fail('desktop_origin_rejected');
      if(canonical(proposed)!==canonical(expected))return fail('desktop_protocol_invalid');
      if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
      const meta=await lease.im.externalService.sseRequestContextService.getRequestMeta(lease.context);
      if(meta.baseURL!=='https://www.doubao.com')return fail('desktop_origin_rejected');
      const url=new URL(input.path,meta.baseURL);
      for(const [k,v] of Object.entries(meta.params||{}))if(v!==undefined&&v!==null)url.searchParams.set(k,String(v));
      url.searchParams.set('tz_name',Intl.DateTimeFormat().resolvedOptions().timeZone);
      lease.controller=new AbortController();lease.queue=[];lease.bytes=0;lease.done=false;lease.error='';lease.active=true;
      lease.submitted=true;if(handoff)handoff.submitted=true;
      let response;
      try {response=await lease.im.externalService.request.apiContext.fetch(url.toString(),{method:'POST',headers:{...meta.headers,'Content-Type':'application/json','Agw-Js-Conv':'str','Accept':'text/event-stream'},body:JSON.stringify(proposed),credentials:'same-origin',redirect:'error',signal:lease.controller.signal});}
      catch {lease.active=false;return fail(lease.controller.signal.aborted?'desktop_canceled':'desktop_submission_uncertain');}
      if(!response.ok||!response.body||response.headers.get('content-type')?.split(';')[0].trim()!=='text/event-stream'){lease.active=false;stop(lease);return fail('desktop_response_rejected');}
      lease.reader=response.body.getReader();
      const logID=response.headers.get('x-tt-logid')||response.headers.get('x-request-id')||'';
      const requestDigest=logID?await digest(logID):'';
      lease.pump=(async()=>{
        try {
          for(;;){const {done,value}=await lease.reader.read();if(done)break;lease.total+=value.byteLength;if(lease.total>32*1024*1024)throw Error('desktop_stream_limit');
            for(let i=0;i<value.length;i+=32768){const part=value.slice(i,i+32768);while(lease.bytes+part.length>512*1024){if(lease.controller.signal.aborted)throw Error('desktop_canceled');await new Promise(resolve=>{lease.drain=resolve;});}
              lease.queue.push(btoa(String.fromCharCode(...part)));lease.bytes+=part.length;lease.wake?.();
            }
          }
          if(await identity()!==input.identity)lease.error='desktop_identity_mismatch';
        }catch(e){lease.error=['desktop_stream_limit','desktop_canceled'].includes(e?.message)?e.message:lease.controller.signal.aborted?'desktop_canceled':'desktop_stream_interrupted';}
        finally{lease.active=false;lease.done=true;lease.wake?.();}
      })();
      return {ok:true,identity:input.identity,data:{status:response.status,request_digest:requestDigest}};
    }
    if(input.operation==='pull_text') {
	  if(await identity()!==input.identity){stop(lease);lease.queue=[];lease.bytes=0;return fail('desktop_identity_mismatch');}
      if(!lease.queue.length&&!lease.done)await new Promise(resolve=>{const timer=setTimeout(resolve,600);lease.wake=()=>{clearTimeout(timer);resolve();};});
      lease.wake=null;lease.touched=Date.now();
      if(lease.queue.length){const chunk=lease.queue.shift();lease.bytes-=atob(chunk).length;lease.drain?.();lease.drain=null;return {ok:true,identity:input.identity,data:{chunk}};}
      if(lease.done)return lease.error?fail(lease.error):{ok:true,identity:input.identity,data:{done:true}};
      return {ok:true,identity:input.identity,data:{waiting:true}};
    }
    return fail('desktop_protocol_invalid');
  } catch {return fail('desktop_protocol_invalid');}
}

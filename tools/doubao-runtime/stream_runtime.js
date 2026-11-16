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
      lease.watchdog=setInterval(()=>{if(Date.now()-lease.touched>15000||Date.now()-lease.created>180000)release(lease);},1000);
      try {
        const im=await r('Z9').K('chatIMService');
        if(!im||im.getTaskService().checkpointService.getIncompleteMainTasks().length||im.getTaskService().checkpointService.getIncompleteSubTasks?.().length)return fail('desktop_busy');
	    // Message construction is lazy-loaded by the official service. Requiring
	    // an absent chunk first can cache empty exports for the client lifetime.
	    if(typeof im.getMessageService!=='function')return fail('desktop_version_unsupported');
	    await im.getMessageService();
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
        if(chosen.show_task_tools===1)return fail('desktop_native_tools_unsupported');
        const message={local_message_id:crypto.randomUUID(),local_conversation_id:r('WMy').createLocalConversationId(),bot_id:bot,create_time:Date.now(),status:r('ntm').G.Available,content_blocks_v2:[r('S6').N(input.prompt)],ext:{model_item_key:chosen.model_item_key,mode_id:chosen.mode_id,conversation_mode:'chat',model_extra_params:JSON.stringify(chosen.model_extra_params||{}),reasoning_effort:String(chosen.reasoning_effort_config.default_level)}};
        const sessionId=crypto.randomUUID();
        const body=await uplink.createChatQueryUplinkBody({messages:[message],sessionId});
        if(body.option?.unique_key!==sessionId||body.option.model_config?.model_item_key!==chosen.model_item_key||body.messages?.length!==1||body.client_meta?.bot_id!==bot)return fail('desktop_protocol_invalid');
        if(await identity()!==input.identity)return fail('desktop_identity_mismatch');
        lease.im=im;lease.body=JSON.parse(JSON.stringify(body));
        lease.context={messages:{[message.local_message_id]:contextModule.P({message,sessionId,conversationId:message.local_conversation_id})}};
        lease.touched=Date.now();lease.prepared=true;
        return {ok:true,identity:input.identity,data:{lease:lease.id,body:lease.body,model:input.model}};
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

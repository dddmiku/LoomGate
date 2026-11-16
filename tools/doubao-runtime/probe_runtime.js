// Diagnostic only: one directly owned fetch, no composer or SDK send lifecycle.
async (input, catalogRuntime) => {
  const fail=error=>({error});
  const key=Symbol.for('loomgate.probe.lease');
  if(window[key])return fail('desktop_busy');
  try {
    const chunks=window['chunk_@flow-web/desktop'];if(!chunks?.push)return fail('desktop_version_unsupported');
    let r;chunks.push([['loomgate-probe-'+crypto.randomUUID()],{},x=>r=x]);
    if(typeof r!=='function')return fail('desktop_version_unsupported');
    const im=await r('Z9').K('chatIMService');
    if(im.getTaskService().checkpointService.getIncompleteMainTasks().length)return fail('desktop_busy');
    const bot=r('WMy').mainConvBotIdSelector(r('WMy').useFeedStore.getState());
    if(bot!==input.bot_id)return fail('desktop_binding_invalid');
    const catalog=await catalogRuntime({operation:'catalog',identity:input.identity,bot_id:bot});
    if(!catalog.ok)return fail(catalog.error);
    let selected;
    for(const entry of catalog.data.entry_list){const menu=entry.active_switch_conf?.menu_conf_v2;if(!menu)continue;for(const mode of menu.use_mode_model_list?menu.mode_list.item_list:[{mode_id:'default',model_list:menu.model_list}])for(const item of mode.model_list.item_list)if(!selected&&item.show_task_tools!==1)selected={...item,mode_id:mode.mode_id};}
    if(!selected)return fail('desktop_model_unavailable');
    const local=r('WMy').createLocalConversationId(),message={local_message_id:crypto.randomUUID(),local_conversation_id:local,bot_id:bot,create_time:Date.now(),status:r('ntm').G.Available,content_blocks_v2:[r('S6').N(input.prompt)],ext:{model_item_key:selected.model_item_key,mode_id:selected.mode_id,conversation_mode:'chat'}};
    const body=await r('WdY').createChatQueryUplinkBody({messages:[message],sessionId:input.owner});
    const meta=await im.externalService.sseRequestContextService.getRequestMeta({messages:{[message.local_message_id]:r('Kk').P({message,sessionId:input.owner,conversationId:local})}});
    if(meta.baseURL!=='https://www.doubao.com'||body.option?.unique_key!==input.owner)return fail('desktop_protocol_invalid');
    const url=new URL('/chat/completion',meta.baseURL);
    for(const [k,v] of Object.entries(meta.params||{}))if(v!==undefined&&v!==null)url.searchParams.set(k,String(v));
    url.searchParams.set('loomgate_probe',input.owner);
    const lease={owner:input.owner,controller:new AbortController(),settled:false};window[key]=lease;
    lease.timer=setTimeout(()=>lease.controller.abort(),1500);
    lease.pending=im.externalService.request.apiContext.fetch(url.href,{method:'POST',headers:{...meta.headers,'Content-Type':'application/json','Agw-Js-Conv':'str'},body:JSON.stringify(body),credentials:'same-origin',redirect:'error',signal:lease.controller.signal}).catch(()=>{}).finally(()=>{lease.settled=true;});
    return {ok:true,owner:input.owner,local_conversation_id:local};
  } catch {return fail('desktop_version_unsupported');}
}

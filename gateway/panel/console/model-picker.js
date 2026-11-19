'use strict';
// Shared by key creation/editing; this reads the same visible catalog as model
// discovery and never calls a model or drops an existing binding on read failure.
window.LoomModelPicker = (() => {
  function rate(raw) {
    if(raw===null||raw===undefined||raw==='')return null;
    const match=String(raw).trim().match(/^(?:[x×]\s*)?(\d+(?:\.\d+)?)(?:\s*credits?)?$/i);
    if(!match)return null;
    const value=Number(match[1]);return Number.isFinite(value)&&value>=0?value:null;
  }
  function rateText(raw){const value=rate(raw);return value===null?'倍率未知':value===0?'免费':'x'+value;}
  function matches(model,query){const hay=[model.id,model.name,model.vendor].filter(Boolean).join(' ').toLowerCase();return query.trim().toLowerCase().split(/\s+/).every(part=>hay.includes(part));}
  function mount(form,{api,esc,selected=[]}) {
    const root=form.querySelector('[data-model-picker]'),input=form.querySelector('[name=models]');
    const chosen=new Set(selected),controller=new AbortController();
    let items=[],loading=false,failed=false,notice='',revision=0;
    root.innerHTML=`<div class="model-picker-tags" aria-label="已绑定模型"></div><div class="model-picker-toolbar"><label class="model-picker-search"><span class="sr-only">搜索可选模型</span><input type="search" data-model-query placeholder="搜索模型名称或渠道" autocomplete="off"></label><button type="button" data-model-clear>清空</button><button type="button" data-model-refresh>刷新</button></div><div class="model-picker-channels" role="group" aria-label="模型渠道">${[['','全部'],['cn','国内版'],['global','国际版'],['trae','Trae'],['autoclaw','AutoClaw']].map(([id,name])=>`<button type="button" data-model-channel="${id}" aria-pressed="${id===''}">${name}</button>`).join('')}</div><p class="model-picker-status small muted" role="status"></p><div class="model-picker-options" role="listbox" aria-multiselectable="true" aria-label="可绑定的客户端模型"></div>`;
    const query=root.querySelector('[data-model-query]'),list=root.querySelector('.model-picker-options'),tags=root.querySelector('.model-picker-tags'),status=root.querySelector('[role=status]'),refresh=root.querySelector('[data-model-refresh]');
    let channel='';
    function badge(model){const n=rate(model?.credits);return `<span class="model-rate ${n===null?'unknown':n===0?'free':n<.5?'low':'high'}">${esc(rateText(model?.credits))}</span>`;}
    function render(){
      if(!form.isConnected||controller.signal.aborted)return;
      input.value=[...chosen].join(',');
      const byID=new Map(items.map(m=>[m.id,m]));
      tags.innerHTML=chosen.size?[...chosen].map(id=>`<span class="model-picker-tag"><span>${esc(id)}</span>${badge(byID.get(id))}<button type="button" data-model-remove="${esc(id)}" aria-label="移除 ${esc(id)}">×</button></span>`).join(''):'<span class="small muted">未绑定模型 · 不限制可用模型</span>';
      const visible=items.filter(m=>(!channel||m.id.startsWith(channel+':'))&&matches(m,query.value));
      status.textContent=loading?'正在获取客户端模型和倍率…':failed?(notice||'模型列表读取失败。已有绑定保留，可稍后刷新。'):notice||`当前 ${visible.length} / ${items.length} 个可选模型，已选 ${chosen.size} 个`;
      status.classList.toggle('error',failed);
      refresh.disabled=loading;
      list.innerHTML=visible.length?visible.map(m=>`<button type="button" role="option" aria-selected="${chosen.has(m.id)}" data-key-model="${esc(m.id)}"><span class="model-option-main"><strong>${esc(m.name||m.id)}</strong><code>${esc(m.id)}</code><small>${esc([m.vendor,...[['supports_images','图片'],['supports_reasoning','推理'],['supports_tool_call','工具']].filter(([key])=>m[key]===true).map(([,name])=>name)].filter(Boolean).join(' · '))}</small></span>${badge(m)}<span class="model-option-check" aria-hidden="true">${chosen.has(m.id)?'✓':''}</span></button>`).join(''):`<p class="small muted model-picker-empty">${loading?'正在获取列表…':failed?'列表未读取，点击刷新重试。':items.length?'没有匹配的模型':'当前客户端未提供可选模型'}</p>`;
    }
    async function load(force){
      if(loading)return;loading=true;failed=false;notice='';const stamp=++revision;render();
      try{
        const data=await api(force?'models/refresh':'models',{...(force?{body:{}}:{}),signal:controller.signal});
        if(controller.signal.aborted||stamp!==revision||!form.isConnected)return;
        if(!Array.isArray(data.items))throw new Error('invalid_model_catalog');
        const seen=new Set();items=data.items.filter(m=>typeof m.id==='string'&&m.id.includes(':')&&!seen.has(m.id)&&(seen.add(m.id),true));
        if(data.provider_errors?.length)notice='部分渠道目录暂不可用。已保留现有绑定；倍率以各渠道返回值为准。';
      }catch(error){if(controller.signal.aborted)return;failed=true;notice='模型列表读取失败，当前选择未改变。可点击刷新重试。';}
      finally{if(stamp===revision){loading=false;render();}}
    }
    root.addEventListener('click',event=>{
      const target=event.target.closest('button');if(!target||target.disabled)return;
      if(target.hasAttribute('data-key-model')){
        const id=target.dataset.keyModel;if(chosen.has(id))chosen.delete(id);else if(chosen.size<64)chosen.add(id);else {notice='最多选择 64 个模型。';render();return;}
        render();[...list.querySelectorAll('[data-key-model]')].find(b=>b.dataset.keyModel===id)?.focus({preventScroll:true});
      }else if(target.hasAttribute('data-model-remove')){chosen.delete(target.dataset.modelRemove);render();}
      else if(target.hasAttribute('data-model-clear')){chosen.clear();render();}
      else if(target.hasAttribute('data-model-refresh')){load(true);}
      else if(target.hasAttribute('data-model-channel')){channel=target.dataset.modelChannel;root.querySelectorAll('[data-model-channel]').forEach(b=>b.setAttribute('aria-pressed',String(b===target)));render();}
    });
    query.oninput=render;
    const dialog=form.closest('dialog');const cleanup=()=>{controller.abort();dialog?.removeEventListener('close',cleanup);};dialog?.addEventListener('close',cleanup);
    render();load(false);
    return {values:()=>[...chosen],destroy:cleanup};
  }
  return {mount,rate,rateText,matches};
})();

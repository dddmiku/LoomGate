'use strict';
(() => {
  const $ = (selector, root = document) => root.querySelector(selector);
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const date = value => value && !String(value).startsWith('0001-') ? new Date(typeof value === 'number' ? value * 1000 : value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const number = value => typeof value === 'number' ? value.toLocaleString('zh-CN') : '—';
  const state = {route:'', version:0, controller:null, providers:null, models:[], catalogs:[], modelProvider:'', modelAccount:'', modelOffset:0, search:'', capability:'', taskProvider:'', taskAccount:'', tasks:[], schedules:[], runs:[], pending:new Set(), poll:null};
  const routes = {overview:['WORKSPACE','工作台','把服务、目录与自动化集中在一个工作区。'], providers:['CONNECTIONS','服务接入','按服务查看已接入账号与真实可用能力。'], accounts:['IDENTITIES','账号','集中查看服务账号的启用状态、会话有效期和目录入口。'], models:['MODEL LIBRARY','模型目录','从上游获取可选模型。能力与可用范围以当前账号的响应为准。'], tasks:['AUTOMATION','任务中心','发现服务提供的任务，按真实执行结果核验进度。'], schedules:['SCHEDULES','执行计划','选择任务与账号，设置执行时间、时区及每日规则。'], runs:['ACTIVITY','执行记录','查看每次派发、执行与服务端复核的结果。'], usage:['OBSERVABILITY','用量','查看网关累计请求与上游报告的用量。'], logs:['REQUEST HISTORY','请求记录','按请求查看模型、状态和耗时，不展示消息内容。'], settings:['PREFERENCES','工作区设置','管理显示偏好、会话与迁移准备。']};
  const labels = {ready:'目录已同步',stale:'旧缓存',unavailable:'暂不可用',disabled:'已停用',queued:'等待执行',running:'执行中',succeeded:'已核验完成',failed:'失败',canceled:'已取消',uncertain:'待复核',skipped:'已跳过',manual_action:'需要人工',discover:'发现',inspect:'检查',execute:'执行',claim:'领取',verify:'复核'};
  const errorLabels = {login_required:'登录已失效，请在官方客户端重新登录。',accounts_unavailable:'账号存储读取失败，请检查服务状态。',catalog_timeout:'目录读取超时，请稍后重试。',catalog_failed:'目录读取失败，请稍后重试。',catalog_unavailable:'上游目录暂不可用。',account_not_found:'账号已不存在，请刷新账号列表。',schedule_not_saved:'计划未保存，请检查时间、时区和任务范围。',run_rejected:'执行请求未被接受，可能已有任务正在运行。',unauthorized:'管理会话已过期，请重新登录。'};
  const badge = (value, text) => `<span class="badge ${['failed','unavailable','uncertain'].includes(value)?'bad':['stale','manual_action'].includes(value)?'warn':['disabled','skipped','canceled'].includes(value)?'neutral':''}" data-status="${esc(value)}">${esc(text || labels[value] || value)}</span>`;
  const button = (action, text, extra = '', cls = '') => `<button data-action="${action}" class="${cls}" ${extra}>${text}</button>`;
  const empty = (title, detail = '') => `<div class="empty"><span class="empty-icon" aria-hidden="true">◇</span><h3>${esc(title)}</h3><p>${esc(detail)}</p></div>`;
  const notice = (text, kind='') => `<div class="notice ${kind}" role="status">${esc(text)}</div>`;
  const options = (rows, value) => rows.map(([id,name]) => `<option value="${esc(id)}" ${id===value?'selected':''}>${esc(name)}</option>`).join('');
  const providerName = id => state.providers?.find(p=>p.id===id)?.name || id;
  const providerOptions = value => options((state.providers || []).map(p=>[p.id,p.name]),value);
  function toast(text) { $('#toast').textContent = text; $('#toast').hidden = false; clearTimeout(toast.timer); toast.timer = setTimeout(()=>$('#toast').hidden=true,6000); }
  function heading(actions='') { const r=routes[state.route]; return `<div class="page-heading"><div><span class="eyebrow">${r[0]}</span><h1>${r[1]}</h1><p>${r[2]}</p></div><div class="actions">${actions}</div></div>`; }
  function table(headers, rows) { return `<div class="table-wrap"><table class="responsive"><thead><tr>${headers.map(x=>`<th>${esc(x)}</th>`).join('')}</tr></thead><tbody>${rows.map(row=>`<tr>${row.map((cell,i)=>`<td data-label="${esc(headers[i])}"><div>${cell}</div></td>`).join('')}</tr>`).join('')}</tbody></table></div>`; }
  class APIError extends Error { constructor(code,status) { super(errorLabels[code] || (status===403?'没有执行此操作的权限，请重新登录。':status===429?'请求过于频繁，请稍后重试。':'请求未完成，请检查服务状态后重试。')); this.code=code; this.status=status; } }
  async function api(path, {body, signal=state.controller?.signal} = {}) {
    const controller=new AbortController(), abort=()=>controller.abort();
    if(signal?.aborted) controller.abort(); else signal?.addEventListener('abort',abort,{once:true});
    const timer=setTimeout(()=>controller.abort(),18000);
    try {
      const response=await fetch('api/'+path,{method:body===undefined?'GET':'POST',credentials:'same-origin',cache:'no-store',signal:controller.signal,headers:body===undefined?{}:{'Content-Type':'application/json','X-Admin-Request':'1'},body:body===undefined?undefined:JSON.stringify(body)});
      const data=await response.json();
      if(!response.ok || data.ok===false) throw new APIError(data.error || data.error_code || 'request_failed',response.status);
      $('#sync-time').textContent='同步于 '+new Date().toLocaleTimeString('zh-CN',{hour12:false});
      return data;
    } catch(e) { if(e.name==='AbortError' && !signal?.aborted) throw new APIError('catalog_timeout',408); throw e; }
    finally { clearTimeout(timer); signal?.removeEventListener('abort',abort); }
  }
  async function providers() { if(!state.providers) { const data=await api('workbench/providers'); if(!Array.isArray(data.providers)) throw new APIError('request_failed',502); state.providers=data.providers; } return state.providers; }
  async function accountsFor(provider) {
    const p=state.providers.find(p=>p.id===provider);
    if(p?.catalog_scope==='account') return (await api('workbench/accounts')).accounts.filter(a=>a.provider===provider).map(a=>({id:a.id,enabled:a.enabled,expires_at:a.expires_at,revision:a.revision}));
    const data=await api('state');
    return (data.accounts || []).filter(a=>provider==='trae'?a.realm==='trae':a.realm==='cn'||a.realm==='global').map(a=>({id:a.uid,enabled:!a.disabled,expires_at:a.expiresAt,realm:a.realm}));
  }
  function fail(e) { if(e.name==='AbortError') return; $('#view').innerHTML=heading()+notice(e instanceof APIError?e.message:'无法连接工作区，请检查网络或服务状态。','error')+button('reload','重新读取')+(e.status===401?' <a href="login">重新登录 →</a>':''); }
  function openDialog(title, html) { $('#dialog-title').textContent=title; $('#dialog-body').innerHTML=html; if(!$('#dialog').open) $('#dialog').showModal(); }
  function closeDialog() { $('#dialog').close(); $('#dialog-body').replaceChildren(); }
  async function mutate(key, path, body, after) {
    if(state.pending.has(key)) return;
    state.pending.add(key);
    const controls=[...document.querySelectorAll('dialog button[type=submit],button[data-lock]')];controls.forEach(b=>b.disabled=true);
    try { const data=await api(path,{body,signal:null}); await after(data); }
    catch(e) { const rejected=e instanceof APIError && e.status<500 && e.status!==408;if(key==='run' && rejected)sessionStorage.removeItem('loomgate.unconfirmedRun');const message=rejected?e.message:'请求结果尚未确认。请先刷新服务端状态，避免重复提交。'; if($('#dialog').open) { const box=$('#form-message'); if(box){box.textContent=message;box.hidden=false;}else toast(message); } else toast(message); }
    finally { state.pending.delete(key); controls.forEach(b=>b.disabled=false); }
  }
  function begin() { state.controller?.abort();clearTimeout(state.poll);state.controller=new AbortController();state.version++;return state.version; }
  async function navigate() {
    const route=location.hash.slice(1).split('?')[0] || 'overview';
    state.route=Object.hasOwn(routes,route)?route:'overview';begin();closeDialog();
    document.querySelectorAll('#navigation a').forEach(a=>{if(a.hash==='#'+state.route)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');});
    document.title=routes[state.route][1]+' · LoomGate';
    $('#view').innerHTML=heading()+`<div class="loading"><p>正在读取工作区…</p><div class="progress"></div></div>`;
    try { await providers(); await renderers[state.route](); } catch(e) {fail(e);}
  }
  async function overview() {
    const work=await api('workbench'), ps=state.providers, schedules=work.schedules || [], runs=work.runs || [];
    $('#view').innerHTML=heading(button('reload','刷新状态'))+`<section class="hero"><div><span class="eyebrow">ONE WORKSPACE. MANY POSSIBILITIES.</span><h2>你的 AI 工作，从这里开始。</h2><p>连接已有服务，选择账号可用的模型，<br>让日常任务按你的节奏运行。</p></div><div class="hero-side"><div><small>接入进度</small><strong>${ps.filter(p=>p.inference_available).length} 个服务支持推理</strong></div><div><small>下一步</small><a href="#models">查看当前模型目录 ↗</a></div></div></section>`+
      `<div class="metrics">${[[ps.length,'服务','按能力独立接入'],[ps.reduce((s,p)=>s+p.accounts.total,0),'账号','包含已停用账号'],[schedules.filter(s=>s.enabled).length,'启用计划','时间以计划时区为准'],[runs.filter(r=>['queued','running'].includes(r.status)).length,'进行中的执行','以服务端状态为准']].map(([n,t,d])=>`<div class="metric"><span>${t}</span><strong>${n}</strong><small>${d}</small></div>`).join('')}</div>`+
      `<div class="split"><section class="panel"><div class="panel-title"><div><span class="eyebrow">CONNECTED SERVICES</span><h2>服务概况</h2></div><a href="#providers">管理 →</a></div>${ps.map(p=>`<div class="catalog-row"><div><h3>${esc(p.name)}</h3><p class="muted small">${p.accounts.enabled} / ${p.accounts.total} 个账号启用</p></div>${badge(p.inference_available?'ready':'manual_action',p.inference_available?'支持 API 推理':'目录与任务接入')}</div>`).join('')}</section><section class="panel"><div class="panel-title"><div><span class="eyebrow">RECENT ACTIVITY</span><h2>最近执行</h2></div><a href="#runs">全部 →</a></div>${runs.length?runTimeline(runs.slice(0,4)):empty('还没有执行记录','到任务中心发现可执行的工作。')}</section></div>`;
  }
  async function services() {
    $('#view').innerHTML=heading(button('reload','同步服务'))+`<div class="grid">${state.providers.map(p=>`<article class="card" data-provider="${esc(p.id)}"><div class="card-top"><span class="provider-symbol">${esc(p.name.slice(0,1))}</span>${badge(p.inference_available?'ready':'manual_action',p.inference_available?'推理已接入':'部分能力接入')}</div><h2>${esc(p.name)}</h2><p>${p.catalog_scope==='account'?'模型随账号动态发现，保留目录缓存和失败状态。':'沿用现有网关的推理与模型目录契约。'}</p><dl><div><dt>已接入账号</dt><dd>${p.accounts.total}</dd></div><div><dt>已启用</dt><dd>${p.accounts.enabled}</dd></div></dl>${!p.inference_available?notice('模型目录可读取；消息转发与双登录尚未接通。','warn'):''}${p.error_code?notice(errorLabels[p.error_code] || '服务状态读取失败','error'):''}<div class="actions">${button('provider-models','查看模型',`data-provider="${esc(p.id)}"`)}${button('provider-accounts','查看账号',`data-provider="${esc(p.id)}"`,'quiet')}</div></article>`).join('')}</div>`;
  }
  async function accounts() {
    const selected=state.accountProvider || state.providers[0]?.id || '';
    $('#view').innerHTML=heading(button('connect-account','连接账号','','primary'))+`<div class="filters two"><label>服务<select id="account-provider">${providerOptions(selected)}</select></label><label>搜索账号标识<input id="account-search" placeholder="输入账号标识"></label></div><div id="account-list" class="loading">正在读取账号…</div><div id="connection-list"></div>`;
    $('#account-provider').addEventListener('change',e=>{state.accountProvider=e.target.value;reload();});
    const rows=await accountsFor(selected);state.accountRows=rows;
    const draw=()=>{const query=$('#account-search').value.toLowerCase();const list=rows.filter(a=>a.id.toLowerCase().includes(query));$('#account-list').className='';$('#account-list').innerHTML=list.length?table(['账号标识','状态','会话有效期','操作'],list.map(a=>[`<code>${esc(a.id)}</code>${a.realm?`<p>${esc(a.realm)}</p>`:''}`,badge(a.enabled?'ready':'disabled',a.enabled?'已启用':'已停用'),date(a.expires_at),`<div class="actions">${button('account-models','查看目录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'compact')}${button('toggle-account',a.enabled?'停用':'启用',`data-provider="${esc(selected)}" data-index="${rows.indexOf(a)}"`,'compact')}${button('reconnect-account','重新登录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'quiet compact')}</div>`])):empty(query?'没有匹配的账号':'尚未接入账号','账号接入状态由服务端读取。');};
    draw();$('#account-search').addEventListener('input',draw);await LoomConnections.list($('#connection-list'));
  }
  async function modelPage() {
    if(!state.providers.some(p=>p.id===state.modelProvider)) state.modelProvider=state.providers[0]?.id || '';
    $('#view').innerHTML=heading(button('model-refresh','刷新上游目录','','primary'))+`<div class="filters"><label>服务<select id="model-provider">${providerOptions(state.modelProvider)}</select></label><label>账号<select id="model-account"><option value="">读取中…</option></select></label><label>能力<select id="model-capability">${options([['','全部能力'],['native_tools','官方工作工具'],['supports_tool_call','API 工具调用'],['supports_images','图像输入'],['supports_reasoning','推理']],state.capability)}</select></label><label>搜索模型<input id="model-search" placeholder="名称或模型 ID" value="${esc(state.search)}"></label></div><div id="model-notice"></div><div id="model-status"></div><div id="model-list"></div><div id="model-pagination" class="pagination"></div>`;
    $('#model-provider').onchange=e=>{state.modelProvider=e.target.value;state.modelAccount='';state.modelOffset=0;reload();};
    $('#model-account').onchange=e=>{state.modelAccount=e.target.value;state.modelOffset=0;loadModels(false).catch(fail);};
    $('#model-capability').onchange=e=>{state.capability=e.target.value;drawModels();};
    $('#model-search').oninput=e=>{state.search=e.target.value;drawModels();};
    const p=state.providers.find(p=>p.id===state.modelProvider);
    if(p.catalog_scope==='account') { const rows=await accountsFor(p.id);$('#model-account').innerHTML=options([['','所有账号（分页）'],...rows.map(a=>[a.id,a.id+(a.enabled?'':' · 已停用')])],state.modelAccount); }
    else {state.modelAccount='';$('#model-account').innerHTML='<option value="">服务级目录</option>';$('#model-account').disabled=true;}
    await loadModels(false);
  }
  async function loadModels(force) {
    const generation=begin();state.modelLoading=true;
    $('#model-list').innerHTML='<div class="loading"><p>正在读取模型目录…</p><div class="progress"></div></div>';$('#model-notice').innerHTML='';$('#model-status').innerHTML='';$('#model-pagination').innerHTML='';
    $('[data-action=model-refresh]').disabled=true;
    try {
      const p=state.providers.find(p=>p.id===state.modelProvider);
      if(p.catalog_scope==='account') {
        const query=new URLSearchParams({provider:p.id,offset:String(state.modelOffset),limit:'8',refresh:force?'1':'0'});if(state.modelAccount)query.set('account',state.modelAccount);
        const data=await api('workbench/models?'+query);if(generation!==state.version)return;
        state.catalogs=data.catalogs;state.modelPage=data;
        state.models=data.catalogs.flatMap(c=>(c.models || []).map(m=>({...m,account_id:c.account_id,catalog_status:c.status})));
        $('#model-notice').innerHTML=notice('此服务当前仅提供模型目录，尚不能通过网关 API 调用。官方工作工具不等同于 API 工具调用。','warn')+(data.catalogs.some(c=>c.status==='stale')?notice('上游刷新失败，正在显示旧缓存。退避时间内重复刷新不会再次请求上游。','warn'):'');
        $('#model-status').innerHTML=`<details class="catalog-status" open><summary>账号目录状态 · 本页 ${data.catalogs.length} / ${data.total}</summary>${data.catalogs.map(c=>`<div class="catalog-row" data-account-status="${esc(c.status)}"><div><code>${esc(c.account_id)}</code><p class="small muted">${esc(errorLabels[c.error_code] || (c.error_code?'目录暂不可用。':'最近获取 '+date(c.fetched_at)))}${c.retry_at?' · 可重试 '+esc(date(c.retry_at)):''}${c.expires_at?' · 过期 '+esc(date(c.expires_at)):''}</p></div>${badge(c.status)}</div>`).join('')}</details>`;
        $('#model-pagination').innerHTML=`<span>账号 ${data.total?data.offset+1:0}–${Math.min(data.offset+data.limit,data.total)} / ${data.total} · 能力与搜索仅筛选本页</span><div class="actions">${button('model-prev','上一页',data.offset===0?'disabled':'')}${button('model-next','下一页',!data.has_more?'disabled':'')}</div>`;
      } else {
        const data=await api(force?'models/refresh':'models',force?{body:{}}:{});if(generation!==state.version)return;
        state.catalogs=[];state.models=(data.items || []).filter(m=>p.id==='trae'?m.id.startsWith('trae:'):!m.id.startsWith('trae:'));
        $('#model-notice').innerHTML=notice('服务目录已接入推理。具体请求仍受账号余额、限流与模型可用性影响。');
      }
      drawModels();
    } catch(e) {if(e.name==='AbortError')return;state.models=[];$('#model-list').innerHTML=empty('目录读取未完成','可以稍后重试。');$('#model-notice').innerHTML=notice(e instanceof APIError?e.message:'无法连接服务，请检查网络。','error');}
    finally {if(generation===state.version){state.modelLoading=false;const b=$('[data-action=model-refresh]');if(b)b.disabled=false;}}
  }
  function drawModels() {
    const query=state.search.trim().toLowerCase(), p=state.providers.find(p=>p.id===state.modelProvider);
    const models=state.models.filter(m=>(!query || (m.name+' '+m.id).toLowerCase().includes(query)) && (!state.capability || m[state.capability]===true));
    $('#model-list').innerHTML=models.length?`<div class="grid">${models.map(m=>`<article class="card" data-model-id="${esc(m.id)}"><div class="card-top"><span class="eyebrow">${esc(m.mode_name || p.name)}</span>${badge(p.inference_available?'ready':'manual_action',p.inference_available?'API 已接入':'仅目录')}</div><h3>${esc(m.name || m.id)}</h3><code>${esc(m.id)}</code><div class="tags">${m.native_tools?badge('ready','官方工作工具'):''}${m.supports_tool_call?badge('ready','API 工具调用'):''}${m.supports_images?badge('ready','图像输入'):''}${m.supports_reasoning?badge('ready','推理'):''}${m.need_login?badge('manual_action','需要登录'):''}${(m.tiers || []).map(t=>badge('neutral',t)).join('')}</div><dl><div><dt>上下文</dt><dd>${m.context_length?number(m.context_length):'上游未声明'}</dd></div><div><dt>推理档位</dt><dd>${esc((m.reasoning_efforts || []).map(x=>x.name || x.level).join(' / ') || '上游未声明')}</dd></div></dl>${m.account_id?`<p>账号 <code>${esc(m.account_id)}</code> · ${esc(labels[m.catalog_status])}</p>`:''}</article>`).join('')}</div>`:empty('没有匹配的模型','调整搜索、能力或账号筛选；账号不可用时请先检查登录状态。');
  }
  async function tasks() {
    state.taskProvider=state.taskProvider || state.providers[0]?.id || '';
    $('#view').innerHTML=heading(button('run-all','执行当前范围','','primary'))+`<div class="filters two"><label>服务<select id="task-provider">${providerOptions(state.taskProvider)}</select></label><label>账号<select id="task-account"><option>读取中…</option></select></label></div><div id="task-list" class="loading">正在发现任务…</div>`;
    $('[data-action=run-all]').disabled=true;
    $('#task-provider').onchange=e=>{state.taskProvider=e.target.value;state.taskAccount='';reload();};
    const accounts=await accountsFor(state.taskProvider);
    $('#task-account').innerHTML=options([['','全部已启用账号'],...accounts.filter(a=>a.enabled).map(a=>[a.id,a.id])],state.taskAccount);
    $('#task-account').onchange=e=>{state.taskAccount=e.target.value;reload();};
    const query=new URLSearchParams({provider:state.taskProvider});if(state.taskAccount)query.set('accounts',state.taskAccount);
    const data=await api('workbench/catalog?'+query);state.tasks=data.tasks || [];
    $('#task-list').className='';$('#task-list').innerHTML=state.tasks.length?`<div class="grid">${state.tasks.map((t,i)=>`<article class="card" data-task="${esc(t.key)}"><div class="card-top"><span class="eyebrow">${esc(providerName(t.provider_id))}</span>${badge(t.manual_only?'manual_action':'ready',t.manual_only?'需人工操作':'可执行')}</div><h3>${esc(t.name)}</h3><p>${esc(t.description)}</p><div class="tags">${badge('neutral',t.once?'一次性':t.once_per_day?'每日任务':'按需执行')}${t.reward?badge('ready',t.reward):''}</div><p>账号 <code>${esc(t.account_id)}</code></p><div class="actions">${button(t.manual_only?'manual-task':'run-task',t.manual_only?'查看所需操作':'执行任务',`data-index="${i}"`,'compact')}${!t.manual_only?button('task-schedule','安排计划',`data-index="${i}"`,'quiet compact'):''}</div></article>`).join('')}</div>`:empty('未发现可用任务','请确认账号已启用，并在需要时完成官方客户端登录。');
    $('[data-action=run-all]').disabled=!state.tasks.some(t=>!t.manual_only);
  }
  function confirmRun(task) {
    const scope=task?[task.account_id]:state.taskAccount?[state.taskAccount]:[];
    const req={provider_id:task?.provider_id || state.taskProvider,task_key:task?.key || '*',account_ids:scope};
    if(sessionStorage.getItem('loomgate.unconfirmedRun')) {toast('上次执行请求结果未确认。请在执行记录核对后再继续，避免重复操作。');location.hash='runs';return;}
    openDialog('确认执行范围',`<form id="run-form" class="form"><p>${esc(task?.name || '当前范围内的全部任务')}</p><div class="notice">服务：${esc(providerName(req.provider_id))}<br>账号：${esc(scope.join('、') || '全部已启用账号')}<br>执行可能产生上游操作。需要人工的任务会保留待处理状态，奖励以服务端复核为准。</div><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认执行</button></div></form>`);
    $('#run-form').onsubmit=e=>{e.preventDefault();if(state.pending.has('run'))return;sessionStorage.setItem('loomgate.unconfirmedRun','1');mutate('run','workbench/run',req,async data=>{if(!data.run?.id)throw new APIError('request_failed',502);sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();toast('已进入执行队列，结果以执行记录为准。');location.hash='runs';});};
  }
  async function schedules() {
    const data=await api('workbench');state.schedules=data.schedules || [];
    $('#view').innerHTML=heading(button('new-schedule','新建计划','','primary'))+(state.schedules.length?table(['计划 / 任务','服务与范围','执行时间','状态','操作'],state.schedules.map((s,i)=>[`<code>${esc(s.id)}</code><p>${esc(s.task_key || '*')}</p>`,`${esc(providerName(s.provider_id))}<p>${esc(s.account_ids?.length?s.account_ids.join('、'):'全部已启用账号')}</p>`,`${s.hours.map(h=>String(h).padStart(2,'0')+':'+String(s.minute).padStart(2,'0')).join(' / ')}<p>${esc(s.timezone)} · ${s.once_per_day?'每日最多一次':'每个时段执行'}</p><p>下次 ${esc(date(s.next_run))}</p>`,badge(s.enabled?'ready':'disabled',s.enabled?'已启用':'已停用'),`<div class="actions">${button('edit-schedule','编辑',`data-index="${i}"`,'compact')}${button('toggle-schedule',s.enabled?'停用':'启用',`data-index="${i}" data-lock`,'compact')}${button('delete-schedule','删除',`data-index="${i}"`,'compact danger')}</div>`])):empty('为工作安排一个时间','从动态任务目录中选择任务，计划保存后由服务端调度。'));
  }
  function cleanSchedule(s) {return {id:s.id,provider_id:s.provider_id,task_key:s.task_key,account_ids:s.account_ids || [],hours:s.hours,minute:s.minute,timezone:s.timezone,enabled:s.enabled,once_per_day:s.once_per_day};}
  async function verifySchedule(s) {const result=await api('workbench');const saved=(result.schedules || []).find(x=>x.id===s.id);if(!saved || JSON.stringify(cleanSchedule(saved))!==JSON.stringify(cleanSchedule(s)))throw new APIError('request_failed',502);}
  async function scheduleEditor(existing, task) {
    const s=existing?cleanSchedule(existing):{id:'plan-'+crypto.randomUUID().slice(0,12),provider_id:task?.provider_id || state.providers[0]?.id,task_key:task?.key || '',account_ids:task?[task.account_id]:[],hours:[9],minute:0,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai',enabled:false,once_per_day:true};
    openDialog(existing?'编辑执行计划':'新建执行计划',`<form id="schedule-form" class="form"><div class="form-row"><label>服务<select id="plan-provider">${providerOptions(s.provider_id)}</select></label><label>任务<select id="plan-task" required disabled><option value="">正在发现任务…</option></select></label></div><label>账号范围</label><div id="plan-accounts" class="account-checks">正在读取账号…</div><div class="form-row"><label>执行小时（0–23，逗号分隔）<input id="plan-hours" value="${esc(s.hours.join(','))}" required pattern="[0-9, ]+"></label><label>分钟<input id="plan-minute" type="number" min="0" max="59" value="${s.minute}" required></label></div><label>时区<input id="plan-timezone" value="${esc(s.timezone)}" list="timezones" required><datalist id="timezones"><option>Asia/Shanghai</option><option>UTC</option><option>America/New_York</option><option>Europe/London</option></datalist></label><label class="check"><input id="plan-daily" type="checkbox" ${s.once_per_day?'checked':''}>同一计划每天最多执行一次</label><label class="check"><input id="plan-enabled" type="checkbox" ${s.enabled?'checked':''}>保存后启用</label><p class="muted small">任务自身的每日 / 一次性规则仍由服务端约束。未勾选“启用”的计划会保存为停用状态。</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button id="plan-save" type="submit" class="primary" disabled>保存计划</button></div></form>`);
    let loadID=0;
    async function load() {
      const id=++loadID,p=$('#plan-provider').value;$('#plan-save').disabled=true;$('#plan-task').disabled=true;
      try {
        const [aa,data]=await Promise.all([accountsFor(p),api('workbench/catalog?'+new URLSearchParams({provider:p}))]);
        if(id!==loadID || !$('#dialog').open || !$('#plan-task'))return;
        const keys=new Map((data.tasks || []).filter(t=>!t.manual_only).map(t=>[t.key,t.name]));
        $('#plan-task').innerHTML=options([['','选择一个任务'],...keys.entries()],p===s.provider_id?s.task_key:'');$('#plan-task').disabled=false;
        $('#plan-accounts').innerHTML=aa.filter(a=>a.enabled || s.account_ids.includes(a.id)).map(a=>`<label class="check"><input type="checkbox" name="plan-account" value="${esc(a.id)}" ${p===s.provider_id && s.account_ids.includes(a.id)?'checked':''}>${esc(a.id)}${a.enabled?'':' · 已停用'}</label>`).join('') || '<span class="muted small">没有可选账号</span>';
        $('#plan-save').disabled=!keys.size || !aa.length;
        if(existing && !keys.has(s.task_key)) {$('#form-message').textContent='原任务当前不可发现，请选择有效任务；未保存前原计划保持不变。';$('#form-message').hidden=false;}
      } catch(e) {if(e.name==='AbortError')return;const message=$('#form-message');if(message){message.textContent='任务或账号读取失败。请关闭后重试。';message.hidden=false;}}
    }
    $('#plan-provider').onchange=load;
    $('#schedule-form').onsubmit=event=>{
      event.preventDefault();const error=$('#form-message');error.hidden=true;
      const hours=$('#plan-hours').value.split(',').map(x=>x.trim());const ids=[...document.querySelectorAll('input[name=plan-account]:checked')].map(x=>x.value);
      if(!hours.length || hours.some(x=>!/^\d+$/.test(x) || +x>23) || new Set(hours.map(Number)).size!==hours.length || !ids.length) {error.textContent='请至少选择一个账号，并输入不重复的 0–23 小时。';error.hidden=false;return;}
      const payload={...s,provider_id:$('#plan-provider').value,task_key:$('#plan-task').value,account_ids:ids,hours:hours.map(Number).sort((a,b)=>a-b),minute:+$('#plan-minute').value,timezone:$('#plan-timezone').value.trim(),enabled:$('#plan-enabled').checked,once_per_day:$('#plan-daily').checked};
      mutate('schedule','workbench/schedule',payload,async()=>{await verifySchedule(payload);closeDialog();toast('计划已保存，并已从服务端核验。');if(state.route==='schedules')await reload();else location.hash='schedules';});
    };
    await load();
  }
  function runTimeline(rows) {return `<ol class="timeline">${rows.map(r=>`<li><time>${esc(date(r.started_at))}</time><div><h3>${esc(r.task_key || '全部任务')}</h3><p>${esc(providerName(r.provider_id))} · ${esc(labels[r.stage] || '等待派发')}</p></div>${badge(r.status)}</li>`).join('')}</ol>`;}
  async function runs() {
    const data=await api('workbench');state.runs=data.runs || [];
    $('#view').innerHTML=heading(button('reload','刷新记录'))+(sessionStorage.getItem('loomgate.unconfirmedRun')?notice('有一次执行提交结果未确认。请核对执行时间、服务和账号；不要直接重复提交。','warn')+button('review-submission','核对后解除提交保护'):'')+`<div id="run-poll" class="muted small" role="status"></div>`+(state.runs.length?table(['执行','服务与范围','进度','开始时间','操作'],state.runs.map((r,i)=>[`<code>${esc(r.id)}</code><p>${esc(r.task_key || '全部任务')}</p>`,`${esc(providerName(r.provider_id))}<p>${esc(r.account_ids?.join('、') || '全部已启用账号')}</p>`,badge(r.status)+`<p>${esc(labels[r.stage] || '等待派发')}</p>`,date(r.started_at),`<div class="actions">${button('run-detail','详情',`data-index="${i}"`,'compact')}${['queued','running'].includes(r.status)?button('cancel-run','取消',`data-index="${i}" data-lock`,'compact danger'):''}</div>`])):empty('还没有执行记录','执行请求会先进入队列；完成结果在此显示。'));
    if(state.runs.some(r=>['queued','running'].includes(r.status))) {
      if(!state.pollStarted)state.pollStarted=Date.now();
      if(Date.now()-state.pollStarted<90000){$('#run-poll').textContent='正在同步执行状态；离开页面将停止同步。';const v=state.version;state.poll=setTimeout(()=>{if(v===state.version)runs().catch(fail);},Math.min(8000,1000+Math.floor((Date.now()-state.pollStarted)/8000)*1000));}
      else $('#run-poll').textContent='自动同步已达到 90 秒上限。任务仍在服务端运行，可点击刷新继续查看。';
    } else state.pollStarted=0;
  }
  async function usage() {
    const data=await api('usage'), totals=data.totals;
    if(!totals)throw new APIError('request_failed',503);
    $('#view').innerHTML=heading(button('reload','刷新用量'))+`<div class="metrics">${[['请求数',totals.requests],['失败请求',totals.failed_requests],['Token 总量',totals.total_tokens],['已报告消耗',totals.credit]].map(([label,n])=>`<div class="metric"><span>${label}</span><strong>${number(n)}</strong></div>`).join('')}</div>`+notice(`账本开始于 ${date(data.since)}。未报告用量的请求：${totals.unreported_requests ?? '未知'}。缺失数据不会按零消耗推断。`)+(data.days?.length?table(['日期','请求','Token','消耗'],data.days.slice(0,30).map(d=>[esc(d.day),number(d.totals.requests),number(d.totals.total_tokens),number(d.totals.credit)])):empty('暂无每日用量','网关收到实际调用后将写入用量账本。'));
  }
  async function logs() {
    const data=await api('requests?limit=30&offset='+String(state.logOffset || 0));
    const rows=data.requests || data.items || data.rows;
    if(!Array.isArray(rows))throw new APIError('request_failed',503);
    $('#view').innerHTML=heading(button('reload','刷新记录'))+(rows.length?table(['请求','模型 / 协议','状态','耗时','时间'],rows.map(r=>[`<code>${esc(r.request_id)}</code>`,`${esc(r.model)}<p>${esc(r.protocol)}</p>`,esc(r.status || r.outcome || '未知'),r.duration_ms!=null?number(r.duration_ms)+' ms':'—',date(r.started_at)])):empty('暂无请求记录','记录由网关真实请求生成，不包括面板的目录读取。'))+`<div class="pagination"><span>共 ${number(data.total)} 条</span><div class="actions">${button('logs-prev','上一页',!state.logOffset?'disabled':'')}${button('logs-next','下一页',(state.logOffset || 0)+30>=data.total?'disabled':'')}</div></div>`;
  }
  async function settings() {
    const session=await api('session');
    $('#view').innerHTML=heading()+`<div class="split"><section class="panel"><div class="panel-title"><div><span class="eyebrow">WORKSPACE</span><h2>显示与会话</h2></div></div><div class="settings-line"><div><h3>界面主题</h3><p>在此浏览器保存偏好。</p></div>${button('theme','切换主题')}</div><div class="settings-line"><div><h3>当前会话</h3><p>有效期至 ${esc(date(session.expiresAt))}</p></div>${badge('ready','已登录')}</div><div class="settings-line"><div><h3>管理账号安全</h3><p>修改密码后其他设备需重新登录。</p></div>${button('password','修改密码')}</div></section><section class="panel"><div class="panel-title"><div><span class="eyebrow">MIGRATION</span><h2>迁移与更新</h2></div></div>${notice('新工作区正在分阶段验收，尚未开放生产切换。','warn')}<p class="muted">配置与账号迁移工具已提供副本演练能力。真实账号迁移、三平台更新与签名连续性仍需验收。</p><div class="section-gap">${badge('manual_action','待独立验收')}</div></section></div>`;
  }
  function passwordDialog() {
    openDialog('修改管理账号',`<form id="password-form" class="form"><label>当前密码<input name="current" type="password" autocomplete="current-password" required></label><label>用户名<input name="username" autocomplete="username" required pattern="[A-Za-z0-9_.-]{3,32}"></label><label>新密码<input name="password" type="password" autocomplete="new-password" minlength="8" required></label><label>再次输入新密码<input name="confirm" type="password" autocomplete="new-password" required></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);
    $('#password-form').onsubmit=e=>{e.preventDefault();const body=Object.fromEntries(new FormData(e.target));if(body.password!==body.confirm){$('#form-message').textContent='两次新密码不一致。';$('#form-message').hidden=false;return;}mutate('password','auth/password',body,async()=>{closeDialog();toast('管理账号已更新。');await reload();});};
  }
  const renderers={overview,providers:services,accounts,models:modelPage,tasks,schedules,runs,usage,logs,settings};
  async function reload() {state.providers=null;state.pollStarted=0;await navigate();}
  async function action(element) {
    const name=element.dataset.action,index=Number(element.dataset.index);
    if(name==='close'){closeDialog();return;}
    if(name==='reload'){await reload();return;}
    if(name==='theme'){toggleTheme();return;}
    if(name==='provider-models' || name==='account-models'){state.modelProvider=element.dataset.provider;state.modelAccount=element.dataset.account || '';state.modelOffset=0;location.hash='models';return;}
    if(name==='provider-accounts'){state.accountProvider=element.dataset.provider;location.hash='accounts';return;}
    if(name==='connect-account'||name==='reconnect-account'){await LoomConnections.open(element.dataset.provider||$('#account-provider').value,element.dataset.account||'');return;}
    if(name==='toggle-account'){
      const a=state.accountRows[index],provider=element.dataset.provider;openDialog(a.enabled?'停用账号':'启用账号',`<form id="account-toggle-form" class="form"><p>账号 <code>${esc(a.id)}</code></p><p class="muted">${a.enabled?'停用后不再用于后续目录请求与任务。正在进行的操作应在执行记录中单独核验。':'启用后该账号可再次用于目录读取和任务。'}</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认${a.enabled?'停用':'启用'}</button></div></form>`);
      $('#account-toggle-form').onsubmit=e=>{e.preventDefault();const modern=state.providers.find(p=>p.id===provider)?.catalog_scope==='account';mutate('account-toggle',modern?'workbench/account/toggle':'account/toggle',modern?{id:a.id,enabled:!a.enabled,revision:a.revision}:{uid:a.id,disabled:a.enabled},async()=>{const updated=(await accountsFor(provider)).find(x=>x.id===a.id);if(!updated||updated.enabled===a.enabled)throw new APIError('request_failed',502);closeDialog();toast('账号状态已从服务端核验。');await reload();});};return;
    }
    if(name==='model-refresh'){await loadModels(true);return;}
    if(name==='model-next' || name==='model-prev'){state.modelOffset=Math.max(0,state.modelOffset+(name==='model-next'?8:-8));await loadModels(false);return;}
    if(name==='connection-help'){openDialog('账号接入状态',notice('现有账号已通过真实后端读取。新控制台的扫码、手机号和授权流程仍在接入，当前不会发送短信或自动开始登录。','warn')+'<p class="muted">豆包需在官方客户端完成登录；会话导入与续期界面将在后续阶段开放。</p>');return;}
    if(name==='manual-task'){const t=state.tasks[index];openDialog('所需人工操作',`<h3>${esc(t.name)}</h3><p class="section-gap">${esc(t.description)}</p>${notice('请在对应官方服务完成要求的内容或操作。完成与奖励以服务端核验为准。','warn')}`);return;}
    if(name==='run-task' || name==='run-all'){confirmRun(name==='run-task'?state.tasks[index]:null);return;}
    if(name==='review-submission'){openDialog('核对未确认的提交',`<form id="review-form" class="form"><p>请先检查执行记录和官方服务状态。解除保护不会重试或取消原请求。</p><label class="check"><input type="checkbox" required>我已核对上次提交，确认不会重复执行已有操作</label><div class="actions">${button('close','返回')}<button type="submit">解除提交保护</button></div></form>`);$('#review-form').onsubmit=e=>{e.preventDefault();sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();reload();};return;}
    if(name==='new-schedule' || name==='edit-schedule' || name==='task-schedule'){await scheduleEditor(name==='edit-schedule'?state.schedules[index]:null,name==='task-schedule'?state.tasks[index]:null);return;}
    if(name==='toggle-schedule'){const s={...cleanSchedule(state.schedules[index]),enabled:!state.schedules[index].enabled};await mutate('schedule','workbench/schedule',s,async()=>{await verifySchedule(s);toast(s.enabled?'计划已启用。':'计划已停用。');await reload();});return;}
    if(name==='delete-schedule'){const s=state.schedules[index];openDialog('删除执行计划',`<form id="delete-form" class="form"><p>删除计划 <code>${esc(s.id)}</code> 后将停止后续派发。已有执行记录保留。</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="danger">删除计划</button></div></form>`);$('#delete-form').onsubmit=e=>{e.preventDefault();mutate('delete','workbench/schedule/delete',{id:s.id},async()=>{const data=await api('workbench');if(data.schedules.some(x=>x.id===s.id))throw new APIError('request_failed',502);closeDialog();toast('计划已删除，并已从服务端核验。');await reload();});};return;}
    if(name==='cancel-run'){const r=state.runs[index];await mutate('cancel-'+r.id,'workbench/cancel',{id:r.id},async data=>{toast(data.cancel_requested?'已发送取消请求，等待服务端确认。':'任务已结束或无法取消，请查看最新状态。');await reload();});return;}
    if(name==='run-detail'){const r=state.runs[index];openDialog('执行详情',`<p><code>${esc(r.id)}</code></p><div class="section-gap">${badge(r.status)}</div>${r.status==='uncertain'?notice('上游操作结果尚未确认，请先核验服务端状态，不要重复执行。','warn'):''}<div class="section-gap">${r.steps?.length?table(['阶段','账号','结果'],r.steps.map(s=>[esc(labels[s.stage] || s.stage),`<code>${esc(s.account_id)}</code>`,badge(s.status)])):empty('尚无执行步骤','任务可能仍在等待派发。')}</div>`);return;}
    if(name==='logs-prev' || name==='logs-next'){state.logOffset=Math.max(0,(state.logOffset || 0)+(name==='logs-next'?30:-30));await reload();return;}
    if(name==='password')passwordDialog();
  }
  function toggleTheme(){const theme=document.documentElement.dataset.theme==='dark'?'light':'dark';document.documentElement.dataset.theme=theme;try{localStorage.setItem('loomgate.theme',theme);}catch{}}
  try{document.documentElement.dataset.theme=localStorage.getItem('loomgate.theme') || 'light';}catch{}
  $('#theme').onclick=toggleTheme;$('#dialog-close').onclick=closeDialog;$('#dialog').addEventListener('cancel',()=>$('#dialog-body').replaceChildren());
  document.addEventListener('click',e=>{const target=e.target.closest('[data-action]');if(target && !target.disabled)action(target).catch(err=>{if(err.name!=='AbortError')toast(err instanceof APIError?err.message:'操作未完成，请刷新状态后重试。');});});
  $('#logout').onclick=()=>mutate('logout','auth/logout',{},async()=>location.replace('login'));
  const network=()=>{$('#connection').hidden=navigator.onLine;$('#connection').textContent='当前网络离线。页面中的数据可能已过期，恢复连接后请刷新。';};window.addEventListener('offline',network);window.addEventListener('online',network);network();
  LoomConnections.init({api,esc,openDialog,toast,reload,providerName,accountConnected:f=>{state.accountProvider=f.provider;}});
  window.addEventListener('hashchange',navigate);window.addEventListener('pagehide',()=>{state.controller?.abort();clearTimeout(state.poll);LoomConnections.stop();});navigate();
})();

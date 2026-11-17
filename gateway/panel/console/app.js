'use strict';
(() => {
  const $ = (selector, root = document) => root.querySelector(selector);
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const date = value => value && !String(value).startsWith('0001-') ? new Date(typeof value === 'number' ? value * 1000 : value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const number = value => typeof value === 'number' ? value.toLocaleString('zh-CN') : '—';
  const state = {route:'', version:0, controller:null, providers:null, models:[], catalogs:[], modelProvider:'', modelAccount:'', modelOffset:0, search:'', capability:'', taskProvider:'', taskAccount:'', tasks:[], schedules:[], runs:[], pending:new Set(), poll:null};
  const routes = {overview:['','工作台','先处理需要关注的事项。'], providers:['CONNECTIONS','服务接入','按服务查看已接入账号与真实可用能力。'], accounts:['IDENTITIES','账号','集中查看服务账号的启用状态、会话有效期和目录入口。'], models:['MODEL LIBRARY','模型目录','从上游获取可选模型。能力与可用范围以当前账号的响应为准。'], tasks:['AUTOMATION','任务中心','发现服务提供的任务，按真实执行结果核验进度。'], schedules:['SCHEDULES','执行计划','选择任务与账号，设置执行时间、时区及每日规则。'], runs:['ACTIVITY','执行记录','查看每次派发、执行与服务端复核的结果。'], usage:['OBSERVABILITY','用量','查看网关累计请求与上游报告的用量。'], logs:['REQUEST HISTORY','请求记录','按请求查看模型、状态和耗时，不展示消息内容。'], keys:['API KEYS','访问密钥','创建、限额与停用，密钥只在创建时显示一次。'], migration:['MIGRATION','迁移','只读核对迁移标记与工具状态；导入需离线确认。'], settings:['PREFERENCES','工作区设置','管理显示偏好、会话与迁移准备。']};
  const labels = {ready:'目录已同步',stale:'旧缓存',unavailable:'暂不可用',disabled:'已停用',queued:'等待执行',running:'执行中',succeeded:'已核验完成',failed:'失败',canceled:'已取消',uncertain:'待复核',skipped:'已跳过',manual_action:'需要人工',discover:'发现',inspect:'检查',execute:'执行',claim:'领取',verify:'复核'};
  const errorLabels = {desktop_unavailable:'官方豆包客户端未连接，请打开客户端并检查本机连接设置。',desktop_identity_mismatch:'客户端当前账号与已绑定账号不同，请切回原账号后刷新。',desktop_login_required:'请先在官方豆包客户端完成登录。',desktop_version_unsupported:'当前客户端版本尚未适配，请更新连接组件。',desktop_timeout:'官方客户端响应超时，请稍后刷新。',desktop_busy:'官方客户端正在处理目录请求，请稍后刷新。',desktop_upstream_rejected:'官方服务拒绝目录请求，请在客户端检查登录状态。',login_required:'登录已失效，请在官方客户端重新登录。',accounts_unavailable:'账号存储读取失败，请检查服务状态。',catalog_timeout:'目录读取超时，请稍后重试。',catalog_failed:'目录读取失败，请稍后重试。',catalog_unavailable:'上游目录暂不可用。',account_not_found:'账号已不存在，请刷新账号列表。',schedule_not_saved:'计划未保存，请检查时间、时区和任务范围。',run_rejected:'执行请求未被接受，可能已有任务正在运行。',unauthorized:'管理会话已过期，请重新登录。'};
  Object.assign(errorLabels,{desktop_cleanup_uncertain:'客户端清理状态未确认，请检查官方客户端后再试。',desktop_ownership_lost:'本次连接的归属已变化，请重新连接官方客户端。',desktop_submission_uncertain:'请求可能已提交，请先核对官方服务状态，避免重复提交。',composer_not_empty:'官方客户端有未发送内容，请先保留或处理草稿。',desktop_model_unavailable:'当前账号的实时目录中没有此模型，请刷新目录。',desktop_native_tools_unsupported:'该请求需要官方任务运行时，但当前选择或任务上下文不匹配，未发送。',provider_output_budget_unsupported:'官方签名请求没有可兑现的输出 token 上限，本次未发送。',provider_system_instructions_unsupported:'官方签名构造器没有可验证的系统指令字段，本次未发送。'});
  const badge = (value, text) => `<span class="badge ${['failed','unavailable','uncertain'].includes(value)?'bad':['stale','manual_action'].includes(value)?'warn':['disabled','skipped','canceled'].includes(value)?'neutral':''}" data-status="${esc(value)}">${esc(text || labels[value] || value)}</span>`;
  const button = (action, text, extra = '', cls = '') => `<button data-action="${action}" class="${cls}" ${extra}>${text}</button>`;
  const empty = (title, detail = '') => `<div class="empty"><span class="empty-icon" aria-hidden="true">◇</span><h3>${esc(title)}</h3><p>${esc(detail)}</p></div>`;
  const notice = (text, kind='') => `<div class="notice ${kind}" role="status">${esc(text)}</div>`;
  const options = (rows, value) => rows.map(([id,name]) => `<option value="${esc(id)}" ${id===value?'selected':''}>${esc(name)}</option>`).join('');
  const providerName = id => state.providers?.find(p=>p.id===id)?.name || id;
  function providerFilter(id, value) {
    return `<div class="provider-filter"><span id="${id}-label" class="filter-label">服务</span><div id="${id}" class="provider-choices" role="group" aria-labelledby="${id}-label">${(state.providers||[]).map(p=>`<button type="button" data-provider-choice="${esc(p.id)}" aria-pressed="${p.id===value}">${esc(p.name)}</button>`).join('')}</div></div>`;
  }
  function bindProviderFilter(id, change) {
    $('#'+id).onclick=async e=>{
      const target=e.target.closest('[data-provider-choice]');if(!target||target.getAttribute('aria-pressed')==='true')return;
      const route=state.route,value=target.dataset.providerChoice;
      change(value);await reload();
      if(state.route===route)[...document.querySelectorAll('#'+id+' button')].find(b=>b.dataset.providerChoice===value)?.focus({preventScroll:true});
    };
  }
  const accountIcon='<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="8" r="3.5"/><path d="M5 21v-2a7 7 0 0 1 14 0v2"/></svg>';
  const accountScope = id => `<button type="button" id="${id}" class="account-scope" aria-haspopup="dialog" disabled>${accountIcon}<span><small>账号范围</small><strong>正在读取账号…</strong></span></button>`;
  function bindAccountScope(id, rows, value, allLabel, change) {
    const trigger=$('#'+id),label=value||allLabel;
    trigger.disabled=false;trigger.setAttribute('aria-label','账号范围：'+label+'，更改');
    trigger.innerHTML=`${accountIcon}<span><small>账号范围</small><strong>${esc(label)}</strong></span><span class="scope-change">更改 <span aria-hidden="true">›</span></span>`;
    trigger.onclick=()=>{
      const route=state.route;
      const choice=(key,title,detail)=>`<button type="button" class="account-choice" data-account-choice="${esc(key)}" aria-pressed="${key===value}"><span class="choice-mark" aria-hidden="true">${key===value?'✓':''}</span><span><strong>${esc(title)}</strong><small>${esc(detail)}</small></span></button>`;
      openDialog('选择账号范围',`<p class="scope-description">${esc(providerName(state.route==='tasks'?state.taskProvider:state.modelProvider))} · 选择账号后立即应用</p><label class="scope-search">搜索账号<input id="scope-search" type="search" autocomplete="off" placeholder="输入账号标识" autofocus></label><div id="scope-all">${choice('',allLabel,`${rows.length} 个账号`)}</div><div class="scope-results"><p id="scope-count" class="small muted" role="status" aria-live="polite"></p><div id="scope-accounts" class="account-choices"></div></div><div class="scope-footer">${button('close','取消','','quiet')}</div>`,'account-dialog');
      const draw=()=>{
        const query=$('#scope-search').value.trim().toLowerCase(),filtered=rows.filter(a=>String(a.id).toLowerCase().includes(query));
        $('#scope-count').textContent=`${query?'匹配':'可选'} ${filtered.length} 个账号`;
        $('#scope-accounts').innerHTML=filtered.length?filtered.map(a=>choice(a.id,a.id,a.enabled?(a.realm==='global'?'国际区 · 已启用':a.realm==='cn'?'国内区 · 已启用':'已启用'):'已停用 · 可查看目录状态')).join(''):empty(query?'没有匹配的账号':'暂无可选账号',query?'试试其他账号标识，或清空搜索。':'接入并启用账号后，可在这里切换范围。');
      };
      const select=async e=>{
        const target=e.target.closest('[data-account-choice]');if(!target)return;
        const next=target.dataset.accountChoice;closeDialog();
        if(next!==value){try{await change(next);}catch(err){if(err.name!=='AbortError')toast(err instanceof APIError?err.message:'账号范围未能读取，请重试。');}}
        if(state.route===route)$('#'+id)?.focus({preventScroll:true});
      };
      $('#scope-all').onclick=select;$('#scope-accounts').onclick=select;$('#scope-search').oninput=draw;draw();$('#scope-search').focus();
    };
  }
  function toast(text) { $('#toast').textContent = text; $('#toast').hidden = false; clearTimeout(toast.timer); toast.timer = setTimeout(()=>$('#toast').hidden=true,6000); }
  function heading(actions='') { const r=routes[state.route]; return `<div class="page-heading"><div><h1>${r[1]}</h1><p>${r[2]}</p></div><div class="actions">${actions}</div></div>`; }
  function table(headers, rows) { return `<div class="table-wrap"><table class="responsive"><thead><tr>${headers.map(x=>`<th>${esc(x)}</th>`).join('')}</tr></thead><tbody>${rows.map(row=>`<tr>${row.map((cell,i)=>`<td data-label="${esc(headers[i])}"><div>${cell}</div></td>`).join('')}</tr>`).join('')}</tbody></table></div>`; }
  class APIError extends Error { constructor(code,status,detail='') { super(detail || errorLabels[code] || (status===403?'没有执行此操作的权限，请重新登录。':status===429?'请求过于频繁，请稍后重试。':'请求未完成，请检查服务状态后重试。')); this.code=code; this.status=status; } }
  async function api(path, {body, signal=state.controller?.signal, allowNotOK=false} = {}) {
    const controller=new AbortController(), abort=()=>controller.abort();
    if(signal?.aborted) controller.abort(); else signal?.addEventListener('abort',abort,{once:true});
    const timer=setTimeout(()=>controller.abort(),path==='connections/start'?40000:18000);
    try {
      const response=await fetch('api/'+path,{method:body===undefined?'GET':'POST',credentials:'same-origin',cache:'no-store',signal:controller.signal,headers:body===undefined?{}:{'Content-Type':'application/json','X-Admin-Request':'1'},body:body===undefined?undefined:JSON.stringify(body)});
      const data=await response.json();
      if(!response.ok || data.ok===false && !allowNotOK) throw new APIError(data.error || data.error_code || 'request_failed',response.status,typeof data.message==='string'?data.message:'');
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
  function fail(e) { if(e.name==='AbortError') return; $('#view').innerHTML=heading()+notice(e instanceof APIError?e.message:'无法连接工作区，请检查网络或服务状态。','error')+button('reload','重新读取')+(e.status===401?' <a href="login">重新登录 →</a>':''); $('#view').dataset.loadState='failed'; }
  function openDialog(title, html, style='') { $('#dialog').className=style; $('#dialog-title').textContent=title; $('#dialog-body').innerHTML=html; if(!$('#dialog').open) $('#dialog').showModal(); }
  function closeDialog() { $('#dialog').close();$('#dialog').className=''; $('#dialog-body').replaceChildren(); }
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
    state.route=Object.hasOwn(routes,route)?route:'overview';begin();closeDialog();closeNavigation();
    document.querySelectorAll('#navigation a').forEach(a=>{if(a.hash==='#'+state.route)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');});
    document.title=routes[state.route][1]+' · LoomGate';
    $('#current-page').textContent=routes[state.route][1];
    $('#view').innerHTML=heading()+`<div class="loading"><p>正在读取工作区…</p><div class="progress"></div></div>`;
    try { await providers(); await renderers[state.route](); $('#view').dataset.loadState='ready'; } catch(e) {fail(e);}
  }

  async function overview() {
    const work=await api('workbench'),ps=state.providers,schedules=work.schedules||[],rows=work.runs||[];
    state.runs=rows;
    const attention=rows.filter(r=>['uncertain','failed','running','queued'].includes(r.status));
    $('#view').innerHTML=heading(button('reload','刷新状态','','quiet'))+`<div class="agenda-summary"><strong>${ps.reduce((n,p)=>n+p.accounts.total,0)}</strong> 个账号，<strong>${schedules.filter(s=>s.enabled).length}</strong> 个计划已启用。</div><div class="agenda-layout"><div><section class="work-section" data-workspace-agenda><div class="section-heading"><h2>需要关注</h2><a href="#tasks">打开任务</a></div>${attention.length?attention.slice(0,6).map(r=>`<div class="attention-line">${badge(r.status)}<div class="line-content"><h3>${esc(r.task_key||'全部任务')}</h3><p>${esc(providerName(r.provider_id))}，${esc(labels[r.stage]||'等待派发')}。${r.status==='uncertain'?'结果未确认，请先核验，避免重复提交。':''}</p></div>${button('run-detail','查看',`data-index="${rows.indexOf(r)}"`,'quiet')}</div>`).join(''):empty('当前没有待处理的执行','可以到任务中心发现工作，或为已有任务安排计划。')}</section><section class="work-section"><div class="section-heading"><h2>最近执行</h2><a href="#runs">查看全部</a></div>${rows.length?runTimeline(rows.slice(0,6)):empty('还没有执行记录','首次执行后，这里会显示经过服务端核验的结果。')}</section></div><aside class="service-aside"><h2>服务与账号</h2>${ps.map(p=>`<div class="service-note"><div><h3>${esc(p.name)}</h3><p>${p.accounts.enabled} / ${p.accounts.total} 个账号启用<br>${p.inference_available?'已接入推理':'目录已接入，推理待接通'}</p></div>${button('provider-models','查看目录',`data-provider="${esc(p.id)}"`,'text-link')}</div>`).join('')}</aside></div>`;
  }
  async function services() {
    $('#view').innerHTML=heading(button('reload','同步服务','','quiet'))+`<div class="service-list">${state.providers.map(p=>`<article class="service-entry" data-provider="${esc(p.id)}"><div><h2>${esc(p.name)}</h2><p>${p.catalog_scope==='account'?'目录随账号变化':'服务级模型目录'}</p></div><div><dl><div><dt>已连接账号</dt><dd>${p.accounts.total}</dd></div><div><dt>已启用</dt><dd>${p.accounts.enabled}</dd></div></dl><p>${p.inference_available?'消息推理已接入。请求仍受账号余额和上游可用性限制。':'模型目录可读取；消息转发与双登录尚未接通。'}</p>${p.error_code?notice(errorLabels[p.error_code]||'服务状态读取失败','error'):''}</div><div class="actions">${button('provider-models','查看模型',`data-provider="${esc(p.id)}"`,'quiet')}${button('provider-accounts','查看账号',`data-provider="${esc(p.id)}"`,'quiet')}</div></article>`).join('')}</div>`;
  }

  async function accounts() {
    const selected=state.accountProvider || state.providers[0]?.id || '';
    state.accountProvider=selected;const generation=state.version;
    $('#view').innerHTML=heading(button('connect-account','连接账号','','primary'))+`<div class="context-bar scope-toolbar">${providerFilter('account-provider',selected)}<label class="search"><span class="sr-only">搜索账号标识</span><input id="account-search" placeholder="搜索账号标识"></label></div><div id="client-status"></div><div id="account-list" class="loading">正在读取账号…</div><div id="connection-list"></div>`;
    bindProviderFilter('account-provider',value=>{state.accountProvider=value;});
    const rows=await accountsFor(selected);if(generation!==state.version)return;state.accountRows=rows;const clientStates=new Map();
    const draw=()=>{const query=$('#account-search').value.toLowerCase();const list=rows.filter(a=>a.id.toLowerCase().includes(query));$('#account-list').className='account-directory';$('#account-list').innerHTML=list.length?table(['账号标识','状态','会话与连接','操作'],list.map(a=>[`<code>${esc(a.id)}</code>${a.realm?`<p>${esc(a.realm)}</p>`:''}`,badge(a.enabled?'ready':'disabled',a.enabled?'已启用':'已停用'),clientStates.has(a.id)?esc(LoomConnections.clientLabels[clientStates.get(a.id).state]||'待核验')+(clientStates.get(a.id).verified_at?`<p class="small muted">最近核验 ${esc(date(clientStates.get(a.id).verified_at))}</p>`:''):date(a.expires_at),`<div class="actions">${button('account-models','查看目录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'compact')}${button('toggle-account',a.enabled?'停用':'启用',`data-provider="${esc(selected)}" data-index="${rows.indexOf(a)}"`,'compact')}${button('reconnect-account','重新登录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'quiet compact')}</div>`])):empty(query?'没有匹配的账号':'尚未接入账号','账号接入状态由服务端读取。');};
    draw();$('#account-search').addEventListener('input',draw);if(selected==='doubao')await LoomConnections.client($('#client-status'),list=>{clientStates.clear();list.forEach(x=>clientStates.set(x.id,x));draw();});await LoomConnections.list($('#connection-list'));
  }
  async function modelPage() {
    if(!state.providers.some(p=>p.id===state.modelProvider)) state.modelProvider=state.providers[0]?.id || '';
    const generation=state.version;
    $('#view').innerHTML=heading(button('model-refresh','刷新目录','','quiet'))+`<div class="context-bar scope-toolbar">${providerFilter('model-provider',state.modelProvider)}${accountScope('model-account')}</div><div class="model-filters"><label class="search"><span class="sr-only">搜索模型</span><input id="model-search" type="search" placeholder="查找模型" value="${esc(state.search)}"></label><div id="model-capability" class="capability-choices" role="group" aria-label="模型能力">${[['','全部能力'],['native_tools','官方工作工具'],['supports_tool_call','API 工具调用'],['supports_images','图像输入'],['supports_reasoning','推理']].map(([id,name])=>`<button type="button" data-capability="${id}" aria-pressed="${id===state.capability}">${name}</button>`).join('')}</div></div><div id="model-notice"></div><div id="model-status"></div><div id="model-list"></div><div id="model-pagination" class="pagination"></div>`;
    bindProviderFilter('model-provider',value=>{state.modelProvider=value;state.modelAccount='';state.modelOffset=0;});
    $('#model-capability').onclick=e=>{const b=e.target.closest('[data-capability]');if(!b)return;state.capability=b.dataset.capability;$('#model-capability').querySelectorAll('button').forEach(x=>x.setAttribute('aria-pressed',String(x===b)));drawModels();};
    $('#model-search').oninput=e=>{state.search=e.target.value;drawModels();};
    const p=state.providers.find(p=>p.id===state.modelProvider);
    if(p.catalog_scope==='account') {
      const rows=await accountsFor(p.id);if(generation!==state.version)return;
      const bind=()=>bindAccountScope('model-account',rows,state.modelAccount,'所有账号（分页）',async value=>{state.modelAccount=value;state.modelOffset=0;bind();await loadModels(false);});bind();
    } else {state.modelAccount='';$('#model-account').innerHTML=`${accountIcon}<span><small>账号范围</small><strong>服务共享目录</strong></span>`;$('#model-account').setAttribute('aria-label','服务共享目录，无需选择账号');}
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
        $('#model-notice').innerHTML=notice(p.inference_available?'普通对话模型通过网关协议调用；支持多轮与流式的能力以每个模型的上游声明为准。官方工作任务仍需在官方服务完成。模型详情显示各自的可调用状态。':'此服务当前仅提供模型目录，尚不能通过网关 API 调用。官方工作工具不等同于 API 工具调用。',p.inference_available?'':'warn')+(data.catalogs.some(c=>c.status==='stale')?notice('上游刷新失败，正在显示旧缓存。退避时间内重复刷新不会再次请求上游。','warn'):'');
        $('#model-status').innerHTML=`<details class="catalog-status" ${data.catalogs.some(c=>c.status!=='ready'&&c.status!=='disabled')?'open':''}><summary>账号目录状态 · 本页 ${data.catalogs.length} / ${data.total} · ${data.catalogs.filter(c=>c.status==='ready').length} 个已同步</summary>${data.catalogs.map(c=>`<div class="catalog-row" data-account-status="${esc(c.status)}"><div><code>${esc(c.account_id)}</code><p class="small muted">${esc(errorLabels[c.error_code] || (c.error_code?'目录暂不可用。':'最近获取 '+date(c.fetched_at)))}${c.retry_at?' · 可重试 '+esc(date(c.retry_at)):''}${c.expires_at?' · 过期 '+esc(date(c.expires_at)):''}</p></div>${badge(c.status)}</div>`).join('')}</details>`;
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
    const query=state.search.trim().toLowerCase(),p=state.providers.find(p=>p.id===state.modelProvider);
    state.visibleModels=state.models.filter(m=>(!query||(m.name+' '+m.id).toLowerCase().includes(query))&&(!state.capability||m[state.capability]===true));
    const rows=state.visibleModels;
    if(!rows.length){$('#model-list').innerHTML=empty('没有匹配的模型','调整搜索、能力或账号筛选；账号不可用时请先检查登录状态。');return;}
    let selected=rows.findIndex(m=>m.id+'|'+(m.account_id||'')===state.modelSelection);if(selected<0)selected=0;
    $('#model-list').innerHTML=`<div class="browser-workspace" id="model-browser"><div class="selection-pane"><p class="selection-caption">${rows.length} 个模型${p.inference_available?'':'，当前仅目录'}</p><div role="listbox" aria-label="模型列表">${rows.map((m,i)=>`<button class="selection-row" role="option" aria-selected="false" data-action="select-model" data-index="${i}" data-model-id="${esc(m.id)}"><span><strong>${esc(m.name||m.id)}</strong><small>${esc(m.mode_name||p.name)}${state.catalogs.length>1&&m.account_id?' / '+esc(m.account_id):''}</small></span><span class="selection-arrow" aria-hidden="true">›</span></button>`).join('')}</div></div><section class="detail-pane" id="model-detail" tabindex="-1" aria-label="模型详情"></section></div>`;
    selectModel(selected,false);
  }
  function selectModel(index,open=true) {
    const m=state.visibleModels?.[index],p=state.providers.find(p=>p.id===state.modelProvider);if(!m)return;
    state.modelSelection=m.id+'|'+(m.account_id||'');
    document.querySelectorAll('[data-action=select-model]').forEach((b,i)=>b.setAttribute('aria-selected',String(i===index)));
    const c=state.catalogs.find(x=>x.account_id===m.account_id),callable=m.inference_available??p.inference_available;
    $('#model-detail').innerHTML=`${button('model-back','返回模型列表','','detail-back')}<div class="detail-subtitle">${esc(m.mode_name||p.name)}</div><h2 class="detail-title">${esc(m.name||m.id)}</h2><p class="detail-body">${callable?(p.catalog_scope==='account'?(m.supports_multi_turn?'支持网关普通文本调用及多轮上下文；文件和调用方工具仍受协议限制。实际可用性以当前账号为准。':'支持网关普通文本调用；多轮能力未在此目录声明，以实际请求结果为准。'):'可通过网关调用，实际请求受上游与账号状态约束。'):'此模型目前仅提供目录，尚不能通过网关 API 调用。下列能力与档位均按上游声明展示。'}</p>${m.catalog_status==='stale'?notice('这是旧缓存，最新刷新未成功。请核对账号目录状态。','warn'):''}<dl class="detail-specs">${callable&&c?.streaming_mode==='incremental'?'<div><dt>文本流</dt><dd>支持即时输出；上游重写已发内容时明确中止，未完成的回答不可视为成功。</dd></div>':''}<div><dt>上下文窗口</dt><dd class="value-large">${m.context_length?number(m.context_length):'未声明'}</dd></div><div><dt>推理档位</dt><dd>${esc((m.reasoning_efforts||[]).map(x=>x.name||x.level).join(' / ')||'上游未声明')}</dd></div>${[['官方工作工具','native_tools'],['API 工具调用','supports_tool_call'],['图像输入','supports_images'],['推理','supports_reasoning']].filter(([,k])=>m[k]===true).map(([label])=>`<div><dt>${label}</dt><dd>上游已声明支持</dd></div>`).join('')}${m.tiers?.length?`<div><dt>档位说明</dt><dd>${esc(m.tiers.join(' / '))}</dd></div>`:''}${m.account_id?`<div><dt>所属账号</dt><dd>${esc(m.account_id)}${m.need_login?'（需登录使用）':''}</dd></div>`:''}</dl><details class="quiet-detail"><summary>查看模型标识与目录时间</summary><code>${esc(m.id)}</code><p>${c?'获取于 '+esc(date(c.fetched_at))+'，过期于 '+esc(date(c.expires_at)):'由服务级目录提供。'}</p></details>`;
    if(open){$('#model-browser').classList.add('detail-open');$('#model-detail').focus({preventScroll:true});}
  }


  async function tasks() {
    state.taskProvider=state.taskProvider||state.providers[0]?.id||'';
    const generation=state.version;
    $('#view').innerHTML=heading(button('reload','刷新状态','','quiet')+button('run-all','执行当前范围','','quiet'))+`<div class="context-bar scope-toolbar">${providerFilter('task-provider',state.taskProvider)}${accountScope('task-account')}</div><div id="task-list" class="loading">正在发现任务…</div>`;
    $('[data-action=run-all]').disabled=true;
    bindProviderFilter('task-provider',value=>{state.taskProvider=value;state.taskAccount='';});
    const accounts=await accountsFor(state.taskProvider);
    if(generation!==state.version)return;
    bindAccountScope('task-account',accounts.filter(a=>a.enabled),state.taskAccount,'全部已启用账号',async value=>{state.taskAccount=value;await reload();});
    const query=new URLSearchParams({provider:state.taskProvider});if(state.taskAccount)query.set('accounts',state.taskAccount);
    const data=await api('workbench/catalog?'+query);if(generation!==state.version)return;state.tasks=data.tasks||[];
    const issues=data.issues||[];
    if(issues.length){const box=document.createElement('div');box.className='notice warn';box.setAttribute('role','status');box.dataset.discoveryIssues=String(issues.length);box.textContent=`${issues.length} 项账号任务目录未能读取：`+issues.map(x=>`${x.account_id} / ${x.module_id}`).join('、')+'。可用任务仍可执行，全部执行会保留部分失败记录。';$('#task-list').before(box);}
    if(issues.some(x=>x.error_code==='desktop_activity_unavailable'))$('#task-list').before(Object.assign(document.createElement('p'),{className:'notice warn',textContent:'官方响应未提供积分活动。请确认选择的是豆包账号；“飞书个人版”不能作为本次积分任务账号。活动资格以此账号的官方任务目录为准。'}));
    $('#task-list').className='';
    $('#task-list').innerHTML=state.tasks.length?`<div id="task-browser" class="browser-workspace"><div class="selection-pane"><p class="selection-caption">${state.tasks.length} 项可发现的工作</p><div role="listbox" aria-label="任务列表">${state.tasks.map((t,i)=>`<button class="selection-row" role="option" aria-selected="false" data-task="${esc(t.key)}" data-action="select-task" data-index="${i}"><span><strong>${esc(t.name)}</strong><small>${esc(t.account_id)}${t.manual_only?' / 需要你操作':''}</small></span><span class="selection-arrow" aria-hidden="true">›</span></button>`).join('')}</div></div><section id="task-detail" class="detail-pane" tabindex="-1" aria-label="任务详情"></section></div>`:empty('未发现可用任务','请确认账号已启用，并在需要时完成官方客户端登录。');
    if(state.tasks.length)selectTask(0,false);
    $('[data-action=run-all]').disabled=!state.tasks.some(t=>!t.manual_only);
  }
  const taskErrors={provider_rejected:'官方服务拒绝了此操作',operation_unconfirmed:'操作结果未确认',account_identity_changed:'账号或绑定发生变化',authentication_required:'需要重新登录',task_expired:'任务标识已过期',operation_not_eligible:'当前尚不满足操作条件',provider_busy:'官方客户端正忙',upstream_failed:'上游操作未成功'};
  const taskPhases={ready:'待执行',pending:'上游处理中',claimable:'可领取奖励',done:'上游已完成',manual:'待完成条件',blocked:'暂不可用'};
  function selectTask(index,open=true) {
    const t=state.tasks[index];if(!t)return;
    document.querySelectorAll('[data-action=select-task]').forEach((b,i)=>b.setAttribute('aria-selected',String(i===index)));
    $('#task-detail').innerHTML=`${button('task-back','返回任务列表','','detail-back')}${t.pending_verification?notice('上次操作结果尚未确认。系统会先核验服务端状态，并阻止重复提交。','warn'):''}<div class="detail-subtitle">${esc(providerName(t.provider_id))} / ${t.once?'一次性工作':t.once_per_day?'每日工作':'按需执行'}</div><h2 class="detail-title">${esc(t.name)}</h2><p class="detail-body">${esc(t.description)}</p><dl class="detail-specs">${t.state?`<div><dt>上游状态</dt><dd data-task-state="${esc(t.state.phase)}">${esc(taskPhases[t.state.phase]||'待核验')}</dd></div>${t.state.total?`<div><dt>服务端进度</dt><dd>${number(t.state.current||0)} / ${number(t.state.total)}</dd></div>`:''}<div><dt>所需操作</dt><dd>${esc(t.state.summary||'刷新状态以核验')}</dd></div>`:''}<div><dt>执行账号</dt><dd>${esc(t.account_id)}</dd></div><div><dt>执行方式</dt><dd>${t.manual_only?'需要你在官方服务完成操作':t.state?.phase==='manual'?'读取服务端条件，达标后可领取':'网关提交后重新核验服务端状态'}</dd></div>${t.reward?`<div><dt>奖励说明</dt><dd>${esc(t.reward)}</dd></div>`:''}</dl><div class="actions">${button(t.manual_only?'manual-task':'run-task',t.manual_only?'查看所需操作':t.pending_verification?'核验上次操作':t.state?.phase==='claimable'?'领取奖励':t.state?.phase==='done'?'再次核验':t.state?.phase==='manual'?'核验进度':t.state?.phase==='pending'?'核验状态':'执行任务',`data-index="${index}"`,'primary')}${!t.manual_only?button('task-schedule','安排计划',`data-index="${index}"`,'quiet'):''}</div><p class="detail-note">${t.manual_only?'按要求提供真实内容。完成与奖励以服务端核验为准。':'任务会进入执行队列；已提交不代表已完成。结果在执行记录中保留。'}</p>`;
    if(open){$('#task-browser').classList.add('detail-open');$('#task-detail').focus({preventScroll:true});}
  }

  function confirmRun(task) {
    const scope=task?[task.account_id]:state.taskAccount?[state.taskAccount]:[];
    const req={provider_id:task?.provider_id || state.taskProvider,task_key:task?.key || '*',account_ids:scope};
    if(sessionStorage.getItem('loomgate.unconfirmedRun')) {toast('上次执行请求结果未确认。请在执行记录核对后再继续，避免重复操作。');location.hash='runs';return;}
    openDialog('确认执行范围',`<form id="run-form" class="form"><p>${esc(task?.name || '当前范围内的全部任务')}</p><div class="notice">服务：${esc(providerName(req.provider_id))}<br>账号：${esc(scope.join('、') || '全部已启用账号')}<br>执行可能产生上游操作。需要人工的任务会保留待处理状态，奖励以服务端复核为准。</div><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认执行</button></div></form>`);
    $('#run-form').onsubmit=e=>{e.preventDefault();if(state.pending.has('run'))return;sessionStorage.setItem('loomgate.unconfirmedRun','1');mutate('run','workbench/run',req,async data=>{if(!data.run?.id)throw new APIError('request_failed',502);sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();toast('已进入执行队列，结果以执行记录为准。');location.hash='runs';});};
  }

  async function schedules() {
    const data=await api('workbench');state.schedules=data.schedules||[];
    $('#view').innerHTML=heading(button('new-schedule','新建计划','','primary'))+(state.schedules.length?`<div class="plan-list">${state.schedules.map((s,i)=>`<article class="plan-row" data-schedule-id="${esc(s.id)}"><div class="plan-time">${s.hours.map(h=>String(h).padStart(2,'0')+':'+String(s.minute).padStart(2,'0')).join('、')}<small>每天 / ${esc(s.timezone)}</small></div><div><h3>${esc(providerName(s.provider_id))}：${esc(state.tasks.find(t=>t.provider_id===s.provider_id&&t.key===s.task_key)?.name||'已保存的任务')}</h3><p>为 ${esc(s.account_ids?.length?s.account_ids.join('、'):'全部已启用账号')} 执行。${s.once_per_day?'同一天最多执行一次。':'每个时段都执行。'}</p>${badge(s.enabled?'ready':'disabled',s.enabled?'已启用':'已停用')}<p>下次 ${esc(date(s.next_run))}</p><details class="quiet-detail"><summary>任务与计划标识</summary><p>${esc(s.task_key||'全部任务')}</p><code>${esc(s.id)}</code></details></div><div class="actions">${button('edit-schedule','编辑',`data-index="${i}"`,'quiet')}${button('toggle-schedule',s.enabled?'停用':'启用',`data-index="${i}" data-lock`,'quiet')}${button('delete-schedule','删除',`data-index="${i}"`,'quiet danger')}</div></article>`).join('')}</div>`:empty('安排第一项工作','从服务的任务目录选择工作，再决定账号与执行时间。'));
  }

  function cleanSchedule(s) {return {id:s.id,provider_id:s.provider_id,task_key:s.task_key,account_ids:s.account_ids || [],hours:s.hours,minute:s.minute,timezone:s.timezone,enabled:s.enabled,once_per_day:s.once_per_day};}
  async function verifySchedule(s) {const result=await api('workbench');const saved=(result.schedules || []).find(x=>x.id===s.id);if(!saved || JSON.stringify(cleanSchedule(saved))!==JSON.stringify(cleanSchedule(s)))throw new APIError('request_failed',502);}

  async function scheduleEditor(existing,task) {
    await LoomPlanEditor.open({existing,task,providers:state.providers,api,accountsFor,esc,options,openDialog,closeDialog,cleanSchedule,verifySchedule,mutate,toast,after:async()=>{if(state.route==='schedules')await reload();else location.hash='schedules';}});
  }

  function runTimeline(rows) {return `<ol class="event-list">${rows.map(r=>`<li><time>${esc(date(r.started_at))}</time><div><h3>${esc(r.task_key || '全部任务')}</h3><p>${esc(providerName(r.provider_id))} · ${esc(labels[r.stage] || '等待派发')}</p></div>${badge(r.status)}</li>`).join('')}</ol>`;}
  async function runs() {
    const data=await api('workbench');state.runs=data.runs || [];
    $('#view').innerHTML=heading(button('reload','刷新记录','','quiet'))+(sessionStorage.getItem('loomgate.unconfirmedRun')?notice('有一次执行提交结果未确认。请核对执行时间、服务和账号；不要直接重复提交。','warn')+button('review-submission','核对后解除提交保护'):'')+`<div id="run-poll" class="muted small" role="status"></div>`+(state.runs.length?`<ol class="execution-journal">${state.runs.map((r,i)=>`<li data-run-id="${esc(r.id)}"><time>${esc(date(r.started_at))}</time><div class="journal-content"><h3>${esc(r.task_key||'全部任务')}</h3><p>${esc(providerName(r.provider_id))}，${esc(r.account_ids?.join('、')||'全部已启用账号')}</p><p>${esc(labels[r.stage]||'等待派发')}${r.discovery_issues?.length?'，'+r.discovery_issues.length+' 项发现失败':''}</p><code>${esc(r.id)}</code></div><div>${badge(r.status)}<div class="actions">${button('run-detail','详情',`data-index="${i}"`,'quiet')}${['queued','running'].includes(r.status)?button('cancel-run','取消',`data-index="${i}" data-lock`,'quiet danger'):''}</div></div></li>`).join('')}</ol>`:empty('还没有执行记录','执行请求会先进入队列；完成结果在此显示。'));
    if(state.runs.some(r=>['queued','running'].includes(r.status))) {
      if(!state.pollStarted)state.pollStarted=Date.now();
      if(Date.now()-state.pollStarted<90000){$('#run-poll').textContent='正在同步执行状态；离开页面将停止同步。';const v=state.version;state.poll=setTimeout(()=>{if(v===state.version)runs().catch(fail);},Math.min(8000,1000+Math.floor((Date.now()-state.pollStarted)/8000)*1000));}
      else $('#run-poll').textContent='自动同步已达到 90 秒上限。任务仍在服务端运行，可点击刷新继续查看。';
    } else state.pollStarted=0;
  }
  async function usage() {
    const data=await api('usage'), totals=data.totals;
    if(!totals)throw new APIError('request_failed',503);
    $('#view').innerHTML=heading(button('reload','刷新用量','','quiet'))+`<div class="usage-summary"><strong data-total-requests>${number(totals.requests)}</strong> 次请求已记入账本，${number(totals.failed_requests)} 次失败。</div><dl class="usage-breakdown"><div><dt>Token 总量</dt><dd>${number(totals.total_tokens)}</dd></div><div><dt>已报告消耗</dt><dd>${number(totals.credit)}</dd></div><div><dt>未报告用量的请求</dt><dd>${number(totals.unreported_requests)}</dd></div></dl>`+notice(`账本开始于 ${date(data.since)}。缺失数据不会按零消耗推断。`)+(data.days?.length?table(['日期','请求','Token','消耗'],data.days.slice(0,30).map(d=>[esc(d.day),number(d.totals.requests),number(d.totals.total_tokens),number(d.totals.credit)])):empty('暂无每日用量','网关收到实际调用后将写入用量账本。'));
  }
  async function logs() {
    const data=await api('requests?limit=30&offset='+String(state.logOffset || 0));
    const rows=data.requests || data.items || data.rows;
    if(!Array.isArray(rows))throw new APIError('request_failed',503);
    $('#view').innerHTML=heading(button('reload','刷新记录'))+(rows.length?table(['请求','模型 / 协议','状态','耗时','时间'],rows.map(r=>[`<code>${esc(r.request_id)}</code>`,`${esc(r.model)}<p>${esc(r.protocol)}</p>`,esc(r.status || r.outcome || '未知'),r.duration_ms!=null?number(r.duration_ms)+' ms':'—',date(r.started_at)])):empty('暂无请求记录','记录由网关真实请求生成，不包括面板的目录读取。'))+`<div class="pagination"><span>共 ${number(data.total)} 条</span><div class="actions">${button('logs-prev','上一页',!state.logOffset?'disabled':'')}${button('logs-next','下一页',(state.logOffset || 0)+30>=data.total?'disabled':'')}</div></div>`;
  }
  async function settings() {
    const session=await api('session');
    let state=null,loop=null,update=null,stateError='',loopError='',updateError='';
    try{state=await api('state');}catch(e){if(e.name!=='AbortError')stateError=e.message||'未能读取';}
    try{loop=await api('features/reasoning-loop');}catch(e){if(e.name!=='AbortError')loopError=e.message||'未能读取';}
    try{update=await api('update',{allowNotOK:true});}catch(e){if(e.name!=='AbortError')updateError=e.message||'未能读取';}
    const lan=state?.lan;
    const loopOn=loop?.stop_only===true;
    const updateStatus=update?.status||{};
    const updateEnabled=update?.ok===true && update?.status?.enabled===true;
    const updateMessage=updateEnabled?'':(typeof update?.message==='string'&&update.message)||'当前构建未启用热更新。';
    const lanTitle=!lan?'状态未读取':lan.enabled?'配置已启用':'配置为仅本机';
    const lanDetail=!lan?notice('局域网状态读取失败：'+esc(stateError||'服务未返回状态')+'。界面不会把它当作已关闭。','error'):`监听 ${esc(lan.listen||'未声明')}${lan.allow_cidrs?'，允许 '+esc(lan.allow_cidrs):''}${lan.has_key?'':'；尚未创建调用密钥'}${lan.enabled?'<br>保存后需要重启服务才会改变实际监听。':''}`;
    const loopTitle=!loop?'状态未读取':loopOn?'命中即停止':'命中后自动重发';
    const loopDetail=!loop?notice('重复推理开关读取失败：'+esc(loopError||'服务未返回状态')+'。','error'):'作用于后续请求；已开始的请求沿用原语义。';
    const updateNotice=!update?notice('更新状态读取失败：'+esc(updateError||'服务未返回状态')+'。','error'):notice(updateEnabled?'可检查官方发布并下载已签名的运行包。':updateMessage,updateEnabled?'':'warn');
    const updateUnread=!update;
    $('#view').innerHTML=heading()+`<div class="split"><section class="panel"><div class="panel-title"><div><h2>显示与会话</h2></div></div><div class="settings-line"><div><h3>界面主题</h3><p>在此浏览器保存偏好。</p></div>${button('theme','切换主题')}</div><div class="settings-line"><div><h3>当前会话</h3><p>有效期至 ${esc(date(session.expiresAt))}</p></div>${badge('ready','已登录')}</div><div class="settings-line"><div><h3>管理账号安全</h3><p>修改密码后其他设备需重新登录。</p></div>${button('password','修改密码')}</div></section><section class="panel"><div class="panel-title"><div><h2>局域网访问</h2></div></div><div class="settings-line"><div><h3>${lanTitle}</h3><p>${lanDetail}</p></div>${state?button('lan-settings',lan?.enabled?'调整':'启用','','quiet'):''}</div></section><section class="panel"><div class="panel-title"><div><h2>重复推理保护</h2></div></div><div class="settings-line"><div><h3>${loopTitle}</h3><p>${loopDetail}</p></div>${loop?button('loop-toggle',loopOn?'改为自动重发':'改为命中即停','','quiet'):''}</div></section><section class="panel"><div class="panel-title"><div><h2>迁移与更新</h2></div></div>${updateNotice}<p class="muted">${update?'检查与下载不会自动切换生产；正式发布前仍需在目标平台完成独立验收与签名。':'更新读取失败时不会显示已启用或已下载。'}</p><div class="section-gap">${updateEnabled?button('update-check','检查更新','','quiet'):''}${updateUnread?badge('manual_action','状态未读取'):updateEnabled?badge(updateStatus.state||'manual_action',updateStatus.state==='downloaded'?'已下载待交接':'待独立验收'):badge('disabled','未启用')}</div></section></div>`;
  }
  async function keyPage() {
    const data=await api('keys');
    if(!Array.isArray(data.keys))throw new APIError('request_failed',502);
    let limits={};
    try{const live=await api('key-limits');if(live?.ok===true&&live.keys&&typeof live.keys==='object')limits=live.keys;}catch(e){if(e.name!=='AbortError')limits={};}
    const rows=data.keys.map(k=>{
      const live=limits[k.id]||null;
      const limitText=k.limits?`${k.limits.requests_per_minute||0}/分 · 并发 ${k.limits.max_concurrent||0}${k.limits.queue_timeout_seconds?' · 排队 '+k.limits.queue_timeout_seconds+'s':''}`:'未配置（沿用服务默认）';
      const liveText=live?`${number(live.requests)} 次/分窗口 · 进行 ${number(live.active)} · 排队 ${number(live.queued)}`:'实时快照未读取';
      const tokens=k.total_tokens===undefined?'—':number(k.total_tokens);
      return [`<strong>${esc(k.name||'未命名')}</strong><p class="small muted">${esc(k.note||'')}</p>`,`<code>${esc(k.masked_key||'')}</code><p class="small muted">${esc((k.models||[]).length?((k.models||[]).slice(0,3).join(', '))+((k.models||[]).length>3?' 等 '+(k.models||[]).length+' 个':''):'不限模型')}</p>`,badge(k.enabled?'ready':'disabled',k.enabled?'启用':'停用'),esc(k.expires_at?date(k.expires_at):'无限制'),esc(limitText)+`<p class="small muted">${esc(liveText)}</p>`,tokens,button('key-edit','编辑',`data-id="${esc(k.id)}"`,'quiet')+button('key-toggle',k.enabled?'停用':'启用',`data-id="${esc(k.id)}" data-enabled="${k.enabled?'1':'0'}"`,'quiet')+button('key-copy','显示密钥',`data-id="${esc(k.id)}"`,'quiet')+button('key-delete','删除',`data-id="${esc(k.id)}"`,'quiet danger')];
    });
    $('#view').innerHTML=heading(button('key-new','创建密钥','','primary')+button('reload','刷新','','quiet'))+(rows.length?table(['名称','密钥','状态','有效期','限流','累计 Token','操作'],rows):empty('还没有访问密钥','创建第一把密钥后才能通过 OpenAI / Anthropic / Gemini 兼容接口调用网关。'))+notice('密钥明文只在创建或「显示密钥」时返回一次；列表只显示掩码。停用会立即生效，删除不可恢复。','warn');
  }
  async function keyEditDialog(id) {
    const data=await api('keys');
    const key=(data.keys||[]).find(k=>k.id===id);
    if(!key)throw new APIError('request_failed',404,'密钥已不存在，请刷新列表。');
    const limits=key.limits||{};const models=(key.models||[]).join(', ');
    openDialog('编辑密钥',`<form id="key-form" class="form"><p class="muted">密钥 <code>${esc(key.masked_key||'')}</code> 的明文不会再显示。留空表示不限制。</p><label>名称<input name="name" value="${esc(key.name||'')}" required maxlength="80"></label><label>备注<input name="note" value="${esc(key.note||'')}" maxlength="200"></label><label>模型绑定（逗号分隔，留空不限）<input name="models" value="${esc(models)}" maxlength="4000"></label><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="${esc(limits.requests_per_minute??0)}"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="${esc(limits.max_concurrent??0)}"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="${esc(limits.queue_timeout_seconds??0)}"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存并核对</button></div></form>`);
    $('#key-form').onsubmit=e=>{e.preventDefault();const f=new FormData(e.target);const body={id:key.id,name:String(f.get('name')||'').trim(),note:String(f.get('note')||'').trim(),models:String(f.get('models')||'').split(',').map(x=>x.trim()).filter(Boolean)};const rpm=Number(f.get('requests_per_minute')||0),conc=Number(f.get('max_concurrent')||0),queue=Number(f.get('queue_timeout_seconds')||0);body.limits=(rpm||conc||queue)?{requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue}:null;mutate('key-edit','keys/update',body,async()=>{const saved=(await api('keys')).keys.find(k=>k.id===key.id);if(!saved||saved.name!==body.name||JSON.stringify(saved.models||[])!==JSON.stringify(body.models))throw new APIError('request_failed',502,'保存后读回与提交不一致。');closeDialog();toast('密钥已更新，并从服务端读回。');await reload();});};
  }
  async function keyNewDialog() {
    openDialog('创建访问密钥',`<form id="key-form" class="form"><label>名称<input name="name" required maxlength="80" autofocus></label><label>备注<input name="note" maxlength="200"></label><label>模型绑定（逗号分隔，留空不限）<input name="models" maxlength="4000"></label><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="0"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="0"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="0"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">创建</button></div></form>`);
    $('#key-form').onsubmit=e=>{e.preventDefault();const f=new FormData(e.target);const body={name:String(f.get('name')||'').trim(),note:String(f.get('note')||'').trim(),models:String(f.get('models')||'').split(',').map(x=>x.trim()).filter(Boolean)};const rpm=Number(f.get('requests_per_minute')||0),conc=Number(f.get('max_concurrent')||0),queue=Number(f.get('queue_timeout_seconds')||0);if(rpm||conc||queue)body.limits={requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue};mutate('key-new','keys',body,async data=>{if(!data.key)throw new APIError('request_failed',502,'服务端没有返回完整密钥。');const secret=String(data.key);$('#dialog-body').innerHTML=`<p>密钥已创建。请立即复制，关闭后无法再次查看。</p><label>密钥<textarea readonly rows="3">${esc(secret)}</textarea></label><div class="actions">${button('close','我已保存')}</div>`;const closeButton=$('#dialog [data-action=close]');if(closeButton)closeButton.onclick=()=>{closeDialog();reload().catch(()=>{});};toast('密钥已创建，请立即保存。');});};
  }
  async function migration() {
    let data={},unavailable='';
    try{data=await api('migration',{allowNotOK:true});}catch(e){if(e.name!=='AbortError')unavailable=e.message||'迁移状态未读取';}
    const marker=data.marker||null;
    const toolAvailable=data.tool?.available===true;
    $('#view').innerHTML=heading(button('migration-check','重新读取','','quiet'))+
      (unavailable?notice('迁移状态读取失败：'+unavailable+'。界面不会把它当作未迁移或已迁移。','error'):'')+
      (marker?`<section class="panel"><div class="panel-title"><div><h2>当前工作区迁移标记</h2></div>${badge('ready','已迁移')}</div>${table(['项目','值'],[['标记版本',esc(marker.format??'—')],['文件数量',number(marker.file_count)],['清单摘要',`<code>${esc(marker.inventory_sha256||'未记录')}</code>`],['源配置摘要',`<code>${esc(marker.source_config_sha256||'未记录')}</code>`],['目标绑定',`<code>${esc(marker.destination_binding||'未记录')}</code>`]])}${notice('标记只说明本工作区由离线复制产生；它不证明旧生产已经切换。','warn')}</section>`:
        (unavailable?'':`<section class="panel"><div class="panel-title"><div><h2>当前工作区</h2></div>${badge('manual_action','未发现迁移标记')}</div><p>当前数据目录没有迁移标记。新部署通常不需要迁移；从旧版切换请先做离线预检、复制和回滚演练。</p></section>`))+
      `<section class="panel"><div class="panel-title"><div><h2>离线迁移工具</h2></div>${toolAvailable?badge('ready','可用'):badge('disabled','未随此包提供')}</div><p>${esc(data.tool?.message||'迁移在离线状态下完成；不会在网关运行中覆盖当前库。')}</p>`+(toolAvailable?`<p class="muted">离线执行顺序：先预检，再加 --apply --offline-confirmed；回滚也要求离线确认。</p><pre class="code">python -I -X utf8 tools/migration/migrate.py copy --source &lt;旧工作区&gt; --destination &lt;新工作区&gt;
python -I -X utf8 tools/migration/migrate.py copy --source &lt;旧工作区&gt; --destination &lt;新工作区&gt; --apply --offline-confirmed
python -I -X utf8 tools/migration/migrate.py rollback --destination &lt;新工作区&gt; --offline-confirmed</pre>`:'')+`</section>`+
      (toolAvailable?`<section class="panel"><div class="panel-title"><div><h2>本机只读预检</h2></div>${badge('ready','dry-run')}</div><p>选择一个明确的旧工作区与一个尚不存在的新目录。预检只读源目录并报告数量与摘要，不创建目标目录、不输出账号或密钥。</p><form id="migration-preflight-form" class="form"><label>旧工作区（绝对路径）<input name="source" required maxlength="4096" autocomplete="off" spellcheck="false" placeholder="例如 D:/old-workbuddy"></label><label>新目录（绝对路径，必须不存在）<input name="destination" required maxlength="4096" autocomplete="off" spellcheck="false" placeholder="例如 D:/LoomGate-data-new"></label><p id="migration-preflight-message" class="notice" role="status" hidden></p><div class="actions"><button type="submit" class="primary">运行只读预检</button></div></form></section>`:'')+
      `<section class="panel"><div class="panel-title"><div><h2>边界</h2></div></div><ul class="muted"><li>只复制配置、账号、密钥、用量、计划和 unknown 状态；旧运行文件与诊断目录显式排除并计数。</li><li>导入的计划全部为停用，自动派发关闭，需在新工作区显式启用。</li><li>迁移不提供任意文件浏览或下载，也不输出密钥明文。</li></ul></section>`;
    const preflightForm=$('#migration-preflight-form');
    if(preflightForm){
      preflightForm.onsubmit=e=>{
        e.preventDefault();
        const form=new FormData(preflightForm);
        const message=$('#migration-preflight-message');
        const submit=preflightForm.querySelector('button[type=submit]');
        message.hidden=false;message.className='notice';message.textContent='正在执行只读预检…';submit.disabled=true;
        api('migration/preflight',{body:{source:String(form.get('source')||'').trim(),destination:String(form.get('destination')||'').trim()},signal:null})
          .then(result=>{
            const r=result.report||{};
            const counts=Object.entries(r.categories||{}).map(([k,v])=>`${k} ${v.files} 个文件`).join('，');
            message.className='notice';
            message.textContent=`预检通过：${r.file_count??0} 个文件，${counts||'无持久化文件'}。未创建目标目录。实际复制必须在离线确认后执行。`;
            if(preflightForm.isConnected)submit.disabled=false;
          })
          .catch(err=>{
            if(!preflightForm.isConnected)return;
            message.className='notice error';
            message.textContent=err instanceof APIError?err.message:'预检结果未确认，请检查路径后重试。';
            submit.disabled=false;
          });
      };
    }
  }
  function passwordDialog() {
    openDialog('修改管理账号',`<form id="password-form" class="form"><label>当前密码<input name="current" type="password" autocomplete="current-password" required></label><label>用户名<input name="username" autocomplete="username" required pattern="[A-Za-z0-9_.-]{3,32}"></label><label>新密码<input name="password" type="password" autocomplete="new-password" minlength="8" required></label><label>再次输入新密码<input name="confirm" type="password" autocomplete="new-password" required></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);
    $('#password-form').onsubmit=e=>{e.preventDefault();const body=Object.fromEntries(new FormData(e.target));if(body.password!==body.confirm){$('#form-message').textContent='两次新密码不一致。';$('#form-message').hidden=false;return;}mutate('password','auth/password',body,async()=>{closeDialog();toast('管理账号已更新。');await reload();});};
  }
  document.addEventListener('keydown',e=>{const row=e.target.closest('.selection-row');if(!row||!['ArrowDown','ArrowUp','Home','End'].includes(e.key))return;const rows=[...row.closest('[role=listbox]').querySelectorAll('.selection-row')],i=rows.indexOf(row),next=e.key==='Home'?0:e.key==='End'?rows.length-1:Math.max(0,Math.min(rows.length-1,i+(e.key==='ArrowDown'?1:-1)));e.preventDefault();if(row.dataset.action==='select-model')selectModel(next,false);else selectTask(next,false);rows[next].focus();});
  const renderers={overview,providers:services,accounts,models:modelPage,tasks,schedules,runs,usage,logs,keys:keyPage,migration,settings};
  async function reload() {state.providers=null;state.pollStarted=0;await navigate();}
  async function action(element) {
    const name=element.dataset.action,index=Number(element.dataset.index);
    if(name==='select-model'){selectModel(index);return;}
    if(name==='select-task'){selectTask(index);return;}
    if(name==='model-back'||name==='task-back'){const kind=name.split('-')[0];$('#'+kind+'-browser').classList.remove('detail-open');$('#'+kind+'-browser [aria-selected=true]')?.focus();return;}
    if(name==='close'){closeDialog();return;}
    if(name==='reload'){await reload();return;}
    if(name==='theme'){toggleTheme();return;}
    if(name==='provider-models' || name==='account-models'){state.modelProvider=element.dataset.provider;state.modelAccount=element.dataset.account || '';state.modelOffset=0;location.hash='models';return;}
    if(name==='provider-accounts'){state.accountProvider=element.dataset.provider;location.hash='accounts';return;}
    if(name==='connect-account'||name==='reconnect-account'){await LoomConnections.open(element.dataset.provider||state.accountProvider,element.dataset.account||'');return;}
    if(name==='toggle-account'){
      const a=state.accountRows[index],provider=element.dataset.provider;openDialog(a.enabled?'停用账号':'启用账号',`<form id="account-toggle-form" class="form"><p>账号 <code>${esc(a.id)}</code></p><p class="muted">${a.enabled?'停用后不再用于后续目录请求与任务。正在进行的操作应在执行记录中单独核验。':'启用后该账号可再次用于目录读取和任务。'}</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认${a.enabled?'停用':'启用'}</button></div></form>`);
      $('#account-toggle-form').onsubmit=e=>{e.preventDefault();const modern=state.providers.find(p=>p.id===provider)?.catalog_scope==='account';mutate('account-toggle',modern?'workbench/account/toggle':'account/toggle',modern?{id:a.id,enabled:!a.enabled,revision:a.revision}:{uid:a.id,disabled:a.enabled},async()=>{const updated=(await accountsFor(provider)).find(x=>x.id===a.id);if(!updated||updated.enabled===a.enabled)throw new APIError('request_failed',502);closeDialog();toast('账号状态已从服务端核验。');await reload();});};return;
    }
    if(name==='model-refresh'){await loadModels(true);return;}
    if(name==='model-next' || name==='model-prev'){state.modelOffset=Math.max(0,state.modelOffset+(name==='model-next'?8:-8));await loadModels(false);return;}
    if(name==='connection-help'){openDialog('账号接入状态',notice('账号状态来自真实后端。WorkBuddy 支持扫码/短信登录与重新连接；豆包登录基于官方客户端会话。只有在服务确认需要人工操作时才会发起对应流程。','')+'<p class="muted">登录流程需要真实扫码或验证码时，界面会显示明确步骤；不会在后台静默重试已成功的操作。</p>');return;}
    if(name==='manual-task'){const t=state.tasks[index];openDialog('所需人工操作',`<h3>${esc(t.name)}</h3><p class="section-gap">${esc(t.description)}</p>${notice('请在对应官方服务完成要求的内容或操作。完成与奖励以服务端核验为准。','warn')}`);return;}
    if(name==='run-task' || name==='run-all'){confirmRun(name==='run-task'?state.tasks[index]:null);return;}
    if(name==='review-submission'){openDialog('核对未确认的提交',`<form id="review-form" class="form"><p>请先检查执行记录和官方服务状态。解除保护不会重试或取消原请求。</p><label class="check"><input type="checkbox" required>我已核对上次提交，确认不会重复执行已有操作</label><div class="actions">${button('close','返回')}<button type="submit">解除提交保护</button></div></form>`);$('#review-form').onsubmit=e=>{e.preventDefault();sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();reload();};return;}
    if(name==='new-schedule' || name==='edit-schedule' || name==='task-schedule'){await scheduleEditor(name==='edit-schedule'?state.schedules[index]:null,name==='task-schedule'?state.tasks[index]:null);return;}
    if(name==='toggle-schedule'){const s={...cleanSchedule(state.schedules[index]),enabled:!state.schedules[index].enabled};await mutate('schedule','workbench/schedule',s,async()=>{await verifySchedule(s);toast(s.enabled?'计划已启用。':'计划已停用。');await reload();});return;}
    if(name==='delete-schedule'){const s=state.schedules[index];openDialog('删除执行计划',`<form id="delete-form" class="form"><p>删除计划 <code>${esc(s.id)}</code> 后将停止后续派发。已有执行记录保留。</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="danger">删除计划</button></div></form>`);$('#delete-form').onsubmit=e=>{e.preventDefault();mutate('delete','workbench/schedule/delete',{id:s.id},async()=>{const data=await api('workbench');if(data.schedules.some(x=>x.id===s.id))throw new APIError('request_failed',502);closeDialog();toast('计划已删除，并已从服务端核验。');await reload();});};return;}
    if(name==='cancel-run'){const r=state.runs[index];await mutate('cancel-'+r.id,'workbench/cancel',{id:r.id},async data=>{toast(data.cancel_requested?'已发送取消请求，等待服务端确认。':'任务已结束或无法取消，请查看最新状态。');await reload();});return;}
    if(name==='run-detail'){const r=state.runs[index];openDialog('执行详情',`<p><code>${esc(r.id)}</code></p><div class="section-gap">${badge(r.status)}</div>${r.status==='uncertain'?notice('上游操作结果尚未确认，请先核验服务端状态，不要重复执行。','warn'):''}<div class="section-gap">${r.steps?.length?table(['阶段','账号','结果'],r.steps.map(s=>[esc(labels[s.stage] || s.stage),`<code>${esc(s.account_id)}</code>`,badge(s.status)+(s.error_code?`<p class="small muted">${esc(taskErrors[s.error_code]||'操作未完成')}${Number.isInteger(s.upstream_code)?'（业务码 '+s.upstream_code+'）':''}</p>`:'')])):empty('尚无执行步骤','任务可能仍在等待派发。')}</div>`);return;}
    if(name==='logs-prev' || name==='logs-next'){state.logOffset=Math.max(0,(state.logOffset || 0)+(name==='logs-next'?30:-30));await reload();return;}
    if(name==='lan-settings'){const st=await api('state');if(!st?.lan)throw new APIError('request_failed',502);const lan=st.lan;openDialog('局域网访问',`<form id="lan-form" class="form"><p class="muted">这里修改的是配置文件中的监听地址与允许网段；保存成功后仍需重启服务，实际监听不会立即改变。</p><label class="check"><input type="checkbox" name="enabled" ${lan.enabled?'checked':''}>启用局域网访问</label><label>监听地址<input name="listen" value="${esc(lan.listen||'')}" readonly aria-readonly="true"></label><label>允许网段<input name="allow_cidrs" value="${esc(lan.allow_cidrs||'')}" placeholder="例如 192.168.1.0/24"></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);$('#lan-form').onsubmit=e=>{e.preventDefault();const form=new FormData(e.target);const want={enabled:form.get('enabled')==='on',allow_cidrs:String(form.get('allow_cidrs')||'').trim()};mutate('lan','lan',want,async data=>{if(data.ok===false)throw new APIError('request_failed',502);const saved=(await api('state'))?.lan;if(!saved||saved.enabled!==want.enabled||String(saved.allow_cidrs||'')!==want.allow_cidrs)throw new APIError('request_failed',502);closeDialog();toast((data.message||'设置已保存。')+' 已从服务端读回配置。');await reload();});};return;}
    if(name==='loop-toggle'){const current=await api('features/reasoning-loop');if(current?.stop_only===undefined)throw new APIError('request_failed',502);const want=!current.stop_only;await mutate('loop','features/reasoning-loop',{stop_only:want},async()=>{const saved=await api('features/reasoning-loop');if(saved?.stop_only!==want)throw new APIError('request_failed',502);toast('重复推理保护已切换，并已从服务端读回。');await reload();});return;}
    if(name==='update-check'){await mutate('update-check','update/check',{},async data=>{if(data.ok===false)throw new APIError('request_failed',502,typeof data.message==='string'?data.message:'');const status=data.status||{};toast('已检查更新：'+(status.message||status.state||'状态未变化')+'。');await reload();});return;}
    if(name==='key-new'){await keyNewDialog();return;}
    if(name==='key-edit'){await keyEditDialog(element.dataset.id);return;}
    if(name==='key-copy'){const id=element.dataset.id;await mutate('key-copy','keys/copy',{id},async data=>{if(!data.key)throw new APIError('request_failed',502,'服务端没有返回密钥。');openDialog('显示密钥',`<p>此密钥只在此刻显示。关闭后可再次点击「显示密钥」。</p><label>密钥<textarea readonly rows="3">${esc(String(data.key))}</textarea></label><div class="actions">${button('close','关闭')}</div>`);});return;}
    if(name==='key-toggle'){const id=element.dataset.id,want=element.dataset.enabled!=='1';await mutate('key-toggle','keys/update',{id,enabled:want},async()=>{const saved=(await api('keys')).keys.find(k=>k.id===id);if(!saved||saved.enabled!==want)throw new APIError('request_failed',502);toast(want?'密钥已启用。':'密钥已停用。');await reload();});return;}
    if(name==='key-delete'){const id=element.dataset.id;openDialog('删除访问密钥',`<form id="key-delete-form" class="form"><p>删除后使用该密钥的客户端会立即收到鉴权失败，且不可恢复。</p><label class="check"><input type="checkbox" required>我确认要删除这把密钥</label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="danger">删除</button></div></form>`);$('#key-delete-form').onsubmit=e=>{e.preventDefault();mutate('key-delete','keys/delete',{id},async()=>{if((await api('keys')).keys.some(k=>k.id===id))throw new APIError('request_failed',502);closeDialog();toast('密钥已删除，并从服务端读回。');await reload();});};return;}
    if(name==='migration-check'){await reload();return;}
    if(name==='password')passwordDialog();
  }
  const wideNavigation=window.matchMedia('(min-width:901px)');
  // CSS can blur a newly hidden link before the media-query event arrives.
  let sidebarHadFocus=false;
  document.addEventListener('focusin',e=>{sidebarHadFocus=$('#sidebar').contains(e.target);});
  function closeNavigation(restoreFocus=false){
    const sidebar=$('#sidebar'),wasOpen=document.body.classList.contains('navigation-open');
    document.body.classList.remove('navigation-open');$('#navigation-toggle').setAttribute('aria-expanded','false');
    sidebar.removeAttribute('role');sidebar.removeAttribute('aria-modal');
    for(const selector of ['#main','footer','.masthead','#connection'])$(selector).inert=false;
    if(restoreFocus&&wasOpen)$('#navigation-toggle').focus({preventScroll:true});
  }
  $('#navigation-toggle').onclick=()=>{
    if(wideNavigation.matches)return;
    if(document.body.classList.contains('navigation-open')){closeNavigation(true);return;}
    document.body.classList.add('navigation-open');$('#navigation-toggle').setAttribute('aria-expanded','true');
    $('#sidebar').setAttribute('role','dialog');$('#sidebar').setAttribute('aria-modal','true');
    for(const selector of ['#main','footer','.masthead','#connection'])$(selector).inert=true;
    ($('#navigation a[aria-current=page]')||$('#navigation-close')).focus();
  };
  $('#navigation-close').onclick=$('#navigation-backdrop').onclick=()=>closeNavigation(true);
  wideNavigation.addEventListener('change',()=>{
    const focused=document.activeElement,inside=$('#sidebar').contains(focused)||sidebarHadFocus;
    closeNavigation();
    if(!wideNavigation.matches&&inside)$('#navigation-toggle').focus({preventScroll:true});
    else if(wideNavigation.matches&&focused===$('#navigation-close'))$('#navigation a[aria-current=page]')?.focus();
  });
  $('#sidebar').addEventListener('click',e=>{if(e.target.closest('a')){closeNavigation();$('#main').focus({preventScroll:true});}});
  document.addEventListener('keydown',e=>{
    if(!document.body.classList.contains('navigation-open'))return;
    if(e.key==='Escape'){e.preventDefault();closeNavigation(true);}
    if(e.key==='Tab'){
      const controls=[...$('#sidebar').querySelectorAll('a[href],button:not(:disabled)')].filter(x=>x.getClientRects().length),first=controls[0],last=controls.at(-1);
      if(e.shiftKey&&document.activeElement===first){e.preventDefault();last.focus();}
      else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first.focus();}
    }
  });
  function themeLabel(){$('#theme-label').textContent=document.documentElement.dataset.theme==='dark'?'浅色模式':'深色模式';}
  function toggleTheme(){const theme=document.documentElement.dataset.theme==='dark'?'light':'dark';document.documentElement.dataset.theme=theme;themeLabel();try{localStorage.setItem('loomgate.theme',theme);}catch{}}
  try{document.documentElement.dataset.theme=localStorage.getItem('loomgate.theme') || 'light';}catch{}
  themeLabel();
  $('#dialog').addEventListener('keydown',e=>{
    if(!$('#dialog').classList.contains('account-dialog'))return;
    if(e.key==='Escape'){e.preventDefault();e.stopPropagation();closeDialog();}
    if(e.key==='Tab'){
      const controls=[...$('#dialog').querySelectorAll('button:not(:disabled),input:not(:disabled)')].filter(x=>x.getClientRects().length),first=controls[0],last=controls.at(-1);
      if(e.shiftKey&&document.activeElement===first){e.preventDefault();last?.focus();}
      else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first?.focus();}
    }
  });
  $('#theme').onclick=toggleTheme;$('#dialog-close').onclick=closeDialog;$('#dialog').addEventListener('cancel',e=>{e.preventDefault();closeDialog();});
  document.addEventListener('click',e=>{const target=e.target.closest('[data-action]');if(target && !target.disabled)action(target).catch(err=>{if(err.name!=='AbortError')toast(err instanceof APIError?err.message:'操作未完成，请刷新状态后重试。');});});
  $('#logout').onclick=()=>mutate('logout','auth/logout',{},async()=>location.replace('login'));
  const network=()=>{$('#connection').hidden=navigator.onLine;$('#connection').textContent='当前网络离线。页面中的数据可能已过期，恢复连接后请刷新。';};window.addEventListener('offline',network);window.addEventListener('online',network);network();
  LoomConnections.init({api,esc,openDialog,toast,reload,providerName,accountConnected:f=>{state.accountProvider=f.provider;}});
  window.addEventListener('hashchange',navigate);window.addEventListener('pagehide',()=>{state.controller?.abort();clearTimeout(state.poll);LoomConnections.stop();});navigate();
})();

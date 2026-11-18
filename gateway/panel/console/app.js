'use strict';
(() => {
  const $ = (selector, root = document) => root.querySelector(selector);
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const date = value => value && !String(value).startsWith('0001-') ? new Date(typeof value === 'number' ? value * 1000 : value).toLocaleString('zh-CN', {hour12:false}) : '—';
  const number = value => typeof value === 'number' ? value.toLocaleString('zh-CN') : '—';
  const state = {route:'', version:0, controller:null, providers:null, models:[], catalogs:[], modelProvider:'', modelAccount:'', modelOffset:0, modelGroupPage:0, modelChannel:'', search:'', capability:'', taskProvider:'', tasks:[], usageRange:null, taskCache:null, schedules:[], runs:[], pending:new Set(), poll:null};
  // Task names are a display convenience, not state: remember the last names the
  // server actually returned so a page refresh on 执行计划/执行记录 still shows
  // "每日签到" instead of a raw task key or "已保存的任务". Bounded and best-effort.
  const taskNameKey='loomgate.taskNames',taskNameLimit=600;
  function loadTaskNames(){const map=new Map();try{const raw=JSON.parse(localStorage.getItem(taskNameKey)||'{}');if(raw&&typeof raw==='object')for(const [k,v] of Object.entries(raw))if(typeof k==='string'&&typeof v==='string'&&v)map.set(k,v);}catch{}return map;}
  function saveTaskNames(map){try{const out={};let n=0;for(const [k,v] of map){if(n++>=taskNameLimit)break;out[k]=v;}localStorage.setItem(taskNameKey,JSON.stringify(out));}catch{}}
  state.taskNames=loadTaskNames();
  function taskLabel(provider,realm,key){
    if(!key)return '全部任务';
    // 迁移计划用「模块/」前缀表达「该模块当前的全部任务」。没有它时计划页会显示成「全部任务」——那会把「成长中心这条流水线」误读成「所有服务的所有任务」，执行范围被描述得比真实的大得多。
    if(key.endsWith('/')){
      const module=key.slice(0,-1),names=state.taskNames;
      const byModule=names?.get(provider+'\u0000'+(realm||'')+'\u0000'+module)||names?.get(provider+'\u0000\u0000'+module)||state.tasks.find(t=>t.provider_id===provider&&(t.key===module||t.key.startsWith(key)))?.name;
      const moduleNames={growth:'成长任务',travel:'Buddy 旅行',redeem:'连登奖励兑换',makeup:'日历补登'};
      return (byModule||moduleNames[module]||module)+'（全部任务）';
    }
    if(key==='*')return '全部任务';
    const names=state.taskNames,rk=realm||'';
    const known=names?.get(provider+'\u0000'+rk+'\u0000'+key)||names?.get(provider+'\u0000\u0000'+key)||state.tasks.find(t=>t.provider_id===provider&&t.key===key)?.name;
    if(known)return known;
    const legacy={checkin:'每日签到',balance:'余额刷新',travel:'Buddy 旅行',keepalive:'账号会话保活',growth:'成长任务',redemption:'连登奖励兑换',redeem:'连登奖励兑换',makeup:'日历补登',resources:'成长资源状态',compensation:'补偿领取',lottery:'盲盒抽奖',buddy_box:'Buddy 盲盒'};
    return legacy[key]||key;
  }
  const routes = {overview:['','工作台','先处理需要关注的事项。'], providers:['CONNECTIONS','服务接入','按服务查看已接入账号与真实可用能力。'], accounts:['IDENTITIES','账号','集中查看服务账号的启用状态、会话有效期和目录入口。'], models:['MODEL LIBRARY','模型目录','从上游获取可选模型。能力与可用范围以当前账号的响应为准。'], tasks:['AUTOMATION','任务','按分区查看任务、定时与运行状态。'], runs:['ACTIVITY','执行记录','查看每次派发、执行与服务端复核的结果。'], usage:['OBSERVABILITY','用量','查看网关累计请求与上游报告的用量。'], logs:['REQUEST HISTORY','请求记录','按请求查看模型、状态和耗时，不展示消息内容。'], keys:['API KEYS','API 密钥','用于调用模型接口的密钥；与面板登录账号无关。'], migration:['MIGRATION','迁移','只读核对迁移标记与工具状态；导入需离线确认。'], settings:['PREFERENCES','工作区设置','管理显示偏好、会话与迁移准备。']};
  const labels = {ready:'目录已同步',stale:'旧缓存',unavailable:'暂不可用',disabled:'已停用',queued:'等待执行',running:'执行中',succeeded:'已核验完成',failed:'失败',canceled:'已取消',uncertain:'待复核',skipped:'已跳过',manual_action:'需要人工',discover:'发现',inspect:'检查',execute:'执行',claim:'领取',verify:'复核'};
  const errorLabels = {desktop_unavailable:'官方豆包客户端未连接，请打开客户端并检查本机连接设置。',desktop_identity_mismatch:'客户端当前账号与已绑定账号不同，请切回原账号后刷新。',desktop_login_required:'请先在官方豆包客户端完成登录。',desktop_version_unsupported:'当前客户端版本尚未适配，请更新连接组件。',desktop_timeout:'官方客户端响应超时，请稍后刷新。',desktop_busy:'官方客户端正在处理目录请求，请稍后刷新。',desktop_upstream_rejected:'官方服务拒绝目录请求，请在客户端检查登录状态。',login_required:'登录已失效，请在官方客户端重新登录。',accounts_unavailable:'账号存储读取失败，请检查服务状态。',catalog_timeout:'目录读取超时，请稍后重试。',migration_timeout:'迁移预检超时：旧工作区可能过大或磁盘较慢，未写入任何文件；可改为在命令行离线执行预检。',catalog_failed:'目录读取失败，请稍后重试。',catalog_unavailable:'上游目录暂不可用。',account_not_found:'账号已不存在，请刷新账号列表。',schedule_not_saved:'计划未保存，请检查时间、时区和任务范围。',run_rejected:'执行请求未被接受，可能已有任务正在运行。',unauthorized:'管理会话已过期，请重新登录。'};
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
    const trigger=$('#'+id),label=(value?rows.find(a=>a.id===value)?.nickname||value:allLabel);
    trigger.disabled=false;trigger.setAttribute('aria-label','账号范围：'+label+'，更改');
    trigger.innerHTML=`${accountIcon}<span><small>账号范围</small><strong>${esc(label)}</strong></span><span class="scope-change">更改 <span aria-hidden="true">›</span></span>`;
    trigger.onclick=()=>{
      const route=state.route;
      const choice=(key,title,detail)=>`<button type="button" class="account-choice" data-account-choice="${esc(key)}" aria-pressed="${key===value}"><span class="choice-mark" aria-hidden="true">${key===value?'✓':''}</span><span><strong>${esc(title)}</strong><small>${esc(detail)}</small></span></button>`;
      openDialog('选择账号范围',`<p class="scope-description">${esc(providerName(state.route==='tasks'?state.taskProvider:state.modelProvider))} · 选择账号后立即应用</p><label class="scope-search">搜索账号<input id="scope-search" type="search" autocomplete="off" placeholder="输入账号标识" autofocus></label><div id="scope-all">${choice('',allLabel,`${rows.length} 个账号`)}</div><div class="scope-results"><p id="scope-count" class="small muted" role="status" aria-live="polite"></p><div id="scope-accounts" class="account-choices"></div></div><div class="scope-footer">${button('close','取消','','quiet')}</div>`,'account-dialog');
      const draw=()=>{
        const query=$('#scope-search').value.trim().toLowerCase(),filtered=rows.filter(a=>(a.id+' '+(a.nickname||'')+' '+(a.mobile||'')+' '+(a.enterpriseId||'')).toLowerCase().includes(query));
        $('#scope-count').textContent=`${query?'匹配':'可选'} ${filtered.length} 个账号`;
        $('#scope-accounts').innerHTML=filtered.length?filtered.map(a=>choice(a.id,a.nickname||a.id,a.id+' · '+(a.enabled?(a.realm==='global'?'国际区 · 已启用':a.realm==='cn'?'国内区 · 已启用':'已启用'):'已停用 · 可查看目录状态'))).join(''):empty(query?'没有匹配的账号':'暂无可选账号',query?'试试其他账号标识，或清空搜索。':'接入并启用账号后，可在这里切换范围。');
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
  function requestTimeout(path){return path==='connections/start'?40000:path==='migration/preflight'?120000:18000;}
  function timeoutErrorCode(path){return path==='migration/preflight'?'migration_timeout':'catalog_timeout';}
  async function api(path, {body, signal=state.controller?.signal, allowNotOK=false} = {}) {
    const controller=new AbortController(), abort=()=>controller.abort();
    if(signal?.aborted) controller.abort(); else signal?.addEventListener('abort',abort,{once:true});
    // 迁移预检在服务端有 120s 的时间预算（要遍历并摘要整个旧工作区），
    // 客户端 18s 就掐断会让慢盘上的正常预检变成失败；超时文案也必须区分，
    // 否则用户看到的是「目录读取超时」——那是模型目录，不是迁移。
    const timer=setTimeout(()=>controller.abort(),requestTimeout(path));
    try {
      const response=await fetch('api/'+path,{method:body===undefined?'GET':'POST',credentials:'same-origin',cache:'no-store',signal:controller.signal,headers:body===undefined?{}:{'Content-Type':'application/json','X-Admin-Request':'1'},body:body===undefined?undefined:JSON.stringify(body)});
      const data=await response.json();
      if(!response.ok || data.ok===false && !allowNotOK) throw new APIError(data.error || data.error_code || 'request_failed',response.status,typeof data.message==='string'?data.message:'');
      $('#sync-time').textContent='同步于 '+new Date().toLocaleTimeString('zh-CN',{hour12:false});
      return data;
    } catch(e) { if(e.name==='AbortError' && !signal?.aborted) throw new APIError(timeoutErrorCode(path),408); throw e; }
    finally { clearTimeout(timer); signal?.removeEventListener('abort',abort); }
  }
  async function providers() { if(!state.providers) { const data=await api('workbench/providers'); if(!Array.isArray(data.providers)) throw new APIError('request_failed',502); state.providers=data.providers; } return state.providers; }
  // 服务端 /api/state 的 AuthSummary 带了三项旧版账号信息，控制台曾把它们整体
  // 丢掉：parsable（凭证文件能否解析）、mobile（Trae 掩码手机号）、
  // enterpriseId（WorkBuddy 企业租户）。丢掉 parsable 的后果最重——损坏或旧格式
  // 的账号文件和「已启用但还没被池加载」在界面上完全一样，用户看不出为什么这个号
  // 从不工作。这里原样保留，并在渲染时单独说明。
  function authSummaryRow(a) {
    return {id:a.uid,nickname:a.nickname||'',enabled:!a.disabled,expires_at:a.expiresAt,realm:a.realm,
      pool:a.pool||null,credits:a.credits||null,mobile:a.mobile||'',enterpriseId:a.enterpriseId||'',
      parsable:a.parsable!==false};
  }
  function credentialWarning(a) {
    if(a.parsable!==false) return '';
    return '账号文件存在但无法解析，网关不会加载它；请重新登录，或核对旧版凭证格式后再启用。';
  }
  async function accountsFor(provider) {
    const p=state.providers.find(p=>p.id===provider);
    if(p?.catalog_scope==='account') return (await api('workbench/accounts')).accounts.filter(a=>a.provider===provider).map(a=>({id:a.id,nickname:a.nickname||'',mobile:a.mobile||'',botId:a.bot_id||'',verifiedAt:a.verified_at||null,enabled:a.enabled,expires_at:a.expires_at,revision:a.revision}));
    const data=await api('state');
    return (data.accounts || []).map(a=>({...a,realm:a.realm||(/trae/i.test(a.domain||'')?'trae':/workbuddy\.ai/i.test(a.domain||'')?'global':'cn')})).filter(a=>provider==='trae'?a.realm==='trae':a.realm==='cn'||a.realm==='global').map(authSummaryRow);
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
    $('#view').innerHTML=heading(button('reload','刷新状态','','quiet'))+`<div class="agenda-summary"><strong>${ps.reduce((n,p)=>n+p.accounts.total,0)}</strong> 个账号，<strong>${schedules.filter(s=>s.enabled).length}</strong> 个计划已启用。</div><div class="agenda-layout"><div><section class="work-section" data-workspace-agenda><div class="section-heading"><h2>需要关注</h2><a href="#tasks">打开任务</a></div>${attention.length?attention.slice(0,6).map(r=>`<div class="attention-line">${badge(r.status)}<div class="line-content"><h3>${esc(taskLabel(r.provider_id,'',r.task_key))}</h3><p>${esc(providerName(r.provider_id))}，${esc(labels[r.stage]||'等待派发')}。${r.status==='uncertain'?'结果未确认，请先核验，避免重复提交。':''}</p></div>${button('run-detail','查看',`data-index="${rows.indexOf(r)}"`,'quiet')}</div>`).join(''):empty('当前没有待处理的执行','可以到任务中心发现工作，或为已有任务安排计划。')}</section><section class="work-section"><div class="section-heading"><h2>最近执行</h2><a href="#tasks">到任务里查看</a></div>${rows.length?runTimeline(rows.slice(0,6)):empty('还没有执行记录','首次执行后，这里会显示经过服务端核验的结果。')}</section></div><aside class="service-aside"><h2>服务与账号</h2>${ps.map(p=>`<div class="service-note"><div><h3>${esc(p.name)}</h3><p>${p.accounts.enabled} / ${p.accounts.total} 个账号启用<br>${p.inference_available?'已接入推理':'目录已接入，推理待接通'}</p></div>${button('provider-models','查看目录',`data-provider="${esc(p.id)}"`,'text-link')}</div>`).join('')}</aside></div>`;
  }
  async function services() {
    $('#view').innerHTML=heading(button('reload','同步服务','','quiet'))+`<div class="service-list">${state.providers.map(p=>`<article class="service-entry" data-provider="${esc(p.id)}"><div><h2>${esc(p.name)}</h2><p>${p.catalog_scope==='account'?'目录随账号变化':'服务级模型目录'}</p></div><div><dl><div><dt>已连接账号</dt><dd>${p.accounts.total}</dd></div><div><dt>已启用</dt><dd>${p.accounts.enabled}</dd></div></dl><p>${p.inference_available?'消息推理已接入。请求仍受账号余额和上游可用性限制。':'模型目录可读取；此服务的消息转发尚未接通。'}</p>${p.error_code?notice(errorLabels[p.error_code]||'服务状态读取失败','error'):''}</div><div class="actions">${button('provider-models','查看模型',`data-provider="${esc(p.id)}"`,'quiet')}${button('provider-accounts','查看账号',`data-provider="${esc(p.id)}"`,'quiet')}</div></article>`).join('')}</div>`;
  }

  async function accounts() {
    const selected=state.accountProvider || state.providers[0]?.id || '';
    state.accountProvider=selected;const generation=state.version;
    $('#view').innerHTML=heading(button('connect-account','添加账号','','primary'))+`<div class="context-bar scope-toolbar">${providerFilter('account-provider',selected)}<label class="search"><span class="sr-only">搜索昵称或账号标识</span><input id="account-search" placeholder="搜索昵称或账号标识"></label></div><div id="client-status"></div><div id="account-list" class="loading">正在读取账号…</div><div id="connection-list"></div>`;
    bindProviderFilter('account-provider',value=>{state.accountProvider=value;});
    const rows=await accountsFor(selected);if(generation!==state.version)return;state.accountRows=rows;const clientStates=new Map();
    const draw=()=>{
      if(generation!==state.version)return;
      const query=$('#account-search').value.toLowerCase(),list=rows.filter(a=>(a.id+' '+(a.nickname||'')+' '+(a.mobile||'')+' '+(a.enterpriseId||'')).toLowerCase().includes(query));
      $('#account-list').className='account-directory';
      $('#account-list').innerHTML=list.length?table(['账号','状态','积分余额','会话与连接','操作'],list.map(a=>{
        const pool=a.pool,credit=a.credits,connection=clientStates.get(a.id);
        let status=badge(a.enabled?'ready':'disabled',a.enabled?'已启用':'已停用');
        if(a.parsable===false)status=badge('failed','凭证无法解析')+`<p>${esc(credentialWarning(a))}</p>`;
        else if(a.enabled&&pool){
          if(pool.disabled)status=badge('failed','池内不可用')+`<p>${esc(pool.disabledReason||'需核对账号状态')}</p>`;
          else if(pool.cooling)status=badge('stale','暂时避让')+`<p>${esc(pool.reason||'等待恢复')} · ${esc(date(pool.until))}</p>`;
          else if(pool.inPool)status=badge('ready','可用');
          else status=badge('manual_action','未加载');
          if(pool.creditsExhausted)status+=`<p>${pool.freeModelsUsable?'余额不足，免费模型可用':'余额不足'}</p>`;
          if(pool.rateLimitedModels?.length)status+=`<details><summary>${pool.rateLimitedModels.length} 个模型暂不可用</summary>${pool.rateLimitedModels.map(m=>`<p><code>${esc(m.model)}</code> · ${m.kind==='unavailable'?'上游未提供':esc(date(m.reset_at||m.until))}</p>`).join('')}</details>`;
        }
        const balance=typeof credit?.remain==='number'?`${number(credit.remain)}<p>${credit.expiring>0?'其中 '+number(credit.expiring)+' 即将过期':''}</p>`:credit?.ok===false?'读取失败':'未读取';
        const sessionText=connection?esc(LoomConnections.clientLabels[connection.state]||'待核验')+(connection.verified_at?`<p>最近核验 ${esc(date(connection.verified_at))}</p>`:''):selected==='doubao'?(a.verifiedAt?`<p>绑定核验于 ${esc(date(a.verifiedAt))}</p><p>官方客户端会话由本机管理</p>`:'<p>未提供会话信息</p>'):`${esc(date(a.expires_at))}${a.expires_at&&a.expires_at*1000<Date.now()?'<p>会话已过期</p>':''}`;
        return [`<strong class="account-name">${esc(a.nickname||(selected==='doubao'&&a.botId?'官方 Bot '+a.botId.slice(0,10):'未提供昵称'))}</strong>${a.mobile?`<p class="account-mobile">${esc(a.mobile)}${selected==='doubao'?'（脱敏）':''}</p>`:''}<p>${esc({cn:'WorkBuddy 国内版',global:'WorkBuddy 国际版',trae:'Trae'}[a.realm]||providerName(selected))}</p><details class="account-id"><summary>账号标识</summary><code>${esc(a.id)}</code>${a.enterpriseId?`<p class="muted small">企业：<code>${esc(a.enterpriseId)}</code></p>`:''}${a.botId?`<p class="muted small">官方 Bot：<code>${esc(a.botId)}</code></p>`:''}${a.verifiedAt?`<p class="muted small">核验于 ${esc(date(a.verifiedAt))}</p>`:''}</details>`,status,balance,sessionText,`<div class="actions">${button('account-models','查看目录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'compact')}${button('toggle-account',a.enabled?'停用':'启用',`data-provider="${esc(selected)}" data-index="${rows.indexOf(a)}"`,'compact')}${button('reconnect-account','重新登录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}" data-realm="${esc(a.realm||'')}"`,'quiet compact')}</div>`];
      })):empty(query?'没有匹配的账号':'尚未接入账号','账号接入状态由服务端读取。');
    };
    draw();$('#account-search').addEventListener('input',draw);if(selected==='doubao')await LoomConnections.client($('#client-status'),list=>{clientStates.clear();list.forEach(x=>clientStates.set(x.id,x));draw();});await LoomConnections.list($('#connection-list'));
  }
  async function modelPage() {
    if(!state.providers.some(p=>p.id===state.modelProvider)) state.modelProvider=state.providers[0]?.id || '';
    const generation=state.version;
    $('#view').innerHTML=heading(button('model-refresh','刷新目录','','quiet'))+`<div class="context-bar scope-toolbar">${providerFilter('model-provider',state.modelProvider)}${accountScope('model-account')}</div><div class="model-filters"><label class="search"><span class="sr-only">搜索模型</span><input id="model-search" type="search" placeholder="查找模型" value="${esc(state.search)}"></label><div id="model-capability" class="capability-choices" role="group" aria-label="模型能力">${[['','全部能力'],['native_tools','官方工作工具'],['supports_tool_call','API 工具调用'],['supports_images','图像输入'],['supports_reasoning','推理']].map(([id,name])=>`<button type="button" data-capability="${id}" aria-pressed="${id===state.capability}">${name}</button>`).join('')}</div></div><div id="model-notice"></div><div id="model-status"></div><div id="model-list"></div><div id="model-pagination" class="pagination"></div>`;
    bindProviderFilter('model-provider',value=>{state.modelProvider=value;state.modelAccount='';state.modelOffset=0;state.modelGroupPage=0;state.modelChannel='';});
    $('#model-capability').onclick=e=>{const b=e.target.closest('[data-capability]');if(!b)return;state.capability=b.dataset.capability;state.modelGroupPage=0;$('#model-capability').querySelectorAll('button').forEach(x=>x.setAttribute('aria-pressed',String(x===b)));drawModels();};
    $('#model-search').oninput=e=>{state.search=e.target.value;state.modelGroupPage=0;drawModels();};
    $('#model-list').onclick=e=>{const button=e.target.closest('[data-model-channel]');if(!button)return;state.modelChannel=button.dataset.modelChannel;state.modelGroupPage=0;drawModels();};
    const p=state.providers.find(p=>p.id===state.modelProvider);
    if(p.catalog_scope==='account') {
      const rows=await accountsFor(p.id);if(generation!==state.version)return;
      const bind=()=>bindAccountScope('model-account',rows,state.modelAccount,'所有账号（分页）',async value=>{state.modelAccount=value;state.modelOffset=0;state.modelGroupPage=0;bind();await loadModels(false);});bind();
    } else {state.modelAccount='';$('#model-account').innerHTML=`${accountIcon}<span><small>账号范围</small><strong>服务共享目录</strong></span>`;$('#model-account').setAttribute('aria-label','服务共享目录，无需选择账号');}
    // 首次进入模型页直接回源刷新一次：冷缓存/上次失败的负缓存会让列表空白，
    // 用户不该先点「刷新目录」才看到模型（用户反馈）。之后正常走缓存。
    const firstVisit=!state.modelLoaded;
    await loadModels(firstVisit);
    state.modelLoaded=true;
  }
  // 模型按「模型」聚合：同一模型在多个账号/渠道各有一份目录时只出现一次，
  // 来源数量收进摘要与详情（用户反馈：Hy4 preview 连出 5 行没法看）。
  function groupModels(models, p) {
    const map=new Map();
    for(const m of models){
      const bare=m.id.includes(':')?m.id.slice(m.id.indexOf(':')+1):m.id;
      const key=p.catalog_scope==='account'?bare:(m.name||bare);
      let g=map.get(key);
      if(!g){g={key,name:m.name||bare,model:m,entries:[]};map.set(key,g);}
      g.entries.push(m);
    }
    for(const g of map.values()){
      const e=g.entries[0];
      g.model=e;
      g.channels=[...new Set(g.entries.map(x=>x.id.includes(':')?x.id.slice(0,x.id.indexOf(':')):'').filter(Boolean))];
      g.accounts=[...new Set(g.entries.map(x=>x.account_id).filter(Boolean))];
      g.stale=g.entries.some(x=>x.catalog_status==='stale');
    }
    return [...map.values()].sort((a,b)=>a.name.localeCompare(b.name,'zh-Hans-CN'));
  }
  // /api/models 是全部 provider 的聚合目录（cn:/global:/trae:/doubao:…）。
  // 模型页是「按服务」展示的：必须按命名空间挑出属于本服务的行，否则豆包
  // 推理在桌面版注册后，doubao:* 会混进 WorkBuddy 的目录，用户选中的模型
  // 实际归另一个服务。这里不用「非 trae 即 WorkBuddy」的二选一。
  const providerModelScopes = {workbuddy:['cn:','global:'],trae:['trae:'],doubao:['doubao:']};
  function modelsForProvider(items, provider) {
    const prefixes = providerModelScopes[provider];
    if (!prefixes) return [];
    return (items || []).filter(m => m && typeof m.id === 'string' && prefixes.some(prefix => m.id.startsWith(prefix)));
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
        state.catalogs=[];state.models=modelsForProvider(data.items,p.id);
        $('#model-notice').innerHTML=notice('服务目录已接入推理。具体请求仍受账号余额、限流与模型可用性影响。');
      }
      drawModels();
    } catch(e) {if(e.name==='AbortError')return;state.models=[];$('#model-list').innerHTML=empty('目录读取未完成','可以稍后重试。');$('#model-notice').innerHTML=notice(e instanceof APIError?e.message:'无法连接服务，请检查网络。','error');}
    finally {if(generation===state.version){state.modelLoading=false;const b=$('[data-action=model-refresh]');if(b)b.disabled=false;}}
  }

  function drawModels() {
    const query=state.search.trim().toLowerCase(),p=state.providers.find(p=>p.id===state.modelProvider);
    const searched=state.models.filter(m=>(!query||(m.name+' '+m.id).toLowerCase().includes(query))&&(!state.capability||m[state.capability]===true));
    const channels=[...new Set(searched.map(m=>m.id.includes(':')?m.id.slice(0,m.id.indexOf(':')):'').filter(Boolean))];
    const all=state.modelChannel?searched.filter(m=>(m.id.includes(':')?m.id.slice(0,m.id.indexOf(':')):'')===state.modelChannel):searched;
    const groups=groupModels(all,p);
    state.visibleModels=all;
    if(!groups.length){state.modelGroups=[];$('#model-list').innerHTML=empty('没有匹配的模型','调整搜索、能力或账号筛选；账号不可用时请先检查登录状态。');return;}
    const pageSize=8,pages=Math.ceil(groups.length/pageSize);state.modelGroupPage=Math.min(state.modelGroupPage,Math.max(0,pages-1));
    const pageGroups=groups.slice(state.modelGroupPage*pageSize,(state.modelGroupPage+1)*pageSize);
    state.modelGroups=pageGroups;
    let selected=pageGroups.findIndex(g=>g.key===state.modelGroupKey);if(selected<0)selected=0;
    state.modelGroupIndex=selected;
    const channelChips=channels.length>1?`<div class="channel-filter" role="group" aria-label="渠道">${[['','全部'],...channels.map(c=>[c,channelName(c)])].map(([id,name])=>`<button type="button" data-model-channel="${esc(id)}" aria-pressed="${id===state.modelChannel}">${esc(name)}</button>`).join('')}</div>`:'';
    $('#model-list').innerHTML=`<div class="browser-workspace" id="model-browser"><div class="selection-pane"><p class="selection-caption">${groups.length} 个模型${groups.length!==all.length?'（'+all.length+' 个账号目录条目）':''}${p.inference_available?'':'，当前仅目录'}${pages>1?` · 第 ${state.modelGroupPage+1}/${pages} 页`:''}</p>${channelChips}<div role="listbox" aria-label="模型列表">${pageGroups.map((g,i)=>{const m=g.model;const capability=['官方工作工具','API 工具调用','图像输入','推理'].filter((label,k)=>m[['native_tools','supports_tool_call','supports_images','supports_reasoning'][k]]===true).join(' · ');return `<button class="selection-row" role="option" aria-selected="false" data-action="select-model-group" data-index="${i}" data-model-id="${esc(m.id)}" data-account="${esc(g.accounts[0]||'')}"><span><strong>${esc(g.name)}</strong><small>${esc([g.channels.map(c=>channelName(c)).join('/'),capability,g.accounts.length>1?g.accounts.length+' 个账号可见':''].filter(Boolean).join(' · '))}</small></span><span class="selection-arrow" aria-hidden="true">›</span></button>`;}).join('')}</div></div><section class="detail-pane" id="model-detail" tabindex="-1" aria-label="模型详情"></section></div><div id="model-group-pagination" class="pagination">${pages>1?`<span>第 ${state.modelGroupPage+1} / ${pages} 页</span><div class="actions">${button('model-group-prev','上一页',state.modelGroupPage===0?'disabled':'')}${button('model-group-next','下一页',state.modelGroupPage>=pages-1?'disabled':'')}</div>`:''}</div>`;
    selectModelGroup(selected,false);
  }
  function channelName(id){return ({cn:'国内版',global:'国际版',trae:'Trae',doubao:'豆包'})[id]||id;}
  function selectModelGroup(index,open=true) {
    const g=state.modelGroups?.[index];
    if(!g)return;
    state.modelGroupKey=g.key;state.modelGroupIndex=index;
    document.querySelectorAll('[data-action=select-model-group]').forEach((b,i)=>b.setAttribute('aria-selected',String(i===index)));
    const m=g.model,p=state.providers.find(p=>p.id===state.modelProvider),callable=m.inference_available??p.inference_available;
    const entries=g.entries;
    $('#model-detail').innerHTML=`${button('model-back','返回模型列表','','detail-back')}<div class="detail-subtitle">${esc(g.channels.map(c=>channelName(c)).join(' / ')||p.name)}${g.accounts.length?' · '+g.accounts.length+' 个账号可见':''}</div><h2 class="detail-title">${esc(g.name)}</h2><p class="detail-body">${callable?(p.catalog_scope==='account'?(m.supports_multi_turn?'支持网关普通文本调用及多轮上下文；文件和调用方工具仍受协议限制。实际可用性以当前账号为准。':'支持网关普通文本调用；多轮能力未在此目录声明，以实际请求结果为准。'):'可通过网关调用，实际请求受上游与账号状态约束。'):'此模型目前仅提供目录，尚不能通过网关 API 调用。下列能力与档位均按上游声明展示。'}</p>${g.stale?notice('部分账号显示的是旧缓存，最新刷新未成功。请核对账号目录状态。','warn'):''}<dl class="detail-specs">${callable&&entries.some(e=>state.catalogs.find(c=>c.account_id===e.account_id)?.streaming_mode==='incremental')?'<div><dt>文本流</dt><dd>支持即时输出；上游重写已发内容时明确中止，未完成的回答不可视为成功。</dd></div>':''}<div><dt>上下文窗口</dt><dd class="value-large">${m.context_length?number(m.context_length):'未声明'}</dd></div><div><dt>推理档位</dt><dd>${esc((m.reasoning_efforts||[]).map(x=>x.name||x.level).join(' / ')||'上游未声明')}</dd></div>${[['官方工作工具','native_tools'],['API 工具调用','supports_tool_call'],['图像输入','supports_images'],['推理','supports_reasoning']].filter(([,k])=>m[k]===true).map(([label])=>`<div><dt>${label}</dt><dd>上游已声明支持</dd></div>`).join('')}${m.tiers?.length?`<div><dt>档位说明</dt><dd>${esc(m.tiers.join(' / '))}</dd></div>`:''}</dl>${entries.length>1?`<details class="quiet-detail" open><summary> ${entries.length} 个账号来源</summary><div class="model-source-list">${entries.map(e=>`<div class="model-source-row"><div><code>${esc(e.account_id||'服务目录')}</code><p class="small muted">${esc(channelName(e.id.includes(':')?e.id.slice(0,e.id.indexOf(':')):''))} · ${esc(e.catalog_status==='stale'?'旧缓存':'已同步')}</p></div></div>`).join('')}</div></details>`:''}<details class="quiet-detail"><summary>查看模型标识与目录时间</summary><code>${esc(m.id)}</code><p>${g.entries[0].account_id?'由账号 '+esc(g.entries[0].account_id)+' 的目录提供。':'由服务级目录提供。'}</p></details>`;
    if(open){$('#model-browser').classList.add('detail-open');$('#model-detail').focus({preventScroll:true});}
  }


  // 任务按「任务定义」聚合：同一 provider + task_key 只出现一次，账号数收进
  // 摘要与详情。176 条「每个号 × 每个任务」会把真正的任务差异淹没（用户反馈）。
  function groupTasks(tasks) {
    const map=new Map();
    for(const t of tasks){
      // 同一区域内各账号发现的任务集合相同（WorkBuddy cn/global 各自一套、
      // Trae 独立一套）。按「区域 + 任务」聚合，账号只在详情里逐条展示，
      // 避免 176 条「账号 × 任务」平铺（用户反馈：每个号都列一次没有意义）。
      const realm=t.realm||'';
      const key=t.provider_id+'\u0000'+realm+'\u0000'+t.key;
      let g=map.get(key);
      if(!g){g={key:t.key,provider_id:t.provider_id,realm,name:t.name,description:t.description,once:t.once,once_per_day:t.once_per_day,manual_only:t.manual_only,reward:t.reward,accounts:[]};map.set(key,g);}
      g.accounts.push(t);
      // 能力取并集：只要任一账号声明手动/奖励，就在组上提示。
      if(t.manual_only)g.manual_only=true;
      if(!g.reward&&t.reward)g.reward=t.reward;
    }
    return [...map.values()].sort((a,b)=>a.name.localeCompare(b.name,'zh-Hans-CN')||realmLabel(a.realm).localeCompare(realmLabel(b.realm),'zh-Hans-CN'));
  }
  function realmLabel(realm){return ({cn:'国内区',global:'国际区',trae:'Trae 区'})[realm]||'全部区域';}
  function groupPhaseCounts(g){
    const counts={claimable:0,pending:0,ready:0,manual:0,blocked:0,done:0,unknown:0};
    for(const t of g.accounts){const p=t.state?.phase;counts[p&&counts[p]!==undefined?p:'unknown']++;}
    return counts;
  }
  // One page for automation, in the shape of a cron panel: every row is a task in
  // a section, with its schedule inline and a single "运行" button. There is no
  // separate account picker — a section means "every enabled account in this
  // service and region", which is how tasks are actually discovered.
  async function tasks() {
    state.taskProvider=state.taskProvider||state.providers[0]?.id||'';
    const generation=state.version;
    $('#view').innerHTML=heading(button('reload','刷新','','quiet')+button('open-runs','回到任务列表','','quiet'))+
      `<div class="context-bar scope-toolbar">${providerFilter('task-provider',state.taskProvider)}</div><div id="task-list" class="loading">正在发现任务…<p class="small muted">正在读取各分区的任务目录；较慢的模块不会阻止其他任务显示。</p></div>`;
    bindProviderFilter('task-provider',value=>{state.taskProvider=value;});
    const [accounts,work]=await Promise.all([accountsFor(state.taskProvider),api('workbench')]);
    if(generation!==state.version)return;
    state.schedules=work.schedules||[];
    let data;
    try{data=await api('workbench/catalog?'+new URLSearchParams({provider:state.taskProvider}));}
    catch(e){if(e.name==='AbortError')return;throw e;}
    if(generation!==state.version)return;
    state.tasks=data.tasks||[];
    state.taskGroups=groupTasks(state.tasks);
    state.taskNames=state.taskNames||new Map();
    for(const t of state.tasks){
      state.taskNames.set(t.provider_id+'\u0000'+(t.realm||'')+'\u0000'+t.key,t.name);
      state.taskNames.set(t.provider_id+'\u0000\u0000'+t.key,t.name);
    }
    saveTaskNames(state.taskNames);
    paintTaskList(data.issues||[],false,accounts);
  }
  // A schedule belongs to a section when it targets the same provider and region
  // and either this exact task key or the whole module.
  function schedulesFor(g){
    return state.schedules.filter(s=>{
      if(s.provider_id!==g.provider_id)return false;
      if((s.realm||'')!==(g.realm||''))return false;
      const key=s.task_key||'';
      if(!key||key==='*')return true;
      if(key.endsWith('/'))return g.key.startsWith(key);
      return key===g.key;
    });
  }
  function scheduleText(s){
    if(!s)return '';
    return (s.hours||[]).map(h=>String(h).padStart(2,'0')+':'+String(s.minute||0).padStart(2,'0')).join('、')+(s.enabled?'':'（已停用）');
  }
  function paintTaskList(issues,cached,accounts=[]){
    const box=$('#task-discovery-notice');if(box)box.remove();
    if(issues.length){
      const byModule=new Map();for(const x of issues){byModule.set(x.module_id,(byModule.get(x.module_id)||0)+1);}
      const summary=[...byModule.entries()].map(([name,n])=>n>1?`${name} × ${n}`:name).join('、');
      const notice=document.createElement('details');notice.className='notice warn';notice.id='task-discovery-notice';notice.setAttribute('role','status');notice.dataset.discoveryIssues=String(issues.length);
      notice.innerHTML=`<summary>${issues.length} 项任务目录未能读取（${esc(summary)}）· 其他任务仍可执行</summary><p class="small">这些分区的对应模块本轮没有返回目录，通常是上游暂时不可用或该账号没有此项任务。稍后刷新重试。</p><ul class="issue-list">${issues.map(x=>`<li><code>${esc(x.module_id)}</code> · ${esc(x.account_id)} · ${esc(errorLabels[x.error_code]||x.error_code||'目录不可用')}</li>`).join('')}</ul>`;
      $('#task-list').before(notice);
    }
    if(!state.taskGroups.length){
      const provider=state.taskProvider;
      const enabled=accounts.filter(a=>a.enabled).length;
      let detail='请确认账号已启用。';
      if(!accounts.length&&provider==='doubao')detail='还没有豆包账号。请先在「账号」里添加豆包账号，添加后会自动发现签到与每日任务。';
      else if(!accounts.length&&provider==='trae')detail='还没有 Trae 账号。请先在「账号」里完成官方授权，接入后会自动发现任务。';
      else if(!accounts.length)detail='还没有该服务的账号。请先在「账号」里接入，任务会在账号可用后自动出现。';
      else if(!enabled)detail='该服务的账号均已停用。启用账号后刷新即可发现任务。';
      else if(provider==='doubao'&&issues.some(x=>x.error_code==='desktop_activity_unavailable'))detail='已接入豆包账号，但官方本次没有返回积分活动目录。请确认账号登录有效，稍后刷新重试。';
      $('#task-list').innerHTML=empty('未发现可用任务',detail)+`<div class="empty-actions">${button('connect-account','添加账号',`data-provider="${esc(provider)}"`,'primary')}${button('reload','刷新任务','','quiet')}</div>`;
      return;
    }
    const sections=new Map();
    for(let i=0;i<state.taskGroups.length;i++){
      const g=state.taskGroups[i];
      const label=providerName(g.provider_id)+(g.realm?' · '+realmLabel(g.realm):'');
      if(!sections.has(label))sections.set(label,[]);
      sections.get(label).push({g,i});
    }
    const rows=[];
    for(const [label,items] of sections){
      rows.push(`<tr class="task-section"><th colspan="6">${esc(label)}<span class="small muted"> · ${items.length} 项任务 · ${items[0].g.accounts.length} 个账号</span></th></tr>`);
      for(const item of items){
        const g=item.g,i=item.i;
        const c=groupPhaseCounts(g);
        const summary=[c.claimable?c.claimable+' 可领取':c.ready?c.ready+' 待执行':c.done?c.done+' 已完成':c.manual?c.manual+' 需人工':'',c.blocked?c.blocked+' 暂不可用':''].filter(Boolean).join(' · ')||'等待核验';
        const pending=g.accounts.some(t=>t.pending_verification);
        const sched=schedulesFor(g)[0];
        const scheduleCell=sched?`<strong>${esc(scheduleText(sched))}</strong><p class="small muted">每天 · ${esc(sched.timezone||'')}</p>`:'<span class="muted">未定时</span>';
        rows.push(`<tr data-task-group="${i}">
          <td class="log-expander"><button type="button" data-action="task-log" data-index="${i}" aria-expanded="false" aria-label="展开最近执行日志">▸</button></td>
          <td><strong>${esc(g.name)}</strong><p class="small muted">${esc(g.once?'一次性':g.once_per_day?'每日':'按需')}${g.manual_only?' · 需要人工':''}${pending?' · 上次结果未确认':''}</p></td>
          <td><span class="small">${esc(summary)}</span></td>
          <td>${scheduleCell}</td>
          <td>${sched?badge(sched.enabled?'ready':'disabled',sched.enabled?'已启用':'已停用'):badge('neutral','—')}</td>
          <td><div class="actions">${g.manual_only?button('select-task-group','查看要求',`data-index="${i}"`,'quiet compact'):button('run-task-group','运行',`data-index="${i}"`,'compact')}${g.manual_only?'':button('task-schedule-group',sched?'改定时':'定时',`data-index="${i}"`,'quiet compact')}${sched?button('toggle-schedule','停用',`data-index="${state.schedules.indexOf(sched)}" data-lock`,'quiet compact'):''}${button('task-log','日志',`data-index="${i}" data-key="${esc(g.key)}" data-realm="${esc(g.realm||'')}"`,'quiet compact')}</div></td>
        </tr>`);
      }
    }
    $('#task-list').innerHTML=`<table class="task-table"><thead><tr><th></th><th>任务</th><th>账号状态</th><th>定时</th><th>状态</th><th>操作</th></tr></thead><tbody>${rows.join('')}</tbody></table>`+(cached?'<p class="small muted">正在后台刷新任务目录。</p>':'');
  }
  const taskErrors={provider_rejected:'官方服务拒绝了此操作',operation_unconfirmed:'操作结果未确认',account_identity_changed:'账号或绑定发生变化',authentication_required:'需要重新登录',task_expired:'任务标识已过期',operation_not_eligible:'当前尚不满足操作条件',provider_busy:'官方客户端正忙',upstream_failed:'上游操作未成功'};
  const taskPhases={ready:'待执行',pending:'上游处理中',claimable:'可领取奖励',done:'上游已完成',manual:'待完成条件',blocked:'暂不可用'};
  function selectTaskGroup(index,open=true) {
    const g=state.taskGroups[index];if(!g)return;
    state.taskGroupIndex=index;
    document.querySelectorAll('[data-action=select-task-group]').forEach((b,i)=>b.setAttribute('aria-selected',String(i===index)));
    const c=groupPhaseCounts(g),actionable=g.accounts.filter(t=>!t.manual_only);
    $('#task-detail').innerHTML=`${button('task-back','返回任务列表','','detail-back')}<div class="detail-subtitle">${esc(providerName(g.provider_id))} / ${g.once?'一次性工作':g.once_per_day?'每日工作':'按需执行'} · ${g.accounts.length} 个账号</div><h2 class="detail-title">${esc(g.name)}</h2><p class="detail-body">${esc(g.description)}</p>${g.accounts.some(t=>t.pending_verification)?notice('上次操作结果尚未确认。系统会先核验服务端状态，并阻止重复提交。','warn'):''}<dl class="detail-specs"><div><dt>上游状态</dt><dd data-task-state="${esc(g.accounts.every(t=>t.state?.phase==='manual')?'manual':g.accounts.every(t=>t.state?.phase==='done')?'done':g.accounts.some(t=>t.state?.phase==='claimable')?'claimable':'pending')}">${esc(g.accounts.length===1&&taskPhases[g.accounts[0].state?.phase]?taskPhases[g.accounts[0].state?.phase]:'按账号核验')}</dd></div></dl><div class="phase-summary">${[['claimable','可领取',c.claimable],['ready','待执行',c.ready],['done','已完成',c.done],['manual','需人工',c.manual],['blocked','暂不可用',c.blocked]].filter(([, ,n])=>n).map(([id,label,n])=>`<span class="phase-chip" data-phase="${id}">${label} ${n}</span>`).join('')||'<span class="phase-chip">等待核验</span>'}</div><div class="actions">${actionable.length?button('run-task-group','为全部 '+actionable.length+' 个账号执行',`data-index="${index}"`,'primary'):''}${actionable.length?button('task-schedule-group','为 '+actionable.length+' 个可执行账号安排计划',`data-index="${index}"`,'quiet'):''}${button('open-runs','回到任务列表','','quiet')}</div><h3 class="section-gap">账号明细</h3><div class="account-task-list">${g.accounts.map(t=>{const ti=state.tasks.indexOf(t),phase=t.state?.phase;return `<div class="account-task-row" data-account-task="${esc(t.account_id)}"><div><strong>${esc(t.account_id)}</strong><p class="small muted">${esc(taskPhases[phase]||'待核验')}${t.state?.total?' · '+number(t.state.current||0)+' / '+number(t.state.total):''}${t.state?.summary?' · '+esc(t.state.summary):''}${t.pending_verification?' · 上次结果未确认':''}</p></div><div class="actions">${button(t.manual_only?'manual-task':'run-task',t.manual_only?'查看所需操作':t.pending_verification?'核验上次操作':phase==='claimable'?'领取奖励':phase==='done'?'再次核验':phase==='manual'?'核验进度':phase==='pending'?'核验状态':'执行任务',`data-task-index="${ti}"`,'compact')}${!t.manual_only?button('task-schedule','安排计划',`data-task-index="${ti}"`,'quiet compact'):''}</div></div>`;}).join('')}</div><p class="detail-note">${g.manual_only?'按要求提供真实内容。完成与奖励以服务端核验为准。':'任务会进入执行队列；已提交不代表已完成。结果在执行记录中保留。'}</p>`;
    if(open){$('#task-browser').classList.add('detail-open');$('#task-detail').focus({preventScroll:true});}
  }
  // The task log is the old run-detail dialog: pick the task's most recent run and
  // show the same complete breakdown the standalone log page used to show (steps,
  // per-step result, failure reason, discovery issues). Nothing navigates away.
  async function openTaskLog(g){
    if(!g)return;
    openDialog(`${g.name} · 执行详情`,'<p class="muted">正在读取该项任务的执行记录…</p>');
    try{
      const data=await api('workbench',{signal:null});
      const runs=(data.runs||[]).filter(r=>r.provider_id===g.provider_id&&(r.task_key||'')===g.key);
      if(!runs.length){
        $('#dialog-body').innerHTML=`<div class="form"><p class="muted">这项任务还没有执行记录。点「运行」后结果会出现在这里。</p><div class="actions"><button data-action="close">关闭</button></div></div>`;
        $('#dialog-body [data-action=close]').onclick=closeDialog;
        return;
      }
      state.taskLogRuns=runs;
      showTaskRunDetail(runs,0);
    }catch(e){
      closeDialog();
      toast(e instanceof APIError?e.message:'执行记录读取失败');
    }
  }
  // Renders one run exactly like the old detail dialog: a run picker at the top
  // when there is more than one, then the full step table.
  function showTaskRunDetail(runs,index){
    const r=runs[index];
    if(!r)return;
    const picker=runs.length>1?`<label>执行批次<select id="task-log-run">${runs.map((x,i)=>`<option value="${i}" ${i===index?'selected':''}>${esc(date(x.started_at))} · ${esc(labels[x.status]||x.status)}</option>`).join('')}</select></label>`:'';
    const steps=r.steps||[];
    $('#dialog-body').innerHTML=`<div class="form run-detail-dialog">
      ${picker}
      <dl class="detail-specs">
        <div><dt>任务</dt><dd>${esc(taskLabel(r.provider_id,'',r.task_key))}</dd></div>
        <div><dt>服务</dt><dd>${esc(providerName(r.provider_id))}</dd></div>
        <div><dt>账号范围</dt><dd>${esc((r.account_ids||[]).join('、')||'全部已启用账号')}</dd></div>
        <div><dt>状态</dt><dd>${badge(r.status)}</dd></div>
        <div><dt>开始时间</dt><dd>${esc(date(r.started_at))}</dd></div>
        <div><dt>执行标识</dt><dd><code>${esc(r.id)}</code></dd></div>
      </dl>
      ${r.status==='uncertain'?notice('上游操作结果尚未确认，请先核验服务端状态，不要重复执行。','warn'):''}
      ${runDiscoveryIssues(r.discovery_issues)}
      <h3 class="section-gap">执行步骤</h3>
      ${steps.length?(()=>{const columns=runStepColumns(steps),withTask=columns.length===4;return table(columns,steps.map(s=>runStepCells(s,withTask)));})():empty('尚无执行步骤','任务可能仍在等待派发。')}
      <div class="actions"><button data-action="close" class="primary">关闭</button></div>
    </div>`;
    $('#dialog-body [data-action=close]').onclick=closeDialog;
    const select=document.getElementById('task-log-run');
    if(select)select.onchange=e=>showTaskRunDetail(runs,Number(e.target.value));
  }

  function taskForGroup(g){
    return {provider_id:g.provider_id,key:g.key,name:g.name,account_ids:g.accounts.filter(t=>!t.manual_only).map(t=>t.account_id),realm:g.realm};
  }

  function confirmRun(task) {
    // 组级执行传 account_ids（数组），单条任务传 account_id。必须优先用数组，
    // 否则组级执行会退化成「全部已启用账号」——执行范围被放大（本轮实测缺陷）。
    const explicit=Array.isArray(task?.account_ids)?task.account_ids.filter(Boolean):task?.account_id?[task.account_id]:[];
    const scope=explicit;
    const req={provider_id:task?.provider_id || state.taskProvider,task_key:task?.key || '*',account_ids:scope};
    if(!scope.length){toast('没有可执行的账号范围，已取消。');return;}
    if(sessionStorage.getItem('loomgate.unconfirmedRun')) {toast('上次执行请求结果未确认。请到任务列表核对后继续，避免重复操作。');location.hash='tasks';return;}
    openDialog('确认执行范围',`<form id="run-form" class="form"><p>${esc(task?.name || '当前范围内的全部任务')}</p><div class="notice">服务：${esc(providerName(req.provider_id))}<br>账号：${esc(scope.join('、') || '全部已启用账号')}<br>执行可能产生上游操作。需要人工的任务会保留待处理状态，奖励以服务端复核为准。</div><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认执行</button></div></form>`);
    $('#run-form').onsubmit=e=>{e.preventDefault();if(state.pending.has('run'))return;sessionStorage.setItem('loomgate.unconfirmedRun','1');mutate('run','workbench/run',req,async data=>{if(!data.run?.id)throw new APIError('request_failed',502);sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();toast('已进入执行队列，结果可在该项任务的「日志」里查看。');location.hash='tasks';});};
  }

  async function schedules() {
    const data=await api('workbench');state.schedules=data.schedules||[];
    const nameFor=s=>taskLabel(s.provider_id,s.realm,s.task_key);
    $('#view').innerHTML=heading(button('new-schedule','选择已有任务','','primary'))+(state.schedules.length?`<div class="plan-list">${state.schedules.map((s,i)=>`<article class="plan-row" data-schedule-id="${esc(s.id)}"><div class="plan-time">${s.hours.map(h=>String(h).padStart(2,'0')+':'+String(s.minute).padStart(2,'0')).join('、')}<small>每天 / ${esc(s.timezone)}</small></div><div><h3>${esc(providerName(s.provider_id))}：${esc(nameFor(s))}</h3><p>为 ${esc(s.account_ids?.length?s.account_ids.join('、'):'全部已启用账号')} 执行。${s.once_per_day?'同一天最多执行一次。':'每个时段都执行。'}</p>${badge(s.enabled?'ready':'disabled',s.enabled?'已启用':'已停用')}${s.legacy_enabled===true?`<span class="badge warn" data-legacy-source="enabled">旧版曾启用</span>`:s.legacy_enabled===false?`<span class="badge neutral" data-legacy-source="disabled">旧版未启用</span>`:''}<p>下次 ${esc(date(s.next_run))}</p><details class="quiet-detail"><summary>任务与计划标识</summary><p>${esc(s.task_key||'全部任务')}</p><code>${esc(s.id)}</code></details></div><div class="actions">${button('edit-schedule','编辑',`data-index="${i}"`,'quiet')}${button('toggle-schedule',s.enabled?'停用':'启用',`data-index="${i}" data-lock`,'quiet')}${button('delete-schedule','删除',`data-index="${i}"`,'quiet danger')}</div></article>`).join('')}</div>`:empty('安排第一项工作','到任务中心选择一项已添加的任务，再设置时间。'));
  }

  // legacy_enabled 是「旧配置原本是否启用」的审计事实。编辑/停启用计划时
  // 必须原样带回，否则一次改时间就会把迁移来源抹掉（rename/badge 也随之消失）。
  function cleanSchedule(s) {return {id:s.id,provider_id:s.provider_id,task_key:s.task_key,account_ids:s.account_ids || [],hours:s.hours,minute:s.minute,timezone:s.timezone,enabled:s.enabled,once_per_day:s.once_per_day,legacy_enabled:s.legacy_enabled};}
  async function verifySchedule(s) {const result=await api('workbench');const saved=(result.schedules || []).find(x=>x.id===s.id);if(!saved || JSON.stringify(cleanSchedule(saved))!==JSON.stringify(cleanSchedule(s)))throw new APIError('request_failed',502);}

  // Schedules are now edited in one compact dialog instead of a three-step wizard:
  // a cron-panel user changes the time and saves. Task and section are already
  // decided by the row the dialog was opened from.
  async function scheduleEditor(existing,task){
    const draft=existing?cleanSchedule(existing):{id:'plan-'+LoomPlanEditor.planID(),provider_id:task?.provider_id||'',task_key:task?.key||'',account_ids:[],hours:[9],minute:0,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone||'Asia/Shanghai',enabled:true,once_per_day:true};
    const times=draft.hours.map(h=>String(h).padStart(2,'0')+':'+String(draft.minute).padStart(2,'0'));
    const timeField=list=>`<div class="time-item"><input type="time" name="plan-time" aria-label="执行时间" value="${esc(list)}" required><button type="button" data-remove-time aria-label="移除">×</button></div>`;
    openDialog(existing?'修改定时':'设置定时',`<form id="schedule-form" class="form">
      <p class="muted">${existing?esc(taskLabel(existing.provider_id,existing.realm,existing.task_key)):esc(task?.name||task?.key||'')} · ${esc(providerName(draft.provider_id))}${task?.realm?' · '+esc(realmLabel(task.realm)):''}</p>
      <label>执行时间<div class="time-list" id="plan-times">${times.map(timeField).join('')}</div></label>
      <button type="button" id="plan-add-time" class="quiet">添加时段</button>
      <label>时区<input name="timezone" value="${esc(draft.timezone)}" list="timezones" required><datalist id="timezones"><option>Asia/Shanghai</option><option>UTC</option><option>America/New_York</option><option>Europe/London</option></datalist></label>
      <label class="check"><input name="daily" type="checkbox" ${draft.once_per_day?'checked':''}>同一天只执行一次</label>
      <label class="check"><input name="enabled" type="checkbox" ${draft.enabled?'checked':''}>启用定时</label>
      <p id="form-message" class="notice error" hidden></p>
      <p class="detail-note">保存后从服务端读回确认。未启用的定时只保存，不会派发。</p>
      <div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div>
    </form>`);
    const form=$('#schedule-form');
    const readTimes=()=>[...form.querySelectorAll('input[name=plan-time]')].map(i=>i.value);
    const paint=()=>{
      const values=readTimes();
      $('#plan-times').innerHTML=values.map(v=>timeField(v)).join('');
      form.querySelectorAll('[data-remove-time]').forEach((b,i)=>{b.disabled=values.length===1;b.onclick=()=>{const cur=readTimes();cur.splice(i,1);$('#plan-times').dataset.values=JSON.stringify(cur);reloadTimes(cur);};});
    };
    function reloadTimes(values){
      $('#plan-times').innerHTML=values.map(v=>timeField(v)).join('');
      form.querySelectorAll('[data-remove-time]').forEach((b,i)=>{b.disabled=values.length===1;b.onclick=()=>{const cur=readTimes();cur.splice(i,1);reloadTimes(cur);};});
    }
    paint();
    $('#plan-add-time').onclick=()=>{const cur=readTimes();if(cur.length>=24)return;const used=new Set(cur.map(t=>t.slice(0,2)));let h=18;while(used.has(String(h).padStart(2,'0'))&&h<24)h++;if(h===24)h=0;cur.push(String(h).padStart(2,'0')+':00');reloadTimes(cur);};
    form.onsubmit=e=>{
      e.preventDefault();
      const values=readTimes();
      if(!values.length||values.some(t=>!/^([01]\d|2[0-3]):[0-5]\d$/.test(t))||new Set(values).size!==values.length){
        $('#form-message').textContent='请填写有效且不重复的执行时间。';$('#form-message').hidden=false;return;
      }
      const minutes=values.map(t=>Number(t.slice(3)));
      if(new Set(minutes).size!==1){$('#form-message').textContent='同一计划的多个时段需使用相同分钟；不同分钟请分两条。';$('#form-message').hidden=false;return;}
      const zone=form.timezone.value.trim();
      try{new Intl.DateTimeFormat('zh-CN',{timeZone:zone}).format();}catch{$('#form-message').textContent='时区无效，例如 Asia/Shanghai。';$('#form-message').hidden=false;return;}
      const payload={...draft,hours:values.map(t=>Number(t.slice(0,2))).sort((a,b)=>a-b),minute:minutes[0],timezone:zone,enabled:form.enabled.checked,once_per_day:form.daily.checked};
      mutate('schedule','workbench/schedule',payload,async()=>{await verifySchedule(payload);closeDialog();toast('定时已保存，并已从服务端读回。');await reload();});
    };
  }

  // 一条计划可以用模块前缀覆盖多个真实任务（迁移来的 growth/ 会展开成
  // growth/accept/<码> 与 growth/<码>）。这时执行详情的步骤表必须带上「任务」列，
  // 否则运维只看到「执行/检查 + 账号 + 成功」，分不清哪一步属于哪个任务，
  // 失败时也不知道该去官方界面核对哪一项。只覆盖一个任务时不加列，避免噪音。
  function runStepColumns(steps) {
    const keys = new Set((steps || []).map(s => s && s.task_key).filter(Boolean));
    return keys.size > 1 ? ['任务','阶段','账号','结果'] : ['阶段','账号','结果'];
  }
  function runStepCells(step, withTask) {
    const note = step.summary ? `<p class="small run-summary">${esc(step.summary)}</p>` : '';
    const reason = step.error_code ? `<p class="small muted">${esc(taskErrors[step.error_code] || '操作未完成')}${Number.isInteger(step.upstream_code) ? '（业务码 ' + step.upstream_code + '）' : ''}</p>` : '';
    const base = [esc(labels[step.stage] || step.stage), `<code>${esc(step.account_id)}</code>`, `${badge(step.status)}${reason}${note}`];
    return withTask ? [`<code>${esc(step.task_key || '—')}</code>`, ...base] : base;
  }
  const discoveryIssueLabels = {module_discovery_failed:'模块状态读取失败',desktop_activity_unavailable:'官方客户端任务目录不可用',desktop_identity_mismatch:'官方客户端账号身份不匹配',desktop_login_required:'官方客户端需要登录'};
  function runDiscoveryIssues(issues) {
    if (!issues?.length) return '';
    return `<section class="section-gap"><h4>任务发现问题</h4>${table(['账号','模块','原因'],issues.map(issue=>[`<code>${esc(issue.account_id)}</code>`,`<code>${esc(issue.module_id)}</code>`,esc(discoveryIssueLabels[issue.error_code] || '任务目录读取失败')]))}</section>`;
  }
  function runTimeline(rows) {return `<ol class="event-list">${rows.map(r=>`<li><time>${esc(date(r.started_at))}</time><div><h3>${esc(taskLabel(r.provider_id,'',r.task_key))}</h3><p>${esc(providerName(r.provider_id))} · ${esc(labels[r.stage] || '等待派发')}</p></div>${badge(r.status)}</li>`).join('')}</ol>`;}
  async function runs() {
    const data=await api('workbench');state.runs=data.runs || [];
    $('#view').innerHTML=heading(button('reload','刷新记录','','quiet'))+(sessionStorage.getItem('loomgate.unconfirmedRun')?notice('有一次执行提交结果未确认。请核对执行时间、服务和账号；不要直接重复提交。','warn')+button('review-submission','核对后解除提交保护'):'')+`<div id="run-poll" class="muted small" role="status"></div>`+(state.runs.length?`<ol class="execution-journal">${state.runs.map((r,i)=>`<li data-run-id="${esc(r.id)}"><time>${esc(date(r.started_at))}</time><div class="journal-content"><h3>${esc(taskLabel(r.provider_id,'',r.task_key))}</h3><p>${esc(providerName(r.provider_id))}，${esc(r.account_ids?.join('、')||'全部已启用账号')}</p><p>${esc(labels[r.stage]||'等待派发')}${r.discovery_issues?.length?'，'+r.discovery_issues.length+' 项发现失败':''}</p><code>${esc(r.id)}</code></div><div>${badge(r.status)}<div class="actions">${button('run-detail','详情',`data-index="${i}"`,'quiet')}${['queued','running'].includes(r.status)?button('cancel-run','取消',`data-index="${i}" data-lock`,'quiet danger'):''}</div></div></li>`).join('')}</ol>`:empty('还没有执行记录','执行请求会先进入队列；完成结果在此显示。'));
    if(state.runs.some(r=>['queued','running'].includes(r.status))) {
      if(!state.pollStarted)state.pollStarted=Date.now();
      if(Date.now()-state.pollStarted<90000){$('#run-poll').textContent='正在同步执行状态；离开页面将停止同步。';const v=state.version;state.poll=setTimeout(()=>{if(v===state.version)runs().catch(fail);},Math.min(8000,1000+Math.floor((Date.now()-state.pollStarted)/8000)*1000));}
      else $('#run-poll').textContent='自动同步已达到 90 秒上限。任务仍在服务端运行，可点击刷新继续查看。';
    } else state.pollStarted=0;
  }
  // Usage follows the shape of a metering dashboard: a date range, KPI tiles, and
  // breakdowns by model and caller. The range is the only time control — the list
  // of individual days is not useful at a glance and is not shown.
  function usageDays(data){return (data.days||[]).slice();}
  function usageSum(days){
    const out={requests:0,failed_requests:0,unreported_requests:0,prompt_tokens:0,completion_tokens:0,cached_tokens:0,total_tokens:0,credit:0};
    for(const d of days){
      const t=d.totals||{};
      for(const k of Object.keys(out))if(typeof t[k]==='number')out[k]+=t[k];
    }
    return out;
  }
  function usageInRange(days,from,to){
    if(!from&&!to)return days;
    return days.filter(d=>(!from||d.day>=from)&&(!to||d.day<=to));
  }
  async function usage() {
    const data=await api('usage'),totals=data.totals;
    if(!totals)throw new APIError('request_failed',503);
    const allDays=usageDays(data);
    const today=new Date().toISOString().slice(0,10);
    const cutoff=n=>new Date(Date.now()-(n-1)*86400000).toISOString().slice(0,10);
    state.usageRange=state.usageRange||{from:'',to:''};
    const range=state.usageRange;
    const scoped=usageInRange(allDays,range.from,range.to);
    const scopedTotals=range.from||range.to?usageSum(scoped):totals;
    const rangeLabel=(()=>{
      if(!range.from&&!range.to)return '全部时间';
      if(range.from===today&&range.to===today)return '今天';
      if(range.from===cutoff(7)&&range.to===today)return '近 7 天';
      if(range.from===cutoff(30)&&range.to===today)return '近 30 天';
      if(range.from===range.to)return range.from;
      if(!range.from)return '截至 '+range.to;
      if(!range.to)return range.from+' 起';
      return range.from+' 至 '+range.to;
    })();
    // Per-model figures come from the scoped day buckets when a range is active;
    // the all-time totals are used only when no range is selected.
    const keys=data.keys||[];
    const byModel=new Map();
    for(const key of keys){
      for(const m of (key.models||[])){
        const row=byModel.get(m.model)||{requests:0,prompt:0,completion:0,cached:0,credit:0};
        row.requests+=m.totals.requests||0;
        row.prompt+=m.totals.prompt_tokens||0;
        row.completion+=m.totals.completion_tokens||0;
        row.cached+=m.totals.cached_tokens||0;
        row.credit+=m.totals.credit||0;
        byModel.set(m.model,row);
      }
    }
    const models=[...byModel.entries()].sort((a,b)=>(b[1].prompt+b[1].completion)-(a[1].prompt+a[1].completion));
    const pct=(n,total)=>total>0?Math.round(n/total*1000)/10:0;
    const palette=['#4f7df3','#3bb273','#e8a33d','#c85fd0','#38a4c9','#9b8cf0','#e0644f'];
    const segments=models.slice(0,6).map((e,i)=>({value:e[1].prompt+e[1].completion,color:palette[i%palette.length]}));
    const rest=models.slice(6).reduce((n,e)=>n+e[1].prompt+e[1].completion,0);
    if(rest>0)segments.push({value:rest,color:palette[6]});
    const donut=(rows)=>{
      const total=rows.reduce((n,s)=>n+s.value,0);
      if(!total)return 'var(--line) 0 100%';
      let offset=0;
      return 'conic-gradient('+rows.map(s=>{const start=offset;offset+=s.value/total*100;return `${s.color} ${start}% ${offset}%`;}).join(',')+')';
    };
    const modelTotal=segments.reduce((n,s)=>n+s.value,0);
    const tile=(label,value,detail)=>`<article class="usage-tile"><span>${esc(label)}</span><strong>${value}</strong>${detail?`<p>${detail}</p>`:''}</article>`;
    const quick=(label,from,to,active)=>`<button type="button" data-action="usage-range" data-from="${from}" data-to="${to}" class="${active?'active':''}">${esc(label)}</button>`;

    $('#view').innerHTML=heading(button('reload','刷新用量','','quiet'))+
      `<div class="usage-filter">
        <div class="usage-quick">${quick('全部', '', '', !range.from&&!range.to)}${quick('今天', today, today, range.from===today&&range.to===today)}${quick('近 7 天', cutoff(7), today, range.from===cutoff(7)&&range.to===today)}${quick('近 30 天', cutoff(30), today, range.from===cutoff(30)&&range.to===today)}</div>
        <label>从<input type="date" id="usage-from" value="${esc(range.from)}" max="${today}"></label>
        <label>到<input type="date" id="usage-to" value="${esc(range.to)}" max="${today}"></label>
        <button type="button" data-action="usage-apply">查询</button>
        <button type="button" data-action="usage-reset" class="quiet">重置</button>
        <span class="small muted">当前：${esc(rangeLabel)} · ${number(scopedTotals.requests)} 次请求</span>
      </div>`+
      `<div class="usage-tiles">
        ${tile('总请求',number(scopedTotals.requests),`${number(scopedTotals.failed_requests||0)} 次失败 · ${number(scopedTotals.unreported_requests||0)} 次未报告用量`)}
        ${tile('总 Token',number(scopedTotals.total_tokens),`输入 ${number(scopedTotals.prompt_tokens)} / 输出 ${number(scopedTotals.completion_tokens)}${scopedTotals.cached_tokens?` / 缓存 ${number(scopedTotals.cached_tokens)}`:''}`)}
        ${tile('已报告消耗',number(scopedTotals.credit),'只累计上游明确报告的数值')}
        ${tile('调用方',number(keys.length),'按 API 密钥统计')}
      </div>`+
      `<div class="usage-grid">
        <section class="usage-card">
          <div class="usage-card-head"><h2>模型分布</h2><span class="small muted">按 Token</span></div>
          ${segments.length?`<div class="donut-wrap"><div class="donut" style="background:${donut(segments)}"></div><table class="donut-legend"><thead><tr><th>模型</th><th>Token</th><th>占比</th></tr></thead><tbody>${models.slice(0,8).map((e,i)=>`<tr><td><span class="dot" style="background:${palette[i%palette.length]}"></span><code>${esc(e[0])}</code></td><td>${number(e[1].prompt+e[1].completion)}</td><td>${pct(e[1].prompt+e[1].completion,modelTotal)}%</td></tr>`).join('')}</tbody></table></div>`:empty('暂无模型用量','网关收到实际模型调用后写入。')}
        </section>
        <section class="usage-card">
          <div class="usage-card-head"><h2>调用方</h2><span class="small muted">按 Token</span></div>
          ${keys.length?table(['调用方','请求','Token'],[...keys].sort((a,b)=>(b.totals?.total_tokens||0)-(a.totals?.total_tokens||0)).slice(0,10).map(k=>[esc(k.name||k.masked_key||k.key_id||'未命名'),number(k.totals?.requests||0),number(k.totals?.total_tokens||0)])):empty('暂无调用方用量','按 API 密钥统计。')}
        </section>
      </div>`+
      `<section class="usage-card">
        <div class="usage-card-head"><h2>按模型明细</h2><span class="small muted">${esc(rangeLabel)}</span></div>
        ${models.length?table(['模型','请求','输入','输出','缓存','消耗'],[...models].map(e=>[`<code>${esc(e[0])}</code>`,number(e[1].requests),number(e[1].prompt),number(e[1].completion),number(e[1].cached),number(e[1].credit)])):empty('暂无明细','')}
      </section>`+
      notice(`账本开始于 ${date(data.since)}。缺失数据不会按零消耗推断。`,'');
  }

  // The request log follows the shape operators know from other gateways: one
  // wide table with every column visible, and an inline expander on the left of
  // each row. Opening a row reveals the detail in place — nothing navigates away.
  const protocolNames={chat_completions:'Chat Completions',responses:'Responses',anthropic_messages:'Anthropic Messages',gemini_generate_content:'Gemini'};
  const protocolName=value=>protocolNames[value]||value||'—';
  const statusTone=value=>['success','ok'].includes(value)?'ready':['rejected','failed','error'].includes(value)?'failed':'manual_action';
  function tokensCell(r){
    const input=r.input_tokens==null?'—':number(r.input_tokens);
    const output=r.output_tokens==null?'—':number(r.output_tokens);
    return `<span class="tok">↓ ${input}</span> <span class="tok">↑ ${output}</span>`;
  }
  function requestRow(r,i,open){
    const tone=statusTone(r.status);
    const secs=ms=>ms==null?'—':(ms/1000).toFixed(1)+'s';
    const tps=r.output_tokens!=null&&r.duration_ms>0?Math.round(r.output_tokens/(r.duration_ms/1000)*10)/10:null;
    // The log records one cached-token figure; Anthropic splits it into write and
    // read tiers, which this ledger does not separate. Showing one number is
    // honest; inventing a second would not be.
    const hit=r.cached_tokens!=null?number(r.cached_tokens):'—';
    return `<tr class="log-row" data-request-row="${i}">
      <td class="log-expander"><button type="button" data-action="request-expand" data-index="${i}" aria-expanded="${open?'true':'false'}" aria-label="展开日志详情">${open?'▾':'▸'}</button></td>
      <td><time>${esc(date(r.started_at))}</time></td>
      <td><code>${esc(r.key_name||r.key_id||'—')}</code></td>
      <td><span class="small">${esc(protocolName(r.protocol))}</span></td>
      <td><strong>${esc(r.model||'—')}</strong></td>
      <td>${secs(r.ttfb_ms)} / ${secs(r.duration_ms)}</td>
      <td>${r.input_tokens==null?'—':number(r.input_tokens)}</td>
      <td>${hit}</td>
      <td>${r.output_tokens==null?'—':number(r.output_tokens)}</td>
      <td>${tps==null?'—':tps+' tok/s'}</td>
      <td>${badge(tone,r.status||'未知')}${r.error_code?`<p class="small muted">${esc(r.error_code)}</p>`:''}</td>
      <td>${r.credit==null?'—':String(r.credit)}</td>
      <td><code class="small">${esc(r.request_id||'—')}</code></td>
    </tr>`+(open?`<tr class="log-detail-row"><td colspan="13">${requestDetailHtml(r)}</td></tr>`:'');
  }
  // Detail is rendered from the list row itself, which already carries attempts
  // and timing. The expanded panel only ever shows metadata — never message text.
  function requestDetailHtml(r){
    const k=(name,value)=>`<div><dt>${esc(name)}</dt><dd>${value??'—'}</dd></div>`;
    return `<div class="log-detail">
      <dl class="log-specs">
        ${k('请求标识',`<code>${esc(r.request_id||'—')}</code>`)}
        ${k('协议',`<code>${esc(protocolName(r.protocol))}</code>`)}
        ${k('模型',`<code>${esc(r.model||'—')}</code>`)}
        ${k('状态',badge(statusTone(r.status),r.status||'未知')+(r.http_status?' · HTTP '+esc(r.http_status):''))}
        ${k('错误码',r.error_code?`<code>${esc(r.error_code)}</code>`:'—')}
        ${k('调用密钥',esc(r.key_name||r.key_id||'—'))}
        ${k('流式',r.stream?'是':'否')}
        ${k('开始时间',esc(date(r.started_at)))}
        ${k('结束时间',r.finished_at?esc(date(r.finished_at)):'—')}
        ${k('总耗时',r.duration_ms!=null?number(r.duration_ms)+' ms':'—')}
        ${k('排队',r.queue_ms!=null?number(r.queue_ms)+' ms':'—')}
        ${k('首字节',r.ttfb_ms!=null?number(r.ttfb_ms)+' ms':'—')}
        ${k('结束原因',esc(r.finish_reason||'—'))}
        ${k('上游已启动',r.upstream_started?'是':'否')}
        ${k('尝试次数',number(r.attempt_count))}
        ${k('决策次数',number(r.decision_count))}
      </dl>
      <p class="small muted">展开只显示请求元数据；消息内容与凭据不在留存量中。</p>
    </div>`;
  }
  async function logs() {
    const data=await api('requests?limit=50&offset='+String(state.logOffset || 0));
    const rows=data.items || data.requests || data.rows;
    if(!Array.isArray(rows))throw new APIError('request_failed',503);
    state.requestRows=rows;
    state.logOpen=new Set();
    const head=['','时间','调用方','协议','模型','TTFB / total','输入','缓存命中','输出','tok/s','状态','花费','请求 ID'];
    $('#view').innerHTML=heading(button('reload','刷新记录','','quiet'))+
      (rows.length?`<div class="log-table-wrap"><table class="log-table"><thead><tr>${head.map(h=>`<th>${esc(h)}</th>`).join('')}</tr></thead><tbody>${rows.map((r,i)=>requestRow(r,i,false)).join('')}</tbody></table></div>`:empty('暂无请求记录','记录由网关真实请求生成，不包括面板的目录读取。'))+
      `<div class="pagination"><span>共 ${number(data.total)} 条</span><div class="actions">${button('logs-prev','上一页',!state.logOffset?'disabled':'')}${button('logs-next','下一页',(state.logOffset || 0)+50>=data.total?'disabled':'')}</div></div>`;
  }
  // Toggle one row in place. Re-rendering the single row keeps scroll position and
  // avoids refetching the page for what is already in the DOM.
  function toggleRequestRow(index){
    const rows=state.requestRows||[],r=rows[index];
    if(!r)return;
    state.logOpen=state.logOpen||new Set();
    const open=!state.logOpen.has(index);
    if(open)state.logOpen.add(index);else state.logOpen.delete(index);
    const current=document.querySelector(`tr[data-request-row="${index}"]`);
    if(!current)return;
    const next=document.createElement('tbody');
    next.innerHTML=requestRow(r,index,open);
    const nodes=[...next.children];
    const detailRow=current.nextElementSibling;
    if(detailRow?.classList.contains('log-detail-row'))detailRow.remove();
    if(open)nodes[1]&&current.after(nodes[1]);
    current.replaceWith(nodes[0]);
  }

  // /api/update 的信封有五种真实形态，控制台必须逐一说实话：
  //   ok=false                      → 未启用（带服务端原因）
  //   enabled=true + unavailable_reason → 不可用（私有仓库/可见性未确认）
  //   state=failed                  → 失败 + last_error
  //   update_ready                  → 可升级
  //   checked_at 非空且无更新         → 已是最新；否则未检查
  // 曾经除「enabled」外一律显示「待独立验收」，把失败与不可用都说成可交接。
  const updateStateLabels = {idle:'空闲',checking:'检查中',downloading:'下载中',handover:'切换中',failed:'失败'};
  function updateView(update) {
    const empty = {unread:true,canCheck:false,badge:'manual_action',badgeText:'状态未读取',detail:'',lines:[]};
    if(!update) return empty;
    const status = update.status || {};
    const lines = [];
    const current = (status.current && status.current !== 'unknown') ? status.current : '未知';
    lines.push('当前版本：' + current + (status.commit && status.commit !== 'unknown' ? ' · ' + String(status.commit).slice(0, 7) : ''));
    if(status.repo) lines.push('发布仓库：' + status.repo + (status.visibility ? ' · ' + ({public:'公开',private:'私有',unknown:'未确认'}[status.visibility] || status.visibility) : ''));
    if(status.latest_tag) lines.push('远端最新：' + status.latest_tag);
    if(status.asset_name) lines.push('更新包：' + status.asset_name + (status.asset_size ? ' · ' + Math.round(status.asset_size / 1048576 * 10) / 10 + ' MB' : ''));
    if(status.checked_at) lines.push('上次检查：' + status.checked_at);
    lines.push(status.signature_verification === true ? '签名校验：已启用（内置发布公钥）' : '签名校验：未内置发布公钥（构建不验签）');
    if(status.state === 'failed' && status.last_error) lines.push('失败原因：' + status.last_error);
    const base = {unread:false,lines,detail:'',canCheck:true};
    if(update.ok === false) return {...base,canCheck:false,badge:'disabled',badgeText:'未启用',detail:typeof update.message === 'string' && update.message ? update.message : '当前构建未启用热更新。'};
    if(status.enabled !== true) return {...base,canCheck:false,badge:'disabled',badgeText:'已关闭',detail:status.unavailable_reason || '热更新已在配置中关闭（config update.enabled=false）。'};
    if(status.unavailable_reason) return {...base,canCheck:false,badge:'unavailable',badgeText:'不可用',detail:status.unavailable_reason};
    if(status.state === 'failed') return {...base,badge:'failed',badgeText:'失败',detail:status.last_error || '上次更新失败，可重新检查。'};
    if(['checking','downloading','handover'].includes(status.state)) return {...base,badge:'ready',badgeText:updateStateLabels[status.state],detail:'更新任务正在进行，完成后会再次核对签名与摘要。'};
    if(status.update_ready === true) return {...base,badge:'ready',badgeText:'可升级',detail:'检测到比当前构建更新的发布；下载前会校验发布签名与 sha256 摘要。'};
    if(status.checked_at) return {...base,badge:'ready',badgeText:'已是最新',detail:'已读取远端发布，没有比当前构建更新的版本。'};
    return {...base,badge:'manual_action',badgeText:'未检查',detail:'尚未读取远端发布；点击「检查更新」后再确认是否有新版本。'};
  }
  async function settings() {
    const session=await api('session');
    let state=null,loop=null,update=null,stateError='',loopError='',updateError='';
    try{state=await api('state');}catch(e){if(e.name!=='AbortError')stateError=e.message||'未能读取';}
    try{loop=await api('features/reasoning-loop');}catch(e){if(e.name!=='AbortError')loopError=e.message||'未能读取';}
    try{update=await api('update',{allowNotOK:true});}catch(e){if(e.name!=='AbortError')updateError=e.message||'未能读取';}
    const lan=state?.lan;
    const loopOn=loop?.stop_only===true;
    const updateInfo=updateView(update);
    const updateEnabled=updateInfo.canCheck;
    const lanTitle=!lan?'状态未读取':lan.enabled?'配置已启用':'配置为仅本机';
    const lanDetail=!lan?notice('局域网状态读取失败：'+esc(stateError||'服务未返回状态')+'。界面不会把它当作已关闭。','error'):`监听 ${esc(lan.listen||'未声明')}${lan.allow_cidrs?'，允许 '+esc(lan.allow_cidrs):''}${lan.has_key?'':'；尚未创建调用密钥'}${lan.enabled?'<br>保存后需要重启服务才会改变实际监听。':''}`;
    const loopTitle=!loop?'状态未读取':loopOn?'命中即停止':'命中后自动重发';
    const loopDetail=!loop?notice('重复推理开关读取失败：'+esc(loopError||'服务未返回状态')+'。','error'):'作用于后续请求；已开始的请求沿用原语义。';
    const updateNotice=updateInfo.unread?notice('更新状态读取失败：'+esc(updateError||'服务未返回状态')+'。','error'):notice(updateInfo.detail,updateInfo.badge==='ready'?'':'warn');
    const updateUnread=updateInfo.unread;
    $('#view').innerHTML=heading()+`<div class="split"><section class="panel"><div class="panel-title"><div><h2>显示与会话</h2></div></div><div class="settings-line"><div><h3>界面主题</h3><p>在此浏览器保存偏好。</p></div>${button('theme','切换主题')}</div><div class="settings-line"><div><h3>当前会话</h3><p>有效期至 ${esc(date(session.expiresAt))}</p></div>${badge('ready','已登录')}</div><div class="settings-line"><div><h3>管理账号安全</h3><p>修改密码后其他设备需重新登录。</p></div>${button('password','修改密码')}</div></section><section class="panel"><div class="panel-title"><div><h2>局域网访问</h2></div></div><div class="settings-line"><div><h3>${lanTitle}</h3><p>${lanDetail}</p></div>${state?button('lan-settings',lan?.enabled?'调整':'启用','','quiet'):''}</div></section><section class="panel"><div class="panel-title"><div><h2>重复推理保护</h2></div></div><div class="settings-line"><div><h3>${loopTitle}</h3><p>${loopDetail}</p></div>${loop?button('loop-toggle',loopOn?'改为自动重发':'改为命中即停','','quiet'):''}</div></section><section class="panel"><div class="panel-title"><div><h2>迁移与更新</h2></div></div>${updateNotice}<p class="muted">${update?'检查与下载不会自动切换生产；正式发布前仍需在目标平台完成独立验收。':'更新读取失败时不会显示已启用或已下载。'}</p>${update?'<dl class="detail-specs update-facts">'+updateInfo.lines.map(line=>{const p=line.indexOf('：');return `<div><dt>${esc(p>0?line.slice(0,p):'状态')}</dt><dd>${esc(p>0?line.slice(p+1):line)}</dd></div>`;}).join('')+'</dl>':''}<div class="section-gap">${updateEnabled?button('update-check','检查更新','','quiet'):''}${badge(updateInfo.badge,updateInfo.badgeText)}</div></section></div>`;
  }
  // 0 在服务端是「不限」的显式语义（keylimit：>0 才生效）。直接打印
  // "0/分 · 并发 0" 会被读成「一个请求都不放行」，与真实行为相反。
  function limitSummary(limits){
    if(!limits)return '未配置（沿用服务默认）';
    const parts=[];
    if(Number(limits.requests_per_minute)>0)parts.push('每分钟 '+limits.requests_per_minute+' 次');
    if(Number(limits.max_concurrent)>0)parts.push('并发 '+limits.max_concurrent);
    if(Number(limits.queue_timeout_seconds)>0)parts.push('排队 '+limits.queue_timeout_seconds+'s');
    return parts.length?parts.join(' · '):'不限流（每次都可立即执行）';
  }
  async function keyPage() {
    const data=await api('keys');
    if(!Array.isArray(data.keys))throw new APIError('request_failed',502);
    let limits={};
    try{const live=await api('key-limits');if(live?.ok===true&&live.keys&&typeof live.keys==='object')limits=live.keys;}catch(e){if(e.name!=='AbortError')limits={};}
    const rows=data.keys.map(k=>{
      const live=limits[k.id]||null;
      const limitText=limitSummary(k.limits);
      const liveText=live?`${number(live.requests)} 次/分窗口 · 进行 ${number(live.active)} · 排队 ${number(live.queued)}`:'实时快照未读取';
      const tokens=k.total_tokens===undefined?'—':number(k.total_tokens);
      return [`<strong>${esc(k.name||'未命名')}</strong><p class="small muted">${esc(k.note||'')}</p>`,`<code>${esc(k.masked_key||'')}</code><p class="small muted">${esc((k.models||[]).length?((k.models||[]).slice(0,3).join(', '))+((k.models||[]).length>3?' 等 '+(k.models||[]).length+' 个':''):'不限模型')}</p>`,badge(k.enabled?'ready':'disabled',k.enabled?'启用':'停用'),esc(k.expires_at?date(k.expires_at):'无限制'),esc(limitText)+`<p class="small muted">${esc(liveText)}</p>`,tokens,button('key-edit','编辑',`data-id="${esc(k.id)}"`,'quiet')+button('key-toggle',k.enabled?'停用':'启用',`data-id="${esc(k.id)}" data-enabled="${k.enabled?'1':'0'}"`,'quiet')+button('key-copy','显示密钥',`data-id="${esc(k.id)}"`,'quiet')+button('key-delete','删除',`data-id="${esc(k.id)}"`,'quiet danger')];
    });
    // The endpoint is the single thing every caller has to copy correctly, so it
    // is shown once at the top of the page instead of being buried in docs.
    const base=location.origin+'/v1';
    $('#view').innerHTML=heading(button('key-new','创建密钥','','primary')+button('reload','刷新','','quiet'))+
      `<section class="endpoint-card">
        <h2>接入地址</h2>
        <p>把客户端的基础地址设为下面这一项；协议路径由客户端自己追加。支持 OpenAI、Anthropic Messages、OpenAI Responses 与 Gemini 四种协议。</p>
        <div class="endpoint-row"><code id="endpoint-base">${esc(base)}</code><button type="button" data-action="copy-endpoint" data-value="${esc(base)}">复制</button></div>
        <dl class="endpoint-paths">
          <div><dt>OpenAI / Chat Completions</dt><dd><code>${esc(base)}/chat/completions</code></dd></div>
          <div><dt>OpenAI Responses</dt><dd><code>${esc(base)}/responses</code></dd></div>
          <div><dt>Anthropic Messages</dt><dd><code>${esc(base)}/messages</code></dd></div>
          <div><dt>Gemini</dt><dd><code>${esc(base)}/beta/models/&lt;模型&gt;:generateContent</code></dd></div>
        </dl>
        <p class="small muted">鉴权用下面任意一把密钥作为 Bearer token；模型名形如 <code>doubao:1:0</code>，可在「模型目录」里查看。</p>
      </section>`+
      (rows.length?table(['名称','密钥','状态','有效期','限流','累计 Token','操作'],rows):empty('还没有 API 密钥','创建第一把密钥后才能通过 OpenAI / Anthropic / Gemini 兼容接口调用网关。'))+
      notice('密钥明文只在创建或「显示密钥」时返回一次；列表只显示掩码。停用会立即生效，删除不可恢复。','warn');
  }
  async function keyEditDialog(id) {
    const data=await api('keys');
    const key=(data.keys||[]).find(k=>k.id===id);
    if(!key)throw new APIError('request_failed',404,'密钥已不存在，请刷新列表。');
    const limits=key.limits||{};
    openDialog('编辑密钥',`<form id="key-form" class="form"><p class="muted">密钥 <code>${esc(key.masked_key||'')}</code> 的明文不会再显示。留空模型表示不限；模型目录和倍率按服务端当前可见目录读取。</p><label>名称<input name="name" value="${esc(key.name||'')}" required maxlength="80"></label><label>备注<input name="note" value="${esc(key.note||'')}" maxlength="200"></label><input type="hidden" name="models"><section class="key-model-picker-section"><h3>绑定模型</h3><div data-model-picker></div></section><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="${esc(limits.requests_per_minute??0)}"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="${esc(limits.max_concurrent??0)}"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="${esc(limits.queue_timeout_seconds??0)}"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存并核对</button></div></form>`);
    const form=$('#key-form'),picker=LoomModelPicker.mount(form,{api,esc,selected:key.models||[]});
    form.onsubmit=e=>{e.preventDefault();const f=new FormData(form);const body={id:key.id,name:String(f.get('name')||'').trim(),note:String(f.get('note')||'').trim(),models:picker.values()};const rpm=Number(f.get('requests_per_minute')||0),conc=Number(f.get('max_concurrent')||0),queue=Number(f.get('queue_timeout_seconds')||0);body.limits=(rpm||conc||queue)?{requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue}:null;mutate('key-edit','keys/update',body,async()=>{const saved=(await api('keys')).keys.find(k=>k.id===key.id);if(!saved||saved.name!==body.name||JSON.stringify(saved.models||[])!==JSON.stringify(body.models))throw new APIError('request_failed',502,'保存后读回与提交不一致。');closeDialog();toast('密钥已更新，并从服务端读回。');await reload();});};
  }
  async function keyNewDialog() {
    openDialog('创建 API 密钥',`<form id="key-form" class="form"><p class="muted">模型绑定使用服务端当前客户端目录及其真实倍率。留空表示不限模型。</p><label>名称<input name="name" required maxlength="80" autofocus></label><label>备注<input name="note" maxlength="200"></label><input type="hidden" name="models"><section class="key-model-picker-section"><h3>绑定模型</h3><div data-model-picker></div></section><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="0"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="0"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="0"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">创建</button></div></form>`);
    const form=$('#key-form'),picker=LoomModelPicker.mount(form,{api,esc,selected:[]});
    form.onsubmit=e=>{e.preventDefault();const f=new FormData(form);const body={name:String(f.get('name')||'').trim(),note:String(f.get('note')||'').trim(),models:picker.values()};const rpm=Number(f.get('requests_per_minute')||0),conc=Number(f.get('max_concurrent')||0),queue=Number(f.get('queue_timeout_seconds')||0);if(rpm||conc||queue)body.limits={requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue};mutate('key-new','keys',body,async data=>{if(!data.key)throw new APIError('request_failed',502,'服务端没有返回完整密钥。');const secret=String(data.key);$('#dialog-body').innerHTML=`<p>密钥已创建。请立即复制，关闭后无法再次查看。</p><label>密钥<textarea readonly rows="3">${esc(secret)}</textarea></label><div class="actions">${button('close','我已保存')}</div>`;const closeButton=$('#dialog [data-action=close]');if(closeButton)closeButton.onclick=()=>{closeDialog();reload().catch(()=>{});};toast('密钥已创建，请立即保存。');});};
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
            message.dataset.preflight='passed';message.textContent=`预检通过：${r.file_count??0} 个文件，${counts||'无持久化文件'}。未创建目标目录。实际复制必须在离线确认后执行。`;
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
  document.addEventListener('keydown',e=>{const row=e.target.closest('.selection-row');if(!row||!['ArrowDown','ArrowUp','Home','End'].includes(e.key))return;const rows=[...row.closest('[role=listbox]').querySelectorAll('.selection-row')],i=rows.indexOf(row),next=e.key==='Home'?0:e.key==='End'?rows.length-1:Math.max(0,Math.min(rows.length-1,i+(e.key==='ArrowDown'?1:-1)));e.preventDefault();if(row.dataset.action==='select-model-group')selectModelGroup(next,false);else selectTaskGroup(next,false);rows[next].focus();});
  const renderers={overview,providers:services,accounts,models:modelPage,tasks,schedules:tasks,runs:tasks,usage,logs,keys:keyPage,migration,settings};
  async function reload() {state.providers=null;state.pollStarted=0;await navigate();}
  async function action(element) {
    const name=element.dataset.action;
    // 组详情里的行内动作用 data-task-index 指回扁平任务数组；
    // 组级动作（select-task-group/run-task-group/task-schedule-group）用 data-index。
    const index=element.dataset.taskIndex!==undefined?Number(element.dataset.taskIndex):Number(element.dataset.index);
    if(name==='select-model-group'){selectModelGroup(index);return;}
    if(name==='select-task-group'){selectTaskGroup(index);return;}
    if(name==='model-back'||name==='task-back'){const kind=name.split('-')[0];$('#'+kind+'-browser').classList.remove('detail-open');$('#'+kind+'-browser [aria-selected=true]')?.focus();return;}
    if(name==='close'){closeDialog();return;}
    if(name==='reload'){await reload();return;}
    if(name==='theme'){toggleTheme();return;}
    if(name==='provider-models' || name==='account-models'){state.modelProvider=element.dataset.provider;state.modelAccount=element.dataset.account || '';state.modelOffset=0;location.hash='models';return;}
    if(name==='provider-accounts'){state.accountProvider=element.dataset.provider;location.hash='accounts';return;}
    if(name==='connect-account'||name==='reconnect-account'){await LoomConnections.open(element.dataset.provider||state.accountProvider,element.dataset.account||'','',element.dataset.realm||'');return;}
    if(name==='toggle-account'){
      const a=state.accountRows[index],provider=element.dataset.provider;openDialog(a.enabled?'停用账号':'启用账号',`<form id="account-toggle-form" class="form"><p>账号 <code>${esc(a.id)}</code></p><p class="muted">${a.enabled?'停用后不再用于后续目录请求与任务。正在进行的操作应在执行记录中单独核验。':'启用后该账号可再次用于目录读取和任务。'}</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认${a.enabled?'停用':'启用'}</button></div></form>`);
      $('#account-toggle-form').onsubmit=e=>{e.preventDefault();const modern=state.providers.find(p=>p.id===provider)?.catalog_scope==='account';mutate('account-toggle',modern?'workbench/account/toggle':'account/toggle',modern?{id:a.id,enabled:!a.enabled,revision:a.revision}:{uid:a.id,disabled:a.enabled},async()=>{const updated=(await accountsFor(provider)).find(x=>x.id===a.id);if(!updated||updated.enabled===a.enabled)throw new APIError('request_failed',502);closeDialog();toast('账号状态已从服务端核验。');await reload();});};return;
    }
    if(name==='model-refresh'){await loadModels(true);return;}
    if(name==='model-capability'){return;}
    if(name==='model-group-next' || name==='model-group-prev'){state.modelGroupPage=Math.max(0,state.modelGroupPage+(name==='model-group-next'?1:-1));drawModels();return;}
    if(name==='model-next' || name==='model-prev'){state.modelOffset=Math.max(0,state.modelOffset+(name==='model-next'?8:-8));await loadModels(false);return;}
    if(name==='connection-help'){openDialog('账号接入状态',notice('账号状态来自真实后端。WorkBuddy 支持扫码/短信登录与重新连接；豆包登录基于官方客户端会话。只有在服务确认需要人工操作时才会发起对应流程。','')+'<p class="muted">登录流程需要真实扫码或验证码时，界面会显示明确步骤；不会在后台静默重试已成功的操作。</p>');return;}
    if(name==='manual-task'){const t=state.tasks[index];openDialog('所需人工操作',`<h3>${esc(t.name)}</h3><p class="section-gap">${esc(t.description)}</p>${notice('请在对应官方服务完成要求的内容或操作。完成与奖励以服务端核验为准。','warn')}`);return;}
    if(name==='run-task' || name==='run-all'){confirmRun(name==='run-task'?state.tasks[index]:null);return;}
    if(name==='open-runs'){location.hash='tasks';return;}
    if(name==='run-task-group'){const g=state.taskGroups[index];if(!g)return;confirmRun(taskForGroup(g));return;}
    if(name==='task-log'){await openTaskLog(state.taskGroups[index]);return;}
    if(name==='review-submission'){openDialog('核对未确认的提交',`<form id="review-form" class="form"><p>请先检查执行记录和官方服务状态。解除保护不会重试或取消原请求。</p><label class="check"><input type="checkbox" required>我已核对上次提交，确认不会重复执行已有操作</label><div class="actions">${button('close','返回')}<button type="submit">解除提交保护</button></div></form>`);$('#review-form').onsubmit=e=>{e.preventDefault();sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();reload();};return;}
    if(name==='new-schedule'){location.hash='tasks';return;}if(name==='edit-schedule' || name==='task-schedule'){await scheduleEditor(name==='edit-schedule'?state.schedules[index]:null,name==='task-schedule'?state.tasks[index]:null);return;}
    if(name==='task-schedule-group'){const g=state.taskGroups[index];if(!g)return;await scheduleEditor(null,taskForGroup(g));return;}
    if(name==='toggle-schedule'){const s={...cleanSchedule(state.schedules[index]),enabled:!state.schedules[index].enabled};await mutate('schedule','workbench/schedule',s,async()=>{await verifySchedule(s);toast(s.enabled?'计划已启用。':'计划已停用。');await reload();});return;}
    if(name==='delete-schedule'){const s=state.schedules[index];openDialog('删除执行计划',`<form id="delete-form" class="form"><p>删除计划 <code>${esc(s.id)}</code> 后将停止后续派发。已有执行记录保留。</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="danger">删除计划</button></div></form>`);$('#delete-form').onsubmit=e=>{e.preventDefault();mutate('delete','workbench/schedule/delete',{id:s.id},async()=>{const data=await api('workbench');if(data.schedules.some(x=>x.id===s.id))throw new APIError('request_failed',502);closeDialog();toast('计划已删除，并已从服务端核验。');await reload();});};return;}
    if(name==='cancel-run'){const r=state.runs[index];await mutate('cancel-'+r.id,'workbench/cancel',{id:r.id},async data=>{toast(data.cancel_requested?'已发送取消请求，等待服务端确认。':'任务已结束或无法取消，请查看最新状态。');await reload();});return;}
    if(name==='run-detail'){const r=state.runs[index];openDialog('执行详情',`<h3>${esc(taskLabel(r.provider_id,'',r.task_key))}</h3><p class="muted">${esc(providerName(r.provider_id))} · ${esc(r.account_ids?.join('、')||'全部已启用账号')}</p><p class="small muted"><code>${esc(r.id)}</code></p><div class="section-gap">${badge(r.status)}</div>${r.status==='uncertain'?notice('上游操作结果尚未确认，请先核验服务端状态，不要重复执行。','warn'):''}${runDiscoveryIssues(r.discovery_issues)}<div class="section-gap">${r.steps?.length?(()=>{const columns=runStepColumns(r.steps),withTask=columns.length===4;return table(columns,r.steps.map(s=>runStepCells(s,withTask)));})():empty('尚无执行步骤','任务可能仍在等待派发。')}</div>`);return;}
    if(name==='copy-endpoint'){const value=element.dataset.value||'';try{await navigator.clipboard.writeText(value);toast('接入地址已复制。');}catch{const box=document.getElementById('endpoint-base');if(box){const range=document.createRange();range.selectNodeContents(box);const sel=window.getSelection();sel.removeAllRanges();sel.addRange(range);}toast('已选中地址，请按 Ctrl+C 复制。');}return;}
    if(name==='logs-prev' || name==='logs-next'){state.logOffset=Math.max(0,(state.logOffset || 0)+(name==='logs-next'?30:-30));await reload();return;}
    if(name==='request-expand'){toggleRequestRow(index);return;}
    if(name==='usage-range'){state.usageRange={from:element.dataset.from||'',to:element.dataset.to||''};await reload();return;}
    if(name==='usage-apply'){const from=document.getElementById('usage-from')?.value||'',to=document.getElementById('usage-to')?.value||'';if(from&&to&&from>to){toast('起始日期不能晚于结束日期。');return;}state.usageRange={from,to};await reload();return;}
    if(name==='usage-reset'){state.usageRange={from:'',to:''};await reload();return;}
    if(name==='lan-settings'){const st=await api('state');if(!st?.lan)throw new APIError('request_failed',502);const lan=st.lan;openDialog('局域网访问',`<form id="lan-form" class="form"><p class="muted">这里修改的是配置文件中的监听地址与允许网段；保存成功后仍需重启服务，实际监听不会立即改变。</p><label class="check"><input type="checkbox" name="enabled" ${lan.enabled?'checked':''}>启用局域网访问</label><label>监听地址<input name="listen" value="${esc(lan.listen||'')}" readonly aria-readonly="true"></label><label>允许网段<input name="allow_cidrs" value="${esc(lan.allow_cidrs||'')}" placeholder="例如 192.168.1.0/24"></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);$('#lan-form').onsubmit=e=>{e.preventDefault();const form=new FormData(e.target);const want={enabled:form.get('enabled')==='on',allow_cidrs:String(form.get('allow_cidrs')||'').trim()};mutate('lan','lan',want,async data=>{if(data.ok===false)throw new APIError('request_failed',502);const saved=(await api('state'))?.lan;if(!saved||saved.enabled!==want.enabled||String(saved.allow_cidrs||'')!==want.allow_cidrs)throw new APIError('request_failed',502);closeDialog();toast((data.message||'设置已保存。')+' 已从服务端读回配置。');await reload();});};return;}
    if(name==='loop-toggle'){const current=await api('features/reasoning-loop');if(current?.stop_only===undefined)throw new APIError('request_failed',502);const want=!current.stop_only;await mutate('loop','features/reasoning-loop',{stop_only:want},async()=>{const saved=await api('features/reasoning-loop');if(saved?.stop_only!==want)throw new APIError('request_failed',502);toast('重复推理保护已切换，并已从服务端读回。');await reload();});return;}
    if(name==='update-check'){await mutate('update-check','update/check',{},async data=>{if(data.ok===false)throw new APIError('request_failed',502,typeof data.message==='string'?data.message:'');const status=data.status||{};toast('已检查更新：'+(status.message||status.state||'状态未变化')+'。');await reload();});return;}
    if(name==='key-new'){await keyNewDialog();return;}
    if(name==='key-edit'){await keyEditDialog(element.dataset.id);return;}
    if(name==='key-copy'){const id=element.dataset.id;await mutate('key-copy','keys/copy',{id},async data=>{if(!data.key)throw new APIError('request_failed',502,'服务端没有返回密钥。');openDialog('显示密钥',`<p>此密钥只在此刻显示。关闭后可再次点击「显示密钥」。</p><label>密钥<textarea readonly rows="3">${esc(String(data.key))}</textarea></label><div class="actions">${button('close','关闭')}</div>`);});return;}
    if(name==='key-toggle'){const id=element.dataset.id,want=element.dataset.enabled!=='1';await mutate('key-toggle','keys/update',{id,enabled:want},async()=>{const saved=(await api('keys')).keys.find(k=>k.id===id);if(!saved||saved.enabled!==want)throw new APIError('request_failed',502);toast(want?'密钥已启用。':'密钥已停用。');await reload();});return;}
    if(name==='key-delete'){const id=element.dataset.id;openDialog('删除 API 密钥',`<form id="key-delete-form" class="form"><p>删除后使用该密钥的客户端会立即收到鉴权失败，且不可恢复。</p><label class="check"><input type="checkbox" required>我确认要删除这把密钥</label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="danger">删除</button></div></form>`);$('#key-delete-form').onsubmit=e=>{e.preventDefault();mutate('key-delete','keys/delete',{id},async()=>{if((await api('keys')).keys.some(k=>k.id===id))throw new APIError('request_failed',502);closeDialog();toast('密钥已删除，并从服务端读回。');await reload();});};return;}
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

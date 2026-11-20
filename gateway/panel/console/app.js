'use strict';
(() => {
  const $ = (selector, root = document) => root.querySelector(selector);
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const date = value => value && !String(value).startsWith('0001-') ? new Date(typeof value === 'number' ? value * 1000 : value).toLocaleString('zh-CN', {hour12:false}) : '—';
  // Go 的 time.Time 零值会序列化成 0001-01-01；只有可解析的时间才是
  // 真实构建、发布或检查时间，不能把零值/Invalid Date展示给运维。
  const validUpdateTime = value => {
    if(typeof value !== 'string' || !value.trim() || value.startsWith('0001-')) return false;
    const parsed = Date.parse(value);
    return Number.isFinite(parsed);
  };
  // 旧版任务日志的顶栏口径：「最近一次 11-09 22:26」，不带年份与秒。
  const shortTime = value => {
    if(!value||String(value).startsWith('0001-')) return '—';
    const d = new Date(typeof value === 'number' ? value * 1000 : value);
    if(isNaN(d.getTime())) return '—';
    const p = n => String(n).padStart(2,'0');
    return `${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  };
  const number = value => typeof value === 'number' ? value.toLocaleString('zh-CN') : '—';
  const state = {route:'', version:0, controller:null, providers:null, models:[], catalogs:[], modelProvider:'', modelAccount:'', modelOffset:0, modelGroupPage:0, modelChannel:'', search:'', capability:'', taskProvider:'', tasks:[], usageRange:null, taskCache:null, schedules:[], runs:[], keys:[], pending:new Set(), poll:null, taskLogGeneration:0};
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
    const sequenceLabels={'growth-sequence':'成长中心有序流水线','checkin-balance-sequence':'签到与余额刷新'};
    const sequenceName=sequenceLabels[key];
    if(sequenceName)return sequenceName;
    const names=state.taskNames,rk=realm||'';
    const known=names?.get(provider+'\u0000'+rk+'\u0000'+key)||names?.get(provider+'\u0000\u0000'+key)||state.tasks.find(t=>t.provider_id===provider&&t.key===key)?.name;
    if(known)return known;
    const legacy={checkin:'每日签到',balance:'余额刷新',travel:'Buddy 旅行',keepalive:'账号会话保活',growth:'成长任务',redemption:'连登奖励兑换',redeem:'连登奖励兑换',makeup:'日历补登',resources:'成长资源状态',compensation:'补偿领取',lottery:'盲盒抽奖',buddy_box:'Buddy 盲盒'};
    return legacy[key]||key;
  }
  const routes = {overview:['','工作台','先处理需要关注的事项。'], providers:['CONNECTIONS','服务接入','按服务查看已接入账号与真实可用能力。'], accounts:['IDENTITIES','账号','集中查看服务账号的启用状态、会话有效期和目录入口。'], models:['MODEL LIBRARY','模型目录','从上游获取可选模型。能力与可用范围以当前账号的响应为准。'], tasks:['AUTOMATION','任务','按分区查看任务、定时与运行状态。'], runs:['ACTIVITY','执行记录','查看每次派发、执行与服务端复核的结果。'], usage:['OBSERVABILITY','用量','查看网关累计请求与上游报告的用量。'], logs:['REQUEST HISTORY','请求记录','按请求查看模型、状态和耗时，不展示消息内容。'], keys:['API KEYS','API 密钥','用于调用模型接口的密钥；与面板登录账号无关。'], migration:['MIGRATION','迁移','只读核对迁移标记与工具状态；导入需离线确认。'], settings:['PREFERENCES','工作区设置','管理显示偏好、会话与迁移准备。']};
  const labels = {ready:'目录已同步',stale:'旧缓存',unavailable:'暂不可用',disabled:'已停用',queued:'等待执行',running:'执行中',succeeded:'已核验完成',failed:'失败',canceled:'已取消',uncertain:'待复核',skipped:'已跳过',manual_action:'需要人工',discover:'发现',inspect:'检查',execute:'执行',claim:'领取',verify:'复核'};
  const errorLabels = {login_required:'登录已失效，请在对应服务重新登录。',doubao_no_account:'尚未添加豆包账号，请导入 Cookie。',doubao_session_invalid:'豆包 Cookie 无效或已过期，请重新导入。',doubao_empty_answer:'上游返回了空回答，请重试。',access_token_expired:'访问令牌已过期，请重新导入 AutoClaw 凭证。',autoclaw_no_account:'尚未添加 AutoClaw 账号。',autoclaw_session_invalid:'AutoClaw 凭证无效，请重新导入。',autoclaw_empty_answer:'上游返回了空回答，请重试。',accounts_unavailable:'账号存储读取失败，请检查服务状态。',catalog_timeout:'目录读取超时，请稍后重试。',migration_timeout:'迁移预检超时：旧工作区可能过大或磁盘较慢，未写入任何文件；可改为在命令行离线执行预检。',catalog_failed:'目录读取失败，请稍后重试。',catalog_unavailable:'上游目录暂不可用。',account_not_found:'账号已不存在，请刷新账号列表。',schedule_not_saved:'计划未保存，请检查时间、时区和任务范围。',run_rejected:'执行请求未被接受，可能已有任务正在运行。',authentication_busy:'该服务还有一个登录窗口未结束，请先完成或取消它，再添加下一个账号。',authentication_request_invalid:'登录请求无效，请重新选择服务与登录方式。',authentication_not_found:'该登录流程已不存在，请重新发起。',authentication_storage_unavailable:'登录状态存储不可用，请检查服务状态。',operation_unconfirmed:'操作结果未确认，请核验官方界面后取消或重新接入。',account_save_failed:'账号保存失败，请检查服务状态。',provider_vault_unavailable:'账号库不可用，登录结果未能保存。',unauthorized:'管理会话已过期，请重新登录。'};
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
      const value=target.dataset.providerChoice;
      change(value);
      const refresh=reload(),generation=state.version;
      await refresh;
      if(generation===state.version)[...document.querySelectorAll('#'+id+' button')].find(b=>b.dataset.providerChoice===value)?.focus({preventScroll:true});
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
  function requestTimeout(path){return path==='connections/start'||path==='connections/step'?40000:path==='migration/preflight'?120000:18000;}
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
  async function providers(refresh=false) {
    if(refresh||!state.providers||state.providersDirty){
      const generation=state.version;
      const data=await api('workbench/providers');
      if(generation!==state.version)return;
      if(!Array.isArray(data.providers))throw new APIError('request_failed',502);
      state.providers=data.providers;state.providersDirty=false;
    }
    return state.providers;
  }
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
  async function accountsFor(provider,options) {
    const p=state.providers.find(p=>p.id===provider);
    if(p?.catalog_scope==='account') return (await api('workbench/accounts',options)).accounts.filter(a=>a.provider===provider).map(a=>({id:a.id,nickname:a.nickname||'',mobile:a.mobile||'',botId:a.bot_id||'',verifiedAt:a.verified_at||null,enabled:a.enabled,expires_at:a.expires_at,revision:a.revision}));
    const data=await api('state',options);
    return (data.accounts || []).map(a=>({...a,realm:a.realm||(/trae/i.test(a.domain||'')?'trae':/workbuddy\.ai/i.test(a.domain||'')?'global':'cn')})).filter(a=>provider==='trae'?a.realm==='trae':a.realm==='cn'||a.realm==='global').map(authSummaryRow);
  }
  function fail(e) {
    if(e.name==='AbortError')return;
    const view=$('#view'),loading=view.querySelector('[data-route-loading]');if(loading)loading.remove();
    const content=view.querySelector('[data-view-content]');if(content){content.inert=false;content.removeAttribute('aria-hidden');}
    const root=content||view,headingNode=root.querySelector('.page-heading');
    const message=notice(e instanceof APIError?e.message:'无法连接工作区，请检查网络或服务状态。','error')+button('reload','重新读取');
    if(headingNode){let box=root.querySelector('[data-route-error]');if(!box){box=document.createElement('div');box.dataset.routeError='1';headingNode.after(box);}box.innerHTML=message;}
    else root.insertAdjacentHTML('afterbegin',heading()+message);
    view.dataset.loadState='failed';
  }
  function openDialog(title, html, style='') { state.taskLogGeneration++; $('#dialog').className=style; delete $('#dialog').dataset.taskLog; $('#dialog-title').textContent=title; $('#dialog-body').innerHTML=html; if(!$('#dialog').open) $('#dialog').showModal(); }
  function closeDialog() { state.taskLogGeneration++; $('#dialog').close();$('#dialog').className='';delete $('#dialog').dataset.taskLog;$('#dialog-body').replaceChildren(); }
  function ownsDialog(node) {return Boolean(node?.isConnected&&$('#dialog').open&&$('#dialog-body').contains(node));}
  async function readDialog(title,load,render){
    openDialog(title,`<div data-dialog-read><p role="status">正在读取…</p><div class="actions">${button('close','取消')}</div></div>`);
    const marker=$('[data-dialog-read]'),generation=state.version,owner=state.taskLogGeneration;
    const current=()=>generation===state.version&&owner===state.taskLogGeneration&&ownsDialog(marker);
    try{
      const data=await load();
      if(current())render(data);
    }catch(e){
      if(!current()||e.name==='AbortError')return;
      marker.innerHTML=notice(e instanceof APIError?e.message:'读取失败，请关闭后重新打开。','error')+button('close','关闭');
    }
  }
  async function mutate(key,path,body,after,sourceControl,{allowNotOK=false,isCurrent=()=>true}={}) {
    if(state.pending.has(key))return;
    state.pending.add(key);
    const dialog=$('#dialog'), owner=state.taskLogGeneration;
    const formMessage=dialog.open&&!sourceControl?$('#form-message',dialog):null;
    const controls=dialog.open?[...dialog.querySelectorAll('button[type=submit]:not(:disabled)')]:[];
    controls.forEach(control=>control.disabled=true);
    sourceControl?.setAttribute('aria-busy','true');
    try{
      const data=await api(path,{body,signal:null,allowNotOK});
      await after(data);
    }catch(e){
      if(!isCurrent())return;
      const rejected=e instanceof APIError&&e.status<500&&e.status!==408;
      if(key==='run'&&rejected)sessionStorage.removeItem('loomgate.unconfirmedRun');
      if(key==='run')paintTaskGuard();
      const message=rejected?e.message:'请求结果尚未确认，请先核对服务端状态，避免重复提交。';
      if(formMessage?.isConnected&&dialog.open&&owner===state.taskLogGeneration){formMessage.textContent=message;formMessage.hidden=false;}
      else toast(message);
    }finally{
      state.pending.delete(key);
      controls.forEach(control=>control.disabled=false);
      sourceControl?.removeAttribute('aria-busy');
    }
  }
  function begin() { state.controller?.abort(); clearTimeout(state.poll); state.controller=new AbortController(); state.version++; return state.version; }
  function showRouteLoading() {
    const view=$('#view'),old=view.querySelector('[data-view-content]');
    if(old){old.inert=true;old.setAttribute('aria-hidden','true');}
    let loading=view.querySelector('[data-route-loading]');
    if(!loading){loading=document.createElement('div');loading.dataset.routeLoading='1';loading.className='loading';loading.innerHTML='<p>正在读取工作区…</p><div class="progress"></div>';view.append(loading);}
    loading.hidden=false;
  }
  function finishRouteRender(generation){
    if(generation!==state.version)return;
    const view=$('#view'),loading=view.querySelector('[data-route-loading]');if(loading)loading.remove();
    const content=view.querySelector('[data-view-content]');if(content){content.inert=false;content.removeAttribute('aria-hidden');}
    view.dataset.loadState='ready';
  }
  async function navigate() {
    const route=location.hash.slice(1).split('?')[0] || 'overview';
    state.route=Object.hasOwn(routes,route)?route:'overview';
    const generation=begin();closeDialog();closeNavigation();
    document.querySelectorAll('#navigation a').forEach(a=>{if(a.hash==='#'+state.route)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');});
    document.title=routes[state.route][1]+' · LoomGate';
    $('#current-page').textContent=routes[state.route][1];
    $('#view').innerHTML=`<div data-view-content></div>`;
    showRouteLoading();
    await renderCurrent(generation);
  }

  // 首页按旧版结构：一排指标卡 + 「账号池 / 积分」两块面板。
  // 不列「需要关注 / 最近执行」那种时间线——运维打开面板第一眼要看的是
  // 「多少个号可用、还剩多少积分、有没有冷却或停用」，而不是一串任务标题。
  const num = v => typeof v === 'number' ? v : (typeof v === 'string' && v.trim() !== '' && !isNaN(Number(v)) ? Number(v) : null);
  function metricCard(label, value, unit, sub, tone = '') {
    return `<div class="ov-metric"><div class="ov-k">${esc(label)}</div>` +
      `<div class="ov-v"><b>${esc(value)}</b>${unit ? `<small>${esc(unit)}</small>` : ''}</div>` +
      (sub ? `<div class="ov-s ${tone}">${esc(sub)}</div>` : '') + '</div>';
  }
  // 分域明细：cn / global / trae 固定顺序，只显示后端确实给了的域。
  function realmRows(totals, render) {
    const order = [['cn', '国内区'], ['global', '国际区'], ['trae', 'Trae']];
    const rows = order.filter(([id]) => totals && totals[id] !== undefined)
      .map(([id, name]) => render(id, name, totals[id]));
    return rows.length ? `<div class="ov-realms">${rows.join('')}</div>` : '';
  }
  async function overview() {
    const generation=state.version;
    const [work, snap] = await Promise.allSettled([api('workbench'), api('state')]);
    if(generation!==state.version)return;
    const schedules = work.status === 'fulfilled' ? (work.value.schedules || []) : [];
    const rows = work.status === 'fulfilled' ? (work.value.runs || []) : [];
    state.runs = rows;
    const st = snap.status === 'fulfilled' ? snap.value : null;
    // 池数据缺失时不能显示成 0/0「全部可用」——那读起来像一切正常，实际是还不知道。
    const pool = st && st.pool ? st.pool : {};
    const totalActive = state.providers.reduce((n, p) => n + (p.accounts.total || 0), 0);
    const enabledActive = state.providers.reduce((n, p) => n + (p.accounts.enabled || 0), 0);
    const total = num(pool.total), healthy = num(pool.healthy);
    const cooling = num(pool.cooling) || 0, disabled = num(pool.disabled) || 0;
    const exhausted = num(pool.creditsExhausted) || 0;
    const poolSub = total === null ? (st ? '正在读取…' : '未读取')
      : total === 0 ? '池中没有账号'
      : healthy === total ? '全部可用'
      : `${healthy} / ${total} 可用`;
    const poolTone = total === null ? '' : (total > 0 && healthy === total ? 'ok' : (healthy ? 'warn' : 'bad'));
    const running = rows.filter(r => r.status === 'running' || r.status === 'queued').length;
    const unconfirmed = rows.filter(r => r.status === 'uncertain').length;
    const enabledPlans = schedules.filter(s => s.enabled).length;
    // 积分合计：直接读后端已经汇总好的 creditTotals，不在前端重算。
    const credit = st && st.creditTotals ? st.creditTotals : null;
    const remain = credit ? num(credit.remain) : null;
    const size = credit ? num(credit.size) : null;
    const expiring = credit ? num(credit.expiring) : null;
    const creditAccounts = credit ? num(credit.accounts) : null;
    const creditOk = credit ? num(credit.ok) : null;
    // 过期窗口是后端配置的（pool.expiring_soon，默认 7 天），文案跟着它走，
    // 否则会出现「面板说 3 天、网关其实按 7 天加权」的错位。
    const expiryHours = credit ? num(credit.expiring_window_hours) : null;
    const expiryLabel = expiryHours && expiryHours % 24 === 0 ? `${expiryHours / 24} 天内过期` : '快过期积分';
    const creditSub = !st ? '未读取'
      : st.creditPending && remain === null ? '查询中'
      : st.creditError && remain === null ? '查询失败'
      : remain === null ? '暂无数据'
      : (size && size > 0 ? `已用 ${Math.round((size - remain) / size * 100)}%` : '');
    const pct = size && size > 0 && remain !== null ? Math.max(0, Math.min(100, Math.round(remain / size * 100))) : null;
    const metrics = [
      metricCard('已启用账号', String(enabledActive), `/ ${totalActive}`, state.providers.length ? `${state.providers.length} 个服务` : ''),
      metricCard('账号池', total === null ? '—' : String(healthy === null ? 0 : healthy), total === null ? '' : `/ ${total}`, poolSub, poolTone),
      metricCard('剩余积分', remain === null ? '—' : remain.toLocaleString('zh-CN'), size ? `/ ${size.toLocaleString('zh-CN')}` : '', creditSub),
      metricCard('启用计划', String(enabledPlans), `/ ${schedules.length}`, running ? `${running} 个正在执行` : (unconfirmed ? `${unconfirmed} 个待复核` : ''), unconfirmed ? 'warn' : ''),
    ].join('');
    const poolPanel = total === null
      ? `<div class="ov-empty">${st ? '正在读取账号池…' : '未读取到账号池状态'}</div>`
      : `<div class="ov-big"><span>${healthy === null ? 0 : healthy}</span><small>/ ${total} 健康</small></div>
         <div class="ov-bar"><i class="a" style="width:${total ? Math.round((healthy || 0) / total * 100) : 0}%"></i></div>
         <div class="ov-cells">
           <div><span>池内总数</span><b>${total}</b></div>
           <div><span>熔断中</span><b class="${cooling ? 'warn' : ''}">${cooling}</b></div>
           <div><span>已停用</span><b class="${disabled ? 'bad' : ''}">${disabled}</b></div>
           <div><span>余额不足</span><b class="${exhausted ? 'warn' : ''}">${exhausted}</b></div>
         </div>
         ${realmRows(pool.realmTotals, (id, name, v) => {
           const t = num(v.total), h = num(v.healthy);
           return `<div class="ov-realm"><span>${esc(name)}</span><b>${h === null ? '—' : h}${t === null ? '' : ` / ${t}`}</b></div>`;
         })}`;
    const creditPanel = remain === null
      ? `<div class="ov-empty">${esc(creditSub || '暂无积分数据')}</div>`
      : `<div class="ov-big"><span>${remain.toLocaleString('zh-CN')}</span><small>剩余</small></div>
         ${pct === null ? '' : `<div class="ov-bar"><i class="a" style="width:${pct}%"></i></div>`}
         <div class="ov-cells">
           <div><span>总量</span><b>${size === null ? '—' : size.toLocaleString('zh-CN')}</b></div>
           <div><span>已用</span><b>${size === null ? '—' : (size - remain).toLocaleString('zh-CN')}</b></div>
           <div><span>${expiryLabel}</span><b class="${expiring ? 'warn' : ''}">${expiring === null ? '—' : expiring.toLocaleString('zh-CN')}</b></div>
           <div><span>取到积分</span><b>${creditOk === null ? '—' : `${creditOk} / ${creditAccounts === null ? '—' : creditAccounts}`}</b></div>
         </div>`;
    const notes = [];
    if (unconfirmed) notes.push(notice(`有 ${unconfirmed} 次执行结果未确认。请先核验官方状态，不要直接重复提交。`, 'warn'));
    if (st && st.creditError && remain === null) notes.push(notice('积分数据读取失败：' + String(st.creditError), 'warn'));
    if (st && st.pendingRestart) notes.push(notice('有账号变更尚未生效，重启网关后生效。', 'warn'));
    $('#view').innerHTML = heading(button('reload', '刷新状态', '', 'quiet')) +
      notes.join('') + `<div data-workspace-overview><div class="ov-metrics">${metrics}</div>` +
      `<div class="ov-pairs">
         <section class="ov-panel"><div class="ov-ph"><h2>账号池</h2><span>按区域</span></div>${poolPanel}</section>
         <section class="ov-panel"><div class="ov-ph"><h2>积分</h2><span>全部账号合计</span></div>${creditPanel}</section>
       </div>` +
      `<div class="ov-links"><a href="#tasks">任务与定时 →</a><a href="#accounts">账号管理 →</a><a href="#logs">请求记录 →</a></div></div>`;
  }
  async function services() {
    $('#view').innerHTML=heading(button('reload','同步服务','','quiet'))+`<div class="service-list">${state.providers.map(p=>`<article class="service-entry" data-provider="${esc(p.id)}"><div><h2>${esc(p.name)}</h2><p>${p.catalog_scope==='account'?'目录随账号变化':'服务级模型目录'}</p></div><div><dl><div><dt>已连接账号</dt><dd>${p.accounts.total}</dd></div><div><dt>已启用</dt><dd>${p.accounts.enabled}</dd></div></dl><p>${p.inference_available?'消息推理已接入。请求仍受账号余额和上游可用性限制。':'模型目录可读取；此服务的消息转发尚未接通。'}</p>${p.error_code?notice(errorLabels[p.error_code]||'服务状态读取失败','error'):''}</div><div class="actions">${button('provider-models','查看模型',`data-provider="${esc(p.id)}"`,'quiet')}${button('provider-accounts','查看账号',`data-provider="${esc(p.id)}"`,'quiet')}</div></article>`).join('')}</div>`;
  }

  function installViewContent(markup){
    const content=document.createElement('div');
    content.dataset.viewContent='1';content.innerHTML=markup;
    $('#view').replaceChildren(content);
    return content;
  }

  function accountStatus(a){
    const pool=a.pool;
    let status=badge(a.enabled?'ready':'disabled',a.enabled?'已启用':'已停用');
    if(a.parsable===false)status=badge('failed','凭证无法解析')+`<p>${esc(credentialWarning(a))}</p>`;
    else if(a.enabled&&pool){
      if(pool.disabled)status=badge('failed','池内不可用')+`<p>${esc(pool.disabledReason||'需核对账号状态')}</p>`;
      else if(pool.cooling)status=badge('stale','账号级限流')+`<p>${esc(pool.reason||'等待恢复')}${pool.until?' · '+esc(date(pool.until)):''}</p>`;
      else if(pool.inPool)status=badge('ready','可用');
      else status=badge('manual_action','未加载');
    }
    return status+limitNotices(pool);
  }
  async function accounts() {
    const selected=state.accountProvider || state.providers[0]?.id || '';
    state.accountProvider=selected;
    const generation=state.version;
    installViewContent(heading(button('connect-account','添加账号','','primary'))+
      `<div class="context-bar scope-toolbar">${providerFilter('account-provider',selected)}<label class="search"><span class="sr-only">搜索昵称或账号标识</span><input id="account-search" placeholder="搜索昵称或账号标识"></label><div id="account-realm" class="realm-filter" role="group" aria-label="区域筛选"></div></div><div id="account-list" class="loading">正在读取账号…</div>`);
    bindProviderFilter('account-provider',value=>{state.accountProvider=value;state.accountRealm='';});
    const rows=await accountsFor(selected);if(generation!==state.version)return;state.accountRows=rows;const clientStates=new Map();
    // 区域筛选只在 WorkBuddy（有 CN/Global 两个独立站点）显示；Trae 与 AutoClaw 各自
    // 只有一个域，出现 CN/Global 只会误导。
    const realmBar=$('#account-realm');
    const drawRealms=()=>{
      if(selected!=='workbuddy'){realmBar.innerHTML='';realmBar.hidden=true;return;}
      realmBar.hidden=false;
      const counts={cn:rows.filter(a=>a.realm==='cn').length,global:rows.filter(a=>a.realm==='global').length};
      realmBar.innerHTML=[['','全区域',rows.length],['cn','CN',counts.cn],['global','Global',counts.global]]
        .map(([id,label,n])=>`<button type="button" data-realm-choice="${id}" aria-pressed="${(state.accountRealm||'')===id}">${label}<small>${n}</small></button>`).join('');
      realmBar.onclick=e=>{
        const t=e.target.closest('[data-realm-choice]');if(!t||t.getAttribute('aria-pressed')==='true')return;
        state.accountRealm=t.dataset.realmChoice;drawRealms();draw();
      };
    };
    const draw=()=>{
      if(generation!==state.version)return;
      const query=$('#account-search').value.toLowerCase();
      const realm=selected==='workbuddy'?(state.accountRealm||''):'';
      const list=rows.filter(a=>(!realm||a.realm===realm)&&(a.id+' '+(a.nickname||'')+' '+(a.mobile||'')+' '+(a.enterpriseId||'')).toLowerCase().includes(query));
      $('#account-list').className='account-directory';
      $('#account-list').innerHTML=list.length?table(['账号','状态与限流','积分与过期','会话与连接','操作'],list.map(a=>{
        const credit=a.credits,connection=clientStates.get(a.id);
        const status=`<div data-account-status>${accountStatus(a)}</div>`;
        const balance=typeof credit?.remain==='number'
          ? `${number(credit.remain)}${credit.expiring>0?`<p class="credit-expiring" data-expiring="${esc(credit.expiring)}">其中 ${number(credit.expiring)} 即将过期${credit.expiry?' · '+esc(date(credit.expiry)):''}</p>`:''}`
          : credit?.ok===false?'<span class="muted">读取失败</span>':'<span class="muted">未读取</span>';
        const sessionText=connection?esc(LoomConnections.clientLabels[connection.state]||'待核验')+(connection.verified_at?`<p>最近核验 ${esc(date(connection.verified_at))}</p>`:''):selected==='autoclaw'?'<p>纯 HTTP 模型调用，不创建会话，不依赖任何客户端</p>':selected==='doubao'?(a.bot_id?'<p>已绑定一个固定会话，不依赖客户端</p>':'<p>首次调用时会创建一个固定会话</p>'):`${esc(date(a.expires_at))}${a.expires_at&&a.expires_at*1000<Date.now()?'<p>会话已过期</p>':''}`;
        return [`<strong class="account-name">${esc(a.nickname||a.mobile||'未提供昵称')}</strong>${a.nickname&&a.mobile?`<p class="account-mobile">${esc(a.mobile)}</p>`:''}<p>${esc({cn:'WorkBuddy 国内版',global:'WorkBuddy 国际版',trae:'Trae'}[a.realm]||providerName(selected))}</p><details class="account-id"><summary>账号标识</summary><code>${esc(a.id)}</code>${a.enterpriseId?`<p class="muted small">企业：<code>${esc(a.enterpriseId)}</code></p>`:''}${a.botId?`<p class="muted small">官方 Bot：<code>${esc(a.botId)}</code></p>`:''}${a.verifiedAt?`<p class="muted small">核验于 ${esc(date(a.verifiedAt))}</p>`:''}</details>`,status,balance,sessionText,`<div class="actions">${button('account-models','查看目录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}"`,'compact')}${button('toggle-account',a.enabled?'停用':'启用',`data-provider="${esc(selected)}" data-index="${rows.indexOf(a)}"`,'compact')}${button('reconnect-account','重新登录',`data-provider="${esc(selected)}" data-account="${esc(a.id)}" data-realm="${esc(a.realm||'')}"`,'quiet compact')}</div>`];
      })):empty(query?'没有匹配的账号':realm?'该区域下没有账号':'尚未接入账号',query?'试试其他昵称或账号标识。':(realm?'可切换区域查看全部账号。':'接入账号后在这里查看状态、限流与积分过期。'));
    };
    drawRealms();draw();
    $('#account-search').addEventListener('input',draw);
  }
  // 模型限流与积分过期必须常显、用高亮色，不能折叠进 details 或只有 hover 才
  // 能看到——用户明确要求「看到了，还在，不要自动隐藏」「还是用高亮色显示」。
  function limitNotices(pool){
    if(!pool?.rateLimitedModels?.length)return '';
    return `<div class="limit-list">${pool.rateLimitedModels.map(m=>{
      const unavailable=m.kind==='unavailable';
      const when=m.resetAt||m.until;
      const label=unavailable?'上游已无此模型':when?'解除 '+esc(date(when)):'等待上游恢复时间';
      return `<p class="model-limit ${unavailable?'unavailable':''}" data-model-limit="${esc(m.model)}"><span class="limit-model">模型限流 ${esc(m.model)}</span><span class="limit-when">${label}</span>${m.reason?`<span class="limit-reason" title="${esc(m.reason)}">${esc(limitShortReason(m.reason))}</span>`:''}</p>`;
    }).join('')}</div>`;
  }
  function limitShortReason(reason){
    if(!reason)return '';
    if(reason.includes('6004'))return '6004';
    if(reason.includes('429'))return '429';
    if(reason.includes('11102'))return '11102';
    return '';
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
  // /api/models 是全部 provider 的聚合目录（cn:/global:/trae:/autoclaw:/doubao:…）。
  // 模型页是「按服务」展示的：必须按命名空间挑出属于本服务的行，否则某个
  // 推理 provider 注册后，它的命名空间会混进 WorkBuddy 的目录，用户选中的模型
  // 实际归另一个服务。这里不用「非 trae 即 WorkBuddy」的二选一。
  const providerModelScopes = {workbuddy:['cn:','global:'],trae:['trae:'],autoclaw:['autoclaw:'],doubao:['doubao:']};
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
    $('#model-list').innerHTML=`<div class="browser-workspace" id="model-browser"><div class="selection-pane"><p class="selection-caption">${groups.length} 个模型${groups.length!==all.length?'（'+all.length+' 个账号目录条目）':''}${p.inference_available?'':'，当前仅目录'}${pages>1?` · 第 ${state.modelGroupPage+1}/${pages} 页`:''}</p>${channelChips}<div role="listbox" aria-label="模型列表">${pageGroups.map((g,i)=>{const m=g.model;const capability=['官方工作工具','API 工具调用','图像输入','推理'].filter((label,k)=>m[['native_tools','supports_tool_call','supports_images','supports_reasoning'][k]]===true).join(' · ');const credits=m.credits?`<span class="model-rate ${/^0(\.0+)?x?$|免费/.test(String(m.credits))?'free':'low'}">${esc(String(m.credits))}</span>`:'<span class="model-rate unknown">倍率未知</span>';
      return `<button class="selection-row" role="option" aria-selected="false" data-action="select-model-group" data-index="${i}" data-model-id="${esc(m.id)}" data-account="${esc(g.accounts[0]||'')}"><span><strong>${esc(g.name)}</strong>${credits}<small>${esc([g.channels.map(c=>channelName(c)).join('/'),capability,g.accounts.length>1?g.accounts.length+' 个账号可见':''].filter(Boolean).join(' · '))}</small></span><span class="selection-arrow" aria-hidden="true">›</span></button>`;}).join('')}</div></div><section class="detail-pane" id="model-detail" tabindex="-1" aria-label="模型详情"></section></div><div id="model-group-pagination" class="pagination">${pages>1?`<span>第 ${state.modelGroupPage+1} / ${pages} 页</span><div class="actions">${button('model-group-prev','上一页',state.modelGroupPage===0?'disabled':'')}${button('model-group-next','下一页',state.modelGroupPage>=pages-1?'disabled':'')}</div>`:''}</div>`;
    selectModelGroup(selected,false);
  }
  function channelName(id){return ({cn:'国内版',global:'国际版',trae:'Trae',autoclaw:'AutoClaw',doubao:'豆包工作'})[id]||id;}
  function selectModelGroup(index,open=true) {
    const g=state.modelGroups?.[index];
    if(!g)return;
    state.modelGroupKey=g.key;state.modelGroupIndex=index;
    document.querySelectorAll('[data-action=select-model-group]').forEach((b,i)=>b.setAttribute('aria-selected',String(i===index)));
    const m=g.model,p=state.providers.find(p=>p.id===state.modelProvider),callable=m.inference_available??p.inference_available;
    const entries=g.entries;
    $('#model-detail').innerHTML=`${button('model-back','返回模型列表','','detail-back')}<div class="detail-subtitle">${esc(g.channels.map(c=>channelName(c)).join(' / ')||p.name)}${g.accounts.length?' · '+g.accounts.length+' 个账号可见':''}</div><h2 class="detail-title">${esc(g.name)}</h2><p class="detail-body">${callable?(p.catalog_scope==='account'?(m.supports_multi_turn?'支持网关普通文本调用及多轮上下文；文件和调用方工具仍受协议限制。实际可用性以当前账号为准。':'支持网关普通文本调用；多轮能力未在此目录声明，以实际请求结果为准。'):'可通过网关调用，实际请求受上游与账号状态约束。'):'此模型目前仅提供目录，尚不能通过网关 API 调用。下列能力与档位均按上游声明展示。'}</p>${g.stale?notice('部分账号显示的是旧缓存，最新刷新未成功。请核对账号目录状态。','warn'):''}<dl class="detail-specs">${callable&&entries.some(e=>state.catalogs.find(c=>c.account_id===e.account_id)?.streaming_mode==='incremental')?'<div><dt>文本流</dt><dd>支持即时输出；上游重写已发内容时明确中止，未完成的回答不可视为成功。</dd></div>':''}<div><dt>计费倍率</dt><dd class="value-large">${esc(m.credits||'上游未公布')}</dd></div><div><dt>上下文窗口</dt><dd class="value-large">${m.context_length?number(m.context_length):'未声明'}</dd></div><div><dt>推理档位</dt><dd>${esc((m.reasoning_efforts||[]).map(x=>x.name||x.level).join(' / ')||'上游未声明')}</dd></div>${[['官方工作工具','native_tools'],['API 工具调用','supports_tool_call'],['图像输入','supports_images'],['推理','supports_reasoning']].filter(([,k])=>m[k]===true).map(([label])=>`<div><dt>${label}</dt><dd>上游已声明支持</dd></div>`).join('')}${m.tiers?.length?`<div><dt>档位说明</dt><dd>${esc(m.tiers.join(' / '))}</dd></div>`:''}</dl>${entries.length>1?`<details class="quiet-detail" open><summary> ${entries.length} 个账号来源</summary><div class="model-source-list">${entries.map(e=>`<div class="model-source-row"><div><code>${esc(e.account_id||'服务目录')}</code><p class="small muted">${esc(channelName(e.id.includes(':')?e.id.slice(0,e.id.indexOf(':')):''))} · ${esc(e.catalog_status==='stale'?'旧缓存':'已同步')}</p></div></div>`).join('')}</div></details>`:''}<details class="quiet-detail"><summary>查看模型标识与目录时间</summary><code>${esc(m.id)}</code><p>${g.entries[0].account_id?'由账号 '+esc(g.entries[0].account_id)+' 的目录提供。':'由服务级目录提供。'}</p></details>`;
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
  // One page for automation, in the shape of a cron panel. Rows come from the
  // plans the build ships with (workbench.schedules), not from whatever the
  // upstream catalog happened to return this minute: a task must stay visible
  // and switchable even when discovery is down. The 状态 cell is the schedule
  // switch itself, so pausing a task is one click and one verified write.
  async function tasks() {
    // 默认选中真正的任务服务：workbench/providers 的排列里 workbuddy/trae 是
    // 服务级目录，autoclaw/doubao 没有随包计划。取第一个「有随包计划」的服务，
    // 否则会默认落在一个永远空表的服务上。
    state.taskProvider=state.taskProvider||state.providers[0]?.id||'';
    const generation=state.version;
    installViewContent(heading(button('reload','刷新','','quiet'))+
      `<div class="context-bar scope-toolbar">${providerFilter('task-provider',state.taskProvider)}</div><div id="task-discovery-notice"></div><div id="task-list" class="loading">正在读取任务…</div><div id="task-guard"></div>`);
    bindProviderFilter('task-provider',value=>{state.taskProviderPicked=true;state.taskProvider=value;});
    // 三个读取各自独立：账号列表读不到不该让任务表消失；计划读取失败则没有
    // 东西可画，必须如实说明，而不是永远停在「正在读取任务…」。
    let accounts=[],work={schedules:[],runs:[]},workFailed=false;
    const [a,w]=await Promise.allSettled([accountsFor(state.taskProvider),api('workbench')]);
    if(generation!==state.version)return;
    if(a.status==='fulfilled')accounts=a.value;
    if(w.status==='fulfilled')work=w.value||work;
    else if(w.reason?.name!=='AbortError')workFailed=true;
    // 选中的服务没有随包计划时（例如误停在 autoclaw），自动落到第一个有计划的服务，
    // 但只在操作员没有手动切过服务时这么做。
    if(!state.taskProviderPicked&&!workFailed&&!(work.schedules||[]).some(s=>s.provider_id===state.taskProvider)){
      const fallback=(work.schedules||[])[0]?.provider_id;
      if(fallback){state.taskProvider=fallback;await tasks();return;}
    }
    state.schedules=(work.schedules||[]).filter(s=>s.provider_id===state.taskProvider);
    state.runs=work.runs||[];
    // 任务页先按「随包发布的计划」立刻画出整张表：任务名、定时、开关都来自
    // 本地，不依赖任何一次上游目录读取。账号状态是唯一需要实时目录的列，
    // 它晚到就晚填、失败就写「状态未读取」，绝不拿加载占位或错误墙顶掉任务表。
    state.tasks=[];
    state.taskGroups=[];
    paintTaskList([],'pending',accounts,workFailed);
    // Account state is enrichment only, loaded after the table is already up.
    let data={tasks:[],issues:[]},catalogState='ready';
    try{data=await api('workbench/catalog?'+new URLSearchParams({provider:state.taskProvider}));}
    catch(e){if(e.name==='AbortError')return;catalogState='failed';}
    if(generation!==state.version)return;
    state.tasks=data.tasks||[];
    state.taskGroups=groupTasks(state.tasks);
    state.taskNames=state.taskNames||new Map();
    for(const t of state.tasks){
      state.taskNames.set(t.provider_id+'\u0000'+(t.realm||'')+'\u0000'+t.key,t.name);
      state.taskNames.set(t.provider_id+'\u0000\u0000'+t.key,t.name);
    }
    saveTaskNames(state.taskNames);
    paintTaskList(data.issues||[],catalogState,accounts,workFailed);
  }
  // A discovered task belongs to a plan when the plan's key covers it. Module
  // plans use a trailing slash ("growth/") and match every task under it.
  function planKeyMatches(pattern,key){
    if(!pattern||pattern==='*')return true;
    if(pattern===key)return true;
    return pattern.endsWith('/')&&String(key||'').startsWith(pattern);
  }
  function groupsForPlan(plan){
    const selectors=Array.isArray(plan.task_sequence)&&plan.task_sequence.length?plan.task_sequence:[plan.task_key];
    return state.taskGroups.filter(g=>g.provider_id===plan.provider_id&&selectors.some(selector=>planKeyMatches(selector,g.key)));
  }
  function scheduleText(s){
    if(!s)return '';
    return (s.hours||[]).map(h=>String(h).padStart(2,'0')+':'+String(s.minute||0).padStart(2,'0')).join('、');
  }
  // catalogState: 'pending'（表格已画出、账号状态还在读）| 'ready' | 'failed'。
  // 三种「没有目录行」的原因必须分开写：把「还在读」或「读取失败」显示成
  // 「目录暂无此任务」，等于告诉用户这个任务不存在——而它明明就在表里。
  function planSummary(groups,catalogState){
    if(!groups.length)return catalogState==='pending'?'正在读取账号状态':catalogState==='failed'?'账号状态未读取':'目录暂无此任务';
    const counts={claimable:0,pending:0,ready:0,manual:0,blocked:0,done:0,unknown:0};
    for(const g of groups)for(const t of g.accounts){const p=t.state?.phase;counts[p&&counts[p]!==undefined?p:'unknown']++;}
    return [counts.claimable?counts.claimable+' 可领取':counts.ready?counts.ready+' 待执行':counts.done?counts.done+' 已完成':counts.manual?counts.manual+' 需人工':'',counts.blocked?counts.blocked+' 暂不可用':''].filter(Boolean).join(' · ')||'等待核验';
  }
  function paintTaskGuard(){
    const guard=$('#task-guard');if(!guard)return;
    guard.innerHTML=sessionStorage.getItem('loomgate.unconfirmedRun')?notice('上次执行请求结果未确认。请核对执行时间、服务和账号；不要直接重复提交。','warn')+button('review-submission','核对后解除提交保护'):'';
  }
  function paintTaskList(issues,catalogState,accounts=[],workFailed=false){
    const box=$('#task-discovery-notice');if(box)box.remove();
    const list=$('#task-list');
    if(!list)return;
    paintTaskGuard();
    if(issues.length){
      const byModule=new Map();for(const x of issues){byModule.set(x.module_id,(byModule.get(x.module_id)||0)+1);}
      const summary=[...byModule.entries()].map(([name,n])=>n>1?`${name} × ${n}`:name).join('、');
      const notice=document.createElement('details');notice.className='notice warn';notice.id='task-discovery-notice';notice.setAttribute('role','status');notice.dataset.discoveryIssues=String(issues.length);
      notice.innerHTML=`<summary>${issues.length} 项任务目录未能读取（${esc(summary)}）· 定时与开关仍可管理</summary><p class="small">这些分区的对应模块本轮没有返回目录，通常是上游暂时不可用或该账号没有此项任务。账号状态会显示「目录不可用」，但计划与开关不受影响。</p><ul class="issue-list">${issues.map(x=>`<li><code>${esc(x.module_id)}</code> · ${esc(x.account_id)} · ${esc(errorLabels[x.error_code]||x.error_code||'目录不可用')}</li>`).join('')}</ul>`;
      $('#task-list').before(notice);
    }
    list.classList.remove('loading');
    if(workFailed){list.innerHTML=notice('任务计划本次未能从服务读取。任务表与开关都来自服务端保存的计划，不会凭空生成，也不会在读取失败时显示成「没有任务」。','warn')+button('reload','重新读取');
      return;
    }
    if(!state.schedules.length && !state.taskGroups.length){
      list.innerHTML=empty('当前没有可管理的任务','任务与定时随版本发布；账号目录本轮也没有返回任务。');
      return;
    }
    const sections=new Map();
    for(const plan of state.schedules){
      const label=providerName(plan.provider_id);
      if(!sections.has(label))sections.set(label,[]);
      sections.get(label).push(plan);
    }
    const rows=[];
    state.planRows=[];
    for(const [label,plans] of sections){
      const totalAccounts=accounts.filter(a=>a.enabled).length;
      rows.push(`<tr class="task-section"><th colspan="6">${esc(label)}<span class="small muted"> · ${plans.length} 项任务 · ${totalAccounts} 个账号</span></th></tr>`);
      for(const plan of plans){
        const groups=groupsForPlan(plan);
        const summary=planSummary(groups,catalogState);
        const pending=groups.some(g=>g.accounts.some(t=>t.pending_verification));
        const name=taskLabel(plan.provider_id,'',plan.task_key);
        const idx=state.planRows.length;
        state.planRows.push({plan,groups});
        const disabled=!plan.enabled;
        rows.push(`<tr data-plan-row="${esc(plan.id)}" data-task="${esc(plan.task_key)}" class="${disabled?'plan-paused':''}">
          <td class="log-expander" data-label=""><button type="button" data-action="task-log" data-plan-index="${idx}" aria-expanded="false" aria-label="展开最近执行日志">▸</button></td>
          <td data-label="任务"><strong>${esc(name)}</strong><p class="small muted"><span data-plan-frequency>${plan.once_per_day?'每日':'按计划'}</span>${pending?' · 上次结果未确认':''}${groups.some(g=>g.manual_only)?' · 含需人工任务':''}</p></td>
          <td data-label="账号状态"><span class="small">${esc(summary)}</span></td>
          <td data-label="定时" data-plan-timing><strong>${esc(scheduleText(plan))}</strong><p class="small muted">${esc(plan.timezone||'')}</p></td>
          <td data-label="状态"><button type="button" class="switch ${plan.enabled?'on':''}" role="switch" aria-checked="${plan.enabled?'true':'false'}" aria-label="${esc(name)}定时${plan.enabled?'已启用':'已暂停'}" data-action="toggle-schedule" data-index="${state.schedules.indexOf(plan)}" data-lock><span class="switch-track" aria-hidden="true"><span class="switch-thumb"></span></span><span class="switch-text">${plan.enabled?'已启用':'已暂停'}</span></button></td>
          <td data-label="操作"><div class="actions">${button('run-plan','运行',`data-plan-index="${idx}"`,'compact')}${button('edit-plan-time','定时',`data-plan-index="${idx}"`,'quiet compact')}${button('task-log','日志',`data-plan-index="${idx}"`,'quiet compact')}</div></td>
        </tr>`);
      }
    }
    // 目录发现但不在随包计划里的任务：保留可见与可运行，
    // 但不伪造定时。它们的状态完全来自本轮目录。
    const covered=new Set();
    for(const plan of state.schedules)for(const g of groupsForPlan(plan))covered.add(g.provider_id+'\u0000'+(g.realm||'')+'\u0000'+g.key);
    const extras=state.taskGroups.filter(g=>!covered.has(g.provider_id+'\u0000'+(g.realm||'')+'\u0000'+g.key));
    if(extras.length){
      rows.push(`<tr class="task-section"><th colspan="6">目录发现<span class="small muted"> · ${extras.length} 项任务 · 无随包定时</span></th></tr>`);
      for(const g of extras){
        const idx=state.planRows.length;
        state.planRows.push({plan:null,groups:[g]});
        rows.push(`<tr data-task="${esc(g.key)}">
          <td class="log-expander" data-label=""><button type="button" data-action="task-log" data-plan-index="${idx}" aria-expanded="false" aria-label="展开最近执行日志">▸</button></td>
          <td data-label="任务"><strong>${esc(g.name)}</strong><p class="small muted">${g.once?'一次性':g.once_per_day?'每日':'按需'}${g.manual_only?' · 需要人工':''}</p></td>
          <td data-label="账号状态"><span class="small">${esc(planSummary([g],catalogState))}</span></td>
          <td data-label="定时"><span class="muted">未定时</span></td>
          <td data-label="状态">${badge('neutral','—')}</td>
          <td data-label="操作"><div class="actions">${g.manual_only?button('manual-plan','查看要求',`data-plan-index="${idx}"`,'quiet compact'):button('run-plan','运行',`data-plan-index="${idx}"`,'compact')}${button('task-log','日志',`data-plan-index="${idx}"`,'quiet compact')}</div></td>
        </tr>`);
      }
    }
    // 任务页只列任务与计划。执行历史在「执行记录」页；任务输出日志在每行的「日志」里。
    list.innerHTML=`<div class="task-table-wrap"><table class="task-table"><thead><tr><th></th><th>任务</th><th>账号状态</th><th>定时</th><th>状态</th><th>操作</th></tr></thead><tbody>${rows.join('')}</tbody></table></div>`+(catalogState==='failed'?'<p class="small muted">账号状态本轮未读取；任务、定时与开关仍来自本地，照常可用。</p>':'');
  }
  const taskErrors={provider_rejected:'官方服务拒绝了此操作',operation_unconfirmed:'操作结果未确认',account_identity_changed:'账号或绑定发生变化',authentication_required:'需要重新登录',task_expired:'任务标识已过期',operation_not_eligible:'当前尚不满足操作条件',provider_busy:'官方客户端正忙',upstream_failed:'上游操作未成功'};
  // 「日志」显示任务自己的输出（旧版行为）：最近一次运行的文本行，而不是
  // 结构化步骤表。每个任务把日志写在 data/task-logs/<key>.json，服务端读回
  // 最近一次执行的行；任务正在跑时继续刷新。
  async function openPlanLog(planRow){
    if(!planRow)return;
    const provider=planRow.plan?planRow.plan.provider_id:planRow.groups[0]?.provider_id;
    const key=planRow.plan?planRow.plan.task_key:planRow.groups[0]?.key;
    if(!key){toast('这个条目没有可读取的任务日志。');return;}
    // 与旧版一致：标题「任务日志 · <任务名>」，副标题只写「运行中…」或
    // 「最近一次 MM-DD HH:MM」；正文是一整块等宽、可滚动的文本。
    openDialog(`任务日志 · ${taskLabel(provider,'',key)}`,
      `<div class="task-log-dialog"><div class="task-log-head"><span class="task-log-status" role="status" aria-live="polite"></span><button type="button" data-action="task-log-refresh" class="quiet compact" aria-label="刷新日志">刷新</button></div><pre class="task-log-output" id="task-log-body" role="log" tabindex="0">加载中…</pre></div>`);
    $('#dialog').dataset.taskLog='1';
    const generation=state.taskLogGeneration;
    let timer=null,lastSig='',running=false,requestSequence=0;
    const currentDialogOpen=()=>state.taskLogGeneration===generation&&document.getElementById('dialog')?.open===true&&document.getElementById('dialog')?.dataset.taskLog==='1';
    const statusText=data=>data.running?'运行中…':(data.endedAt?'最近一次 '+shortTime(data.endedAt):'');
    const load=async()=>{
      if(!currentDialogOpen())return;
      const request=++requestSequence,isLatest=()=>currentDialogOpen()&&request===requestSequence;
      clearTimeout(timer);
      try{
        const data=await api('task/log?key='+encodeURIComponent(key)+'&provider='+encodeURIComponent(provider),{signal:null});
        if(!isLatest())return;
        running=!!data.running;
        const lines=Array.isArray(data.lines)?data.lines:[];
        const status=$('#dialog-body .task-log-status');
        if(status)status.textContent=statusText(data);
        // 比较完整输出，保留未变文本的选区和滚动；不能仅比较行数和末行。
        const sig=lines.length?lines.join("\n"):(data.message||'这个任务还没有运行记录。点「运行」后，输出会出现在这里。');
        if(sig!==lastSig){
          lastSig=sig;
          const body=$('#task-log-body');
          if(body){
            const atBottom=body.scrollTop+body.clientHeight>=body.scrollHeight-24;
            body.textContent=sig;
            if(atBottom)body.scrollTop=body.scrollHeight;
          }
        }
      }catch(e){
        if(!isLatest())return;
        running=false;lastSig='';
        const body=$('#task-log-body');
        if(body)body.textContent='拉取失败：'+(e instanceof APIError?e.message:'任务日志读取失败。');
      }
      // 只有最新读取拥有刷新定时器，旧请求不能重新启动旧状态的轮询。
      if(isLatest())timer=setTimeout(()=>load(),running?2000:5000);
    };
    $('#dialog-body [data-action=task-log-refresh]').onclick=()=>load();
    await load();
  }

  // 一条计划「运行」时作用在哪些账号上：
  //   1. 计划自己保存了账号范围 → 就用它（操作员显式选择过）；
  //   2. 否则用实时目录里这条计划覆盖到的账号；
  //   3. 目录本轮不可用（没有账号行）→ 返回空数组。**空绝不等于全部**：
  //      看不到账号就不知道会动谁，宁可拒绝并说明原因，也不扩大执行范围。
  function planRunTask(planRow){
    if(!planRow)return null;
    const plan=planRow.plan;
    const provider=plan?plan.provider_id:planRow.groups[0]?.provider_id;
    const key=plan?plan.task_key:planRow.groups[0]?.key;
    if(!provider||!key)return null;
    const planScope=Array.isArray(plan?.account_ids)?plan.account_ids.filter(Boolean):[];
    const groups=planRow.groups||[];
    const catalogScope=[...new Set(groups.flatMap(g=>(g.accounts||[]).filter(t=>!t.manual_only).map(t=>t.account_id)))];
    const sequence=Array.isArray(plan?.task_sequence)?plan.task_sequence:[];
    const accountIDs=planScope.length?planScope:catalogScope;
    return {provider_id:provider,key,name:taskLabel(provider,'',key),account_ids:accountIDs,...(sequence.length?{task_sequence:[...sequence]}:{})};
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

  function confirmRun(task,sourceControl) {
    // 组级执行传 account_ids（数组），单条任务传 account_id。必须优先用数组，
    // 否则组级执行会退化成「全部已启用账号」——执行范围被放大（本轮实测缺陷）。
    const explicit=Array.isArray(task?.account_ids)?task.account_ids.filter(Boolean):task?.account_id?[task.account_id]:[];
    const scope=explicit;
    const req={provider_id:task?.provider_id || state.taskProvider,task_key:task?.key || '*',account_ids:scope};
    if(Array.isArray(task?.task_sequence)&&task.task_sequence.length)req.task_sequence=[...task.task_sequence];
    // 空范围不发起请求：看不到账号就不知道会动谁，宁可拒绝并说明原因，也不扩大范围。
    if(!scope.length){toast('没有可执行的账号范围：账号状态尚未读取，请先刷新任务目录再运行。');return;}
    if(sessionStorage.getItem('loomgate.unconfirmedRun')) {toast('上次执行请求结果未确认。请到任务列表核对后继续，避免重复操作。');location.hash='tasks';return;}
    openDialog('确认执行范围',`<form id="run-form" class="form"><p>${esc(task?.name || '当前范围内的全部任务')}</p><div class="notice">服务：${esc(providerName(req.provider_id))}<br>账号：${esc(scope.join('、'))}<br>执行可能产生上游操作。需要人工的任务会保留待处理状态，奖励以服务端复核为准。</div><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认执行</button></div></form>`);
    const form=$('#run-form');
    form.onsubmit=e=>{
      e.preventDefault();if(state.pending.has('run'))return;
      sessionStorage.setItem('loomgate.unconfirmedRun','1');
      mutate('run','workbench/run',req,async data=>{
        if(!data.run?.id)throw new APIError('request_failed',502);
        sessionStorage.removeItem('loomgate.unconfirmedRun');
        state.runs=[data.run,...(state.runs||[]).filter(run=>run.id!==data.run.id)];
        if(sourceControl){sourceControl.dataset.accepted='true';sourceControl.setAttribute('aria-label','已进入执行队列');sourceControl.textContent='已排队';}
        if(form.isConnected&&$('#dialog').open)closeDialog();
        paintTaskGuard();
        toast('已进入执行队列，进度可在对应任务的「日志」中查看。');
      },sourceControl);
    };
  }

  async function openPlanEditor(plan){
    await LoomPlanEditor.open({
      existing: plan, providers: state.providers, esc, options, cleanSchedule,
      openDialog, closeDialog, toast, api, accountsFor, mutate, verifySchedule,
      after: updatePlanRow,
    });
  }

  // legacy_enabled 是「旧配置原本是否启用」的审计事实。编辑/停启用计划时
  // 必须原样带回，否则一次改时间就会把迁移来源抹掉（rename/badge 也随之消失）。
  function cleanSchedule(s) {return {id:s.id,provider_id:s.provider_id,task_key:s.task_key,...(Array.isArray(s.task_sequence)&&s.task_sequence.length?{task_sequence:s.task_sequence}:{}),account_ids:s.account_ids || [],hours:s.hours,minute:s.minute,timezone:s.timezone,enabled:s.enabled,once_per_day:s.once_per_day,legacy_enabled:s.legacy_enabled};}
  async function verifySchedule(schedule){
    const data=await api('workbench',{signal:null});
    const saved=(data.schedules||[]).find(item=>item.id===schedule.id);
    if(!saved||JSON.stringify(cleanSchedule(saved))!==JSON.stringify(cleanSchedule(schedule)))throw new APIError('request_failed',502);
    return saved;
  }

  function updateScheduleRow(schedule){
    const row=document.querySelector(`tr[data-plan-row="${CSS.escape(String(schedule.id))}"]`);
    if(!row)return;
    row.classList.toggle('plan-paused',!schedule.enabled);
    const timing=row.querySelector('[data-plan-timing]');
    if(timing){$('strong',timing).textContent=scheduleText(schedule);$('p',timing).textContent=schedule.timezone||'';}
    const frequency=row.querySelector('[data-plan-frequency]');
    if(frequency)frequency.textContent=schedule.once_per_day?'每日':'按计划';
    const toggle=row.querySelector('[data-action="toggle-schedule"]');
    if(toggle){
      toggle.classList.toggle('on',!!schedule.enabled);
      toggle.setAttribute('aria-checked',schedule.enabled?'true':'false');
      toggle.setAttribute('aria-label',`${taskLabel(schedule.provider_id,'',schedule.task_key)}定时${schedule.enabled?'已启用':'已暂停'}`);
      const text=toggle.querySelector('.switch-text');if(text)text.textContent=schedule.enabled?'已启用':'已暂停';
    }
  }

  // A schedule toggle is a row mutation, not a route reload. Keep the catalog,
  // account states, scroll position and keyboard focus owned by the current page.
  async function toggleSchedule(index,control){
    const current=state.schedules[index];if(!current)return;
    const next={...cleanSchedule(current),enabled:!current.enabled};
    await mutate('schedule','workbench/schedule',next,async()=>{
      const saved=await verifySchedule(next);
      updatePlanRow(saved);
      toast(next.enabled?'计划已启用。':'计划已停用。');
    },control);
  }

  // A plan editor saves its single plan and the caller can update only that row.
  function updatePlanRow(plan){
    state.schedules=state.schedules.map(s=>s.id===plan.id?plan:s);
    for(const row of state.planRows||[])if(row.plan?.id===plan.id)row.plan=plan;
    updateScheduleRow(plan);
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
  const discoveryIssueLabels = {module_discovery_failed:'模块状态读取失败（原因未归类）',upstream_unavailable:'上游服务暂时不可用，稍后会自动重试',upstream_rate_limited:'上游正在限流，稍后会自动重试',authentication_required:'账号登录已失效，需要重新登录',account_unavailable:'账号当前不可用（额度或授权受限）',module_not_offered:'该账号没有这项能力',buddy_unrecognized:'官方 Buddy 档案无法识别，未做任何操作',travel_state_unrecognized:'官方旅行状态无法识别，本次未操作',travel_directory_unrecognized:'官方旅行地点目录无法识别',makeup_state_unrecognized:'官方补登状态无法识别，本次未操作',redemption_directory_unrecognized:'官方兑换档位目录无法识别',redemption_state_unrecognized:'官方兑换档位状态无法识别',streak_unrecognized:'官方连登记录无法识别',desktop_activity_unavailable:'官方客户端任务目录不可用',desktop_identity_mismatch:'官方客户端账号身份不匹配',desktop_login_required:'官方客户端需要登录'};
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
    const tile=(label,value,detail,tag='')=>`<article class="usage-tile"><span>${esc(label)}</span><strong ${tag}>${value}</strong>${detail?`<p>${detail}</p>`:''}</article>`;
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
        ${tile('总请求',number(scopedTotals.requests),`${number(scopedTotals.failed_requests||0)} 次失败 · ${number(scopedTotals.unreported_requests||0)} 次未报告用量`,'data-total-requests')}
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
    const data=await api('requests?limit=30&offset='+String(state.logOffset || 0));
    const rows=data.items || data.requests || data.rows;
    if(!Array.isArray(rows))throw new APIError('request_failed',503);
    state.requestRows=rows;
    state.logOpen=new Set();
    const head=['','时间','调用方','协议','模型','TTFB / total','输入','缓存命中','输出','tok/s','状态','花费','请求 ID'];
    $('#view').innerHTML=heading(button('reload','刷新记录','','quiet'))+
      (rows.length?`<div class="log-table-wrap"><table class="log-table"><thead><tr>${head.map(h=>`<th>${esc(h)}</th>`).join('')}</tr></thead><tbody>${rows.map((r,i)=>requestRow(r,i,false)).join('')}</tbody></table></div>`:empty('暂无请求记录','记录由网关真实请求生成，不包括面板的目录读取。'))+
      `<div class="pagination"><span>共 ${number(data.total)} 条</span><div class="actions">${button('logs-prev','上一页',!state.logOffset?'disabled':'')}${button('logs-next','下一页',(state.logOffset || 0)+30>=data.total?'disabled':'')}</div></div>`;
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
    const empty = {unread:true,canCheck:false,canApply:false,badge:'manual_action',badgeText:'状态未读取',detail:'',lines:[]};
    if(!update) return empty;
    const status = update.status || {};
    const lines = [];
    const current = (status.current && status.current !== 'unknown') ? status.current : '未知';
    lines.push('当前版本：' + current + (status.commit && status.commit !== 'unknown' ? ' · ' + String(status.commit).slice(0, 7) : ''));
    if(status.repo) lines.push('发布仓库：' + status.repo + (status.visibility ? ' · ' + ({public:'公开',private:'私有',unknown:'未确认'}[status.visibility] || status.visibility) : ''));
    if(status.latest_tag) lines.push('远端最新：' + status.latest_tag);
    if(status.asset_name) lines.push('更新包：' + status.asset_name + (status.asset_size ? ' · ' + Math.round(status.asset_size / 1048576 * 10) / 10 + ' MB' : ''));
    if(validUpdateTime(status.built_at))lines.push('构建时间：'+date(status.built_at));
    if(validUpdateTime(status.deployed_at))lines.push('部署时间：'+date(status.deployed_at));
    if(validUpdateTime(status.latest_at))lines.push('远端发布时间：'+date(status.latest_at));
    const checked=validUpdateTime(status.checked_at);
    if(checked)lines.push('上次成功检查：'+date(status.checked_at));
    if(Number.isFinite(status.downloaded_bytes)&&status.downloaded_bytes>=0&&status.state==='downloading'){
      const mb=value=>Math.round(value/1048576*100)/100;
      lines.push('已下载：'+mb(status.downloaded_bytes)+(Number.isFinite(status.asset_size)&&status.asset_size>0?' / '+mb(status.asset_size):'')+' MB');
    }
    lines.push(status.signature_verification === true ? '签名校验：已启用（内置发布公钥）' : '签名校验：未内置发布公钥（构建不验签）');
    if(status.state === 'failed' && status.last_error) lines.push('失败原因：' + status.last_error);
    const base = {unread:false,lines,detail:'',canCheck:true,canApply:false};
    if(update.ok === false) return {...base,canCheck:false,badge:'disabled',badgeText:'未启用',detail:typeof update.message === 'string' && update.message ? update.message : '当前构建未启用热更新。'};
    if(status.enabled !== true) return {...base,canCheck:false,badge:'disabled',badgeText:'已关闭',detail:status.unavailable_reason || '热更新已在配置中关闭（config update.enabled=false）。'};
    if(status.unavailable_reason) return {...base,canCheck:false,badge:'unavailable',badgeText:'不可用',detail:status.unavailable_reason};
    if(status.state === 'failed') return {...base,badge:'failed',badgeText:'失败',detail:status.last_error || '上次更新失败，可重新检查。'};
    if(['checking','downloading','handover'].includes(status.state)) return {...base,canCheck:false,badge:'ready',badgeText:updateStateLabels[status.state],detail:'更新任务正在进行，完成后以运行版本读回核验；下载完成不等于已切换。'};
    if(status.update_ready === true){
      const canApply=status.state==='idle'&&status.signature_verification===true&&typeof status.repo==='string'&&status.repo.trim()!==''&&
        typeof status.asset_name==='string'&&status.asset_name!==''&&typeof status.latest_tag==='string'&&/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(status.latest_tag);
      return {...base,canApply,badge:'ready',badgeText:'可升级',detail:canApply?'检测到更新；需确认目标后才下载，保留服务端更新代理及签名校验。':'检测到更新，但本构建缺少验签能力或完整目标信息；此界面不会应用未核验的更新。'};
    }
    if(checked) return {...base,badge:'ready',badgeText:'已是最新',detail:'已读取远端发布，没有比当前构建更新的版本。'};
    return {...base,badge:'manual_action',badgeText:'未检查',detail:'尚未读取远端发布；点击「检查更新」后再确认是否有新版本。'};
  }
  const updateIntentKey='loomgate.unconfirmedUpdate';
  function readUpdateIntent(){
    try{
      const raw=sessionStorage.getItem(updateIntentKey);if(!raw)return null;
      const intent=JSON.parse(raw);
      return typeof intent?.tag==='string'&&typeof intent?.repo==='string'?intent:{blocked:true};
    }catch{return {blocked:true};}
  }
  function clearUpdateIntent(intent){
    try{
      const current=readUpdateIntent();
      if(current?.tag!==intent.tag||current?.repo!==intent.repo)return false;
      sessionStorage.removeItem(updateIntentKey);return sessionStorage.getItem(updateIntentKey)===null;
    }catch{return false;}
  }
  function updateActionNotice(card,text,kind='warn'){
    const node=$('[data-update-action-status]',card);if(!node)return;
    node.textContent=text;node.hidden=!text;node.className='notice '+kind;
  }
  function paintUpdateCard(card,update,error='') {
    if(!card?.isConnected)return;
    const info=updateView(update),status=update?.status||{};
    let intent=readUpdateIntent(),completed=false;
    if(intent&&!intent.blocked&&update?.ok===true&&status.repo===intent.repo&&status.current===intent.tag){
      completed=clearUpdateIntent(intent);if(completed)intent=null;
    }
    $('.update-notice',card).innerHTML=info.unread?notice('更新状态读取失败：'+(error||'服务未返回状态')+'。','error'):notice(info.detail,info.badge==='ready'?'':'warn');
    $('.update-facts',card).innerHTML=info.lines.map(line=>{const p=line.indexOf('：');return `<div><dt>${esc(p>0?line.slice(0,p):'状态')}</dt><dd>${esc(p>0?line.slice(p+1):line)}</dd></div>`;}).join('');
    $('.update-facts',card).hidden=!info.lines.length;
    $('[data-action="update-check"]',card).hidden=!info.canCheck||Boolean(intent);
    $('[data-action="update-apply"]',card).hidden=!info.canApply||Boolean(intent);
    $('[data-action="update-review"]',card).hidden=!intent||intent.blocked||info.unread||['checking','downloading','handover'].includes(status.state);
    $('.update-badge',card).innerHTML=badge(info.badge,info.badgeText);
    if(completed)updateActionNotice(card,'运行版本已核验：'+status.current+'。此处是当前进程读回，不是部署时间记录。','');
    else if(intent?.blocked)updateActionNotice(card,'无法安全读取或记录本标签页的更新提交，已阻止应用更新。可继续只读刷新状态。');
    else if(intent)updateActionNotice(card,status.state==='failed'?'服务端报告更新失败；原提交保护仍保留，请核对后解除。':'有一次更新提交尚未核验运行版本；只刷新状态，不会自动重复应用。');
  }
  async function readUpdateStatus(card,generation,polls=0){
    clearTimeout(state.poll);
    card._updateReadSequence=(card._updateReadSequence||0)+1;
    const request=card._updateReadSequence;
    const current=()=>generation===state.version&&request===card._updateReadSequence&&card?.isConnected&&$('#setting-update')===card;
    if(!current())return;
    try{
      const snapshot=await api('update',{signal:null,allowNotOK:true});
      if(!current())return;
      paintUpdateCard(card,snapshot);
      const intent=readUpdateIntent(),status=snapshot.status||{};
      if(polls>0&&intent&&!intent.blocked&&snapshot.ok===true&&status.enabled===true&&!status.unavailable_reason&&status.state!=='failed'){
        state.poll=setTimeout(()=>readUpdateStatus(card,generation,polls-1),2000);
      }
    }catch(e){
      if(current())paintUpdateCard(card,undefined,e instanceof APIError?e.message:'状态读取失败；未重复提交更新。');
    }
  }
  function updateTarget(status){return JSON.stringify([status.repo,status.current,status.commit,status.latest_tag,status.asset_name]);}
  async function updateApplyDialog(){
    return readDialog('核对更新目标',()=>api('update',{allowNotOK:true}),snapshot=>{
      if(readUpdateIntent())throw new APIError('request_failed',409,'存在未核验的更新提交，请先刷新状态并核对。');
      if(!updateView(snapshot).canApply)throw new APIError('request_failed',409,'当前没有可应用的已签名更新，请重新检查状态。');
      const target=snapshot.status,card=$('#setting-update'),generation=state.version;
      openDialog('确认应用更新',`<form id="update-form" class="form"><p>当前：<code>${esc(target.current)}</code> → 目标：<code>${esc(target.latest_tag)}</code></p><p>仓库：${esc(target.repo)}<br>更新包：${esc(target.asset_name)}</p><p class="notice warn">确认后才提交下载与切换。保留服务端原加速代理、签名和摘要校验；下载或请求受理不等于新版本已运行。不会在此执行系统重启。</p><label class="check"><input name="acknowledge" type="checkbox" required>我确认上述目标，并同意应用更新</label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">确认应用</button></div></form>`);
      const form=$('#update-form'),submit=$('button[type=submit]',form),message=$('#form-message',form);
      form.onsubmit=async e=>{
        e.preventDefault();
        if(form.dataset.submitting==='1'||form.dataset.submitted==='1'||state.pending.has('runtime-update'))return;
        if(!form.reportValidity())return;
        form.dataset.submitting='1';submit.disabled=true;state.pending.add('runtime-update');
        let attempted=false,rejected=false,intent=null;
        try{
          if(readUpdateIntent())throw new APIError('request_failed',409,'已有未核验的更新提交，请先核对。');
          const fresh=await api('update',{signal:null,allowNotOK:true});
          if(generation!==state.version||!ownsDialog(form))return;
          if(!updateView(fresh).canApply||updateTarget(fresh.status)!==updateTarget(target))throw new APIError('request_failed',409,'更新目标或运行构建已变化；请关闭并重新确认，未提交更新。');
          intent={tag:target.latest_tag,repo:target.repo};
          const encoded=JSON.stringify(intent);
          try{sessionStorage.setItem(updateIntentKey,encoded);if(sessionStorage.getItem(updateIntentKey)!==encoded)throw new Error();}
          catch{throw new APIError('request_failed',409,'无法保存提交保护，未提交更新。');}
          attempted=true;form.dataset.submitted='1';
          const data=await api('update/apply',{body:{tag:intent.tag},signal:null,allowNotOK:true});
          if(data.ok===false){rejected=true;clearUpdateIntent(intent);throw new APIError('request_failed',409,typeof data.message==='string'?data.message:'服务端拒绝了更新请求。');}
          if(data.ok!==true)throw new APIError('request_failed',502);
          if(ownsDialog(form))closeDialog();
          toast('更新请求已接受；将只读核验运行版本，不会重复提交。');
          await readUpdateStatus(card,generation,30);
        }catch(e){
          rejected=rejected||e instanceof APIError&&e.status>=400&&e.status<500&&e.status!==408;
          if(attempted&&rejected)clearUpdateIntent(intent);
          const text=!attempted||rejected?(e instanceof APIError?e.message:'更新预检失败，未提交。'):'更新提交结果未确认。请刷新状态核对，不要重复应用。';
          if(ownsDialog(form)){message.textContent=text;message.hidden=false;}
          if(generation===state.version&&card?.isConnected){
            if(attempted)paintUpdateCard(card,undefined,text);
            updateActionNotice(card,text);
          }
        }finally{
          state.pending.delete('runtime-update');form.dataset.submitting='0';
          if(!attempted||rejected){form.dataset.submitted='0';if(ownsDialog(form))submit.disabled=false;}
        }
      };
    });
  }
  async function reviewUpdateIntent(){
    return readDialog('核对更新提交保护',()=>api('update',{allowNotOK:true}),snapshot=>{
      const intent=readUpdateIntent(),status=snapshot.status||{};
      if(!intent||intent.blocked||snapshot.ok!==true||['checking','downloading','handover'].includes(status.state))throw new APIError('request_failed',409,'更新状态未确认或仍在进行，暂不能解除提交保护。');
      openDialog('解除更新提交保护',`<form id="update-review-form" class="form"><p>原目标：${esc(intent.tag)}<br>当前运行：${esc(status.current||'未读取')}<br>当前状态：${esc(updateStateLabels[status.state]||'未读取')}</p><p class="notice warn">此操作只解除浏览器防重保护，不会下载、安装或重发更新。尚未确认时请取消。</p><label class="check"><input type="checkbox" required>我已核对运行程序及更新状态，确认可以解除保护</label><div class="actions">${button('close','取消')}<button type="submit">解除保护</button></div></form>`);
      const form=$('#update-review-form'),card=$('#setting-update');
      form.onsubmit=e=>{e.preventDefault();if(!form.reportValidity())return;if(clearUpdateIntent(intent)){if(ownsDialog(form))closeDialog();paintUpdateCard(card,snapshot);updateActionNotice(card,'已按人工确认解除提交保护；未执行任何更新。','');}};
    });
  }
  function paintLoopSetting(card,stopOnly,error='') {
    const known=typeof stopOnly==='boolean', control=$('[data-action="loop-toggle"]',card);
    $('h3',card).textContent=!known?'状态未读取':stopOnly?'命中即停止':'命中后自动重发';
    $('.setting-detail',card).textContent=known?'作用于后续请求；已开始的请求沿用原设置。':('重复推理保护读取失败：'+(error||'服务未返回状态')+'。');
    control.hidden=!known;
    control.dataset.stopOnly=String(stopOnly);
    control.textContent=stopOnly?'改为自动重发':'改为命中即停';
  }
  function validLANConfig(lan){return Boolean(lan&&typeof lan.enabled==='boolean'&&typeof lan.listen==='string'&&typeof lan.allow_cidrs==='string');}
  function lanConfigDetail(lan){return `已保存监听配置：${esc(lan.listen||'未声明')}${lan.allow_cidrs?'，允许 '+esc(lan.allow_cidrs):''}${lan.has_key?'':'；尚未创建调用密钥'}<br>此处不核验当前进程的实际监听；修改监听地址需要重启服务后生效。`;}
  function updateLANCard(card,lan){
    if(!card?.isConnected)return;
    $('[data-lan-title]',card).textContent=lan.enabled?'配置已启用':'配置为仅本机';
    $('[data-lan-config]',card).innerHTML=lanConfigDetail(lan);
    const control=$('[data-action=lan-settings]',card);if(control)control.textContent=lan.enabled?'调整':'启用';
  }
  async function lanSettingsDialog(control){
    return readDialog('读取局域网配置',()=>api('state'),snapshot=>{
      const lan=snapshot?.lan;
      if(!validLANConfig(lan))throw new APIError('request_failed',502,'局域网配置尚未读回，未打开保存表单。');
      openDialog('局域网访问',`<form id="lan-form" class="form"><p class="muted">这里修改的是配置文件中的监听地址与允许网段；实际监听与进程重启状态不由此表单推断。</p><label class="check"><input type="checkbox" name="enabled" ${lan.enabled?'checked':''}>启用局域网访问</label><label>已保存监听地址<input name="listen" value="${esc(lan.listen||'')}" readonly aria-readonly="true"></label><label>允许网段<input name="allow_cidrs" value="${esc(lan.allow_cidrs||'')}" placeholder="例如 192.0.2.0/24"></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);
      const form=$('#lan-form'),card=$('#setting-lan'),generation=state.version;
      form.onsubmit=e=>{
        e.preventDefault();
        const fields=new FormData(form),enabled=fields.get('enabled')==='on';
        const want={enabled,allow_cidrs:enabled?String(fields.get('allow_cidrs')||'').trim():''};
        mutate('lan','lan',want,async data=>{
          const saved=(await api('state',{signal:null}))?.lan;
          if(!validLANConfig(saved)||saved.enabled!==want.enabled||saved.allow_cidrs!==want.allow_cidrs)throw new APIError('request_failed',502,'配置保存后的读回不匹配。');
          if(generation===state.version)updateLANCard(card,saved);
          if(ownsDialog(form)){closeDialog();if(control?.isConnected)control.focus({preventScroll:true});}
          toast((data.message||'局域网配置已保存。')+' 已核验保存值；不据此推断实际监听。');
        });
      };
    });
  }
  async function settings() {
    const generation=state.version;
    const [session,snapshot,loop,update]=await Promise.allSettled([
      api('session'), api('state'), api('features/reasoning-loop'), api('update',{allowNotOK:true}),
    ]);
    if(generation!==state.version)return;
    if(session.status==='rejected')throw session.reason;
    state.sessionUsername=session.value.username||'';
    const lan=validLANConfig(snapshot.value?.lan)?snapshot.value.lan:null;
    const lanTitle=!lan?'状态未读取':lan.enabled?'配置已启用':'配置为仅本机';
    const lanDetail=!lan?notice('局域网状态读取失败：'+(snapshot.reason?.message||'服务未返回状态')+'。','error'):lanConfigDetail(lan);
    installViewContent(heading()+`<div class="split">
      <section class="panel"><div class="panel-title"><h2>显示与会话</h2></div>
        <div class="settings-line"><div><h3>界面主题</h3><p>在此浏览器保存偏好。</p></div>${button('theme','切换主题')}</div>
        <div class="settings-line" data-session-status><div><h3>当前会话</h3><p>有效期至 <span data-session-expiry>${esc(date(session.value.expiresAt))}</span></p></div>${badge('ready','已登录')}</div>
        <div class="settings-line" data-account-security><div><h3>管理账号安全</h3><p>修改密码后其他设备需重新登录。</p></div>${button('password','修改密码')}</div>
      </section>
      <section class="panel" id="setting-lan"><div class="panel-title"><h2>局域网访问</h2></div>
        <div class="settings-line"><div><h3 data-lan-title>${lanTitle}</h3><div data-lan-config>${lanDetail}</div></div>${lan?button('lan-settings',lan.enabled?'调整':'启用','','quiet'):''}</div>
      </section>
      <section class="panel" id="setting-loop"><div class="panel-title"><h2>重复推理保护</h2></div>
        <div class="settings-line"><div><h3></h3><p class="setting-detail"></p></div>${button('loop-toggle','改为命中即停','hidden','quiet')}</div>
      </section>
      <section class="panel" id="setting-update"><div class="panel-title"><h2>迁移与更新</h2></div>
        <div class="update-notice"></div><dl class="detail-specs update-facts" hidden></dl><p data-update-action-status class="notice warn" role="status" hidden></p>
        <div class="section-gap actions">${button('update-check','检查更新','hidden','quiet')}${button('update-apply','应用更新','hidden','primary')}${button('update-status','刷新状态','','quiet')}${button('update-review','核对后解除提交保护','hidden','quiet')}<span class="update-badge"></span></div>
      </section>
    </div>`);
    paintLoopSetting($('#setting-loop'),loop.value?.stop_only,loop.reason?.message);
    paintUpdateCard($('#setting-update'),update.value,update.reason?.message);
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
  function keyModelsText(models){return (models||[]).length?(models.slice(0,3).join(', '))+(models.length>3?' 等 '+models.length+' 个':''):'不限模型';}
  function keyList(data){
    if(!Array.isArray(data?.keys))throw new APIError('request_failed',502,'密钥列表读回无效。');
    return data.keys;
  }
  function keyLimitsValue(limits){return ['requests_per_minute','max_concurrent','queue_timeout_seconds'].map(name=>limits?.[name]??0);}
  function keyEditMatches(saved,want){
    const models=saved?.models??[];
    return Boolean(saved&&saved.id===want.id&&saved.name===want.name&&(saved.note||'')===want.note&&Array.isArray(models)&&
      JSON.stringify([...models].sort())===JSON.stringify([...want.models].sort())&&
      JSON.stringify(keyLimitsValue(saved.limits))===JSON.stringify(keyLimitsValue(want.limits)));
  }
  async function mutateKey(id,path,body,after,control){
    const key='key-write:'+id;
    if(state.pending.has(key)){toast('这把密钥还有一项操作尚未核验，请先等待其结果。');return;}
    await mutate(key,path,body,after,control);
  }
  function keyRow(id){return [...document.querySelectorAll('[data-key-row]')].find(node=>node.dataset.keyRow===id)?.closest('tr');}
  function updateKeyRow(saved){
    const row=keyRow(saved.id);
    if(!row)return;
    const text=(node,value)=>{if(node&&node.textContent!==value)node.textContent=value;};
    text(row.querySelector('[data-key-row] strong'),saved.name||'未命名');
    text(row.querySelector('[data-key-row] p'),saved.note||'');
    text(row.cells[1]?.querySelector('code'),saved.masked_key||'');
    text(row.cells[1]?.querySelector('p'),keyModelsText(saved.models));
    text(row.querySelector('[data-key-limits]'),limitSummary(saved.limits));
    const status=row.querySelector('[data-key-enabled]'),toggle=row.querySelector('[data-action=key-toggle]');
    if(toggle?.dataset.enabled!==(saved.enabled?'1':'0')){
      if(status)status.innerHTML=badge(saved.enabled?'ready':'disabled',saved.enabled?'启用':'停用');
      if(toggle){toggle.dataset.enabled=saved.enabled?'1':'0';toggle.textContent=saved.enabled?'停用':'启用';}
    }
  }
  const keyColumns=['名称','密钥','状态','有效期','限流','累计 Token','操作'];
  function keyCells(k,live=null){
    const liveText=live?`${number(live.requests)} 次/分窗口 · 进行 ${number(live.active)} · 排队 ${number(live.queued)}`:'实时快照未读取';
    const tokens=k.total_tokens===undefined?'—':number(k.total_tokens);
    return [`<span data-key-row="${esc(k.id)}"><strong>${esc(k.name||'未命名')}</strong><p class="small muted">${esc(k.note||'')}</p></span>`,`<code>${esc(k.masked_key||'')}</code><p class="small muted">${esc(keyModelsText(k.models))}</p>`, `<span data-key-enabled>${badge(k.enabled?'ready':'disabled',k.enabled?'启用':'停用')}</span>`,esc(k.expires_at?date(k.expires_at):'无限制'),`<span data-key-limits>${esc(limitSummary(k.limits))}</span><p class="small muted">${esc(liveText)}</p>`,tokens,button('key-edit','编辑',`data-id="${esc(k.id)}"`,'quiet')+button('key-toggle',k.enabled?'停用':'启用',`data-id="${esc(k.id)}" data-enabled="${k.enabled?'1':'0'}"`,'quiet')+button('key-copy','显示密钥',`data-id="${esc(k.id)}"`,'quiet')+button('key-delete','删除',`data-id="${esc(k.id)}"`,'quiet danger')];
  }
  function insertKeyRow(saved){
    const list=$('#key-list');if(!list)return;
    if(keyRow(saved.id)){updateKeyRow(saved);return;}
    const html=table(keyColumns,[keyCells(saved)]),body=$('tbody',list);
    if(!body){list.innerHTML=html;return;}
    const template=document.createElement('template');template.innerHTML=html;
    body.append($('tbody tr',template.content));
  }
  async function keyPage() {
    const generation=state.version;
    const data=await api('keys');
    if(!Array.isArray(data.keys))throw new APIError('request_failed',502);
    if(generation!==state.version)return;
    state.keys=data.keys.slice();
    let limits={};
    try{const live=await api('key-limits');if(generation!==state.version)return;if(live?.ok===true&&live.keys&&typeof live.keys==='object')limits=live.keys;}catch(e){if(e.name!=='AbortError')limits={};}
    if(generation!==state.version)return;
    const rows=data.keys.map(k=>keyCells(k,limits[k.id]||null));
    // The endpoint is the single thing every caller has to copy correctly, so it
    // is shown once at the top of the page instead of being buried in docs.
    const base=location.origin+'/v1';
    installViewContent(heading(button('key-new','创建密钥','','primary')+button('reload','刷新','','quiet'))+
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
        <p class="small muted">鉴权用下面任意一把密钥作为 Bearer token；模型名形如 <code>autoclaw:glm-5.3-flash</code> 或 <code>doubao:deepseek-v4-flash</code>，可在「模型目录」里查看。</p>
      </section>`+
      `<div id="key-list">${rows.length?table(keyColumns,rows):empty('还没有 API 密钥','创建第一把密钥后才能通过 OpenAI / Anthropic / Gemini 兼容接口调用网关。')}</div>`+
      notice('密钥明文只在创建或「显示密钥」时返回一次；列表只显示掩码。停用会立即生效，删除不可恢复。','warn'));
  }
  async function keyEditDialog(id) {
    return readDialog('读取密钥配置',()=>api('keys'),data=>{
    const key=keyList(data).find(k=>k.id===id);
    if(!key)throw new APIError('request_failed',404,'密钥已不存在，请刷新列表。');
    const limits=key.limits||{};
    openDialog('编辑密钥',`<form id="key-form" class="form"><p class="muted">密钥 <code>${esc(key.masked_key||'')}</code> 的明文不会再显示。留空模型表示不限；模型目录和倍率按服务端当前可见目录读取。</p><label>名称<input name="name" value="${esc(key.name||'')}" required maxlength="80"></label><label>备注<input name="note" value="${esc(key.note||'')}" maxlength="200"></label><input type="hidden" name="models"><section class="key-model-picker-section"><h3>绑定模型</h3><div data-model-picker></div></section><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="${esc(limits.requests_per_minute??0)}"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="${esc(limits.max_concurrent??0)}"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="${esc(limits.queue_timeout_seconds??0)}"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存并核对</button></div></form>`);
    const form=$('#key-form'),picker=LoomModelPicker.mount(form,{api,esc,selected:key.models||[],providers:state.providers||[]});
    form.onsubmit=e=>{
      e.preventDefault();
      const fields=new FormData(form),body={id:key.id,name:String(fields.get('name')||'').trim(),note:String(fields.get('note')||'').trim(),models:picker.values()};
      const rpm=Number(fields.get('requests_per_minute')||0),conc=Number(fields.get('max_concurrent')||0),queue=Number(fields.get('queue_timeout_seconds')||0);
      body.limits=(rpm||conc||queue)?{requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue}:null;
      const generation=state.version;
      mutateKey(key.id,'keys/update',body,async()=>{
        const saved=keyList(await api('keys',{signal:null})).find(k=>k.id===key.id);
        if(!keyEditMatches(saved,body))throw new APIError('request_failed',502,'保存后读回与提交不一致。');
        if(generation===state.version){state.keys=state.keys.map(k=>k.id===key.id?saved:k);updateKeyRow(saved);}
        if(ownsDialog(form))closeDialog();
        toast('密钥已更新，并逐项核验备注、模型和限流。');
      });
    };
    });
  }
  async function keyNewDialog() {
    openDialog('创建 API 密钥',`<form id="key-form" class="form"><p class="muted">模型绑定使用服务端当前客户端目录及其真实倍率。留空表示不限模型。</p><label>名称<input name="name" required maxlength="80" autofocus></label><label>备注<input name="note" maxlength="200"></label><input type="hidden" name="models"><section class="key-model-picker-section"><h3>绑定模型</h3><div data-model-picker></div></section><fieldset><legend>限流</legend><label>每分钟请求数（0 不限）<input name="requests_per_minute" type="number" min="0" max="60000" value="0"></label><label>并发上限（0 不限）<input name="max_concurrent" type="number" min="0" max="256" value="0"></label><label>排队秒数（0 不排队）<input name="queue_timeout_seconds" type="number" min="0" max="30" value="0"></label></fieldset><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">创建</button></div></form>`);
    const form=$('#key-form'),picker=LoomModelPicker.mount(form,{api,esc,selected:[],providers:state.providers||[]}),generation=state.version;
    form.onsubmit=e=>{e.preventDefault();const f=new FormData(form);const body={name:String(f.get('name')||'').trim(),note:String(f.get('note')||'').trim(),models:picker.values()};const rpm=Number(f.get('requests_per_minute')||0),conc=Number(f.get('max_concurrent')||0),queue=Number(f.get('queue_timeout_seconds')||0);if(rpm||conc||queue)body.limits={requests_per_minute:rpm,max_concurrent:conc,queue_timeout_seconds:queue};mutate('key-new','keys',body,async data=>{
        if(typeof data.key!=='string'||!data.key)throw new APIError('request_failed',502,'服务端没有返回完整密钥。');
        // Deliver the one-time result before any readback that could fail or outlive its form.
        const result=document.createElement('section');result.className='panel';result.dataset.keyCreatedResult='1';
        result.innerHTML=`<p>密钥「${esc(body.name)}」已返回创建回执。请立即复制保存；此结果不会存入浏览器存储。</p><label>密钥<textarea readonly rows="3"></textarea></label><p data-key-sync data-state="reading" class="small muted" role="status">正在核对新密钥的列表记录…</p><div class="actions"><button type="button" class="quiet">我已保存</button></div>`;
        result.querySelector('textarea').value=data.key;
        const inDialog=ownsDialog(form),sync=$('[data-key-sync]',result);
        if(inDialog){
          $('#dialog-body').replaceChildren(result);
          result.querySelector('button').onclick=()=>{if(ownsDialog(result))closeDialog();};
        }else{
          // A late success still belongs to its submitter, never to a newer draft.
          $('#view').append(result);
          result.querySelector('button').onclick=()=>result.remove();
        }
        toast(inDialog?'密钥已返回，请立即保存；列表正在核验。':'上一笔密钥已返回，结果保留在页面下方；当前表单未改变。');
        try{
          const id=data.entry?.id;
          if(typeof id!=='string'||!id||!keyEditMatches(data.entry,{...body,id}))throw new APIError('request_failed',502);
          const saved=keyList(await api('keys',{signal:null})).find(k=>k.id===id);
          if(!keyEditMatches(saved,{...body,id}))throw new APIError('request_failed',502);
          if(generation===state.version&&state.route==='keys'){
            state.keys=[...state.keys.filter(k=>k.id!==id),saved];
            insertKeyRow(saved);
          }
          if(result.isConnected){sync.dataset.state='verified';sync.textContent='新密钥已按返回标识核对名称、备注、模型和限流。';}
        }catch{
          if(result.isConnected){sync.dataset.state='unconfirmed';sync.className='notice warn';sync.textContent='创建已返回密钥，但列表记录尚未核对匹配。请先保存此结果，再刷新列表核对；不要重复创建。';}
        }
      });};
  }
  async function migration() {
    const generation=state.version;
    let data={},unavailable='';
    try{data=await api('migration',{allowNotOK:true});}catch(e){if(e.name!=='AbortError')unavailable=e.message||'迁移状态未读取';}
    if(generation!==state.version)return;
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
    openDialog('修改管理账号',`<form id="password-form" class="form"><label>当前密码<input name="current" type="password" autocomplete="current-password" required></label><label>用户名<input name="username" value="${esc(state.sessionUsername||'')}" autocomplete="username" required pattern="[A-Za-z0-9_.-]{3,32}"></label><label>新密码<input name="password" type="password" autocomplete="new-password" minlength="8" required></label><label>再次输入新密码<input name="confirm" type="password" autocomplete="new-password" required></label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="primary">保存</button></div></form>`);
    const form=$('#password-form');
    form.onsubmit=e=>{
      e.preventDefault();
      const body=Object.fromEntries(new FormData(form));
      if(body.password!==body.confirm){const message=$('#form-message',form);message.textContent='两次新密码不一致。';message.hidden=false;return;}
      const generation=state.version;
      mutate('password','auth/password',body,async data=>{
        if(data.username!==body.username||!Number.isFinite(data.expiresAt)||data.expiresAt<=0)throw new APIError('request_failed',502);
        const session=await api('session',{signal:null});
        if(session.username!==body.username||!Number.isFinite(session.expiresAt)||session.expiresAt<=0)throw new APIError('request_failed',502,'管理账号已返回保存回执，但新会话读回未匹配。');
        for(const name of ['current','password','confirm'])form.elements[name].value='';
        if(generation===state.version){
          state.sessionUsername=session.username;
          const expiry=$('[data-session-expiry]');if(expiry)expiry.textContent=date(session.expiresAt);
          const info=$('[data-account-security] p');if(info)info.textContent='管理账号已更新，新会话已核验；其他设备需重新登录。';
        }
        if(ownsDialog(form))closeDialog();
        toast('管理账号已更新，新会话已从服务端核验。');
      });
    };
  }
  document.addEventListener('keydown',e=>{const row=e.target.closest('.selection-row');if(!row||!['ArrowDown','ArrowUp','Home','End'].includes(e.key))return;const rows=[...row.closest('[role=listbox]').querySelectorAll('.selection-row')],i=rows.indexOf(row),next=e.key==='Home'?0:e.key==='End'?rows.length-1:Math.max(0,Math.min(rows.length-1,i+(e.key==='ArrowDown'?1:-1)));e.preventDefault();if(row.dataset.action==='select-model-group')selectModelGroup(next,false);else return;rows[next].focus();});
  const renderers={overview,providers:services,accounts,models:modelPage,tasks,schedules:tasks,runs:tasks,usage,logs,keys:keyPage,migration,settings};
  async function renderCurrent(generation,refreshProviders=false,showLoading=true){
    const render=renderers[state.route];
    if(showLoading)showRouteLoading();
    try{
      await providers(refreshProviders);
      if(generation!==state.version)return;
      await render();
      finishRouteRender(generation);
    }catch(e){if(generation===state.version)fail(e);}
  }
  async function reload(refreshProviders=false) {
    state.pollStarted=0;
    await renderCurrent(begin(),refreshProviders,false);
  }
  async function action(element) {
    const name=element.dataset.action;
    // 组详情里的行内动作用 data-task-index 指回扁平任务数组；
    const index=element.dataset.taskIndex!==undefined?Number(element.dataset.taskIndex):Number(element.dataset.index);
    if(name==='select-model-group'){selectModelGroup(index);return;}
    if(name==='model-back'||name==='task-back'){const kind=name.split('-')[0];$('#'+kind+'-browser').classList.remove('detail-open');$('#'+kind+'-browser [aria-selected=true]')?.focus();return;}
    if(name==='close'){closeDialog();return;}
    if(name==='reload'){await reload(['providers','overview'].includes(state.route));return;}
    if(name==='theme'){toggleTheme();return;}
    if(name==='provider-models' || name==='account-models'){state.modelProvider=element.dataset.provider;state.modelAccount=element.dataset.account || '';state.modelOffset=0;location.hash='models';return;}
    if(name==='provider-accounts'){state.accountProvider=element.dataset.provider;location.hash='accounts';return;}
    if(name==='connect-account'||name==='reconnect-account'){
      const provider=element.dataset.provider||state.accountProvider;
      // Cookie-authenticated providers have no login flow: they take a pasted
      // session cookie instead. AutoClaw takes a phone number in its SMS flow.
      if(provider==='autoclaw'){await LoomConnections.open('autoclaw',element.dataset.account||'','','');return;}
      await LoomConnections.open(provider,element.dataset.account||'','',element.dataset.realm||'');
      return;
    }
    if(name==='toggle-account'){
      const a=state.accountRows[index],provider=element.dataset.provider,generation=state.version;
      if(!a)return;
      const row=element.closest('tr'),accountID=a.id,targetEnabled=!a.enabled,revision=a.revision;
      const modern=state.providers.find(p=>p.id===provider)?.catalog_scope==='account';
      openDialog(targetEnabled?'启用账号':'停用账号',`<form id="account-toggle-form" class="form"><p>账号 <code>${esc(accountID)}</code></p><p class="muted">${targetEnabled?'启用后该账号可再次用于目录读取和任务。':'停用后不再用于后续目录请求与任务。正在进行的操作仍需单独核验。'}</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','返回')}<button type="submit" class="primary">确认${targetEnabled?'启用':'停用'}</button></div></form>`);
      const form=$('#account-toggle-form');
      form.onsubmit=e=>{
        e.preventDefault();
        mutate('account-toggle',modern?'workbench/account/toggle':'account/toggle',modern?{id:accountID,enabled:targetEnabled,revision}:{uid:accountID,disabled:!targetEnabled},async()=>{
          const updated=(await accountsFor(provider,{signal:null})).find(x=>x.id===accountID);
          if(!updated||updated.enabled!==targetEnabled)throw new APIError('request_failed',502);
          state.providersDirty=true;
          if(generation===state.version){
            Object.assign(a,updated);
            const control=$(`#account-list [data-action="toggle-account"][data-index="${index}"]`);
            const status=control?.closest('tr').querySelector('[data-account-status]');
            if(status)status.innerHTML=accountStatus(a);
            if(control)control.textContent=a.enabled?'停用':'启用';
          }
          if(form.isConnected&&$('#dialog').open){closeDialog();if(row?.isConnected)element.focus({preventScroll:true});}
          toast('账号状态已从服务端核验。');
        });
      };return;
    }
    if(name==='model-refresh'){await loadModels(true);return;}
    if(name==='model-capability'){return;}
    if(name==='model-group-next' || name==='model-group-prev'){state.modelGroupPage=Math.max(0,state.modelGroupPage+(name==='model-group-next'?1:-1));drawModels();return;}
    if(name==='model-next' || name==='model-prev'){state.modelOffset=Math.max(0,state.modelOffset+(name==='model-next'?8:-8));await loadModels(false);return;}
    if(name==='connection-help'){openDialog('账号接入状态',notice('账号状态来自真实后端。WorkBuddy 支持扫码/短信登录与重新连接；AutoClaw 是纯 HTTP 模型调用，只需导入一次凭证即可，不创建任何会话、不依赖客户端。只有在服务确认需要人工操作时才会发起对应流程。','')+'<p class="muted">登录流程需要真实扫码或验证码时，界面会显示明确步骤；不会在后台静默重试已成功的操作。</p>');return;}
    if(name==='manual-task'){const t=state.tasks[index];openDialog('所需人工操作',`<h3>${esc(t.name)}</h3><p class="section-gap">${esc(t.description)}</p>${notice('请在对应官方服务完成要求的内容或操作。完成与奖励以服务端核验为准。','warn')}`);return;}
    if(name==='run-task' || name==='run-all'){confirmRun(name==='run-task'?state.tasks[index]:null);return;}
    if(name==='open-runs'){location.hash='tasks';return;}
    if(name==='task-log'){await openPlanLog(state.planRows?.[Number(element.dataset.planIndex)]);return;}
    if(name==='manual-plan'){const row=state.planRows?.[Number(element.dataset.planIndex)];const g=row?.groups?.[0];if(g)openDialog('所需人工操作',`<h3>${esc(g.name)}</h3><p class="section-gap">${esc(g.description||'')}</p>${notice('请在对应官方服务完成要求的内容或操作。完成与奖励以服务端核验为准。','warn')}`);return;}
    if(name==='run-plan'){
      const row=state.planRows?.[Number(element.dataset.planIndex)],task=planRunTask(row);
      if(!task){toast('任务目录本轮不可用，无法确定执行账号；可先刷新。');return;}
      confirmRun(task,element);return;
    }
    if(name==='edit-plan-time'){const row=state.planRows?.[Number(element.dataset.planIndex)];if(row)await openPlanEditor(row.plan);return;}
    if(name==='review-submission'){openDialog('核对未确认的提交',`<form id="review-form" class="form"><p>请先检查任务日志和官方服务状态。解除保护不会重试或取消原请求。</p><label class="check"><input type="checkbox" required>我已核对上次提交，确认不会重复执行已有操作</label><div class="actions">${button('close','返回')}<button type="submit">解除提交保护</button></div></form>`);$('#review-form').onsubmit=e=>{e.preventDefault();sessionStorage.removeItem('loomgate.unconfirmedRun');closeDialog();paintTaskGuard();};return;}
    if(name==='toggle-schedule'){await toggleSchedule(index,element);return;}
    if(name==='cancel-run'){const r=state.runs[index];await mutate('cancel-'+r.id,'workbench/cancel',{id:r.id},async data=>{toast(data.cancel_requested?'已发送取消请求，等待服务端确认。':'任务已结束或无法取消，请查看最新状态。');await reload();});return;}
    if(name==='run-detail'){const r=state.runs[index];openDialog('执行详情',`<h3>${esc(taskLabel(r.provider_id,'',r.task_key))}</h3><p class="muted">${esc(providerName(r.provider_id))} · ${esc(r.account_ids?.join('、')||'全部已启用账号')}</p><p class="small muted"><code>${esc(r.id)}</code></p><div class="section-gap">${badge(r.status)}</div>${r.status==='uncertain'?notice('上游操作结果尚未确认，请先核验服务端状态，不要重复执行。','warn'):''}${runDiscoveryIssues(r.discovery_issues)}<div class="section-gap">${r.steps?.length?(()=>{const columns=runStepColumns(r.steps),withTask=columns.length===4;return table(columns,r.steps.map(s=>runStepCells(s,withTask)));})():empty('尚无执行步骤','任务可能仍在等待派发。')}</div>`);return;}
    if(name==='copy-endpoint'){const value=element.dataset.value||'';try{await navigator.clipboard.writeText(value);toast('接入地址已复制。');}catch{const box=document.getElementById('endpoint-base');if(box){const range=document.createRange();range.selectNodeContents(box);const sel=window.getSelection();sel.removeAllRanges();sel.addRange(range);}toast('已选中地址，请按 Ctrl+C 复制。');}return;}
    if(name==='logs-prev' || name==='logs-next'){state.logOffset=Math.max(0,(state.logOffset || 0)+(name==='logs-next'?30:-30));await reload();return;}
    if(name==='request-expand'){toggleRequestRow(index);return;}
    if(name==='usage-range'){state.usageRange={from:element.dataset.from||'',to:element.dataset.to||''};await reload();return;}
    if(name==='usage-apply'){const from=document.getElementById('usage-from')?.value||'',to=document.getElementById('usage-to')?.value||'';if(from&&to&&from>to){toast('起始日期不能晚于结束日期。');return;}state.usageRange={from,to};await reload();return;}
    if(name==='usage-reset'){state.usageRange={from:'',to:''};await reload();return;}
    if(name==='lan-settings'){await lanSettingsDialog(element);return;}
    if(name==='loop-toggle'){
      const card=element.closest('#setting-loop');
      const want=element.dataset.stopOnly!=='true';
      await mutate('loop','features/reasoning-loop',{stop_only:want},async()=>{
        const saved=await api('features/reasoning-loop',{signal:null});
        if(saved?.stop_only!==want)throw new APIError('request_failed',502);
        if(card?.isConnected)paintLoopSetting(card,saved.stop_only);
        toast('重复推理保护已从服务端核验。');
      },element);return;
    }
    if(name==='update-status'){await readUpdateStatus($('#setting-update'),state.version);return;}
    if(name==='update-apply'){await updateApplyDialog();return;}
    if(name==='update-review'){await reviewUpdateIntent();return;}
    if(name==='update-check'){
      if(state.pending.has('runtime-update')||readUpdateIntent()){toast('有未核验的更新提交，请使用刷新状态。');return;}
      const card=element.closest('#setting-update'),generation=state.version;
      await mutate('update-check','update/check',{},async data=>{
        if(!data.status&&data.ok!==false)throw new APIError('request_failed',502);
        if(card?.isConnected)paintUpdateCard(card,data);
        if(data.ok===false)throw new APIError('request_failed',400,typeof data.message==='string'?data.message:'更新检查失败。');
        toast('已检查更新。');
      },element,{allowNotOK:true,isCurrent:()=>card?.isConnected&&generation===state.version});return;
    }
    if(name==='key-new'){await keyNewDialog();return;}
    if(name==='key-edit'){await keyEditDialog(element.dataset.id);return;}
    if(name==='key-copy'){
      const id=element.dataset.id;
      openDialog('显示密钥',`<div data-key-reveal><p role="status">正在读取密钥；关闭或打开其他窗口后不会显示迟到的结果。</p><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}</div></div>`);
      const owner=$('[data-key-reveal]');
      await mutate('key-copy:'+id,'keys/copy',{id},async data=>{
        if(!ownsDialog(owner))return;
        if(!data.key)throw new APIError('request_failed',502,'服务端没有返回密钥。');
        openDialog('显示密钥',`<p>此密钥只在此刻显示。关闭后可再次点击「显示密钥」。</p><label>密钥<textarea readonly rows="3"></textarea></label><div class="actions">${button('close','关闭')}</div>`);
        $('#dialog-body textarea').value=String(data.key);
      });return;
    }
    if(name==='key-toggle'){
      const id=element.dataset.id,want=element.dataset.enabled!=='1';
      const rows=[...document.querySelectorAll('[data-key-row]')],row=rows.find(candidate=>candidate.dataset.keyRow===id)?.closest('tr'),focused=document.activeElement===element;
      await mutateKey(id,'keys/update',{id,enabled:want},async()=>{
        const saved=keyList(await api('keys',{signal:null})).find(key=>key.id===id);
        if(!saved||saved.enabled!==want)throw new APIError('request_failed',502);
        state.keys=state.keys.map(key=>key.id===id?saved:key);
        if(row?.isConnected){
          row.querySelector('[data-key-enabled]').innerHTML=badge(saved.enabled?'ready':'disabled',saved.enabled?'启用':'停用');
          const toggle=row.querySelector('[data-action="key-toggle"]');
          if(toggle){toggle.dataset.enabled=saved.enabled?'1':'0';toggle.textContent=saved.enabled?'停用':'启用';}
        }
        if(focused&&element.isConnected)element.focus({preventScroll:true});
        toast(want?'密钥已启用。':'密钥已停用。');
      });return;
    }
    if(name==='key-delete'){
      const id=element.dataset.id,row=keyRow(id),generation=state.version;
      openDialog('删除 API 密钥',`<form id="key-delete-form" class="form"><p>删除后使用该密钥的客户端会立即收到鉴权失败，且不可恢复。</p><label class="check"><input type="checkbox" required>我确认要删除这把密钥</label><p id="form-message" class="notice error" hidden></p><div class="actions">${button('close','取消')}<button type="submit" class="danger">删除</button></div></form>`);
      const form=$('#key-delete-form');
      form.onsubmit=e=>{
        e.preventDefault();
        mutateKey(id,'keys/delete',{id},async()=>{
          if(keyList(await api('keys',{signal:null})).some(k=>k.id===id))throw new APIError('request_failed',502);
          if(generation===state.version){
            state.keys=state.keys.filter(k=>k.id!==id);
            if(row?.isConnected)row.remove();
            const list=$('#key-list');
            if(list&&!state.keys.length)list.innerHTML=empty('还没有 API 密钥','创建密钥后才能调用模型；接入地址保持不变。');
          }
          if(ownsDialog(form)){closeDialog();$('#view [data-action=key-new]')?.focus({preventScroll:true});}
          toast('密钥已删除，并从服务端读回。');
        });
      };return;
    }
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
  // 未确认提交的保护条渲染在任务页上；对话框关闭时若保护仍在，刷新任务页让
  // 用户能看到「核对后解除」。重复关闭不会重复提交任何东西。
  $('#dialog').addEventListener('close',()=>{if(['tasks','schedules','runs'].includes(state.route))paintTaskGuard();});
  document.addEventListener('click',e=>{const target=e.target.closest('[data-action]');if(target && !target.disabled)action(target).catch(err=>{if(err.name!=='AbortError')toast(err instanceof APIError?err.message:'操作未完成，请刷新状态后重试。');});});
  $('#logout').onclick=()=>mutate('logout','auth/logout',{},async()=>location.replace('login'));
  const network=()=>{$('#connection').hidden=navigator.onLine;$('#connection').textContent='当前网络离线。页面中的数据可能已过期，恢复连接后请刷新。';};window.addEventListener('offline',network);window.addEventListener('online',network);network();
  LoomConnections.init({api,esc,openDialog,toast,reload,providerName,dialogRevision:()=>state.taskLogGeneration,accountConnected:f=>{state.accountProvider=f.provider;state.providersDirty=true;}});
  window.addEventListener('hashchange',navigate);window.addEventListener('pagehide',()=>{state.controller?.abort();clearTimeout(state.poll);LoomConnections.stop();});navigate();
})();

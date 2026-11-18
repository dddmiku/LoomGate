'use strict';
// Authentication values stay in form memory and protected requests. Never log
// inputs or persist them in browser storage. Only notification flow IDs persist.
window.LoomConnections = (() => {
  let ui, timer, generation=0, current=null, pollUntil=0;
  // Per-flow save progress. 'inflight' while a commit request is pending;
  // 'unknown' when the response was lost (never auto-retried — the user gets
  // an explicit, idempotent retry button); 'saved' after confirmed success.
  const commitState=new Map();
  const labels={working:'处理中',waiting_phone:'等待手机号',waiting_code:'等待验证码',waiting_scan:'等待扫码',scanned:'已扫码，待确认',waiting_authorization:'等待官方授权',verifying:'核验会话',verified:'待保存账号',succeeded:'账号已接入',manual_action:'需要人工处理',uncertain:'结果待核验',failed:'未完成',expired:'已过期',canceled:'已取消'};
  const terminal=f=>['succeeded','expired','failed','canceled'].includes(f.status);
  const safeURL=raw=>{try{const u=new URL(raw);return u.protocol==='https:'&&!u.username&&!u.password?u.href:'';}catch{return '';}};
  const esc=x=>ui.esc(x);
  function stop(){clearTimeout(timer);generation++;current=null;}
  function init(helpers){ui=helpers;document.getElementById('dialog').addEventListener('close',stop);}
  async function list(target){
    const data=await ui.api('connections/flows');
    target.innerHTML=(data.flows || []).length?`<section class="panel section-gap"><div class="panel-title"><h2>接入进度</h2><span class="muted small">刷新和重启后可继续查看</span></div>${data.flows.slice().reverse().map(f=>`<div class="catalog-row"><div><h3>${esc(ui.providerName(f.provider))} · ${esc(labels[f.status] || f.status)}</h3><p class="muted small">${esc(f.notice || '')}</p></div><button data-connection-resume="${esc(f.id)}">查看流程</button></div>`).join('')}</section>`:'';
    target.querySelectorAll('[data-connection-resume]').forEach(b=>b.onclick=()=>show(data.flows.find(f=>f.id===b.dataset.connectionResume)));
  }
  async function open(provider,accountID='',preferredMethod='',preferredRealm=''){
    stop();const data=await ui.api('connections/providers');const providers=data.providers||[];
    if(!providers.length){ui.openDialog('添加账号','<p class="notice">暂无可用服务，请稍后刷新。</p>','account-dialog connection-drawer');return;}
    const selected=providers.some(p=>p.id===provider)?provider:providers[0]?.id;
    const radio=(name,value,label,checked,disabled=false,detail='')=>`<label class="login-choice"><input type="radio" name="${name}" value="${esc(value)}" ${checked?'checked':''} ${disabled?'disabled':''}><span><strong>${esc(label)}</strong>${detail?`<small>${esc(detail)}</small>`:''}</span></label>`;
    ui.openDialog(accountID?'重新登录账号':'添加账号',`<form id="connection-start" class="form"><p class="scope-description">选择服务与区域，完成官方授权后系统自动核验并保存账号。</p><fieldset class="login-choices"><legend>服务</legend><div id="connection-provider" class="login-choice-row">${providers.map(p=>radio('provider',p.id,ui.providerName(p.id),p.id===selected)).join('')}</div></fieldset><fieldset id="connection-realm-label" class="login-choices"><legend>区域</legend><div id="connection-realm" class="login-choice-row">${radio('realm','cn','国内版 CN',preferredRealm==='cn'||!preferredRealm)}${radio('realm','global','国际版 Global',preferredRealm==='global')}</div></fieldset><fieldset class="login-choices"><legend>登录方式</legend><div id="connection-method" class="login-methods"></div></fieldset><p id="connection-hint" class="notice"></p><p id="connection-error" class="notice error" role="alert" hidden></p><div class="actions"><button type="button" data-action="close" class="quiet">取消</button><button type="submit" id="connection-begin" class="primary">开始授权</button></div></form>`,'account-dialog connection-drawer');
    const form=document.getElementById('connection-start'),value=name=>form.querySelector(`[name="${name}"]:checked`)?.value;
    const methodReason=m=>{
      if(m.available)return m.id==='desktop'?'将读取官方客户端当前账号并复核后保存。':'授权状态由服务端确认；成功后自动核验并保存。';
      if(m.reason==='official_desktop_connection_required')return '当前网关运行环境没有可用的官方客户端连接组件，无法从这里添加豆包账号。';
      if(m.reason==='official_sdk_context_and_signer_required')return '当前网关运行环境缺少官方客户端连接组件。';
      return '当前环境暂不可用。';
    };
    const selectMethod=()=>{const p=providers.find(p=>p.id===value('provider')),methods=p?.methods||[],selectedMethod=methods.find(m=>m.id===preferredMethod&&m.available)||methods.find(m=>m.available);form.querySelector('#connection-method').innerHTML=methods.map(m=>radio('method',m.id,m.name,m.id===selectedMethod?.id,!m.available,methodReason(m))).join('')||'<p class="muted">当前环境没有可用登录方式。</p>';form.querySelector('#connection-realm-label').hidden=p?.id!=='workbuddy';const hint=form.querySelector('#connection-hint');if(selectedMethod){hint.textContent=methodReason(selectedMethod);}else if(p?.id==='doubao'){hint.textContent='豆包账号需要官方客户端（豆包账号/飞书个人版）在本机登录后，由网关复用该会话。当前服务端环境未提供该连接组件，无法添加豆包账号；这不是浏览器登录失败，也不会重发短信。';}else{hint.textContent='此运行环境尚未接好官方客户端登录组件，不能开始登录。请在安装了官方客户端的桌面运行并配置连接组件。';}form.querySelector('#connection-begin').disabled=!selectedMethod;const label=methods.find(m=>m.id===value('method'))?.name||'';form.querySelector('#connection-begin').textContent=selectedMethod?'开始'+label:'暂不可用';};
    selectMethod();form.querySelector('#connection-provider').onchange=selectMethod;form.querySelector('#connection-realm').onchange=selectMethod;
    form.onsubmit=async e=>{e.preventDefault();const b=form.querySelector('#connection-begin');if(b.disabled)return;const p=value('provider'),method=value('method'),realm=value('realm');form.querySelectorAll('input,button').forEach(x=>x.disabled=true);try{const result=await ui.api('connections/start',{body:{provider:p,method,realm:p==='workbuddy'?realm:'',account_id:p===provider?accountID:''},signal:null});if(form.isConnected)show(result.flow);}catch(error){if(form.isConnected){const box=form.querySelector('#connection-error');box.textContent=error instanceof Error?error.message:'授权流程未建立，请稍后重试。';box.hidden=false;form.querySelectorAll('input,button').forEach(x=>x.disabled=false);selectMethod();}}};
  }

  function show(flow){
    clearTimeout(timer);generation++;current=flow;pollUntil=Date.now()+90000;render(flow);schedule(flow);
  }
  function render(f){
    current=f;const link=safeURL(f.action_url),qr=safeURL(f.qr_url)||(f.provider==='workbuddy'?link:'');
    ui.openDialog(`${ui.providerName(f.provider)} · ${labels[f.status] || f.status}`,`<div class="form connection-flow"><p class="notice ${['uncertain','manual_action','failed'].includes(f.status)?'warn':''}">${esc(f.notice || labels[f.status])}</p><p class="muted small">等待截止：${esc(new Date(f.expires_at).toLocaleString('zh-CN'))}</p>${qr?'<div id="connection-qr" class="connection-qr" aria-label="官方登录二维码"></div>':''}${link?`<a class="official-link" href="${esc(link)}" target="_blank" rel="noopener noreferrer">打开官方授权页面 ↗</a>`:''}${f.input_mode!=='official'&&(f.status==='waiting_phone'||f.status==='waiting_code')?`<form id="connection-sensitive" class="form"><label>${f.status==='waiting_phone'?'国家区号与手机号':'短信验证码'}<input name="${f.status==='waiting_phone'?'phone':'code'}" type="password" inputmode="${f.status==='waiting_phone'?'tel':'numeric'}" autocomplete="off" placeholder="${f.status==='waiting_phone'?'+86 手机号':'验证码'}" required maxlength="${f.status==='waiting_phone'?64:8}"></label><button type="submit" class="primary">${f.status==='waiting_phone'?'发送一次验证码':'提交验证码'}</button></form>`:''}${f.provider==='trae'&&f.status==='waiting_authorization'?'<form id="connection-callback" class="form"><label>官方授权完成后的回调地址<input name="callback" type="password" autocomplete="off" maxlength="16384" required></label><p class="muted small">仅粘贴到此本地表单，不发送到聊天或日志。</p><button type="submit" class="primary">核验授权结果</button></form>':''}<p id="connection-progress" class="muted small" role="status"></p><p id="connection-error" class="notice error" hidden></p><div class="actions connection-actions">${f.can_verify?'<button id="connection-verify">核验账号</button>':''}${f.status==='verified'?(commitState.get(f.id)==='unknown'?'<span class="muted">保存结果未确认</span><button id="connection-retry-save" class="primary">重试保存</button>':'<span class="muted">身份已核验，正在安全保存…</span>'):''}${!terminal(f)?'<button id="connection-cancel" class="quiet">取消</button>':'<button id="connection-done" class="primary">完成</button>'}<button id="connection-refresh" class="quiet">刷新</button></div></div>`,'account-dialog connection-drawer');
    if(Number.isInteger(f.upstream_code)){const code=document.createElement('p');code.className='muted small';code.textContent='官方业务码：'+f.upstream_code+' · 不代表已定位拒绝原因';document.querySelector('#dialog-body .form').prepend(code);}
    if(qr){const target=document.getElementById('connection-qr');if(typeof window.qrcode==='function'){const code=window.qrcode(0,'M');code.addData(qr);code.make();target.innerHTML=code.createSvgTag(4,4);}else target.textContent='二维码组件不可用，请在官方页面继续。';}
    const form=document.getElementById('connection-sensitive');if(form)form.onsubmit=e=>{e.preventDefault();const input=Object.fromEntries(new FormData(form));step(f.status==='waiting_phone'?'send_code':'submit_code',input);form.reset();};
    const callback=document.getElementById('connection-callback');if(callback)callback.onsubmit=e=>{e.preventDefault();const input=Object.fromEntries(new FormData(callback));step('callback',input);callback.reset();};
    const verify=document.getElementById('connection-verify');if(verify)verify.onclick=()=>step('verify');
    if(f.input_mode==='official'&&['uncertain','manual_action','waiting_authorization'].includes(f.status)&&f.provider==='doubao'&&f.method!=='desktop'&&f.error_code!=='official_already_authenticated'){const resume=document.createElement('button');resume.id='connection-resume';resume.textContent='继续当前官方窗口';resume.onclick=()=>step('resume');document.querySelector('#dialog-body .actions').prepend(resume);}
    if(f.input_mode==='official'&&f.provider==='doubao'&&f.status==='expired'){
      const note=document.createElement('p');note.className='notice';note.textContent='如果你已在官方窗口完成登录，可核验当前会话并另行保存连接。原过期记录保留，不会重新发送短信或生成二维码。';document.getElementById('connection-progress').before(note);
      const recover=document.createElement('button');recover.id='connection-recover-session';recover.textContent='核验已完成的官方登录';recover.onclick=()=>recoverSession(f);document.querySelector('#dialog-body .actions').prepend(recover);
    }
    if(f.authentication==='existing_session'){const source=document.createElement('p');source.className='muted small';source.textContent='本次核验的是官方客户端已有会话，没有新发起登录。';document.getElementById('connection-progress').before(source);}
    if(f.status==='verified'&&!commitState.has(f.id))queueMicrotask(()=>{if(current?.id!==f.id||step.pending)return;const st=commitState.get(f.id);if(st==='inflight'||st==='unknown'||st==='saved')return;commitState.set(f.id,'inflight');void commitFlow(f);});
    const cancel=document.getElementById('connection-cancel');if(cancel)cancel.onclick=()=>step('cancel');
    const retry=document.getElementById('connection-retry-save');if(retry)retry.onclick=()=>retryCommit(f);
    const done=document.getElementById('connection-done');if(done)done.onclick=()=>{if(f.status==='succeeded')ui.accountConnected(f);document.getElementById('dialog').close();ui.reload();};
    document.getElementById('connection-refresh').onclick=refresh;
    if(['waiting_code','waiting_scan','waiting_authorization','manual_action'].includes(f.status)&&(f.method!=='desktop'||f.error_code==='desktop_login_required'))notifyOnce(f);
  }
  async function refresh(){const data=await ui.api('connections/flows');const f=data.flows.find(x=>x.id===current?.id);if(f)show(f);}
  // commitFlow saves a verified flow and resolves the two failure modes that
  // used to leave the drawer stuck on "正在安全保存…" forever (P1-06):
  //   - request failed but the save actually succeeded server-side → re-read,
  //     if status is succeeded mark saved and refresh once;
  //   - request failed and flow is still verified → mark 'unknown' and expose
  //     an explicit retry button (commit is idempotent server-side: BindingFlow).
  // No unbounded automatic retry; nothing happens after the dialog closed or a
  // different flow was opened.
  async function commitFlow(f){
    const id=f.id;
    try{
      const data=await ui.api('connections/step',{body:{id,action:'commit',input:{}},signal:null});
      if(current?.id===id)commitState.set(id,'saved');
      if(current?.id===id)render(data.flow);
    }catch{
      commitState.set(id,'unknown');
      if(current?.id!==id)return;
      try{
        const data=await ui.api('connections/flows');const latest=data.flows.find(x=>x.id===id);
        if(current?.id!==id)return;
        if(latest?.status==='succeeded'){commitState.set(id,'saved');render(latest);ui.reload();return;}
        if(latest)render(latest);
        const box=document.getElementById('connection-error');
        if(box){box.textContent='保存结果未确认。账号可能已保存；请先刷新状态，确认仍为待保存后再点“重试保存”，不会重复创建。';box.hidden=false;}
      }catch{
        const box=document.getElementById('connection-error');
        if(box&&current?.id===id){box.textContent='保存结果未确认，网络恢复后请刷新流程状态。不会自动重复保存。';box.hidden=false;}
      }
    }
  }
  async function retryCommit(f){
    if(!f||commitState.get(f.id)==='inflight')return;
    try{const data=await ui.api('connections/flows');const latest=data.flows.find(x=>x.id===f.id);if(current?.id!==f.id)return;
      if(latest?.status==='succeeded'){commitState.set(f.id,'saved');render(latest);ui.reload();return;}
      if(latest)render(latest);
    }catch{}
    if(current?.id!==f.id||commitState.get(f.id)==='inflight')return;
    commitState.set(f.id,'inflight');await commitFlow(f);
  }
  async function recoverSession(f){
    if(step.pending||current?.id!==f.id)return;step.pending=true;
    document.querySelectorAll('#dialog-body button').forEach(b=>b.disabled=true);
    try{const data=await ui.api('connections/start',{body:{provider:'doubao',method:'desktop',account_id:f.account_id||''},signal:null});if(current?.id===f.id)show(data.flow);}
    catch{if(current?.id===f.id){const box=document.getElementById('connection-error');box.textContent='会话核验结果未确认，请关闭窗口后在接入进度查看现有流程。不会自动重复发起登录。';box.hidden=false;}}
    finally{step.pending=false;if(current?.id===f.id){document.getElementById('dialog-close').disabled=false;document.getElementById('connection-refresh').disabled=false;}}
  }
  async function step(action,input={}){
    if(!current||step.pending)return;step.pending=true;clearTimeout(timer);const id=current.id;
    document.querySelectorAll('#dialog-body button').forEach(b=>b.disabled=true);
    try{const data=await ui.api('connections/step',{body:{id,action,input},signal:null});if(current?.id===id){render(data.flow);schedule(data.flow);}}
    catch{const box=document.getElementById('connection-error');if(box){box.textContent='操作未确认，请刷新流程状态。不会自动重复发送短信或提交验证码。';box.hidden=false;}}
    finally{step.pending=false;document.querySelectorAll('#dialog-body button').forEach(b=>b.disabled=false);}
  }
  function schedule(f){
    if(!['waiting_scan','scanned','verifying'].includes(f.status)&&!(f.input_mode==='official'&&['waiting_phone','waiting_code'].includes(f.status)))return;
    const box=document.getElementById('connection-progress');
    if(Date.now()>=pollUntil){if(box)box.textContent='自动检查已暂停，可刷新继续。未结束流程会保留。';return;}
    if(box)box.textContent='等待官方状态，离开弹窗会停止检查。';
    const stamp=generation,delay=Math.max(2200,new Date(f.next_poll_at).getTime()-Date.now()+100);
    timer=setTimeout(()=>{if(generation===stamp&&current?.id===f.id)step('poll');},delay);
  }
  function notifyOnce(f){
    const key='loomgate.auth-notified.'+f.id+(f.error_code==='official_mfa_required'?'.mfa':'');try{if(sessionStorage.getItem(key))return;sessionStorage.setItem(key,'1');}catch{return;}
    try{const Audio=window.AudioContext||window.webkitAudioContext;const a=new Audio(),o=a.createOscillator(),g=a.createGain();o.frequency.value=660;g.gain.value=.08;o.connect(g);g.connect(a.destination);o.start();o.stop(a.currentTime+.2);o.onended=()=>a.close();}catch{}
  }
  const clientLabels={not_configured:'尚未连接',disconnected:'连接已断开',unbound:'等待绑定当前账号',login_required:'需要官方登录',busy:'连接正忙',unsupported:'客户端版本待适配',ready:'当前账号可用',switch_required:'需切换官方账号',connection_unavailable:'等待连接恢复',disabled:'账号已停用',legacy_session:'原有会话',conflict:'绑定冲突'};
  async function client(target,onStatus){
    const read=async()=>{
      target.innerHTML='<p class="muted small" role="status">正在核验官方客户端连接…</p>';
      try{
        const data=await ui.api('workbench/client');if(!target.isConnected)return;
        target.innerHTML=`<section class="panel section-gap" aria-label="官方客户端连接"><div class="section-heading"><h2>官方客户端连接</h2><button id="client-status-refresh" class="quiet">刷新连接</button></div><p class="badge ${data.state==='ready'?'':'neutral'}" data-client-state="${esc(data.state)}">${esc(clientLabels[data.state]||'待核验')}</p><p class="muted small section-gap">${esc(data.notice||'')}</p>${data.configured?'<div class="actions section-gap"><button id="connect-official-client">连接当前官方账号</button></div>':''}<p class="muted small section-gap">登录与续期由官方客户端管理。切换账号后，只有身份匹配的绑定可用。</p></section>`;
        document.getElementById('client-status-refresh').onclick=read;
        const connect=document.getElementById('connect-official-client');if(connect)connect.onclick=()=>open('doubao','','desktop');
        onStatus?.(data.accounts||[]);
      }catch(e){if(e.name==='AbortError'||!target.isConnected)return;target.innerHTML='<p class="notice warn">客户端状态暂时读不到。请稍后重新读取账号页面。</p>';}
    };
    await read();
  }
  return {init,open,list,stop,client,clientLabels};
})();

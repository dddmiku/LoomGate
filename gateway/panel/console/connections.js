'use strict';
// Authentication values stay in form memory and protected requests. Never log
// inputs or persist them in browser storage. Only notification flow IDs persist.
window.LoomConnections = (() => {
  let ui, timer, generation=0, renderRevision=0, refreshSequence=0, current=null, view=null, activeStep=null, pollUntil=0;
  // Per-flow save progress. 'inflight' while a commit request is pending;
  // 'unknown' when the response was lost (never auto-retried — the user gets
  // an explicit, idempotent retry button); 'saved' after confirmed success.
  const commitState=new Map();
  const labels={working:'处理中',waiting_phone:'等待手机号',waiting_code:'等待验证码',waiting_scan:'等待扫码',scanned:'已扫码，待确认',waiting_authorization:'等待官方授权',verifying:'核验会话',verified:'待保存账号',succeeded:'账号已接入',manual_action:'需要人工处理',uncertain:'结果待核验',failed:'未完成',expired:'已过期',canceled:'已取消'};
  const terminal=f=>['succeeded','expired','failed','canceled'].includes(f.status);
  const safeURL=raw=>{try{const u=new URL(raw);return u.protocol==='https:'&&!u.username&&!u.password?u.href:'';}catch{return '';}};
  const esc=x=>ui.esc(x);
  function stop(){clearTimeout(timer);generation++;renderRevision++;refreshSequence++;current=null;view=null;activeStep=null;}
  function ownsFlow(id,owner){return generation===owner&&current?.id===id&&view?.isConnected&&document.getElementById('dialog').open;}
  function init(helpers){ui=helpers;document.getElementById('dialog').addEventListener('close',stop);}
  // 账号页不再渲染「接入进度」历史列表：用户明确要求去掉。未完成的登录流程
  // 仍可在重新发起时从服务端恢复，不需要一页过期/已取消的历史记录。
  async function list(){ /* intentionally empty: no 接入进度 section */ }
  async function open(provider,accountID='',preferredMethod='',preferredRealm=''){
    stop();const owner=generation,dialogOwner=ui.dialogRevision?.();
    const data=await ui.api('connections/providers');
    if(generation!==owner||dialogOwner!==ui.dialogRevision?.())return;
    const providers=data.providers||[];
    if(!providers.length){ui.openDialog('添加账号','<p class="notice">暂无可用服务，请稍后刷新。</p>','account-dialog connection-drawer');return;}
    const selected=providers.some(p=>p.id===provider)?provider:providers[0]?.id;
    const radio=(name,value,label,checked,disabled=false,detail='')=>`<label class="login-choice"><input type="radio" name="${name}" value="${esc(value)}" ${checked?'checked':''} ${disabled?'disabled':''}><span><strong>${esc(label)}</strong>${detail?`<small>${esc(detail)}</small>`:''}</span></label>`;
    ui.openDialog(accountID?'重新登录账号':'添加账号',`<form id="connection-start" class="form"><p class="scope-description">选择服务与区域，完成官方授权后系统自动核验并保存账号。</p><fieldset class="login-choices"><legend>服务</legend><div id="connection-provider" class="login-choice-row">${providers.map(p=>radio('provider',p.id,ui.providerName(p.id),p.id===selected)).join('')}</div></fieldset><fieldset id="connection-realm-label" class="login-choices"><legend>区域</legend><div id="connection-realm" class="login-choice-row">${radio('realm','cn','国内版 CN',preferredRealm==='cn'||!preferredRealm)}${radio('realm','global','国际版 Global',preferredRealm==='global')}</div></fieldset><fieldset class="login-choices"><legend>登录方式</legend><div id="connection-method" class="login-methods"></div></fieldset><p id="connection-hint" class="notice"></p><p id="connection-error" class="notice error" role="alert" hidden></p><div class="actions"><button type="button" data-action="close" class="quiet">取消</button><button type="submit" id="connection-begin" class="primary">开始授权</button></div></form>`,'account-dialog connection-drawer');
    const form=document.getElementById('connection-start'),value=name=>form.querySelector(`[name="${name}"]:checked`)?.value;
    view=form;
    const methodReason=m=>{
      if(m.available)return m.id==='desktop'?'将读取官方客户端当前账号并复核后保存。':'授权状态由服务端确认；成功后自动核验并保存。';
      if(m.reason==='official_desktop_connection_required')return '当前网关运行环境没有可用的官方客户端连接组件，无法从这里添加需要客户端的账号。';
      if(m.reason==='official_sdk_context_and_signer_required')return '当前网关运行环境缺少官方客户端连接组件。';
      return '当前环境暂不可用。';
    };
    const selectMethod=()=>{const p=providers.find(p=>p.id===value('provider')),methods=p?.methods||[],selectedMethod=methods.find(m=>m.id===preferredMethod&&m.available)||methods.find(m=>m.available);form.querySelector('#connection-method').innerHTML=methods.map(m=>radio('method',m.id,m.name,m.id===selectedMethod?.id,!m.available,methodReason(m))).join('')||'<p class="muted">当前环境没有可用登录方式。</p>';form.querySelector('#connection-realm-label').hidden=p?.id!=='workbuddy';const hint=form.querySelector('#connection-hint');if(selectedMethod){hint.textContent=methodReason(selectedMethod);}else if(p?.id==='autoclaw'){hint.textContent='AutoClaw 是纯 HTTP 模型调用：把已有的 accessToken / refreshToken 凭证导入账号池即可，不需要浏览器登录，也不需要运行任何客户端。';}else{hint.textContent='此运行环境尚未接好官方客户端登录组件，不能开始登录。请在安装了官方客户端的桌面运行并配置连接组件。';}form.querySelector('#connection-begin').disabled=!selectedMethod;const label=methods.find(m=>m.id===value('method'))?.name||'';form.querySelector('#connection-begin').textContent=selectedMethod?'开始'+label:'暂不可用';};
    selectMethod();form.querySelector('#connection-provider').onchange=selectMethod;form.querySelector('#connection-realm').onchange=selectMethod;
    form.onsubmit=async e=>{e.preventDefault();const b=form.querySelector('#connection-begin');if(b.disabled)return;const p=value('provider'),method=value('method'),realm=value('realm');form.querySelectorAll('input,button').forEach(x=>x.disabled=true);try{const result=await ui.api('connections/start',{body:{provider:p,method,realm:p==='workbuddy'?realm:'',account_id:p===provider?accountID:''},signal:null});if(form.isConnected)show(result.flow);}catch(error){if(form.isConnected){const box=form.querySelector('#connection-error');box.textContent=error instanceof Error?error.message:'授权流程未建立，请稍后重试。';box.hidden=false;form.querySelectorAll('input,button').forEach(x=>x.disabled=false);selectMethod();}}};
  }

  function show(flow){
    clearTimeout(timer);generation++;current=flow;pollUntil=Date.now()+90000;render(flow);schedule(flow);
  }
  function render(f){
    clearTimeout(timer);renderRevision++;
    current=f;const link=safeURL(f.action_url),qr=safeURL(f.qr_url)||(f.provider==='workbuddy'?link:'');
    ui.openDialog(`${ui.providerName(f.provider)} · ${labels[f.status] || f.status}`,`<div class="form connection-flow"><p class="notice ${['uncertain','manual_action','failed'].includes(f.status)?'warn':''}">${esc(f.notice || labels[f.status])}</p><p class="muted small">等待截止：${esc(new Date(f.expires_at).toLocaleString('zh-CN'))}</p>${qr?'<div id="connection-qr" class="connection-qr" aria-label="官方登录二维码"></div>':''}${link?`<a class="official-link" href="${esc(link)}" target="_blank" rel="noopener noreferrer">打开官方授权页面 ↗</a>`:''}${f.input_mode!=='official'&&(f.status==='waiting_phone'||f.status==='waiting_code')?`<form id="connection-sensitive" class="form"><label>${f.status==='waiting_phone'?'国家区号与手机号':'短信验证码'}<input name="${f.status==='waiting_phone'?'phone':'code'}" type="text" inputmode="${f.status==='waiting_phone'?'tel':'numeric'}" autocomplete="off" spellcheck="false" placeholder="${f.status==='waiting_phone'?'+86 手机号':'验证码'}" required maxlength="${f.status==='waiting_phone'?64:8}"></label>${f.status==='waiting_phone'&&f.provider==='autoclaw'?`<label>邀请码（可选）<input name="invite_code" type="text" autocomplete="off" spellcheck="false" placeholder="留空使用默认邀请码" maxlength="32"></label>`:''}<button type="submit" class="primary">${f.status==='waiting_phone'?'发送一次验证码':'提交验证码'}</button></form>`:''}${f.provider==='trae'&&f.status==='waiting_authorization'?'<form id="connection-callback" class="form"><label>官方授权完成后的回调地址<input name="callback" type="text" autocomplete="off" spellcheck="false" maxlength="16384" required></label><p class="muted small">仅粘贴到此本地表单，不发送到聊天或日志。</p><button type="submit" class="primary">核验授权结果</button></form>':''}<p id="connection-progress" class="muted small" role="status"></p><p id="connection-error" class="notice error" hidden></p><div class="actions connection-actions">${f.can_verify?'<button id="connection-verify">核验账号</button>':''}${f.status==='verified'?(commitState.get(f.id)==='unknown'?'<span class="muted">保存结果未确认</span><button id="connection-retry-save" class="primary">重试保存</button>':'<span class="muted">身份已核验，正在安全保存…</span>'):''}${!terminal(f)?'<button id="connection-cancel" class="quiet">取消</button>':'<button id="connection-done" class="primary">完成</button>'}<button id="connection-refresh" class="quiet">刷新</button></div></div>`,'account-dialog connection-drawer');
    view=document.querySelector('#dialog-body .connection-flow');
    if(Number.isInteger(f.upstream_code)){const code=document.createElement('p');code.className='muted small';code.textContent='官方业务码：'+f.upstream_code+' · 不代表已定位拒绝原因';document.querySelector('#dialog-body .form').prepend(code);}
    if(qr){const target=document.getElementById('connection-qr');if(typeof window.qrcode==='function'){const code=window.qrcode(0,'M');code.addData(qr);code.make();target.innerHTML=code.createSvgTag(4,4);}else target.textContent='二维码组件不可用，请在官方页面继续。';}
    const form=document.getElementById('connection-sensitive');if(form){
      if(f.provider==='autoclaw'&&f.status==='waiting_phone'){const phone=form.elements.namedItem('phone');phone.closest('label').firstChild.textContent='手机号';phone.placeholder='11 位大陆手机号';phone.maxLength=11;phone.pattern='1[0-9]{10}';}
      form.onsubmit=e=>{
        e.preventDefault();
        const input=Object.fromEntries(new FormData(form));
        if('invite_code' in input){input.invite_code=input.invite_code.trim();if(!input.invite_code)delete input.invite_code;}
        // A response replaces the form on success. Keep its in-memory draft
        // when the request fails so a rejected send does not erase the phone.
        step(f.status==='waiting_phone'?'send_code':'submit_code',input);
      };
    }
    const callback=document.getElementById('connection-callback');if(callback)callback.onsubmit=e=>{e.preventDefault();const input=Object.fromEntries(new FormData(callback));step('callback',input);callback.reset();};
    const verify=document.getElementById('connection-verify');if(verify)verify.onclick=()=>step('verify');
    if(f.input_mode==='official'&&['uncertain','manual_action','waiting_authorization'].includes(f.status)&&f.method!=='desktop'&&f.error_code!=='official_already_authenticated'){const resume=document.createElement('button');resume.id='connection-resume';resume.textContent='继续当前官方窗口';resume.onclick=()=>step('resume');document.querySelector('#dialog-body .actions').prepend(resume);}
    if(f.input_mode==='official'&&f.status==='expired'){
      const note=document.createElement('p');note.className='notice';note.textContent='如果你已在官方窗口完成登录，可核验当前会话并另行保存连接。原过期记录保留，不会重新发送短信或生成二维码。';document.getElementById('connection-progress').before(note);
      const recover=document.createElement('button');recover.id='connection-recover-session';recover.textContent='核验已完成的官方登录';recover.onclick=()=>recoverSession(f);document.querySelector('#dialog-body .actions').prepend(recover);
    }
    if(f.authentication==='existing_session'){const source=document.createElement('p');source.className='muted small';source.textContent='本次核验的是官方客户端已有会话，没有新发起登录。';document.getElementById('connection-progress').before(source);}
    if(f.status==='verified'&&!commitState.has(f.id)){
      const owner=generation;
      queueMicrotask(()=>{if(!ownsFlow(f.id,owner)||activeStep)return;const st=commitState.get(f.id);if(st==='inflight'||st==='unknown'||st==='saved')return;commitState.set(f.id,'inflight');void commitFlow(f);});
    }
    const cancel=document.getElementById('connection-cancel');if(cancel)cancel.onclick=()=>step('cancel');
    const retry=document.getElementById('connection-retry-save');if(retry)retry.onclick=()=>retryCommit(f);
    const done=document.getElementById('connection-done');if(done)done.onclick=()=>{if(f.status==='succeeded')ui.accountConnected(f);document.getElementById('dialog').close();ui.reload();};
    if(f.status==='succeeded'){
      const another=document.createElement('button');another.id='connection-add-another';another.className='primary';another.textContent='再添加一个';done.className='quiet';done.before(another);
      another.onclick=()=>{
        ui.accountConnected(f);
        // Reuse the successful choice, never its account_id or credentials.
        const pending=open(f.provider,'',f.method||'',f.realm||''),owner=generation;
        pending.catch(()=>{
          if(generation!==owner||!document.getElementById('dialog').open)return;
          render(f);const box=document.getElementById('connection-error');box.textContent='添加入口未读取到，请稍后重试。已保存账号不受影响。';box.hidden=false;
        });
      };
    }
    document.getElementById('connection-refresh').onclick=refresh;
    if(['waiting_code','waiting_scan','waiting_authorization','manual_action'].includes(f.status)&&(f.method!=='desktop'||f.error_code==='desktop_login_required'))notifyOnce(f);
  }
  async function refresh(){
    const id=current?.id,owner=generation,revision=renderRevision,request=++refreshSequence;
    if(!id||activeStep)return;
    const currentRead=()=>ownsFlow(id,owner)&&revision===renderRevision&&request===refreshSequence&&!activeStep;
    try{
      const data=await ui.api('connections/flows');
      if(!currentRead())return;
      const f=data.flows.find(x=>x.id===id);
      if(!f||commitState.get(id)==='saved'&&f.status!=='succeeded')return;
      // 只读刷新不更换流程所有权，否则先返回的 GET 会丢弃后返回的保存回执。
      pollUntil=Date.now()+90000;render(f);schedule(f);
    }catch{
      if(currentRead()){const box=document.getElementById('connection-error');box.textContent='未读取到流程状态，请稍后刷新。不会重复提交登录或保存。';box.hidden=false;}
    }
  }
  // commitFlow saves a verified flow and resolves the two failure modes that
  // used to leave the drawer stuck on "正在安全保存…" forever (P1-06):
  //   - request failed but the save actually succeeded server-side → re-read,
  //     if status is succeeded mark saved and refresh once;
  //   - request failed and flow is still verified → mark 'unknown' and expose
  //     an explicit retry button (commit is idempotent server-side: BindingFlow).
  // No unbounded automatic retry; nothing happens after the dialog closed or a
  // different flow was opened.
  async function commitFlow(f){
    const id=f.id,owner=generation;
    try{
      const data=await ui.api('connections/step',{body:{id,action:'commit',input:{}},signal:null});
      if(data.flow?.id!==id)throw new Error('connection_flow_mismatch');
      commitState.set(id,data.flow.status==='succeeded'?'saved':'unknown');
      if(ownsFlow(id,owner))render(data.flow);
    }catch{
      commitState.set(id,'unknown');
      if(!ownsFlow(id,owner))return;
      try{
        const data=await ui.api('connections/flows');const latest=data.flows.find(x=>x.id===id);
        if(!ownsFlow(id,owner))return;
        if(latest?.status==='succeeded'){commitState.set(id,'saved');render(latest);return;}
        if(latest)render(latest);
        const box=document.getElementById('connection-error');
        if(box){box.textContent='保存结果未确认。账号可能已保存；请先刷新状态，确认仍为待保存后再点“重试保存”，不会重复创建。';box.hidden=false;}
      }catch{
        const box=document.getElementById('connection-error');
        if(box&&ownsFlow(id,owner)){box.textContent='保存结果未确认，网络恢复后请刷新流程状态。不会自动重复保存。';box.hidden=false;}
      }
    }
  }
  async function retryCommit(f){
    const owner=generation;
    if(!f||commitState.get(f.id)==='inflight'||!ownsFlow(f.id,owner))return;
    try{
      const data=await ui.api('connections/flows');const latest=data.flows.find(x=>x.id===f.id);
      if(!ownsFlow(f.id,owner))return;
      if(latest?.status==='succeeded'){commitState.set(f.id,'saved');render(latest);return;}
      if(latest?.status!=='verified'){if(latest)render(latest);return;}
    }catch{
      if(ownsFlow(f.id,owner)){const box=document.getElementById('connection-error');box.textContent='未读取到保存状态，请先刷新；本次未重复保存。';box.hidden=false;}
      return;
    }
    if(!ownsFlow(f.id,owner)||commitState.get(f.id)==='inflight')return;
    commitState.set(f.id,'inflight');await commitFlow(f);
  }
  async function flowRequest(path,body,accept,message){
    if(!current||activeStep)return;
    const id=current.id,owner=generation;
    if(!ownsFlow(id,owner))return;
    const attempt={id,owner};activeStep=attempt;clearTimeout(timer);
    const controls=[...view.querySelectorAll('button')].map(control=>[control,control.disabled]);
    controls.forEach(([control])=>control.disabled=true);
    try{
      const data=await ui.api(path,{body,signal:null});
      if(ownsFlow(id,owner))accept(data);
    }catch(error){
      if(ownsFlow(id,owner)){
        const box=document.getElementById('connection-error');
        if(box){
          const rejected=error?.status>=400&&error.status<500&&error.status!==408;
          const rejectedText=body.action==='send_code'&&error.status===409?'验证码请求暂未受理，可能仍在冷却或有登录操作进行中。请稍后刷新状态再试。':({authentication_request_invalid:'登录输入未受理，请检查手机号与可选邀请码后重试。',authentication_not_found:'该登录流程已不存在，请重新发起。',unauthorized:'管理会话已过期，请重新登录。'})[error?.code]||'登录请求未受理，请检查输入或刷新登录状态后重试。';
          box.textContent=rejected?rejectedText:message;
          box.hidden=false;
        }
      }
    }finally{
      if(activeStep===attempt)activeStep=null;
      controls.forEach(([control,disabled])=>{if(control.isConnected)control.disabled=disabled;});
    }
  }
  async function recoverSession(f){
    if(current?.id!==f.id)return;
    await flowRequest('connections/start',{provider:f.provider||'',method:'desktop',account_id:f.account_id||''},
      data=>show(data.flow),'会话核验结果未确认，请先刷新流程状态，不会自动重新发起登录。');
  }
  async function step(action,input={}){
    if(!current)return;
    await flowRequest('connections/step',{id:current.id,action,input},
      data=>{render(data.flow);schedule(data.flow);},'操作未确认，请刷新流程状态。不会自动重复发送短信或提交验证码。');
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
  // The "official desktop client" panel was never backed by a route: there is
  // no /workbench/client handler, and neither legacy driver exposes a
  // "desktop" method, so both the status read and the connect button could
  // only fail. Everything an account needs is already shown per row (state,
  // limits, credits), so this panel is gone rather than left to 404.
  //
  // clientLabels stays: the per-account status line uses it.
  return {init,open,list,stop,clientLabels};
})();

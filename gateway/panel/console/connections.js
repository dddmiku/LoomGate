'use strict';
// Authentication values stay in form memory and protected requests. Never log
// inputs or persist them in browser storage. Only notification flow IDs persist.
window.LoomConnections = (() => {
  let ui, timer, generation=0, current=null, pollUntil=0;
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
  async function open(provider,accountID=''){
    stop();const data=await ui.api('connections/providers');const providers=data.providers || [];
    ui.openDialog(accountID?'重新登录账号':'连接一个服务',`<form id="connection-start" class="form"><label>服务<select id="connection-provider">${providers.map(p=>`<option value="${esc(p.id)}" ${p.id===provider?'selected':''}>${esc(ui.providerName(p.id))}</option>`).join('')}</select></label><label id="connection-realm-label">区域<select id="connection-realm"><option value="cn">国内版</option><option value="global">国际版</option></select></label><label>登录方式<select id="connection-method"></select></label><p id="connection-hint" class="notice"></p><p id="connection-error" class="notice error" hidden></p><div class="actions"><button type="submit" id="connection-begin" class="primary">开始接入</button></div></form>`);
    const choose=()=>{const p=providers.find(p=>p.id===document.getElementById('connection-provider').value);document.getElementById('connection-method').innerHTML=p.methods.map(m=>`<option value="${esc(m.id)}" ${!m.available?'disabled':''}>${esc(m.name)}${!m.available?' · 暂不可用':''}</option>`).join('');const method=p.methods.find(m=>m.available);if(method)document.getElementById('connection-method').value=method.id;document.getElementById('connection-begin').disabled=!method;document.getElementById('connection-realm-label').hidden=p.id!=='workbuddy';document.getElementById('connection-hint').textContent=method?'授权由官方服务完成。遇到额外验证时会保留状态，不能把等待当作登录成功。':'当前缺少官方 SDK 登录上下文，尚不能开始网关登录。请先在官方客户端完成登录，等待接入配置。';};choose();document.getElementById('connection-provider').onchange=choose;
    document.getElementById('connection-start').onsubmit=async e=>{e.preventDefault();const b=document.getElementById('connection-begin');if(b.disabled)return;b.disabled=true;const p=document.getElementById('connection-provider').value;try{const data=await ui.api('connections/start',{body:{provider:p,method:document.getElementById('connection-method').value,realm:p==='workbuddy'?document.getElementById('connection-realm').value:'',account_id:p===provider?accountID:''},signal:null});show(data.flow);}catch{document.getElementById('connection-error').textContent='接入未开始或已有未结束流程。关闭窗口，在接入进度里恢复或取消原流程。';document.getElementById('connection-error').hidden=false;}finally{if(document.getElementById('connection-begin'))document.getElementById('connection-begin').disabled=false;}};
  }
  function show(flow){
    clearTimeout(timer);generation++;current=flow;pollUntil=Date.now()+90000;render(flow);schedule(flow);
  }
  function render(f){
    current=f;const link=safeURL(f.action_url),qr=safeURL(f.qr_url);
    ui.openDialog(`${ui.providerName(f.provider)} · ${labels[f.status] || f.status}`,`<div class="form"><p class="notice ${['uncertain','manual_action','failed'].includes(f.status)?'warn':''}">${esc(f.notice || labels[f.status])}</p><p class="muted small">等待截止：${esc(new Date(f.expires_at).toLocaleString('zh-CN'))}</p>${qr?'<div id="connection-qr" class="connection-qr" aria-label="官方登录二维码"></div>':''}${link?`<a class="official-link" href="${esc(link)}" target="_blank" rel="noopener noreferrer">打开官方授权页面 ↗</a>`:''}${f.status==='waiting_phone'||f.status==='waiting_code'?`<form id="connection-sensitive" class="form"><label>${f.status==='waiting_phone'?'国家区号与手机号':'短信验证码'}<input name="${f.status==='waiting_phone'?'phone':'code'}" type="password" inputmode="${f.status==='waiting_phone'?'tel':'numeric'}" autocomplete="off" placeholder="${f.status==='waiting_phone'?'+86 手机号':'验证码'}" required maxlength="${f.status==='waiting_phone'?64:8}"></label><button type="submit" class="primary">${f.status==='waiting_phone'?'发送一次验证码':'提交验证码'}</button></form>`:''}${f.provider==='trae'&&f.status==='waiting_authorization'?'<form id="connection-callback" class="form"><label>官方授权完成后的回调地址<input name="callback" type="password" autocomplete="off" maxlength="16384" required></label><p class="muted small">仅粘贴到此本地表单，不发送到聊天或日志。</p><button type="submit" class="primary">核验授权结果</button></form>':''}<p id="connection-progress" class="muted small" role="status"></p><p id="connection-error" class="notice error" hidden></p><div class="actions">${f.status==='verified'?'<button id="connection-commit" class="primary">保存已核验账号</button>':''}${!terminal(f)?'<button id="connection-cancel">取消此次接入</button>':'<button id="connection-done">完成</button>'}<button id="connection-refresh">刷新状态</button></div></div>`);
    if(qr){const target=document.getElementById('connection-qr');if(typeof window.qrcode==='function'){const code=window.qrcode(0,'M');code.addData(qr);code.make();target.innerHTML=code.createSvgTag(4,4);}else target.textContent='二维码组件不可用，请在官方页面继续。';}
    const form=document.getElementById('connection-sensitive');if(form)form.onsubmit=e=>{e.preventDefault();const input=Object.fromEntries(new FormData(form));step(f.status==='waiting_phone'?'send_code':'submit_code',input);form.reset();};
    const callback=document.getElementById('connection-callback');if(callback)callback.onsubmit=e=>{e.preventDefault();const input=Object.fromEntries(new FormData(callback));step('callback',input);callback.reset();};
    const commit=document.getElementById('connection-commit');if(commit)commit.onclick=()=>step('commit');
    const cancel=document.getElementById('connection-cancel');if(cancel)cancel.onclick=()=>step('cancel');
    const done=document.getElementById('connection-done');if(done)done.onclick=()=>{if(f.status==='succeeded')ui.accountConnected(f);document.getElementById('dialog').close();ui.reload();};
    document.getElementById('connection-refresh').onclick=refresh;
    if(['waiting_code','waiting_scan','waiting_authorization','manual_action'].includes(f.status))notifyOnce(f);
  }
  async function refresh(){const data=await ui.api('connections/flows');const f=data.flows.find(x=>x.id===current?.id);if(f)show(f);}
  async function step(action,input={}){
    if(!current||step.pending)return;step.pending=true;clearTimeout(timer);const id=current.id;
    document.querySelectorAll('#dialog-body button').forEach(b=>b.disabled=true);
    try{const data=await ui.api('connections/step',{body:{id,action,input},signal:null});if(current?.id===id){render(data.flow);schedule(data.flow);}}
    catch{const box=document.getElementById('connection-error');if(box){box.textContent='操作未确认，请刷新流程状态。不会自动重复发送短信或提交验证码。';box.hidden=false;}}
    finally{step.pending=false;document.querySelectorAll('#dialog-body button').forEach(b=>b.disabled=false);}
  }
  function schedule(f){
    if(!['waiting_scan','scanned','verifying'].includes(f.status))return;
    const box=document.getElementById('connection-progress');
    if(Date.now()>=pollUntil){if(box)box.textContent='自动检查已暂停，可刷新继续。未结束流程会保留。';return;}
    if(box)box.textContent='等待官方状态，离开弹窗会停止检查。';
    const stamp=generation,delay=Math.max(2200,new Date(f.next_poll_at).getTime()-Date.now()+100);
    timer=setTimeout(()=>{if(generation===stamp&&current?.id===f.id)step('poll');},Math.min(8000,delay));
  }
  function notifyOnce(f){
    const key='loomgate.auth-notified.'+f.id;if(sessionStorage.getItem(key))return;sessionStorage.setItem(key,'1');
    try{const Audio=window.AudioContext||window.webkitAudioContext;const a=new Audio(),o=a.createOscillator(),g=a.createGain();o.frequency.value=660;g.gain.value=.08;o.connect(g);g.connect(a.destination);o.start();o.stop(a.currentTime+.2);o.onended=()=>a.close();}catch{}
  }
  return {init,open,list,stop};
})();

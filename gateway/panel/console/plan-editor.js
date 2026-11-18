'use strict';
// The editor is a draft until its final submit. Going back, changing service or
// closing the dialog never writes a schedule or dispatches an upstream task.
window.LoomPlanEditor = (() => {
  // crypto.randomUUID is only exposed in secure contexts. The product explicitly
  // supports plain-HTTP LAN access, where randomUUID is undefined and opening the
  // editor threw a TypeError the user only saw as "操作未完成". getRandomValues is
  // available in insecure contexts too, so build the 32-hex id ourselves.
  function planID() {
    const bytes = new Uint8Array(16);
    if (window.crypto && typeof window.crypto.getRandomValues === 'function') window.crypto.getRandomValues(bytes);
    else for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
    bytes[6] = bytes[6] & 0x0f | 0x40; // version 4
    bytes[8] = bytes[8] & 0x3f | 0x80; // variant
    let hex = '';
    for (const b of bytes) hex += b.toString(16).padStart(2, '0');
    return hex.slice(0, 12);
  }
  async function open(ui) {
    const {existing,task,providers,esc,options}=ui;
    const draft=existing?ui.cleanSchedule(existing):{id:'plan-'+planID(),provider_id:task?.provider_id||providers[0]?.id,task_key:task?.key||'',account_ids:task?(task.account_ids||[task.account_id].filter(Boolean)):[],hours:[9],minute:0,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone||'Asia/Shanghai',enabled:false,once_per_day:true};
    const fixedWork=true,firstStep=1;
    let step=firstStep,loadID=0,loaded=fixedWork,saving=false,keys=new Map(),times=draft.hours.map(h=>String(h).padStart(2,'0')+':'+String(draft.minute).padStart(2,'0'));
    const progress=fixedWork?'':`<span data-plan-progress="0" aria-current="step">选择工作</span><span data-plan-progress="1">安排时间</span><span data-plan-progress="2">确认计划</span>`;
    ui.openDialog(existing?'编辑执行计划':task?'为任务安排计划':'新建执行计划',`<form id="schedule-form" novalidate><div class="plan-progress" aria-label="编辑步骤">${progress}</div><section class="plan-step" data-plan-step="0" ${firstStep===1?'hidden':''}><h3 tabindex="-1">安排什么工作？</h3><p>选择一项工作，再决定由哪些账号执行。</p><label>服务<select id="plan-provider">${options(providers.map(p=>[p.id,p.name]),draft.provider_id)}</select></label><label>任务<select id="plan-task" disabled><option value="">正在发现任务…</option></select></label><div class="account-summary">执行账号，至少选择一个</div><div id="plan-accounts" class="account-checks">正在读取账号…</div></section><section class="plan-step" data-plan-step="1" ${firstStep===0?'hidden':''}><h3 tabindex="-1">什么时候执行？</h3><p>${task?`为「${esc(task.name||task.key)}」安排时间；任务和账号范围已从任务中心带入。`:existing?.task_key?'调整原计划的执行时间；任务和账号范围保持不变。':'添加每天运行的时间。任务自身的每日或一次性规则仍然生效。'}</p><div id="plan-times" class="time-list"></div><details class="plan-advanced"><summary>时区与重复规则</summary><div><label>时区<input id="plan-timezone" value="${esc(draft.timezone)}" list="timezones" required><datalist id="timezones"><option>Asia/Shanghai</option><option>UTC</option><option>America/New_York</option><option>Europe/London</option></datalist></label><label class="check"><input id="plan-daily" type="checkbox" ${draft.once_per_day?'checked':''}>同一计划每天最多执行一次</label></div></details><p id="plan-zone-summary" class="small muted"></p><label class="check"><input id="plan-enabled-quick" type="checkbox" ${draft.enabled?'checked':''}>保存后启用</label><p class="detail-note">未启用的计划只保存，不会派发任务。启用计划仍受任务自身规则约束。</p></section><section class="plan-step" data-plan-step="2" hidden><h3 tabindex="-1">确认这项计划</h3><p>核对时间与范围，保存后会从服务端重新读取确认。</p><div id="plan-review"></div><label class="check"><input id="plan-enabled" type="checkbox" ${draft.enabled?'checked':''}>保存后启用</label><p class="detail-note">未启用的计划只保存，不会派发任务。启用计划仍受任务自身规则约束。</p></section><p id="form-message" class="notice error" role="alert" hidden></p><div class="plan-footer"><button id="plan-back" class="quiet" type="button">取消</button><div class="actions"><button id="plan-next" class="primary" type="button" ${fixedWork?'hidden':''} disabled>继续</button><button id="plan-save" class="primary" type="submit" ${fixedWork?'':'hidden'}>保存计划</button></div></div></form>`);
    const form=document.getElementById('schedule-form'),dialog=document.getElementById('dialog');
    dialog.classList.add('plan-dialog');
    const $=selector=>form.querySelector(selector),alive=()=>form.isConnected&&dialog.open;
    const error=text=>{if(!alive())return;$('#form-message').textContent=text;$('#form-message').hidden=false;};
    const clearError=()=>{$('#form-message').hidden=true;};
    const readTimes=()=>[...form.querySelectorAll('input[name=plan-time]')].map(i=>i.value);
    function paintTimes(){
      $('#plan-times').innerHTML=times.map((time,i)=>`<div class="time-item"><input type="time" name="plan-time" aria-label="执行时间 ${i+1}" value="${esc(time)}" required><button type="button" data-remove-time="${i}" aria-label="移除时间 ${i+1}" ${times.length===1?'disabled':''}>×</button></div>`).join('')+`<button id="plan-add-time" type="button" class="quiet" ${times.length>=24?'disabled':''}>添加时段</button>`;
      $('#plan-add-time').onclick=()=>{times=readTimes();const used=new Set(times.map(t=>t.split(':')[0]));let h=18;while(used.has(String(h).padStart(2,'0'))&&h<24)h++;if(h===24)h=Array.from({length:24},(_,i)=>i).find(i=>!used.has(String(i).padStart(2,'0')));times.push(String(h).padStart(2,'0')+':'+(times[0]?.split(':')[1]||'00'));paintTimes();form.querySelectorAll('[name=plan-time]').item(times.length-1).focus();};
      form.querySelectorAll('[data-remove-time]').forEach(button=>button.onclick=()=>{times=readTimes();times.splice(Number(button.dataset.removeTime),1);paintTimes();$('#plan-add-time').focus();});
    }
    function readWork(){
      if(fixedWork)return Boolean(draft.provider_id&&draft.task_key);
      if(!loaded){error('任务目录尚未读取完成，请稍后重试。');return false;}
      const ids=[...form.querySelectorAll('input[name=plan-account]:checked')].map(i=>i.value),key=$('#plan-task').value;
      if(!keys.has(key)||!ids.length){error('请选择有效任务，并至少选择一个账号。');return false;}
      draft.provider_id=$('#plan-provider').value;draft.task_key=key;draft.account_ids=ids;return true;
    }
    function readTiming(){
      times=readTimes();
      if(!times.length||times.some(t=>!/^([01]\d|2[0-3]):[0-5]\d$/.test(t))||new Set(times).size!==times.length){error('请填写有效且不重复的执行时间。');return false;}
      const minutes=times.map(t=>Number(t.split(':')[1]));
      if(new Set(minutes).size!==1){error('同一计划的多个时段需使用相同分钟。不同分钟请分别创建计划。');return false;}
      const zone=$('#plan-timezone').value.trim();
      try{new Intl.DateTimeFormat('zh-CN',{timeZone:zone}).format();}catch{error('时区无效，请选择有效时区，例如 Asia/Shanghai。');return false;}
      if(!zone){error('请选择执行时区。');return false;}
      draft.hours=times.map(t=>Number(t.split(':')[0])).sort((a,b)=>a-b);draft.minute=minutes[0];draft.timezone=zone;draft.once_per_day=$('#plan-daily').checked;return true;
    }
    function paintStep(){
      if(step>=1&&!fixedWork&&!readTiming())step=0;
      form.querySelectorAll('[data-plan-step]').forEach(x=>x.hidden=Number(x.dataset.planStep)!==step);
      form.querySelectorAll('[data-plan-progress]').forEach(x=>{if(Number(x.dataset.planProgress)===step)x.setAttribute('aria-current','step');else x.removeAttribute('aria-current');});
      $('#plan-back').textContent=step>firstStep?'上一步':'取消';$('#plan-next').hidden=fixedWork||step===2;$('#plan-save').hidden=!fixedWork&&step!==2;$('#plan-next').disabled=!loaded;
      if(step===2){$('#plan-enabled').checked=draft.enabled;}
      if(fixedWork){const q=$('#plan-enabled-quick');if(q)q.checked=draft.enabled;}
      $('#plan-zone-summary').textContent='当前时区：'+$('#plan-timezone').value;
      if(step===2){
        const provider=providers.find(p=>p.id===draft.provider_id)?.name||draft.provider_id;
        $('#plan-review').innerHTML=`<p class="plan-preview">每天 <strong>${draft.hours.map(h=>String(h).padStart(2,'0')+':'+String(draft.minute).padStart(2,'0')).join('、')}</strong>，为 <strong>${draft.account_ids.length} 个账号</strong> 执行 <strong>${esc(keys.get(draft.task_key))}</strong>。</p><dl class="plan-review"><div><dt>服务</dt><dd>${esc(provider)}</dd></div><div><dt>账号</dt><dd>${esc(draft.account_ids.join('、'))}</dd></div><div><dt>时区</dt><dd>${esc(draft.timezone)}</dd></div><div><dt>重复规则</dt><dd>${draft.once_per_day?'同一天最多执行一次':'每个时段都执行'}</dd></div></dl>`;
      }
      document.getElementById('dialog-body').scrollTo(0,0);
      form.querySelector(`[data-plan-step="${step}"] h3`).focus({preventScroll:true});
    }
    async function load(){
      if(fixedWork){const selected=task?.name||task?.key||existing?.task_key||draft.task_key;keys.set(draft.task_key,selected);paintStep();return;}
      const id=++loadID,provider=$('#plan-provider').value;loaded=false;$('#plan-next').disabled=true;$('#plan-task').disabled=true;
      clearError();
      try{
        const [accounts,data]=await Promise.all([ui.accountsFor(provider),ui.api('workbench/catalog?'+new URLSearchParams({provider}))]);
        if(!alive()||id!==loadID)return;
        keys=new Map((data.tasks||[]).filter(t=>!t.manual_only).map(t=>[t.key,t.name]));
        $('#plan-task').innerHTML=options([['','选择一个任务'],...keys.entries()],provider===draft.provider_id?draft.task_key:'');$('#plan-task').disabled=false;
        $('#plan-accounts').innerHTML=accounts.filter(a=>a.enabled||(provider===draft.provider_id&&draft.account_ids.includes(a.id))).map(a=>`<label class="check"><input type="checkbox" name="plan-account" value="${esc(a.id)}" ${provider===draft.provider_id&&draft.account_ids.includes(a.id)?'checked':''}>${esc(a.id)}${a.enabled?'':'（已停用）'}</label>`).join('')||'<p class="small muted">没有可选账号，请先连接账号。</p>';
        loaded=keys.size>0&&accounts.length>0;$('#plan-next').disabled=!loaded;
        if(existing&&provider===draft.provider_id&&!keys.has(draft.task_key))error('原任务当前不可发现，请重新选择。未保存前原计划保持不变。');
      }catch(e){if(e.name!=='AbortError'&&alive()&&id===loadID)error('任务或账号读取失败，请重新选择服务或关闭后重试。');}
    }
    $('#plan-provider').onchange=load;
    $('#plan-back').onclick=()=>{if(step===firstStep){ui.closeDialog();return;}clearError();step--;paintStep();};
    $('#plan-next').onclick=()=>{clearError();if(step===0&&!readWork()||step===1&&!readTiming())return;step++;paintStep();};
    $('#plan-timezone').oninput=()=>$('#plan-zone-summary').textContent='当前时区：'+$('#plan-timezone').value;
    form.onsubmit=async event=>{
      event.preventDefault();if(!fixedWork&&step!==2){$('#plan-next').click();return;}
      if(saving)return;
      clearError();if(!readWork()||!readTiming())return;
      const enabled=fixedWork?$('#plan-enabled-quick')?.checked:$('#plan-enabled').checked;const payload={...draft,enabled:Boolean(enabled)};
      saving=true;
      const controls=[...form.querySelectorAll('button,input,select')].map(e=>[e,e.disabled]);
      controls.forEach(([e])=>e.disabled=true);
      try {await ui.mutate('schedule','workbench/schedule',payload,async()=>{await ui.verifySchedule(payload);ui.closeDialog();ui.toast('计划已保存，并已从服务端核验。');await ui.after();});}
      finally {saving=false;if(alive())controls.forEach(([e,disabled])=>e.disabled=disabled);}
    };
    paintTimes();paintStep();await load();
  }
  return {open,planID};
})();

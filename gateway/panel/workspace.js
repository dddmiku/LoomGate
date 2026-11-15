"use strict";
var LW={providers:[],schedules:[],runs:[],tasks:[],models:[],offset:0,epoch:0};
var LP={workbuddy:'WorkBuddy',trae:'Trae',doubao:'豆包'};
var LSTATUS={queued:'排队中',running:'执行中',succeeded:'已核实完成',failed:'失败',canceled:'已取消',uncertain:'结果待核实',skipped:'已跳过',manual_action:'需要人工操作'};
function loomEmpty(title,text){return '<div class="loom-state"><strong>'+esc(title)+'</strong>'+esc(text)+'</div>';}
function loomNotice(id,text){var el=$('#'+id);if(el)el.innerHTML=text?'<div class="loom-notice" role="alert">'+esc(text)+'</div>':'';}
async function loomState(){var d=await api('api/workbench');LW.providers=d.providers||[];LW.schedules=d.schedules||[];LW.runs=d.runs||[];return d;}
async function loomRoute(view){
  if(typeof catalogCancel==='function')catalogCancel();
  var generation=++LW.epoch;var map={providers:'providersNotice',models:'modelsNotice',schedules:'schedulesNotice',executions:'executionsNotice',tasks:'definitionsNotice',migration:'migrationNotice'};
  if(!map[view])return;
  loomNotice(map[view],'');
  try{
    if(view==='models'){await loomModels(false);return;}
    await loomState();if(generation!==LW.epoch)return;
    if(view==='providers')await loomProviders();if(view==='schedules')loomSchedules();if(view==='executions')loomExecutions();if(view==='tasks')await loomTasks();if(view==='migration')loomMigration();
  }catch(e){loomNotice(map[view],e.status===404?'当前服务尚未启用新的任务工作台。旧版服务能力仍可从账号与系统页面查看。':'无法加载当前数据，请检查服务连接后重试。');}
}
async function loomTasks(){
  var results=await Promise.all(LW.providers.map(async function(p){try{var d=await api('api/workbench/catalog?provider='+encodeURIComponent(p));return {tasks:d.tasks||[]};}catch(e){return {tasks:[],failed:p};}}));
  LW.tasks=[];var failed=[];results.forEach(function(r){LW.tasks=LW.tasks.concat(r.tasks);if(r.failed)failed.push(LP[r.failed]||r.failed);});
  if(failed.length)loomNotice('definitionsNotice',failed.join('、')+' 的任务目录暂时不可用，请检查账号状态后刷新。');
  $('#definitionGrid').innerHTML=LW.tasks.map(function(t,i){return '<article class="loom-card"><div class="loom-tags"><span class="loom-tag">'+esc(LP[t.provider_id]||t.provider_id)+'</span><span class="loom-tag">'+esc(t.once?'一次性':t.once_per_day?'每日':'上游状态驱动')+'</span></div><h3>'+esc(t.name||t.key)+'</h3><code>'+esc(t.key)+'</code><p>'+esc(t.description||'执行前查询服务端状态，领取后再次核实。')+'</p><p>'+esc(t.reward||'奖励以服务端回执为准')+'</p><div class="card-actions"><button class="btn pri" data-loom-run="'+i+'">执行并核实</button></div></article>';}).join('')||loomEmpty('还没有可执行任务','登录服务账号后刷新；目录来自账号当前的真实活动。');
}
function loomSchedules(){
  $('#scheduleGrid').innerHTML=LW.schedules.map(function(s,i){return '<article class="loom-card"><div class="loom-tags"><span class="loom-tag '+(s.enabled?'good':'')+'">'+(s.enabled?'已启用':'已停用')+'</span><span class="loom-tag">'+esc(LP[s.provider_id]||s.provider_id)+'</span></div><h3>'+esc(s.id)+'</h3><p>'+esc(s.hours.map(function(h){return pad(h)+':'+pad(s.minute);}).join(' · '))+'</p><code>'+esc(s.timezone)+'</code><p>下次运行 '+esc(fmtTime(s.next_run))+'<br>账号范围 '+(s.account_ids.length?s.account_ids.length+' 个指定账号':'全部可用账号')+'</p><div class="card-actions"><button class="btn" data-loom-schedule="'+i+'">编辑</button><button class="btn" data-loom-toggle="'+i+'">'+(s.enabled?'停用':'启用')+'</button><button class="btn danger" data-loom-delete="'+i+'">删除</button></div></article>';}).join('')||loomEmpty('创建第一条调度计划','配置时间、时区与账号范围，保存后立即生效。');
}
function loomScheduleEditor(s){
  s=s||{id:'schedule-'+Date.now(),provider_id:LW.providers[0]||'',task_key:'*',enabled:true,hours:[9],minute:0,timezone:'Asia/Shanghai',account_ids:[],once_per_day:true};
  var d=document.createElement('dialog');d.className='loom-dialog';d.innerHTML='<form><h2>'+esc(LW.schedules.some(function(x){return x.id===s.id;})?'编辑计划':'新建计划')+'</h2><div class="loom-fields"><label>计划名称<input name="id" required pattern="[A-Za-z0-9](?:[A-Za-z0-9._:]|-){0,127}" value="'+esc(s.id)+'"></label><label>服务<select name="provider_id">'+LW.providers.map(function(p){return '<option value="'+esc(p)+'"'+(p===s.provider_id?' selected':'')+'>'+esc(LP[p]||p)+'</option>';}).join('')+'</select></label><label>执行小时<input name="hours" required value="'+esc(s.hours.join(','))+'"><small>0–23，多个小时用逗号分隔</small></label><label>分钟<input name="minute" type="number" min="0" max="59" value="'+s.minute+'" required></label><label>时区<input name="timezone" value="'+esc(s.timezone)+'" required></label><label>任务<select name="task_key"><option value="*">全部动态任务</option>'+LW.tasks.filter(function(t){return t.provider_id===s.provider_id;}).map(function(t){return '<option value="'+esc(t.key)+'"'+(t.key===s.task_key?' selected':'')+'>'+esc(t.name||t.key)+'</option>';}).join('')+(s.task_key!=='*'&&!LW.tasks.some(function(t){return t.key===s.task_key;})?'<option selected value="'+esc(s.task_key)+'">'+esc(s.task_key)+'</option>':'')+'</select></label><label class="full">账号标识范围<input name="accounts" value="'+esc(s.account_ids.join(','))+'"><small>留空为该服务全部可用账号；多个标识用逗号分隔</small></label><label><span><input type="checkbox" name="enabled" '+(s.enabled?'checked':'')+'> 启用计划</span></label><label><span><input type="checkbox" name="daily" '+(s.once_per_day?'checked':'')+'> 每个业务日最多派发一次</span></label></div><div class="loom-notice hide" role="alert"></div><footer><button type="button" class="btn" data-close>取消</button><button class="btn pri" type="submit">保存计划</button></footer></form>';
  document.body.appendChild(d);d.showModal();d.addEventListener('close',function(){d.remove();});$('[data-close]',d).onclick=function(){d.close();};$('form',d).onsubmit=async function(ev){ev.preventDefault();var f=new FormData(ev.target),btn=$('[type=submit]',d);btn.disabled=true;var out={id:f.get('id'),provider_id:f.get('provider_id'),task_key:f.get('task_key'),enabled:f.has('enabled'),hours:String(f.get('hours')).split(',').map(Number),minute:Number(f.get('minute')),timezone:f.get('timezone'),account_ids:String(f.get('accounts')).split(',').map(function(x){return x.trim();}).filter(Boolean),once_per_day:f.has('daily')};try{await api('api/workbench/schedule',out);d.close();await loomRoute('schedules');toast('计划已保存并生效','ok');}catch(e){var n=$('.loom-notice',d);n.textContent='保存失败，请检查时间、时区和账号标识后重试。';n.classList.remove('hide');btn.disabled=false;}};
}
function loomExecutions(){var filter=$('#executionStatus').value,rows=LW.runs.filter(function(r){return !filter||r.status===filter;}),slice=rows.slice(LW.offset,LW.offset+20);$('#executionRows').innerHTML=slice.map(function(r){return '<tr><td>'+esc(r.task_key||'全部任务')+'<br><code>'+esc(r.id.slice(0,12))+'</code></td><td>'+esc(LP[r.provider_id]||r.provider_id)+'<br><small>'+esc(r.trigger)+'</small></td><td><span class="loom-tag '+(r.status==='succeeded'?'good':'wait')+'">'+esc(LSTATUS[r.status]||r.status)+'</span></td><td>'+esc(fmtTime(r.started_at))+'</td><td>'+esc(r.stage||'—')+'</td><td>'+esc(r.error_code||'—')+'</td><td><button class="btn sm" data-loom-detail="'+esc(r.id)+'">详情</button>'+(['queued','running'].indexOf(r.status)>=0?'<button class="btn sm danger" data-loom-cancel="'+esc(r.id)+'">取消</button>':'')+'</td></tr>';}).join('')||'<tr><td colspan="7">'+loomEmpty('暂无执行记录','手动执行或调度触发后，这里会记录每一步和最终结果。')+'</td></tr>';$('#executionRange').textContent=rows.length+' 条记录';$('#executionsPrev').disabled=LW.offset===0;$('#executionsNext').disabled=LW.offset+20>=rows.length;}
function loomMigration(){ $('#migrationContent').innerHTML=loomEmpty('独立工作区已启用','当前运行使用独立的数据目录。迁移工具支持 dry-run、复制校验、中断恢复和回滚；真实数据迁移须完成副本验收。'); }
function loomSettings(kind){
  var root=$('.view[data-view="system"]');
  $$('[data-settings]',root).forEach(function(b){b.setAttribute('aria-selected',String(b.dataset.settings===kind));});
  $$('.panel',root).forEach(function(p){var title=$('h2',p),name=title?title.textContent:'';var group=(name==='登录账号'||name==='局域网访问')?'security':name==='版本与热更新'?'updates':'service';p.classList.toggle('hide',group!==kind);});
  $('.pairs',root).style.display='block';
}
document.addEventListener('click',function(ev){var b=ev.target.closest('[data-settings]');if(b)loomSettings(b.dataset.settings);});
loomSettings('service');
document.addEventListener('click',async function(ev){var b=ev.target.closest('[data-loom-add],[data-loom-run],[data-loom-schedule],[data-loom-toggle],[data-loom-delete],[data-loom-cancel],[data-loom-detail]');if(!b)return;try{
  if(b.hasAttribute('data-loom-add')){$('#btnAdd').click();return;}
  if(b.hasAttribute('data-loom-schedule')){loomScheduleEditor(LW.schedules[Number(b.dataset.loomSchedule)]);return;}
  if(b.hasAttribute('data-loom-detail')){var r=LW.runs.find(function(x){return x.id===b.dataset.loomDetail;}),d=document.createElement('dialog');d.className='loom-dialog';d.innerHTML='<div class="loom-detail"></div><form method="dialog"><button class="btn">关闭</button></form>';$('div',d).textContent=JSON.stringify(r,null,2);document.body.appendChild(d);d.showModal();d.addEventListener('close',function(){d.remove();});return;}
  b.disabled=true;
  if(b.hasAttribute('data-loom-run')){var t=LW.tasks[Number(b.dataset.loomRun)];await api('api/workbench/run',{provider_id:t.provider_id,task_key:t.key,account_ids:[t.account_id]});go('executions');}
  if(b.hasAttribute('data-loom-toggle')){var s=Object.assign({},LW.schedules[Number(b.dataset.loomToggle)]);s.enabled=!s.enabled;await api('api/workbench/schedule',s);await loomRoute('schedules');}
  if(b.hasAttribute('data-loom-delete')){var s=LW.schedules[Number(b.dataset.loomDelete)];if(window.confirm('删除计划“'+s.id+'”？已产生的执行记录会保留。')){await api('api/workbench/schedule/delete',{id:s.id});await loomRoute('schedules');}}
  if(b.hasAttribute('data-loom-cancel')){await api('api/workbench/cancel',{id:b.dataset.loomCancel});toast('已请求取消，最终状态以执行记录为准');await loomRoute('executions');}
 }catch(e){toast('操作未完成，请刷新状态后重试。','err');}finally{b.disabled=false;}});
['providers','schedules','migration'].forEach(function(v){$('#'+v+'Reload').onclick=function(){loomRoute(v);};});
$('#definitionsReload').onclick=function(){loomRoute('tasks');};$('#scheduleNew').onclick=function(){loomScheduleEditor();};$('#executionFilters').onsubmit=function(e){e.preventDefault();LW.offset=0;loomRoute('executions');};$('#executionsPrev').onclick=function(){LW.offset=Math.max(0,LW.offset-20);loomExecutions();};$('#executionsNext').onclick=function(){LW.offset+=20;loomExecutions();};

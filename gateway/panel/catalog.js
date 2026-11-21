"use strict";
// Catalogs are management metadata. A discovered desktop model is not exposed
// as an inference route until that provider's transport is implemented.
var LC = {generation:0, controller:null, providers:[], accounts:[], rows:[], offset:0,
  total:0, hasMore:false, loading:false, error:'', initialized:false};
var CSTATE={ready:'目录已同步',stale:'旧缓存',unavailable:'目录不可用',disabled:'账号已停用',loading:'正在同步'};
var CERROR={login_required:'登录状态已失效，需要在官方客户端重新登录。',catalog_timeout:'目录请求超时，请稍后重试。',catalog_failed:'上游目录暂时不可用，请稍后重试。',catalog_empty:'上游尚未返回可用模型。',catalog_limit:'上游目录超出安全读取上限。',accounts_unavailable:'账号存储暂时无法读取。'};
function catalogCancel(){LC.generation++;if(LC.controller)LC.controller.abort();LC.controller=null;LC.loading=false;}
async function catalogAPI(path,body,signal){
  var opt={cache:'no-store',headers:{'Content-Type':'application/json'},signal:signal};
  if(body!==undefined){opt.method='POST';opt.headers['X-Admin-Request']='1';opt.body=JSON.stringify(body);}
  var res=await fetch(path,opt);if(res.status===401){location.replace('login');throw new Error('session_expired');}
  var data=await res.json();if(!res.ok||data.ok===false){var e=new Error('catalog_request_failed');e.status=res.status;throw e;}return data;
}
async function catalogMetadata(signal){
  var results=await Promise.allSettled([catalogAPI('api/workbench/providers',undefined,signal),catalogAPI('api/workbench/accounts',undefined,signal)]);
  if(results[0].status!=='fulfilled')throw results[0].reason;
  return {providers:results[0].value.providers||[],accounts:results[1].status==='fulfilled'?(results[1].value.accounts||[]):[],accountError:results[1].status!=='fulfilled'};
}
function catalogOptions(){
  var provider=$('#modelProvider'),account=$('#modelAccount'),selected=provider.value,chosen=account.value;
  provider.innerHTML='<option value="">全部服务</option>'+LC.providers.map(function(p){return '<option value="'+esc(p.id)+'">'+esc(p.name)+'</option>';}).join('');
  if(LC.providers.some(function(p){return p.id===selected;}))provider.value=selected;
  var options=LC.accounts.filter(function(a){return !provider.value||a.provider===provider.value;});
  account.innerHTML='<option value="">全部账号</option>'+options.map(function(a){return '<option value="'+esc(a.id)+'">'+esc(a.id)+(a.enabled?'':' · 已停用')+'</option>';}).join('');
  if(options.some(function(a){return a.id===chosen;}))account.value=chosen;
  var chosenProvider=LC.providers.find(function(p){return p.id===provider.value;});
  account.disabled=!(chosenProvider&&chosenProvider.catalog_scope==='account');
}
// 聚合目录 /api/models 里每个 provider 的命名空间是确定的：WorkBuddy 用
// cn:/global:，Trae 用 trae:。账号级目录的 provider（scope=account）走
// /api/workbench/models，按账号返回并带 account_id；聚合目录里它的命名空间
// 没有账号归属，不能猜。未知命名空间同理丢弃。
var LEGACY_PROVIDER_PREFIXES={workbuddy:['cn:','global:'],trae:['trae:']};
function legacyCatalogGroups(items,provider){
  var groups={};
  (items||[]).forEach(function(m){
    var id=m&&typeof m.id==='string'?m.id:'';
    if(!id)return;
    var owner='';
    Object.keys(LEGACY_PROVIDER_PREFIXES).some(function(p){
      if(LEGACY_PROVIDER_PREFIXES[p].some(function(prefix){return id.indexOf(prefix)===0;})){owner=p;return true;}
      return false;
    });
    if(!owner||(provider&&provider!==owner))return;
    (groups[owner]||(groups[owner]=[])).push(m);
  });
  return groups;
}
async function loomProviders(){
  catalogCancel();var generation=LC.generation,controller=new AbortController();LC.controller=controller;
  var timer=setTimeout(function(){controller.abort();},18000);
  $('#providerGrid').innerHTML=loomEmpty('正在读取服务','读取当前账号数量与已经接通的能力。');
  try{
    var meta=await catalogMetadata(controller.signal);if(generation!==LC.generation)return;
    LC.providers=meta.providers;LC.accounts=meta.accounts;LC.initialized=true;
    $('#providerGrid').innerHTML=meta.providers.map(function(p){
      var counts=p.accounts||{},isCatalogOnly=!p.inference_available;
      return '<article class="loom-card" data-provider="'+esc(p.id)+'"><div class="card-head"><span class="provider-icon">'+esc(p.name.slice(0,1))+'</span><div><h3>'+esc(p.name)+'</h3><code>'+esc(p.id)+'</code></div></div><div class="loom-tags"><span class="loom-tag '+(isCatalogOnly?'wait':'good')+'">'+(isCatalogOnly?'目录与任务接入':'API 调用已接入')+'</span><span class="loom-tag">'+esc(counts.enabled||0)+' / '+esc(counts.total||0)+' 个账号启用</span></div><p>'+(isCatalogOnly?'可查看账号模型目录。当前仅提供账号目录与任务接入，还不能通过网关调用这些模型。':'模型来自当前服务目录，可继续使用已有账号登录与网关 API。')+'</p>'+(p.error_code?'<p role="alert">'+esc(CERROR[p.error_code]||'服务状态暂时不可用。')+'</p>':'')+'<div class="card-actions"><button class="btn pri" data-catalog-provider="'+esc(p.id)+'">查看模型目录</button>'+(p.login_available?'<button class="btn" data-loom-add="'+esc(p.id)+'">管理账号</button>':'')+'</div></article>';
    }).join('')||loomEmpty('尚无已注册服务','服务配置就绪后可在此查看。');
    loomNotice('providersNotice',meta.accountError?'加密账号存储读取失败，账号信息暂不可用。':'');
  }catch(e){if(generation!==LC.generation)return;$('#providerGrid').innerHTML=loomEmpty('服务信息读取失败','检查网关连接后，点击刷新目录重试。');loomNotice('providersNotice','无法读取当前服务能力。');}
  finally{clearTimeout(timer);if(generation===LC.generation)LC.controller=null;}
}
async function loomModels(force,append){
  catalogCancel();var generation=LC.generation,controller=new AbortController();LC.controller=controller;LC.loading=true;LC.error='';
  var timer=setTimeout(function(){controller.abort();},18000),provider=$('#modelProvider').value,account=$('#modelAccount').value;
  if(!append){LC.rows=[];LC.offset=0;LC.total=0;LC.hasMore=false;}
  $('#modelsReload').disabled=true;$('#modelsMore').disabled=true;
  loomNotice('modelsNotice','');if(!append)$('#modelGrid').innerHTML=loomEmpty('正在同步目录','读取账号可见的模型、档位与能力。');
  try{
    var meta=await catalogMetadata(controller.signal);if(generation!==LC.generation)return;
    LC.providers=meta.providers;LC.accounts=meta.accounts;LC.initialized=true;catalogOptions();
    provider=$('#modelProvider').value;account=$('#modelAccount').value;
    var jobs=[];
    var chosen=LC.providers.find(function(p){return p.id===provider;});
    var accountScoped=!!chosen&&chosen.catalog_scope==='account';
    // Legacy catalogs remain provider-wide. Do not label a union as an account
    // specific result. Selecting an account-scoped provider never queries them.
    if(!append&&!accountScoped&&!account){
      jobs.push(catalogAPI(force?'api/models/refresh':'api/models',force?{}:undefined,controller.signal).then(function(d){
        var items=Array.isArray(d.items)?d.items:[];return {kind:'legacy',items:items};
      }).catch(function(){return {kind:'legacy',error:true};}));
    }
    if(!provider||accountScoped){
      var query='?limit=20&offset='+(append?LC.offset:0)+(force?'&refresh=1':'')+(provider?'&provider='+encodeURIComponent(provider):'')+(account?'&account='+encodeURIComponent(account):'');
      var kind=provider||'account';
      jobs.push(catalogAPI('api/workbench/models'+query,undefined,controller.signal).then(function(d){return {kind:kind,page:d};}).catch(function(){return {kind:kind,error:true};}));
    }
    var results=await Promise.all(jobs);if(generation!==LC.generation)return;
    results.forEach(function(result){
      if(result.error){LC.rows.push({provider_id:result.kind==='legacy'?(provider||'legacy'):result.kind,status:'unavailable',error_code:'catalog_failed',models:[]});return;}
      if(result.kind==='legacy'){
        var groups=legacyCatalogGroups(result.items,provider);
        Object.keys(groups).forEach(function(p){LC.rows.push({provider_id:p,status:'ready',routing_status:'available',scope:'provider',models:groups[p]});});
      }else{
        LC.rows=LC.rows.concat(result.page.catalogs||[]);LC.total=result.page.total||0;LC.offset=(result.page.offset||0)+(result.page.catalogs||[]).length;LC.hasMore=!!result.page.has_more;
      }
    });
    if(meta.accountError)LC.error='账号存储读取失败，账号筛选暂不可用。';
    loomModelCards();
  }catch(e){if(generation!==LC.generation)return;LC.error='目录请求未完成，请检查连接后重试。';if(!append)LC.rows=[];loomModelCards();}
  finally{clearTimeout(timer);if(generation===LC.generation){LC.loading=false;LC.controller=null;$('#modelsReload').disabled=false;$('#modelsMore').disabled=false;}}
}
function loomModelCards(){
  var q=$('#modelSearch').value.trim().toLowerCase(),cap=$('#modelCapability').value,provider=$('#modelProvider').value;
  var models=[],states=[];
  LC.rows.forEach(function(row){
    if(provider&&row.provider_id!==provider&&row.provider_id!=='legacy')return;
    var who=(LP[row.provider_id]||row.provider_id)+(row.account_id?' · '+row.account_id:' · 服务目录');
    var text=CERROR[row.error_code]||'';
    states.push('<div class="catalog-status" data-catalog-status="'+esc(row.status)+'"><div><b>'+esc(who)+'</b><span class="loom-tag '+(row.status==='ready'?'good':'wait')+'">'+esc(CSTATE[row.status]||row.status)+'</span></div><small>'+esc(text||(row.status==='disabled'?'已停用账号不会向上游查询。':row.scope==='provider'?'服务范围目录，账号权限可能不同。':'同步 '+fmtTime(row.fetched_at)+' · 有效至 '+fmtTime(row.expires_at)))+(row.retry_at?' 可重试 '+esc(fmtTime(row.retry_at)):'')+'</small></div>');
    (row.models||[]).forEach(function(m){
      var inferred=Object.assign({},m,{supports_reasoning:m.supports_reasoning===true||(m.reasoning_efforts||[]).length>0});
      if(q&&(String(m.id)+' '+String(m.name||'')+' '+String(m.mode_name||'')+' '+String(m.vendor||'')).toLowerCase().indexOf(q)<0)return;
      if(cap&&inferred[cap]!==true)return;
      models.push({model:m,row:row});
    });
  });
  $('#catalogStatus').innerHTML=states.join('');$('#modelCount').textContent=models.length+' 个模型条目';
  $('#modelsMore').classList.toggle('hide',!LC.hasMore);$('#modelsPageInfo').textContent=LC.total?'账号级目录 '+Math.min(LC.offset,LC.total)+' / '+LC.total:'';
  var stale=LC.rows.some(function(r){return r.status==='stale';});
  loomNotice('modelsNotice',LC.error||(stale?'部分目录刷新失败，旧缓存仅供查看；缓存过旧时不再展示模型。':''));
  $('#modelGrid').innerHTML=models.map(function(entry){
    var m=entry.model,r=entry.row,only=r.routing_status==='catalog_only',tags=[];
    if(m.context_length)tags.push('上下文 '+compactTokens(m.context_length));if(m.max_output_tokens)tags.push('输出 '+compactTokens(m.max_output_tokens));
    if(m.mode_name)tags.push(m.mode_name);if(m.vendor)tags.push(m.vendor);if(m.native_tools)tags.push('官方工作工具');
    (m.tiers||[]).forEach(function(v){tags.push(v);});
    var efforts=(m.reasoning_efforts||[]).map(function(e){return e.name;}).join(' / ');
    return '<article class="loom-card" data-model-id="'+esc(m.id)+'" data-account-id="'+esc(r.account_id||'')+'"><div class="loom-tags"><span class="loom-tag">'+esc(LP[r.provider_id]||r.provider_id)+'</span><span class="loom-tag '+(only?'wait':'good')+'">'+(only?'仅目录 · 调用未接通':'可用于网关调用')+'</span>'+(r.status==='stale'?'<span class="loom-tag wait">旧缓存</span>':'')+'</div><div><h3>'+esc(m.name||m.id)+'</h3><code>'+esc(m.id)+'</code></div><p>'+esc(m.description||(only?'官方客户端返回的模型选项。原生工作工具不代表支持外部 API function tools。':'上游未提供模型说明。'))+'</p><div class="loom-tags" title="'+esc((m.context_length ? '上下文 '+exactTokens(m.context_length)+' tokens' : '')+(m.max_output_tokens ? ' 输出 '+exactTokens(m.max_output_tokens)+' tokens' : ''))+'">'+tags.map(function(t){return '<span class="loom-tag">'+esc(t)+'</span>';}).join('')+'</div>'+(efforts?'<p>推理档位：'+esc(efforts)+'</p>':'')+'<small class="sub">'+esc(r.account_id?'账号 '+r.account_id:'服务范围目录')+'</small></article>';
  }).join('')||loomEmpty(LC.error?'目录暂不可用':'没有匹配的模型',LC.rows.length?'调整筛选条件，或查看上方账号同步状态。':'接入服务账号后同步目录。');
}
document.addEventListener('click',function(e){var b=e.target.closest('[data-catalog-provider]');if(!b)return;var select=$('#modelProvider');catalogOptions();select.value=b.dataset.catalogProvider;$('#modelAccount').value='';go('models');});
$('#modelsReload').onclick=function(){loomModels(true,false);};
$('#modelsMore').onclick=function(){loomModels(false,true);};
$('#modelSearch').oninput=loomModelCards;$('#modelCapability').onchange=loomModelCards;
$('#modelProvider').onchange=function(){$('#modelAccount').value='';catalogOptions();loomModels(false,false);};
$('#modelAccount').onchange=function(){loomModels(false,false);};

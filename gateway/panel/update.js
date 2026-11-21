/* ── 版本与热更新 ────────────────────────────────────
 * 状态来自网关的本机管理接口 GET /update；真正的升级动作走
 * POST /update/apply，由网关下载校验后把监听套接字交给新进程，
 * 旧进程继续把在途请求（含长流式对话）跑完再退出，所以前台不需要重连。
 *
 * 2026-10-13：点「立即更新」后弹出进度对话框，全程显示下载/校验/切换进度。
 */
var UP = { data:null, timer:null, loading:false, modal:false, modalBusy:false, modalDone:false,
  target:'', targetRepo:'', submitting:false, awaiting:false, readError:'', applyError:'', pollExpired:false,
  frontendReloadPending:false, frontendReloadEligible:false };
var UP_STATE = {
  idle:      { t:'空闲',   cls:''   },
  checking:  { t:'检查中', cls:'w'  },
  downloading:{ t:'下载中', cls:'w' },
  handover:  { t:'切换中', cls:'w'  },
  restarting:{ t:'重连中', cls:'w' },
  failed:    { t:'失败',   cls:'e'  }
};
var UP_BUSY = { checking:1, downloading:1, handover:1, restarting:1 };

function upServerBusy(s){
  return !!UP_BUSY[s.state] || (s.auto_restart === true && s.restart_required === true);
}

function upRunningTarget(s){
  var target = UP.target || s.target_version || s.staged_version;
  return !!target && !!UP.targetRepo && s.repo === UP.targetRepo && s.current === target && s.state !== 'failed' && !UP_BUSY[s.state] && !s.restart_required &&
    (!Object.prototype.hasOwnProperty.call(s, 'ready') || s.ready === true) &&
    (!Object.prototype.hasOwnProperty.call(s, 'healthy') || s.healthy === true) && !UP.readError;
}

function reloadUpdatedFrontend(s){
  if (UP.frontendReloadPending || typeof s.repo !== 'string' || !s.repo || typeof s.current !== 'string' || !s.current) return;
  var key = 'loomgate.frontendReload';
  var marker = JSON.stringify({repo:s.repo, tag:s.current, commit:s.commit || ''});
  // A single durable marker also protects against a stale readback after the document reload.
  try{
    if (sessionStorage.getItem(key) === marker) return;
    sessionStorage.setItem(key, marker);
    if (sessionStorage.getItem(key) !== marker) return;
  }catch(err){ return; }
  UP.frontendReloadPending = true;
  setTimeout(function(){ window.location.reload(); }, 0);
}

function observeUpdateStatus(s){
  if (!UP.target && (s.target_version || s.staged_version)) UP.target = s.target_version || s.staged_version;
  if (!UP.targetRepo && typeof s.repo === 'string' && s.repo) UP.targetRepo = s.repo;
  if (UP.readError) return;
  if (s.state === 'downloading' || s.state === 'handover' || s.state === 'restarting' ||
      (s.auto_restart === true && s.restart_required === true)) UP.frontendReloadEligible = true;
  var wasUpdating = UP.frontendReloadEligible && (UP.awaiting || UP.submitting);
  if (s.state === 'failed' || upRunningTarget(s) || (s.restart_required && s.auto_restart !== true)){
    UP.awaiting = false;
    if (upRunningTarget(s)){
      UP.pollExpired = false;
      if (wasUpdating) reloadUpdatedFrontend(s);
    }
    UP.frontendReloadEligible = false;
    if (UP.timer) stopUpdatePoll();
  } else if (upServerBusy(s) && !UP.pollExpired){
    // A check may poll, but it is not an application and cannot authorize navigation.
    if (s.state !== 'checking' || UP.frontendReloadEligible) UP.awaiting = true;
    startUpdatePoll();
  } else if (!UP.frontendReloadEligible && !UP.submitting){
    UP.awaiting = false;
    if (UP.timer) stopUpdatePoll();
  }
}

function upState(st){
  var m = UP_STATE[st] || { t: st || '未知', cls:'' };
  return '<span class="upd-tag ' + m.cls + '">' + esc(m.t) + '</span>';
}

function upBytes(n){
  if (!n) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

// upDownBytes 与 upBytes 相同，但 0 显示为「0 B」而不是「—」。
//
// upBytes 的「—」语义是**缺失**（发布没给大小），而下载进度里的 0 是真实观测到的
// 「已下载 0 字节」。两者混用会让刚开始下载时显示「— / 19.1 MB」，看起来像字节数
// 没在动（2026-10-13 加进度条时发现）。
function upDownBytes(n){
  n = Number(n) || 0;
  return n === 0 ? '0 B' : upBytes(n);
}

// Go 的 time.Time 零值会序列化成 0001-01-01；只有可解析的时间才可作为
// 构建、发布或检查证据，不能把零值当成「已是最新」。
function validUpdateTime(value){
  if (typeof value !== 'string' || !value.trim() || value.indexOf('0001-') === 0) return false;
  return Number.isFinite(Date.parse(value));
}

function renderUpdate(){
  var r = UP.data, box = $('#updRows');
  if (!box) return;
  if (!r || r.ok === false){
    box.innerHTML = '<div class="upd-empty">' + esc((r && r.message) || '网关未响应热更新接口（旧版本网关请先升级）') + '</div>';
    $('#updState').textContent = '不可用';
    $('#updHint').textContent = '';
    $('#btnUpdApply').disabled = true;
    $('#btnUpdCheck').disabled = true;
    return;
  }
  var s = r.status || {};
  observeUpdateStatus(s);
  // 不可用有两种来源：配置关掉了（update.enabled=false），或发布仓库不开放
  // （私有且没配令牌）。两者都要如实说明原因，而不是让按钮点下去无声失败。
  var unavailable = (s.enabled === false) || !!s.unavailable_reason;
  if (unavailable){
    var why = s.enabled === false
      ? '热更新已在网关关闭（config update.enabled=false）'
      : (s.unavailable_reason || '发布仓库当前不可读');
    var mode = (s.visibility === 'private')
      ? '发布仓库为私有：把仓库改回公开，或配置 update.token'
      : '手工部署：本地构建运行包后上传替换，或让维护方协助';
    box.innerHTML = '<div class="upd-empty">' + esc(why) + '</div>' +
      mrow('当前版本', (s.current || '—') + (s.commit && s.commit !== 'unknown' ? ' · ' + s.commit.slice(0, 7) : '')) +
      mrow('发布仓库', (s.repo || '—') + (s.visibility ? ' · ' + s.visibility : '')) +
      mrow('升级方式', mode);
    $('#updState').textContent = s.enabled === false ? '已关闭' : '不可用';
    $('#updHint').textContent = s.enabled === false
      ? '如需恢复热更新，把 config 的 update.enabled 改回 true 并重载网关。'
      : (s.unavailable_reason || '');
    $('#btnUpdApply').disabled = true;
    $('#btnUpdCheck').disabled = true;
    return;
  }
  var busy = upServerBusy(s) || UP.submitting || UP.awaiting;
  var current = s.current || '—';
  var commit = (s.commit && s.commit !== 'unknown') ? ' · ' + s.commit.slice(0, 7) : '';
  var latest = s.latest_tag || (validUpdateTime(s.checked_at) ? '已是最新' : '未检查');
  var rows =
    mrow('当前版本', current + commit) +
    mrow('构建时间', validUpdateTime(s.built_at) ? fmtTime(s.built_at) : '未记录') +
    mrow('部署时间', validUpdateTime(s.deployed_at) ? fmtTime(s.deployed_at) : '未记录') +
    mrow('远端发布时间', validUpdateTime(s.latest_at) ? fmtTime(s.latest_at) : '未记录') +
    mrow('远端最新', latest, s.update_ready ? 'w' : 'ok') +
    mrow('更新状态', UP_STATE[s.state] ? UP_STATE[s.state].t : (s.state || '—'),
      (UP_STATE[s.state] || {}).cls || '') +
    mrow('下载包', s.asset_name ? (s.asset_name + ' · ' + upBytes(s.asset_size)) : '—') +
    mrow('上次成功检查', validUpdateTime(s.checked_at) ? relTime(s.checked_at) : '未记录') +
    mrow('仓库', s.repo || '—');
  box.innerHTML = rows;
  $('#updState').innerHTML = upState(s.state);
  $('#btnUpdCheck').disabled = busy;
  // 「立即更新」只在**确实有可升级版本**时可用：
  //   - 检查过且 update_ready=true → 可点；
  //   - 还没检查过（latest_tag 为空且无 checked_at）→ 可点，网关会先查一次远端；
  //   - 检查过但没有可升级版本（已是最新）→ 禁用，否则点下去会把服务白重启一遍。
  var checked = validUpdateTime(s.checked_at);
  var canApply = !busy && !s.restart_required && (!checked || !!s.update_ready);
  $('#btnUpdApply').disabled = !canApply;
  var hint;
  if (UP.readError && UP.awaiting){
    hint = '切换连接暂时中断，正在自动重新连接并核验运行版本；不会重复提交更新。';
    $('#updState').textContent = '重新连接中';
  }
  else if (UP.pollExpired) hint = '自动核验已超时，尚未确认目标版本运行；刷新页面可继续只读查询。';
  else if (s.restart_required && s.auto_restart === true){
    hint = '更新包已校验，程序正在自动切换并等待新版本就绪。';
    $('#updState').textContent = '自动切换中';
  }
  else if (s.restart_required){
    hint = '更新包 ' + (s.staged_version || '') + ' 已准备好；当前版本继续服务，重启网关或容器后生效。';
    $('#updState').textContent = '等待重启';
  }
  else if (s.state === 'failed') hint = s.last_error || '更新失败';
  else if (busy) hint = '正在' + ((UP_STATE[s.state] || {}).t || '准备更新') + '…完成后自动核验运行版本';
  else if (s.update_ready) hint = '可升级到 ' + s.latest_tag;
  else if (checked) hint = '已是最新版本（' + (s.latest_tag || s.current || '') + '）';
  else hint = '点「立即更新」会先检查远端版本';
  $('#updHint').textContent = hint;
  renderUpdateModal(s);
}

// ── 更新进度对话框 ──────────────────────────────────────────────────────
//
// 点「立即更新」后弹出，直到成功或失败才自动收起。它读的是与卡片**同一个**
// /update 快照（renderUpdate 每次都会调过来），所以进度与卡片永远一致。

/** 打开进度对话框。 */
function openUpdateModal(){
  UP.modal = true;
  UP.modalBusy = false;
  UP.modalDone = false;
  UP.pollExpired = false;
  UP.applyError = '';
  $('#updModalTitle').textContent = '正在更新';
  // 图标类必须**显式重置**：上一次失败时它被设成 'mic e'，不重置的话重试时
  // 标题写着「正在更新」而图标还是红色叹号，自相矛盾。
  $('#updModalIcon').className = 'mic w';
  $('#updModalText').textContent = '正在准备…';
  $('#updModalNote').textContent = '';
  var bar = $('#updModalBar');
  bar.classList.remove('unknown');
  bar.querySelector('i').style.width = '0%';
  $('#updModalClose').textContent = '后台运行';
  $('#updModalClose').disabled = false;
  $('#scrimU').classList.add('on');
  $('#updModal').classList.add('on');
}

function closeUpdateModal(){
  UP.modal = false;
  $('#scrimU').classList.remove('on');
  $('#updModal').classList.remove('on');
}

/** 按当前状态刷新对话框。没打开时什么都不做。 */
function renderUpdateModal(s){
  if (!UP.modal) return;
  var bar = $('#updModalBar');
  var fill = bar.querySelector('i');
  var text = $('#updModalText');
  var note = $('#updModalNote');
  var state = s.state || 'idle';
  var total = Number(s.asset_size) || 0;
  var done = Number(s.downloaded_bytes) || 0;

  if (UP.applyError){
    UP.modalDone = true;
    $('#updModalTitle').textContent = '更新未开始';
    $('#updModalIcon').className = 'mic e';
    text.textContent = UP.applyError;
    note.textContent = '未自动重发更新请求。';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  if (UP.pollExpired){
    $('#updModalTitle').textContent = '运行版本尚未确认';
    $('#updModalIcon').className = 'mic w';
    text.textContent = '自动核验已超时，尚未确认目标版本运行。';
    note.textContent = '刷新页面可继续只读查询；不会自动重发应用请求。';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  if (UP.readError && UP.awaiting){
    $('#updModalTitle').textContent = '正在重新连接';
    $('#updModalIcon').className = 'mic w';
    text.textContent = '更新切换期间连接暂时中断，正在自动重新连接。';
    note.textContent = '只核验运行版本，不会重复提交更新。';
    bar.classList.add('unknown');
    return;
  }

  if (s.restart_required && s.auto_restart !== true){
    UP.modalDone = true;
    bar.classList.remove('unknown');
    fill.style.width = '100%';
    $('#updModalTitle').textContent = '等待重启网关';
    $('#updModalIcon').className = 'mic w';
    text.textContent = '更新包 ' + (s.staged_version || '') + ' 已下载并校验。';
    note.textContent = '当前版本仍在运行。重启网关或容器后启用，再刷新核验版本。';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  if (state === 'failed'){
    // 终态：把原因写清楚，按钮变成「关闭」。不自动收起——用户需要读到失败原因。
    UP.modalDone = true;
    $('#updModalTitle').textContent = '更新失败';
    $('#updModalIcon').className = 'mic e';
    text.textContent = s.last_error || '更新失败，请查看网关日志。';
    bar.classList.remove('unknown');
    fill.style.width = '0%';
    note.textContent = '';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  if (upServerBusy(s)){
    UP.modalBusy = true;
  }

  if (state === 'checking'){
    text.textContent = '正在检查远端版本…';
    bar.classList.add('unknown');
    note.textContent = '';
    return;
  }

  if (state === 'downloading'){
    // 总大小未知（发布没给 size）时用不确定态滑动，而不是显示一个假的 0%
    // ——那会让人以为卡住了。
    //
    // 已下载字节一律用 upDownBytes 而不是 upBytes：upBytes(0) 是「—」（它的语义
    // 是「缺失」），但这里的 0 是真实的「还没下到字节」，显示成「— / 19.1 MB」
    // 会让人以为字节数根本没在动。
    if (total > 0){
      var pct = Math.max(0, Math.min(100, Math.round((done / total) * 100)));
      bar.classList.remove('unknown');
      fill.style.width = pct + '%';
      text.textContent = '正在下载更新包…';
      note.innerHTML = '<b>' + upDownBytes(done) + '</b> / ' + upBytes(total) + ' · ' + pct + '%';
    } else {
      bar.classList.add('unknown');
      text.textContent = '正在下载更新包…';
      note.innerHTML = '<b>' + upDownBytes(done) + '</b>';
    }
    return;
  }

  if (state === 'handover' || state === 'restarting' || (s.restart_required && s.auto_restart === true)){
    // 下载已完成：进度条补满，明确告诉用户「不会再等很久」。
    bar.classList.remove('unknown');
    fill.style.width = '100%';
    text.textContent = '更新包已校验，正在自动切换并核验新版本…';
    note.textContent = '连接恢复后自动读取运行版本，无需再次点击应用。';
    return;
  }

  // busy -> idle 也可能仍是旧进程。只有目标版本实际运行、可选健康位为真才完成。
  if (upRunningTarget(s)){
    UP.modalDone = true;
    bar.classList.remove('unknown');
    fill.style.width = '100%';
    $('#updModalTitle').textContent = '更新完成';
    $('#updModalIcon').className = 'mic ok';
    text.textContent = '已切换到新版本 ' + (s.current || '') + '。';
    note.textContent = '已从当前运行进程读回目标版本。';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  // 还没看到任何进行中的状态：保持「正在准备…」，等下一轮轮询。
  text.textContent = '正在准备…';
  bar.classList.add('unknown');
  note.textContent = '';
}

async function loadUpdate(){
  if (UP.loading) return;
  UP.loading = true;
  try{
    UP.data = await api('api/update');
    UP.readError = '';
  }catch(err){
    if (UP.awaiting || UP.submitting || upServerBusy((UP.data && UP.data.status) || {})) UP.readError = err.message;
    else UP.data = { ok:false, message: err.message };
  }finally{
    UP.loading = false;
  }
  renderUpdate();
}

function stopUpdatePoll(){
  if (UP.timer){ clearInterval(UP.timer); UP.timer = null; }
}

function startUpdatePoll(){
  if (UP.timer || UP.pollExpired) return;
  var ticks = 0, deadline = Date.now() + 20 * 60 * 1000;
  function expired(){ return ticks >= 600 || Date.now() >= deadline; }
  UP.timer = setInterval(async function(){
    ticks++;
    if (UP.loading){
      if (expired()){
        UP.pollExpired = true;
        UP.awaiting = false;
        UP.frontendReloadEligible = false;
        stopUpdatePoll();
        renderUpdate();
      }
      return;
    }
    await loadUpdate();
    var s = (UP.data && UP.data.status) || {};
    if (!UP.readError && (s.state === 'failed' || upRunningTarget(s) || (s.restart_required && s.auto_restart !== true))){
      UP.awaiting = false;
      stopUpdatePoll();
    } else if (expired()){
      UP.pollExpired = true;
      UP.awaiting = false;
      UP.frontendReloadEligible = false;
      stopUpdatePoll();
      renderUpdate();
    }
  }, 2000);
}

async function actUpdateCheck(){
  if (UP.submitting || UP.awaiting || upServerBusy((UP.data && UP.data.status) || {})){
    await loadUpdate();
    startUpdatePoll();
    return;
  }
  $('#btnUpdCheck').disabled = true;
  try{
    var r = await api('api/update/check', {});
    UP.data = r.ok === false ? r : r;
    renderUpdate();
    toast(r.ok === false ? ((r.message) || '检查失败') : '已检查远端版本', r.ok === false ? 'err' : 'ok');
  }catch(err){
    toast(err.message, 'err');
    $('#btnUpdCheck').disabled = false;
  }
}

async function actUpdateApply(){
  var s = (UP.data && UP.data.status) || {};
  if (UP.submitting) return;
  if (UP.awaiting || upServerBusy(s)){
    if (!UP.target) UP.target = s.target_version || s.staged_version || s.latest_tag || '';
    if (!UP.targetRepo && typeof s.repo === 'string') UP.targetRepo = s.repo;
    UP.awaiting = true;
    if (!UP.modal) openUpdateModal();
    renderUpdate();
    startUpdatePoll();
    return;
  }
  if (s.restart_required){
    toast('更新包已准备好，重启网关或容器后生效。', 'ok');
    return;
  }
  if (!UP.data || UP.data.ok !== true || s.enabled !== true || s.unavailable_reason){
    toast(s.unavailable_reason || '更新服务当前不可用，请先刷新状态。', 'err');
    return;
  }
  // 兜底：按钮已按 update_ready 禁用，这里再挡一次，避免通过其它入口触发无意义的重启。
  if (validUpdateTime(s.checked_at) && !s.update_ready){
    toast('已是最新版本（' + (s.latest_tag || s.current || '') + '），无需更新', 'ok');
    return;
  }
  UP.submitting = true;
  UP.awaiting = true;
  UP.target = s.latest_tag || s.target_version || '';
  UP.targetRepo = typeof s.repo === 'string' ? s.repo : '';
  UP.frontendReloadEligible = true;
  UP.readError = '';
  openUpdateModal();
  $('#btnUpdApply').disabled = true;
  $('#btnUpdCheck').disabled = true;
  try{
    var r = await api('api/update/apply', UP.target ? {tag:UP.target} : {});
    if (!r || r.ok !== true){
      UP.awaiting = false;
      UP.frontendReloadEligible = false;
      UP.applyError = (r && r.message) || '服务端未接受更新请求。';
      if (r && r.status) UP.data = r;
      toast(UP.applyError, 'err');
      return;
    }
    if (r.status) UP.data = r;
    if (!UP.target && r.status) UP.target = r.status.target_version || r.status.staged_version || r.status.latest_tag || '';
    startUpdatePoll();
  }catch(err){
    if (err.status >= 400 && err.status < 500 && err.status !== 408){
      UP.awaiting = false;
      UP.frontendReloadEligible = false;
      UP.applyError = err.message || '更新请求被拒绝。';
      toast(UP.applyError, 'err');
    } else {
      UP.readError = err.message || '更新提交连接中断';
      startUpdatePoll();
    }
  }finally{
    UP.submitting = false;
    renderUpdate();
  }
}

document.addEventListener('click', function(e){
  if (e.target.closest('#btnUpdCheck')){ actUpdateCheck(); return; }
  if (e.target.closest('#btnUpdApply')){ actUpdateApply(); return; }
  // 「后台运行」只收起对话框，**不取消更新**：更新跑在网关进程里，关掉界面
  // 不会影响它；用户想走开时不该被一个对话框按住。
  if (e.target.closest('#updModalClose')){ closeUpdateModal(); return; }
});
$('#scrimU').addEventListener('click', closeUpdateModal);

/* ── 重复推理保护（命中后重发 / 命中即停止）────────────────
 * 运行期开关，走网关本机管理接口 GET/POST /features/reasoning-loop。
 * 与 config.json 的 features.reasoning_loop_stop_only 同义，但改完立即生效。
 */
var RL = { data:null, loading:false };

function renderReasoningLoop(){
  var box = $('#rlRows');
  if (!box) return;
  var r = RL.data;
  if (!r || r.ok === false){
    box.innerHTML = '<div class="upd-empty">' +
      esc((r && r.message) || '网关未响应（旧版本网关请先升级）') + '</div>';
    $('#rlState').textContent = '不可用';
    $('#rlStopOnly').disabled = true;
    return;
  }
  var stopOnly = !!r.stop_only;
  $('#rlStopOnly').checked = stopOnly;
  $('#rlStopOnly').disabled = false;
  $('#rlState').innerHTML = '<span class="upd-tag ' + (stopOnly ? 'w' : '') + '">' +
    (stopOnly ? '命中即停止' : '命中后自动重发') + '</span>';
  box.innerHTML =
    mrow('当前行为', stopOnly ? '命中即停止，不重发' : '命中后同账号重发一次', stopOnly ? 'w' : 'ok') +
    mrow('启动配置', r.default ? '只停不重发' : '命中后重发') +
    mrow('生效方式', '立即生效，不需要重启');
}

async function loadReasoningLoop(){
  if (RL.loading) return;
  RL.loading = true;
  try{
    RL.data = await api('api/features/reasoning-loop');
  }catch(err){
    RL.data = { ok:false, message: err.message };
  }finally{
    RL.loading = false;
  }
  renderReasoningLoop();
}

async function actReasoningLoopToggle(next){
  var input = $('#rlStopOnly');
  input.disabled = true;
  try{
    var r = await api('api/features/reasoning-loop', { stop_only: next });
    if (r.ok === false){ toast(r.message || '切换失败', 'err'); }
    else { toast(r.message || '已切换', 'ok'); }
    await loadReasoningLoop();
  }catch(err){
    toast('切换失败：' + (err && err.message || err), 'err');
    // 失败时把开关恢复成服务端当前值，避免界面显示与实际不一致。
    await loadReasoningLoop();
  }finally{
    input.disabled = false;
  }
}

document.addEventListener('change', function(e){
  if (e.target && e.target.id === 'rlStopOnly') actReasoningLoopToggle(e.target.checked);
});

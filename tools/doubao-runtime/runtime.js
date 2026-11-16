// Evaluated only in the attached official desktop page. The adapter fingerprints
// are public bundle hashes, not model IDs, device data or signing material.
async (input) => {
  const fail = error => ({ok:false,error});
  if (!['identity','catalog'].includes(input.operation)) return fail('desktop_protocol_invalid');
  const digest = async value => Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(value))),x=>x.toString(16).padStart(2,'0')).join('');
  const slot = Symbol.for('loomgate.catalog.read.v1');
  if (window[slot]) return fail('desktop_busy');
  const lease = Date.now()+10000;
  window[slot] = lease;
  const work = async () => {
    const chunks = window['chunk_@flow-web/desktop'];
    if (!chunks || typeof chunks.push !== 'function') return fail('desktop_version_unsupported');
    const reqKey = Symbol.for('loomgate.catalog.require.v1');
    let req = window[reqKey];
    if (!req) {
      chunks.push([['loomgate-catalog-'+crypto.randomUUID()],{},r=>{req=r;}]);
      if (!req) return fail('desktop_version_unsupported');
      window[reqKey]=req;
    }
    const factories = {
      z5:'9687119c89c1376874f12b2c26072d6b908ae54c2b26c83dca8b9410ead436d8',
      uui:'0e16f7576bb91259b254f905a5fe84f853d4d4289f57a6b1418fddd93bd2fc65',
      Pp:'fdb80e31bbdecf4a092e0f242c334a70ebb98e6fd5cd85778ab44597dbca4775'
    };
    for (const [id,hash] of Object.entries(factories)) {
      if (typeof req.m?.[id] !== 'function' || await digest(String(req.m[id])) !== hash) return fail('desktop_version_unsupported');
    }
    const sdk = await req('z5').getFlowAccount().passportApi.getPassportSDK();
    const identity = async () => {
      const user = await sdk.login.checkLogin({});
      const id = user?.user_id_str || user?.user_id;
      if (user?.is_visitor_account || !id || (typeof id !== 'string' && !Number.isSafeInteger(id))) return null;
      return await digest(String(id));
    };
    let before;
    try { before = await identity(); } catch { return fail('desktop_login_required'); }
    if (!before) return fail('desktop_login_required');
    if (input.operation === 'identity') return {ok:true,identity:before};
    if (before !== input.identity) return fail('desktop_identity_mismatch');
    const bot=req('WMy').mainConvBotIdSelector(req('WMy').useFeedStore.getState());
    if(typeof bot!=='string'||!bot)return fail('desktop_version_unsupported');
    const api = req('uui').createRequest().apiContext;
    if (!api || typeof api.httpClient !== 'function' || api.getBaseURL() !== 'https://www.doubao.com') return fail('desktop_version_unsupported');
    let result;
    try {
      result = await api.httpClient({
        url:'https://www.doubao.com/alice/slot/action_bar_v3/brief_list',method:'POST',
        params:await api.getCommonParams(),
        headers:{...await api.getCommonHeaders(),...req('Pp').Y()},
        data:{bot_id:bot,language_code:'zh'}
      });
    } catch { return fail('desktop_upstream_rejected'); }
    let after;
    try { after = await identity(); } catch { return fail('desktop_login_required'); }
    if (before !== after) return fail('desktop_identity_mismatch');
    if (result?.code !== 0) return fail('desktop_upstream_rejected');
    // Project only the public catalog schema. Signed icon URLs, headers and all
    // unrelated account fields never leave the official renderer.
    const str = x => typeof x === 'string' ? x.slice(0,1024) : '';
    const integer = x => Number.isSafeInteger(x) ? x : 0;
    const items = rows => (Array.isArray(rows)?rows:[]).slice(0,512).map(x=>({
      ...(Number.isSafeInteger(x.item_id)?{item_id:x.item_id}:{}),
      model_item_key:str(x.model_item_key),name:str(x.name),default_mode:str(x.default_mode),
      show_task_tools:integer(x.show_task_tools),need_login:x.need_login===true,
      model_extra_params:{total_window_size:str(x.model_extra_params?.total_window_size),memory_profile:str(x.model_extra_params?.memory_profile)},
      reasoning_effort_config:{default_level:integer(x.reasoning_effort_config?.default_level),option_list:(x.reasoning_effort_config?.option_list||[]).slice(0,32).map(e=>({level:integer(e.level),name:str(e.name)}))},
      tag_list:(x.tag_list||[]).slice(0,32).map(t=>({text:str(t.text)}))
    }));
    if (!Array.isArray(result.data?.entry_list)) return fail('desktop_protocol_invalid');
    const entries = result.data.entry_list.slice(0,64).map(e=> {
      const m=e.active_switch_conf?.menu_conf_v2;
      if (!m) return {active_switch_conf:{}};
      return {active_switch_conf:{menu_conf_v2:{
        use_mode_model_list:m.use_mode_model_list===true,
        model_list:{item_list:items(m.model_list?.item_list)},
        mode_list:{item_list:(m.mode_list?.item_list||[]).slice(0,64).map(o=>({mode_id:str(o.mode_id),name:str(o.name),model_list:{item_list:items(o.model_list?.item_list)}}))}
      }}};
    });
    return {ok:true,identity:before,data:{entry_list:entries}};
  };
  let timer;
  try {
    const pending = work().finally(()=>{if(window[slot]===lease)delete window[slot];});
    return await Promise.race([pending,new Promise(resolve=>{timer=setTimeout(()=>resolve(fail('desktop_timeout')),8500);})]);
  } catch { return fail('desktop_protocol_invalid'); }
  finally {
    clearTimeout(timer);
    // The SDK read releases its own lease when it settles. Caller timeout must
    // not allow overlapping SDK reads; a stuck official SDK requires reopening
    // the client. This does not block the gateway or other providers.
  }
}

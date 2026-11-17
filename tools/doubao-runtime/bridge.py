"""Restricted CDP adapter. JSON over inherited pipes; no HTTP listener.

The protocol permits fixed identity/catalog/owned-text operations only. No
generic URL or script execution endpoint, cookie export, or credential output.
Message data is private pipe traffic and must never be written to shared logs.
"""
import http.client
import json
import re
import sys
import time
import uuid
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

LIMIT = 2 << 20
CATALOG_PATH = '/alice/slot/action_bar_v3/brief_list'
COMPLETION_PATH = '/chat/completion'
ASYNC_PATH = '/chat/async/chunk_stream'


class BridgeError(Exception):
    pass

def shape(value, depth=0):
    if depth > 4: return '<nested>'
    if isinstance(value, dict): return {k: shape(v, depth + 1) for k, v in value.items()}
    if isinstance(value, list): return [shape(v, depth + 1) for v in value[:3]]
    if isinstance(value, str): return '<string>'
    if isinstance(value, bool): return 'bool'
    if isinstance(value, (int, float)): return 'number'
    if value is None: return 'null'
    return '<value>'

def completion_body_valid(value, bot_id):
    if not isinstance(value, dict) or not isinstance(value.get('client_meta'), dict) or not isinstance(value.get('messages'), list) or not value['messages']:
        return False
    meta = value['client_meta']
    if meta.get('bot_id') != bot_id or not isinstance(meta.get('local_conversation_id'), str):
        return False
    option = value.get('option')
    if not isinstance(option, dict) or not isinstance(option.get('unique_key'), str) or not option['unique_key']:
        return False
    if not isinstance(option.get('start_seq'), int) or option['start_seq'] < 0:
        return False
    opts = option.get('sse_recv_event_options')
    if not isinstance(opts, dict) or opts.get('support_chunk_delta') is not True:
        return False
    message = value['messages'][0]
    return isinstance(message, dict) and isinstance(message.get('content_block'), list) and bool(message['content_block'])


def message_text(value):
    """Only text block payloads count; metadata never proves message ownership."""
    try:
        blocks = value['messages'][0]['content_block']
        parts = []
        for block in blocks:
            content = block.get('content', {})
            text = content.get('text_block', {}).get('text')
            if text is None: text = block.get('text')  # isolated schema fixtures
            if not isinstance(text, str): return None
            parts.append(text)
        return ''.join(parts)
    except (KeyError, IndexError, AttributeError, TypeError):
        return None


def validate(request):
    if isinstance(request,dict) and request.get('operation')=='work_read':
        if set(request)-{'operation','port','identity','conversation','session','message'} or type(request.get('port')) is not int or not 1<=request['port']<=65535:raise BridgeError('desktop_protocol_invalid')
        if not isinstance(request.get('identity'),str) or not re.fullmatch('[a-f0-9]{64}',request['identity']):raise BridgeError('desktop_protocol_invalid')
        for key in ['conversation']+(['session','message'] if 'session' in request or 'message'in request else []):
            if not isinstance(request.get(key),str) or not re.fullmatch('[A-Za-z0-9_-]{1,128}',request[key]):raise BridgeError('desktop_protocol_invalid')
        return
    if isinstance(request,dict) and request.get('operation') in ('activities','reward_records','task_checkin','task_claim'):
        if set(request)-{'operation','port','identity','group','task','instance'} or type(request.get('port')) is not int or not 1<=request['port']<=65535 or not isinstance(request.get('identity'),str) or not re.fullmatch('[a-f0-9]{64}',request['identity']):raise BridgeError('desktop_protocol_invalid')
        if request['operation'] in ('activities','reward_records') and set(request)!={'operation','port','identity'}:raise BridgeError('desktop_protocol_invalid')
        if request['operation'] not in ('activities','reward_records'):
            for field in ('group','task'):
                if not isinstance(request.get(field),str) or not re.fullmatch('[A-Za-z0-9._:-]{1,128}',request[field]):raise BridgeError('desktop_protocol_invalid')
            if request.get('instance') and (not isinstance(request['instance'],str) or not re.fullmatch('[A-Za-z0-9._:-]{1,128}',request['instance'])):raise BridgeError('desktop_protocol_invalid')
        return
    if not isinstance(request, dict) or set(request) - {'operation', 'port', 'identity', 'bot_id', 'prompt', 'model', 'lease', 'path', 'body', 'resume','task_id','scene','conversation','work','work_task'}:
        raise BridgeError('desktop_protocol_invalid')
    if type(request.get('port')) is not int or not 1 <= request['port'] <= 65535:
        raise BridgeError('desktop_protocol_invalid')
    if request.get('operation') in ('prepare_text','release_text','stream_text','authorize_async'):
        if not isinstance(request.get('identity'),str) or not re.fullmatch('[a-f0-9]{64}',request['identity']):raise BridgeError('desktop_protocol_invalid')
        if request['operation']=='prepare_text':
            if set(request)-{'operation','port','identity','model','prompt','conversation','work','work_task'} or not isinstance(request.get('model'),str) or not request['model'].startswith('doubao:') or len(request['model'])>256 or not isinstance(request.get('prompt'),str) or not 1<=len(request['prompt'].encode())<=65536 or type(request.get('work',False)) is not bool:raise BridgeError('desktop_protocol_invalid')
            if 'work_task' in request:
                task=request['work_task']
                if request.get('work') is not True or not isinstance(task,dict) or set(task)!={'group','task','instance'}:raise BridgeError('desktop_protocol_invalid')
                if any(not isinstance(task.get(k),str) or not re.fullmatch('[A-Za-z0-9._:-]{1,128}',task[k]) for k in ['group','task']):raise BridgeError('desktop_protocol_invalid')
                if not isinstance(task['instance'],str) or task['instance'] and not re.fullmatch('[A-Za-z0-9._:-]{1,128}',task['instance']):raise BridgeError('desktop_protocol_invalid')
            if 'conversation' in request and (not isinstance(request['conversation'],str) or not re.fullmatch('[A-Za-z0-9_-]{1,128}',request['conversation'])):raise BridgeError('desktop_protocol_invalid')
        else:
            if not isinstance(request.get('lease'),str) or not re.fullmatch('[a-f0-9-]{36}',request['lease']):raise BridgeError('desktop_protocol_invalid')
            if request['operation']=='release_text' and set(request)!={'operation','port','identity','lease'}:raise BridgeError('desktop_protocol_invalid')
            if request['operation']=='authorize_async' and (set(request)!={'operation','port','identity','lease','task_id','scene'} or not isinstance(request.get('task_id'),str) or not 1<=len(request['task_id'])<=512):raise BridgeError('desktop_protocol_invalid')
            if request['operation']=='stream_text':
                if set(request)-{'operation','port','identity','lease','path','body','resume','work'} or request.get('path') not in (COMPLETION_PATH,ASYNC_PATH) or not isinstance(request.get('body'),dict) or type(request.get('resume',False)) is not bool or type(request.get('work',False)) is not bool:raise BridgeError('desktop_protocol_invalid')
        return
    if set(request)-{'operation','port','identity','bot_id','prompt'}:raise BridgeError('desktop_protocol_invalid')
    if request.get('operation') == 'identity':
        if set(request) != {'operation', 'port'}:
            raise BridgeError('desktop_protocol_invalid')
    elif request.get('operation') == 'connection':
        if set(request)-{'operation','port','identity'} or (request.get('identity') and (not isinstance(request['identity'],str) or not re.fullmatch('[a-f0-9]{64}',request['identity']))):raise BridgeError('desktop_protocol_invalid')
    elif request.get('operation') == 'catalog':
        if not isinstance(request.get('identity'), str) or not isinstance(request.get('bot_id'), str) or not re.fullmatch('[a-f0-9]{64}', request['identity']) or not re.fullmatch('[A-Za-z0-9_-]{1,128}', request['bot_id']):
            raise BridgeError('desktop_protocol_invalid')
    elif request.get('operation') == 'prepare_completion':
        if not isinstance(request.get('identity'), str) or not isinstance(request.get('bot_id'), str) or not isinstance(request.get('prompt'), str) or not re.fullmatch('[a-f0-9]{64}', request['identity']) or not re.fullmatch('[A-Za-z0-9_-]{1,128}', request['bot_id']) or not 1 <= len(request['prompt']) <= 8192:
            raise BridgeError('desktop_protocol_invalid')
    else:
        raise BridgeError('desktop_protocol_invalid')


def target(port):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=2)
    try:
        conn.request('GET', '/json/list')
        response = conn.getresponse()
        data = response.read(LIMIT+1)
        if response.status != 200 or len(data) > LIMIT:
            raise BridgeError('desktop_unavailable')
        rows = json.loads(data)
    except OSError:
        raise BridgeError('desktop_unavailable') from None
    finally:
        conn.close()
    if not isinstance(rows, list):
        raise BridgeError('desktop_unavailable')
    pages = [r for r in rows if r.get('type') == 'page' and urlsplit(r.get('url','')).scheme == 'doubao' and urlsplit(r.get('url','')).hostname == 'doubao-chat' and urlsplit(r.get('url','')).path.startswith('/chat')]
    if len(pages) != 1:
        raise BridgeError('desktop_unavailable')
    ws = pages[0].get('webSocketDebuggerUrl', '')
    u = urlsplit(ws)
    if u.scheme != 'ws' or u.hostname != '127.0.0.1' or u.port != port or u.username or u.password or not re.fullmatch('/devtools/page/[A-Za-z0-9-]+', u.path) or u.query or u.fragment:
        raise BridgeError('desktop_unavailable')
    return ws

def prepare_completion_transaction(ws, call, request, deadline, pending, canceled=None):
    request_ids = set()
    editor = {'modified': False}
    owner = str(uuid.uuid4())
    cleanup_failed = False
    identity = ''
    result = None
    failure = None
    enabled = False

    def receive():
        while True:
            if canceled and canceled.is_set():raise BridgeError('desktop_canceled')
            remaining = deadline-time.monotonic()
            if remaining <= 0: raise BridgeError('desktop_timeout')
            try:return json.loads(ws.recv(timeout=min(.2,remaining) if canceled else remaining))
            except TimeoutError:
                if not canceled:raise

    def evaluate(expression, await_promise=False):
        response = call('Runtime.evaluate', {'expression':expression,'awaitPromise':await_promise,'returnByValue':True})
        if response.get('exceptionDetails'): raise BridgeError('desktop_version_unsupported')
        value = response.get('result',{}).get('value')
        if value is None: raise BridgeError('desktop_version_unsupported')
        return value

    def stop_paused(event):
        params=event.get('params',{});rid=params.get('requestId')
        if not rid: raise BridgeError('desktop_protocol_invalid')
        request_ids.add(rid)
        call('Fetch.failRequest',{'requestId':rid,'errorReason':'BlockedByClient'})
        request_ids.discard(rid)

    try:
        identity_script="""async()=>{const c=window['chunk_@flow-web/desktop'];if(!c)return {error:'desktop_version_unsupported'};const e=document.querySelector('[contenteditable=true]');if(!e)return {error:'desktop_version_unsupported'};const k=Symbol.for('loomgate.phase08.require');let r=window[k];if(typeof r!=='function'){r=await new Promise((resolve,reject)=>{const t=setTimeout(()=>reject(Error('timeout')),3000);c.push([['loomgate-identity-'+crypto.randomUUID()],{},x=>{clearTimeout(t);resolve(x)}])});if(typeof r!=='function')return {error:'desktop_version_unsupported'};window[k]=r}const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK();const u=await sdk.login.checkLogin({});const id=String(u?.user_id_str||u?.user_id||'');if(!id)return {error:'desktop_login_required'};const b=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(id));return {identity:Array.from(new Uint8Array(b),x=>x.toString(16).padStart(2,'0')).join(''),composerEmpty:!(e.innerText||'').trim()}}"""
        ident=evaluate('('+identity_script+')()',True)
        if ident.get('error'): raise BridgeError(ident['error'])
        if ident.get('composerEmpty') is not True: raise BridgeError('composer_not_empty')
        identity=ident.get('identity','')
        if identity!=request['identity']: raise BridgeError('desktop_identity_mismatch')

        call('Fetch.enable',{'patterns':[{'urlPattern':'https://www.doubao.com/chat/completion*loomgate_probe='+owner+'*','requestStage':'Request'}]})
        enabled=True
        prepare_script=Path(__file__).with_name('probe_runtime.js').read_text(encoding='utf-8')
        catalog_script=Path(__file__).with_name('runtime.js').read_text(encoding='utf-8')
        editor['modified']=True  # means an owned fetch lease may now exist, never a DOM edit
        probe_input={'prompt':request['prompt'],'owner':owner,'identity':identity,'bot_id':request['bot_id']}
        started=evaluate('('+prepare_script+')(/*loomgate-input*/'+json.dumps(probe_input)+',('+catalog_script+'))',True)
        if started.get('error'): raise BridgeError(started['error'])
        if started.get('owner')!=owner or not isinstance(started.get('local_conversation_id'),str): raise BridgeError('desktop_protocol_invalid')
        captured=None
        while time.monotonic()<deadline:
            event=pending.pop(0) if pending else receive()
            if event.get('method')!='Fetch.requestPaused':
                continue
            params=event.get('params',{});u=urlsplit(params.get('request',{}).get('url',''))
            request_ids.add(params.get('requestId'))
            if u.scheme!='https' or u.hostname!='www.doubao.com' or u.path!=COMPLETION_PATH or params.get('request',{}).get('method')!='POST':
                stop_paused(event);continue
            raw=params.get('request',{}).get('postData','') or ''
            try: body=json.loads(raw)
            except Exception: body=None
            if not completion_body_valid(body,request['bot_id']):
                stop_paused(event);continue
            if message_text(body) != request['prompt']:
                stop_paused(event);continue
            if body['option']['unique_key']!=owner or body['client_meta']['local_conversation_id']!=started['local_conversation_id']:
                stop_paused(event);continue
            query=parse_qs(u.query)
            if query.get('loomgate_probe')!=[owner]:
                stop_paused(event);continue
            if not query.get('a_bogus') or not query.get('msToken'):
                stop_paused(event);continue
            captured={'host':u.hostname,'path':u.path,'method':'POST','query_keys':sorted(query),'header_keys':sorted((params.get('request',{}).get('headers') or {}).keys()),'body_shape':shape(body),'body_sha256':__import__('hashlib').sha256(raw.encode()).hexdigest(),'network_sent':False,'mutations':0,'conversation_bound':True,'identity_bound':identity}
            stop_paused(event)
            result={'ok':True,'identity':identity,'data':captured,'signed':True}
            break
        if result is None: raise BridgeError('desktop_timeout')
    except BridgeError as e:
        failure=e
    except TimeoutError:
        failure=BridgeError('desktop_timeout')
    except Exception:
        failure=BridgeError('desktop_unavailable')
    finally:
        # Drain every currently paused matching request before detaching Fetch.
        # Paused requests from other origins are outside this probe and untouched.
        # Each cleanup RPC has a separate short deadline. A missing fail ACK must
        # leave enough time for the renderer's owned AbortController to settle.
        call.cleanup_deadline = time.monotonic()+5
        call.cleanup_rpc_timeout = .9
        drain_end=time.monotonic()+.2
        while pending:
            event=pending.pop(0)
            if event.get('method')!='Fetch.requestPaused': continue
            params=event.get('params',{});u=urlsplit(params.get('request',{}).get('url',''))
            if u.scheme=='https' and u.hostname=='www.doubao.com' and u.path in (COMPLETION_PATH,ASYNC_PATH):
                try: stop_paused(event)
                except Exception: pass
        while enabled and time.monotonic()<drain_end:
            try: event=json.loads(ws.recv(timeout=max(.05,drain_end-time.monotonic())))
            except Exception: break
            if event.get('method')!='Fetch.requestPaused': continue
            params=event.get('params',{});u=urlsplit(params.get('request',{}).get('url',''))
            if u.scheme=='https' and u.hostname=='www.doubao.com' and u.path in (COMPLETION_PATH,ASYNC_PATH):
                try: stop_paused(event)
                except Exception: pass
        for rid in list(request_ids):
            try:
                call('Fetch.failRequest',{'requestId':rid,'errorReason':'BlockedByClient'})
                request_ids.discard(rid)
            except Exception: pass
        if editor['modified']:
            restore=Path(__file__).with_name('probe_cleanup.js').read_text(encoding='utf-8')
            try:
                value=call('Runtime.evaluate',{'expression':'('+restore+')('+json.dumps(owner)+')','awaitPromise':True,'returnByValue':True}).get('result',{}).get('value')
                if not isinstance(value,dict) or value.get('ok') is not True: cleanup_failed=True
            except Exception: cleanup_failed=True
        while pending:
            event=pending.pop(0)
            if event.get('method')=='Fetch.requestPaused':
                try: stop_paused(event)
                except Exception: cleanup_failed=True
        if enabled:
            if request_ids:
                cleanup_failed=True
                # Do not release uncertain paused requests by Fetch.disable.
                try: call('Page.stopLoading')
                except Exception: pass
            else:
                try: call('Fetch.disable')
                except Exception: cleanup_failed=True
        if cleanup_failed: failure=BridgeError('desktop_cleanup_uncertain')
    if failure: raise failure
    return result


def execute(request,canceled=None):
    validate(request)
    from websockets.sync.client import connect
    script = Path(__file__).with_name('runtime.js').read_text(encoding='utf-8')
    deadline = time.monotonic()+(20 if request['operation'] in ('activities','task_checkin','task_claim') or request.get('work') else 10)
    with connect(target(request['port']), open_timeout=2, close_timeout=.1, max_size=LIMIT) as ws:
        seq = 0
        signed = False
        pending = []
        def call(method, params=None):
            nonlocal seq, signed
            seq += 1
            ws.send(json.dumps({'id':seq, 'method':method, 'params':params or {}}))
            rpc_deadline=min(getattr(call,'cleanup_deadline',deadline),time.monotonic()+getattr(call,'cleanup_rpc_timeout',max(0,deadline-time.monotonic())))
            while True:
                remaining = rpc_deadline-time.monotonic()
                if remaining <= 0:
                    raise BridgeError('desktop_timeout')
                if canceled and canceled.is_set() and not hasattr(call,'cleanup_deadline'):raise BridgeError('desktop_canceled')
                try:event = json.loads(ws.recv(timeout=min(.2,remaining) if canceled else remaining))
                except TimeoutError:
                    if canceled:continue
                    raise
                if event.get('method') == 'Network.requestWillBeSent':
                    u = urlsplit(event.get('params', {}).get('request', {}).get('url', ''))
                    if u.hostname == 'www.doubao.com' and u.path == CATALOG_PATH and parse_qs(u.query).get('a_bogus'):
                        signed = True
                if event.get('id') == seq:
                    if 'error' in event:
                        raise BridgeError('desktop_protocol_invalid')
                    return event.get('result', {})
                if event.get('method') == 'Fetch.requestPaused':
                    pending.append(event)
        if request['operation'] == 'prepare_completion':
            return prepare_completion_transaction(ws, call, request, deadline, pending,canceled)
        if request['operation'] in ('activities','reward_records','task_checkin','task_claim'):
            source=Path(__file__).with_name('tasks_runtime.js').read_text(encoding='utf-8')
            value=call('Runtime.evaluate',{'expression':'('+source+')('+json.dumps(request)+')','awaitPromise':True,'returnByValue':True})
            if value.get('exceptionDetails'):raise BridgeError('desktop_protocol_invalid')
            result=value.get('result',{}).get('value')
            if not isinstance(result,dict):raise BridgeError('desktop_protocol_invalid')
            return result
        if request['operation']=='work_read':
            source=Path(__file__).with_name('work_runtime.js').read_text(encoding='utf-8')
            value=call('Runtime.evaluate',{'expression':'('+source+')('+json.dumps(request)+')','awaitPromise':True,'returnByValue':True})
            if value.get('exceptionDetails'):raise BridgeError('desktop_protocol_invalid')
            result=value.get('result',{}).get('value')
            if not isinstance(result,dict):raise BridgeError('desktop_protocol_invalid')
            return result
        if request['operation'] in ('prepare_text','release_text','authorize_async'):
            source=Path(__file__).with_name('stream_runtime.js').read_text(encoding='utf-8')
            result=call('Runtime.evaluate',{'expression':'('+source+')('+json.dumps(request)+',('+script+'))','awaitPromise':True,'returnByValue':True})
            if result.get('exceptionDetails'):raise BridgeError('desktop_protocol_invalid')
            value=result.get('result',{}).get('value')
            if not isinstance(value,dict):raise BridgeError('desktop_protocol_invalid')
            return value
        call('Network.enable')
        result = call('Runtime.evaluate', {'expression':'('+script+')('+json.dumps(request)+')', 'awaitPromise':True, 'returnByValue':True})
        if 'exceptionDetails' in result:
            raise BridgeError('desktop_protocol_invalid')
        value = result.get('result', {}).get('value')
        if not isinstance(value, dict):
            raise BridgeError('desktop_protocol_invalid')
        if value.get('ok') and request['operation'] in ('catalog','connection'):
            if not signed:
                raise BridgeError('desktop_protocol_invalid')
            value['signed'] = True
        return value


def main():
    try:
        raw = sys.stdin.buffer.readline((1<<20)+1)
        if len(raw) > 1<<20:
            raise BridgeError('desktop_protocol_invalid')
        request=json.loads(raw)
        validate(request)
        if request['operation']=='stream_text':
            import importlib.util
            spec=importlib.util.spec_from_file_location('loomgate_stream_bridge',Path(__file__).with_name('stream_bridge.py'))
            module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
            module.pipe(request,target)
            return
        canceled=None
        if request['operation']=='prepare_completion':
            import threading
            canceled=threading.Event()
            def watch_parent():
                sys.stdin.buffer.readline(128)
                canceled.set()
            threading.Thread(target=watch_parent,daemon=True).start()
        result = execute(request,canceled)
    except BridgeError as e:
        result = {'ok':False, 'error':str(e)}
    except TimeoutError:
        result = {'ok':False, 'error':'desktop_timeout'}
    except Exception:
        result = {'ok':False, 'error':'desktop_unavailable'}
    encoded = json.dumps(result, ensure_ascii=True)
    if len(encoded) > LIMIT:
        encoded = '{"ok":false,"error":"desktop_protocol_invalid"}'
    sys.stdout.write(encoded)


if __name__ == '__main__':
    main()

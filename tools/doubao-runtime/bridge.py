"""Restricted one-shot CDP reader. JSON over inherited pipes; no HTTP listener.

Only the logged-in identity digest and projected public model catalog can leave
the renderer. No generic URL, JavaScript, cookie export or mutation operation.
"""
import http.client
import json
import re
import sys
import time
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


def validate(request):
    if not isinstance(request, dict) or set(request) - {'operation', 'port', 'identity', 'bot_id', 'prompt'}:
        raise BridgeError('desktop_protocol_invalid')
    if type(request.get('port')) is not int or not 1 <= request['port'] <= 65535:
        raise BridgeError('desktop_protocol_invalid')
    if request.get('operation') == 'identity':
        if set(request) != {'operation', 'port'}:
            raise BridgeError('desktop_protocol_invalid')
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

def prepare_completion_transaction(ws, call, request, deadline, pending):
    request_ids = set()
    editor = {'original': None, 'modified': False}
    identity = ''
    result = None
    failure = None
    enabled = False

    def receive():
        remaining = deadline-time.monotonic()
        if remaining <= 0: raise BridgeError('desktop_timeout')
        return json.loads(ws.recv(timeout=remaining))

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
        c=Symbol=None
        identity_script="""async()=>{const c=window['chunk_@flow-web/desktop'];if(!c)return {error:'desktop_version_unsupported'};const e=document.querySelector('[contenteditable=true]');if(!e)return {error:'desktop_version_unsupported'};const k=Symbol.for('loomgate.phase08.require');let r=window[k];if(typeof r!=='function'){r=await new Promise((resolve,reject)=>{const t=setTimeout(()=>reject(Error('timeout')),3000);c.push([['phase08-identity'],{},x=>{clearTimeout(t);resolve(x)}])});if(typeof r!=='function')return {error:'desktop_version_unsupported'};window[k]=r}const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK();const u=await sdk.login.checkLogin({});const id=String(u?.user_id_str||u?.user_id||'');if(!id)return {error:'desktop_login_required'};const b=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(id));return {identity:Array.from(new Uint8Array(b),x=>x.toString(16).padStart(2,'0')).join(''),composerEmpty:!(e.innerText||'').trim()}}"""
        ident=evaluate('('+identity_script+')()',True)
        if ident.get('error'): raise BridgeError(ident['error'])
        if ident.get('composerEmpty') is not True: raise BridgeError('composer_not_empty')
        identity=ident.get('identity','')
        if identity!=request['identity']: raise BridgeError('desktop_identity_mismatch')

        call('Fetch.enable',{'patterns':[{'urlPattern':'https://www.doubao.com/chat/completion*','requestStage':'Request'}]})
        enabled=True
        prepare_script="""(prompt)=>{const e=document.querySelector('[contenteditable=true]');if(!e||(e.innerText||'').trim())return {error:'composer_not_empty'};window.__loomgatePhase08Original=e.innerHTML;window.__loomgatePhase08Touched=false;e.innerText=prompt;const b=document.querySelector('[data-testid=chat_input_send_button]');if(!b)return {error:'desktop_version_unsupported'};window.__loomgatePhase08Touched=true;b.click();return {ok:true}}"""
        started=evaluate('('+prepare_script+')('+json.dumps(request['prompt'])+')')
        if started.get('error'): raise BridgeError(started['error'])
        editor['modified']=True
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
            if request['prompt'] not in raw:
                stop_paused(event);continue
            query=parse_qs(u.query)
            if not query.get('a_bogus') or not query.get('msToken'):
                stop_paused(event);continue
            captured={'host':u.hostname,'path':u.path,'method':'POST','query_keys':sorted(query),'header_keys':sorted((params.get('request',{}).get('headers') or {}).keys()),'body_shape':shape(body),'body_sha256':__import__('hashlib').sha256(raw.encode()).hexdigest(),'network_sent':False,'mutations':0,'conversation_bound':True,'identity_bound':identity}
            stop_paused(event)
            result={'ok':True,'identity':identity,'data':captured,'signed':True}
            break
        if result is None: raise BridgeError('desktop_timeout')
    except BridgeError as e:
        failure=e
    except Exception:
        failure=BridgeError('desktop_unavailable')
    finally:
        # Drain every currently paused matching request before detaching Fetch.
        # Paused requests from other origins are outside this probe and untouched.
        drain_end=time.monotonic()+1.5
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
            try: call('Fetch.failRequest',{'requestId':rid,'errorReason':'BlockedByClient'})
            except Exception: pass
        if editor['modified']:
            restore="""()=>{const e=document.querySelector('[contenteditable=true]');if(!e)return false;if((e.innerText||'').includes('LoomGate phase08 synthetic request preparation probe. Do not answer.')){e.innerHTML=window.__loomgatePhase08Original||'';e.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'deleteContent'}));delete window.__loomgatePhase08Original;delete window.__loomgatePhase08Touched;return true}return false}"""
            try: call('Runtime.evaluate',{'expression':'('+restore+')()','returnByValue':True})
            except Exception: pass
        if enabled:
            try: call('Fetch.disable')
            except Exception: pass
    if failure: raise failure
    return result


def execute(request):
    validate(request)
    from websockets.sync.client import connect
    script = Path(__file__).with_name('runtime.js').read_text(encoding='utf-8')
    deadline = time.monotonic()+10
    with connect(target(request['port']), open_timeout=2, close_timeout=.1, max_size=LIMIT) as ws:
        seq = 0
        signed = False
        pending = []
        def call(method, params=None):
            nonlocal seq, signed
            seq += 1
            ws.send(json.dumps({'id':seq, 'method':method, 'params':params or {}}))
            while True:
                remaining = deadline-time.monotonic()
                if remaining <= 0:
                    raise BridgeError('desktop_timeout')
                event = json.loads(ws.recv(timeout=remaining))
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
            return prepare_completion_transaction(ws, call, request, deadline, pending)
        call('Network.enable')
        result = call('Runtime.evaluate', {'expression':'('+script+')('+json.dumps(request)+')', 'awaitPromise':True, 'returnByValue':True})
        if 'exceptionDetails' in result:
            raise BridgeError('desktop_protocol_invalid')
        value = result.get('result', {}).get('value')
        if not isinstance(value, dict):
            raise BridgeError('desktop_protocol_invalid')
        if value.get('ok') and request['operation'] == 'catalog':
            if not signed:
                raise BridgeError('desktop_protocol_invalid')
            value['signed'] = True
        return value


def main():
    try:
        raw = sys.stdin.buffer.read(4097)
        if len(raw) > 4096:
            raise BridgeError('desktop_protocol_invalid')
        result = execute(json.loads(raw))
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

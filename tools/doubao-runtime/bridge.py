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
        call('Network.enable')
        if request['operation'] == 'prepare_completion':
            call('Fetch.enable', {'patterns':[{'urlPattern':'https://www.doubao.com/chat/completion*','requestStage':'Request'},{'urlPattern':'https://www.doubao.com/chat/async/chunk_stream*','requestStage':'Request'}]})
            identity_script = """async()=>{const c=window['chunk_@flow-web/desktop'];const k=Symbol.for('loomgate.phase08.require');let r=window[k];if(typeof r!=='function'){r=await new Promise(resolve=>c.push([['phase08-identity'],{},x=>resolve(x)]));if(typeof r!=='function')throw Error('require_unavailable');window[k]=r}const sdk=await r('z5').getFlowAccount().passportApi.getPassportSDK();const u=await sdk.login.checkLogin({});const id=String(u?.user_id_str||u?.user_id||'');const b=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(id));return Array.from(new Uint8Array(b),x=>x.toString(16).padStart(2,'0')).join('')}"""
            identity = call('Runtime.evaluate', {'expression':'('+identity_script+')()','awaitPromise':True,'returnByValue':True}).get('result',{}).get('value','')
            if identity != request['identity']:
                raise BridgeError('desktop_identity_mismatch')
            expr = """(prompt)=>{const e=document.querySelector('[contenteditable=true]');if(!e) return {ok:false,error:'desktop_version_unsupported'};e.focus();e.innerHTML='<p>'+prompt.replace(/[&<>]/g,'')+'</p>';e.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:prompt}));const b=document.querySelector('[data-testid=chat_input_send_button]');if(!b) return {ok:false,error:'desktop_version_unsupported'};b.click();return {ok:true}}"""
            started = call('Runtime.evaluate', {'expression':'('+expr+')('+json.dumps(request['prompt'])+')','returnByValue':True}).get('result',{}).get('value',{})
            if not started.get('ok'):
                raise BridgeError(started.get('error','desktop_protocol_invalid'))
            captured = None
            while time.monotonic() < deadline:
                remaining = deadline-time.monotonic()
                if remaining <= 0: break
                event = pending.pop(0) if pending else json.loads(ws.recv(timeout=remaining))
                if event.get('method') != 'Fetch.requestPaused': continue
                p = event.get('params',{});u = urlsplit(p.get('request',{}).get('url',''))
                if u.hostname != 'www.doubao.com' or u.path not in (COMPLETION_PATH,ASYNC_PATH):
                    call('Fetch.failRequest', {'requestId':p.get('requestId'),'errorReason':'BlockedByClient'}); continue
                body = p.get('request',{}).get('postData','') or ''
                try: parsed=json.loads(body)
                except Exception: parsed=None
                captured={'host':u.hostname,'path':u.path,'query_keys':sorted(parse_qs(u.query).keys()),'method':p.get('request',{}).get('method'),'header_keys':sorted((p.get('request',{}).get('headers') or {}).keys()),'body_shape':shape(parsed),'body_sha256':__import__('hashlib').sha256(body.encode()).hexdigest() if body else None,'network_sent':False,'mutations':0}
                call('Fetch.failRequest', {'requestId':p.get('requestId'),'errorReason':'BlockedByClient'})
                break
            if captured is None: raise BridgeError('desktop_timeout')
            return {'ok':True,'identity':identity,'data':captured,'signed':('a_bogus' in captured['query_keys'] and 'msToken' in captured['query_keys'])}
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

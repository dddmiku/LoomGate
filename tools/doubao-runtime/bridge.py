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


class BridgeError(Exception):
    pass


def validate(request):
    if not isinstance(request, dict) or set(request) - {'operation', 'port', 'identity', 'bot_id'}:
        raise BridgeError('desktop_protocol_invalid')
    if type(request.get('port')) is not int or not 1 <= request['port'] <= 65535:
        raise BridgeError('desktop_protocol_invalid')
    if request.get('operation') == 'identity':
        if set(request) != {'operation', 'port'}:
            raise BridgeError('desktop_protocol_invalid')
    elif request.get('operation') == 'catalog':
        if not isinstance(request.get('identity'), str) or not isinstance(request.get('bot_id'), str) or not re.fullmatch('[a-f0-9]{64}', request['identity']) or not re.fullmatch('[A-Za-z0-9_-]{1,128}', request['bot_id']):
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
    pages = [r for r in rows if r.get('type') == 'page' and r.get('url') in ('doubao://doubao-chat/chat', 'chrome://doubao-chat/chat')]
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

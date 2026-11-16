"""Synthetic loopback-only CDP tests. Never attach to an installed client."""
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

import bridge
from websockets.sync.server import serve
from websockets.exceptions import ConnectionClosedError, ConnectionClosedOK


class BridgeTests(unittest.TestCase):
    def test_missing_client_is_unavailable(self):
        with patch.object(bridge.http.client.HTTPConnection,'request',side_effect=TimeoutError):
            with self.assertRaisesRegex(bridge.BridgeError,'^desktop_unavailable$'):
                bridge.target(1234)

    def test_operation_validation(self):
        for value in ({'operation':'sms','port':1234}, {'operation':'identity','port':1234,'url':'https://example.invalid/'}, {'operation':'catalog','port':1234,'identity':'bad','bot_id':'x'}, {'operation':'identity','port':True}):
            with self.assertRaises(bridge.BridgeError):
                bridge.validate(value)

    def test_target_restricts_origin_port_and_unique_page(self):
        target = {'type':'page','url':'doubao://doubao-chat/chat'}
        class Handler(BaseHTTPRequestHandler):
            def log_message(self,*args): pass
            def do_GET(self):
                body=json.dumps(self.server.rows).encode()
                self.send_response(200);self.end_headers();self.wfile.write(body)
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        port=server.server_port
        try:
            good=f'ws://127.0.0.1:{port}/devtools/page/synthetic-target'
            server.rows=[dict(target,webSocketDebuggerUrl=good)]
            self.assertEqual(bridge.target(port),good)
            for address in ('ws://example.invalid/devtools/page/x',good+'?secret=fake',good.replace(str(port),'1'),good.replace('ws:','wss:')):
                server.rows=[dict(target,webSocketDebuggerUrl=address)]
                with self.assertRaises(bridge.BridgeError):bridge.target(port)
            server.rows=[dict(target,webSocketDebuggerUrl=good)]*2
            with self.assertRaises(bridge.BridgeError):bridge.target(port)
        finally:
            server.shutdown();server.server_close();thread.join()

    def test_cdp_signature_required_and_projected_result(self):
        for signed in (False,True):
            calls=[]
            def handler(ws):
                while True:
                    try:
                        raw=ws.recv()
                    except ConnectionClosedError as e:
                        if not signed and e.rcvd and e.rcvd.code==1011:return
                        raise
                    except ConnectionClosedOK:
                        if len(calls)==2:return
                        raise
                    request=json.loads(raw);calls.append(request['method'])
                    result={}
                    if request['method']=='Runtime.evaluate':
                        if signed:
                            ws.send(json.dumps({'method':'Network.requestWillBeSent','params':{'request':{'url':'https://www.doubao.com'+bridge.CATALOG_PATH+'?a_bogus=synthetic-signature'}}}))
                        result={'result':{'value':{'ok':True,'identity':'a'*64,'data':{'entry_list':[]}}}}
                    ws.send(json.dumps({'id':request['id'],'result':result}))
            server=serve(handler,'127.0.0.1',0)
            thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
            try:
                ws_url=f'ws://127.0.0.1:{server.socket.getsockname()[1]}'
                with patch.object(bridge,'target',return_value=ws_url):
                    request={'operation':'catalog','port':1234,'identity':'a'*64,'bot_id':'synthetic-bot'}
                    if signed:
                        result=bridge.execute(request);self.assertTrue(result['signed']);self.assertNotIn('url',result)
                    else:
                        with self.assertRaises(bridge.BridgeError):bridge.execute(request)
                self.assertEqual(calls,['Network.enable','Runtime.evaluate'])
            finally:
                server.shutdown();thread.join()

    def test_prepare_completion_aborts_signed_request_before_network(self):
        calls=[]
        def handler(ws):
            for raw in ws:
                request=json.loads(raw);calls.append(request['method'])
                if request['method']=='Runtime.evaluate':
                    if len([x for x in calls if x=='Runtime.evaluate']) == 1:
                        value={'result':{'value':'a'*64}}
                    else:
                        value={'result':{'value':{'ok':True}}}
                    ws.send(json.dumps({'id':request['id'],'result':value}))
                elif request['method']=='Fetch.enable':
                    ws.send(json.dumps({'id':request['id'],'result':{}}))
                    ws.send(json.dumps({'method':'Fetch.requestPaused','params':{'requestId':'synthetic-request','request':{'url':'https://www.doubao.com/chat/completion?a_bogus=synthetic&msToken=synthetic','method':'POST','headers':{'Content-Type':'application/json'},'postData':'{"messages":[],"option":{}}'}}}))
                else:
                    ws.send(json.dumps({'id':request['id'],'result':{}}))
        server=serve(handler,'127.0.0.1',0)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            ws_url=f'ws://127.0.0.1:{server.socket.getsockname()[1]}'
            request={'operation':'prepare_completion','port':1234,'identity':'a'*64,'bot_id':'synthetic-bot','prompt':'synthetic'}
            with patch.object(bridge,'target',return_value=ws_url):
                with patch.object(bridge,'Path',wraps=bridge.Path):
                    result=bridge.execute(request)
            self.assertTrue(result['ok']);self.assertTrue(result['signed']);self.assertFalse(result['data']['network_sent']);self.assertEqual(result['data']['mutations'],0)
            self.assertIn('Fetch.enable',calls);self.assertIn('Fetch.requestPaused',calls if False else ['Fetch.requestPaused'])
        finally:
            server.shutdown();thread.join()


if __name__=='__main__':unittest.main()

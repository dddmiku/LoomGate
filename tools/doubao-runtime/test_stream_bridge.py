"""Inherited-pipe protocol behavior with synthetic CDP only."""
import importlib.util
import io
import json
import logging
import os
import subprocess
import sys
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('stream_bridge',Path(__file__).with_name('stream_bridge.py'))
stream=importlib.util.module_from_spec(spec);spec.loader.exec_module(stream)

class WaitInput:
    def __init__(self):self.done=threading.Event();self.buffer=self
    def readline(self,*args):self.done.wait(3);return b''

class CDP:
    def __init__(self,*,signed=True,foreign=False,cleanup=True):self.queue=[];self.calls=[];self.signed=signed;self.foreign=foreign;self.cleanup=cleanup;self.pulls=0
    def __enter__(self):return self
    def __exit__(self,*args):return False
    def send(self,raw):
        req=json.loads(raw);self.calls.append(req);result={}
        if req['method']=='Runtime.evaluate':
            data=json.JSONDecoder().raw_decode(req['params']['expression'].split('/*loomgate-stream-input*/',1)[1])[0]
            operation=data['operation'];value={'ok':True,'data':{}}
            if operation=='open_text':
                body=data['body']
                if self.foreign:body={**body,'option':{'unique_key':'foreign'}}
                if self.signed:self.queue.append({'method':'Network.requestWillBeSent','params':{'request':{'url':'https://www.doubao.com/chat/completion?a_bogus=synthetic','postData':json.dumps(body)}}})
                value['data']={'status':200,'request_digest':'a'*64}
            elif operation=='pull_text':
                value['data']={'chunk':'c3ludGhldGlj'} if not self.pulls else {'done':True};self.pulls+=1
            elif operation=='cancel_text':value={'ok':self.cleanup}
            result={'result':{'value':value}}
        self.queue.append({'id':req['id'],'result':result})
    def recv(self,timeout=None):
        if not self.queue:raise TimeoutError()
        return json.dumps(self.queue.pop(0))

class StreamPipeTests(unittest.TestCase):
    def invoke(self,cdp):
        stdin=WaitInput();stdout=io.StringIO();request={'port':1234,'identity':'a'*64,'lease':'00000000-0000-4000-8000-000000000000','path':'/chat/completion','body':{'option':{'unique_key':'synthetic-transaction'},'messages':[{'local_message_id':'synthetic-message'}]}}
        exit_code=0
        try:
            with patch('websockets.sync.client.connect',return_value=cdp),patch.object(sys,'stdin',stdin),patch.object(sys,'stdout',stdout):
                try:stream.pipe(request,lambda port:'ws://127.0.0.1:1234/synthetic')
                except SystemExit as e:exit_code=e.code
        finally:stdin.done.set()
        return exit_code,[json.loads(line) for line in stdout.getvalue().splitlines()]

    def test_signed_owned_stream_returns_chunks_and_waits_for_cleanup(self):
        code,frames=self.invoke(CDP());self.assertEqual(code,0);self.assertEqual([x['kind'] for x in frames],['start','chunk','end']);self.assertNotIn('a_bogus',json.dumps(frames))

    def test_foreign_signed_request_does_not_prove_this_submission(self):
        for cdp in (CDP(signed=False),CDP(foreign=True)):
            code,frames=self.invoke(cdp);self.assertEqual(code,0);self.assertEqual([x['kind'] for x in frames],['error'])

    def test_cleanup_error_has_nonzero_exit_even_after_end(self):
        code,frames=self.invoke(CDP(cleanup=False));self.assertEqual(code,3);self.assertEqual(frames[-1]['kind'],'end')

    def test_actual_process_parent_pipe_close_cancels_and_exits_cleanly(self):
        from websockets.sync.server import serve
        fake=CDP();cleaned=threading.Event()
        def handler(ws):
            try:
                while True:
                    raw=ws.recv();fake.send(raw)
                    if 'cancel_text' in raw:cleaned.set()
                    while fake.queue:ws.send(json.dumps(fake.queue.pop(0)))
            except Exception:return
        def request_hook(connection,request):
            if request.path=='/json/list':
                port=server.socket.getsockname()[1]
                rows=[{'type':'page','url':'doubao://doubao-chat/chat','webSocketDebuggerUrl':f'ws://127.0.0.1:{port}/devtools/page/synthetic'}]
                return connection.respond(200,json.dumps(rows))
        logger=logging.Logger('synthetic-cdp');logger.addHandler(logging.NullHandler());logger.propagate=False
        server=serve(handler,'127.0.0.1',0,process_request=request_hook,logger=logger)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        child=None
        try:
            request={'operation':'stream_text','port':server.socket.getsockname()[1],'identity':'a'*64,'lease':'00000000-0000-4000-8000-000000000000','path':'/chat/completion','body':{'option':{'unique_key':'synthetic-transaction'},'messages':[{'local_message_id':'synthetic-message'}]},'resume':False}
            child=subprocess.Popen([sys.executable,'-I','-B',str(Path(__file__).with_name('bridge.py'))],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,env={**os.environ,'PYTHONDONTWRITEBYTECODE':'1'})
            child.stdin.write(json.dumps(request).encode()+b'\n');child.stdin.flush()
            first=json.loads(child.stdout.readline());self.assertEqual(first['kind'],'start')
            child.stdin.close();child.stdout.close()
            self.assertEqual(child.wait(timeout=4),0);self.assertTrue(cleaned.wait(1))
        finally:
            if child and child.poll() is None:child.kill();child.wait(timeout=3)
            server.shutdown();thread.join(timeout=3)

if __name__=='__main__':unittest.main()

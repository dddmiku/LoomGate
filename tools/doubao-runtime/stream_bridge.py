"""Bounded stdio frames for one renderer-owned stream attempt. Never log payloads."""
import json
import os
import sys
import threading
import time
from pathlib import Path
from urllib.parse import parse_qs,urlsplit

LIMIT=2<<20

def pipe(request,target):
    from websockets.sync.client import connect
    canceled=threading.Event()
    def watch_parent():
        # EOF means the owning gateway went away. No credentials in this channel.
        sys.stdin.buffer.readline(128)
        canceled.set()
    threading.Thread(target=watch_parent,daemon=True).start()
    def frame(kind,**fields):
        sys.stdout.write(json.dumps({'kind':kind,**fields},ensure_ascii=True)+'\n');sys.stdout.flush()
    source=Path(__file__).with_name('stream_runtime.js').read_text(encoding='utf-8')
    catalog=Path(__file__).with_name('runtime.js').read_text(encoding='utf-8')
    sequence=0;signed=False;opened=False
    cleanup_ok=False
    deadline=time.monotonic()+(130 if request.get('work') else 40)
    base={'identity':request['identity'],'lease':request['lease']}
    try:
        with connect(target(request['port']),open_timeout=2,close_timeout=.1,max_size=LIMIT) as ws:
            def call(method,params):
                nonlocal sequence,signed
                sequence+=1
                ws.send(json.dumps({'id':sequence,'method':method,'params':params}))
                until=min(deadline,time.monotonic()+12)
                while time.monotonic()<until:
                    event=json.loads(ws.recv(timeout=max(.01,until-time.monotonic())))
                    if event.get('method')=='Network.requestWillBeSent':
                        req=event.get('params',{}).get('request',{});url=urlsplit(req.get('url',''))
                        if url.hostname=='www.doubao.com' and url.path==request['path'] and parse_qs(url.query).get('a_bogus'):
                            try:
                                sent=json.loads(req.get('postData','{}'))
                                if request['path']=='/chat/completion':
                                    owned=sent.get('option',{}).get('unique_key')==request['body'].get('option',{}).get('unique_key') and bool(sent.get('option',{}).get('unique_key'))
                                    owned=owned and [x.get('local_message_id') for x in sent.get('messages',[])]==[x.get('local_message_id') for x in request['body'].get('messages',[])]
                                else:owned=sent.get('task_id')==request['body'].get('task_id') and bool(sent.get('task_id'))
                                signed=signed or owned
                            except (ValueError,TypeError,AttributeError):pass
                    if event.get('id')==sequence:
                        if event.get('error'):raise RuntimeError('protocol')
                        return event.get('result',{})
                raise TimeoutError()
            def invoke(operation,**fields):
                value={**base,'operation':operation,**fields}
                result=call('Runtime.evaluate',{'expression':'('+source+')(/*loomgate-stream-input*/'+json.dumps(value)+',('+catalog+'))','awaitPromise':True,'returnByValue':True})
                if result.get('exceptionDetails'):raise RuntimeError('protocol')
                return result.get('result',{}).get('value',{})
            try:
                call('Network.enable',{})
                if canceled.is_set():frame('error',error='desktop_canceled');return
                result=invoke('open_text',path=request['path'],body=request['body'],resume=request.get('resume',False))
                if not result.get('ok'):frame('error',error=result.get('error','desktop_protocol_invalid'));return
                opened=True
                if not signed:frame('error',error='desktop_protocol_invalid');return
                frame('start',signed=True,**result.get('data',{}))
                while time.monotonic()<deadline and not canceled.is_set():
                    result=invoke('pull_text')
                    if not result.get('ok'):frame('error',error=result.get('error','desktop_protocol_invalid'));return
                    data=result.get('data',{})
                    if 'chunk' in data:frame('chunk',data=data['chunk'])
                    elif data.get('done'):frame('end');return
                if not canceled.is_set():frame('error',error='desktop_timeout')
            finally:
                # Also cancel an unknown open: fetch may be submitted before CDP timeout.
                try:cleanup_ok=invoke('cancel_text').get('ok') is True
                except Exception:cleanup_ok=False
    except BrokenPipeError:
        # Prevent Python's final stdout flush from turning an acknowledged
        # cancellation into exit120 after the parent closes its output pipe.
        try:
            with open(os.devnull,'wb') as sink:os.dup2(sink.fileno(),sys.stdout.fileno())
        except (AttributeError,OSError,ValueError):pass
    except Exception:
        try:frame('error',error='desktop_stream_interrupted' if opened else 'desktop_submission_uncertain')
        except (BrokenPipeError,OSError):pass
    finally:
        # Every protocol frame was explicitly flushed. The parent can close its
        # read end immediately after any frame; Python shutdown must not attempt
        # a second flush to that now-closed pipe.
        try:
            with open(os.devnull,'wb') as sink:os.dup2(sink.fileno(),sys.stdout.fileno())
        except (AttributeError,OSError,ValueError):pass
        if not cleanup_ok:raise SystemExit(3)

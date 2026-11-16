"""Frozen independent Phase09 assertions; synthetic CDP plus actual Node DOM expressions."""
import json, os, subprocess, unittest
from pathlib import Path
from unittest.mock import patch
import bridge
ROOT=Path(__file__).resolve().parents[2]/'gateway'
ENV=os.environ.copy()

REQUEST={'operation':'prepare_completion','port':1234,'identity':'a'*64,'bot_id':'synthetic-bot','prompt':'synthetic'}

def body(prompt='synthetic'):
    return json.dumps({'client_meta':{'bot_id':'synthetic-bot','local_conversation_id':'conversation-a'},'messages':[{'content_block':[{'text':prompt}]}],'option':{'unique_key':'transaction-a','start_seq':0,'sse_recv_event_options':{'support_chunk_delta':True}}})

def paused(identifier,path='/chat/completion',method='POST',data=None):
    return {'method':'Fetch.requestPaused','params':{'requestId':identifier,'request':{'url':'https://www.doubao.com'+path+'?a_bogus=synthetic&msToken=synthetic','method':method,'headers':{'Content-Type':'application/json'},'postData':body() if data is None else data}}}

class FakeCDP:
    def __init__(self,items,missing_button=False,reject_fail=None):
        self.items=items; self.queue=[]; self.calls=[]; self.failed=[]; self.missing_button=missing_button; self.reject_fail=reject_fail
    def __enter__(self): return self
    def __exit__(self,*args): return False
    def send(self,text):
        r=json.loads(text); self.calls.append(r); result={}; error=None
        if r['method']=='Runtime.evaluate':
            n=sum(x['method']=='Runtime.evaluate' for x in self.calls)
            value={'identity':'a'*64,'composerEmpty':True} if n==1 else ({'error':'desktop_version_unsupported'} if n==2 and self.missing_button else {'ok':True})
            if '/*loomgate-input*/' in r['params']['expression']:
                # The new probe constructs its own transaction, without clicking
                # a composer. Reflect its nonce in otherwise unchanged fixtures.
                own=json.JSONDecoder().raw_decode(r['params']['expression'].split('/*loomgate-input*/',1)[1])[0]['owner']
                for event in self.items:
                    req=event['params']['request'];req['url']+='&loomgate_probe='+own
                    try:
                        parsed=json.loads(req.get('postData',''))
                        if 'option' in parsed:parsed['option']['unique_key']=own
                        if 'client_meta' in parsed:parsed['client_meta']['local_conversation_id']='local_'+own
                        req['postData']=json.dumps(parsed)
                    except ValueError:pass
                if not self.missing_button:value.update(owner=own,local_conversation_id='local_'+own)
            result={'result':{'value':value}}
        if r['method']=='Fetch.failRequest':
            self.failed.append(r['params']['requestId'])
            if r['params']['requestId']==self.reject_fail: error={'code':-1,'message':'synthetic rejection'}
        self.queue.append({'id':r['id'],**({'error':error} if error else {'result':result})})
        if r['method']=='Fetch.enable': self.queue.extend(self.items)
    def recv(self,timeout=None):
        if not self.queue: raise TimeoutError()
        return json.dumps(self.queue.pop(0))

def execute(fake,request=None):
    with patch('websockets.sync.client.connect',return_value=fake),patch.object(bridge,'target',return_value='ws://127.0.0.1:1234/devtools/page/synthetic'):
        return bridge.execute((request or REQUEST).copy())

def eval_dom(expressions,missing_button=False):
    # Execute the production expressions in a minimal DOM, not a mocked success result.
    node=r'''const vm=require('node:vm');let raw='';process.stdin.on('data',x=>raw+=x);process.stdin.on('end',()=>{const d=JSON.parse(raw);let text='',html='<p><br></p>';const e={get innerText(){return text},set innerText(x){text=x;html=x},get innerHTML(){return html},set innerHTML(x){html=x;text=x==='<p><br></p>'?'':x},dispatchEvent(){return true}};const ctx=vm.createContext({window:{},document:{querySelector:s=>s==='[contenteditable=true]'?e:d.missing_button?null:{click(){}}},InputEvent:function(){}});let results=[];for(const expression of d.expressions){results.push(vm.runInContext(expression,ctx))}process.stdout.write(JSON.stringify({text,html,results}));});'''
    r=subprocess.run(['node','-e',node],input=json.dumps({'expressions':expressions,'missing_button':missing_button}),text=True,capture_output=True,timeout=5,cwd=ROOT,env=ENV)
    if r.returncode: raise AssertionError('synthetic DOM evaluation failed')
    return json.loads(r.stdout)

class OriginalBoundaryTests(unittest.TestCase):
    def test_review_all_paused_requests_aborted_before_return(self):
        fake=FakeCDP([paused('first'),paused('second')]); result=execute(fake)
        self.assertTrue(result['ok']); self.assertEqual(set(fake.failed),{'first','second'})
    def test_review_malformed_body_cannot_prove_preparation(self):
        fake=FakeCDP([paused('malformed',data='not-json')])
        try: result=execute(fake)
        except bridge.BridgeError: return
        self.assertFalse(result.get('ok'))
    def test_review_async_get_cannot_prove_initial_completion(self):
        fake=FakeCDP([paused('other',path='/chat/async/chunk_stream',method='GET')])
        try: result=execute(fake)
        except bridge.BridgeError: return
        self.assertFalse(result.get('ok'))

class NewBoundaryTests(unittest.TestCase):
    def test_prompt_in_metadata_not_message_cannot_prove_preparation(self):
        data=body('unrelated message') # 'synthetic' occurs in bot_id, not the message.
        fake=FakeCDP([paused('metadata-only',data=data)])
        try: result=execute(fake)
        except bridge.BridgeError: return
        self.assertFalse(result.get('ok'),'A prompt substring in bot_id was accepted as the submitted message')
    def test_cleanup_failed_ack_cannot_report_success(self):
        fake=FakeCDP([paused('first'),paused('second')],reject_fail='second')
        try: result=execute(fake)
        except bridge.BridgeError: return
        self.assertFalse(result.get('ok'),'Cleanup error swallowed while probe reported successful no-network preparation')
    def test_arbitrary_prompt_composer_restored(self):
        request={**REQUEST,'prompt':'A fresh synthetic arbitrary prompt'}
        fake=FakeCDP([paused('current',data=body(request['prompt']))]); self.assertTrue(execute(fake,request)['ok'])
        expressions=[x['params']['expression'] for x in fake.calls if x['method']=='Runtime.evaluate'][1:]
        dom=eval_dom(expressions)
        self.assertEqual(dom['html'],'<p><br></p>','Production restore only recognizes one hard-coded phase08 prompt')
    def test_missing_send_button_still_restores_modified_composer(self):
        fake=FakeCDP([],missing_button=True)
        with self.assertRaises(bridge.BridgeError): execute(fake)
        expressions=[x['params']['expression'] for x in fake.calls if x['method']=='Runtime.evaluate'][1:]
        dom=eval_dom(expressions,missing_button=True)
        self.assertEqual(dom['html'],'<p><br></p>','Composer modified before missing button error but restoration omitted')

class ExtendedBoundaryTests(unittest.TestCase):
    def test_wrong_transaction_never_proves_ownership(self):
        class WrongTransaction(FakeCDP):
            def send(self,text):
                super().send(text)
                request=json.loads(text)
                if '/*loomgate-input*/' in request.get('params',{}).get('expression',''):
                    data=json.loads(self.items[0]['params']['request']['postData']);data['option']['unique_key']='foreign-transaction';self.items[0]['params']['request']['postData']=json.dumps(data)
        with self.assertRaises(bridge.BridgeError):execute(WrongTransaction([paused('foreign')]))

    def test_retry_ack_success_clears_only_confirmed_ids(self):
        class TransientFail(FakeCDP):
            def send(self,text):
                request=json.loads(text)
                if request['method']=='Fetch.failRequest' and request['params']['requestId']=='second' and 'second' in self.failed:self.reject_fail=None
                super().send(text)
        fake=TransientFail([paused('first'),paused('second')],reject_fail='second')
        self.assertTrue(execute(fake)['ok']);self.assertEqual(fake.failed.count('second'),2)
        self.assertIn('Fetch.disable',[x['method'] for x in fake.calls])

    def test_uncertain_cleanup_never_disables_interception(self):
        fake=FakeCDP([paused('first'),paused('second')],reject_fail='second')
        with self.assertRaisesRegex(bridge.BridgeError,'desktop_cleanup_uncertain'):execute(fake)
        self.assertNotIn('Fetch.disable',[x['method'] for x in fake.calls])

    def test_identity_mismatch_has_no_fetch_or_mutation(self):
        fake=FakeCDP([])
        with self.assertRaisesRegex(bridge.BridgeError,'desktop_identity_mismatch'):execute(fake,{**REQUEST,'identity':'b'*64})
        self.assertNotIn('Fetch.enable',[x['method'] for x in fake.calls])

    def test_timeout_still_runs_independent_cleanup(self):
        fake=FakeCDP([])
        with self.assertRaises(bridge.BridgeError):execute(fake)
        expressions=[x['params']['expression'] for x in fake.calls if x['method']=='Runtime.evaluate']
        self.assertTrue(any('lease.controller.abort()' in x for x in expressions))
        self.assertIn('Fetch.disable',[x['method'] for x in fake.calls])

if __name__=='__main__':unittest.main()

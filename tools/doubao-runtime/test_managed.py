"""Synthetic lifecycle boundaries; never launches or logs into a real client."""
import json,os,tempfile,unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import managed


class ManagedTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.state={'version':1,'port':19227,'profile':str(self.root/'profile'),'executable':str(self.root/'Doubao.exe'),'pid':4242,'created':12.0,'launch':'running'}
    def test_manifest_cannot_redirect_profile(self):
        self.state['profile']=str(self.root.parent/'foreign')
        managed.atomic(self.root/'client.json',self.state)
        with self.assertRaises(managed.Failure):managed.load(self.root)
    def test_explicit_executable_conflict_never_launches(self):
        managed.atomic(self.root/'client.json',self.state)
        with patch.object(managed.subprocess,'Popen') as spawn:
            with self.assertRaises(managed.Failure):managed.ensure(self.root,str(self.root/'other.exe'),True)
            spawn.assert_not_called()
    def test_read_only_status_does_not_create_profile_or_process(self):
        with patch.object(managed.subprocess,'Popen') as spawn:
            with self.assertRaises(managed.Failure):managed.ensure(self.root,'',False)
            spawn.assert_not_called()
        self.assertFalse((self.root/'client.json').exists())
    def test_alive_process_is_reused_without_spawn(self):
        managed.atomic(self.root/'client.json',self.state)
        proc=SimpleNamespace(pid=4242,create_time=lambda:12.0)
        with patch.object(managed,'find_owned',return_value=proc),patch.object(managed,'port_owner',return_value=True),patch.object(managed.subprocess,'Popen') as spawn:
            result=managed.ensure(self.root,'',True);self.assertEqual(result['pid'],4242);spawn.assert_not_called()
    def test_port_hijack_never_accepted(self):
        import psutil
        row=SimpleNamespace(status=psutil.CONN_LISTEN,laddr=SimpleNamespace(port=19227,ip='0.0.0.0'),pid=4242)
        with patch.object(psutil,'net_connections',return_value=[row]):
            with self.assertRaises(managed.Failure):managed.port_owner(self.state,SimpleNamespace(pid=4242))
        row.laddr.ip='127.0.0.1';row.pid=9191
        with patch.object(psutil,'net_connections',return_value=[row]):
            with self.assertRaises(managed.Failure):managed.port_owner(self.state,SimpleNamespace(pid=4242))
    def test_cached_pid_still_checks_creation_executable_and_command(self):
        import psutil
        class Process:
            pid=4242
            def create_time(self):return 12.0
            def exe(self):return self_path
            def cmdline(self):return args
        self_path=self.state['executable'];args=['--user-data-dir='+self.state['profile'],'--remote-debugging-port=19227','--remote-debugging-address=127.0.0.1']
        with patch.object(psutil,'Process',return_value=Process()),patch.object(psutil,'process_iter') as scan:
            self.assertEqual(managed.find_owned(self.state).pid,4242);scan.assert_not_called()
        args=['--user-data-dir='+str(self.root/'unrelated')]
        with patch.object(psutil,'Process',return_value=Process()),patch.object(psutil,'process_iter',return_value=[]):self.assertIsNone(managed.find_owned(self.state))
    def test_login_nonce_conflict_does_not_attach_or_click(self):
        state={**self.state,'login':{'nonce':'a'*32,'status':'waiting','deadline':10**12,'method':'sms'}}
        with self.assertRaises(managed.Failure):managed.login(self.root,state,'login_start','b'*32,'qr')
    def test_missing_attempt_cannot_be_polled_or_canceled(self):
        for action in ['login_poll','login_cancel']:
            with self.subTest(action=action),self.assertRaises(managed.Failure):managed.login(self.root,self.state,action,'a'*32,'sms')
    def test_unknown_action_rejected_before_any_lifecycle_work(self):
        with patch.object(managed,'ensure') as ensure:
            with self.assertRaises(managed.Failure):managed.execute({'action':'send_sms','root':str(self.root),'phone':'synthetic'})
            ensure.assert_not_called()
    def test_non_windows_host_rejects_managed_lifecycle_before_disk_or_process(self):
        with patch.object(managed.os,'name','posix'),patch.object(managed,'ensure') as ensure:
            for action in ('status','ensure','login_start'):
                with self.subTest(action=action),self.assertRaisesRegex(managed.Failure,'^desktop_management_platform_unsupported$'):
                    managed.execute({'action':action,'root':str(self.root),'nonce':'a'*32,'method':'qr'})
            ensure.assert_not_called()
        self.assertEqual(list(self.root.iterdir()),[])


class SyntheticOfficial:
    def __init__(self):
        self.opens=0;self.identity_error='desktop_login_required';self.loading_fails=False
        self.shape={'sms':False,'code':False,'expired':False,'scanned':False,'manual':False,'qr':True,'error':False}
        self.chat=self.Page(self,'doubao://doubao-chat/chat');self.pages=[self.chat]
        self.browser=SimpleNamespace(contexts=[SimpleNamespace(pages=self.pages)])
    class Page:
        def __init__(self,owner,url):self.owner=owner;self.url=url
        def wait_for_function(self,*a,**k):pass
        def wait_for_load_state(self,*a,**k):
            if self.owner.loading_fails:raise TimeoutError('synthetic-loading')
        def wait_for_timeout(self,*a):raise AssertionError('unexpected wait')
        def get_by_text(self,*a,**k):return self
        def count(self):return 1
        def click(self,*a,**k):
            self.owner.opens+=1;self.owner.pages.append(SyntheticOfficial.Page(self.owner,'doubao://doubao-login/login'))
        def locator(self,*a,**k):return self
        def wait_for(self,*a,**k):pass
        def bring_to_front(self):pass
        def close(self,*a,**k):self.owner.pages.remove(self)
        def evaluate(self,*a):
            if len(a)>1:return {'ok':False,'error':self.owner.identity_error}
            return self.owner.shape
    def __enter__(self):return SimpleNamespace(chromium=SimpleNamespace(connect_over_cdp=lambda *a,**k:self.browser))
    def __exit__(self,*args):pass


class LoginJournalTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.state={'port':19227};self.client=SyntheticOfficial();self.nonce='a'*32
        self.mock=patch('playwright.sync_api.sync_playwright',return_value=self.client);self.mock.start();self.addCleanup(self.mock.stop)
    def test_cold_start_unready_does_not_create_attempt_and_explicit_resume_opens_once(self):
        self.client.identity_error='desktop_version_unsupported'
        with self.assertRaises(managed.Failure):managed.login(self.root,self.state,'login_start',self.nonce,'qr')
        self.assertNotIn('login',self.state);self.assertEqual(self.client.opens,0)
        self.client.identity_error='desktop_login_required'
        result=managed.login(self.root,self.state,'login_resume',self.nonce,'qr')
        self.assertEqual(result['state'],'waiting_scan');self.assertEqual(self.client.opens,1)
        managed.login(self.root,self.state,'login_resume',self.nonce,'qr');self.assertEqual(self.client.opens,1)
    def test_opening_journal_survives_lost_page_readiness_without_reopen(self):
        self.client.loading_fails=True
        with self.assertRaises(TimeoutError):managed.login(self.root,self.state,'login_start',self.nonce,'qr')
        self.assertEqual(self.client.opens,1);self.assertEqual(self.state['login']['status'],'opening')
        self.client.loading_fails=False
        restored=json.loads((self.root/'client.json').read_text())
        self.assertEqual(managed.login(self.root,restored,'login_resume',self.nonce,'qr')['state'],'waiting_scan')
        self.assertEqual(self.client.opens,1)
    def test_existing_unknown_official_window_prevents_new_start(self):
        self.client.pages.append(SyntheticOfficial.Page(self.client,'doubao://doubao-login/login'))
        with self.assertRaises(managed.Failure):managed.login(self.root,self.state,'login_resume',self.nonce,'qr')
        self.assertEqual(self.client.opens,0);self.assertNotIn('login',self.state)
    def test_mfa_status_readback_cancel_and_deadline(self):
        managed.login(self.root,self.state,'login_start',self.nonce,'qr');self.client.shape['manual']=True
        result=managed.login(self.root,self.state,'login_poll',self.nonce,'qr')
        self.assertEqual(result,{'state':'manual_action','reason':'official_verification_required'})
        self.assertEqual(self.client.opens,1)
        result=managed.login(self.root,self.state,'login_cancel',self.nonce,'qr');self.assertEqual(result['state'],'canceled');self.assertEqual(len(self.client.pages),1)
    def test_expired_challenge_does_not_poll_or_reopen(self):
        managed.login(self.root,self.state,'login_start',self.nonce,'qr');self.client.shape['manual']=True;self.state['login']['deadline']=1
        self.assertEqual(managed.login(self.root,self.state,'login_poll',self.nonce,'qr')['state'],'expired');self.assertEqual(self.client.opens,1)
        self.assertEqual(managed.login(self.root,self.state,'login_resume',self.nonce,'qr')['state'],'expired');self.assertEqual(self.client.opens,1)
    def test_same_attempt_cannot_switch_authentication_method(self):
        managed.login(self.root,self.state,'login_start',self.nonce,'qr')
        with self.assertRaises(managed.Failure):managed.login(self.root,self.state,'login_resume',self.nonce,'sms')
        self.assertEqual(self.client.opens,1)

if __name__=='__main__':unittest.main()

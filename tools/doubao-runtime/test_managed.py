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
    def test_missing_attempt_cannot_be_polled_or_resumed(self):
        for action in ['login_poll','login_resume','login_cancel']:
            with self.subTest(action=action),self.assertRaises(managed.Failure):managed.login(self.root,self.state,action,'a'*32,'sms')
    def test_unknown_action_rejected_before_any_lifecycle_work(self):
        with patch.object(managed,'ensure') as ensure:
            with self.assertRaises(managed.Failure):managed.execute({'action':'send_sms','root':str(self.root),'phone':'synthetic'})
            ensure.assert_not_called()

if __name__=='__main__':unittest.main()

"""Full production binding driver via a synthetic official-client runtime."""
import argparse,json,os,subprocess,sys,time,traceback
from pathlib import Path
from playwright.sync_api import sync_playwright
from urllib.parse import urlsplit
p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);p.add_argument('--fixture',required=True);args=p.parse_args()
root=Path(args.runtime).resolve();out=Path(args.output).resolve();binary=Path(args.fixture).resolve()
if root.name!='catalog-fixture' or any(p.drive.upper()!='D:' for p in [root,out,binary]):raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);base='http://127.0.0.1:17863';report={'actions':[],'page_errors':[],'console_errors':[],'external_requests':[]};stage='start';child=None
def main():
 global stage,child
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000})
  page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__));page.on('request',lambda r:report['external_requests'].append('external') if urlsplit(r.url).hostname not in [None,'127.0.0.1'] else None)
  page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None)
  def mode(name):assert page.request.post(base+'/__fixture/mode/'+name).status==204
  def snapshot():return page.request.get(base+'/admin/api/workbench/accounts').json()['accounts']
  def flows():return page.request.get(base+'/admin/api/connections/flows').json()['flows']
  def step(action,status):
   current=flows()[-1]
   with page.expect_response(lambda r:r.url==base+'/admin/api/connections/step' and r.request.method=='POST') as response:
    page.locator('#connection-'+action).click()
   assert response.value.status==200
   result=response.value.json()['flow']
   assert result['id']==current['id'] and result['revision']>current['revision'] and result['status']==status
   if status=='waiting_authorization':page.locator('#connection-commit').wait_for(state='detached')
   elif status=='verified':page.locator('#connection-commit').wait_for()
   elif status=='succeeded':page.locator('#connection-done').wait_for()
   return result
  def verify():
   current=flows()[-1];deadline=time.time()+12
   while time.time()<deadline:
    if time.time()>__import__('datetime').datetime.fromisoformat(current['next_poll_at'].replace('Z','+00:00')).timestamp():break
    time.sleep(.1)
   step('verify','verified')
  fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
  assert page.request.get(base+'/admin/api/workbench/client').status in (401,302)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  page.locator('#navigation a[href="#accounts"]').click();page.locator('#account-provider').select_option('doubao');page.locator('[data-client-state=unbound]').wait_for()
  before=len(snapshot());mode('client-disconnected');page.locator('#client-status-refresh').click();page.locator('[data-client-state=disconnected]').wait_for();page.locator('#connect-official-client').click();page.locator('#connection-begin').click();page.locator('#connection-verify').wait_for();assert flows()[-1]['status']=='waiting_authorization';assert len(snapshot())==before
  report['actions'].append('disconnected_state_and_explicit_readonly_recovery')
  stage='restart';page.locator('#dialog-close').click();assert page.request.post(base+'/__fixture/stop').status==204
  deadline=time.monotonic()+6
  while time.monotonic()<deadline:
   try:page.request.get(base+'/__fixture/stats',timeout=200)
   except Exception:break
   time.sleep(.1)
  with (root/'stdout.log').open('ab') as stdout,(root/'stderr.log').open('ab') as stderr:child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863','-desktop-connection'],stdout=stdout,stderr=stderr,env={**os.environ,'TEMP':r'D:\LoomGate-work\tmp','TMP':r'D:\LoomGate-work\tmp'},creationflags=subprocess.CREATE_NO_WINDOW)
  (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8')
  deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   try:
    if page.request.get(base+'/__fixture/stats',timeout=300).status==200:break
   except Exception:pass
   time.sleep(.1)
  else:raise AssertionError('restart_failed')
  page.reload();page.locator('[data-connection-resume]').click();verify();page.locator('#connection-commit').wait_for();assert len(snapshot())==before
  report['actions'].append('real_server_restart_resumes_verified_binding_without_save')
  stage='identity-switch';mode('client-switched');step('commit','waiting_authorization');assert not page.locator('#connection-commit').count();assert len(snapshot())==before
  mode('success');verify();saved=step('commit','succeeded')['account_id'];assert len(snapshot())==before+1
  report['actions'].append('identity_switch_does_not_save_then_correct_identity_commits')
  page.locator('#connection-done').click();page.locator('[data-client-state=ready]').wait_for();assert page.locator('#account-list').inner_text().count('当前账号可用')==1
  stage='reconnect';row=page.locator('#account-list tr').filter(has=page.get_by_text(saved,exact=True));row.locator('[data-action=reconnect-account]').click();page.locator('#connection-method').select_option('desktop');page.locator('#connection-begin').click();page.locator('#connection-commit').wait_for();prior=next(a for a in snapshot() if a['id']==saved)
  changed=page.request.post(base+'/admin/api/workbench/account/toggle',data={'id':saved,'enabled':False,'revision':prior['revision']},headers={'X-Admin-Request':'1','Origin':base});assert changed.status==200
  step('commit','waiting_authorization');assert next(a for a in snapshot() if a['id']==saved)['enabled'] is False
  page.locator('#connection-cancel').click();page.locator('#connection-done').click();report['actions'].append('concurrent_disabled_account_is_not_reenabled')
  stage='mobile';page.set_viewport_size({'width':390,'height':844});mode('client-login');page.locator('#client-status-refresh').click();page.locator('[data-client-state=login_required]').wait_for();assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'client-login-mobile.png'),full_page=True)
  mode('success');page.locator('#client-status-refresh').click();page.locator('[data-client-state=unbound]').wait_for();page.screenshot(path=str(out/'client-disabled-mobile.png'),full_page=True)
  report['actions'].append('mobile_official_login_and_disabled_binding_states')
  stats=page.request.get(base+'/__fixture/stats').json();assert stats['sms_calls']==0 and stats['qr_calls']==0 and stats['mutations']==0
  report['actions'].append('no_sms_qr_or_task_mutation_from_binding_workflow')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('desktop_connection_browser.py')])
finally:
 if child is not None:
  import urllib.request
  try:urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop',data=b'',method='POST'),timeout=3).close()
  except Exception:pass
  try:child.wait(timeout=6)
  except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=6)
(out/'desktop-connection-browser.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report));sys.exit(report['result']!='PASS')

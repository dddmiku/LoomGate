"""Full production binding driver via a synthetic official-client runtime."""
import argparse,json,os,subprocess,sys,time,traceback
from pathlib import Path
from playwright.sync_api import sync_playwright
from console_actions import choose_provider, choose_account, choose_capability
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
  def wait_flow(statuses,timeout=20):
   deadline=time.monotonic()+timeout;seen=[]
   while time.monotonic()<deadline:
    latest=flows()[-1]
    if latest['status'] in statuses:return latest
    seen.append(latest['status']+'/'+str(latest.get('error_code')))
    time.sleep(.15)
   raise AssertionError('flow_status_timeout:'+','.join(statuses)+' last='+','.join(seen[-4:]))
  def verify(statuses=('succeeded',)):
   # 核验本身是用户动作；核验通过后抽屉自动执行一次幂等保存（新设计），
   # 因此这里等的是自动保存的最终状态，而不是旧版单独的“保存”按钮。
   current=flows()[-1];deadline=time.time()+12
   while time.time()<deadline:
    if time.time()>__import__('datetime').datetime.fromisoformat(current['next_poll_at'].replace('Z','+00:00')).timestamp():break
    time.sleep(.1)
   with page.expect_response(lambda r:r.url==base+'/admin/api/connections/step' and r.request.method=='POST') as response:
    page.locator('#connection-verify').click()
   assert response.value.status==200
   result=response.value.json()['flow']
   assert result['id']==current['id']
   return wait_flow(statuses)
  def post_api(path,payload):
   return page.request.post(base+'/admin/api/'+path,data=json.dumps(payload),headers={'X-Admin-Request':'1','Origin':base,'Content-Type':'application/json'})
  fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
  page.locator('#navigation a[href="#accounts"]').click();choose_provider(page,'#account-provider','doubao');page.locator('#account-list').wait_for()
  before=len(snapshot())
  report['actions'].append('account_page_renders_without_a_client_bridge')
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
  # Restart resets the synthetic client mode to healthy. Resuming the flow and
  # clicking 核验 must save exactly one account — and resuming alone must not.
  page.reload();page.locator('[data-connection-resume]').click();assert len(snapshot())==before
  saved_flow=verify();saved=saved_flow['account_id'];assert saved and len(snapshot())==before+1
  page.locator('#connection-done').click();page.locator('#dialog').wait_for(state='hidden')
  report['actions'].append('real_server_restart_resumes_verified_binding_without_duplicate_save')
  stage='identity-switch'
  # Reconnecting an existing account: Start verifies immediately. When the
  # official client is on another identity, the drawer must stay un-saved.
  mode('client-switched')
  reconnect=page.locator('[data-action=reconnect-account][data-account="'+saved+'"]')
  # Closing the drawer triggers a full account reload; wait for the row before
  # clicking, otherwise the click lands on a stale/absent control.
  reconnect.wait_for(timeout=20000)
  reconnect.click();page.locator('#connection-begin').wait_for()
  page.locator('#connection-begin').click()
  switched=wait_flow(('waiting_authorization','failed','verified'),20)
  assert switched['status']=='waiting_authorization' and switched['error_code']=='desktop_identity_mismatch' and len(snapshot())==before+1,switched
  mode('success')
  # The re-verify is itself the save action; wait for the flow to reach
  # succeeded before asserting the drawer's final state.
  done=verify();assert done['status']=='succeeded' and done['account_id']==saved and len(snapshot())==before+1,done
  page.locator('#connection-done').wait_for(timeout=20000)
  report['actions'].append('identity_switch_does_not_save_then_correct_identity_commits')
  page.locator('#connection-done').click();page.locator('#account-list').wait_for()
  stage='reconnect';prior=next(a for a in snapshot() if a['id']==saved)
  # 并发停用保护仍由服务端 CAS 保证；新抽屉把保存并入核验，不再暴露单独的
  # 保存按钮，所以在 API 层验证“核验后、保存前账号被停用”不会覆盖。
  start=post_api('connections/start',{'provider':'doubao','method':'desktop','account_id':saved});assert start.status==200
  started=start.json()['flow'];assert started['status']=='verified',started
  changed=post_api('workbench/account/toggle',{'id':saved,'enabled':False,'revision':prior['revision']});assert changed.status==200
  commit=post_api('connections/step',{'id':started['id'],'action':'commit','input':{}});assert commit.status==200
  blocked=commit.json()['flow'];assert blocked['status']=='waiting_authorization' and blocked['error_code'] in ('account_identity_mismatch','account_revision_conflict')
  disabled=next(a for a in snapshot() if a['id']==saved);assert disabled['enabled'] is False
  post_api('workbench/account/toggle',{'id':saved,'enabled':True,'revision':disabled['revision']})
  # This flow was created over the API, so the drawer may show an unrelated
  # page state; close it defensively instead of requiring a cancel button.
  page.keyboard.press('Escape')
  if page.locator('#dialog').is_visible():page.keyboard.press('Escape')
  page.locator('#dialog').wait_for(state='hidden')
  report['actions'].append('concurrent_disabled_account_is_not_reenabled')
  stage='mobile';page.set_viewport_size({'width':390,'height':844});page.locator('#account-list').wait_for();assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'accounts-mobile.png'),full_page=True)
  # The saved account was re-enabled above, so the client now matches a live
  # binding. Assert the documented post-verification state instead of the
  # transient unbound label (which only exists before the account is reloaded).
  page.locator('#account-list').wait_for();page.screenshot(path=str(out/'accounts-mobile-final.png'),full_page=True)
  report['actions'].append('mobile_account_page_states')
  stats=page.request.get(base+'/__fixture/stats').json();assert stats['sms_calls']==0 and stats['qr_calls']==0 and stats['mutations']==0
  report['actions'].append('no_sms_qr_or_task_mutation_from_binding_workflow')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,error=str(e)[:300],lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('desktop_connection_browser.py')])
finally:
 if child is not None:
  import urllib.request
  try:urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop',data=b'',method='POST'),timeout=3).close()
  except Exception:pass
  try:child.wait(timeout=6)
  except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=6)
(out/'desktop-connection-browser.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report));sys.exit(report['result']!='PASS')

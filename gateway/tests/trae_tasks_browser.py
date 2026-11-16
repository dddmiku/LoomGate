"""Production Trae task modules and encrypted cooldown store with synthetic HTTP."""
import argparse,json,os,subprocess,sys,time,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from run_wait import wait_run
from console_actions import choose_provider, choose_account, choose_capability, select_task,plan_timing,plan_times,plan_review
p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);p.add_argument('--fixture',required=True);args=p.parse_args()
root=Path(args.runtime).resolve();out=Path(args.output).resolve();binary=Path(args.fixture).resolve()
if root.name!='catalog-fixture' or any(x.drive.upper()!='D:' for x in [root,out,binary]):raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);base='http://127.0.0.1:17863';report={'actions':[],'page_errors':[],'console_errors':[],'external_requests':[]};stage='start';child=None
def main():
 global stage,child
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000})
  page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__));page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None);page.on('request',lambda r:report['external_requests'].append('external') if urlsplit(r.url).hostname not in [None,'127.0.0.1'] else None)
  def api(path):
   response=page.request.get(base+'/admin/api/'+path);assert response.status==200;return response.json()
  def stats():return page.request.get(base+'/__fixture/trae-stats').json()
  def mode(value):assert page.request.post(base+'/__fixture/mode/'+value).status==204
  def nav(route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+route+'"]').click()
  def tasks():
   nav('overview');page.locator('[data-workspace-agenda]').wait_for();nav('tasks');choose_provider(page,'#task-provider','trae');page.locator('[data-task=balance]').wait_for()
  def run(key,status):
   select_task(page,key,'run-task')
   with page.expect_response(lambda r:r.url.endswith('/api/workbench/run') and r.request.method=='POST') as response:page.locator('#run-form button[type=submit]').click()
   assert response.value.status==202;rid=response.value.json()['run']['id'];page.locator('#dialog').wait_for(state='hidden');return wait_run(page.request,base+'/admin/api/workbench',rid,status,timeout=20)
  fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  stage='directory';tasks();rows=api('workbench/catalog?provider=trae')['tasks'];assert {x['key'] for x in rows}=={'checkin','balance'} and all(x['account_id']=='3001' for x in rows);assert stats()['claims']==0;report['actions'].append('trae_only_directory_with_independent_balance')
  stage='rate';mode('trae-rate');first=run('checkin','manual_action');assert stats()['claims']==1;assert any(s.get('action_outcome')=='rejected' and s.get('upstream_code')==9074 for s in first['steps']);tasks();select_task(page,'checkin');page.get_by_text('退避中',exact=False).wait_for();assert not next(t for t in api('workbench/catalog?provider=trae')['tasks'] if t['key']=='checkin').get('pending_verification');report['actions'].append('explicit_9074_refusal_audited_without_fake_success_or_unknown_lock')
  stage='balance';tasks();before=stats();run('balance','succeeded');s=stats();assert s['reads']==before['reads']+1 and s['claims']==1 and s['credits']==800 and s['expiring']==400;report['actions'].append('actual_pool_balance_and_expiry_refresh_during_checkin_backoff')
  stage='plan';tasks();select_task(page,'balance','task-schedule');plan_timing(page);plan_times(page,['08:13','20:13'],'Asia/Shanghai');plan_review(page);page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden');plan=api('workbench')['schedules'][0];assert plan['task_key']=='balance' and plan['provider_id']=='trae' and plan['account_ids']==['3001'] and plan['minute']==13 and not plan['enabled']
  stage='restart';assert page.request.post(base+'/__fixture/stop').status==204;deadline=time.monotonic()+6
  while time.monotonic()<deadline:
   try:page.request.get(base+'/__fixture/stats',timeout=200)
   except Exception:break
   time.sleep(.1)
  with (root/'stdout.log').open('ab') as stdout,(root/'stderr.log').open('ab') as stderr:child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863','-trae-tasks'],stdout=stdout,stderr=stderr,cwd=root,env={**os.environ,'TEMP':r'D:\LoomGate-work\tmp','TMP':r'D:\LoomGate-work\tmp'},creationflags=subprocess.CREATE_NO_WINDOW)
  (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8');deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   try:
    if page.request.get(base+'/__fixture/stats',timeout=300).status==200:break
   except Exception:pass
   time.sleep(.1)
  else:raise AssertionError('fixture_restart_failed')
  page.reload();tasks();before=stats();run('checkin','manual_action');assert stats()['claims']==1 and stats()['statuses']==before['statuses'];assert api('workbench')['schedules'][0]==plan;report['actions'].append('encrypted_backoff_and_schedule_survive_real_process_restart')
  stage='advance';mode('trae-advance');tasks();run('checkin','succeeded');assert stats()['claims']==2;tasks();run('checkin','skipped');assert stats()['claims']==2;report['actions'].append('after_cooldown_recheck_claim_verify_and_daily_no_replay')
  stage='invalid-balance';before=stats();mode('trae-broken-balance');tasks();run('balance','failed');assert stats()['credits']==before['credits'] and stats()['expiring']==before['expiring'];report['actions'].append('malformed_balance_does_not_replace_pool_with_zero')
  stage='mobile';mode('success');tasks();select_task(page,'checkin');page.locator('#toast').wait_for(state='hidden');page.screenshot(path=str(out/'trae-task-desktop.png'),full_page=True);page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'trae-task-mobile.png'),full_page=True);report['actions'].append('desktop_and_mobile_show_authoritative_daily_status')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('trae_tasks_browser.py')])
finally:
 if child is not None:
  import urllib.request
  try:urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop',data=b'',method='POST'),timeout=3).close()
  except Exception:pass
  try:child.wait(timeout=6)
  except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=6)
(out/'trae-tasks-browser.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report));sys.exit(report['result']!='PASS')

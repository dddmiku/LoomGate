"""Registered production lifecycle modules, fake upstream, real UI and restart."""
import argparse,json,os,subprocess,sys,time,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from run_wait import wait_run
from console_actions import reveal_filter,select_task,plan_timing,plan_times,plan_review
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
  def stats():return page.request.get(base+'/__fixture/task-stats').json()
  def mode(value):assert page.request.post(base+'/__fixture/mode/'+value).status==204
  def nav(route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+route+'"]').click()
  def tasks():
   nav('overview');page.locator('[data-workspace-agenda]').wait_for();nav('tasks');page.locator('[data-task=balance]').first.wait_for();reveal_filter(page,'#task-account');page.locator('#task-account').select_option('1001');page.locator('[data-task=compensation]').wait_for()
  def run(key,status):
   select_task(page,key,'run-task')
   with page.expect_response(lambda r:r.url.endswith('/api/workbench/run') and r.request.method=='POST') as response:page.locator('#run-form button[type=submit]').click()
   assert response.value.status==202;rid=response.value.json()['run']['id'];page.locator('#dialog').wait_for(state='hidden');return wait_run(page.request,base+'/admin/api/workbench',rid,status,timeout=20)
  fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  stage='directory';tasks();catalog=api('workbench/catalog?provider=workbuddy&accounts=1001');assert not catalog['issues']
  expected=['travel/depart','travel/claim','redeem/fixture-future','makeup','resources','compensation'];assert all(any(t['key']==key and t.get('state') for t in catalog['tasks']) for key in expected)
  assert all(stats().get(k,0)==0 for k in ['travel_departures','travel_claims','redemptions','makeups','compensation_claims']);report['actions'].append('read_only_dynamic_lifecycle_directory_and_progress')
  stage='travel';run('travel/depart','succeeded');assert stats()['travel_departures']==1;tasks();select_task(page,'travel/claim');assert page.locator('[data-task-state=manual]').count()==1
  mode('lifecycle-arrived');tasks();run('travel/claim','succeeded');assert stats()['travel_claims']==1;report['actions'].append('dynamic_destination_departure_and_exact_record_reward_readback')
  stage='redemption';tasks();run('redeem/fixture-future','succeeded');assert stats()['redemptions']==1;tasks();run('redeem/fixture-future','succeeded');assert stats()['redemptions']==1;report['actions'].append('dynamic_tier_claim_and_no_repeat_when_claimed')
  stage='makeup';tasks();select_task(page,'makeup');assert '2099-01-02' in page.locator('#task-detail').inner_text();run('makeup','succeeded');assert stats()['makeups']==1;report['actions'].append('server_calendar_one_card_and_target_date_verification')
  stage='resources';tasks();select_task(page,'resources');assert '能量 11' in page.locator('#task-detail').inner_text() and '抽奖机会 4' in page.locator('#task-detail').inner_text();before=stats();run('resources','succeeded');assert stats()==before;report['actions'].append('resource_balances_read_without_consumption')
  stage='uncertain';mode('lifecycle-silent');tasks();unknown=run('compensation','uncertain');assert stats()['compensation_claims']==1
  mode('lifecycle-rollover');tasks();catalog=api('workbench/catalog?provider=workbuddy&accounts=1001');pending=next(t for t in catalog['tasks'] if t['key']=='compensation');assert pending['pending_verification'];select_task(page,'compensation');page.get_by_text('上次操作结果尚未确认。',exact=False).wait_for();page.locator('#toast').wait_for(state='hidden');page.screenshot(path=str(out/'lifecycle-pending-desktop.png'),full_page=True);report['actions'].append('unconfirmed_reward_retains_original_offer_after_rollover')
  stage='plan';select_task(page,'travel/depart','task-schedule');plan_timing(page);plan_times(page,['09:17','21:17'],'UTC');plan_review(page);page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden')
  plans=api('workbench')['schedules'];assert len(plans)==1;plan=plans[0];assert plan['task_key']=='travel/depart' and plan['hours']==[9,21] and plan['minute']==17 and plan['timezone']=='UTC' and plan['account_ids']==['1001'] and not plan['enabled']
  stage='restart';assert page.request.post(base+'/__fixture/stop').status==204;deadline=time.monotonic()+6
  while time.monotonic()<deadline:
   try:page.request.get(base+'/__fixture/stats',timeout=200)
   except Exception:break
   time.sleep(.1)
  with (root/'stdout.log').open('ab') as stdout,(root/'stderr.log').open('ab') as stderr:child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863','-lifecycle-modules'],stdout=stdout,stderr=stderr,cwd=root,env={**os.environ,'TEMP':r'D:\LoomGate-work\tmp','TMP':r'D:\LoomGate-work\tmp'},creationflags=subprocess.CREATE_NO_WINDOW)
  (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8');deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   try:
    if page.request.get(base+'/__fixture/stats',timeout=300).status==200:break
   except Exception:pass
   time.sleep(.1)
  else:raise AssertionError('fixture_restart_failed')
  page.reload();nav('schedules');page.locator('[data-schedule-id="'+plan['id']+'"]').wait_for();saved=api('workbench')['schedules'][0];assert saved==plan
  for enabled in [True,False]:
   with page.expect_response(lambda r:r.url.endswith('/api/workbench/schedule') and r.request.method=='POST') as response:page.locator('[data-action=toggle-schedule]').click()
   assert response.value.status==200;page.get_by_text('已启用' if enabled else '已停用',exact=True).first.wait_for();saved=api('workbench')['schedules'][0];assert saved['enabled']==enabled and bool(saved.get('next_run'))==enabled
  report['actions'].append('plan_time_timezone_scope_and_live_enable_survive_process_restart')
  stage='no-replay';tasks();run('compensation','uncertain');assert stats()['compensation_claims']==1;work=api('workbench');assert any(r['id']==unknown['id'] and r['status']=='uncertain' for r in work['runs']);report['actions'].append('restart_and_explicit_recheck_never_repeat_unknown_claim')
  stage='mobile';tasks();select_task(page,'compensation');page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.locator('#toast').wait_for(state='hidden');page.screenshot(path=str(out/'lifecycle-pending-mobile.png'),full_page=True)
  nav('tasks');reveal_filter(page,'#task-account');page.locator('#task-account').select_option('1003');page.locator('[data-task=balance]').wait_for();assert all(not page.locator('[data-task="'+key+'"]').count() for key in expected);report['actions'].append('mobile_layout_and_global_account_excluded_from_writes')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('lifecycle_modules_browser.py')])
finally:
 if child is not None:
  import urllib.request
  try:urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop',data=b'',method='POST'),timeout=3).close()
  except Exception:pass
  try:child.wait(timeout=6)
  except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=6)
(out/'lifecycle-browser.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report));sys.exit(report['result']!='PASS')

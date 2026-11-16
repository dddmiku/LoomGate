"""Production keepalive modules and storage with local synthetic HTTP only."""
import argparse,json,os,socket,subprocess,sys,time,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from console_actions import choose_provider,choose_account,select_task,plan_timing,plan_times,plan_review
from run_wait import wait_run

p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);p.add_argument('--fixture',required=True);args=p.parse_args()
root,out,binary=Path(args.runtime).resolve(),Path(args.output).resolve(),Path(args.fixture).resolve()
if root.name!='catalog-fixture' or any(x.drive.upper()!='D:' for x in [root,out,binary]):raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);base='http://127.0.0.1:17863';stage='start';child=None
report={'actions':[],'page_errors':[],'console_errors':[],'external_requests':[]}

def main():
 global stage,child
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000})
  page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__))
  page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None)
  page.on('request',lambda r:report['external_requests'].append('external') if urlsplit(r.url).hostname not in (None,'127.0.0.1') else None)
  def api(path):return page.request.get(base+'/admin/api/'+path).json()
  def stats():return page.request.get(base+'/__fixture/refresh-stats').json()
  def nav(route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+route+'"]').click();page.locator('#view > .loading').wait_for(state='detached')
  def tasks(provider='workbuddy'):
   nav('overview');page.locator('[data-workspace-agenda]').wait_for();nav('tasks');choose_provider(page,'#task-provider',provider);page.locator('#task-list.loading').wait_for(state='detached')
  def submit():
   with page.expect_response(lambda r:r.url.endswith('/api/workbench/run') and r.request.method=='POST') as response:page.locator('#run-form button[type=submit]').click()
   assert response.value.status==202;rid=response.value.json()['run']['id'];page.locator('#dialog').wait_for(state='hidden');return wait_run(page.request,base+'/admin/api/workbench',rid,'succeeded',timeout=15)
  fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  stage='discovery';tasks();rows=api('workbench/catalog?provider=workbuddy')['tasks'];assert {x['key'] for x in rows}=={'keepalive'} and {x['account_id'] for x in rows}=={'4101','4102','4103'}
  assert stats()['workbuddy_refreshes']==0;report['actions'].append('real_directory_excludes_manual_review_but_includes_session_recovery')
  stage='run-all';page.locator('[data-action=run-all]').click();run=submit();s=stats();assert s['workbuddy_refreshes']==3
  assert all(a['saved'] and a['rotated'] and not a['disabled'] for a in s['accounts'] if a['id'] in ['4101','4102','4103'])
  assert all(a['disabled'] and not a['rotated'] for a in s['accounts'] if a['id'] in ['4104','4105'])
  report['actions'].append('cn_global_and_session_dead_refresh_save_readback_without_reviving_manual_review')
  stage='plan';tasks();choose_account(page,'#task-account','4102');page.locator('#task-list.loading').wait_for(state='detached');select_task(page,'keepalive','task-schedule');plan_timing(page);plan_times(page,['08:23','20:23'],'UTC');plan_review(page);page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden')
  plan=api('workbench')['schedules'][0];assert plan['task_key']=='keepalive' and plan['account_ids']==['4102'] and plan['hours']==[8,20] and plan['minute']==23 and plan['timezone']=='UTC' and not plan['enabled']
  report['actions'].append('refresh_plan_time_zone_scope_saved_through_real_form')
  stage='restart';assert page.request.post(base+'/__fixture/stop').status==204
  deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   with socket.socket() as sock:
    if sock.connect_ex(('127.0.0.1',17863))!=0:break
   time.sleep(.1)
  else:raise AssertionError('fixture_not_stopped')
  with (root/'stdout.log').open('ab') as stdout,(root/'stderr.log').open('ab') as stderr:child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863','-keepalive-modules'],cwd=root,env={**os.environ,'TEMP':r'D:\LoomGate-work\tmp','TMP':r'D:\LoomGate-work\tmp'},stdout=stdout,stderr=stderr,creationflags=subprocess.CREATE_NO_WINDOW)
  (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8');deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   try:
    if page.request.get(base+'/__fixture/stats',timeout=300).status==200:break
   except Exception:pass
   time.sleep(.1)
  else:raise AssertionError('fixture_not_ready')
  page.reload();page.locator('h1').wait_for();assert api('workbench')['schedules'][0]==plan;assert stats()['workbuddy_refreshes']==3
  assert all(a['saved'] and a['rotated'] for a in stats()['accounts'] if a['id'] in ['4101','4102','4103'])
  report['actions'].append('actual_process_restart_preserves_rotation_and_plan_without_refresh_replay')
  stage='plan-toggle';nav('schedules');page.locator('[data-action=toggle-schedule]').click();page.get_by_text('已启用',exact=True).first.wait_for();enabled=api('workbench')['schedules'][0];assert enabled['enabled'] and enabled['next_run']
  page.locator('[data-action=toggle-schedule]').click();page.get_by_text('已停用',exact=True).first.wait_for();assert not api('workbench')['schedules'][0]['enabled']
  stage='trae';tasks('trae');assert page.locator('[data-task=keepalive]').count()==1;select_task(page,'keepalive','run-task');submit();assert stats()['trae_refreshes']==1;row=next(a for a in stats()['accounts'] if a['id']=='4201');assert row['saved'] and row['rotated']
  report['actions'].append('live_schedule_switch_and_trae_official_exchange_module')
  stage='screenshots';tasks('trae');select_task(page,'keepalive');page.locator('#toast').wait_for(state='hidden');page.screenshot(path=str(out/'keepalive-desktop.png'),full_page=True);page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'keepalive-mobile.png'),full_page=True)
  report['actions'].append('responsive_real_task_controls')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('keepalive_browser.py')])
finally:
 if child is not None:
  import urllib.request
  try:urllib.request.urlopen(urllib.request.Request(base+'/__fixture/stop',data=b'',method='POST'),timeout=3).close()
  except Exception:pass
  try:child.wait(timeout=6)
  except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=6)
(out/'keepalive-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8');print(json.dumps(report,ensure_ascii=False));sys.exit(report['result']!='PASS')

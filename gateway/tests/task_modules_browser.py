"""Exercise real registered task modules through the synthetic panel runtime."""
import argparse,json,sys,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from run_wait import wait_run
from console_actions import reveal_filter, select_task, plan_timing, plan_times, plan_review
p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);args=p.parse_args();root=Path(args.runtime).resolve();out=Path(args.output).resolve()
if root.name!='catalog-fixture' or root.drive.upper()!='D:' or out.drive.upper()!='D:':raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);base='http://127.0.0.1:17863';report={'actions':[],'page_errors':[],'console_errors':[],'external_requests':[]};stage='start'
def main():
 global stage
 with sync_playwright() as pw:
  b=pw.chromium.launch(headless=True);page=b.new_page(viewport={'width':1440,'height':1000});page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__));page.on('console',lambda m:report['console_errors'].append(m.text[:180]) if m.type=='error' else None);page.on('request',lambda r:report['external_requests'].append(urlsplit(r.url).hostname) if urlsplit(r.url).hostname not in [None,'127.0.0.1'] else None)
  def api(path):return page.request.get(base+'/admin/api/'+path).json()
  def stats():return page.request.get(base+'/__fixture/task-stats').json()
  def done(x):report['actions'].append(x)
  def nav(route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+route+'"]').click()
  def submit(status):
   with page.expect_response(lambda r:r.url.endswith('/api/workbench/run') and r.request.method=='POST') as response:
    page.locator('#run-form button[type=submit]').click()
   result=response.value.json();rid=result['run']['id'];page.locator('#dialog').wait_for(state='hidden')
   return wait_run(page.request,base+'/admin/api/workbench',rid,status)
  fields=dict(line.split(':',1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  page.request.post(base+'/__fixture/mode/success');initial=stats()
  stage='discover';nav('tasks');page.locator('[data-task=balance]').first.wait_for();assert page.locator('[data-task=balance]').count()==3;assert page.locator('[data-task=checkin]').count()==2;assert page.locator('[data-task="growth/fixture-growth"]').count()==2;done('registered_tasks_dynamic_growth_and_global_scope')
  stage='balance';reveal_filter(page,'#task-account');page.locator('#task-account').select_option('1003');page.locator('[data-task=balance]').wait_for();assert page.locator('[data-task=checkin]').count()==0
  select_task(page,'balance','run-task');submit('succeeded');s=stats();assert s['balance_reads']==initial['balance_reads']+1 and s['signins']==initial['signins'] and s['claims']==initial['claims']
  assert stats()['global_credits']==750 and stats()['global_expiring']==750;done('balance_only_updates_actual_pool_without_signin')
  stage='partial';page.request.post(base+'/__fixture/mode/failed');nav('tasks');reveal_filter(page,'#task-account');page.locator('#task-account').select_option('');page.locator('[data-discovery-issues="2"]').wait_for();assert page.locator('[data-task=balance]').count()==3
  page.screenshot(path=str(out/'tasks-partial-desktop.png'),full_page=True)
  page.locator('[data-action=run-all]').click();run=submit('failed')
  assert len(run['discovery_issues'])==2;assert any(x['task_key']=='balance' for x in run['steps']);assert stats()['balance_reads']>=initial['balance_reads']+4;done('partial_failure_visible_and_other_modules_still_execute')
  stage='schedule';nav('tasks');page.locator('[data-task=balance]').first.wait_for();select_task(page,'balance','task-schedule',first=True);page.locator('#plan-next:not([disabled])').wait_for();assert page.locator('#plan-task').input_value()=='balance';plan_timing(page);plan_times(page,['08:00','20:00'],'UTC');plan_review(page);page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden');plan=next(s for s in api('workbench')['schedules'] if s['task_key']=='balance');assert plan['timezone']=='UTC' and plan['hours']==[8,20] and not plan['enabled'];done('module_task_schedule_persists_without_engine_specific_code')
  stage='mobile';nav('tasks');page.locator('[data-discovery-issues]').wait_for();page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'tasks-partial-mobile.png'),full_page=True);done('partial_catalog_mobile_layout')
  stage='recover';page.request.post(base+'/__fixture/mode/success');nav('overview');page.locator('[data-workspace-agenda]').wait_for();nav('tasks');page.locator('[data-task="growth/fixture-growth"]').first.wait_for();assert page.locator('[data-discovery-issues]').count()==0
  reveal_filter(page,'#task-account');page.locator('#task-account').select_option('1001');select_task(page,'growth/fixture-growth','run-task');submit('succeeded');assert stats()['claims']<=initial['claims']+1;done('growth_recovery_claim_and_authoritative_verification')
  nav('schedules');page.locator('[data-schedule-id="'+plan['id']+'"]').wait_for();page.locator('[data-schedule-id]').filter(has=page.get_by_text(plan['id'],exact=True)).locator('[data-action=delete-schedule]').click();page.locator('#delete-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden');assert not api('workbench')['schedules'];done('synthetic_schedule_cleanup')
  assert not report['page_errors'] and not report['console_errors'] and not report['external_requests'];b.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',failed_stage=stage,error_type=type(e).__name__,source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('task_modules_browser.py')])
finally:(out/'task-modules-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ['result','failed_stage','error_type'] if k in report}));print('actions='+str(len(report['actions'])));sys.exit(0 if report['result']=='PASS' else 1)

"""Behavioral regression for task-first schedule setup in the synthetic console."""
import argparse,json,sys,traceback
from pathlib import Path
from playwright.sync_api import sync_playwright
from console_actions import choose_provider,select_task,plan_times

p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);args=p.parse_args()
root=Path(args.runtime).resolve();out=Path(args.output).resolve()
if root.name!='catalog-fixture' or root.drive.upper()!='D:' or out.drive.upper()!='D:':raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);report={'actions':[],'page_errors':[],'external_requests':[]};stage='start'

def main():
 global stage
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000});base='http://127.0.0.1:17863'
  page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__))
  page.on('request',lambda r:report['external_requests'].append('external') if not r.url.startswith(base+'/') else None)
  assert page.request.get(base+'/__fixture/stats').status==200
  page.request.post(base+'/__fixture/mode/success')
  def api():return page.request.get(base+'/admin/api/workbench').json()
  def nav(route):
   if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
   page.locator('#navigation a[href="#'+route+'"]').click();page.locator('#view>.loading').wait_for(state='detached')
  fields=dict(line.split(':',1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
  baseline=api()['schedules'];writes=[]
  page.on('request',lambda r:writes.append('schedule') if r.url.endswith('/api/workbench/schedule') and r.method=='POST' else None)
  stage='existing-task-entry';nav('schedules');page.locator('[data-action=new-schedule]').click();page.wait_for_function("location.hash==='#tasks'")
  page.locator('[data-task]').first.wait_for();choose_provider(page,'#task-provider','doubao');page.locator('[data-task]').first.wait_for()
  task_buttons=page.locator('[data-action=select-task-group]');picked=False
  for i in range(task_buttons.count()):
   task_buttons.nth(i).click()
   if page.locator('#task-detail [data-action=task-schedule-group]').count():picked=True;break
  assert picked,'no schedulable existing task group'
  task_name=page.locator('#task-detail .detail-title').inner_text()
  expected_accounts=[x.get_attribute('data-account-task') for x in page.locator('#task-detail .account-task-row').all()]
  page.locator('#task-detail [data-action=task-schedule-group]').click()
  assert page.locator('[data-plan-step="1"]').is_visible() and not page.locator('[data-plan-step="0"]').is_visible()
  assert not page.locator('#plan-provider').is_visible() and not page.locator('#plan-task').is_visible() and not page.locator('#plan-accounts').is_visible()
  assert page.locator('#plan-save').is_visible() and not page.locator('#plan-next').is_visible()
  report['actions'].append('existing_task_opens_time_only_editor')
  stage='timing-validation'
  for values,message in [(['08:20','08:20'],'请填写有效且不重复的执行时间。'),(['08:20','18:21'],'同一计划的多个时段需使用相同分钟。不同分钟请分别创建计划。')]:
   plan_times(page,values);page.locator('#plan-save').click();page.get_by_text(message,exact=True).wait_for();assert not writes and api()['schedules']==baseline
  plan_times(page,['08:20','18:20'],'Invalid/Nowhere');page.locator('#plan-save').click();page.get_by_text('时区无效，请选择有效时区，例如 Asia/Shanghai。',exact=True).wait_for();assert not writes and api()['schedules']==baseline
  report['actions'].append('invalid_timing_rejected_without_write')
  stage='save-one-step';plan_times(page,['08:35','17:35'],'UTC');page.screenshot(path=str(out/'task-plan-time-only.png'),full_page=True)
  page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden');page.wait_for_function("location.hash==='#schedules'")
  saved=[x for x in api()['schedules'] if x['id'] not in {s['id'] for s in baseline}]
  assert len(writes)==1 and len(saved)==1;record=saved[0]
  if not (record['hours']==[8,17] and record['minute']==35 and record['timezone']=='UTC' and record['account_ids']==expected_accounts and not record['enabled'] and record['once_per_day']):
   report['saved_schedule_summary']={k:record.get(k) for k in ('task_key','provider_id','enabled','hours','minute','timezone','account_ids','once_per_day')};raise AssertionError('saved_schedule_does_not_match_requested_timing_or_scope')
  assert record['task_key'] and task_name
  report['actions'].append('single_submit_saves_exact_task_scope_and_timing')
  stage='edit-cancel';row=page.locator('[data-schedule-id="'+record['id']+'"]');row.locator('[data-action=edit-schedule]').click()
  edit_state={'save_visible':page.locator('#plan-save').is_visible(),'provider_visible':page.locator('#plan-provider').is_visible(),'task_visible':page.locator('#plan-task').is_visible()};report['edit_state']=edit_state;assert edit_state=={'save_visible':True,'provider_visible':False,'task_visible':False}
  plan_times(page,['10:00'],'Asia/Shanghai');page.locator('#dialog-close').click();assert next(x for x in api()['schedules'] if x['id']==record['id'])==record and len(writes)==1
  row.locator('[data-action=edit-schedule]').click();plan_times(page,['11:40'],'Asia/Shanghai');page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden')
  edited=next(x for x in api()['schedules'] if x['id']==record['id']);assert edited['hours']==[11] and edited['minute']==40 and edited['timezone']=='Asia/Shanghai' and edited['account_ids']==record['account_ids'] and edited['task_key']==record['task_key'] and len(writes)==2
  record=edited;report['actions'].append('existing_plan_save_reads_back_time_and_preserves_task_scope')
  stage='mobile-detail';page.set_viewport_size({'width':390,'height':844});nav('tasks');choose_provider(page,'#task-provider','doubao');page.locator('[data-task]').first.wait_for()
  row=page.locator('[data-task]').first;key=row.get_attribute('data-task');select_task(page,key)
  assert not page.locator('.selection-pane').is_visible() and page.locator('#task-detail').is_visible()
  assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
  page.locator('[data-action=task-back]').click();assert page.locator('[data-task="'+key+'"]').evaluate('(e)=>e===document.activeElement')
  report['actions'].append('mobile_detail_back_focus_and_no_horizontal_overflow')
  stage='cleanup';nav('schedules');page.locator('[data-schedule-id="'+record['id']+'"] [data-action=delete-schedule]').click();page.locator('#delete-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden');assert api()['schedules']==baseline
  report['final_counts']={'schedule_writes':len(writes),'page_errors':len(report['page_errors']),'external_requests':len(report['external_requests'])}
  assert len(writes)==2 and not report['page_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',failed_stage=stage,error_type=type(e).__name__,source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('plan_editor_browser.py')])
(out/'plan-editor-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ('result','failed_stage','error_type','source_lines') if k in report}));print('actions='+str(len(report['actions'])));sys.exit(0 if report['result']=='PASS' else 1)

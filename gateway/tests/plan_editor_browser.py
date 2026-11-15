"""Behavioral regression for real progressive plan persistence and navigation.
Uses only the synthetic catalog-workbench runtime; no real provider account.
"""
import argparse,json,sys,traceback
from pathlib import Path
from playwright.sync_api import sync_playwright
from console_actions import reveal_filter,select_task,plan_timing,plan_times,plan_review
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
   page.locator('#navigation a[href="#'+route+'"]').click()
   page.locator('#view>.loading').wait_for(state='detached')
  fields=dict(line.split(':',1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
  page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-agenda]').wait_for()
  baseline=api()['schedules'];writes=[]
  page.on('request',lambda r:writes.append('schedule') if r.url.endswith('/api/workbench/schedule') and r.method=='POST' else None)
  nav('schedules');page.locator('[data-action=new-schedule]').click();page.locator('#plan-provider').select_option('doubao');page.locator('#plan-next:not([disabled])').wait_for()
  stage='work-required';page.locator('#plan-next').click();page.get_by_text('请选择有效任务，并至少选择一个账号。',exact=True).wait_for();assert not writes
  page.locator('#plan-task').select_option('fixture-check');page.locator('input[name=plan-account]').first.check();plan_timing(page)
  assert not page.locator('#plan-provider').is_visible() and not page.locator('#plan-timezone').is_visible() and not page.locator('#plan-enabled').is_visible()
  report['actions'].append('only_current_decision_visible_no_early_write')
  stage='timing-errors'
  for values,message in [(['08:20','08:20'],'请填写有效且不重复的执行时间。'),(['08:20','18:21'],'同一计划的多个时段需使用相同分钟。不同分钟请分别创建计划。')]:
   plan_times(page,values);page.locator('#plan-next').click();page.get_by_text(message,exact=True).wait_for();assert not writes
  plan_times(page,['08:35','17:35'],'Invalid/Nowhere');page.locator('#plan-next').click();page.get_by_text('时区无效，请选择有效时区，例如 Asia/Shanghai。',exact=True).wait_for();assert api()['schedules']==baseline
  report['actions'].append('duplicates_minute_contract_and_invalid_zone_rejected_without_write')
  stage='back-persistence';plan_times(page,['08:35','17:35'],'UTC');page.locator('#plan-daily').uncheck();page.locator('#plan-back').click()
  assert page.locator('#plan-task').input_value()=='fixture-check' and page.locator('input[name=plan-account]').first.is_checked()
  plan_timing(page)
  assert [x.input_value() for x in page.locator('[name=plan-time]').all()]==['08:35','17:35']
  plan_review(page);assert '08:35、17:35' in page.locator('#plan-review').inner_text() and 'UTC' in page.locator('#plan-review').inner_text()
  assert not page.locator('#plan-enabled').is_checked() and not writes
  page.screenshot(path=str(out/'plan-review-desktop.png'),full_page=True)
  report['actions'].append('back_navigation_preserves_draft_and_summary_matches')
  stage='save';page.locator('#plan-save').evaluate('(b)=>{b.click();b.click()}');page.locator('#dialog').wait_for(state='hidden')
  saved=[x for x in api()['schedules'] if x['id'] not in {s['id'] for s in baseline}];assert len(writes)==1 and len(saved)==1
  record=saved[0];sid=record['id'];assert record['hours']==[8,17] and record['minute']==35 and record['timezone']=='UTC' and record['account_ids']==['a-fixture'] and not record['enabled'] and not record['once_per_day']
  report['actions'].append('one_final_submit_verified_from_server')
  stage='cancel-edit';row=page.locator('[data-schedule-id="'+sid+'"]');row.locator('[data-action=edit-schedule]').click();plan_timing(page)
  assert [x.input_value() for x in page.locator('[name=plan-time]').all()]==['08:35','17:35'];plan_times(page,['10:00'],'Asia/Shanghai');page.locator('#dialog-close').click()
  assert next(x for x in api()['schedules'] if x['id']==sid)==record and len(writes)==1
  report['actions'].append('cancel_edit_preserves_persisted_plan')
  stage='mobile-detail';page.set_viewport_size({'width':390,'height':844});nav('tasks');page.locator('#task-provider').select_option('doubao');page.locator('[data-task=fixture-check]').wait_for()
  assert page.locator('.selection-pane').is_visible() and not page.locator('#task-detail').is_visible()
  select_task(page,'fixture-check');assert not page.locator('.selection-pane').is_visible();assert page.locator('#task-detail [data-action=run-task]').is_visible();page.screenshot(path=str(out/'task-detail-mobile.png'),full_page=True)
  page.locator('[data-action=task-back]').click();assert page.locator('[data-task=fixture-check]').evaluate('(e)=>e===document.activeElement');assert page.locator('.selection-pane').is_visible()
  nav('models');page.locator('[data-model-id]').first.wait_for();page.locator('[data-model-id]').first.click();assert page.locator('#model-detail').is_visible();page.locator('[data-action=model-back]').click();assert page.locator('.selection-row[aria-selected=true]').evaluate('(e)=>e===document.activeElement')
  report['actions'].append('mobile_list_detail_back_and_focus_restore')
  stage='mobile-plan';nav('schedules');page.locator('[data-schedule-id="'+sid+'"] [data-action=edit-schedule]').click();plan_timing(page);page.screenshot(path=str(out/'plan-time-mobile.png'),full_page=True)
  assert page.locator('#plan-next').is_visible() and page.locator('#dialog-title').is_visible();page.keyboard.press('Escape');assert not page.locator('#dialog').is_visible()
  report['actions'].append('mobile_progressive_edit_keyboard_cancel')
  stage='cleanup';page.locator('[data-schedule-id="'+sid+'"] [data-action=delete-schedule]').click();page.locator('#delete-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden');assert api()['schedules']==baseline
  assert not report['page_errors'] and not report['external_requests'];browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',failed_stage=stage,error_type=type(e).__name__,source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('plan_editor_browser.py')])
(out/'plan-editor-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ('result','failed_stage','error_type','source_lines') if k in report}));print('actions='+str(len(report['actions'])));sys.exit(0 if report['result']=='PASS' else 1)

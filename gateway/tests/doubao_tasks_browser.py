"""Real panel, production task adapter, encrypted state; synthetic upstream only."""
import argparse,json,os,socket,subprocess,sys,time,traceback
sys.path.insert(0,str(__import__("pathlib").Path(__file__).resolve().parent))
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from run_wait import wait_run
from console_actions import choose_provider,choose_account,select_task,plan_timing,plan_times,plan_review

p=argparse.ArgumentParser();p.add_argument('--runtime',required=True);p.add_argument('--output',required=True);p.add_argument('--fixture',required=True);p.add_argument('--withhold',action='store_true');a=p.parse_args()
root,out,binary=[Path(v).resolve() for v in [a.runtime,a.output,a.fixture]]
assert root.name=='catalog-fixture' and all(v.drive.upper()=='D:' for v in [root,out,binary])
root.mkdir(parents=True,exist_ok=True);out.mkdir(parents=True,exist_ok=True)
base='http://127.0.0.1:17863';child=None;report={'actions':[],'real_upstream_mutations':0,'page_errors':[],'console_errors':[]};stage='start'
env={**os.environ,'TEMP':'D:/LoomGate-work/tmp','TMP':'D:/LoomGate-work/tmp','PYTHONDONTWRITEBYTECODE':'1'}

def start(request):
 global child
 with socket.socket() as sock:assert sock.connect_ex(('127.0.0.1',17863))!=0,'port_in_use'
 with (root/'stdout.log').open('ab') as stdout,(root/'stderr.log').open('ab') as stderr:
  child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863','-doubao-work'],cwd=root,env=env,stdout=stdout,stderr=stderr,creationflags=subprocess.CREATE_NO_WINDOW)
 (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'binary':str(binary),'port':17863}),encoding='utf-8')
 end=time.monotonic()+12
 while time.monotonic()<end:
  try:
   if request.get(base+'/__fixture/work-stats',timeout=300).status==200:return
  except Exception:pass
  if child.poll() is not None:raise AssertionError('fixture_exited')
  time.sleep(.1)
 raise AssertionError('fixture_not_ready')

def stop(request):
 global child
 if child is not None and child.poll() is None:
  assert request.post(base+'/__fixture/stop').status==204
  child.wait(timeout=8)
  assert child.returncode==0
 child=None

def main():
 global stage
 with sync_playwright() as pw:
  browser=pw.chromium.launch(headless=True)
  try:
   page=browser.new_page(viewport={'width':1440,'height':1000});page.set_default_timeout(15000)
   page.on('pageerror',lambda _:report['page_errors'].append('page_error'))
   page.on('console',lambda m:report['console_errors'].append('console_error') if m.type=='error' else None)
   start(page.request)
   assert page.request.get(base+'/__fixture/work-stats').json()['generated']==0,'used_fixture_do_not_replay'
   def api(path):
    reply=page.request.get(base+'/admin/api/'+path);assert reply.status==200;return reply.json()
   def stats():return page.request.get(base+'/__fixture/work-stats').json()
   def tasks():
    if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
    page.locator('#navigation a[href="#overview"]').click();page.locator('[data-workspace-overview]').wait_for()
    page.locator('#navigation a[href="#tasks"]').click();choose_provider(page,'#task-provider','doubao');choose_account(page,'#task-account','a-fixture');page.locator('[data-task="synthetic-work-group:synthetic-work-task"]').wait_for()
   def run(expected):
    select_task(page,'synthetic-work-group:synthetic-work-task','run-task')
    with page.expect_response(lambda r:r.url.endswith('/workbench/run') and r.request.method=='POST') as reply:page.locator('#run-form button[type=submit]').click()
    assert reply.value.status==202;rid=reply.value.json()['run']['id'];page.locator('#dialog').wait_for(state='hidden');return wait_run(page.request,base+'/admin/api/workbench',rid,expected,timeout=15)
   fields=dict(x.split(':',1) for x in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in x)
   page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
   stage='task-directory';tasks();task=next(t for t in api('workbench/catalog?provider=doubao')['tasks'] if t['account_id']=='a-fixture');assert task['state']['phase']=='ready';assert 'work_configurable' not in task
   assert page.locator('[data-action="configure-work"]').count()==0
   for route in ['works','work-status','artifact','work-input']:
    response=page.request.get(base+'/admin/api/workbench/doubao/'+route);report.setdefault('removed_route_status',{})[route]=response.status;assert response.status==503 and response.json().get('ok') is False
   response=page.request.post(base+'/admin/api/workbench/doubao/work-input',data={},headers={'X-Admin-Request':'1'});assert response.status==404
   report['actions'].append('general_work_routes_and_editor_absent_task_directory_kept')
   stage='plan';tasks();select_task(page,task['key'],'task-schedule');plan_timing(page);plan_times(page,['08:13','20:13'],'Asia/Shanghai');plan_review(page);page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden');plan=api('workbench')['schedules'][0];assert plan['account_ids']==['a-fixture'] and not plan['enabled'] and plan['timezone']=='Asia/Shanghai'
   stage='execute';tasks()
   if a.withhold:assert page.request.post(base+'/__fixture/work-mode/withhold').status==204
   first=run('uncertain' if a.withhold else 'succeeded');assert stats()['generated']==1 and stats()['claims']==1
   report['actions'].append('generate_claim_and_reward_missing_guard' if a.withhold else 'generate_claim_new_reward_verified')
   stage='restart';stop(page.request);start(page.request);page.reload();tasks();assert api('workbench')['schedules'][0]==plan
   run('uncertain' if a.withhold else 'skipped');assert stats()['generated']==1 and stats()['claims']==1;report['actions'].append('real_process_restart_preserves_plan_and_blocks_replay')
   if a.withhold:
    stage='late-proof';assert page.request.post(base+'/__fixture/work-mode/publish').status==204;tasks();run('succeeded');assert stats()['generated']==1 and stats()['claims']==1;report['actions'].append('late_reward_proof_resolves_without_claim_or_generation_replay')
   stage='view';tasks();select_task(page,task['key']);assert page.locator('[data-action="configure-work"]').count()==0
   page.screenshot(path=str(out/'doubao-tasks-desktop.png'),full_page=True);page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');page.screenshot(path=str(out/'doubao-tasks-mobile.png'),full_page=True)
   report['actions'].append('desktop_mobile_task_and_reward_status');assert not report['page_errors'];assert not report['console_errors'];report['stats']=stats()
  finally:
   stop(page.request);browser.close()

try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',stage=stage,error_type=type(e).__name__,lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('doubao_tasks_browser.py')])
(out/'doubao-tasks-browser.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report));raise SystemExit(report['result']!='PASS')

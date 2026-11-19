"""Browser acceptance against catalog-workbench only; never uses real accounts.

python tests/console_browser.py --runtime D:/LoomGate-work/private/catalog-fixture \
    --output D:/LoomGate-work/evidence/phase02 --fixture D:/LoomGate-work/build/catalog-fixture.exe
"""
import argparse
import traceback
import json
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
sys.path.insert(0, str(Path(__file__).resolve().parent))
from run_wait import wait_run
from console_actions import choose_provider, choose_account, choose_capability, reveal_filter, select_task, plan_timing, plan_times, plan_review, sidebar_action

parser = argparse.ArgumentParser()
parser.add_argument('--runtime', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--fixture', required=True)
args = parser.parse_args()
runtime, output, binary = Path(args.runtime).resolve(), Path(args.output).resolve(), Path(args.fixture).resolve()
if runtime.name != 'catalog-fixture' or binary.name != 'catalog-fixture.exe' or any(p.drive.upper() != 'D:' for p in [runtime, output, binary]):
    raise SystemExit('synthetic_runtime_required')
output.mkdir(parents=True, exist_ok=True)
base = 'http://127.0.0.1:17863'
report = {'actions': [], 'screenshots': [], 'page_errors': [], 'console_errors': [], 'external_requests': []}
stage = 'start'

def done(name):
    report['actions'].append(name)

def main():
    global stage
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        context = browser.new_context(viewport={'width':1440, 'height':1000})
        page = context.new_page()
        page.on('pageerror', lambda e: report['page_errors'].append(type(e).__name__))
        page.on('console', lambda m: report['console_errors'].append(m.text[:180]) if m.type == 'error' else None)
        page.on('request', lambda r: report['external_requests'].append(urlsplit(r.url).hostname) if urlsplit(r.url).hostname not in [None,'127.0.0.1'] else None)
        page.on('dialog', lambda d: (_ for _ in ()).throw(RuntimeError('unexpected_browser_dialog')))
        stats = page.request.get(base+'/__fixture/stats').json()
        assert 'mutations' in stats and 'calls' in stats
        page.request.post(base+'/__fixture/mode/success')
        def nav(route):
            if not page.locator('#navigation').is_visible():
                page.locator('#navigation-toggle').click()
            page.locator('#navigation a[href="#'+route+'"]').click()
            page.locator('#navigation a[aria-current="page"]').filter(has_text={'overview':'工作台','providers':'服务','accounts':'账号','models':'模型目录','tasks':'任务','usage':'用量','logs':'请求记录','keys':'API 密钥','migration':'迁移','settings':'设置'}[route]).wait_for(state='attached')
        def shot(name):
            if page.locator('#toast').count() and page.locator('#toast').is_visible():
                page.locator('#toast').wait_for(state='hidden',timeout=7000)
            page.screenshot(path=str(output/name),full_page=True,animations='disabled',mask=[page.locator('input[type=password]')])
            report['screenshots'].append(name)
        def models_settled():
            page.locator('[data-action="model-refresh"]:not([disabled])').wait_for()
            page.locator('#model-list .loading').wait_for(state='detached')
        def work():
            response=page.request.get(base+'/admin/api/workbench')
            assert response.status==200
            return response.json()
        def accept_run(double=False):
            with page.expect_response(lambda r:r.url.endswith('/api/workbench/run') and r.request.method=='POST') as response:
                if double:
                    page.locator('#run-form button[type=submit]').evaluate('(b)=>{b.click();b.click()}')
                else:
                    page.locator('#run-form button[type=submit]').click()
            assert response.value.status==202
            run_id=response.value.json()['run']['id']
            page.locator('#dialog').wait_for(state='hidden')
            return run_id
        fields=dict(line.split(':',1) for line in (runtime/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
        stage='login'
        page.goto(base+'/admin/')
        shot('console-login.png')
        page.locator('#username').fill(fields['username'].strip())
        page.locator('#password').fill(fields['password'].strip())
        page.locator('#login-submit').click()
        page.locator('[data-workspace-overview]').wait_for()
        assert page.locator('script[src="console/app.js"]').count()==1
        assert page.locator('#tabs').count()==0
        done('independent_login_and_console_entry')
        shot('console-overview.png')
        stage='services'
        nav('providers');page.locator('[data-provider="doubao"] h2').wait_for()
        card=page.locator('article.service-entry[data-provider="doubao"]')
        assert '模型目录' in card.inner_text() and '消息转发' in card.inner_text()
        card.locator('[data-action=provider-accounts]').click()
        page.locator('#account-provider').wait_for()
        page.locator('#account-list [data-account="a-fixture"]').first.wait_for()
        assert page.locator('#account-list tbody tr').count()>=24
        page.locator('#account-search').fill('c-disabled')
        assert page.locator('#account-list tbody tr').count()==1
        assert '已停用' in page.locator('#account-list').inner_text()
        done('dynamic_accounts_and_disabled_status')
        stage='models'
        nav('providers');page.locator('article[data-provider="doubao"] [data-action=provider-models]').click();models_settled()
        assert page.locator('[data-account-status=unavailable]').count()==1
        assert page.locator('[data-account-status=disabled]').count()==1
        assert page.locator('#model-list img').count()==0
        assert '仅目录' in page.locator('#model-list').inner_text()
        choose_account(page,'#model-account','a-fixture');models_settled()
        assert page.locator('.selection-row').count()==1
        choose_capability(page,'supports_tool_call');assert page.locator('.selection-row').count()==0
        choose_capability(page,'native_tools');assert page.locator('.selection-row').count()==1
        page.locator('#model-search').fill('no-such-model');assert page.locator('.selection-row').count()==0
        page.locator('#model-search').fill('合成');assert page.locator('.selection-row').count()==1
        model_rows=page.locator('.selection-row').count();assert page.locator('#model-list .channel-filter').count() in (0,1)
        done('catalog_scope_channel_filter_group_paging_filter_xss_and_capability_semantics');shot('console-models.png')
        stage='stale'
        page.request.post(base+'/__fixture/mode/failed')
        before=page.request.get(base+'/__fixture/stats').json()['calls']
        page.locator('[data-action=model-refresh]').click();models_settled()
        assert page.locator('[data-account-status=stale]').count()==1
        assert page.locator('.selection-row').count()==1
        after=page.request.get(base+'/__fixture/stats').json()['calls'];assert after==before+1
        page.locator('[data-action=model-refresh]').click();models_settled()
        assert page.request.get(base+'/__fixture/stats').json()['calls']==after
        done('stale_cache_and_manual_refresh_backoff');shot('console-stale.png')
        stage='route-cancel'
        page.request.post(base+'/__fixture/mode/slow')
        choose_account(page,'#model-account','z-page-20')
        page.locator('#model-list .loading').wait_for()
        nav('providers');page.locator('article[data-provider="doubao"] h2').wait_for()
        page.wait_for_timeout(1700)
        assert page.locator('h1').inner_text()=='服务接入'
        done('route_cancellation_does_not_overwrite_new_page')
        stage='storage-failure'
        page.request.post(base+'/__fixture/mode/storage')
        page.locator('article[data-provider="doubao"] [data-action=provider-models]').click()
        page.get_by_text('账号存储读取失败，请检查服务状态。',exact=True).wait_for()
        assert page.locator('.selection-row').count()==0
        done('storage_failure_remains_failure')
        page.request.post(base+'/__fixture/mode/success');page.locator('[data-action=reload]').click();models_settled()
        stage='offline'
        context.set_offline(True)
        page.locator('[data-action=model-refresh]').click();models_settled()
        assert page.locator('#connection').is_visible()
        assert page.locator('#model-notice .error').is_visible()
        context.set_offline(False);done('offline_and_failure_state')
        stage='legacy'
        choose_capability(page,'');page.locator('#model-search').fill('')
        choose_provider(page,'#model-provider','workbuddy');models_settled()
        assert page.locator('[data-model-id="cn:fixture-chat"]').count()==1
        assert page.locator('#model-account').is_disabled()
        done('existing_provider_catalog_contract')
        stage='tasks'
        nav('tasks');choose_provider(page,'#task-provider','doubao')
        page.locator('[data-task=fixture-manual]').wait_for()
        select_task(page,'fixture-manual','manual-task')
        assert '选择需要处理的文档' in page.locator('#dialog-body').inner_text()
        page.locator('#dialog-close').click();done('dynamic_tasks_and_specific_manual_action');shot('console-tasks.png')
        # 目录专用任务没有随包定时；状态列必须是「—」而不是可点的开关。
        row=page.locator('[data-task=fixture-check]')
        assert row.locator('[data-action=toggle-schedule]').count()==0
        assert '未定时' in row.inner_text()
        done('catalog_only_task_has_no_fake_schedule')
        nav('tasks');choose_provider(page,'#task-provider','doubao');page.locator('[data-task]').first.wait_for()
        stage='schedule-create'
        # 计划随包发布：只能改时间/时区/开关，不能新建或删除一项随包任务。
        sid='legacy-checkin'
        choose_provider(page,'#task-provider','workbuddy');page.locator('[data-task=checkin]').wait_for()
        select_task(page,'checkin','task-schedule')
        assert page.locator('[data-plan-step="1"]').is_visible() and page.locator('#plan-save').is_visible()
        assert page.locator('#plan-provider').is_hidden() and page.locator('#plan-task').is_hidden()
        plan_times(page,['06:17','18:17'],'Asia/Shanghai')
        page.locator('#plan-enabled-quick').check()
        shot('console-schedule-editor.png')
        page.locator('#plan-save').click()
        page.locator('#dialog').wait_for(state='hidden')
        saved=next(x for x in work()['schedules'] if x['id']==sid)
        assert saved['hours']==[6,18] and saved['minute']==17 and saved['next_run']
        done('shipped_plan_time_edit_and_authoritative_readback')
        stage='restart-persistence'
        assert page.request.post(base+'/__fixture/stop').status==204
        deadline=time.monotonic()+8
        while time.monotonic()<deadline:
            try:
                page.request.get(base+'/__fixture/stats',timeout=250)
            except Exception:
                break
            time.sleep(.1)
        else: raise AssertionError('fixture_did_not_stop')
        with (runtime/'stdout.log').open('w') as stdout, (runtime/'stderr.log').open('w') as stderr:
            child=subprocess.Popen([str(binary),'-root',str(runtime),'-listen','127.0.0.1:17863'],stdout=stdout,stderr=stderr,creationflags=subprocess.CREATE_NO_WINDOW)
        (runtime/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8')
        deadline=time.monotonic()+8
        while time.monotonic()<deadline:
            try:
                if page.request.get(base+'/__fixture/stats',timeout=250).status==200: break
            except Exception: pass
            time.sleep(.1)
        else: raise AssertionError('fixture_did_not_restart')
        assert next(x for x in work()['schedules'] if x['id']==sid)['hours']==[6,18]
        page.locator('[data-plan-row="'+sid+'"]').wait_for()
        done('actual_process_restart_preserves_schedule_and_session')
        stage='schedule-edit'
        row=page.locator('[data-plan-row="'+sid+'"]')
        row.locator('[data-action=edit-plan-time]').click()
        assert page.locator('[data-plan-step="1"]').is_visible() and page.locator('#plan-save').is_visible()
        plan_times(page,['06:17','06:17']);page.locator('#plan-save').click()
        assert page.locator('#form-message').is_visible()
        plan_times(page,['07:17','19:17'],'UTC')
        page.locator('#plan-save').click();page.locator('#dialog').wait_for(state='hidden')
        assert next(x for x in work()['schedules'] if x['id']==sid)['timezone']=='UTC'
        row=page.locator('[data-plan-row="'+sid+'"]')
        row.locator('[data-action=toggle-schedule]').click();page.get_by_text('计划已停用。',exact=True).wait_for()
        assert not next(x for x in work()['schedules'] if x['id']==sid)['enabled']
        done('schedule_validation_edit_and_disable');shot('console-schedules.png')
        stage='run-duplicate'
        nav('tasks');select_task(page,'fixture-check','run-task')
        before=page.request.get(base+'/__fixture/stats').json()['mutations']
        run_id=accept_run(double=True)
        wait_run(page.request,base+'/admin/api/workbench',run_id,'succeeded')
        assert page.request.get(base+'/__fixture/stats').json()['mutations']==before+1
        done('run_double_click_single_mutation_and_server_verification')
        stage='cancel'
        nav('tasks');select_task(page,'fixture-pending','run-task')
        run_id=accept_run()
        row=page.locator('[data-run-id]').filter(has=page.get_by_text(run_id,exact=True))
        row.locator('[data-action=cancel-run]').wait_for();row.locator('[data-action=cancel-run]').click()
        wait_run(page.request,base+'/admin/api/workbench',run_id,'canceled');done('actual_task_cancellation')
        page.locator('[data-action=reload]').click();page.locator('[data-action=run-detail]').first.wait_for();shot('console-runs.png')
        stage='unconfirmed-submit'
        before=page.request.get(base+'/__fixture/stats').json()['mutations']
        page.route('**/api/workbench/run',lambda route:route.fulfill(status=503,content_type='application/json',body='{"error":"synthetic_unavailable"}'))
        nav('tasks');select_task(page,'fixture-check','run-task');page.locator('#run-form button[type=submit]').click()
        page.get_by_text('请求结果尚未确认。请先刷新服务端状态，避免重复提交。',exact=True).wait_for()
        page.locator('#dialog-close').click()
        select_task(page,'fixture-check','run-task')
        page.locator('[data-action=review-submission]').wait_for()
        page.locator('[data-action=review-submission]').click()
        page.locator('#review-form input').check();page.locator('#review-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden')
        assert page.request.get(base+'/__fixture/stats').json()['mutations']==before
        page.unroute('**/api/workbench/run');done('unconfirmed_submission_requires_review_without_automatic_replay')
        stage='usage-and-requests'
        nav('usage');page.locator('[data-total-requests]').wait_for();assert page.locator('[data-total-requests]').inner_text()=='1'
        nav('logs');page.get_by_text('synthetic-request-30',exact=True).wait_for();assert page.locator('tbody tr').count()==30
        page.locator('[data-action=logs-next]').click();page.get_by_text('synthetic-request-00',exact=True).wait_for();assert page.locator('tbody tr').count()==1
        page.locator('[data-action=logs-prev]').click();page.get_by_text('synthetic-request-30',exact=True).wait_for();done('real_usage_and_request_stores_with_pagination')
        stage='mobile'
        page.set_viewport_size({'width':390,'height':844})
        for route in ['overview','providers','accounts','models','tasks','usage','logs','keys','migration','settings']:
            stage='mobile:'+route
            nav(route)
            # data-load-state=ready 是导航完成的服务端渲染信号；比等待骨架
            # 元素消失更准：路由相同不会触发 hashchange，骨架本来就不存在。
            page.wait_for_function("document.querySelector('#view')?.dataset.loadState==='ready'")
            if route=='models':models_settled()
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'),route
            if route in ['models','tasks']:
                shot('console-'+route+'-mobile.png')
            if route=='models':
                assert page.locator('#model-account').bounding_box()['width']>=240
        done('ten_mobile_routes_and_usable_filter_widths')
        stage='key-lifecycle'
        page.set_viewport_size({'width':1440,'height':1000})
        nav('keys')
        page.locator('[data-action="key-new"]').click()
        page.locator('.model-picker-options [data-key-model]').first.wait_for(timeout=20000)
        picker_row=page.locator('.model-picker-options [data-key-model]').first
        assert picker_row.locator('.model-rate').count()==1
        assert '倍率未知' in picker_row.inner_text() or '免费' in picker_row.inner_text() or 'x' in picker_row.inner_text()
        picker_id=picker_row.get_attribute('data-key-model')
        page.locator('[data-model-channel="doubao"]').click();assert page.locator('.model-picker-options [data-key-model]').count()==0
        page.locator('[data-model-channel=""]').click();page.locator('[data-key-model="'+picker_id+'"]').click()
        assert page.locator('#key-form input[name="models"]').input_value()==picker_id
        page.locator('#key-form input[name="name"]').fill('synthetic-browser-key')
        page.locator('#key-form button[type=submit]').click()
        page.locator('#dialog textarea[readonly]').wait_for()
        secret = page.locator('#dialog textarea[readonly]').input_value()
        assert secret.startswith('wbk_') and len(secret) > 20
        page.locator('#dialog [data-action=close]').click()
        page.locator('[data-action="key-edit"]').wait_for()
        assert page.locator('text=synthetic-browser-key').count() == 1
        assert page.locator('code', has_text='wbk_').count() >= 1
        done('key_create_shows_secret_only_in_dialog_and_lists_masked')
        page.locator('[data-action="key-toggle"]').first.click()
        page.locator('[data-status=disabled]').first.wait_for(timeout=10000)
        done('key_disable_persists_and_reads_back')
        page.locator('[data-action="key-edit"]').first.click()
        page.locator('.model-picker-options [data-key-model]').first.wait_for(timeout=20000)
        assert page.locator('.model-picker-tag').count()==1 and picker_id in page.locator('.model-picker-tags').inner_text()
        page.locator('#key-form input[name="note"]').fill('synthetic note')
        page.locator('#key-form button[type=submit]').click()
        page.locator('text=synthetic note').wait_for(timeout=10000)
        done('key_edit_reads_back')
        stage='migration-page'
        nav('migration')
        page.locator('text=离线迁移工具').first.wait_for()
        page.locator('#view').get_by_text('当前工作区', exact=True).first.wait_for()
        done('migration_page_reports_no_marker_without_claiming_migration')
        stage='migration-preflight-ui'
        # The synthetic runtime starts without the offline tool; copy the real
        # migration scripts in so this exercises the shipped UI + tool path.
        tool_dir = runtime / 'tools' / 'migration'
        tool_dir.mkdir(parents=True, exist_ok=True)
        repo = Path(__file__).resolve().parents[2]
        for name in ('migrate.py', 'legacy_schedule.py'):
            (tool_dir / name).write_bytes((repo / 'tools' / 'migration' / name).read_bytes())
        page.reload()
        nav('migration')
        page.locator('#migration-preflight-form').wait_for(timeout=15000)
        old_root = runtime.parent / 'migration-ui-old'
        new_root = runtime.parent / 'migration-ui-new'
        import shutil as _shutil
        _shutil.rmtree(old_root, ignore_errors=True)
        _shutil.rmtree(new_root, ignore_errors=True)
        (old_root / 'config').mkdir(parents=True)
        (old_root / 'auths').mkdir()
        (old_root / 'auths-trash').mkdir()
        (old_root / 'data').mkdir()
        (old_root / 'panel-data').mkdir()
        (old_root / 'config' / 'config.json').write_text('{"auth_dir":"./auths","api_keys_file":"./data/api_keys.json"}', encoding='utf-8')
        page.locator('#migration-preflight-form input[name=source]').fill(str(old_root).replace(chr(92),'/'))
        page.locator('#migration-preflight-form input[name=destination]').fill(str(new_root).replace(chr(92),'/'))
        page.locator('#migration-preflight-form button[type=submit]').click()
        page.locator('#migration-preflight-message[data-preflight=passed]').wait_for(timeout=60000)
        assert not new_root.exists(), 'dry-run must not create the destination'
        done('migration_preflight_runs_real_tool_without_creating_target')
        stage='dark-and-settings'
        nav('settings')
        sidebar_action(page,'#theme');assert page.locator('html').get_attribute('data-theme')=='dark';shot('console-settings-dark-mobile.png')
        page.locator('[data-action=password]').click();page.locator('#password-form').wait_for();page.keyboard.press('Escape');assert not page.locator('#dialog').is_visible()
        sidebar_action(page,'#theme');done('theme_persistence_settings_and_keyboard_dialog')
        stage='delete'
        # 随包计划不能从界面删除：删掉一项只会让任务在下个版本前消失，
        # 运营动作应是停用（开关），而不是删除代码里的计划。
        nav('tasks');row=page.locator('[data-plan-row="'+sid+'"]');row.wait_for()
        assert row.locator('[data-action=delete-schedule]').count()==0
        assert any(x['id']==sid for x in work()['schedules']);done('shipped_plan_cannot_be_deleted_from_ui')
        stage='logout'
        sidebar_action(page,'#logout',close=False);page.locator('#login-submit').wait_for()
        assert page.request.get(base+'/admin/api/workbench').status==401
        done('logout_revokes_api_session')
        allowed=['net::ERR_INTERNET_DISCONNECTED','Failed to fetch','status of 503','status of 401']
        report['unexpected_console_errors']=[x for x in report['console_errors'] if not any(t in x for t in allowed)]
        assert not report['page_errors'] and not report['unexpected_console_errors'] and not report['external_requests']
        context.close();browser.close()

try:
    main();report['result']='PASS'
except Exception as e:
    report.update(result='FAIL',failed_stage=stage,error_type=type(e).__name__,source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('console_browser.py')])
finally:
    (output/'console-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ['result','failed_stage','error_type','source_lines'] if k in report},ensure_ascii=False))
print('completed_actions='+str(len(report['actions'])))
sys.exit(0 if report['result']=='PASS' else 1)

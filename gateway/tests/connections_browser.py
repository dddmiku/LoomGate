"""Authentication UI acceptance on synthetic catalog-workbench only."""
import argparse,json,subprocess,sys,time,traceback
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright
from console_actions import choose_provider, choose_account, choose_capability, sidebar_action
parser=argparse.ArgumentParser()
parser.add_argument('--runtime',required=True);parser.add_argument('--output',required=True);parser.add_argument('--fixture',required=True)
args=parser.parse_args();root=Path(args.runtime).resolve();out=Path(args.output).resolve();binary=Path(args.fixture).resolve()
if root.name!='catalog-fixture' or binary.name!='catalog-fixture.exe' or any(p.drive.upper()!='D:' for p in [root,out,binary]):raise SystemExit('synthetic_runtime_required')
out.mkdir(parents=True,exist_ok=True);base='http://127.0.0.1:17863';report={'actions':[],'page_errors':[],'console_errors':[],'external_requests':[],'screenshots':[]};stage='start'
def done(name):report['actions'].append(name)
def main():
    global stage
    with sync_playwright() as pw:
        browser=pw.chromium.launch(headless=True);page=browser.new_page(viewport={'width':1440,'height':1000})
        page.on('pageerror',lambda e:report['page_errors'].append(type(e).__name__))
        page.on('console',lambda m:report['console_errors'].append(m.text[:180]) if m.type=='error' else None)
        page.on('request',lambda r:report['external_requests'].append(urlsplit(r.url).hostname) if urlsplit(r.url).hostname not in [None,'127.0.0.1'] else None)
        def api(path):return page.request.get(base+'/admin/api/'+path).json()
        def post(path,data):return page.request.post(base+'/admin/api/'+path,data=data,headers={'X-Admin-Request':'1'})
        def flows():return api('connections/flows')['flows']
        def stats():return page.request.get(base+'/__fixture/stats').json()
        def accounts():return api('workbench/accounts')['accounts']
        def shot(name):page.screenshot(path=str(out/name),full_page=False,animations='disabled',mask=[page.locator('input[type=password]')],mask_color='#e3e8e3');report['screenshots'].append(name)
        def nav():
            if not page.locator('#navigation').is_visible():page.locator('#navigation-toggle').click()
            page.locator('#navigation a[href="#accounts"]').click();page.locator('#account-provider').wait_for();choose_provider(page,'#account-provider','doubao');page.locator('#account-list .loading').wait_for(state='detached');page.locator('[data-action=connect-account]').wait_for()
        def pick(selector,value):
            page.locator(selector+' input[value="'+value+'"]').check()
        def begin(method,provider='doubao',realm=None):
            page.locator('[data-action=connect-account]').click();pick('#connection-provider',provider)
            page.locator('#connection-provider').dispatch_event('change')
            if realm:pick('#connection-realm',realm)
            pick('#connection-method',method);page.locator('#connection-begin').click()
        fields=dict(line.split(':',1) for line in (root/'panel-data/initial-password.txt').read_text(encoding='utf-8').splitlines() if ':' in line)
        page.goto(base+'/admin/');page.locator('#username').fill(fields['username'].strip());page.locator('#password').fill(fields['password'].strip());page.locator('#login-submit').click();page.locator('[data-workspace-overview]').wait_for()
        assert 'sms_calls' in stats();page.request.post(base+'/__fixture/mode/success')
        # Only fixture flows are touched. A previous interrupted test is not
        # silently retried; it is canceled before this explicit test begins.
        for f in flows():
            if f['status'] not in ['succeeded','expired','canceled','failed']:
                post('connections/step',{'id':f['id'],'action':'cancel'})
        stage='sms-start';nav();begin('sms');page.locator('#connection-sensitive input[name=phone]').wait_for();shot('connection-sms.png')
        f=next(f for f in flows() if f['status']=='waiting_phone');fid=f['id'];before=stats()['sms_calls']
        page.locator('#connection-sensitive input').fill('+1 2025550100')
        page.locator('#connection-sensitive button').evaluate('(b)=>{b.click();b.click()}')
        page.locator('#connection-sensitive input[name=code]').wait_for();assert stats()['sms_calls']==before+1
        response=post('connections/step',{'id':fid,'action':'send_code','input':{'phone':'+1 2025550100'}})
        assert response.status==409 and stats()['sms_calls']==before+1
        done('sms_single_dispatch_and_server_cooldown')
        public=json.dumps(flows());assert '+1 2025550100' not in public and 'synthetic-qr-value' not in public and 'fixture_session' not in public
        done('no_private_fields_in_flow_projection')
        stage='restart';page.locator('#dialog-close').click();assert page.request.post(base+'/__fixture/stop').status==204
        deadline=time.monotonic()+8
        while time.monotonic()<deadline:
            try:page.request.get(base+'/__fixture/stats',timeout=250)
            except Exception:break
            time.sleep(.1)
        else:raise AssertionError('fixture_did_not_stop')
        with (root/'stdout.log').open('w') as stdout,(root/'stderr.log').open('w') as stderr:
            child=subprocess.Popen([str(binary),'-root',str(root),'-listen','127.0.0.1:17863'],stdout=stdout,stderr=stderr,creationflags=subprocess.CREATE_NO_WINDOW)
        (root/'owned-process.json').write_text(json.dumps({'pid':child.pid,'path':str(binary),'port':17863}),encoding='utf-8')
        deadline=time.monotonic()+8
        while time.monotonic()<deadline:
            try:
                if page.request.get(base+'/__fixture/stats',timeout=250).status==200:break
            except Exception:pass
            time.sleep(.1)
        else:raise AssertionError('fixture_did_not_restart')
        page.reload();page.locator('[data-connection-resume="'+fid+'"]').wait_for();page.locator('[data-connection-resume="'+fid+'"]').click()
        page.locator('#connection-sensitive input[name=code]').wait_for();assert stats()['sms_calls']==0
        done('actual_process_restart_resumes_code_wait_without_resend')
        stage='sms-verify';page.locator('#connection-sensitive input').fill('000000');page.locator('#connection-sensitive button').click();page.locator('#connection-done').wait_for(timeout=20000)
        completed=next(f for f in flows() if f['id']==fid);assert completed['status']=='succeeded';aid=completed['account_id'];assert any(a['id']==aid for a in accounts())
        page.locator('#connection-done').click();done('official_protocol_fixture_verification_and_encrypted_account_commit')
        stage='account-toggle';page.locator('#account-list [data-account="'+aid+'"]').first.wait_for();row=page.locator('#account-list tbody tr').filter(has=page.locator('[data-account="'+aid+'"]'));row.locator('[data-action=toggle-account]').click();page.locator('#account-toggle-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden')
        assert not next(a for a in accounts() if a['id']==aid)['enabled']
        page.locator('#account-list [data-account="'+aid+'"]').first.wait_for();page.locator('#account-list tbody tr').filter(has=page.locator('[data-account="'+aid+'"]')).locator('[data-action=toggle-account]').click();page.locator('#account-toggle-form button[type=submit]').click();page.locator('#dialog').wait_for(state='hidden');assert next(a for a in accounts() if a['id']==aid)['enabled']
        done('account_enable_disable_and_readback')
        stage='qr';begin('qr');page.locator('#connection-qr svg').wait_for(state='attached');shot('connection-qr.png')
        qr=next(f for f in flows() if f['status']=='waiting_scan');qbefore=stats()['qr_calls'];assert post('connections/start',{'provider':'doubao','method':'qr'}).status==409 and stats()['qr_calls']==qbefore
        page.get_by_text('豆包工作 · 已扫码，待确认',exact=True).wait_for(timeout=12000)
        assert next(f for f in flows() if f['id']==qr['id'])['status']=='scanned'
        page.locator('#connection-done').wait_for(timeout=25000);assert next(f for f in flows() if f['id']==qr['id'])['status'] in ('succeeded','verified')
        page.locator('#connection-done').click();done('qr_wait_scan_confirm_verification_and_duplicate_start_guard')
        stage='expired';page.request.post(base+'/__fixture/mode/qr-expired');begin('qr');page.locator('#connection-qr svg').wait_for(state='attached');before=stats()['qr_calls'];page.get_by_text('豆包工作 · 已过期',exact=True).wait_for(timeout=12000);assert stats()['qr_calls']==before
        page.locator('#connection-done').click();done('official_qr_expiry_without_auto_refresh')
        stage='challenge';page.request.post(base+'/__fixture/mode/challenge');begin('qr');page.get_by_text('豆包工作 · 需要人工处理',exact=True).wait_for();assert 'synthetic-private-rejection' not in page.locator('#dialog-body').inner_text();shot('connection-manual.png');page.locator('#connection-cancel').click();page.locator('#connection-done').click();page.locator('#dialog').wait_for(state='hidden');done('manual_verification_preserves_safe_status')
        stage='legacy-methods';page.request.post(base+'/__fixture/mode/success')
        page.locator('[data-action=connect-account]').click();pick('#connection-provider','workbuddy');page.locator('#connection-provider').dispatch_event('change');assert page.locator('#connection-realm').is_visible()
        pick('#connection-realm','global');page.locator('#connection-realm').dispatch_event('change')
        assert page.locator('#dialog').is_visible() and page.locator('#connection-method input:checked').count()==1
        pick('#connection-realm','cn');page.locator('#connection-realm').dispatch_event('change')
        assert page.locator('#dialog').is_visible() and page.locator('#connection-method input:checked').count()==1
        done('realm_reselection_inside_open_drawer_rebuilds_methods')
        pick('#connection-realm','global');pick('#connection-method','browser');page.locator('#connection-begin').click();page.locator('#connection-qr svg, a.official-link').first.wait_for();done('workbuddy_global_realm_drawer_opens_supported_method')
        page.locator('#dialog-close').click();page.locator('#dialog').wait_for(state='hidden')
        page.locator('[data-action=connect-account]').click();pick('#connection-provider','trae');page.locator('#connection-provider').dispatch_event('change');pick('#connection-method','browser');page.locator('#connection-begin').click();page.locator('#connection-callback input').wait_for();done('trae_authorization_ui_retained')
        stage='mobile';page.set_viewport_size({'width':390,'height':844});assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1');shot('connection-mobile.png');done('mobile_auth_forms_and_dialog');page.locator('#dialog-close').click();page.locator('#dialog').wait_for(state='hidden')
        stage='permissions';assert page.request.post(base+'/admin/api/connections/start',data={'provider':'trae','method':'browser'}).status==403
        assert page.request.post(base+'/admin/api/connections/start',data={'provider':'trae','method':'browser'},headers={'Origin':'https://foreign.invalid','X-Admin-Request':'1'}).status==403
        sidebar_action(page,'#logout',close=False);page.locator('#login-submit').wait_for();assert page.request.get(base+'/admin/api/connections/flows').status==401;done('authentication_management_permission_boundary')
        expected=['status of 409','status of 401','status of 403']
        report['unexpected_console_errors']=[x for x in report['console_errors'] if not any(t in x for t in expected)]
        assert not report['page_errors'] and not report['unexpected_console_errors'] and not report['external_requests']
        browser.close()
try:main();report['result']='PASS'
except Exception as e:report.update(result='FAIL',failed_stage=stage,error_type=type(e).__name__,source_lines=[x.lineno for x in traceback.extract_tb(e.__traceback__) if x.filename.endswith('connections_browser.py')])
finally:(out/'connections-browser.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({k:report[k] for k in ['result','failed_stage','error_type'] if k in report}));print('completed_actions='+str(len(report['actions'])))
sys.exit(0 if report['result']=='PASS' else 1)

"""Actual console auto-update checks. Synthetic local HTTP only."""
import argparse
import importlib.util
import json
import hashlib
from pathlib import Path
import threading
from urllib.parse import urlsplit, parse_qs
from playwright.sync_api import sync_playwright
_spec=importlib.util.spec_from_file_location('live_fixture_assets',Path(__file__).with_name('console_interactions_browser.py'))
_assets=importlib.util.module_from_spec(_spec);_spec.loader.exec_module(_assets)
assets_server=_assets.assets_server

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--output',type=Path,required=True);args=parser.parse_args()
    args.output.mkdir(parents=True,exist_ok=True)
    server=assets_server();threading.Thread(target=server.serve_forever,daemon=True).start()
    phase={'new':False,'extra':0};calls=[];failures=[]
    def row(i):
        return {'request_id':f'fixture-request-{i}','model':'autoclaw:fixture','protocol':'responses','key_name':'fixture caller','status':'success','started_at':'2026-11-23T08:00:00Z','finished_at':'2026-11-23T08:00:01Z','duration_ms':1000,'ttfb_ms':110,'output_tokens':90,'attempt_count':1,'upstream_started':True}
    def route_api(route):
        u=urlsplit(route.request.url);path=u.path;calls.append(path)
        if path.endswith('/workbench/providers'):
            data={'providers':[{'id':'autoclaw','name':'AutoClaw','catalog_scope':'account','accounts':{'total':1,'enabled':1}}]}
        elif path.endswith('/workbench/accounts'):
            data={'accounts':[{'id':'fixture-account','provider':'autoclaw','nickname':'fixture account','enabled':True,'credits':{'supported':True,'remain':10000,'expiring':0}}]}
        elif path.endswith('/requests/accounts'):
            data={'items':[{'account_id':'fixture-account','provider':'autoclaw','last_started_at':'2026-11-23T08:00:00Z','last_finished_at':'2026-11-23T08:00:01Z','last_request_id':'fixture-request-1','last_model':'autoclaw:fixture','last_status':'success','attempts_24h':3+int(phase['new'])+phase['extra'],'success_24h':3+int(phase['new'])+phase['extra'],'error_24h':0,'canceled_24h':0}],'window_seconds':86400,'partial':False}
        elif path.endswith('/requests'):
            items=[row(i) for i in range(35)]
            if phase['new']:items.insert(0,row('new'))
            for i in range(phase['extra']):items.insert(0,row(f'extra-{i}'))
            wanted=parse_qs(u.query).get('request_id',[''])[0]
            if wanted:items=[r for r in items if r['request_id']==wanted]
            offset=int(parse_qs(u.query).get('offset',['0'])[0]);data={'items':items[offset:offset+30],'total':len(items)}
        elif '/requests/' in path:
            data={'record':{'request_id':path.rsplit('/',1)[-1],'attempts':[{'number':1,'credit':0}]}}
        else:data={}
        if path.endswith('/requests') or path.endswith('/requests/accounts'):
            revision=hashlib.sha256(json.dumps(data,sort_keys=True).encode()).hexdigest()
            if parse_qs(u.query).get('revision',[''])[0]==revision:data={'ok':True,'not_modified':True,'revision':revision}
            else:data['revision']=revision
        if route.request.method!='GET':failures.append('unexpected mutation')
        route.fulfill(status=200,content_type='application/json',body=json.dumps(data))
    try:
        with sync_playwright() as p:
            browser=p.chromium.launch();page=browser.new_page(viewport={'width':1440,'height':1000})
            page.route('**/api/**',route_api)
            page.goto(f'http://127.0.0.1:{server.server_port}/#logs')
            page.wait_for_selector('[data-action=request-expand]')
            page.locator('[data-action=request-expand]').nth(3).click()
            page.wait_for_selector('.log-detail-row')
            phase['new']=True
            page.wait_for_function("document.querySelector('.log-table')?.textContent.includes('fixture-request-new')",timeout=7500)
            assert page.locator('.log-detail-row').count()==1,'auto-update lost expanded row'
            assert 'fixture-request-3' in page.locator('.log-detail-row').inner_text(),'expansion followed index instead of request id'
            page.screenshot(path=str(args.output/'logs-live-desktop.png'))
            page.locator('[data-action=logs-pause]').click();before=calls.count('/api/requests');phase['extra']=1
            page.wait_for_timeout(3300)
            assert calls.count('/api/requests')==before,'paused list kept polling'
            assert 'fixture-request-extra-0' not in page.locator('.log-table').inner_text()
            page.locator('[data-action=logs-pause]').click()
            page.wait_for_function("document.querySelector('.log-table')?.textContent.includes('fixture-request-extra-0')",timeout=6500)
            page.evaluate('window.scrollTo(0,400)');position=page.evaluate('window.scrollY');assert position>160
            phase['extra']=2
            page.locator('#records-new:not([hidden])').wait_for(timeout=6500)
            assert 'fixture-request-extra-1' not in page.locator('.log-table').inner_text(),'new rows displaced a reading view'
            assert page.evaluate('window.scrollY')==position,'background update moved the scroll anchor'
            page.locator('#records-new').click()
            page.wait_for_function("document.querySelector('.log-table')?.textContent.includes('fixture-request-extra-1')",timeout=6500)
            page.locator('[data-action=logs-next]').click();page.wait_for_function("document.querySelector('.log-table tbody')?.children.length<20")
            historical=page.locator('.log-table tbody').inner_text();phase['extra']=3
            page.locator('#records-new:not([hidden])').wait_for(timeout=6500)
            assert page.locator('.log-table tbody').inner_text()==historical,'poll rewrote the historical page'
            page.locator('#records-new').click()
            page.wait_for_function("document.querySelector('.log-table')?.textContent.includes('fixture-request-extra-2')",timeout=6500)
            page.goto(f'http://127.0.0.1:{server.server_port}/#accounts')
            page.wait_for_selector('[data-account-activity]')
            page.wait_for_function("document.querySelector('[data-account-activity]')?.textContent.includes('7 次上游尝试')",timeout=6500)
            page.locator('details.account-id summary').click();phase['extra']=4
            page.wait_for_function("document.querySelector('[data-account-activity]')?.textContent.includes('8 次上游尝试')",timeout=7500)
            assert page.locator('details.account-id').evaluate('(node)=>node.open'),'account activity update collapsed identity details'
            page.screenshot(path=str(args.output/'accounts-live-desktop.png'))
            page.set_viewport_size({'width':390,'height':844})
            assert page.evaluate('document.documentElement.scrollWidth<=window.innerWidth'),'mobile activity table overflowed'
            page.screenshot(path=str(args.output/'accounts-live-mobile.png'),full_page=True)
            assert not failures,failures
            browser.close()
            (args.output/'result.json').write_text(json.dumps({'result':'PASS','auto_updated_without_manual_refresh':True,'expanded_request_preserved':True,'pause_resume':True,'reading_scroll_preserved':True,'historical_page_preserved':True,'account_activity_auto_updated':True,'account_details_preserved':True,'mobile_no_overflow':True,'mutations':0,'external_provider_requests':0},indent=2),encoding='utf-8')
            print('PASS: actual console live records and account activity')
    finally:server.shutdown();server.server_close()
if __name__=='__main__':main()
